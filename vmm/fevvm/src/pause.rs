use std::cell::RefCell;
use std::sync::atomic::{AtomicBool, AtomicU8, Ordering};
use std::sync::{Arc, Condvar, Mutex, MutexGuard};
use std::time::{Duration, Instant};

const REKICK: Duration = Duration::from_millis(1);

pub const END_GRACE: Duration = Duration::from_millis(250);

pub const PARK_LIMIT: Duration = Duration::from_secs(5);

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Refused {
    Stopping,
    NotParked(usize),
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Paused {
    pub changed: bool,
    pub at: u64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Resumed {
    pub changed: bool,
    pub offset: u64,
    pub total: u64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct Account {
    pub paused: bool,
    pub at: u64,
    pub total: u64,
    pub pauses: u64,
    pub rekicks: u64,
    pub claimed: u64,
}

const IN: u8 = 0;
const AWAY: u8 = 1;
const CLAIMED: u8 = 2;

thread_local! {
    static HERE: RefCell<Option<Arc<AtomicU8>>> = const { RefCell::new(None) };
}

pub fn away<R>(call: impl FnOnce() -> R) -> R {
    HERE.with(|here| {
        let here = here.borrow();
        let Some(place) = here.as_ref() else {
            return call();
        };
        if place
            .compare_exchange(IN, AWAY, Ordering::SeqCst, Ordering::SeqCst)
            .is_err()
        {
            return call();
        }
        let result = call();
        let _ = place.compare_exchange(AWAY, IN, Ordering::SeqCst, Ordering::SeqCst);
        result
    })
}

#[derive(Default)]
struct State {
    paused: bool,
    parked: bool,
    stopping: bool,
    inside: usize,
    held: usize,
    at: u64,
    total: u64,
    pauses: u64,
    rekicks: u64,
    claimed: u64,
}

pub struct Gate {
    wanted: AtomicBool,
    state: Mutex<State>,
    changed: Condvar,
    serving: Mutex<()>,
    limit: Duration,
    places: Mutex<Vec<Arc<AtomicU8>>>,
}

impl Default for Gate {
    fn default() -> Self {
        Self::new()
    }
}

impl Gate {
    pub fn new() -> Self {
        Self::with_limit(PARK_LIMIT)
    }

    fn with_limit(limit: Duration) -> Self {
        Gate {
            wanted: AtomicBool::new(false),
            state: Mutex::new(State::default()),
            changed: Condvar::new(),
            serving: Mutex::new(()),
            limit,
            places: Mutex::new(Vec::new()),
        }
    }

    pub fn attach(&self) {
        let place = Arc::new(AtomicU8::new(IN));
        self.places.lock().unwrap().push(place.clone());
        HERE.with(|h| *h.borrow_mut() = Some(place));
    }

    fn claim(&self, s: &mut State) {
        for place in self.places.lock().unwrap().iter() {
            if place
                .compare_exchange(AWAY, CLAIMED, Ordering::SeqCst, Ordering::SeqCst)
                .is_ok()
            {
                s.inside -= 1;
                s.claimed += 1;
            }
        }
    }

    pub fn back(&self) -> Option<u64> {
        HERE.with(|here| {
            let here = here.borrow();
            let place = here.as_ref()?;
            if place.load(Ordering::SeqCst) != CLAIMED {
                return None;
            }
            let s = self.state.lock().unwrap();
            place.store(IN, Ordering::SeqCst);
            Some(self.hold(s))
        })
    }

    pub fn enter(&self) -> u64 {
        self.hold(self.state.lock().unwrap())
    }

    pub fn wanted(&self) -> bool {
        self.wanted.load(Ordering::SeqCst)
    }

    pub fn park(&self) -> u64 {
        let mut s = self.state.lock().unwrap();
        s.inside -= 1;
        self.changed.notify_all();
        self.hold(s)
    }

    pub fn leave(&self) {
        self.state.lock().unwrap().inside -= 1;
        self.changed.notify_all();
    }

    fn hold(&self, mut s: MutexGuard<'_, State>) -> u64 {
        if s.paused && !s.stopping {
            s.held += 1;
            while s.paused && !s.stopping {
                s = self.changed.wait(s).unwrap();
            }
            s.held -= 1;
            self.changed.notify_all();
        }
        s.inside += 1;
        s.total
    }

    pub fn pause(&self, kick: &dyn Fn(), now: &dyn Fn() -> u64) -> Result<Paused, Refused> {
        let _one = self.serving.lock().unwrap();
        let mut s = self.state.lock().unwrap();
        if s.stopping {
            return Err(Refused::Stopping);
        }
        if s.paused {
            return Ok(Paused {
                changed: false,
                at: s.at,
            });
        }
        s.paused = true;
        self.wanted.store(true, Ordering::SeqCst);
        let began = Instant::now();
        let mut first = true;
        loop {
            self.claim(&mut s);
            if s.inside == 0 || s.stopping {
                break;
            }
            if began.elapsed() >= self.limit {
                let left = s.inside;
                s.paused = false;
                self.wanted.store(false, Ordering::SeqCst);
                self.changed.notify_all();
                return Err(Refused::NotParked(left));
            }
            if !first {
                s.rekicks += 1;
            }
            first = false;
            kick();
            s = self
                .changed
                .wait_timeout_while(s, REKICK, |s| s.inside > 0 && !s.stopping)
                .unwrap()
                .0;
        }
        if s.stopping {
            return Err(Refused::Stopping);
        }
        s.at = now();
        s.parked = true;
        s.pauses += 1;
        Ok(Paused {
            changed: true,
            at: s.at,
        })
    }

    pub fn resume(&self, now: &dyn Fn() -> u64) -> Result<Resumed, Refused> {
        self.resume_after(now, &|| {})
    }

    pub fn resume_after(
        &self,
        now: &dyn Fn() -> u64,
        first: &dyn Fn(),
    ) -> Result<Resumed, Refused> {
        let _one = self.serving.lock().unwrap();
        let mut s = self.state.lock().unwrap();
        if s.stopping {
            return Err(Refused::Stopping);
        }
        if !s.paused {
            return Ok(Resumed {
                changed: false,
                offset: 0,
                total: s.total,
            });
        }
        drop(s);
        first();
        s = self.state.lock().unwrap();
        if s.stopping {
            return Err(Refused::Stopping);
        }
        let offset = now().saturating_sub(s.at);
        s.total += offset;
        let total = s.total;
        s.paused = false;
        s.parked = false;
        self.wanted.store(false, Ordering::SeqCst);
        self.changed.notify_all();
        while s.held > 0 && !s.stopping {
            s = self.changed.wait(s).unwrap();
        }
        Ok(Resumed {
            changed: true,
            offset,
            total,
        })
    }

    pub fn stop(&self) {
        self.state.lock().unwrap().stopping = true;
        self.changed.notify_all();
    }

    pub fn ended(&self, kick: &dyn Fn(), grace: Duration) -> usize {
        let began = Instant::now();
        let mut s = self.state.lock().unwrap();
        loop {
            let late = began.elapsed() >= grace;
            if late {
                self.claim(&mut s);
            }
            let left = self
                .places
                .lock()
                .unwrap()
                .iter()
                .filter(|place| place.load(Ordering::SeqCst) == CLAIMED)
                .count();
            if s.inside == 0 && (left == 0 || late) {
                return left;
            }
            kick();
            s = self.changed.wait_timeout(s, REKICK).unwrap().0;
        }
    }

    pub fn account(&self) -> Account {
        let s = self.state.lock().unwrap();
        Account {
            paused: s.parked,
            at: s.at,
            total: s.total,
            pauses: s.pauses,
            rekicks: s.rekicks,
            claimed: s.claimed,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicU64, AtomicUsize};
    use std::sync::Arc;
    use std::thread::JoinHandle;

    struct Cpus {
        gate: Arc<Gate>,
        steps: Vec<Arc<AtomicU64>>,
        totals: Vec<Arc<AtomicU64>>,
        deaf: Arc<AtomicUsize>,
        kicks: Arc<AtomicUsize>,
        off: Arc<AtomicBool>,
        threads: Vec<JoinHandle<()>>,
    }

    impl Cpus {
        fn start(n: usize, gate: Gate) -> Cpus {
            let mut cpus = Cpus {
                gate: Arc::new(gate),
                steps: Vec::new(),
                totals: Vec::new(),
                deaf: Arc::new(AtomicUsize::new(0)),
                kicks: Arc::new(AtomicUsize::new(0)),
                off: Arc::new(AtomicBool::new(false)),
                threads: Vec::new(),
            };
            for _ in 0..n {
                cpus.add();
            }
            cpus
        }

        fn add(&mut self) {
            let (steps, total) = (Arc::new(AtomicU64::new(0)), Arc::new(AtomicU64::new(0)));
            self.steps.push(steps.clone());
            self.totals.push(total.clone());
            let (gate, deaf, kicks, off) = (
                self.gate.clone(),
                self.deaf.clone(),
                self.kicks.clone(),
                self.off.clone(),
            );
            self.threads.push(std::thread::spawn(move || {
                total.store(gate.enter(), Ordering::SeqCst);
                while !off.load(Ordering::SeqCst) {
                    if gate.wanted() && kicks.load(Ordering::SeqCst) >= deaf.load(Ordering::SeqCst)
                    {
                        total.store(gate.park(), Ordering::SeqCst);
                        continue;
                    }
                    steps.fetch_add(1, Ordering::SeqCst);
                    std::thread::yield_now();
                }
                gate.leave();
            }));
        }

        fn kick(&self) -> impl Fn() + '_ {
            || {
                self.kicks.fetch_add(1, Ordering::SeqCst);
            }
        }

        fn steps(&self) -> Vec<u64> {
            self.steps
                .iter()
                .map(|s| s.load(Ordering::SeqCst))
                .collect()
        }

        fn totals(&self) -> Vec<u64> {
            self.totals
                .iter()
                .map(|s| s.load(Ordering::SeqCst))
                .collect()
        }

        fn running(&self) {
            let from = self.steps();
            let deadline = Instant::now() + Duration::from_secs(5);
            while self.steps().iter().zip(&from).any(|(now, was)| now == was) {
                assert!(Instant::now() < deadline, "a CPU is not running");
                std::thread::sleep(Duration::from_millis(1));
            }
        }

        fn end(self) {
            self.off.store(true, Ordering::SeqCst);
            self.gate.stop();
            let deadline = Instant::now() + Duration::from_secs(5);
            for t in self.threads {
                while !t.is_finished() {
                    assert!(Instant::now() < deadline, "a vCPU is still at the gate");
                    std::thread::sleep(Duration::from_millis(1));
                }
                t.join().unwrap();
            }
        }
    }

    #[test]
    fn pause_returns_when_every_vcpu_is_out_and_resume_when_all_run() {
        let cpus = Cpus::start(4, Gate::new());
        cpus.running();
        let clock = AtomicU64::new(1000);
        let now = || clock.load(Ordering::SeqCst);
        let paused = cpus.gate.pause(&cpus.kick(), &now).unwrap();
        assert_eq!(
            paused,
            Paused {
                changed: true,
                at: 1000
            }
        );
        let held = cpus.steps();
        std::thread::sleep(Duration::from_millis(30));
        assert_eq!(cpus.steps(), held, "a vCPU ran while the VM was paused");
        assert!(cpus.gate.account().paused);

        clock.store(1750, Ordering::SeqCst);
        let resumed = cpus.gate.resume(&now).unwrap();
        assert_eq!(
            resumed,
            Resumed {
                changed: true,
                offset: 750,
                total: 750
            }
        );
        cpus.running();
        assert_eq!(cpus.totals(), [750; 4]);
        assert!(!cpus.gate.account().paused);
        cpus.end();
    }

    #[test]
    fn what_a_resume_sends_first_goes_before_any_vcpu_runs() {
        let cpus = Cpus::start(4, Gate::new());
        cpus.running();
        let clock = AtomicU64::new(0);
        let now = || clock.load(Ordering::SeqCst);
        let calls = AtomicUsize::new(0);
        let held = std::cell::Cell::new(Vec::new());
        let first = || {
            calls.fetch_add(1, Ordering::SeqCst);
            let before = cpus.steps();
            std::thread::sleep(Duration::from_millis(30));
            held.set(vec![before, cpus.steps()]);
        };
        assert!(!cpus.gate.resume_after(&now, &first).unwrap().changed);
        assert_eq!(calls.load(Ordering::SeqCst), 0);

        clock.store(100, Ordering::SeqCst);
        cpus.gate.pause(&cpus.kick(), &now).unwrap();
        clock.store(400, Ordering::SeqCst);
        let resumed = cpus.gate.resume_after(&now, &first).unwrap();
        assert!(resumed.changed);
        assert_eq!(calls.load(Ordering::SeqCst), 1);
        let seen = held.take();
        assert_eq!(
            seen[0], seen[1],
            "a vCPU ran while the resume's first word was being sent"
        );
        cpus.running();
        assert_eq!(cpus.totals(), [300; 4]);
        cpus.end();
    }

    #[test]
    fn a_second_pause_and_a_second_resume_change_nothing_and_say_so() {
        let cpus = Cpus::start(2, Gate::new());
        let clock = AtomicU64::new(0);
        let now = || clock.load(Ordering::SeqCst);
        let gate = &cpus.gate;

        assert_eq!(
            gate.resume(&now).unwrap(),
            Resumed {
                changed: false,
                offset: 0,
                total: 0
            }
        );
        clock.store(100, Ordering::SeqCst);
        assert!(gate.pause(&cpus.kick(), &now).unwrap().changed);
        let kicks = cpus.kicks.load(Ordering::SeqCst);
        clock.store(200, Ordering::SeqCst);
        assert_eq!(
            gate.pause(&cpus.kick(), &now).unwrap(),
            Paused {
                changed: false,
                at: 100
            }
        );
        assert_eq!(cpus.kicks.load(Ordering::SeqCst), kicks);
        clock.store(350, Ordering::SeqCst);
        assert_eq!(gate.resume(&now).unwrap().offset, 250);
        clock.store(900, Ordering::SeqCst);
        assert_eq!(
            gate.resume(&now).unwrap(),
            Resumed {
                changed: false,
                offset: 0,
                total: 250
            }
        );
        cpus.running();
        assert_eq!(cpus.totals(), [250; 2]);

        clock.store(1000, Ordering::SeqCst);
        gate.pause(&cpus.kick(), &now).unwrap();
        clock.store(1010, Ordering::SeqCst);
        assert_eq!(
            gate.resume(&now).unwrap(),
            Resumed {
                changed: true,
                offset: 10,
                total: 260
            }
        );
        let account = gate.account();
        assert_eq!((account.pauses, account.total), (2, 260));
        cpus.end();
    }

    #[test]
    fn the_pause_begins_when_the_last_vcpu_is_out_not_when_it_was_asked_for() {
        let cpus = Cpus::start(2, Gate::new());
        cpus.running();
        cpus.deaf.store(20, Ordering::SeqCst);
        let now = || cpus.steps().iter().sum::<u64>();
        let asked = now();
        let paused = cpus.gate.pause(&cpus.kick(), &now).unwrap();
        assert!(paused.at > asked, "the vCPUs did not run on past the call");
        assert_eq!(
            paused.at,
            now(),
            "the pause is counted from before the last vCPU stopped"
        );
        let account = cpus.gate.account();
        assert!(account.rekicks >= 19, "{account:?}");
        cpus.deaf.store(0, Ordering::SeqCst);
        cpus.end();
    }

    #[test]
    fn a_vcpu_turned_on_during_or_after_a_pause_gets_the_same_total() {
        let mut cpus = Cpus::start(2, Gate::new());
        let clock = AtomicU64::new(0);
        let now = || clock.load(Ordering::SeqCst);
        cpus.gate.pause(&cpus.kick(), &now).unwrap();
        cpus.add();
        std::thread::sleep(Duration::from_millis(30));
        assert_eq!(
            cpus.steps()[2],
            0,
            "a vCPU entered the guest during a pause"
        );
        clock.store(40, Ordering::SeqCst);
        cpus.gate.resume(&now).unwrap();
        cpus.running();
        assert_eq!(cpus.totals(), [40; 3]);
        cpus.add();
        cpus.running();
        assert_eq!(cpus.totals(), [40; 4]);
        cpus.end();
    }

    #[test]
    fn a_pause_that_cannot_park_a_vcpu_is_undone_with_no_offset() {
        let cpus = Cpus::start(3, Gate::with_limit(Duration::from_millis(50)));
        cpus.running();
        cpus.deaf.store(usize::MAX, Ordering::SeqCst);
        let clock = AtomicU64::new(7);
        let now = || clock.load(Ordering::SeqCst);
        assert_eq!(
            cpus.gate.pause(&cpus.kick(), &now),
            Err(Refused::NotParked(3))
        );
        assert!(!cpus.gate.wanted());
        let account = cpus.gate.account();
        assert_eq!(
            (account.paused, account.pauses, account.total),
            (false, 0, 0)
        );
        cpus.running();
        cpus.deaf.store(0, Ordering::SeqCst);
        assert!(cpus.gate.pause(&cpus.kick(), &now).unwrap().changed);
        clock.store(9, Ordering::SeqCst);
        assert_eq!(cpus.gate.resume(&now).unwrap().total, 2);
        cpus.end();
    }

    #[test]
    fn stopping_wakes_the_gate_and_refuses_what_comes_after() {
        let cpus = Cpus::start(2, Gate::new());
        let now = || 0;
        cpus.gate.pause(&cpus.kick(), &now).unwrap();
        let gate = cpus.gate.clone();
        cpus.end();
        assert_eq!(gate.pause(&|| {}, &now), Err(Refused::Stopping));
        assert_eq!(gate.resume(&now), Err(Refused::Stopping));
    }

    #[test]
    fn the_machine_ends_without_a_vcpu_that_is_inside_a_call_on_the_mac() {
        let gate = Arc::new(Gate::new());
        let (release, held) = std::sync::mpsc::channel::<()>();
        let (entered_tx, entered) = std::sync::mpsc::channel::<()>();
        let stuck = {
            let gate = gate.clone();
            std::thread::spawn(move || {
                gate.attach();
                gate.enter();
                away(|| {
                    entered_tx.send(()).unwrap();
                    let _ = held.recv();
                });
                let total = gate.back();
                gate.leave();
                total
            })
        };
        let stopping = Arc::new(AtomicBool::new(false));
        let (running_tx, is_running) = std::sync::mpsc::channel::<()>();
        let running = {
            let (gate, stopping) = (gate.clone(), stopping.clone());
            std::thread::spawn(move || {
                gate.attach();
                gate.enter();
                running_tx.send(()).unwrap();
                while !stopping.load(Ordering::SeqCst) {
                    std::thread::yield_now();
                }
                std::thread::sleep(Duration::from_millis(30));
                gate.leave();
            })
        };
        entered.recv().unwrap();
        is_running.recv().unwrap();
        let (done, ended) = std::sync::mpsc::channel();
        let ender = {
            let gate = gate.clone();
            std::thread::spawn(move || {
                gate.stop();
                stopping.store(true, Ordering::SeqCst);
                let start = Instant::now();
                let left = gate.ended(&|| {}, Duration::from_millis(100));
                let _ = done.send((left, start.elapsed()));
            })
        };
        let result = ended.recv_timeout(Duration::from_secs(2));
        release.send(()).unwrap();
        let (left, took) = result.expect("the machine's end waited for a call on the Mac");
        assert_eq!(left, 1, "one vCPU was left inside its call");
        assert!(
            took >= Duration::from_millis(100),
            "the vCPU inside a call was given no time to come back: {took:?}"
        );
        ender.join().unwrap();
        running.join().unwrap();
        assert_eq!(stuck.join().unwrap(), Some(0));
        assert_eq!(gate.account().claimed, 1);
    }

    #[test]
    fn the_machine_s_end_waits_for_a_call_on_the_mac_that_is_only_in_flight() {
        let gate = Arc::new(Gate::new());
        let (entered_tx, entered) = std::sync::mpsc::channel::<()>();
        let kicks = AtomicUsize::new(0);
        let vcpu = {
            let gate = gate.clone();
            std::thread::spawn(move || {
                gate.attach();
                gate.enter();
                away(|| {
                    entered_tx.send(()).unwrap();
                    std::thread::sleep(Duration::from_millis(40));
                });
                let total = gate.back();
                gate.leave();
                total
            })
        };
        entered.recv().unwrap();
        gate.stop();
        let start = Instant::now();
        let left = gate.ended(
            &|| {
                kicks.fetch_add(1, Ordering::SeqCst);
            },
            Duration::from_secs(2),
        );
        let took = start.elapsed();
        assert_eq!(left, 0, "a call in flight was left behind");
        assert!(
            took >= Duration::from_millis(30) && took < Duration::from_secs(1),
            "{took:?}"
        );
        assert!(kicks.load(Ordering::SeqCst) >= 10, "{kicks:?}");
        assert_eq!(vcpu.join().unwrap(), None);
        assert_eq!(gate.account().claimed, 0);
    }

    #[test]
    fn a_vcpu_that_begins_a_call_after_the_pause_was_asked_is_counted_out() {
        let gate = Arc::new(Gate::with_limit(Duration::from_secs(3)));
        let go = Arc::new(AtomicBool::new(false));
        let (release, held) = std::sync::mpsc::channel::<()>();
        let (entered_tx, entered) = std::sync::mpsc::channel::<()>();
        let vcpu = {
            let (gate, go) = (gate.clone(), go.clone());
            std::thread::spawn(move || {
                gate.attach();
                gate.enter();
                entered_tx.send(()).unwrap();
                while !go.load(Ordering::SeqCst) {
                    std::thread::yield_now();
                }
                away(|| {
                    let _ = held.recv();
                });
                let total = gate.back();
                gate.leave();
                total
            })
        };
        let later = {
            let go = go.clone();
            std::thread::spawn(move || {
                std::thread::sleep(Duration::from_millis(30));
                go.store(true, Ordering::SeqCst);
            })
        };
        entered.recv().unwrap();
        let start = Instant::now();
        let paused = gate.pause(&|| {}, &|| 5);
        let took = start.elapsed();
        let account = gate.account();
        let _ = gate.resume(&|| 9);
        release.send(()).unwrap();
        later.join().unwrap();
        assert!(paused.is_ok(), "{paused:?}: the pause waited for the call");
        assert!(took >= Duration::from_millis(25), "claimed too early");
        assert!(took < Duration::from_secs(1), "{took:?}");
        assert_eq!(account.claimed, 1);
        assert!(account.rekicks >= 1);
        assert_eq!(vcpu.join().unwrap(), Some(4));
    }

    #[test]
    fn a_pause_under_way_when_the_machine_stops_is_refused() {
        let cpus = Cpus::start(1, Gate::new());
        cpus.running();
        cpus.deaf.store(usize::MAX, Ordering::SeqCst);
        let gate = cpus.gate.clone();
        let stopper = std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(20));
            gate.stop();
        });
        assert_eq!(cpus.gate.pause(&cpus.kick(), &|| 0), Err(Refused::Stopping));
        stopper.join().unwrap();
        cpus.end();
    }
}
