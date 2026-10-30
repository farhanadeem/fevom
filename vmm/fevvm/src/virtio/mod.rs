pub mod balloon;
pub mod blk;
pub mod console;
pub mod fs;
pub mod net;
pub mod netlink;
pub mod queue;
pub mod rng;
pub mod vsock;

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use crate::bus::MmioDevice;
use crate::irq::IrqLine;
use crate::memory::GuestMemory;
use queue::Queue;

pub const F_VERSION_1: u64 = 1 << 32;
pub const F_INDIRECT_DESC: u64 = 1 << 28;
pub const F_EVENT_IDX: u64 = 1 << 29;
const TRANSPORT_FEATURES: u64 = F_VERSION_1 | F_INDIRECT_DESC | F_EVENT_IDX;

const MAGIC: u32 = 0x7472_6976;
const MMIO_VERSION: u32 = 2;
const VENDOR: u32 = 0x4854_5242;

const STATUS_FEATURES_OK: u32 = 8;
const STATUS_DRIVER_OK: u32 = 4;
const STATUS_NEEDS_RESET: u32 = 0x40;

const INT_USED_RING: u32 = 1;
const INT_CONFIG: u32 = 2;

const R_MAGIC: u64 = 0x000;
const R_VERSION: u64 = 0x004;
const R_DEVICE_ID: u64 = 0x008;
const R_VENDOR_ID: u64 = 0x00c;
const R_DEVICE_FEATURES: u64 = 0x010;
const R_DEVICE_FEATURES_SEL: u64 = 0x014;
const R_DRIVER_FEATURES: u64 = 0x020;
const R_DRIVER_FEATURES_SEL: u64 = 0x024;
const R_QUEUE_SEL: u64 = 0x030;
const R_QUEUE_NUM_MAX: u64 = 0x034;
const R_QUEUE_NUM: u64 = 0x038;
const R_QUEUE_READY: u64 = 0x044;
const R_QUEUE_NOTIFY: u64 = 0x050;
const R_INTERRUPT_STATUS: u64 = 0x060;
const R_INTERRUPT_ACK: u64 = 0x064;
const R_STATUS: u64 = 0x070;
const R_QUEUE_DESC_LOW: u64 = 0x080;
const R_QUEUE_DESC_HIGH: u64 = 0x084;
const R_QUEUE_DRIVER_LOW: u64 = 0x090;
const R_QUEUE_DRIVER_HIGH: u64 = 0x094;
const R_QUEUE_DEVICE_LOW: u64 = 0x0a0;
const R_QUEUE_DEVICE_HIGH: u64 = 0x0a4;
const R_SHM_SEL: u64 = 0x0ac;
const R_SHM_LEN_LOW: u64 = 0x0b0;
const R_SHM_LEN_HIGH: u64 = 0x0b4;
const R_SHM_BASE_LOW: u64 = 0x0b8;
const R_SHM_BASE_HIGH: u64 = 0x0bc;
const R_CONFIG_GENERATION: u64 = 0x0fc;
const R_CONFIG: u64 = 0x100;

pub struct Interrupt {
    status: Mutex<u32>,
    needs_reset: AtomicBool,
    line: IrqLine,
}

impl Interrupt {
    pub fn new(line: IrqLine) -> Self {
        Self {
            status: Mutex::new(0),
            needs_reset: AtomicBool::new(false),
            line,
        }
    }

    fn raise(&self, bits: u32) {
        let mut status = self.status.lock().unwrap();
        *status |= bits;
        self.line.set(true);
    }

    pub fn signal_used(&self) {
        self.raise(INT_USED_RING);
    }

    pub fn signal_used_for(&self, sender: Option<u32>) {
        let mut status = self.status.lock().unwrap();
        *status |= INT_USED_RING;
        self.line.raise_for(sender);
    }

    pub fn fail(&self, device: &str, why: &dyn std::fmt::Display) {
        if !self.needs_reset.swap(true, Ordering::SeqCst) {
            crate::vmm_log!("{device}: {why}; device stopped until the guest resets it");
            self.raise(INT_CONFIG);
        }
    }

    pub fn failed(&self) -> bool {
        self.needs_reset.load(Ordering::SeqCst)
    }

    fn status(&self) -> u32 {
        *self.status.lock().unwrap()
    }

    fn ack(&self, bits: u32) {
        let mut status = self.status.lock().unwrap();
        *status &= !bits;
        if *status == 0 {
            self.line.set(false);
        }
    }

    fn reset(&self) {
        let mut status = self.status.lock().unwrap();
        *status = 0;
        self.needs_reset.store(false, Ordering::SeqCst);
        self.line.set(false);
        self.line.forget_route();
    }
}

pub struct Activation {
    pub mem: Arc<GuestMemory>,
    pub irq: Arc<Interrupt>,
    pub queues: Vec<Queue>,
    pub features: u64,
}

#[derive(Default)]
pub struct HoldFlags {
    pub held: AtomicBool,
    pub owed: AtomicBool,
}

struct Hold {
    irq: Arc<Interrupt>,
    flags: Arc<HoldFlags>,
    mem: Arc<GuestMemory>,
    avail_idx: u64,
    used_idx: u64,
    limit: std::time::Duration,
}

impl Hold {
    fn wait(self) {
        self.spin();
        self.flags.held.store(false, Ordering::SeqCst);
        if self.flags.owed.swap(false, Ordering::SeqCst) {
            self.irq.signal_used_for(crate::irq::current_vcpu());
        }
    }

    fn spin(&self) {
        let rings = || {
            Some((
                self.mem.read_u16(self.avail_idx).ok()?,
                self.mem.read_u16(self.used_idx).ok()?,
            ))
        };
        let Some((first, used)) = rings() else { return };
        if first.wrapping_sub(used) != 1 {
            return;
        }
        let start = std::time::Instant::now();
        loop {
            match rings() {
                Some((avail, used)) if avail == first && used != first => {}
                _ => return,
            }
            if start.elapsed() >= self.limit {
                return;
            }
            std::hint::spin_loop();
        }
    }
}

pub trait Inline: Send + Sync {
    fn notified(&self, queue: u32);
}

enum After {
    Hold(Hold),
    Serve(Arc<dyn Inline>, u32),
}

pub trait VirtioDevice: Send {
    fn device_id(&self) -> u32;
    fn features(&self) -> u64;
    fn queue_max_sizes(&self) -> &[u16];
    fn read_config(&self, _offset: u64, _data: &mut [u8]) {}
    fn write_config(&mut self, _offset: u64, _data: &[u8]) {}
    fn shm_region(&self, _id: u8) -> Option<(u64, u64)> {
        None
    }
    fn activate(&mut self, activation: Activation);
    fn notify(&mut self, queue: u32);
    fn hold(&mut self) -> Option<(std::time::Duration, Arc<HoldFlags>)> {
        None
    }
    fn inline(&mut self) -> Option<Arc<dyn Inline>> {
        None
    }
    fn reset(&mut self);
}

pub fn read_config_bytes(config: &[u8], offset: u64, data: &mut [u8]) {
    for (i, b) in data.iter_mut().enumerate() {
        *b = usize::try_from(offset)
            .ok()
            .and_then(|o| o.checked_add(i))
            .and_then(|o| config.get(o))
            .copied()
            .unwrap_or(0);
    }
}

struct Transport {
    device: Box<dyn VirtioDevice>,
    mem: Arc<GuestMemory>,
    irq: Arc<Interrupt>,
    status: u32,
    device_features_sel: u32,
    driver_features: u64,
    driver_features_sel: u32,
    queue_sel: u32,
    shm_sel: u32,
    queues: Vec<Queue>,
    active: bool,
    after: Option<After>,
    staged: bool,
}

pub struct MmioTransport {
    inner: Mutex<Transport>,
}

impl MmioTransport {
    pub fn new(device: Box<dyn VirtioDevice>, mem: Arc<GuestMemory>, line: IrqLine) -> Self {
        let queues = device
            .queue_max_sizes()
            .iter()
            .map(|s| Queue::new(*s))
            .collect();
        let staged = matches!(device.device_id(), 2 | 26);
        Self {
            inner: Mutex::new(Transport {
                device,
                mem,
                irq: Arc::new(Interrupt::new(line)),
                status: 0,
                device_features_sel: 0,
                driver_features: 0,
                driver_features_sel: 0,
                queue_sel: 0,
                shm_sel: 0,
                queues,
                active: false,
                after: None,
                staged,
            }),
        }
    }
}

fn set_low(v: &mut u64, low: u32) {
    *v = (*v & !0xffff_ffff) | low as u64;
}

fn set_high(v: &mut u64, high: u32) {
    *v = (*v & 0xffff_ffff) | (high as u64) << 32;
}

impl Transport {
    fn offered(&self) -> u64 {
        self.device.features() | TRANSPORT_FEATURES
    }

    fn queue(&mut self) -> Option<&mut Queue> {
        if self.active {
            return None;
        }
        self.queues.get_mut(self.queue_sel as usize)
    }

    fn watch_label(&self) -> String {
        format!(
            "type {} at {:#x} q",
            self.device.device_id(),
            self.queues.first().map_or(0, |q| q.desc_table)
        )
    }

    fn reset(&mut self) {
        if self.active {
            self.device.reset();
            queue::forget_watches(&self.watch_label());
        }
        self.active = false;
        self.status = 0;
        self.device_features_sel = 0;
        self.driver_features = 0;
        self.driver_features_sel = 0;
        self.queue_sel = 0;
        self.shm_sel = 0;
        for q in &mut self.queues {
            *q = Queue::new(q.max_size);
        }
        self.irq.reset();
    }

    fn set_status(&mut self, value: u32) {
        if value == 0 {
            self.reset();
            return;
        }
        let mut value = value;
        if value & STATUS_FEATURES_OK != 0 && self.status & STATUS_FEATURES_OK == 0 {
            let ok = self.driver_features & !self.offered() == 0
                && self.driver_features & F_VERSION_1 != 0;
            if !ok {
                value &= !STATUS_FEATURES_OK;
            }
        }
        self.status = value;
        if value & STATUS_DRIVER_OK != 0 && value & STATUS_FEATURES_OK != 0 && !self.active {
            self.active = true;
            let mut queues = self.queues.clone();
            let label = self.watch_label();
            for (n, q) in queues.iter_mut().enumerate() {
                q.event_idx = self.driver_features & F_EVENT_IDX != 0;
                q.indirect = self.driver_features & F_INDIRECT_DESC != 0;
                q.watch(format!("{label}{n}"));
            }
            self.device.activate(Activation {
                mem: self.mem.clone(),
                irq: self.irq.clone(),
                queues,
                features: self.driver_features & !TRANSPORT_FEATURES,
            });
        }
    }

    fn read(&mut self, offset: u64, size: u8) -> u64 {
        if offset >= R_CONFIG {
            let mut data = [0u8; 8];
            let n = (size as usize).min(8);
            self.device.read_config(offset - R_CONFIG, &mut data[..n]);
            return u64::from_le_bytes(data);
        }
        let selected = self.queues.get(self.queue_sel as usize);
        let value = match offset {
            R_MAGIC => MAGIC,
            R_VERSION => MMIO_VERSION,
            R_DEVICE_ID => self.device.device_id(),
            R_VENDOR_ID => VENDOR,
            R_DEVICE_FEATURES => match self.device_features_sel {
                0 => self.offered() as u32,
                1 => (self.offered() >> 32) as u32,
                _ => 0,
            },
            R_QUEUE_NUM_MAX => selected.map_or(0, |q| q.max_size as u32),
            R_QUEUE_READY => selected.map_or(0, |q| q.ready as u32),
            R_INTERRUPT_STATUS => {
                if self.staged {
                    crate::stage::status_read();
                }
                self.irq.status()
            }
            R_STATUS => {
                self.status
                    | if self.irq.failed() {
                        STATUS_NEEDS_RESET
                    } else {
                        0
                    }
            }
            R_CONFIG_GENERATION => 0,
            R_SHM_LEN_LOW | R_SHM_LEN_HIGH | R_SHM_BASE_LOW | R_SHM_BASE_HIGH => {
                let region = u8::try_from(self.shm_sel)
                    .ok()
                    .and_then(|id| self.device.shm_region(id));
                match (offset, region) {
                    (_, None) => u32::MAX,
                    (R_SHM_LEN_LOW, Some((_, len))) => len as u32,
                    (R_SHM_LEN_HIGH, Some((_, len))) => (len >> 32) as u32,
                    (R_SHM_BASE_LOW, Some((base, _))) => base as u32,
                    (_, Some((base, _))) => (base >> 32) as u32,
                }
            }
            _ => 0,
        };
        value as u64
    }

    fn write(&mut self, offset: u64, size: u8, value: u64) {
        if offset >= R_CONFIG {
            let n = (size as usize).min(8);
            self.device
                .write_config(offset - R_CONFIG, &value.to_le_bytes()[..n]);
            return;
        }
        let value = value as u32;
        match offset {
            R_DEVICE_FEATURES_SEL => self.device_features_sel = value,
            R_DRIVER_FEATURES_SEL => self.driver_features_sel = value,
            R_DRIVER_FEATURES if self.status & STATUS_FEATURES_OK == 0 => {
                match self.driver_features_sel {
                    0 => set_low(&mut self.driver_features, value),
                    1 => set_high(&mut self.driver_features, value),
                    _ => {}
                }
            }
            R_QUEUE_SEL => self.queue_sel = value,
            R_QUEUE_NUM => {
                if let Some(q) = self.queue() {
                    q.size = value.min(u16::MAX as u32) as u16;
                }
            }
            R_QUEUE_READY => {
                if let Some(q) = self.queue() {
                    q.ready = value & 1 != 0;
                }
            }
            R_QUEUE_DESC_LOW => {
                if let Some(q) = self.queue() {
                    set_low(&mut q.desc_table, value);
                }
            }
            R_QUEUE_DESC_HIGH => {
                if let Some(q) = self.queue() {
                    set_high(&mut q.desc_table, value);
                }
            }
            R_QUEUE_DRIVER_LOW => {
                if let Some(q) = self.queue() {
                    set_low(&mut q.avail_ring, value);
                }
            }
            R_QUEUE_DRIVER_HIGH => {
                if let Some(q) = self.queue() {
                    set_high(&mut q.avail_ring, value);
                }
            }
            R_QUEUE_DEVICE_LOW => {
                if let Some(q) = self.queue() {
                    set_low(&mut q.used_ring, value);
                }
            }
            R_QUEUE_DEVICE_HIGH => {
                if let Some(q) = self.queue() {
                    set_high(&mut q.used_ring, value);
                }
            }
            R_QUEUE_NOTIFY => {
                if self.active && !self.irq.failed() && (value as usize) < self.queues.len() {
                    if self.staged {
                        crate::stage::notify();
                    }
                    if let Some(server) = self.device.inline() {
                        self.after = Some(After::Serve(server, value));
                        return;
                    }
                    self.device.notify(value);
                    let q = &self.queues[value as usize];
                    self.after = match (
                        self.device.hold(),
                        q.avail_ring.checked_add(2),
                        q.used_ring.checked_add(2),
                    ) {
                        (Some((limit, flags)), Some(avail_idx), Some(used_idx)) => {
                            Some(After::Hold(Hold {
                                irq: self.irq.clone(),
                                flags,
                                mem: self.mem.clone(),
                                avail_idx,
                                used_idx,
                                limit,
                            }))
                        }
                        _ => None,
                    };
                }
            }
            R_INTERRUPT_ACK => {
                if self.staged {
                    crate::stage::ack();
                }
                self.irq.ack(value)
            }
            R_SHM_SEL => self.shm_sel = value,
            R_STATUS => self.set_status(value),
            _ => {}
        }
    }
}

impl MmioDevice for MmioTransport {
    fn read(&self, offset: u64, size: u8) -> u64 {
        self.inner.lock().unwrap().read(offset, size)
    }

    fn write(&self, offset: u64, size: u8, value: u64) {
        let after = {
            let mut inner = self.inner.lock().unwrap();
            inner.write(offset, size, value);
            inner.after.take()
        };
        match after {
            Some(After::Hold(hold)) => hold.wait(),
            Some(After::Serve(server, queue)) => server.notified(queue),
            None => {}
        }
    }
}

#[cfg(test)]
pub mod testing {
    use super::*;
    use crate::irq::testing::RecordingChip;

    pub const INTID: u32 = 48;

    pub fn interrupt() -> (Arc<RecordingChip>, Arc<Interrupt>) {
        let chip = Arc::new(RecordingChip::default());
        let steering = crate::irq::Steering::new(4, true);
        (0..4).for_each(|c| steering.cpu_on(c));
        let irq = Arc::new(Interrupt::new(steering.line(chip.clone(), INTID)));
        (chip, irq)
    }

    pub fn wait_high(chip: &RecordingChip) {
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
        while !chip.level(INTID) {
            assert!(std::time::Instant::now() < deadline, "no interrupt");
            std::thread::yield_now();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::queue::testing::{memory, Driver};
    use super::*;
    use crate::irq::testing::RecordingChip;
    use std::sync::mpsc;

    const BASE: u64 = 0x4000_0000;

    #[test]
    fn a_hold_ends_with_the_answer_or_the_bound_and_raises_what_is_owed() {
        use std::time::{Duration, Instant};
        let mem = Arc::new(memory());
        let (avail_idx, used_idx) = (BASE + 2, BASE + 0x1000 + 2);
        let hold = |limit: Duration| {
            let (chip, irq) = testing::interrupt();
            let flags = Arc::new(HoldFlags::default());
            flags.held.store(true, Ordering::SeqCst);
            let hold = Hold {
                irq,
                flags: flags.clone(),
                mem: mem.clone(),
                avail_idx,
                used_idx,
                limit,
            };
            (chip, flags, hold)
        };
        let set = |avail: u16, used: u16| {
            mem.write_u16(avail_idx, avail).unwrap();
            mem.write_u16(used_idx, used).unwrap();
        };

        set(7, 6);
        let (chip, flags, h) = hold(Duration::from_secs(5));
        let answering = {
            let (mem, flags) = (mem.clone(), flags.clone());
            std::thread::spawn(move || {
                std::thread::sleep(Duration::from_millis(20));
                assert!(flags.held.load(Ordering::SeqCst), "still held");
                flags.owed.store(true, Ordering::SeqCst);
                mem.write_u16(used_idx, 7).unwrap();
            })
        };
        let start = Instant::now();
        h.wait();
        let took = start.elapsed();
        answering.join().unwrap();
        assert!(
            took >= Duration::from_millis(20) && took < Duration::from_secs(4),
            "{took:?}"
        );
        assert!(chip.level(testing::INTID), "the vCPU raised it");
        assert!(!flags.held.load(Ordering::SeqCst) && !flags.owed.load(Ordering::SeqCst));

        set(7, 6);
        let (chip, flags, h) = hold(Duration::from_millis(30));
        let start = Instant::now();
        h.wait();
        let took = start.elapsed();
        assert!(
            took >= Duration::from_millis(30) && took < Duration::from_secs(2),
            "{took:?}"
        );
        assert!(!chip.level(testing::INTID));
        assert!(!flags.held.load(Ordering::SeqCst));

        for (avail, used) in [(9, 6), (7, 7)] {
            set(avail, used);
            let (_, flags, h) = hold(Duration::from_secs(5));
            let start = Instant::now();
            h.wait();
            assert!(start.elapsed() < Duration::from_secs(1), "{avail} {used}");
            assert!(!flags.held.load(Ordering::SeqCst));
        }
        set(7, 6);
        let (_, _, h) = hold(Duration::from_secs(5));
        let adding = {
            let mem = mem.clone();
            std::thread::spawn(move || {
                std::thread::sleep(Duration::from_millis(20));
                mem.write_u16(avail_idx, 8).unwrap();
            })
        };
        let start = Instant::now();
        h.wait();
        adding.join().unwrap();
        assert!(start.elapsed() < Duration::from_secs(4));

        let (_, flags, mut h) = hold(Duration::from_secs(5));
        h.used_idx = u64::MAX - 1;
        let start = Instant::now();
        h.wait();
        assert!(start.elapsed() < Duration::from_secs(1));
        assert!(!flags.held.load(Ordering::SeqCst));
    }

    struct Echo {
        active: Option<Activation>,
        events: mpsc::Sender<&'static str>,
        region: Option<(u64, u64)>,
    }

    impl VirtioDevice for Echo {
        fn device_id(&self) -> u32 {
            42
        }
        fn features(&self) -> u64 {
            0b101
        }
        fn queue_max_sizes(&self) -> &[u16] {
            &[8, 4]
        }
        fn read_config(&self, offset: u64, data: &mut [u8]) {
            read_config_bytes(&[1, 2, 3, 4, 5], offset, data);
        }
        fn shm_region(&self, id: u8) -> Option<(u64, u64)> {
            self.region.filter(|_| id == 0)
        }
        fn activate(&mut self, activation: Activation) {
            self.events.send("activate").unwrap();
            self.active = Some(activation);
        }
        fn notify(&mut self, queue: u32) {
            let a = self.active.as_mut().unwrap();
            let q = &mut a.queues[queue as usize];
            if q.drain(&a.mem, |c| c.head as u32).unwrap() {
                a.irq.signal_used();
            }
        }
        fn reset(&mut self) {
            self.events.send("reset").unwrap();
            self.active = None;
        }
    }

    struct Fixture {
        t: MmioTransport,
        mem: Arc<GuestMemory>,
        chip: Arc<RecordingChip>,
        events: mpsc::Receiver<&'static str>,
    }

    fn fixture() -> Fixture {
        fixture_with(None)
    }

    fn fixture_with(region: Option<(u64, u64)>) -> Fixture {
        let (tx, events) = mpsc::channel();
        let mem = Arc::new(memory());
        let chip = Arc::new(RecordingChip::default());
        let t = MmioTransport::new(
            Box::new(Echo {
                active: None,
                events: tx,
                region,
            }),
            mem.clone(),
            chip.line(40),
        );
        Fixture {
            t,
            mem,
            chip,
            events,
        }
    }

    fn r(t: &MmioTransport, offset: u64) -> u32 {
        t.read(offset, 4) as u32
    }

    fn w(t: &MmioTransport, offset: u64, value: u32) {
        t.write(offset, 4, value as u64);
    }

    fn bring_up(f: &Fixture) -> Driver {
        let t = &f.t;
        w(t, R_STATUS, 1);
        w(t, R_STATUS, 3);
        w(t, R_DRIVER_FEATURES_SEL, 0);
        w(t, R_DRIVER_FEATURES, 0b001);
        w(t, R_DRIVER_FEATURES_SEL, 1);
        w(t, R_DRIVER_FEATURES, 1);
        w(t, R_STATUS, 0xb);
        assert_eq!(r(t, R_STATUS) & STATUS_FEATURES_OK, STATUS_FEATURES_OK);

        let (drv, _) = Driver::new(&f.mem, BASE, 8);
        w(t, R_QUEUE_SEL, 0);
        w(t, R_QUEUE_NUM, 8);
        w(t, R_QUEUE_DESC_LOW, drv.desc as u32);
        w(t, R_QUEUE_DESC_HIGH, (drv.desc >> 32) as u32);
        w(t, R_QUEUE_DRIVER_LOW, drv.avail as u32);
        w(t, R_QUEUE_DRIVER_HIGH, (drv.avail >> 32) as u32);
        w(t, R_QUEUE_DEVICE_LOW, drv.used as u32);
        w(t, R_QUEUE_DEVICE_HIGH, (drv.used >> 32) as u32);
        w(t, R_QUEUE_READY, 1);
        w(t, R_STATUS, 0xf);
        drv
    }

    #[test]
    fn identifies_itself() {
        let f = fixture();
        assert_eq!(r(&f.t, R_MAGIC), 0x7472_6976);
        assert_eq!(r(&f.t, R_VERSION), 2);
        assert_eq!(r(&f.t, R_DEVICE_ID), 42);
        w(&f.t, R_DEVICE_FEATURES_SEL, 0);
        assert_eq!(
            r(&f.t, R_DEVICE_FEATURES),
            0b101 | (F_INDIRECT_DESC | F_EVENT_IDX) as u32
        );
        w(&f.t, R_DEVICE_FEATURES_SEL, 1);
        assert_eq!(r(&f.t, R_DEVICE_FEATURES), 1);
        w(&f.t, R_QUEUE_SEL, 1);
        assert_eq!(r(&f.t, R_QUEUE_NUM_MAX), 4);
        w(&f.t, R_QUEUE_SEL, 2);
        assert_eq!(r(&f.t, R_QUEUE_NUM_MAX), 0);
        assert_eq!(f.t.read(R_CONFIG + 1, 4), 0x0504_0302);
        assert_eq!(f.t.read(R_CONFIG + 4, 2), 5);
    }

    #[test]
    fn a_device_with_a_window_describes_its_region_and_no_other() {
        let (base, len) = (0x40_0000_0000u64, 0x12_3456_4000u64);
        let f = fixture_with(Some((base, len)));
        let read = |f: &Fixture| {
            (
                (r(&f.t, R_SHM_BASE_HIGH) as u64) << 32 | r(&f.t, R_SHM_BASE_LOW) as u64,
                (r(&f.t, R_SHM_LEN_HIGH) as u64) << 32 | r(&f.t, R_SHM_LEN_LOW) as u64,
            )
        };
        assert_eq!(read(&f), (base, len));
        for sel in [1, 2, 0xff, 0x100, u32::MAX] {
            w(&f.t, R_SHM_SEL, sel);
            assert_eq!(read(&f), (u64::MAX, u64::MAX), "region {sel}");
        }
        w(&f.t, R_SHM_SEL, 0);
        assert_eq!(read(&f), (base, len));
        bring_up(&f);
        assert_eq!(read(&f), (base, len));
        w(&f.t, R_SHM_SEL, 7);
        assert_eq!(read(&f), (u64::MAX, u64::MAX));
        w(&f.t, R_STATUS, 0);
        assert_eq!(read(&f), (base, len));
        assert_eq!(r(&f.t, R_SHM_SEL), 0, "the selector is write-only");
    }

    #[test]
    fn every_shared_memory_region_reads_as_absent() {
        let f = fixture();
        for sel in [0, 1, 0xff, u32::MAX] {
            w(&f.t, R_SHM_SEL, sel);
            let len = (r(&f.t, R_SHM_LEN_HIGH) as u64) << 32 | r(&f.t, R_SHM_LEN_LOW) as u64;
            assert_eq!(
                len,
                u64::MAX,
                "region {sel}: all ones is the only 'no region'"
            );
            let base = (r(&f.t, R_SHM_BASE_HIGH) as u64) << 32 | r(&f.t, R_SHM_BASE_LOW) as u64;
            assert_eq!(base, u64::MAX);
        }
        bring_up(&f);
        assert_eq!(r(&f.t, R_SHM_LEN_LOW), u32::MAX);
        assert_eq!(r(&f.t, R_SHM_SEL), 0);
        assert_eq!(r(&f.t, 0x0c0), 0);
    }

    #[test]
    fn negotiates_activates_notifies_and_interrupts() {
        let f = fixture();
        let mut drv = bring_up(&f);
        assert_eq!(f.events.try_recv(), Ok("activate"));

        let head = drv.add(&f.mem, &[(BASE + 0x8000, 4, false)]);
        w(&f.t, R_QUEUE_NOTIFY, 0);
        assert_eq!(drv.take_used(&f.mem), Some((head, head as u32)));
        assert_eq!(r(&f.t, R_INTERRUPT_STATUS), 1);
        assert!(f.chip.level(40));
        w(&f.t, R_INTERRUPT_ACK, 1);
        assert_eq!(r(&f.t, R_INTERRUPT_STATUS), 0);
        assert!(!f.chip.level(40));

        w(&f.t, R_QUEUE_NOTIFY, 9);
    }

    #[test]
    fn a_completion_is_routed_to_its_sender_only_while_the_status_is_clear() {
        let (chip, irq) = testing::interrupt();
        let id = testing::INTID;
        irq.signal_used_for(Some(2));
        assert_eq!((irq.status(), chip.level(id)), (INT_USED_RING, true));
        irq.signal_used_for(Some(3));
        assert_eq!(chip.routed(id), [2]);
        irq.fail("test", &"for the second status bit");
        irq.ack(INT_USED_RING);
        assert!(chip.level(id));
        irq.signal_used_for(Some(3));
        assert_eq!(chip.routed(id), [2]);
        irq.ack(INT_USED_RING | INT_CONFIG);
        assert!(!chip.level(id));
        irq.signal_used_for(Some(3));
        assert_eq!(chip.routed(id), [2, 3]);
        irq.ack(INT_USED_RING);
        irq.signal_used();
        irq.ack(INT_USED_RING);
        assert_eq!(chip.routed(id), [2, 3]);
        irq.reset();
        irq.signal_used_for(Some(3));
        assert_eq!(chip.routed(id), [2, 3, 3]);
        assert!(chip.routes.lock().unwrap().iter().all(|r| !r.while_high));
    }

    #[test]
    fn queues_get_the_ring_features_the_driver_accepted() {
        struct Probe(mpsc::Sender<(bool, bool, u64)>);
        impl VirtioDevice for Probe {
            fn device_id(&self) -> u32 {
                1
            }
            fn features(&self) -> u64 {
                0b1
            }
            fn queue_max_sizes(&self) -> &[u16] {
                &[8]
            }
            fn activate(&mut self, a: Activation) {
                let q = &a.queues[0];
                self.0.send((q.event_idx, q.indirect, a.features)).unwrap();
            }
            fn notify(&mut self, _queue: u32) {}
            fn reset(&mut self) {}
        }
        for (low, want) in [
            (0b1 | F_EVENT_IDX as u32, (true, false, 1)),
            (F_INDIRECT_DESC as u32, (false, true, 0)),
            (0, (false, false, 0)),
        ] {
            let (tx, rx) = mpsc::channel();
            let chip = Arc::new(RecordingChip::default());
            let t = MmioTransport::new(Box::new(Probe(tx)), Arc::new(memory()), chip.line(40));
            w(&t, R_STATUS, 3);
            w(&t, R_DRIVER_FEATURES_SEL, 0);
            w(&t, R_DRIVER_FEATURES, low);
            w(&t, R_DRIVER_FEATURES_SEL, 1);
            w(&t, R_DRIVER_FEATURES, 1);
            w(&t, R_STATUS, 0xb);
            w(&t, R_QUEUE_READY, 1);
            w(&t, R_STATUS, 0xf);
            assert_eq!(rx.try_recv(), Ok(want));
        }
    }

    #[test]
    fn refuses_legacy_drivers_and_unoffered_features() {
        let f = fixture();
        w(&f.t, R_STATUS, 3);
        w(&f.t, R_DRIVER_FEATURES_SEL, 0);
        w(&f.t, R_DRIVER_FEATURES, 0b001);
        w(&f.t, R_STATUS, 0xb);
        assert_eq!(r(&f.t, R_STATUS) & STATUS_FEATURES_OK, 0);

        let f = fixture();
        w(&f.t, R_STATUS, 3);
        w(&f.t, R_DRIVER_FEATURES_SEL, 0);
        w(&f.t, R_DRIVER_FEATURES, 0b010);
        w(&f.t, R_DRIVER_FEATURES_SEL, 1);
        w(&f.t, R_DRIVER_FEATURES, 1);
        w(&f.t, R_STATUS, 0xb);
        assert_eq!(r(&f.t, R_STATUS) & STATUS_FEATURES_OK, 0);
        w(&f.t, R_STATUS, 0xf);
        assert!(f.events.try_recv().is_err(), "must not activate");
    }

    #[test]
    fn reset_stops_the_device_and_clears_state() {
        let f = fixture();
        let mut drv = bring_up(&f);
        drv.add(&f.mem, &[(BASE + 0x8000, 4, false)]);
        w(&f.t, R_QUEUE_NOTIFY, 0);
        assert!(f.chip.level(40));

        w(&f.t, R_STATUS, 0);
        assert_eq!(
            f.events.try_iter().collect::<Vec<_>>(),
            ["activate", "reset"]
        );
        assert_eq!(r(&f.t, R_STATUS), 0);
        assert_eq!(r(&f.t, R_INTERRUPT_STATUS), 0);
        assert!(!f.chip.level(40));
        assert_eq!(r(&f.t, R_QUEUE_READY), 0);

        bring_up(&f);
        assert_eq!(f.events.try_recv(), Ok("activate"));
    }

    #[test]
    fn queue_setup_is_frozen_while_running() {
        let f = fixture();
        bring_up(&f);
        w(&f.t, R_QUEUE_SEL, 0);
        w(&f.t, R_QUEUE_READY, 0);
        assert_eq!(r(&f.t, R_QUEUE_READY), 1);
    }

    #[test]
    fn a_failed_device_reports_needs_reset() {
        let f = fixture();
        bring_up(&f);
        let irq = f.t.inner.lock().unwrap().irq.clone();
        irq.fail("test", &"malformed queue");
        assert_eq!(r(&f.t, R_STATUS) & STATUS_NEEDS_RESET, STATUS_NEEDS_RESET);
        assert_eq!(r(&f.t, R_INTERRUPT_STATUS), 2);
        w(&f.t, R_STATUS, 0);
        assert_eq!(r(&f.t, R_STATUS), 0);
    }
}
