use std::io::Read;
use std::os::unix::process::CommandExt;
use std::process::ExitCode;
use std::sync::Arc;
use std::time::{Duration, Instant};

use fevvm::bus::Bus;
use fevvm::config::{Config, ConsoleMode, ParseError, SyncMode, USAGE};
use fevvm::console::ConsoleOut;
use fevvm::control::{self, Control};
use fevvm::devices::{pl011::Pl011, pl031::Pl031};
use fevvm::fdt::{self, FdtConfig, IrqDevice, Region};
use fevvm::hvf::{self, Vm};
use fevvm::irq::{IrqLine, Steering};
use fevvm::layout::*;
use fevvm::machine::{mpidr_for, Machine, Outcome};
use fevvm::memory::GuestMemory;
use fevvm::trace::{self, Mark, Trace};
use fevvm::virtio::balloon::{Balloon, BalloonStats};
use fevvm::virtio::blk::{Block, Disk, ImageWindow, MAX_QUEUES};
use fevvm::virtio::console::{Console, ConsoleInput};
use fevvm::virtio::fs::Fs;
use fevvm::virtio::net::Net;
use fevvm::virtio::rng::{host_entropy, Rng};
use fevvm::virtio::vsock::Vsock;
use fevvm::virtio::{MmioTransport, VirtioDevice};
use fevvm::vmmctl;
use fevvm::{boot, lifeline, pause, terminal, vmm_log};

const BUILD: &str = match option_env!("FEV_BUILD_ID") {
    Some(id) => id,
    None => "dev",
};

const RESETS_ENV: &str = "FEV_VMM_RESETS";

const STEER_ENV: &str = "FEV_VMM_IRQ_STEER";

const WINDOW_ENV: &str = "FEV_VMM_WINDOW";

const IPA_BITS_ENV: &str = "FEV_VMM_IPA_BITS";

const QUEUE_WATCH_ENV: &str = "FEV_VMM_QUEUE_WATCH";

const GIC_DIST_SIZE: u64 = 0x1_0000;
const CONSOLE_FLUSH_LIMIT: Duration = Duration::from_secs(2);
const ESCAPE: u8 = 0x01;

type Error = Box<dyn std::error::Error>;

fn main() -> ExitCode {
    let main_ns = trace::now_ns();
    let start = Instant::now();
    unsafe { libc::pthread_sigmask(libc::SIG_BLOCK, &report_signals(), std::ptr::null_mut()) };
    let cfg = match Config::parse(std::env::args().skip(1)) {
        Ok(cfg) => cfg,
        Err(ParseError::Help) => {
            println!("{USAGE}");
            return ExitCode::SUCCESS;
        }
        Err(ParseError::Version) => {
            println!("{BUILD}");
            return ExitCode::SUCCESS;
        }
        Err(ParseError::Bad(msg)) => {
            eprintln!("fevvm: {msg}\n\n{USAGE}");
            return ExitCode::from(2);
        }
    };

    let trace = match Trace::new(cfg.trace_fd) {
        Ok(trace) => Arc::new(trace),
        Err(e) => {
            eprintln!("fevvm: --trace-fd {}: {e}", cfg.trace_fd.unwrap_or(-1));
            return ExitCode::from(2);
        }
    };
    if let Some(fd) = cfg.lifeline_fd.filter(|fd| !trace::is_open(*fd)) {
        eprintln!("fevvm: --lifeline-fd {fd}: not an open descriptor");
        return ExitCode::from(2);
    }
    if let Some(fd) = cfg.control_fd.filter(|fd| !trace::is_open(*fd)) {
        eprintln!("fevvm: --control-fd {fd}: not an open descriptor");
        return ExitCode::from(2);
    }
    trace.main(main_ns, BUILD);
    let resets = std::env::var(RESETS_ENV)
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(0u64);
    let outcome = run(&cfg, start, &trace, resets);
    trace.exit(match &outcome {
        Ok(Outcome::PowerOff) => "power_off",
        Ok(Outcome::Reset) => "reset",
        Ok(Outcome::Stopped) => "stopped",
        Ok(Outcome::Fault(_)) => "fault",
        Err(_) => "error",
    });
    fevvm::flush_log();
    terminal::restore();
    match outcome {
        Ok(Outcome::PowerOff | Outcome::Stopped) => ExitCode::SUCCESS,
        Ok(Outcome::Reset) if cfg.no_reboot => ExitCode::SUCCESS,
        Ok(Outcome::Reset) => {
            let err = std::process::Command::new(std::env::current_exe().unwrap())
                .args(std::env::args_os().skip(1))
                .env(RESETS_ENV, (resets + 1).to_string())
                .exec();
            eprintln!("fevvm: restart after guest reset failed: {err}");
            ExitCode::FAILURE
        }
        Ok(Outcome::Fault(msg)) => {
            eprintln!("fevvm: {msg}");
            ExitCode::FAILURE
        }
        Err(e) => {
            eprintln!("fevvm: {e}");
            ExitCode::FAILURE
        }
    }
}

fn run(cfg: &Config, start: Instant, trace: &Arc<Trace>, resets: u64) -> Result<Outcome, Error> {
    let queue_watch = std::env::var(QUEUE_WATCH_ENV).ok();
    fevvm::virtio::queue::set_watching(cfg.verbose || queue_watch.is_some());
    fevvm::stage::init_from_env();
    fevvm::hops::init_from_env();
    let all_queues = queue_watch.as_deref() == Some("all");
    let max_cpus = Vm::max_vcpus()?;
    if cfg.cpus > max_cpus {
        return Err(format!("--cpus {}: this Mac supports at most {max_cpus}", cfg.cpus).into());
    }
    let mem_len = cfg.mem_mib << 20;
    if RAM_BASE + mem_len > 1 << DEFAULT_IPA_BITS {
        return Err(format!(
            "--mem {}: at most {} MiB fits in the guest address space",
            cfg.mem_mib,
            ((1u64 << DEFAULT_IPA_BITS) - RAM_BASE) >> 20
        )
        .into());
    }

    let disks = cfg
        .disks
        .iter()
        .map(|d| {
            Disk::open(&d.path, d.read_only)
                .map(|disk| {
                    let disk = disk.with_sync(cfg.disk_sync);
                    Arc::new(match d.swap {
                        true => disk.with_space(Arc::new(fevvm::space::Space::off())),
                        false => disk,
                    })
                })
                .map_err(|e| format!("--disk {}: {e}", d.path.display()))
        })
        .collect::<Result<Vec<_>, _>>()?;
    let flush_disks = disks.clone();

    let data_disk = cfg
        .disks
        .iter()
        .zip(&disks)
        .position(|(c, d)| !d.is_read_only() && !c.swap);
    let no_window = cfg.no_window || std::env::var(WINDOW_ENV).is_ok_and(|v| v == "off");
    let want_window = data_disk.is_some() && !no_window && cfg.flat.is_none();
    let forced_bits = std::env::var(IPA_BITS_ENV)
        .ok()
        .and_then(|v| v.parse::<u32>().ok());
    let ipa_bits = match forced_bits {
        Some(bits) => bits,
        None if want_window => Vm::max_ipa_bits()?.min(WINDOW_IPA_BITS),
        None => DEFAULT_IPA_BITS,
    };
    let vm = Vm::new(
        GIC_DIST_BASE,
        GIC_REDIST_BASE,
        (ipa_bits != DEFAULT_IPA_BITS || forced_bits.is_some()).then_some(ipa_bits),
    )?;
    let mem = Arc::new(GuestMemory::new(RAM_BASE, mem_len)?);
    vm.map_memory(&mem)?;
    let window = match data_disk {
        None => {
            let mut none = DiskWindow::off("no writable disk");
            none.has_disk = false;
            none
        }
        Some(_) if cfg.no_window => DiskWindow::off("--no-window"),
        Some(_) if no_window => DiskWindow::off("FEV_VMM_WINDOW=off"),
        Some(_) if cfg.flat.is_some() => DiskWindow::off("--flat"),
        Some(i) => DiskWindow::map(&vm, &disks[i], ipa_bits, mem.end()),
    };
    if let Some(i) = data_disk.filter(|_| cfg.verbose || window.region().is_none()) {
        vmm_log!("disk {i}: {}", window.line());
    }
    trace.mark(Mark::MemoryMapped);
    let chip = vm.irq_chip();

    let out: Box<dyn std::io::Write + Send> = match cfg.console {
        ConsoleMode::Stdio => Box::new(std::io::stdout()),
        ConsoleMode::None => Box::new(std::io::sink()),
    };
    let out = Arc::new(ConsoleOut::new(out, cfg.stamp.then_some(start)));
    if cfg.trace_fd.is_some() {
        let trace = trace.clone();
        out.on_first_write(move || trace.mark(Mark::ConsoleFirst));
    }

    let (console, input) = Console::new(out.clone());
    let mut devices: Vec<Box<dyn VirtioDevice>> = vec![Box::new(console), Box::new(Rng::new())];
    let blk_queues = cfg.cpus.min(MAX_QUEUES as u32) as u16;
    devices.extend(disks.iter().enumerate().map(|(i, d)| {
        let mut blk = Block::new(d.clone(), blk_queues);
        if let (Some((base, len)), true) = (window.region(), Some(i) == data_disk) {
            blk = blk.with_window(base, len);
        }
        Box::new(blk) as Box<dyn VirtioDevice>
    }));
    let (balloon, balloon_stats) = Balloon::new(vm.releaser(mem.clone()));
    devices.push(Box::new(balloon));
    if let Some(path) = &cfg.net {
        let net = Net::connect(path)
            .map_err(|e| format!("--net {}: {e} (is the netstack running?)", path.display()))?;
        devices.push(Box::new(net));
    }
    if !cfg.shares.is_empty() {
        if let Err(e) = materialize_dataless_files() {
            vmm_log!("warning: files kept in the cloud may not be readable through the share: {e}");
        }
    }
    let mut shares = Vec::new();
    for share in &cfg.shares {
        let mut pc = fileshare::passthrough::Config::new(&share.path);
        pc.read_only = share.read_only;
        pc.deny = share.deny.clone();
        pc.sync = match share.sync.unwrap_or(cfg.disk_sync) {
            SyncMode::Fsync => fileshare::passthrough::Sync::Fsync,
            SyncMode::Barrier | SyncMode::Paced => fileshare::passthrough::Sync::Barrier,
            SyncMode::Full => fileshare::passthrough::Sync::Full,
        };
        let fs = fileshare::passthrough::Passthrough::new(&pc)
            .map_err(|e| format!("--share tag={}: {e}", share.tag))?;
        if cfg.trace_fd.is_some() {
            let trace = trace.clone();
            fs.on_denied(Arc::new(move |folder: &[u8]| trace.denied(folder)));
        }
        shares.push((share.tag.clone(), Arc::new(fs)));
    }
    let mut share_feed = None;
    if let Some(dir) = &cfg.vsock_dir {
        let mut vsock = Vsock::new(dir, &cfg.vsock_ports)
            .map_err(|e| format!("--vsock-dir {}: {e}", dir.display()))?;
        let feed = (!shares.is_empty()).then(|| {
            fileshare::feed::Feed::new(
                shares.iter().map(|(_, fs)| fs.clone()).collect(),
                BUILD,
                Box::new(|line| vmm_log!("{line}")),
            )
        });
        share_feed = feed.clone();
        vsock.answer(
            fileshare::fsevents::HOST_PORT,
            Box::new(move |stream| feed.as_ref().is_some_and(|f| f.serve(stream))),
        );
        devices.push(Box::new(vsock));
    }
    for (tag, fs) in shares {
        let server = fileshare::Server::shared(fs);
        let dev = Fs::new(&tag, server, cfg.cpus, cfg.verbose)
            .map_err(|_| format!("--share tag={tag}: not a usable tag"))?;
        devices.push(Box::new(dev));
    }
    if cfg.rosetta {
        let device = (|| -> Result<Fs, Error> {
            if !fileshare::rosetta::installed() {
                return Err("Rosetta for Linux is not installed; run softwareupdate --install-rosetta on this Mac, or select emulation.engine fex".into());
            }
            let mut pc = fileshare::passthrough::Config::new(fileshare::rosetta::DIR);
            pc.read_only = true;
            let fs = fileshare::passthrough::Passthrough::new(&pc)?.with_rosetta()?;
            let server = fileshare::Server::new(Box::new(fs));
            Fs::new(fileshare::rosetta::TAG, server, cfg.cpus, cfg.verbose)
                .map_err(|_| "invalid Rosetta share tag".into())
        })();
        match device {
            Ok(device) => devices.push(Box::new(device)),
            Err(error) if cfg.rosetta_optional => {
                eprintln!("rosetta: unavailable, automatic mode will use FEX: {error}")
            }
            Err(error) => return Err(error),
        }
    }
    if devices.len() as u32 > VIRTIO_MAX_SLOTS {
        return Err(format!(
            "too many devices: {} for {VIRTIO_MAX_SLOTS} slots; fewer disks or shares",
            devices.len()
        )
        .into());
    }
    if cfg.flat.is_some() {
        devices.clear();
    }
    let last_intid = virtio_slot_intid((devices.len() as u32).max(1) - 1);
    if UART_INTID < vm.spi_base || last_intid >= vm.spi_base + vm.spi_count {
        return Err("interrupt layout does not fit the interrupt controller".into());
    }

    if vm.vtimer_intid != fdt::VTIMER_INTID {
        return Err(format!("unexpected virtual timer INTID {}", vm.vtimer_intid).into());
    }

    let steering = Steering::new(
        cfg.cpus,
        std::env::var(STEER_ENV).map_or(true, |v| v != "off"),
    );
    let mut bus = Bus::new();
    bus.insert(
        UART_BASE,
        UART_SIZE,
        Arc::new(Pl011::new(
            out.clone(),
            IrqLine::new(chip.clone(), UART_INTID),
        )),
    );
    if cfg.flat.is_some() {
        let silent = IrqLine::new(Arc::new(NoIrq), 0);
        bus.insert(
            UART_EARLY_BASE,
            UART_SIZE,
            Arc::new(Pl011::new(out.clone(), silent)),
        );
    }
    bus.insert(RTC_BASE, RTC_SIZE, Arc::new(Pl031::new()));
    let mut virtio_nodes = Vec::new();
    for (slot, device) in devices.into_iter().enumerate() {
        let (base, intid) = (
            virtio_slot_base(slot as u32),
            virtio_slot_intid(slot as u32),
        );
        let line = steering.line(chip.clone(), intid);
        bus.insert(
            base,
            VIRTIO_SLOT_SIZE,
            Arc::new(MmioTransport::new(device, mem.clone(), line)),
        );
        virtio_nodes.push(IrqDevice {
            region: Region {
                base,
                size: VIRTIO_REG_SIZE,
            },
            intid,
        });
    }

    trace.mark(Mark::DevicesReady);

    let loaded = match &cfg.flat {
        Some(flat) => boot::load_flat(&mem, flat, cfg.initrd.as_deref())?,
        None => boot::load(&mem, &cfg.kernel, cfg.initrd.as_deref())?,
    };
    let mpidrs: Vec<u64> = (0..cfg.cpus).map(mpidr_for).collect();
    let mut rng_seed = [0u8; 32];
    host_entropy(&mut rng_seed);
    let blob = fdt::build(&FdtConfig {
        ram: Region {
            base: RAM_BASE,
            size: mem_len,
        },
        cpus: &mpidrs,
        cmdline: &cfg.cmdline,
        initrd: loaded.initrd,
        rng_seed: &rng_seed,
        gic_dist: Region {
            base: GIC_DIST_BASE,
            size: GIC_DIST_SIZE,
        },
        gic_redist: Region {
            base: GIC_REDIST_BASE,
            size: vm.redist_size_per_cpu * cfg.cpus as u64,
        },
        uart: IrqDevice {
            region: Region {
                base: UART_BASE,
                size: UART_SIZE,
            },
            intid: UART_INTID,
        },
        rtc: Region {
            base: RTC_BASE,
            size: RTC_SIZE,
        },
        virtio: &virtio_nodes,
    })
    .map_err(|e| format!("device tree: {e}"))?;
    if blob.len() as u64 > boot::FDT_MAX_SIZE {
        return Err("device tree exceeds 2 MiB".into());
    }
    mem.write(loaded.fdt_addr, &blob)?;
    trace.mark(Mark::KernelLoaded);

    let machine = Machine::new(
        mem.clone(),
        bus,
        cfg.cpus,
        (loaded.entry, loaded.fdt_addr),
        steering.clone(),
        window.region(),
        cfg.verbose,
    );
    let window = Arc::new(window);

    report_memory_on_signal(
        mem.clone(),
        balloon_stats.clone(),
        flush_disks.clone(),
        steering.clone(),
        window.clone(),
    );

    if let Some(fd) = cfg.lifeline_fd {
        let machine = machine.clone();
        lifeline::watch(fd, move || {
            vmm_log!("the lifeline closed; stopping the VM");
            machine.finish(Outcome::Stopped);
        })
        .map_err(|e| format!("--lifeline-fd {fd}: {e}"))?;
    }

    if cfg.console == ConsoleMode::Stdio {
        let raw = terminal::enter_raw();
        let machine = machine.clone();
        std::thread::Builder::new()
            .name("console-in".into())
            .spawn(move || read_console(input, raw, &machine))?;
    }

    if cfg.verbose {
        vmm_log!(
            "{} vCPUs, {} MiB, kernel entry {:#x}, device tree {:#x}; guest starts at +{:.1} ms",
            cfg.cpus,
            cfg.mem_mib,
            loaded.entry,
            loaded.fdt_addr,
            start.elapsed().as_secs_f64() * 1000.0
        );
    }
    let control = match cfg.control_fd {
        Some(fd) => Some(
            Control::start(
                fd,
                Arc::new(VmTarget {
                    machine: machine.clone(),
                    mem: mem.clone(),
                    balloon: balloon_stats.clone(),
                    resets,
                    cpus: cfg.cpus as u64,
                    feed: share_feed.clone(),
                    window: window.clone(),
                    disks: flush_disks
                        .iter()
                        .filter(|d| !d.is_read_only())
                        .cloned()
                        .collect(),
                }),
            )
            .map_err(|e| format!("--control-fd {fd}: {e}"))?,
        ),
        None => None,
    };
    if let Some(control) = &control {
        for disk in flush_disks.iter().filter(|d| !d.is_read_only()) {
            let notifier = control.notifier();
            disk.space().on_report(move |space| notifier.space(space));
        }
    }
    trace.mark(Mark::GuestEntry);
    let threads = machine.start(cfg.cpus);
    let mut outcome = machine.wait();
    let away = machine.ended();
    for t in threads {
        if away == 0 || t.is_finished() {
            let _ = t.join();
        }
    }
    if away > 0 {
        vmm_log!(
            "warning: {away} vCPU thread(s) were still inside a call on the Mac {} ms after \
             the machine ended, and were not waited for",
            fevvm::pause::END_GRACE.as_millis()
        );
        if outcome == Outcome::Reset {
            outcome = Outcome::Fault(format!(
                "the guest reset itself while {away} vCPU thread(s) were inside a call on the \
                 Mac that did not return; not restarted in place"
            ));
        }
    }
    if !out.flush(CONSOLE_FLUSH_LIMIT) {
        vmm_log!("console output was not fully written");
    }
    for (disk, d) in disks.iter().zip(&cfg.disks) {
        if !d.read_only {
            if let Err(e) = disk.sync() {
                vmm_log!("--disk {}: final sync failed: {e}", d.path.display());
            }
        }
    }
    if machine.unclaimed_mmio() > 0 {
        vmm_log!(
            "warning: the guest made {} accesses to addresses with no device",
            machine.unclaimed_mmio()
        );
    }
    if cfg.verbose {
        vmm_log!("{}", memory_report(&mem, &balloon_stats));
        for line in flush_report(&flush_disks) {
            vmm_log!("{line}");
        }
        vmm_log!("{}", steering.report());
        let pause = machine.pause_account();
        vmm_log!(
            "pause: {} made, {} vCPUs counted out while in a call on the Mac",
            pause.pauses,
            pause.claimed
        );
    }
    for line in fevvm::virtio::queue::watch_report(&mem, all_queues) {
        vmm_log!("{line}");
    }
    if cfg.verbose {
        let exits: Vec<String> = machine
            .exit_counts()
            .iter()
            .filter(|(_, n)| *n > 0)
            .map(|(kind, n)| format!("{kind:?} {n}"))
            .collect();
        vmm_log!(
            "{outcome:?} after {:.1} ms; exits: {}",
            start.elapsed().as_secs_f64() * 1000.0,
            exits.join(", ")
        );
    }
    if let Some(control) = control {
        if outcome == Outcome::Reset {
            control.reset(resets + 1, trace::now_ns());
        }
        control.close();
    }
    drop(vm);
    Ok(outcome)
}

struct VmTarget {
    machine: Arc<Machine>,
    mem: Arc<GuestMemory>,
    balloon: Arc<BalloonStats>,
    resets: u64,
    cpus: u64,
    feed: Option<Arc<fileshare::feed::Feed>>,
    window: Arc<DiskWindow>,
    disks: Vec<Arc<Disk>>,
}

struct DiskWindow {
    state: vmmctl::WindowState,
    reason: String,
    has_disk: bool,
    base: u64,
    image: Option<ImageWindow>,
}

impl DiskWindow {
    fn off(reason: &str) -> Self {
        Self {
            state: vmmctl::WindowState::Off,
            reason: reason.into(),
            has_disk: true,
            base: 0,
            image: None,
        }
    }

    fn failed(reason: String) -> Self {
        Self {
            state: vmmctl::WindowState::Failed,
            reason,
            has_disk: true,
            base: 0,
            image: None,
        }
    }

    fn map(vm: &Vm, disk: &Disk, ipa_bits: u32, ram_end: u64) -> Self {
        let Some((base, len)) = disk_window(ipa_bits, ram_end, disk.len()) else {
            return Self::off(&format!(
                "no room for one in a guest address space of {ipa_bits} bits"
            ));
        };
        let image = match disk.map_window(len) {
            Ok(w) => w,
            Err(e) => return Self::failed(format!("mmap: {e}")),
        };
        if let Err(e) = unsafe { vm.map_read_only(image.host(), base, len) } {
            return Self::failed(e.to_string());
        }
        Self {
            state: vmmctl::WindowState::On,
            reason: String::new(),
            has_disk: true,
            base,
            image: Some(image),
        }
    }

    fn region(&self) -> Option<(u64, u64)> {
        self.image.as_ref().map(|w| (self.base, w.len()))
    }

    fn line(&self) -> String {
        match &self.image {
            Some(w) => format!("read window of {} MiB at {:#x}", w.len() >> 20, self.base),
            None => format!("no read window: {}", self.reason),
        }
    }

    fn stats(&self, faults: u64) -> vmmctl::Window {
        let mut w = vmmctl::Window::new(self.state);
        w.reason = self.reason.clone();
        if let Some((base, len)) = self.region() {
            w.size_bytes = len;
            w.guest_addr = base;
        }
        w.faults = faults;
        w
    }
}

impl Drop for DiskWindow {
    fn drop(&mut self) {
        std::mem::forget(self.image.take());
    }
}

fn refusal(why: pause::Refused) -> vmmctl::RpcError {
    match why {
        pause::Refused::Stopping => vmmctl::RpcError::stopping(),
        pause::Refused::NotParked(left) => vmmctl::RpcError::new(
            vmmctl::code::FAILED,
            format!(
                "{left} vCPUs did not leave the guest within {} s; the machine is running",
                pause::PARK_LIMIT.as_secs()
            ),
        ),
    }
}

impl control::Target for VmTarget {
    fn pause(&self, arrived_ns: u64) -> Result<vmmctl::PauseResult, vmmctl::RpcError> {
        let paused = self.machine.pause().map_err(refusal)?;
        if paused.changed {
            vmm_log!("paused");
        }
        Ok(vmmctl::PauseResult {
            state: vmmctl::State::Paused,
            changed: paused.changed,
            vcpus: self.cpus,
            took_ns: if paused.changed {
                trace::now_ns().saturating_sub(arrived_ns)
            } else {
                0
            },
            paused_at_ns: hvf::ticks_to_ns(paused.at),
        })
    }

    fn resume(&self, arrived_ns: u64) -> Result<vmmctl::ResumeResult, vmmctl::RpcError> {
        let resumed = self
            .machine
            .resume(&|| {
                if let Some(feed) = &self.feed {
                    feed.resumed();
                }
            })
            .map_err(refusal)?;
        if resumed.changed {
            vmm_log!(
                "resumed after {:.3} s paused",
                hvf::ticks_to_ns(resumed.offset) as f64 / 1e9
            );
        }
        Ok(vmmctl::ResumeResult {
            state: vmmctl::State::Running,
            changed: resumed.changed,
            vcpus: self.cpus,
            took_ns: if resumed.changed {
                trace::now_ns().saturating_sub(arrived_ns)
            } else {
                0
            },
            offset_ns: hvf::ticks_to_ns(resumed.offset),
            total_offset_ns: hvf::ticks_to_ns(resumed.total),
        })
    }

    fn stats(&self) -> vmmctl::Stats {
        use std::sync::atomic::Ordering::Relaxed;
        let usage = self.mem.host_usage();
        let mut devices = std::collections::BTreeMap::new();
        let mut exits: std::collections::BTreeMap<String, u64> = self
            .machine
            .exit_counts()
            .into_iter()
            .map(|(kind, n)| (format!("{kind:?}").to_lowercase(), n))
            .collect();
        exits.insert("unclaimed_mmio".into(), self.machine.unclaimed_mmio());
        let pause = self.machine.pause_account();
        devices.insert(
            "pause".to_string(),
            [
                ("rekicks".to_string(), pause.rekicks),
                ("claimed".to_string(), pause.claimed),
            ]
            .into(),
        );
        devices.insert("exits".to_string(), exits);
        if let Some(disk) = self.disks.first() {
            devices.insert(
                "space".to_string(),
                disk.space()
                    .counters()
                    .into_iter()
                    .map(|(name, n)| (name.to_string(), n))
                    .collect(),
            );
        }
        devices.insert(
            "balloon".to_string(),
            [
                ("reports".to_string(), self.balloon.ranges.load(Relaxed)),
                ("settles".to_string(), self.balloon.settles.load(Relaxed)),
            ]
            .into(),
        );
        vmmctl::Stats {
            api_version: vmmctl::API_VERSION,
            build: BUILD.to_string(),
            state: if self.machine.stopping() {
                vmmctl::State::Stopping
            } else if pause.paused {
                vmmctl::State::Paused
            } else {
                vmmctl::State::Running
            },
            at_ns: trace::now_ns(),
            pauses: pause.pauses,
            total_offset_ns: hvf::ticks_to_ns(pause.total),
            paused_at_ns: if pause.paused {
                hvf::ticks_to_ns(pause.at)
            } else {
                0
            },
            resets: self.resets,
            vcpus: self
                .machine
                .vcpu_usage()
                .into_iter()
                .map(|u| vmmctl::Vcpu {
                    cpu_ns: u.cpu_ns,
                    exits: u.exits,
                })
                .collect(),
            memory: vmmctl::Memory {
                guest_bytes: self.mem.len(),
                footprint_bytes: usage.footprint,
                resident_bytes: usage.resident,
                compressed_bytes: usage.compressed,
                released_bytes: self.balloon.released.load(Relaxed),
                released_ranges: self.balloon.ranges.load(Relaxed),
            },
            devices,
            windows: self.window.has_disk.then(|| vmmctl::Windows {
                data_disk: Some(self.window.stats(self.machine.window_faults())),
                file_share: None,
            }),
        }
    }

    fn budget(&self, baseline: vmmctl::Baseline) -> vmmctl::BudgetResult {
        let held_requests = self
            .disks
            .iter()
            .map(|d| {
                d.space().baseline(
                    baseline.available_bytes,
                    baseline.image_bytes,
                    baseline.margin_bytes,
                )
            })
            .sum();
        vmmctl::BudgetResult { held_requests }
    }

    fn stopping(&self) -> bool {
        self.machine.stopping()
    }

    fn stop(&self) {
        vmm_log!("asked to stop on the control socket; stopping the VM");
        self.machine.finish(Outcome::Stopped);
    }

    fn kill(&self) {
        unsafe {
            libc::kill(libc::getpid(), libc::SIGKILL);
            libc::sleep(1);
            libc::_exit(137);
        }
    }
}

struct NoIrq;

impl fevvm::irq::IrqChip for NoIrq {
    fn set_spi(&self, _intid: u32, _level: bool) {}
}

extern "C" {
    fn setiopolicy_np(iotype: libc::c_int, scope: libc::c_int, policy: libc::c_int) -> libc::c_int;
    fn getiopolicy_np(iotype: libc::c_int, scope: libc::c_int) -> libc::c_int;
}
const IOPOL_TYPE_VFS_MATERIALIZE_DATALESS_FILES: libc::c_int = 3;
const IOPOL_SCOPE_PROCESS: libc::c_int = 0;
const IOPOL_MATERIALIZE_DATALESS_FILES_ON: libc::c_int = 2;

fn materialize_dataless_files() -> std::io::Result<()> {
    let set = unsafe {
        setiopolicy_np(
            IOPOL_TYPE_VFS_MATERIALIZE_DATALESS_FILES,
            IOPOL_SCOPE_PROCESS,
            IOPOL_MATERIALIZE_DATALESS_FILES_ON,
        )
    };
    if set != 0 {
        return Err(std::io::Error::last_os_error());
    }
    let now = unsafe {
        getiopolicy_np(
            IOPOL_TYPE_VFS_MATERIALIZE_DATALESS_FILES,
            IOPOL_SCOPE_PROCESS,
        )
    };
    if now != IOPOL_MATERIALIZE_DATALESS_FILES_ON {
        return Err(std::io::Error::other(format!(
            "the policy reads back as {now}"
        )));
    }
    Ok(())
}

fn flush_report(disks: &[Arc<Disk>]) -> Vec<String> {
    disks
        .iter()
        .enumerate()
        .filter(|(_, d)| !d.is_read_only())
        .flat_map(|(i, d)| {
            let mut lines = vec![format!("disk {i}: {}", d.flushes().report())];
            let space = d.space();
            if space.params.every > 0 || space.params.hold {
                lines.push(format!("disk {i}: {}", space.summary()));
            }
            lines
        })
        .collect()
}

fn memory_report(mem: &GuestMemory, balloon: &BalloonStats) -> String {
    use std::sync::atomic::Ordering::Relaxed;
    let usage = mem.host_usage();
    let mib = |bytes: u64| bytes >> 20;
    format!(
        "memory: footprint {} MiB, guest RAM resident {} MiB, compressed {} MiB of {} MiB; \
         released {} MiB of {} MiB reported, in {} ranges",
        mib(usage.footprint),
        mib(usage.resident),
        mib(usage.compressed),
        mib(mem.len()),
        mib(balloon.released.load(Relaxed)),
        mib(balloon.reported.load(Relaxed)),
        balloon.ranges.load(Relaxed)
    )
}

fn report_memory_on_signal(
    mem: Arc<GuestMemory>,
    balloon: Arc<BalloonStats>,
    disks: Vec<Arc<Disk>>,
    steering: Arc<Steering>,
    window: Arc<DiskWindow>,
) {
    let spawned = std::thread::Builder::new()
        .name("memory-report".into())
        .spawn(move || loop {
            let mut signal = 0;
            if unsafe { libc::sigwait(&report_signals(), &mut signal) } != 0 {
                return;
            }
            vmm_log!("{}", memory_report(&mem, &balloon));
            if let Some(path) = std::env::var_os("FEV_VMM_PAGE_DUMP") {
                match mem.page_states().and_then(|v| std::fs::write(&path, v)) {
                    Ok(()) => vmm_log!("memory: page states written to {}", path.to_string_lossy()),
                    Err(e) => vmm_log!("memory: page states not written: {e}"),
                }
            }
            for line in flush_report(&disks) {
                vmm_log!("{line}");
            }
            vmm_log!("{}", steering.report());
            if let Some(line) = fevvm::stage::report() {
                vmm_log!("{line}");
            }
            if window.has_disk {
                vmm_log!("data disk: {}", window.line());
            }
            for line in fevvm::virtio::queue::watch_report(&mem, true) {
                vmm_log!("{line}");
            }
            if signal == libc::SIGTERM || signal == libc::SIGALRM {
                fevvm::flush_log();
                std::process::exit(128 + signal);
            }
        });
    if let Err(e) = spawned {
        vmm_log!("memory reports on SIGUSR1 are unavailable: {e}");
        unsafe {
            let mut set: libc::sigset_t = std::mem::zeroed();
            libc::sigemptyset(&mut set);
            libc::sigaddset(&mut set, libc::SIGTERM);
            libc::sigaddset(&mut set, libc::SIGALRM);
            libc::pthread_sigmask(libc::SIG_UNBLOCK, &set, std::ptr::null_mut());
        }
    }
}

fn report_signals() -> libc::sigset_t {
    unsafe {
        let mut set: libc::sigset_t = std::mem::zeroed();
        libc::sigemptyset(&mut set);
        libc::sigaddset(&mut set, libc::SIGUSR1);
        libc::sigaddset(&mut set, libc::SIGINFO);
        libc::sigaddset(&mut set, libc::SIGTERM);
        libc::sigaddset(&mut set, libc::SIGALRM);
        set
    }
}

fn read_console(input: ConsoleInput, escapes: bool, machine: &Machine) {
    let mut stdin = std::io::stdin().lock();
    let mut buf = [0u8; 1024];
    let mut escaped = false;
    loop {
        let n = match stdin.read(&mut buf) {
            Ok(0) | Err(_) => return,
            Ok(n) => n,
        };
        if !escapes {
            input.push(&buf[..n]);
            continue;
        }
        let mut pass = Vec::with_capacity(n);
        for &b in &buf[..n] {
            if escaped {
                escaped = false;
                match b {
                    b'x' | b'X' => return machine.finish(Outcome::Stopped),
                    ESCAPE => pass.push(ESCAPE),
                    other => pass.extend([ESCAPE, other]),
                }
            } else if b == ESCAPE {
                escaped = true;
            } else {
                pass.push(b);
            }
        }
        input.push(&pass);
    }
}
