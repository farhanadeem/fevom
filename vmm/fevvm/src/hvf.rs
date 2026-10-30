use std::fmt;
use std::sync::Arc;

use hvf_sys as sys;

use crate::irq::IrqChip;
use crate::memory::GuestMemory;
use crate::virtio::balloon::{releasable, Releaser};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct HvError {
    pub call: &'static str,
    pub code: u32,
}

impl fmt::Display for HvError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let name = match self.code {
            0xfae9_4001 => "HV_ERROR",
            0xfae9_4002 => "HV_BUSY",
            0xfae9_4003 => "HV_BAD_ARGUMENT",
            0xfae9_4005 => "HV_NO_RESOURCES",
            0xfae9_4006 => "HV_NO_DEVICE",
            0xfae9_4007 => "HV_DENIED (is the binary signed? run tools/sign-dev.sh)",
            0xfae9_400f => "HV_UNSUPPORTED",
            _ => "unknown error",
        };
        write!(f, "{} failed: {:#x} {}", self.call, self.code, name)
    }
}

impl std::error::Error for HvError {}

pub type Result<T> = std::result::Result<T, HvError>;

fn check(call: &'static str, ret: sys::hv_return_t) -> Result<()> {
    if ret == sys::HV_SUCCESS as sys::hv_return_t {
        Ok(())
    } else {
        Err(HvError {
            call,
            code: ret as u32,
        })
    }
}

#[non_exhaustive]
pub struct Vm {
    pub redist_size_per_cpu: u64,
    pub spi_base: u32,
    pub spi_count: u32,
    pub vtimer_intid: u32,
}

impl Vm {
    pub fn max_ipa_bits() -> Result<u32> {
        let mut bits = 0u32;
        check("hv_vm_config_get_max_ipa_size", unsafe {
            sys::hv_vm_config_get_max_ipa_size(&mut bits)
        })?;
        Ok(bits)
    }

    pub fn new(gic_dist_base: u64, gic_redist_base: u64, ipa_bits: Option<u32>) -> Result<Self> {
        unsafe {
            match ipa_bits {
                Some(bits) => {
                    let vmcfg = sys::hv_vm_config_create();
                    let ret = check(
                        "hv_vm_config_set_ipa_size",
                        sys::hv_vm_config_set_ipa_size(vmcfg, bits),
                    )
                    .and_then(|()| check("hv_vm_create", sys::hv_vm_create(vmcfg)));
                    sys::os_release(vmcfg.cast());
                    ret?;
                }
                None => check("hv_vm_create", sys::hv_vm_create(std::ptr::null_mut()))?,
            }

            let cfg = sys::hv_gic_config_create();
            let ret = check(
                "hv_gic_config_set_distributor_base",
                sys::hv_gic_config_set_distributor_base(cfg, gic_dist_base),
            )
            .and_then(|()| {
                check(
                    "hv_gic_config_set_redistributor_base",
                    sys::hv_gic_config_set_redistributor_base(cfg, gic_redist_base),
                )
            })
            .and_then(|()| check("hv_gic_create", sys::hv_gic_create(cfg)));
            sys::os_release(cfg.cast());
            ret?;

            let mut redist_size = 0usize;
            check(
                "hv_gic_get_redistributor_size",
                sys::hv_gic_get_redistributor_size(&mut redist_size),
            )?;
            let (mut spi_base, mut spi_count) = (0u32, 0u32);
            check(
                "hv_gic_get_spi_interrupt_range",
                sys::hv_gic_get_spi_interrupt_range(&mut spi_base, &mut spi_count),
            )?;
            let mut vtimer_intid = 0u32;
            check(
                "hv_gic_get_intid",
                sys::hv_gic_get_intid(sys::HV_GIC_INT_EL1_VIRTUAL_TIMER, &mut vtimer_intid),
            )?;

            Ok(Self {
                redist_size_per_cpu: redist_size as u64,
                spi_base,
                spi_count,
                vtimer_intid,
            })
        }
    }

    pub fn max_vcpus() -> Result<u32> {
        let mut n = 0u32;
        check("hv_vm_get_max_vcpu_count", unsafe {
            sys::hv_vm_get_max_vcpu_count(&mut n)
        })?;
        Ok(n)
    }

    pub fn map_memory(&self, mem: &GuestMemory) -> Result<()> {
        let flags = (sys::HV_MEMORY_READ | sys::HV_MEMORY_WRITE | sys::HV_MEMORY_EXEC) as u64;
        check("hv_vm_map", unsafe {
            sys::hv_vm_map(
                mem.host_base().cast(),
                mem.base(),
                mem.len() as usize,
                flags,
            )
        })
    }

    pub unsafe fn map_read_only(&self, host: *mut u8, ipa: u64, len: u64) -> Result<()> {
        check("hv_vm_map", unsafe {
            sys::hv_vm_map(host.cast(), ipa, len as usize, sys::HV_MEMORY_READ as u64)
        })
    }

    pub fn irq_chip(&self) -> Arc<dyn IrqChip> {
        Arc::new(Gic)
    }

    pub fn releaser(&self, mem: Arc<GuestMemory>) -> Arc<dyn Releaser> {
        Arc::new(HvfReleaser { mem })
    }
}

impl Drop for Vm {
    fn drop(&mut self) {
        unsafe { sys::hv_vm_destroy() };
    }
}

const HOST_PAGE: u64 = 16 << 10;

struct HvfReleaser {
    mem: Arc<GuestMemory>,
}

impl Releaser for HvfReleaser {
    fn release(&self, addr: u64, len: u64) -> u64 {
        let Some((start, len)) = releasable(addr, len, self.mem.base(), self.mem.len(), HOST_PAGE)
        else {
            return 0;
        };
        let Ok(host) = self.mem.host_range(start, len) else {
            return 0;
        };
        let flags = (sys::HV_MEMORY_READ | sys::HV_MEMORY_WRITE | sys::HV_MEMORY_EXEC) as u64;
        unsafe {
            if sys::hv_vm_unmap(start, len as usize) != sys::HV_SUCCESS as sys::hv_return_t {
                return 0;
            }
            let fresh = libc::mmap(
                host.cast(),
                len as usize,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_PRIVATE | libc::MAP_ANON | libc::MAP_FIXED,
                -1,
                0,
            );
            let remapped = sys::hv_vm_map(host.cast(), start, len as usize, flags);
            if remapped != sys::HV_SUCCESS as sys::hv_return_t {
                crate::vmm_log!(
                    "could not map guest memory at {start:#x} back after releasing it ({:#x}); \
                     the guest will fault when it next uses that range",
                    remapped as u32
                );
                return 0;
            }
            if fresh == libc::MAP_FAILED {
                return 0;
            }
        }
        len
    }

    fn settle(&self) -> bool {
        self.mem.drop_host_translations()
    }
}

const GIC_SPI_FIRST: u32 = 32;
static ROUTE_REFUSED: std::sync::atomic::AtomicBool = std::sync::atomic::AtomicBool::new(false);

struct Gic;

impl IrqChip for Gic {
    fn set_spi(&self, intid: u32, level: bool) {
        let ret = unsafe { sys::hv_gic_set_spi(intid, level) };
        debug_assert_eq!(ret, 0, "hv_gic_set_spi({intid}, {level})");
    }

    fn route(&self, intid: u32, cpu: u32) {
        let Some(reg) = intid
            .checked_sub(GIC_SPI_FIRST)
            .and_then(|n| u16::try_from(8 * n).ok())
            .and_then(|n| sys::HV_GIC_DISTRIBUTOR_REG_GICD_IROUTER32.checked_add(n))
        else {
            return;
        };
        let ret = unsafe { sys::hv_gic_set_distributor_reg(reg, crate::machine::mpidr_for(cpu)) };
        if ret != 0 && !ROUTE_REFUSED.swap(true, std::sync::atomic::Ordering::Relaxed) {
            crate::vmm_log!(
                "warning: the interrupt controller refused a route for interrupt {intid} to \
                 vcpu{cpu} (status {ret:#x}); interrupts stay where they are"
            );
        }
    }
}

extern "C" {
    fn mach_absolute_time() -> u64;
    fn mach_timebase_info(info: *mut [u32; 2]) -> i32;
}

pub fn host_ticks() -> u64 {
    unsafe { mach_absolute_time() }
}

pub fn ticks_to_ns(ticks: u64) -> u64 {
    static TIMEBASE: std::sync::OnceLock<(u64, u64)> = std::sync::OnceLock::new();
    let (numer, denom) = *TIMEBASE.get_or_init(|| {
        let mut info = [0u32; 2];
        unsafe { mach_timebase_info(&mut info) };
        (info[0].max(1) as u64, info[1].max(1) as u64)
    });
    (ticks as u128 * numer as u128 / denom as u128) as u64
}

#[derive(Debug, Clone, Copy)]
pub enum Exit {
    Canceled,
    Exception { syndrome: u64, ipa: u64, va: u64 },
    VtimerActivated,
    Unknown(u32),
}

pub struct Vcpu {
    id: sys::hv_vcpu_t,
    exit: *const sys::hv_vcpu_exit_t,
}

#[derive(Clone, Copy)]
pub struct VcpuKick(sys::hv_vcpu_t);

impl VcpuKick {
    pub fn kick(self) {
        let mut id = self.0;
        unsafe { sys::hv_vcpus_exit(&mut id, 1) };
    }
}

pub use sys::{
    HV_SYS_REG_ELR_EL1 as SYS_ELR_EL1, HV_SYS_REG_ESR_EL1 as SYS_ESR_EL1,
    HV_SYS_REG_FAR_EL1 as SYS_FAR_EL1, HV_SYS_REG_ID_AA64MMFR0_EL1 as SYS_ID_AA64MMFR0_EL1,
    HV_SYS_REG_MPIDR_EL1 as SYS_MPIDR_EL1, HV_SYS_REG_SCTLR_EL1 as SYS_SCTLR_EL1,
    HV_SYS_REG_SPSR_EL1 as SYS_SPSR_EL1, HV_SYS_REG_VBAR_EL1 as SYS_VBAR_EL1,
};

impl Vcpu {
    pub fn new() -> Result<Self> {
        let mut id = 0;
        let mut exit = std::ptr::null_mut();
        check("hv_vcpu_create", unsafe {
            sys::hv_vcpu_create(&mut id, &mut exit, std::ptr::null_mut())
        })?;
        Ok(Self { id, exit })
    }

    pub fn kicker(&self) -> VcpuKick {
        VcpuKick(self.id)
    }

    pub fn run(&mut self) -> Result<Exit> {
        check("hv_vcpu_run", unsafe { sys::hv_vcpu_run(self.id) })?;
        let exit = unsafe { &*self.exit };
        Ok(match exit.reason {
            sys::HV_EXIT_REASON_CANCELED => Exit::Canceled,
            sys::HV_EXIT_REASON_EXCEPTION => Exit::Exception {
                syndrome: exit.exception.syndrome,
                ipa: exit.exception.physical_address,
                va: exit.exception.virtual_address,
            },
            sys::HV_EXIT_REASON_VTIMER_ACTIVATED => Exit::VtimerActivated,
            other => Exit::Unknown(other),
        })
    }

    pub fn x(&self, n: u8) -> Result<u64> {
        if n >= 31 {
            return Ok(0);
        }
        let mut v = 0;
        check("hv_vcpu_get_reg", unsafe {
            sys::hv_vcpu_get_reg(self.id, sys::HV_REG_X0 + n as u32, &mut v)
        })?;
        Ok(v)
    }

    pub fn set_x(&mut self, n: u8, value: u64) -> Result<()> {
        if n >= 31 {
            return Ok(());
        }
        check("hv_vcpu_set_reg", unsafe {
            sys::hv_vcpu_set_reg(self.id, sys::HV_REG_X0 + n as u32, value)
        })
    }

    pub fn pc(&self) -> Result<u64> {
        let mut v = 0;
        check("hv_vcpu_get_reg(PC)", unsafe {
            sys::hv_vcpu_get_reg(self.id, sys::HV_REG_PC, &mut v)
        })?;
        Ok(v)
    }

    pub fn set_pc(&mut self, value: u64) -> Result<()> {
        check("hv_vcpu_set_reg(PC)", unsafe {
            sys::hv_vcpu_set_reg(self.id, sys::HV_REG_PC, value)
        })
    }

    pub fn pstate(&self) -> Result<u64> {
        let mut v = 0;
        check("hv_vcpu_get_reg(CPSR)", unsafe {
            sys::hv_vcpu_get_reg(self.id, sys::HV_REG_CPSR, &mut v)
        })?;
        Ok(v)
    }

    pub fn set_pstate(&mut self, value: u64) -> Result<()> {
        check("hv_vcpu_set_reg(CPSR)", unsafe {
            sys::hv_vcpu_set_reg(self.id, sys::HV_REG_CPSR, value)
        })
    }

    pub fn vtimer_offset(&self) -> Result<u64> {
        let mut v = 0;
        check("hv_vcpu_get_vtimer_offset", unsafe {
            sys::hv_vcpu_get_vtimer_offset(self.id, &mut v)
        })?;
        Ok(v)
    }

    pub fn set_vtimer_offset(&mut self, ticks: u64) -> Result<()> {
        check("hv_vcpu_set_vtimer_offset", unsafe {
            sys::hv_vcpu_set_vtimer_offset(self.id, ticks)
        })
    }

    pub fn sys_reg(&self, reg: sys::hv_sys_reg_t) -> Result<u64> {
        let mut v = 0;
        check("hv_vcpu_get_sys_reg", unsafe {
            sys::hv_vcpu_get_sys_reg(self.id, reg, &mut v)
        })?;
        Ok(v)
    }

    pub fn set_sys_reg(&mut self, reg: sys::hv_sys_reg_t, value: u64) -> Result<()> {
        check("hv_vcpu_set_sys_reg", unsafe {
            sys::hv_vcpu_set_sys_reg(self.id, reg, value)
        })
    }
}

impl Drop for Vcpu {
    fn drop(&mut self) {
        unsafe { sys::hv_vcpu_destroy(self.id) };
    }
}
