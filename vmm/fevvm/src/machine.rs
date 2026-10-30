use std::collections::HashSet;
use std::sync::atomic::{AtomicBool, AtomicU32, AtomicU64, Ordering};
use std::sync::{Arc, Condvar, Mutex};
use std::thread::JoinHandle;

use crate::bus::Bus;
use crate::exit::{self, InjectedAbort, MmioAccess, SysRegAccess};
use crate::hvf::{self, Exit, Vcpu, VcpuKick};
use crate::irq::Steering;
use crate::memory::GuestMemory;
use crate::pause::{self, Gate};
use crate::psci::{self, Action, Psci};
use crate::vmm_log;

const PSTATE_EL1H_MASKED: u64 = 0x3c5;

const UNCLAIMED_MMIO_REPORTS: u64 = 8;

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Outcome {
    PowerOff,
    Reset,
    Stopped,
    Fault(String),
}

fn offer_ecv(vcpu: &mut Vcpu, index: usize) {
    if std::env::var("FEV_VMM_ECV").as_deref() == Ok("off") || !host_has_ecv() {
        return;
    }
    let Ok(mmfr0) = vcpu.sys_reg(hvf::SYS_ID_AA64MMFR0_EL1) else {
        return;
    };
    if mmfr0 >> 60 != 0 {
        return;
    }
    let want = mmfr0 | 1 << 60;
    if let Err(e) = vcpu.set_sys_reg(hvf::SYS_ID_AA64MMFR0_EL1, want) {
        if index == 0 {
            eprintln!("vcpu: FEAT_ECV not offered: {e}");
        }
    }
}

fn host_has_ecv() -> bool {
    let mut v: i32 = 0;
    let mut len = std::mem::size_of::<i32>();
    let name = c"hw.optional.arm.FEAT_ECV";
    unsafe {
        libc::sysctlbyname(
            name.as_ptr(),
            &mut v as *mut i32 as *mut libc::c_void,
            &mut len,
            std::ptr::null_mut(),
            0,
        ) == 0
            && v == 1
    }
}

pub fn mpidr_for(index: u32) -> u64 {
    ((index as u64 / 16) << 8) | (index as u64 % 16)
}

pub struct Machine {
    mem: Arc<GuestMemory>,
    bus: Bus,
    steering: Arc<Steering>,
    psci: Mutex<Psci>,
    power_changed: Condvar,
    kicks: Mutex<Vec<VcpuKick>>,
    outcome: Mutex<Option<Outcome>>,
    finished: Condvar,
    stopping: AtomicBool,
    verbose: bool,
    unmodelled_sysregs: Mutex<HashSet<exit::SysRegEncoding>>,
    unclaimed_mmio: AtomicU64,
    window: Option<(u64, u64)>,
    window_faults: AtomicU64,
    exits: [AtomicU64; ExitKind::COUNT],
    accounts: Vec<VcpuAccount>,
    gate: Arc<Gate>,
}

#[derive(Default)]
struct VcpuAccount {
    exits: AtomicU64,
    thread: AtomicU32,
    ended_ns: AtomicU64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct VcpuUsage {
    pub cpu_ns: u64,
    pub exits: u64,
}

extern "C" {
    fn mach_thread_self() -> u32;
}

fn thread_cpu_ns(port: u32) -> u64 {
    if port == 0 {
        return 0;
    }
    let mut info: libc::thread_basic_info = unsafe { std::mem::zeroed() };
    let mut count = libc::THREAD_BASIC_INFO_COUNT;
    let ret = unsafe {
        libc::thread_info(
            port,
            libc::THREAD_BASIC_INFO as u32,
            (&mut info as *mut libc::thread_basic_info).cast(),
            &mut count,
        )
    };
    if ret != 0 {
        return 0;
    }
    let us = |t: libc::time_value_t| t.seconds as u64 * 1_000_000 + t.microseconds as u64;
    (us(info.user_time) + us(info.system_time)) * 1000
}

#[derive(Debug, Clone, Copy)]
pub enum ExitKind {
    Mmio,
    Hypercall,
    SysReg,
    Vtimer,
    Canceled,
    Wfx,
    Other,
}

impl ExitKind {
    const COUNT: usize = 7;
    const ALL: [Self; Self::COUNT] = [
        Self::Mmio,
        Self::Hypercall,
        Self::SysReg,
        Self::Vtimer,
        Self::Canceled,
        Self::Wfx,
        Self::Other,
    ];
}

impl Machine {
    pub fn new(
        mem: Arc<GuestMemory>,
        bus: Bus,
        cpus: u32,
        boot: (u64, u64),
        steering: Arc<Steering>,
        window: Option<(u64, u64)>,
        verbose: bool,
    ) -> Arc<Self> {
        let mut psci = Psci::new((0..cpus).map(mpidr_for).collect());
        psci.set_boot(boot.0, boot.1);
        Arc::new(Self {
            mem,
            bus,
            steering,
            psci: Mutex::new(psci),
            power_changed: Condvar::new(),
            kicks: Mutex::new(Vec::new()),
            outcome: Mutex::new(None),
            finished: Condvar::new(),
            stopping: AtomicBool::new(false),
            verbose,
            unmodelled_sysregs: Mutex::new(HashSet::new()),
            unclaimed_mmio: AtomicU64::new(0),
            window,
            window_faults: AtomicU64::new(0),
            exits: Default::default(),
            accounts: (0..cpus).map(|_| VcpuAccount::default()).collect(),
            gate: Arc::new(Gate::new()),
        })
    }

    pub fn start(self: &Arc<Self>, cpus: u32) -> Vec<JoinHandle<()>> {
        (0..cpus)
            .map(|index| {
                let machine = self.clone();
                std::thread::Builder::new()
                    .name(format!("vcpu{index}"))
                    .spawn(move || {
                        if let Err(e) = machine.vcpu_thread(index as usize) {
                            machine.finish(Outcome::Fault(format!("vcpu{index}: {e}")));
                        }
                        let account = &machine.accounts[index as usize];
                        let port = account.thread.load(Ordering::Relaxed);
                        account
                            .ended_ns
                            .store(thread_cpu_ns(port), Ordering::Relaxed);
                        account.thread.store(0, Ordering::Relaxed);
                    })
                    .expect("spawn vCPU thread")
            })
            .collect()
    }

    pub fn finish(&self, outcome: Outcome) {
        {
            let mut slot = self.outcome.lock().unwrap();
            if slot.is_none() {
                *slot = Some(outcome);
            }
        }
        self.stopping.store(true, Ordering::SeqCst);
        self.gate.stop();
        self.kick_all();
        drop(self.psci.lock().unwrap());
        self.power_changed.notify_all();
        self.finished.notify_all();
    }

    fn kick_all(&self) {
        for kick in self.kicks.lock().unwrap().iter() {
            kick.kick();
        }
    }

    pub fn pause(&self) -> Result<pause::Paused, pause::Refused> {
        self.gate.pause(&|| self.kick_all(), &hvf::host_ticks)
    }

    pub fn resume(&self, first: &dyn Fn()) -> Result<pause::Resumed, pause::Refused> {
        self.gate.resume_after(&hvf::host_ticks, first)
    }

    pub fn ended(&self) -> usize {
        self.gate.ended(&|| self.kick_all(), pause::END_GRACE)
    }

    pub fn pause_account(&self) -> pause::Account {
        self.gate.account()
    }

    pub fn wait(&self) -> Outcome {
        let mut slot = self.outcome.lock().unwrap();
        loop {
            if let Some(outcome) = slot.clone() {
                return outcome;
            }
            slot = self.finished.wait(slot).unwrap();
        }
    }

    pub fn exit_counts(&self) -> Vec<(ExitKind, u64)> {
        ExitKind::ALL
            .iter()
            .map(|k| (*k, self.exits[*k as usize].load(Ordering::Relaxed)))
            .collect()
    }

    pub fn window_faults(&self) -> u64 {
        self.window_faults.load(Ordering::Relaxed)
    }

    pub fn unclaimed_mmio(&self) -> u64 {
        self.unclaimed_mmio.load(Ordering::Relaxed)
    }

    pub fn vcpu_usage(&self) -> Vec<VcpuUsage> {
        self.accounts
            .iter()
            .map(|a| VcpuUsage {
                cpu_ns: match a.thread.load(Ordering::Relaxed) {
                    0 => a.ended_ns.load(Ordering::Relaxed),
                    port => thread_cpu_ns(port),
                },
                exits: a.exits.load(Ordering::Relaxed),
            })
            .collect()
    }

    fn count(&self, index: usize, kind: ExitKind) {
        self.exits[kind as usize].fetch_add(1, Ordering::Relaxed);
        self.accounts[index].exits.fetch_add(1, Ordering::Relaxed);
    }

    pub fn stopping(&self) -> bool {
        self.stopping.load(Ordering::SeqCst)
    }

    fn vcpu_thread(&self, index: usize) -> hvf::Result<()> {
        let mut vcpu = Vcpu::new()?;
        vcpu.set_sys_reg(hvf::SYS_MPIDR_EL1, mpidr_for(index as u32))?;
        offer_ecv(&mut vcpu, index);
        let sctlr_reset = vcpu.sys_reg(hvf::SYS_SCTLR_EL1)?;
        let mut timer = TimerOffset {
            base: vcpu.vtimer_offset()?,
            applied: 0,
        };
        crate::irq::enter_vcpu(index as u32);
        self.gate.attach();
        self.kicks.lock().unwrap().push(vcpu.kicker());
        self.accounts[index]
            .thread
            .store(unsafe { mach_thread_self() }, Ordering::Relaxed);

        loop {
            let (entry, context) = {
                let mut psci = self.psci.lock().unwrap();
                loop {
                    if self.stopping() {
                        return Ok(());
                    }
                    if let Some(start) = psci.started(index) {
                        break start;
                    }
                    psci = self.power_changed.wait(psci).unwrap();
                }
            };
            vcpu.set_sys_reg(hvf::SYS_SCTLR_EL1, sctlr_reset)?;
            vcpu.set_pstate(PSTATE_EL1H_MASKED)?;
            vcpu.set_pc(entry)?;
            vcpu.set_x(0, context)?;
            for n in 1..=3 {
                vcpu.set_x(n, 0)?;
            }
            if self.verbose {
                vmm_log!("vcpu{index}: on, entry {entry:#x}");
            }
            self.steering.cpu_on(index as u32);
            let total = self.gate.enter();
            let ran = timer
                .apply(&mut vcpu, total)
                .and_then(|()| self.run(&mut vcpu, index, &mut timer));
            self.gate.leave();
            if let Ok(true) = ran {
                self.steering.cpu_off(index as u32);
            }
            if !ran? {
                return Ok(());
            }
        }
    }

    fn run(&self, vcpu: &mut Vcpu, index: usize, timer: &mut TimerOffset) -> hvf::Result<bool> {
        loop {
            if self.gate.wanted() {
                let total = self.gate.park();
                timer.apply(vcpu, total)?;
            }
            if self.stopping() {
                return Ok(false);
            }
            crate::stage::entering();
            let exit = vcpu.run()?;
            match exit {
                Exit::Canceled => self.count(index, ExitKind::Canceled),
                Exit::VtimerActivated => {
                    self.count(index, ExitKind::Vtimer);
                    self.finish(Outcome::Fault(format!(
                        "vcpu{index}: the framework asked the VMM to deliver the virtual timer, \
                         which the in-kernel interrupt controller is expected to do"
                    )));
                    return Ok(false);
                }
                Exit::Exception { syndrome, ipa, va } => match exit::exception_class(syndrome) {
                    exit::EC_DATA_ABORT_LOWER => {
                        crate::stage::exit_taken();
                        self.count(index, ExitKind::Mmio);
                        let go_on = self.mmio(vcpu, index, syndrome, ipa);
                        if let Some(total) = self.gate.back() {
                            timer.apply(vcpu, total)?;
                        }
                        if !go_on? {
                            return Ok(false);
                        }
                    }
                    ec @ (exit::EC_HVC64 | exit::EC_SMC64) => {
                        self.count(index, ExitKind::Hypercall);
                        if ec == exit::EC_SMC64 {
                            vcpu.set_pc(vcpu.pc()? + 4)?;
                        }
                        match self.hypercall(vcpu, index)? {
                            Flow::Continue => {}
                            Flow::CpuOff => return Ok(true),
                            Flow::Stop => return Ok(false),
                        }
                    }
                    exit::EC_SYSREG => {
                        self.count(index, ExitKind::SysReg);
                        let access = SysRegAccess::decode(syndrome);
                        if !access.is_known_inert()
                            && self
                                .unmodelled_sysregs
                                .lock()
                                .unwrap()
                                .insert(access.encoding)
                        {
                            let (op0, op1, crn, crm, op2) = access.encoding;
                            vmm_log!(
                                "warning: guest {} unmodelled system register \
                                     S{op0}_{op1}_C{crn}_C{crm}_{op2} at pc {:#x}; treated as zero",
                                if access.read { "read" } else { "wrote" },
                                vcpu.pc()?
                            );
                        }
                        if access.read {
                            vcpu.set_x(access.reg, 0)?;
                        }
                        vcpu.set_pc(vcpu.pc()? + 4)?;
                    }
                    exit::EC_WFX => {
                        self.count(index, ExitKind::Wfx);
                        vcpu.set_pc(vcpu.pc()? + 4)?;
                    }
                    exit::EC_INSN_ABORT_LOWER => {
                        self.count(index, ExitKind::Other);
                        let pc = vcpu.pc()?;
                        let pstate = vcpu.pstate()?;
                        let Some(abort) = InjectedAbort::prefetch(pstate) else {
                            self.finish(Outcome::Fault(format!(
                                "vcpu{index}: instruction fetch from {ipa:#x} in an unexpected \
                                     state (pstate {pstate:#x}, pc {pc:#x})"
                            )));
                            return Ok(false);
                        };
                        vcpu.set_sys_reg(hvf::SYS_ELR_EL1, pc)?;
                        vcpu.set_sys_reg(hvf::SYS_SPSR_EL1, pstate)?;
                        vcpu.set_sys_reg(hvf::SYS_ESR_EL1, abort.esr)?;
                        vcpu.set_sys_reg(hvf::SYS_FAR_EL1, va)?;
                        vcpu.set_pstate(abort.pstate)?;
                        vcpu.set_pc(vcpu.sys_reg(hvf::SYS_VBAR_EL1)? + abort.vector)?;
                    }
                    ec => {
                        self.count(index, ExitKind::Other);
                        self.finish(Outcome::Fault(format!(
                            "vcpu{index}: unexpected exception class {ec:#x} \
                                 (syndrome {syndrome:#x}, pc {:#x}, address {va:#x}, ipa {ipa:#x})",
                            vcpu.pc()?
                        )));
                        return Ok(false);
                    }
                },
                Exit::Unknown(reason) => {
                    self.count(index, ExitKind::Other);
                    self.finish(Outcome::Fault(format!(
                        "vcpu{index}: unknown exit reason {reason}"
                    )));
                    return Ok(false);
                }
            }
        }
    }

    fn mmio(&self, vcpu: &mut Vcpu, index: usize, syndrome: u64, ipa: u64) -> hvf::Result<bool> {
        let pc = vcpu.pc()?;
        if self.mem.host_range(ipa, 1).is_ok() {
            self.finish(Outcome::Fault(format!(
                "vcpu{index}: fault on guest RAM at {ipa:#x} (syndrome {syndrome:#x}, pc {pc:#x})"
            )));
            return Ok(false);
        }
        if let Some((base, _)) = self.window.filter(|(b, l)| ipa >= *b && ipa - *b < *l) {
            self.window_faults.fetch_add(1, Ordering::Relaxed);
            let what = if exit::data_abort_is_write(syndrome) {
                "the guest wrote to the data disk's read window"
            } else {
                "the Mac could not supply a page of the data image that the guest read \
                 through its window (is the image cut short, or unreadable?)"
            };
            self.finish(Outcome::Fault(format!(
                "vcpu{index}: {what}: image offset {:#x} (syndrome {syndrome:#x}, pc {pc:#x})",
                ipa - base
            )));
            return Ok(false);
        }
        let Some(access) = MmioAccess::decode(syndrome) else {
            self.finish(Outcome::Fault(format!(
                "vcpu{index}: device access at {ipa:#x} with an instruction that cannot be \
                 emulated (syndrome {syndrome:#x}, pc {pc:#x})"
            )));
            return Ok(false);
        };
        let claimed = if access.write {
            let value = access.store_value(vcpu.x(access.reg)?);
            self.bus.write(ipa, access.size, value)
        } else {
            let value = self.bus.read(ipa, access.size);
            vcpu.set_x(access.reg, access.load_value(value.unwrap_or(0)))?;
            value.is_some()
        };
        if !claimed {
            let seen = self.unclaimed_mmio.fetch_add(1, Ordering::Relaxed);
            if seen < UNCLAIMED_MMIO_REPORTS {
                vmm_log!(
                    "warning: vcpu{index}: guest {} address {ipa:#x}, where there is no device, \
                     at pc {pc:#x}",
                    if access.write { "wrote to" } else { "read" }
                );
            }
        }
        vcpu.set_pc(pc + 4)?;
        Ok(true)
    }

    fn hypercall(&self, vcpu: &mut Vcpu, index: usize) -> hvf::Result<Flow> {
        let args = [vcpu.x(0)?, vcpu.x(1)?, vcpu.x(2)?, vcpu.x(3)?];
        let action = self.psci.lock().unwrap().call(index, args);
        Ok(match action {
            Action::Return(value) => {
                vcpu.set_x(0, value as u64)?;
                Flow::Continue
            }
            Action::CpuOn { .. } => {
                self.power_changed.notify_all();
                vcpu.set_x(0, psci::SUCCESS as u64)?;
                Flow::Continue
            }
            Action::CpuOff => {
                if self.verbose {
                    vmm_log!("vcpu{index}: off");
                }
                Flow::CpuOff
            }
            Action::SystemOff => {
                self.finish(Outcome::PowerOff);
                Flow::Stop
            }
            Action::SystemReset => {
                self.finish(Outcome::Reset);
                Flow::Stop
            }
        })
    }
}

struct TimerOffset {
    base: u64,
    applied: u64,
}

impl TimerOffset {
    fn apply(&mut self, vcpu: &mut Vcpu, total: u64) -> hvf::Result<()> {
        if total != self.applied {
            vcpu.set_vtimer_offset(self.base + total)?;
            self.applied = total;
        }
        Ok(())
    }
}

enum Flow {
    Continue,
    CpuOff,
    Stop,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn mpidrs_are_unique_and_keep_aff0_below_16() {
        let all: Vec<u64> = (0..64).map(mpidr_for).collect();
        assert_eq!(all[0], 0);
        assert_eq!(all[15], 0xf);
        assert_eq!(all[16], 0x100);
        assert_eq!(all[63], 0x30f);
        let unique: HashSet<_> = all.iter().collect();
        assert_eq!(unique.len(), 64);
        assert!(all.iter().all(|m| m & 0xff < 16));
    }
}
