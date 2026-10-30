pub const EC_WFX: u64 = 0x01;
pub const EC_HVC64: u64 = 0x16;
pub const EC_SMC64: u64 = 0x17;
pub const EC_SYSREG: u64 = 0x18;
pub const EC_INSN_ABORT_LOWER: u64 = 0x20;
pub const EC_DATA_ABORT_LOWER: u64 = 0x24;
pub const EC_BRK: u64 = 0x3c;

pub fn exception_class(syndrome: u64) -> u64 {
    (syndrome >> 26) & 0x3f
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct MmioAccess {
    pub write: bool,
    pub size: u8,
    pub reg: u8,
    pub sign_extend: bool,
    pub wide: bool,
}

impl MmioAccess {
    pub fn decode(syndrome: u64) -> Option<Self> {
        if exception_class(syndrome) != EC_DATA_ABORT_LOWER || syndrome & (1 << 24) == 0 {
            return None;
        }
        Some(Self {
            write: syndrome & (1 << 6) != 0,
            size: 1 << ((syndrome >> 22) & 3),
            reg: ((syndrome >> 16) & 0x1f) as u8,
            sign_extend: syndrome & (1 << 21) != 0,
            wide: syndrome & (1 << 15) != 0,
        })
    }

    pub fn store_value(&self, reg_value: u64) -> u64 {
        match self.size {
            8 => reg_value,
            n => reg_value & ((1u64 << (n * 8)) - 1),
        }
    }

    pub fn load_value(&self, device_value: u64) -> u64 {
        let bits = self.size as u32 * 8;
        let mut v = self.store_value(device_value);
        if self.sign_extend && bits < 64 {
            let shift = 64 - bits;
            v = ((v << shift) as i64 >> shift) as u64;
        }
        if !self.wide {
            v &= 0xffff_ffff;
        }
        v
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct InjectedAbort {
    pub esr: u64,
    pub pstate: u64,
    pub vector: u64,
}

const PSTATE_M_MASK: u64 = 0xf;
const PSTATE_EL0T: u64 = 0b0000;
const PSTATE_EL1T: u64 = 0b0100;
const PSTATE_EL1H: u64 = 0b0101;
const PSTATE_DAIF: u64 = 0x3c0;
const ESR_IL: u64 = 1 << 25;
const FSC_EXTERNAL_ABORT: u64 = 0x10;
const EC_INSN_ABORT_SAME: u64 = 0x21;

impl InjectedAbort {
    pub fn prefetch(pstate: u64) -> Option<Self> {
        let (ec, vector) = match pstate & (PSTATE_M_MASK | 0x10) {
            PSTATE_EL1T => (EC_INSN_ABORT_SAME, 0x000),
            PSTATE_EL1H => (EC_INSN_ABORT_SAME, 0x200),
            PSTATE_EL0T => (EC_INSN_ABORT_LOWER, 0x400),
            _ => return None,
        };
        Some(Self {
            esr: (ec << 26) | ESR_IL | FSC_EXTERNAL_ABORT,
            pstate: PSTATE_EL1H | PSTATE_DAIF,
            vector,
        })
    }
}

pub fn data_abort_is_write(syndrome: u64) -> bool {
    syndrome & (1 << 6) != 0
}

pub type SysRegEncoding = (u8, u8, u8, u8, u8);

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SysRegAccess {
    pub read: bool,
    pub reg: u8,
    pub encoding: SysRegEncoding,
}

const OSLAR_EL1: SysRegEncoding = (2, 0, 1, 0, 4);
const OSDLR_EL1: SysRegEncoding = (2, 0, 1, 3, 4);

impl SysRegAccess {
    pub fn is_known_inert(&self) -> bool {
        matches!(self.encoding, OSLAR_EL1 | OSDLR_EL1)
    }

    pub fn decode(syndrome: u64) -> Self {
        let f = |shift: u32, mask: u64| ((syndrome >> shift) & mask) as u8;
        Self {
            read: syndrome & 1 != 0,
            reg: f(5, 0x1f),
            encoding: (f(20, 3), f(14, 7), f(10, 0xf), f(1, 0xf), f(17, 7)),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn decodes_a_store_seen_under_hypervisor_framework() {
        let a = MmioAccess::decode(0x9300_0046).unwrap();
        assert!(a.write);
        assert_eq!(a.size, 1);
        assert_eq!(a.reg, 0);
        assert_eq!(a.store_value(0x1234_5641), 0x41);
    }

    #[test]
    fn decodes_width_register_and_direction() {
        let s = (0x24u64 << 26) | (1 << 25) | (1 << 24) | (2 << 22) | (5 << 16);
        let a = MmioAccess::decode(s).unwrap();
        assert_eq!((a.write, a.size, a.reg, a.wide), (false, 4, 5, false));
        assert_eq!(a.load_value(0xffff_ffff_8000_0001), 0x8000_0001);

        let s = (0x24u64 << 26) | (1 << 24) | (3 << 22) | (9 << 16) | (1 << 15) | (1 << 6);
        let a = MmioAccess::decode(s).unwrap();
        assert_eq!((a.write, a.size, a.reg, a.wide), (true, 8, 9, true));
        assert_eq!(a.store_value(u64::MAX), u64::MAX);
    }

    #[test]
    fn sign_extends_loads() {
        let s = (0x24u64 << 26) | (1 << 24) | (1 << 21) | (1 << 16) | (1 << 15);
        let a = MmioAccess::decode(s).unwrap();
        assert_eq!(a.load_value(0x80), 0xffff_ffff_ffff_ff80);
        assert_eq!(a.load_value(0x7f), 0x7f);
        let s = (0x24u64 << 26) | (1 << 24) | (1 << 22) | (1 << 21) | (1 << 16);
        let a = MmioAccess::decode(s).unwrap();
        assert_eq!(a.load_value(0x8000), 0xffff_8000);
    }

    #[test]
    fn a_window_abort_says_whether_it_was_a_write() {
        assert!(data_abort_is_write(
            (0x24u64 << 26) | (1 << 25) | (1 << 6) | 0x0f
        ));
        assert!(!data_abort_is_write((0x24u64 << 26) | (1 << 25) | 0x07));
    }

    #[test]
    fn refuses_syndromes_without_instruction_details() {
        assert_eq!(MmioAccess::decode((0x24u64 << 26) | (2 << 22)), None);
        assert_eq!(MmioAccess::decode((0x20u64 << 26) | (1 << 24)), None);
    }

    #[test]
    fn a_prefetch_abort_enters_the_right_vector_for_where_it_came_from() {
        let a = InjectedAbort::prefetch(0x3c5).unwrap();
        assert_eq!(a.vector, 0x200);
        assert_eq!(exception_class(a.esr), 0x21);
        assert_eq!(a.esr & 0x3f, 0x10, "synchronous external abort");
        assert_ne!(a.esr & (1 << 25), 0, "32-bit instruction");
        assert_eq!(a.pstate, 0x3c5, "EL1h with everything masked");
        assert_eq!(InjectedAbort::prefetch(0x004).unwrap().vector, 0x000);
        let u = InjectedAbort::prefetch(0x6000_0000).unwrap();
        assert_eq!((u.vector, exception_class(u.esr)), (0x400, 0x20));
        assert_eq!(InjectedAbort::prefetch(0x10), None);
        assert_eq!(InjectedAbort::prefetch(0x9), None);
    }

    #[test]
    fn decodes_system_register_traps() {
        let s = (0x18u64 << 26) | (3 << 20) | (1 << 17) | (1 << 10) | (3 << 5) | 1;
        let a = SysRegAccess::decode(s);
        assert!(a.read);
        assert_eq!(a.reg, 3);
        assert_eq!(a.encoding, (3, 0, 1, 0, 1));
        assert!(!a.is_known_inert());
    }

    #[test]
    fn knows_the_debug_lock_registers_the_kernel_writes_at_boot() {
        let oslar = (0x18u64 << 26) | (2 << 20) | (4 << 17) | (1 << 10);
        let osdlr = oslar | (3 << 1);
        assert!(SysRegAccess::decode(oslar).is_known_inert());
        assert!(SysRegAccess::decode(osdlr).is_known_inert());
        assert!(!SysRegAccess::decode(oslar).read);
    }
}
