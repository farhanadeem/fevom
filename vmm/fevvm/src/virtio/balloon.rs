use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;

use super::{read_config_bytes, Activation, VirtioDevice};

const DEVICE_ID: u32 = 5;

const F_STATS_VQ: u64 = 1 << 1;
const F_FREE_PAGE_HINT: u64 = 1 << 3;
const F_PAGE_POISON: u64 = 1 << 4;
const F_REPORTING: u64 = 1 << 5;

const Q_INFLATE: u32 = 0;
const Q_DEFLATE: u32 = 1;
const Q_FREE_PAGE: u32 = 3;
const Q_REPORTING: u32 = 4;
const QUEUE_SIZES: [u16; 5] = [128, 128, 16, 128, 128];

pub trait Releaser: Send + Sync {
    fn release(&self, addr: u64, len: u64) -> u64;

    fn settle(&self) -> bool {
        false
    }
}

#[derive(Default)]
pub struct BalloonStats {
    pub released: AtomicU64,
    pub ranges: AtomicU64,
    pub reported: AtomicU64,
    pub settles: AtomicU64,
}

pub struct Balloon {
    releaser: Arc<dyn Releaser>,
    stats: Arc<BalloonStats>,
    active: Option<Activation>,
    actual: u32,
    poison: u32,
    poison_known: bool,
}

impl Balloon {
    pub fn new(releaser: Arc<dyn Releaser>) -> (Self, Arc<BalloonStats>) {
        let stats = Arc::new(BalloonStats::default());
        (
            Self {
                releaser,
                stats: stats.clone(),
                active: None,
                actual: 0,
                poison: 0,
                poison_known: false,
            },
            stats,
        )
    }

    fn config(&self) -> [u8; 16] {
        let mut c = [0u8; 16];
        c[4..8].copy_from_slice(&self.actual.to_le_bytes());
        c[12..16].copy_from_slice(&self.poison.to_le_bytes());
        c
    }

    fn may_release(&self) -> bool {
        !self.poison_known || self.poison == 0
    }
}

impl VirtioDevice for Balloon {
    fn device_id(&self) -> u32 {
        DEVICE_ID
    }

    fn features(&self) -> u64 {
        F_STATS_VQ | F_FREE_PAGE_HINT | F_PAGE_POISON | F_REPORTING
    }

    fn queue_max_sizes(&self) -> &[u16] {
        &QUEUE_SIZES
    }

    fn read_config(&self, offset: u64, data: &mut [u8]) {
        read_config_bytes(&self.config(), offset, data);
    }

    fn write_config(&mut self, offset: u64, data: &[u8]) {
        let Ok(value) = <[u8; 4]>::try_from(data) else {
            return;
        };
        match offset {
            4 => self.actual = u32::from_le_bytes(value),
            12 => self.poison = u32::from_le_bytes(value),
            _ => {}
        }
    }

    fn activate(&mut self, activation: Activation) {
        self.poison_known = activation.features & F_PAGE_POISON != 0;
        self.active = Some(activation);
    }

    fn notify(&mut self, queue: u32) {
        let may_release = self.may_release();
        let Some(a) = self.active.as_mut() else {
            return;
        };
        let Some(q) = a.queues.get_mut(queue as usize) else {
            return;
        };
        let (releaser, stats) = (&self.releaser, &self.stats);
        let result = match queue {
            Q_REPORTING => {
                let mut freed_any = false;
                let drained = q.drain(&a.mem, |chain| {
                    for b in chain.writable.iter().chain(&chain.readable) {
                        stats.ranges.fetch_add(1, Ordering::Relaxed);
                        stats.reported.fetch_add(b.len as u64, Ordering::Relaxed);
                        if may_release {
                            let freed = releaser.release(b.addr, b.len as u64);
                            stats.released.fetch_add(freed, Ordering::Relaxed);
                            freed_any |= freed > 0;
                        }
                    }
                    0
                });
                if freed_any && releaser.settle() {
                    stats.settles.fetch_add(1, Ordering::Relaxed);
                }
                drained
            }
            Q_INFLATE | Q_DEFLATE | Q_FREE_PAGE => q.drain(&a.mem, |_| 0),
            _ => return,
        };
        match result {
            Ok(true) => a.irq.signal_used(),
            Ok(false) => {}
            Err(e) => a.irq.fail("virtio-balloon", &e),
        }
    }

    fn reset(&mut self) {
        self.active = None;
        self.actual = 0;
        self.poison = 0;
        self.poison_known = false;
    }
}

pub fn releasable(
    addr: u64,
    len: u64,
    ram_base: u64,
    ram_len: u64,
    host_page: u64,
) -> Option<(u64, u64)> {
    let end = addr.checked_add(len)?;
    if addr < ram_base || end > ram_base.checked_add(ram_len)? {
        return None;
    }
    let start = addr.checked_next_multiple_of(host_page)?;
    let stop = end / host_page * host_page;
    (start < stop).then(|| (start, stop - start))
}

#[cfg(test)]
mod tests {
    use super::super::queue::testing::{memory, Driver};
    use super::super::testing::{interrupt, INTID};
    use super::*;
    use std::sync::Mutex;

    const BASE: u64 = 0x4000_0000;

    #[derive(Default)]
    struct Recorder(Mutex<Vec<(u64, u64)>>, AtomicU64);

    impl Releaser for Recorder {
        fn release(&self, addr: u64, len: u64) -> u64 {
            self.0.lock().unwrap().push((addr, len));
            len
        }
        fn settle(&self) -> bool {
            self.1.fetch_add(1, Ordering::Relaxed);
            true
        }
    }

    struct Fixture {
        mem: Arc<crate::memory::GuestMemory>,
        drivers: Vec<Driver>,
        dev: Balloon,
        stats: Arc<BalloonStats>,
        released: Arc<Recorder>,
        chip: Arc<crate::irq::testing::RecordingChip>,
    }

    fn fixture(features: u64) -> Fixture {
        let mem = Arc::new(memory());
        let released = Arc::new(Recorder::default());
        let (mut dev, stats) = Balloon::new(released.clone());
        let (chip, irq) = interrupt();
        let mut drivers = Vec::new();
        let mut queues = Vec::new();
        for i in 0..5u64 {
            let (d, q) = Driver::new(&mem, BASE + i * 0x2000, 16);
            drivers.push(d);
            queues.push(q);
        }
        dev.activate(Activation {
            mem: mem.clone(),
            irq,
            queues,
            features,
        });
        Fixture {
            mem,
            drivers,
            dev,
            stats,
            released,
            chip,
        }
    }

    #[test]
    fn reported_ranges_are_released_and_acknowledged() {
        let mut f = fixture(F_REPORTING);
        let q = Q_REPORTING as usize;
        let h1 = f.drivers[q].add(
            &f.mem,
            &[
                (0x4020_0000, 2 << 20, true),
                (0x4080_0000, 4 << 20, true),
                (0x5000_0000, 2 << 20, true),
            ],
        );
        let h2 = f.drivers[q].add(&f.mem, &[(0x6000_0000, 2 << 20, true)]);
        f.dev.notify(Q_REPORTING);

        assert_eq!(
            *f.released.0.lock().unwrap(),
            [
                (0x4020_0000, 2 << 20),
                (0x4080_0000, 4 << 20),
                (0x5000_0000, 2 << 20),
                (0x6000_0000, 2 << 20)
            ]
        );
        assert_eq!(f.drivers[q].take_used(&f.mem), Some((h1, 0)));
        assert_eq!(f.drivers[q].take_used(&f.mem), Some((h2, 0)));
        assert!(f.chip.level(INTID));
        assert_eq!(f.stats.released.load(Ordering::Relaxed), 10 << 20);
        assert_eq!(f.stats.ranges.load(Ordering::Relaxed), 4);
        assert_eq!(f.released.1.load(Ordering::Relaxed), 1);
        assert_eq!(f.stats.settles.load(Ordering::Relaxed), 1);
        f.dev.notify(Q_REPORTING);
        assert_eq!(f.released.1.load(Ordering::Relaxed), 1);
    }

    #[test]
    fn a_guest_that_poisons_freed_pages_keeps_them() {
        let mut f = fixture(F_REPORTING | F_PAGE_POISON);
        f.dev.write_config(12, &0xaaaa_aaaau32.to_le_bytes());
        let q = Q_REPORTING as usize;
        let head = f.drivers[q].add(&f.mem, &[(0x4020_0000, 2 << 20, true)]);
        f.dev.notify(Q_REPORTING);
        assert!(f.released.0.lock().unwrap().is_empty());
        assert_eq!(
            f.drivers[q].take_used(&f.mem),
            Some((head, 0)),
            "still acknowledged"
        );
        assert_eq!(f.stats.released.load(Ordering::Relaxed), 0);
        assert_eq!(f.released.1.load(Ordering::Relaxed), 0, "nothing came back");

        f.dev.write_config(12, &0u32.to_le_bytes());
        f.drivers[q].add(&f.mem, &[(0x4020_0000, 2 << 20, true)]);
        f.dev.notify(Q_REPORTING);
        assert_eq!(f.released.0.lock().unwrap().len(), 1);
    }

    #[test]
    fn the_balloon_itself_is_never_inflated() {
        let mut f = fixture(F_REPORTING);
        assert_eq!(f.dev.config()[0..4], [0; 4], "target size is zero");
        for q in [Q_INFLATE, Q_DEFLATE, Q_FREE_PAGE] {
            let head = f.drivers[q as usize].add(&f.mem, &[(BASE + 0x20000, 64, false)]);
            f.dev.notify(q);
            assert_eq!(f.drivers[q as usize].take_used(&f.mem), Some((head, 0)));
        }
        assert!(f.released.0.lock().unwrap().is_empty());
        f.drivers[2].add(&f.mem, &[(BASE + 0x20000, 64, true)]);
        f.dev.notify(2);
        assert_eq!(f.drivers[2].take_used(&f.mem), None);
        f.dev.notify(9);
    }

    #[test]
    fn config_and_reset() {
        let mut f = fixture(F_REPORTING | F_PAGE_POISON);
        assert_eq!(f.dev.features() & F_REPORTING, F_REPORTING);
        assert_eq!(f.dev.queue_max_sizes().len(), 5);
        f.dev.write_config(4, &7u32.to_le_bytes());
        f.dev.write_config(12, &9u32.to_le_bytes());
        f.dev.write_config(4, &[1, 2]);
        let mut c = [0u8; 16];
        f.dev.read_config(0, &mut c);
        assert_eq!(
            (&c[4..8], &c[12..16]),
            (&7u32.to_le_bytes()[..], &9u32.to_le_bytes()[..])
        );
        f.dev.reset();
        assert_eq!(f.dev.config(), [0; 16]);
        assert!(f.dev.may_release());
    }

    #[test]
    fn only_whole_host_pages_inside_ram_are_releasable() {
        const RAM: u64 = 0x4000_0000;
        const LEN: u64 = 1 << 30;
        const PAGE: u64 = 16 << 10;
        let r = |addr, len| releasable(addr, len, RAM, LEN, PAGE);
        assert_eq!(
            r(RAM + (2 << 20), 2 << 20),
            Some((RAM + (2 << 20), 2 << 20))
        );
        assert_eq!(r(RAM, LEN), Some((RAM, LEN)));
        assert_eq!(r(RAM + 0x1000, 0x10000), Some((RAM + 0x4000, 0xc000)));
        assert_eq!(r(RAM + 0x1000, 0x4000), None);
        assert_eq!(r(RAM, 0), None);
        assert_eq!(r(RAM - 0x4000, 0x8000), None);
        assert_eq!(r(RAM + LEN - 0x4000, 0x8000), None);
        assert_eq!(r(0, 0x4000), None);
        assert_eq!(
            r(0x0800_0000, 0x10000),
            None,
            "the interrupt controller is not RAM"
        );
        assert_eq!(r(u64::MAX - 0x1000, 0x4000), None);
        assert_eq!(r(RAM, u64::MAX), None);
    }

    #[test]
    fn a_malformed_reporting_queue_stops_the_device() {
        let mut f = fixture(F_REPORTING);
        f.drivers[Q_REPORTING as usize].publish(&f.mem, 99);
        f.dev.notify(Q_REPORTING);
        assert!(f.dev.active.as_ref().unwrap().irq.failed());
        assert!(f.released.0.lock().unwrap().is_empty());
    }
}
