use std::cell::Cell;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering::Relaxed};

use crate::hvf::{host_ticks, ticks_to_ns};

static ON: AtomicBool = AtomicBool::new(false);
static LAST: AtomicU64 = AtomicU64::new(0);

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
#[repr(usize)]
pub enum Stage {
    GuestSubmit,
    NotifyIn,
    Kick,
    Wake,
    Pop,
    Serve,
    Used,
    Raise,
    NotifyOut,
    GuestIrq,
    Status,
    GuestAck,
    Ack,
}

const STAGES: usize = Stage::Ack as usize + 1;
const NAMES: [&str; STAGES] = [
    "guest_submit",
    "notify_in",
    "kick",
    "wake",
    "pop",
    "serve",
    "used",
    "raise",
    "notify_out",
    "guest_irq",
    "status",
    "guest_ack",
    "ack",
];

static TICKS: [AtomicU64; STAGES] = [const { AtomicU64::new(0) }; STAGES];
static COUNT: [AtomicU64; STAGES] = [const { AtomicU64::new(0) }; STAGES];

static NOTIFY: [AtomicU64; 5] = [const { AtomicU64::new(0) }; 5];
static GIC: [AtomicU64; 4] = [const { AtomicU64::new(0) }; 4];
static INLINE: [AtomicU64; 2] = [const { AtomicU64::new(0) }; 2];

#[derive(Clone, Copy, PartialEq, Eq)]
enum In {
    Nothing,
    Notify,
    Served,
    Status,
    Ack,
}

thread_local! {
    static EXIT_AT: Cell<u64> = const { Cell::new(0) };
    static KIND: Cell<In> = const { Cell::new(In::Nothing) };
}

pub fn init_from_env() {
    if std::env::var_os("FEV_VMM_STAGES").is_some_and(|v| v == "1") {
        ON.store(true, Relaxed);
    }
}

#[inline]
pub fn on() -> bool {
    ON.load(Relaxed)
}

const GAP_TICKS: u64 = 120_000;
static GAPS: [AtomicU64; 2] = [const { AtomicU64::new(0) }; 2];

fn chain(stage: Stage, now: u64) {
    let last = LAST.fetch_max(now, Relaxed);
    if last == 0 {
        return;
    }
    let took = now.saturating_sub(last);
    if took > GAP_TICKS {
        GAPS[0].fetch_add(1, Relaxed);
        GAPS[1].fetch_add(took, Relaxed);
        return;
    }
    TICKS[stage as usize].fetch_add(took, Relaxed);
    COUNT[stage as usize].fetch_add(1, Relaxed);
}

#[inline]
pub fn mark(stage: Stage) {
    if on() {
        chain(stage, host_ticks());
    }
}

#[inline]
pub fn exit_taken() {
    if on() {
        EXIT_AT.set(host_ticks());
        KIND.set(In::Nothing);
    }
}

#[inline]
pub fn notify() {
    if on() {
        chain(Stage::GuestSubmit, EXIT_AT.get());
        chain(Stage::NotifyIn, host_ticks());
        KIND.set(In::Notify);
    }
}

#[inline]
pub fn inline(served: bool) {
    if on() {
        INLINE[!served as usize].fetch_add(1, Relaxed);
        if served {
            KIND.set(In::Served);
        }
    }
}

#[inline]
pub fn status_read() {
    if on() {
        chain(Stage::GuestIrq, EXIT_AT.get());
        KIND.set(In::Status);
    }
}

#[inline]
pub fn ack() {
    if on() {
        chain(Stage::GuestAck, EXIT_AT.get());
        KIND.set(In::Ack);
    }
}

#[inline]
pub fn entering() {
    if !on() {
        return;
    }
    let kind = KIND.replace(In::Nothing);
    if kind == In::Nothing {
        return;
    }
    let now = host_ticks();
    match kind {
        In::Status => chain(Stage::Status, now),
        In::Ack => chain(Stage::Ack, now),
        In::Served => chain(Stage::NotifyOut, now),
        In::Notify | In::Nothing => {}
    }
    if matches!(kind, In::Notify | In::Served) {
        let held = now.saturating_sub(EXIT_AT.get());
        NOTIFY[0].fetch_add(1, Relaxed);
        NOTIFY[1].fetch_add(held, Relaxed);
        let ns = ticks_to_ns(held);
        for (slot, bound) in [(2, 20_000), (3, 100_000), (4, 1_000_000)] {
            if ns > bound {
                NOTIFY[slot].fetch_add(1, Relaxed);
            }
        }
    }
}

#[inline]
pub fn gic_done(start: u64) {
    if start != 0 {
        let slot = if crate::irq::current_vcpu().is_some() {
            0
        } else {
            2
        };
        GIC[slot].fetch_add(1, Relaxed);
        GIC[slot + 1].fetch_add(host_ticks().saturating_sub(start), Relaxed);
    }
}

#[inline]
pub fn gic_start() -> u64 {
    if on() {
        host_ticks()
    } else {
        0
    }
}

pub fn report() -> Option<String> {
    if !on() {
        return None;
    }
    let mut out = String::from("stages:");
    for (n, name) in NAMES.iter().enumerate() {
        out += &format!(
            " {name}={}/{}",
            COUNT[n].load(Relaxed),
            ticks_to_ns(TICKS[n].load(Relaxed))
        );
    }
    let l = |a: &AtomicU64| a.load(Relaxed);
    out += &format!(
        " notify_exit={}/{} over20us={}/0 over100us={}/0 over1ms={}/0 gic_vcpu={}/{} \
         gic_other={}/{} inline={}/0 handed={}/0 gaps={}/{}",
        l(&NOTIFY[0]),
        ticks_to_ns(l(&NOTIFY[1])),
        l(&NOTIFY[2]),
        l(&NOTIFY[3]),
        l(&NOTIFY[4]),
        l(&GIC[0]),
        ticks_to_ns(l(&GIC[1])),
        l(&GIC[2]),
        ticks_to_ns(l(&GIC[3])),
        l(&INLINE[0]),
        l(&INLINE[1]),
        l(&GAPS[0]),
        ticks_to_ns(l(&GAPS[1])),
    );
    Some(out)
}
