use std::cell::Cell;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, Weak};

pub trait IrqChip: Send + Sync {
    fn set_spi(&self, intid: u32, level: bool);
    fn route(&self, _intid: u32, _cpu: u32) {}
}

thread_local! {
    static VCPU: Cell<u32> = const { Cell::new(0) };
}

pub fn enter_vcpu(index: u32) {
    VCPU.with(|v| v.set(index + 1));
}

pub fn current_vcpu() -> Option<u32> {
    VCPU.with(|v| v.get()).checked_sub(1)
}

#[derive(Default)]
pub struct Sender(std::sync::atomic::AtomicU32);

impl Sender {
    pub fn note(&self) {
        self.0
            .store(current_vcpu().map_or(0, |c| c + 1), Ordering::Relaxed);
    }

    pub fn get(&self) -> Option<u32> {
        self.0.load(Ordering::Relaxed).checked_sub(1)
    }
}

pub struct Steering {
    enabled: bool,
    on: Vec<AtomicBool>,
    lines: Mutex<Vec<Weak<Shared>>>,
    routed: AtomicU64,
    moved: AtomicU64,
}

impl Steering {
    pub fn new(cpus: u32, enabled: bool) -> Arc<Self> {
        Arc::new(Self {
            enabled,
            on: (0..cpus).map(|_| AtomicBool::new(false)).collect(),
            lines: Mutex::new(Vec::new()),
            routed: AtomicU64::new(0),
            moved: AtomicU64::new(0),
        })
    }

    pub fn line(self: &Arc<Self>, chip: Arc<dyn IrqChip>, intid: u32) -> IrqLine {
        let line = IrqLine::build(chip, intid, self.enabled.then(|| self.clone()));
        self.lines
            .lock()
            .unwrap()
            .push(Arc::downgrade(&line.shared));
        line
    }

    fn is_on(&self, cpu: u32) -> bool {
        self.on
            .get(cpu as usize)
            .is_some_and(|c| c.load(Ordering::SeqCst))
    }

    pub fn cpu_on(&self, cpu: u32) {
        if let Some(c) = self.on.get(cpu as usize) {
            c.store(true, Ordering::SeqCst);
        }
    }

    pub fn cpu_off(&self, cpu: u32) {
        let Some(c) = self.on.get(cpu as usize) else {
            return;
        };
        c.store(false, Ordering::SeqCst);
        let lines: Vec<Arc<Shared>> = {
            let mut lines = self.lines.lock().unwrap();
            lines.retain(|l| l.strong_count() > 0);
            lines.iter().filter_map(Weak::upgrade).collect()
        };
        for line in lines {
            let mut state = line.state.lock().unwrap();
            if state.route != Some(cpu) {
                continue;
            }
            let Some(to) = (0..self.on.len() as u32).find(|c| self.is_on(*c)) else {
                continue;
            };
            if state.high {
                line.chip.set_spi(line.intid, false);
            }
            line.chip.route(line.intid, to);
            if state.high {
                line.chip.set_spi(line.intid, true);
            }
            state.route = Some(to);
            self.moved.fetch_add(1, Ordering::Relaxed);
        }
    }

    pub fn counts(&self) -> (u64, u64) {
        (
            self.routed.load(Ordering::Relaxed),
            self.moved.load(Ordering::Relaxed),
        )
    }

    pub fn report(&self) -> String {
        let high: Vec<String> = self
            .lines
            .lock()
            .unwrap()
            .iter()
            .filter_map(Weak::upgrade)
            .filter_map(|line| {
                let state = line.state.lock().unwrap();
                state.high.then(|| match state.route {
                    Some(cpu) => format!("{} (routed to vcpu{cpu})", line.intid),
                    None => format!("{} (the guest's route)", line.intid),
                })
            })
            .collect();
        let high = match high.is_empty() {
            true => "no line high".into(),
            false => format!("high now: {}", high.join(", ")),
        };
        if !self.enabled {
            return format!("interrupts: left where the guest routes them; {high}");
        }
        let (routed, moved) = self.counts();
        format!(
            "interrupts: routed to a request's sender {routed} times, moved off a CPU turned \
             off {moved} times; {high}"
        )
    }
}

struct State {
    high: bool,
    route: Option<u32>,
}

struct Shared {
    chip: Arc<dyn IrqChip>,
    intid: u32,
    steering: Option<Arc<Steering>>,
    state: Mutex<State>,
}

#[derive(Clone)]
pub struct IrqLine {
    shared: Arc<Shared>,
}

impl IrqLine {
    pub fn new(chip: Arc<dyn IrqChip>, intid: u32) -> Self {
        Self::build(chip, intid, None)
    }

    fn build(chip: Arc<dyn IrqChip>, intid: u32, steering: Option<Arc<Steering>>) -> Self {
        Self {
            shared: Arc::new(Shared {
                chip,
                intid,
                steering,
                state: Mutex::new(State {
                    high: false,
                    route: None,
                }),
            }),
        }
    }

    pub fn intid(&self) -> u32 {
        self.shared.intid
    }

    pub fn set(&self, level: bool) {
        let mut state = self.shared.state.lock().unwrap();
        self.shared.chip.set_spi(self.shared.intid, level);
        state.high = level;
    }

    pub fn raise_for(&self, sender: Option<u32>) {
        let line = &*self.shared;
        let mut state = line.state.lock().unwrap();
        if let (false, Some(steering), Some(cpu)) = (state.high, &line.steering, sender) {
            if state.route != Some(cpu) && steering.is_on(cpu) {
                line.chip.route(line.intid, cpu);
                state.route = Some(cpu);
                steering.routed.fetch_add(1, Ordering::Relaxed);
            }
        }
        let start = crate::stage::gic_start();
        line.chip.set_spi(line.intid, true);
        crate::stage::gic_done(start);
        state.high = true;
    }

    pub fn forget_route(&self) {
        self.shared.state.lock().unwrap().route = None;
    }
}

#[cfg(test)]
pub mod testing {
    use super::*;

    #[derive(Default)]
    pub struct RecordingChip {
        pub events: Mutex<Vec<(u32, bool)>>,
        pub routes: Mutex<Vec<Route>>,
    }

    #[derive(Debug, Clone, Copy, PartialEq, Eq)]
    pub struct Route {
        pub intid: u32,
        pub cpu: u32,
        pub while_high: bool,
    }

    impl IrqChip for RecordingChip {
        fn set_spi(&self, intid: u32, level: bool) {
            self.events.lock().unwrap().push((intid, level));
        }

        fn route(&self, intid: u32, cpu: u32) {
            let while_high = self.level(intid);
            self.routes.lock().unwrap().push(Route {
                intid,
                cpu,
                while_high,
            });
        }
    }

    impl RecordingChip {
        pub fn line(self: &Arc<Self>, intid: u32) -> IrqLine {
            IrqLine::new(self.clone(), intid)
        }

        pub fn level(&self, intid: u32) -> bool {
            let events = self.events.lock().unwrap();
            events
                .iter()
                .rev()
                .find(|(id, _)| *id == intid)
                .is_some_and(|(_, level)| *level)
        }

        pub fn routed(&self, intid: u32) -> Vec<u32> {
            let routes = self.routes.lock().unwrap();
            routes
                .iter()
                .filter(|r| r.intid == intid)
                .map(|r| r.cpu)
                .collect()
        }
    }
}

#[cfg(test)]
mod tests {
    use super::testing::RecordingChip;
    use super::*;

    const INTID: u32 = 50;

    fn steered(cpus: u32) -> (Arc<RecordingChip>, Arc<Steering>, IrqLine) {
        let chip = Arc::new(RecordingChip::default());
        let steering = Steering::new(cpus, true);
        for c in 0..cpus {
            steering.cpu_on(c);
        }
        let line = steering.line(chip.clone(), INTID);
        (chip, steering, line)
    }

    #[test]
    fn the_route_follows_the_sender_and_is_written_only_when_it_changes() {
        let (chip, steering, line) = steered(4);
        for cpu in [3, 3, 3, 1, 1, 3] {
            line.raise_for(Some(cpu));
            assert!(chip.level(INTID));
            line.set(false);
        }
        assert_eq!(chip.routed(INTID), [3, 1, 3]);
        assert_eq!(steering.counts(), (3, 0));
        line.raise_for(None);
        line.set(false);
        assert_eq!(chip.routed(INTID), [3, 1, 3]);
    }

    #[test]
    fn a_line_that_is_high_is_not_rerouted() {
        let (chip, _steering, line) = steered(4);
        line.raise_for(Some(1));
        line.raise_for(Some(2));
        assert_eq!(chip.routed(INTID), [1]);
        line.set(false);
        line.raise_for(Some(2));
        assert_eq!(chip.routed(INTID), [1, 2]);
        assert!(chip.routes.lock().unwrap().iter().all(|r| !r.while_high));
    }

    #[test]
    fn a_cpu_that_is_off_is_never_a_route() {
        let (chip, steering, line) = steered(4);
        steering.cpu_off(2);
        line.raise_for(Some(2));
        line.set(false);
        assert_eq!(
            chip.routed(INTID),
            [],
            "a request sent before its CPU went off"
        );
        line.raise_for(Some(9));
        line.set(false);
        assert_eq!(chip.routed(INTID), []);
        steering.cpu_on(2);
        line.raise_for(Some(2));
        assert_eq!(chip.routed(INTID), [2]);
    }

    #[test]
    fn turning_a_cpu_off_moves_its_lines_with_the_line_low() {
        let (chip, steering, line) = steered(4);
        let other = steering.line(chip.clone(), INTID + 1);
        steering.cpu_off(0);

        line.raise_for(Some(3));
        line.set(false);
        other.raise_for(Some(2));
        other.set(false);
        steering.cpu_off(3);
        assert_eq!(chip.routed(INTID), [3, 1]);
        assert_eq!(
            chip.routed(INTID + 1),
            [2],
            "a line routed elsewhere is left"
        );
        assert!(!chip.level(INTID));

        steering.cpu_on(3);
        line.raise_for(Some(3));
        let before = chip.events.lock().unwrap().len();
        steering.cpu_off(3);
        assert_eq!(chip.routed(INTID), [3, 1, 3, 1]);
        assert_eq!(
            chip.events.lock().unwrap()[before..],
            [(INTID, false), (INTID, true)]
        );
        assert!(chip.level(INTID));
        assert!(chip.routes.lock().unwrap().iter().all(|r| !r.while_high));
        assert_eq!(steering.counts().1, 2);

        line.set(false);
        line.raise_for(Some(1));
        assert_eq!(chip.routed(INTID), [3, 1, 3, 1]);

        line.set(false);
        steering.cpu_off(2);
        steering.cpu_off(1);
        assert_eq!(chip.routed(INTID), [3, 1, 3, 1]);
    }

    #[test]
    fn a_reset_forgets_the_route() {
        let (chip, _steering, line) = steered(2);
        line.raise_for(Some(1));
        line.set(false);
        line.forget_route();
        line.raise_for(Some(1));
        assert_eq!(chip.routed(INTID), [1, 1]);
    }

    #[test]
    fn steering_switched_off_writes_no_route() {
        let chip = Arc::new(RecordingChip::default());
        let steering = Steering::new(2, false);
        steering.cpu_on(1);
        let line = steering.line(chip.clone(), INTID);
        line.raise_for(Some(1));
        assert!(chip.level(INTID));
        steering.cpu_off(1);
        assert!(chip.routes.lock().unwrap().is_empty());
        let plain = chip.line(INTID + 1);
        plain.raise_for(Some(1));
        assert!(chip.routes.lock().unwrap().is_empty());
    }

    #[test]
    fn no_interleaving_reroutes_a_high_line_or_leaves_a_line_on_a_cpu_that_is_off() {
        struct Strict {
            high: [AtomicBool; 2],
            routes: AtomicU64,
            while_high: AtomicU64,
        }
        impl IrqChip for Strict {
            fn set_spi(&self, intid: u32, level: bool) {
                self.high[(intid - INTID) as usize].store(level, Ordering::SeqCst);
            }
            fn route(&self, intid: u32, _cpu: u32) {
                self.routes.fetch_add(1, Ordering::Relaxed);
                if self.high[(intid - INTID) as usize].load(Ordering::SeqCst) {
                    self.while_high.fetch_add(1, Ordering::Relaxed);
                }
            }
        }
        const CPUS: u32 = 4;
        let chip = Arc::new(Strict {
            high: [AtomicBool::new(false), AtomicBool::new(false)],
            routes: AtomicU64::new(0),
            while_high: AtomicU64::new(0),
        });
        let steering = Steering::new(CPUS, true);
        for c in 0..CPUS {
            steering.cpu_on(c);
        }
        let lines = [
            steering.line(chip.clone(), INTID),
            steering.line(chip.clone(), INTID + 1),
        ];
        let stop = Arc::new(AtomicBool::new(false));
        let mut threads = Vec::new();
        for cpu in 0..CPUS {
            let (lines, stop) = (lines.clone(), stop.clone());
            threads.push(std::thread::spawn(move || {
                let mut n = 0usize;
                while !stop.load(Ordering::Relaxed) {
                    lines[n % 2].raise_for(Some(cpu));
                    n += 1;
                }
            }));
        }
        for _ in 0..2 {
            let (lines, stop) = (lines.clone(), stop.clone());
            threads.push(std::thread::spawn(move || {
                let mut n = 0usize;
                while !stop.load(Ordering::Relaxed) {
                    lines[n % 2].set(false);
                    n += 1;
                }
            }));
        }
        let left_on_off = Arc::new(AtomicU64::new(0));
        {
            let (steering, stop, lines) = (steering.clone(), stop.clone(), lines.clone());
            let left = left_on_off.clone();
            threads.push(std::thread::spawn(move || {
                let mut n = 0u32;
                while !stop.load(Ordering::Relaxed) {
                    let cpu = 1 + n % (CPUS - 1);
                    steering.cpu_off(cpu);
                    for _ in 0..2 {
                        for line in &lines {
                            if line.shared.state.lock().unwrap().route == Some(cpu) {
                                left.fetch_add(1, Ordering::Relaxed);
                            }
                        }
                        std::thread::yield_now();
                    }
                    steering.cpu_on(cpu);
                    n += 1;
                }
            }));
        }
        std::thread::sleep(std::time::Duration::from_millis(300));
        stop.store(true, Ordering::Relaxed);
        for t in threads {
            t.join().unwrap();
        }
        let routes = chip.routes.load(Ordering::Relaxed);
        assert!(
            routes > 1000,
            "only {routes} routes: the test raced nothing"
        );
        assert!(steering.counts().1 > 0, "no line was ever moved off a CPU");
        assert_eq!(chip.while_high.load(Ordering::Relaxed), 0);
        assert_eq!(left_on_off.load(Ordering::Relaxed), 0);
    }
}
