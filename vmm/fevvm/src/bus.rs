use std::sync::Arc;

pub trait MmioDevice: Send + Sync {
    fn read(&self, offset: u64, size: u8) -> u64;
    fn write(&self, offset: u64, size: u8, value: u64);
}

struct Entry {
    base: u64,
    len: u64,
    device: Arc<dyn MmioDevice>,
}

#[derive(Default)]
pub struct Bus {
    entries: Vec<Entry>,
}

impl Bus {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn insert(&mut self, base: u64, len: u64, device: Arc<dyn MmioDevice>) {
        assert!(len > 0 && base.checked_add(len).is_some());
        for e in &self.entries {
            assert!(
                base + len <= e.base || e.base + e.len <= base,
                "MMIO range {base:#x}+{len:#x} overlaps {:#x}+{:#x}",
                e.base,
                e.len
            );
        }
        let at = self.entries.partition_point(|e| e.base < base);
        self.entries.insert(at, Entry { base, len, device });
    }

    fn find(&self, addr: u64) -> Option<(&dyn MmioDevice, u64)> {
        let at = self.entries.partition_point(|e| e.base <= addr);
        let e = self.entries.get(at.checked_sub(1)?)?;
        let offset = addr - e.base;
        (offset < e.len).then_some((&*e.device, offset))
    }

    pub fn read(&self, addr: u64, size: u8) -> Option<u64> {
        self.find(addr).map(|(d, offset)| d.read(offset, size))
    }

    pub fn write(&self, addr: u64, size: u8, value: u64) -> bool {
        match self.find(addr) {
            Some((d, offset)) => {
                d.write(offset, size, value);
                true
            }
            None => false,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicU64, Ordering};

    struct Reg(AtomicU64);

    impl MmioDevice for Reg {
        fn read(&self, offset: u64, _size: u8) -> u64 {
            self.0.load(Ordering::Relaxed) + offset
        }
        fn write(&self, _offset: u64, _size: u8, value: u64) {
            self.0.store(value, Ordering::Relaxed);
        }
    }

    #[test]
    fn dispatches_by_address() {
        let mut bus = Bus::new();
        bus.insert(0x2000, 0x1000, Arc::new(Reg(AtomicU64::new(200))));
        bus.insert(0x1000, 0x1000, Arc::new(Reg(AtomicU64::new(100))));
        assert_eq!(bus.read(0x1000, 4), Some(100));
        assert_eq!(bus.read(0x1fff, 1), Some(100 + 0xfff));
        assert_eq!(bus.read(0x2004, 4), Some(204));
        assert_eq!(bus.read(0x0fff, 4), None);
        assert_eq!(bus.read(0x3000, 4), None);
        assert!(bus.write(0x2000, 4, 7));
        assert_eq!(bus.read(0x2000, 4), Some(7));
        assert!(!bus.write(0x4000, 4, 7));
    }

    #[test]
    #[should_panic(expected = "overlaps")]
    fn refuses_overlap() {
        let mut bus = Bus::new();
        bus.insert(0x1000, 0x1000, Arc::new(Reg(AtomicU64::new(0))));
        bus.insert(0x1800, 0x1000, Arc::new(Reg(AtomicU64::new(0))));
    }
}
