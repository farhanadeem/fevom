use std::sync::atomic::{AtomicU32, Ordering};
use std::time::{SystemTime, UNIX_EPOCH};

use super::primecell_id;
use crate::bus::MmioDevice;

const DR: u64 = 0x00;
const LR: u64 = 0x08;
const CR: u64 = 0x0c;

const PERIPH_ID: [u8; 4] = [0x31, 0x10, 0x04, 0x00];

#[derive(Default)]
pub struct Pl031 {
    offset: AtomicU32,
}

impl Pl031 {
    pub fn new() -> Self {
        Self::default()
    }
}

fn host_seconds() -> u32 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_secs() as u32)
}

impl MmioDevice for Pl031 {
    fn read(&self, offset: u64, _size: u8) -> u64 {
        if let Some(id) = primecell_id(offset, PERIPH_ID) {
            return id;
        }
        (match offset {
            DR => host_seconds().wrapping_add(self.offset.load(Ordering::Relaxed)),
            LR => self.offset.load(Ordering::Relaxed),
            CR => 1,
            _ => 0,
        }) as u64
    }

    fn write(&self, offset: u64, _size: u8, value: u64) {
        if offset == LR {
            self.offset.store(
                (value as u32).wrapping_sub(host_seconds()),
                Ordering::Relaxed,
            );
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reads_host_time() {
        let rtc = Pl031::new();
        let now = host_seconds() as u64;
        let got = rtc.read(DR, 4);
        assert!((now..=now + 2).contains(&got));
        assert_eq!(rtc.read(CR, 4), 1);
    }

    #[test]
    fn keeps_a_time_the_guest_sets() {
        let rtc = Pl031::new();
        rtc.write(LR, 4, 1_000_000);
        let got = rtc.read(DR, 4);
        assert!((1_000_000..=1_000_002).contains(&got));
    }

    #[test]
    fn identifies_as_a_primecell_rtc() {
        let rtc = Pl031::new();
        let id: Vec<u64> = (0..8).map(|i| rtc.read(0xfe0 + i * 4, 4)).collect();
        assert_eq!(id, [0x31, 0x10, 0x04, 0x00, 0x0d, 0xf0, 0x05, 0xb1]);
    }
}
