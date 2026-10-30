use std::sync::{Arc, Mutex};

use super::primecell_id;
use crate::bus::MmioDevice;
use crate::console::ConsoleOut;
use crate::irq::IrqLine;

const DR: u64 = 0x00;
const FR: u64 = 0x18;
const IBRD: u64 = 0x24;
const FBRD: u64 = 0x28;
const LCR_H: u64 = 0x2c;
const CR: u64 = 0x30;
const IFLS: u64 = 0x34;
const IMSC: u64 = 0x38;
const RIS: u64 = 0x3c;
const MIS: u64 = 0x40;
const ICR: u64 = 0x44;

const FR_IDLE: u32 = 0x90;
const INT_TX: u32 = 1 << 5;
const PERIPH_ID: [u8; 4] = [0x11, 0x10, 0x14, 0x00];

#[derive(Default)]
struct Regs {
    ibrd: u32,
    fbrd: u32,
    lcr_h: u32,
    cr: u32,
    ifls: u32,
    imsc: u32,
    ris: u32,
}

pub struct Pl011 {
    out: Arc<ConsoleOut>,
    irq: IrqLine,
    regs: Mutex<Regs>,
}

impl Pl011 {
    pub fn new(out: Arc<ConsoleOut>, irq: IrqLine) -> Self {
        Self {
            out,
            irq,
            regs: Mutex::new(Regs {
                cr: 0x300,
                ifls: 0x12,
                ..Regs::default()
            }),
        }
    }
}

impl MmioDevice for Pl011 {
    fn read(&self, offset: u64, _size: u8) -> u64 {
        if let Some(id) = primecell_id(offset, PERIPH_ID) {
            return id;
        }
        let r = self.regs.lock().unwrap();
        (match offset {
            FR => FR_IDLE,
            IBRD => r.ibrd,
            FBRD => r.fbrd,
            LCR_H => r.lcr_h,
            CR => r.cr,
            IFLS => r.ifls,
            IMSC => r.imsc,
            RIS => r.ris,
            MIS => r.ris & r.imsc,
            _ => 0,
        }) as u64
    }

    fn write(&self, offset: u64, _size: u8, value: u64) {
        let value = value as u32;
        let mut r = self.regs.lock().unwrap();
        match offset {
            DR => {
                self.out.write(&[value as u8]);
                r.ris |= INT_TX;
            }
            IBRD => r.ibrd = value,
            FBRD => r.fbrd = value,
            LCR_H => r.lcr_h = value,
            CR => r.cr = value,
            IFLS => r.ifls = value,
            IMSC => r.imsc = value,
            ICR => r.ris &= !value,
            _ => return,
        }
        self.irq.set(r.ris & r.imsc != 0);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::console::testing::capture;
    use crate::irq::testing::RecordingChip;

    fn uart() -> (crate::console::testing::Capture, Arc<RecordingChip>, Pl011) {
        let (cap, out) = capture();
        let chip = Arc::new(RecordingChip::default());
        let uart = Pl011::new(out, chip.line(32));
        (cap, chip, uart)
    }

    #[test]
    fn transmits_and_always_has_room() {
        let (cap, _chip, uart) = uart();
        for b in b"ok\n" {
            assert_eq!(uart.read(FR, 4) & 0x20, 0, "TX FIFO must never be full");
            uart.write(DR, 4, *b as u64);
        }
        assert_eq!(cap.text(), "ok\n");
        assert_eq!(uart.read(DR, 4), 0);
        assert_eq!(uart.read(FR, 4) & 0x10, 0x10, "RX FIFO is always empty");
    }

    #[test]
    fn identifies_as_a_primecell_uart() {
        let (_cap, _chip, uart) = uart();
        let id: Vec<u64> = (0..8).map(|i| uart.read(0xfe0 + i * 4, 4)).collect();
        assert_eq!(id, [0x11, 0x10, 0x14, 0x00, 0x0d, 0xf0, 0x05, 0xb1]);
    }

    #[test]
    fn tx_interrupt_follows_mask_and_clear() {
        let (_cap, chip, uart) = uart();
        uart.write(DR, 4, b'x' as u64);
        assert!(!chip.level(32), "masked by default");
        uart.write(IMSC, 4, INT_TX as u64);
        assert!(chip.level(32));
        assert_eq!(uart.read(MIS, 4), INT_TX as u64);
        uart.write(ICR, 4, INT_TX as u64);
        assert!(!chip.level(32));
        uart.write(DR, 4, b'y' as u64);
        assert!(chip.level(32));
        uart.write(IMSC, 4, 0);
        assert!(!chip.level(32));
    }
}
