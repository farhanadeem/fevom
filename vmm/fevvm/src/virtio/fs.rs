use std::collections::VecDeque;
use std::sync::atomic::{AtomicBool, AtomicU32, AtomicU64, Ordering};
use std::sync::{Arc, Condvar, Mutex};
use std::thread::JoinHandle;
use std::time::{Duration, Instant};

use fileshare::abi::{op, InHeader, IN_HEADER_LEN};
use fileshare::iov::IoVecs;
use fileshare::server::{head_len, InitInfo, Request, Server};

use super::queue::{Buf, Chain, Queue, QueueError};
use super::{read_config_bytes, Activation, Interrupt, VirtioDevice};
use crate::irq::Sender;
use crate::memory::GuestMemory;
use crate::stage::{mark, Stage};

const DEVICE_ID: u32 = 26;
pub const QUEUE_SIZE: u16 = 1024;
pub const MAX_REQUEST_QUEUES: u32 = 4;
pub const TAG_LEN: usize = 36;
const CONFIG_LEN: usize = TAG_LEN + 4;

const HOLD: Duration = Duration::from_micros(60);
const SPIN: Duration = Duration::from_micros(30);
const INLINE_MAX: u64 = 64 << 10;
const SLOW: Duration = Duration::from_micros(500);
const QUICK_AGAIN: u32 = 8;
const INLINE_ENV: &str = "FEV_FS_INLINE";

fn micros_from_env(name: &str, default: Duration) -> Duration {
    std::env::var(name)
        .ok()
        .and_then(|v| v.parse().ok())
        .map_or(default, Duration::from_micros)
}

fn read_at(mem: &GuestMemory, bufs: &[Buf], mut skip: u64, out: &mut [u8]) -> bool {
    let mut done = 0;
    for b in bufs {
        if done == out.len() {
            break;
        }
        let len = b.len as u64;
        if skip >= len {
            skip -= len;
            continue;
        }
        let n = ((len - skip) as usize).min(out.len() - done);
        let Some(addr) = b.addr.checked_add(skip) else {
            return false;
        };
        if mem.read(addr, &mut out[done..done + n]).is_err() {
            return false;
        }
        done += n;
        skip = 0;
    }
    done == out.len()
}

struct Iovs {
    short: [libc::iovec; 4],
    len: usize,
    long: Vec<libc::iovec>,
}

impl Iovs {
    fn push(&mut self, iov: libc::iovec) {
        if self.long.is_empty() && self.len < self.short.len() {
            self.short[self.len] = iov;
            self.len += 1;
        } else {
            if self.long.is_empty() {
                self.long.extend_from_slice(&self.short[..self.len]);
            }
            self.long.push(iov);
        }
    }

    fn as_slice(&self) -> &[libc::iovec] {
        if self.long.is_empty() {
            &self.short[..self.len]
        } else {
            &self.long
        }
    }
}

fn host_iov(mem: &GuestMemory, bufs: &[Buf], mut skip: u64, mut len: u64) -> Option<Iovs> {
    let mut out = Iovs {
        short: [libc::iovec {
            iov_base: std::ptr::null_mut(),
            iov_len: 0,
        }; 4],
        len: 0,
        long: Vec::new(),
    };
    for b in bufs {
        if len == 0 {
            break;
        }
        let blen = b.len as u64;
        if skip >= blen {
            skip -= blen;
            continue;
        }
        let take = (blen - skip).min(len);
        let host = mem.host_range(b.addr.checked_add(skip)?, take).ok()?;
        out.push(libc::iovec {
            iov_base: host.cast(),
            iov_len: take as usize,
        });
        len -= take;
        skip = 0;
    }
    Some(out)
}

pub struct Fetched {
    short: [u8; SHORT_HEAD],
    long: Vec<u8>,
    head_len: usize,
    total: u64,
    opcode: Option<u32>,
    whole: bool,
    writeback: bool,
}

const SHORT_HEAD: usize = 256;

impl Fetched {
    fn head(&self) -> &[u8] {
        if self.long.is_empty() {
            &self.short[..self.head_len]
        } else {
            &self.long
        }
    }
}

pub fn fetch(mem: &GuestMemory, chain: &Chain) -> Fetched {
    let mut f = Fetched {
        short: [0u8; SHORT_HEAD],
        long: Vec::new(),
        head_len: 0,
        total: 0,
        opcode: None,
        whole: false,
        writeback: false,
    };
    if !read_at(mem, &chain.readable, 0, &mut f.short[..IN_HEADER_LEN]) {
        return f;
    }
    let Some(h) = InHeader::parse(&f.short) else {
        return f;
    };
    f.opcode = Some(h.opcode);
    f.total = (h.len as u64).clamp(IN_HEADER_LEN as u64, chain.readable_len());
    f.head_len = head_len(h.opcode, f.total as usize);
    let rest = if f.head_len <= SHORT_HEAD {
        &mut f.short[IN_HEADER_LEN..f.head_len]
    } else {
        f.long = vec![0u8; f.head_len];
        f.long[..IN_HEADER_LEN].copy_from_slice(&f.short[..IN_HEADER_LEN]);
        &mut f.long[IN_HEADER_LEN..]
    };
    if !read_at(mem, &chain.readable, IN_HEADER_LEN as u64, rest) {
        return f;
    }
    f.whole = true;
    let flags_at = IN_HEADER_LEN + 20;
    f.writeback = h.opcode == op::WRITE
        && f.head_len >= flags_at + 4
        && f.head()[flags_at] & fileshare::abi::WRITE_CACHE as u8 != 0;
    f
}

pub fn serve(server: &Server, mem: &GuestMemory, chain: &Chain, f: &Fetched) -> u32 {
    if !f.whole {
        return 0;
    }
    let (Some(payload), Some(reply)) = (
        host_iov(
            mem,
            &chain.readable,
            f.head_len as u64,
            f.total - f.head_len as u64,
        ),
        host_iov(mem, &chain.writable, 0, chain.writable_len()),
    ) else {
        return 0;
    };
    let written = server.handle(&Request {
        head: f.head(),
        payload: unsafe { IoVecs::new(payload.as_slice()) },
        reply: unsafe { IoVecs::new(reply.as_slice()) },
    });
    written.min(u32::MAX as usize) as u32
}

pub fn handle(server: &Server, mem: &GuestMemory, chain: &Chain) -> (u32, Option<u32>, bool) {
    let f = fetch(mem, chain);
    (serve(server, mem, chain, &f), f.opcode, !f.writeback)
}

#[derive(Default)]
pub struct Stats {
    requests: AtomicU64,
    serve_ns: AtomicU64,
    wakes: AtomicU64,
    wake_ns: AtomicU64,
    cpu_ns: AtomicU64,
    by_opcode: [AtomicU64; 32],
    by_opcode_high: [AtomicU64; 32],
    ns_by_opcode: [[AtomicU64; 32]; 2],
    inline: AtomicU64,
    handed: AtomicU64,
    over: [AtomicU64; 4],
    interrupts: AtomicU64,
}

const OVER_US: [u64; 4] = [100, 250, 500, 1000];

impl Stats {
    fn report(&self, tag: &str) {
        let take = |a: &AtomicU64| a.swap(0, Ordering::Relaxed);
        let (requests, serve, wakes, wake, cpu) = (
            take(&self.requests),
            take(&self.serve_ns),
            take(&self.wakes),
            take(&self.wake_ns),
            take(&self.cpu_ns),
        );
        let ops: Vec<String> = self
            .by_opcode
            .iter()
            .chain(&self.by_opcode_high)
            .enumerate()
            .map(|(i, n)| (i, take(n)))
            .filter(|(_, n)| *n > 0)
            .map(|(i, n)| format!("{i}:{n}"))
            .collect();
        let ns: Vec<String> = self
            .ns_by_opcode
            .iter()
            .flatten()
            .enumerate()
            .map(|(i, n)| (i, take(n)))
            .filter(|(_, n)| *n > 0)
            .map(|(i, n)| format!("{i}:{n}"))
            .collect();
        let over: Vec<String> = OVER_US
            .iter()
            .zip(&self.over)
            .map(|(us, n)| format!("{us}:{}", take(n)))
            .collect();
        crate::vmm_log!(
            "virtio-fs {tag}: stats requests={requests} serve_ns={serve} wakes={wakes} \
             wake_ns={wake} worker_cpu_ns={cpu} opcode_ns={} opcodes={} inline={} handed={} \
             inline_over_us={} inline_interrupts={}",
            ns.join(","),
            ops.join(","),
            take(&self.inline),
            take(&self.handed),
            over.join(","),
            take(&self.interrupts)
        );
    }
}

fn thread_cpu_ns() -> u64 {
    let mut ts = libc::timespec {
        tv_sec: 0,
        tv_nsec: 0,
    };
    unsafe { libc::clock_gettime(libc::CLOCK_THREAD_CPUTIME_ID, &mut ts) };
    ts.tv_sec as u64 * 1_000_000_000 + ts.tv_nsec as u64
}

#[derive(Default)]
struct Signal {
    kicked: bool,
    stop: bool,
    busy: bool,
    since: Option<Instant>,
}

#[derive(Default)]
struct Pace {
    quick: AtomicBool,
    flags: Arc<super::HoldFlags>,
}

struct Lane {
    state: Mutex<LaneState>,
    idle: Condvar,
    signal: (Mutex<Signal>, Condvar),
    pace: Pace,
    sender: Sender,
    slow: AtomicU32,
}

struct LaneState {
    q: Queue,
    live: bool,
    handed: VecDeque<(Chain, Fetched)>,
    serving: u32,
}

struct Shared {
    tag: String,
    server: Server,
    stats: Option<Stats>,
    verbose: bool,
    hold: Duration,
    spin: Duration,
    inline_max: u64,
    slow_after: Duration,
}

struct Lanes {
    shared: Arc<Shared>,
    mem: Arc<GuestMemory>,
    irq: Arc<Interrupt>,
    lanes: Vec<Lane>,
}

impl Shared {
    fn vcpu_may_serve(&self, f: &Fetched, chain: &Chain) -> bool {
        if !f.whole || self.inline_max == 0 {
            return false;
        }
        let small = match f.opcode {
            Some(op::READ) => {
                chain.writable_len() <= self.inline_max + fileshare::abi::OUT_HEADER_LEN as u64
            }
            Some(op::WRITE) => !f.writeback && f.total <= self.inline_max + f.head_len as u64,
            _ => true,
        };
        small && self.server.quick(f.head())
    }

    fn account(&self, opcode: Option<u32>, took: Duration, by_vcpu: bool) {
        if let Some(stats) = &self.stats {
            let ns = took.as_nanos() as u64;
            stats.requests.fetch_add(1, Ordering::Relaxed);
            stats.serve_ns.fetch_add(ns, Ordering::Relaxed);
            if let Some(n) = opcode.and_then(|o| stats.ns_by_opcode.as_flattened().get(o as usize))
            {
                n.fetch_add(ns, Ordering::Relaxed);
            }
            let slot = |o: u32| match o {
                0..32 => stats.by_opcode.get(o as usize),
                _ => stats.by_opcode_high.get(o as usize - 32),
            };
            if let Some(n) = opcode.and_then(slot) {
                n.fetch_add(1, Ordering::Relaxed);
            }
            if by_vcpu {
                stats.inline.fetch_add(1, Ordering::Relaxed);
                for (us, n) in OVER_US.iter().zip(&stats.over) {
                    if ns > us * 1000 {
                        n.fetch_add(1, Ordering::Relaxed);
                    }
                }
            }
        }
        match opcode {
            Some(op::INIT) if self.verbose => self.log_init(),
            Some(op::STATFS | op::DESTROY) => {
                if let Some(stats) = &self.stats {
                    stats.report(&self.tag);
                }
            }
            _ => {}
        }
    }

    fn log_init(&self) {
        if let Some(InitInfo {
            major,
            minor,
            flags_in,
            flags_out,
            max_readahead,
            max_write,
            max_pages,
        }) = self.server.negotiated()
        {
            crate::vmm_log!(
                "virtio-fs {}: INIT from a {major}.{minor} client: flags offered {flags_in:#x}, \
                 accepted {flags_out:#x}, max_readahead {max_readahead}, max_write {max_write}, \
                 max_pages {max_pages}",
                self.tag
            );
        }
    }
}

impl Lanes {
    fn kick(&self, lane: &Lane) {
        let mut s = lane.signal.0.lock().unwrap();
        s.kicked = true;
        if self.shared.stats.is_some() && s.since.is_none() {
            s.since = Some(Instant::now());
        }
        drop(s);
        mark(Stage::Kick);
        lane.signal.1.notify_one();
    }

    fn drain(&self, n: usize) -> Result<bool, QueueError> {
        let lane = &self.lanes[n];
        let mut served = false;
        loop {
            let (chain, handed) = {
                let mut st = lane.state.lock().unwrap();
                loop {
                    if let Some((chain, f)) = st.handed.pop_front() {
                        break (chain, Some(f));
                    }
                    if let Some(chain) = st.q.pop(&self.mem)? {
                        break (chain, None);
                    }
                    if !st.q.enable_notifications(&self.mem)? {
                        return Ok(served);
                    }
                }
            };
            served = true;
            mark(Stage::Pop);
            let fetched;
            let f: &Fetched = match &handed {
                Some(f) => f,
                None => {
                    fetched = fetch(&self.mem, &chain);
                    &fetched
                }
            };
            let start = Instant::now();
            let written = serve(&self.shared.server, &self.mem, &chain, f);
            mark(Stage::Serve);
            let interrupt = {
                let mut st = lane.state.lock().unwrap();
                st.q.push_used(&self.mem, chain.head, written)?;
                mark(Stage::Used);
                st.q.needs_interrupt(&self.mem)?
            };
            if interrupt {
                let flags = &lane.pace.flags;
                let sender = &lane.sender;
                if flags.held.load(Ordering::SeqCst) {
                    flags.owed.store(true, Ordering::SeqCst);
                    if !flags.held.load(Ordering::SeqCst)
                        && flags.owed.swap(false, Ordering::SeqCst)
                    {
                        self.irq.signal_used_for(sender.get());
                    }
                } else {
                    self.irq.signal_used_for(sender.get());
                }
            }
            mark(Stage::Raise);
            let took = start.elapsed();
            lane.pace
                .quick
                .store(!f.writeback && took <= self.shared.hold, Ordering::Relaxed);
            let slow = lane.slow.load(Ordering::Relaxed);
            if slow != 0 && took <= self.shared.slow_after {
                lane.slow.store(slow - 1, Ordering::Relaxed);
            }
            self.shared.account(f.opcode, took, false);
        }
    }

    fn has_available(&self, n: usize) -> Result<bool, QueueError> {
        self.lanes[n]
            .state
            .lock()
            .unwrap()
            .q
            .has_available(&self.mem)
    }
}

impl super::Inline for Lanes {
    fn notified(&self, queue: u32) {
        let Some(lane) = self.lanes.get(queue as usize) else {
            return;
        };
        lane.sender.note();
        if queue == 0 || lane.slow.load(Ordering::Relaxed) != 0 {
            return self.kick(lane);
        }
        let fail = |e: QueueError| self.irq.fail("virtio-fs", &e);
        let mut st = lane.state.lock().unwrap();
        if !st.live {
            return;
        }
        let chain = match st.q.pop(&self.mem) {
            Ok(Some(chain)) => chain,
            Ok(None) => {
                let more = st.q.enable_notifications(&self.mem);
                drop(st);
                return match more {
                    Ok(true) => self.kick(lane),
                    Ok(false) => {}
                    Err(e) => fail(e),
                };
            }
            Err(e) => {
                drop(st);
                return fail(e);
            }
        };
        mark(Stage::Pop);
        let f = fetch(&self.mem, &chain);
        let more = match st.q.enable_notifications(&self.mem) {
            Ok(more) => more || !st.handed.is_empty(),
            Err(e) => {
                drop(st);
                return fail(e);
            }
        };
        if !self.shared.vcpu_may_serve(&f, &chain) {
            crate::stage::inline(false);
            st.handed.push_back((chain, f));
            drop(st);
            if let Some(stats) = &self.shared.stats {
                stats.handed.fetch_add(1, Ordering::Relaxed);
            }
            return self.kick(lane);
        }
        st.serving += 1;
        drop(st);
        if more {
            self.kick(lane);
        }
        crate::stage::inline(true);
        let start = Instant::now();
        let written = crate::pause::away(|| serve(&self.shared.server, &self.mem, &chain, &f));
        let took = start.elapsed();
        mark(Stage::Serve);
        let mut st = lane.state.lock().unwrap();
        st.serving -= 1;
        let result = if st.live {
            (|| {
                st.q.push_used(&self.mem, chain.head, written)?;
                mark(Stage::Used);
                if st.q.needs_interrupt(&self.mem)? {
                    self.irq.signal_used_for(crate::irq::current_vcpu());
                    if let Some(stats) = &self.shared.stats {
                        stats.interrupts.fetch_add(1, Ordering::Relaxed);
                    }
                }
                mark(Stage::Raise);
                Ok(())
            })()
        } else {
            Ok(())
        };
        if st.serving == 0 {
            lane.idle.notify_all();
        }
        drop(st);
        if !self.shared.slow_after.is_zero() && took > self.shared.slow_after {
            lane.slow.store(QUICK_AGAIN, Ordering::Relaxed);
        }
        self.shared.account(f.opcode, took, true);
        if let Err(e) = result {
            fail(e);
        }
    }
}

pub struct Fs {
    shared: Arc<Shared>,
    tag: [u8; TAG_LEN],
    queue_sizes: Vec<u16>,
    lanes: Option<Arc<Lanes>>,
    workers: Vec<JoinHandle<()>>,
    hold: Option<(Duration, Arc<super::HoldFlags>)>,
}

#[derive(Debug, PartialEq, Eq)]
pub struct BadTag;

impl Fs {
    pub fn new(tag: &str, server: Server, cpus: u32, verbose: bool) -> Result<Self, BadTag> {
        if tag.is_empty() || tag.len() > TAG_LEN || tag.bytes().any(|b| b == 0) {
            return Err(BadTag);
        }
        let mut raw = [0u8; TAG_LEN];
        raw[..tag.len()].copy_from_slice(tag.as_bytes());
        let number = |name: &str| std::env::var(name).ok().and_then(|v| v.parse::<u64>().ok());
        let request_queues = cpus.clamp(1, MAX_REQUEST_QUEUES) as usize;
        let env = |name| std::env::var_os(name).is_some_and(|v| v == "1");
        let inline_max = if std::env::var_os(INLINE_ENV).is_some_and(|v| v == "off") {
            0
        } else {
            number("FEV_FS_INLINE_KIB").map_or(INLINE_MAX, |kib| kib << 10)
        };
        Ok(Self {
            shared: Arc::new(Shared {
                tag: tag.into(),
                server,
                stats: env("FEV_FS_STATS").then(Stats::default),
                verbose,
                hold: micros_from_env("FEV_FS_HOLD_US", HOLD),
                spin: micros_from_env(
                    "FEV_FS_SPIN_US",
                    if inline_max == 0 {
                        SPIN
                    } else {
                        Duration::ZERO
                    },
                ),
                inline_max,
                slow_after: micros_from_env("FEV_FS_SLOW_US", SLOW),
            }),
            hold: None,
            tag: raw,
            queue_sizes: vec![QUEUE_SIZE; 1 + request_queues],
            lanes: None,
            workers: Vec::new(),
        })
    }

    #[cfg(test)]
    fn with(mut self, change: impl FnOnce(&mut Shared)) -> Self {
        change(Arc::get_mut(&mut self.shared).expect("not active yet"));
        self
    }

    fn config(&self) -> [u8; CONFIG_LEN] {
        let mut c = [0u8; CONFIG_LEN];
        c[..TAG_LEN].copy_from_slice(&self.tag);
        let n = self.queue_sizes.len() as u32 - 1;
        c[TAG_LEN..].copy_from_slice(&n.to_le_bytes());
        c
    }

    fn stop(&mut self, wait: bool) {
        let Some(lanes) = self.lanes.take() else {
            return;
        };
        let mut busy = Vec::new();
        for lane in &lanes.lanes {
            let mut s = lane.signal.0.lock().unwrap();
            s.stop = true;
            busy.push(s.busy);
            drop(s);
            lane.signal.1.notify_one();
        }
        let workers: Vec<_> = self.workers.drain(..).zip(busy).collect();
        crate::pause::away(|| {
            for (thread, busy) in workers {
                if wait || !busy {
                    let _ = thread.join();
                }
            }
            for lane in &lanes.lanes {
                let mut st = lane.state.lock().unwrap();
                st.live = false;
                while wait && st.serving != 0 {
                    st = lane.idle.wait(st).unwrap();
                }
            }
        });
        self.shared.server.reset();
    }
}

fn run_worker(lanes: Arc<Lanes>, n: usize) {
    let shared = &lanes.shared;
    let (lock, cv) = &lanes.lanes[n].signal;
    let mut cpu_seen = 0;
    loop {
        {
            let mut s = lock.lock().unwrap();
            s.busy = false;
            while !s.kicked && !s.stop {
                s = cv.wait(s).unwrap();
            }
            if s.stop {
                return;
            }
            s.kicked = false;
            s.busy = true;
            if let (Some(stats), Some(since)) = (&shared.stats, s.since.take()) {
                stats.wakes.fetch_add(1, Ordering::Relaxed);
                stats
                    .wake_ns
                    .fetch_add(since.elapsed().as_nanos() as u64, Ordering::Relaxed);
            }
        }
        mark(Stage::Wake);
        loop {
            match lanes.drain(n) {
                Ok(false) => break,
                Ok(true) => {}
                Err(e) => return lanes.irq.fail("virtio-fs", &e),
            }
            let start = Instant::now();
            let mut more = false;
            while !shared.spin.is_zero() && start.elapsed() < shared.spin {
                match lanes.has_available(n) {
                    Ok(true) => {
                        more = true;
                        break;
                    }
                    Ok(false) => std::hint::spin_loop(),
                    Err(e) => return lanes.irq.fail("virtio-fs", &e),
                }
            }
            if !more {
                break;
            }
        }
        if let Some(stats) = &shared.stats {
            let now = thread_cpu_ns();
            stats.cpu_ns.fetch_add(now - cpu_seen, Ordering::Relaxed);
            cpu_seen = now;
        }
    }
}

impl VirtioDevice for Fs {
    fn device_id(&self) -> u32 {
        DEVICE_ID
    }

    fn features(&self) -> u64 {
        0
    }

    fn queue_max_sizes(&self) -> &[u16] {
        &self.queue_sizes
    }

    fn read_config(&self, offset: u64, data: &mut [u8]) {
        read_config_bytes(&self.config(), offset, data);
    }

    fn activate(&mut self, activation: Activation) {
        if self.shared.verbose {
            let sizes: Vec<String> = activation
                .queues
                .iter()
                .map(|q| {
                    if q.ready {
                        q.size.to_string()
                    } else {
                        "off".into()
                    }
                })
                .collect();
            let q0 = activation.queues.first();
            crate::vmm_log!(
                "virtio-fs {}: driver ready; queues (high-priority first) {}; indirect \
                 descriptors {}, event index {}",
                self.shared.tag,
                sizes.join(" "),
                q0.is_some_and(|q| q.indirect),
                q0.is_some_and(|q| q.event_idx)
            );
        }
        let lanes = Arc::new(Lanes {
            shared: self.shared.clone(),
            mem: activation.mem.clone(),
            irq: activation.irq.clone(),
            lanes: activation
                .queues
                .into_iter()
                .map(|q| Lane {
                    state: Mutex::new(LaneState {
                        q,
                        live: true,
                        handed: VecDeque::new(),
                        serving: 0,
                    }),
                    idle: Condvar::new(),
                    signal: (Mutex::new(Signal::default()), Condvar::new()),
                    pace: Pace::default(),
                    sender: Sender::default(),
                    slow: AtomicU32::new(0),
                })
                .collect(),
        });
        for n in 0..lanes.lanes.len() {
            let lanes = lanes.clone();
            let thread = std::thread::Builder::new()
                .name(format!("virtio-fs-q{n}"))
                .spawn(move || run_worker(lanes, n))
                .expect("spawn virtio-fs worker");
            self.workers.push(thread);
        }
        self.lanes = Some(lanes);
    }

    fn hold(&mut self) -> Option<(Duration, Arc<super::HoldFlags>)> {
        self.hold.take()
    }

    fn inline(&mut self) -> Option<Arc<dyn super::Inline>> {
        match &self.lanes {
            Some(lanes) if self.shared.inline_max != 0 => Some(lanes.clone()),
            _ => None,
        }
    }

    fn notify(&mut self, queue: u32) {
        let Some(lanes) = &self.lanes else { return };
        if let Some(lane) = lanes.lanes.get(queue as usize) {
            lane.sender.note();
            self.hold = (queue != 0
                && !self.shared.hold.is_zero()
                && lane.pace.quick.load(Ordering::Relaxed))
            .then(|| {
                lane.pace.flags.held.store(true, Ordering::SeqCst);
                (self.shared.hold, lane.pace.flags.clone())
            });
            lanes.kick(lane);
        }
    }

    fn reset(&mut self) {
        self.stop(true);
    }
}

impl Drop for Fs {
    fn drop(&mut self) {
        self.stop(false);
    }
}

#[cfg(test)]
mod tests {
    use super::super::queue::testing::{memory, Driver};
    use super::super::testing::{interrupt, INTID};
    use super::*;
    use fileshare::abi::{self, Writer};
    use fileshare::fake::FakeFs;
    use std::time::Duration;

    const BASE: u64 = 0x4000_0000;
    const REQ: u64 = BASE + 0x8000;
    const REPLY: u64 = BASE + 0x10000;

    fn server() -> Server {
        let mut fs = FakeFs::new();
        fs.add_file(b"hello", b"hello from the Mac\n");
        Server::new(Box::new(fs))
    }

    fn request(opcode: u32, nodeid: u64, body: &[u8]) -> Vec<u8> {
        let mut w = Writer::default();
        w.u32((IN_HEADER_LEN + body.len()) as u32)
            .u32(opcode)
            .u64(77)
            .u64(nodeid)
            .u32(0)
            .u32(0)
            .u32(1)
            .u32(0);
        w.0.extend_from_slice(body);
        w.0
    }

    fn init_body() -> Vec<u8> {
        let mut w = Writer::default();
        w.u32(7)
            .u32(45)
            .u32(131072)
            .u32(!(1u32 << 31))
            .u32(u32::MAX)
            .zeros(44);
        w.0
    }

    fn roundtrip(
        server: &Server,
        mem: &GuestMemory,
        req: &[u8],
        pieces: usize,
        cap: u32,
    ) -> Vec<u8> {
        let (mut drv, mut q) = Driver::new(mem, BASE, 16);
        mem.write(REQ, req).unwrap();
        mem.write(REPLY, &vec![0xee; cap as usize]).unwrap();
        let step = req.len().div_ceil(pieces);
        let mut bufs: Vec<(u64, u32, bool)> = (0..req.len())
            .step_by(step)
            .map(|at| (REQ + at as u64, step.min(req.len() - at) as u32, false))
            .collect();
        if cap > 0 {
            bufs.push((REPLY, 16.min(cap), true));
            if cap > 16 {
                bufs.push((REPLY + 16, cap - 16, true));
            }
        }
        drv.add(mem, &bufs);
        let chain = q.pop(mem).unwrap().unwrap();
        let (written, opcode, _) = handle(server, mem, &chain);
        assert!(opcode.is_some());
        let mut out = vec![0u8; written as usize];
        mem.read(REPLY, &mut out).unwrap();
        out
    }

    #[test]
    fn config_space_is_the_tag_and_the_request_queue_count() {
        for (cpus, queues) in [(1, 1), (2, 2), (4, 4), (10, 4)] {
            let fs = Fs::new("mac", server(), cpus, false).unwrap();
            assert_eq!(fs.device_id(), 26);
            assert_eq!(fs.features(), 0);
            assert_eq!(fs.queue_max_sizes(), vec![1024u16; 1 + queues]);
            let mut c = [0xffu8; 48];
            fs.read_config(0, &mut c);
            assert_eq!(&c[..3], b"mac");
            assert!(c[3..36].iter().all(|b| *b == 0), "the tag is NUL padded");
            assert_eq!(
                u32::from_le_bytes(c[36..40].try_into().unwrap()),
                queues as u32
            );
            assert!(c[40..].iter().all(|b| *b == 0));
        }
        let long = "x".repeat(36);
        assert!(Fs::new(&long, server(), 1, false).is_ok());
        assert_eq!(
            Fs::new(&"x".repeat(37), server(), 1, false).err(),
            Some(BadTag)
        );
        assert_eq!(Fs::new("", server(), 1, false).err(), Some(BadTag));
        assert_eq!(Fs::new("a\0b", server(), 1, false).err(), Some(BadTag));
    }

    #[test]
    fn a_request_split_across_descriptors_is_answered_in_guest_memory() {
        let (server, mem) = (server(), memory());
        let out = roundtrip(&server, &mem, &request(op::INIT, 0, &init_body()), 3, 80);
        assert_eq!(out.len(), 16 + abi::INIT_OUT_LEN);
        assert_eq!(u64::from_le_bytes(out[8..16].try_into().unwrap()), 77);
        assert_eq!(&out[16..24], &[7, 0, 0, 0, 45, 0, 0, 0]);

        let out = roundtrip(
            &server,
            &mem,
            &request(op::LOOKUP, 1, b"hello\0"),
            2,
            16 + 128,
        );
        assert_eq!(out.len(), 16 + 128);
        let node = u64::from_le_bytes(out[16..24].try_into().unwrap());
        let size = u64::from_le_bytes(out[16 + 40 + 8..][..8].try_into().unwrap());
        assert_eq!(size, 19);

        let mut open = Writer::default();
        open.u32(0).u32(0);
        let out = roundtrip(&server, &mem, &request(op::OPEN, node, &open.0), 1, 32);
        let fh = u64::from_le_bytes(out[16..24].try_into().unwrap());
        let mut read = Writer::default();
        read.u64(fh).u64(6).u32(4096).zeros(20);
        let out = roundtrip(
            &server,
            &mem,
            &request(op::READ, node, &read.0),
            1,
            16 + 4096,
        );
        assert_eq!(&out[16..], b"from the Mac\n");
        let mut after = [0u8; 4];
        mem.read(REPLY + out.len() as u64, &mut after).unwrap();
        assert_eq!(after, [0xee; 4]);
    }

    #[test]
    fn a_header_that_lies_about_its_length_is_served_on_what_was_sent() {
        let (server, mem) = (server(), memory());
        roundtrip(&server, &mem, &request(op::INIT, 0, &init_body()), 1, 80);
        let mut req = request(op::LOOKUP, 1, b"hello\0");
        req[0..4].copy_from_slice(&u32::MAX.to_le_bytes());
        let out = roundtrip(&server, &mem, &req, 1, 16 + 128);
        assert_eq!(out.len(), 16 + 128);
        req[0..4].copy_from_slice(&43u32.to_le_bytes());
        let out = roundtrip(&server, &mem, &req, 1, 16 + 128);
        assert_eq!(i32::from_le_bytes(out[4..8].try_into().unwrap()), -22);
        req[0..4].copy_from_slice(&3u32.to_le_bytes());
        let out = roundtrip(&server, &mem, &req, 1, 16 + 128);
        assert_eq!(i32::from_le_bytes(out[4..8].try_into().unwrap()), -22);
    }

    #[test]
    fn buffers_outside_ram_are_never_touched() {
        let (server, mem) = (server(), memory());
        roundtrip(&server, &mem, &request(op::INIT, 0, &init_body()), 1, 80);
        let req = request(op::GETATTR, 1, &[0; 16]);
        mem.write(REQ, &req).unwrap();
        for reply_addr in [BASE - 0x1000, BASE + (1 << 20) - 8, u64::MAX - 4] {
            let (mut drv, mut q) = Driver::new(&mem, BASE, 16);
            drv.add(
                &mem,
                &[(REQ, req.len() as u32, false), (reply_addr, 120, true)],
            );
            let chain = q.pop(&mem).unwrap().unwrap();
            assert_eq!(handle(&server, &mem, &chain), (0, Some(op::GETATTR), true));
        }
        for (addr, len) in [(BASE - 64, 56u32), (u64::MAX - 8, 56), (REQ, 39)] {
            let (mut drv, mut q) = Driver::new(&mem, BASE, 16);
            drv.add(&mem, &[(addr, len, false), (REPLY, 120, true)]);
            let chain = q.pop(&mem).unwrap().unwrap();
            assert_eq!(handle(&server, &mem, &chain).0, 0);
        }
    }

    fn wait_used(drv: &mut Driver, mem: &GuestMemory) -> (u16, u32) {
        for _ in 0..2000 {
            if let Some(used) = drv.take_used(mem) {
                return used;
            }
            std::thread::sleep(Duration::from_millis(1));
        }
        panic!("the worker did not answer");
    }

    #[test]
    fn workers_serve_their_queues_and_forget_goes_on_the_high_priority_one() {
        let mem = Arc::new(memory());
        let (chip, irq) = interrupt();
        let mut fs = Fs::new("mac", server(), 2, false).unwrap();
        let (mut hi, q0) = Driver::new(&mem, BASE, 16);
        let (mut req, q1) = Driver::new(&mem, BASE + 0x2000, 16);
        let (_, q2) = Driver::new(&mem, BASE + 0x4000, 16);
        fs.activate(Activation {
            mem: mem.clone(),
            irq,
            queues: vec![q0, q1, q2],
            features: 0,
        });

        let send = |drv: &mut Driver, bytes: &[u8], cap: u32| {
            mem.write(REQ, bytes).unwrap();
            let mut bufs = vec![(REQ, bytes.len() as u32, false)];
            if cap > 0 {
                bufs.push((REPLY, cap, true));
            }
            drv.add(&mem, &bufs)
        };
        let head = send(&mut req, &request(op::INIT, 0, &init_body()), 80);
        crate::irq::enter_vcpu(3);
        fs.notify(1);
        assert_eq!(wait_used(&mut req, &mem), (head, 80));
        super::super::testing::wait_high(&chip);
        assert_eq!(chip.routed(INTID), [3]);

        send(&mut req, &request(op::LOOKUP, 1, b"hello\0"), 144);
        fs.notify(1);
        assert_eq!(wait_used(&mut req, &mem).1, 144);
        let mut node = [0u8; 8];
        mem.read(REPLY + 16, &mut node).unwrap();
        let node = u64::from_le_bytes(node);

        let head = send(&mut hi, &request(op::FORGET, node, &1u64.to_le_bytes()), 0);
        fs.notify(0);
        assert_eq!(wait_used(&mut hi, &mem), (head, 0));
        send(&mut req, &request(op::GETATTR, node, &[0; 16]), 120);
        fs.notify(1);
        assert_eq!(wait_used(&mut req, &mem).1, 16);
        let mut err = [0u8; 4];
        mem.read(REPLY + 4, &mut err).unwrap();
        assert_eq!(i32::from_le_bytes(err), -116, "the node was forgotten");

        fs.reset();
        assert!(fs.workers.is_empty() && fs.lanes.is_none());
        assert!(fs.shared.server.negotiated().is_none());
        fs.notify(1);
    }

    #[test]
    fn a_hold_is_offered_after_a_quick_answer_and_not_after_writeback() {
        let mem = Arc::new(memory());
        let (chip, irq) = interrupt();
        let mut accepting = FakeFs::new().accepting();
        accepting.add_file(b"file", b"content");
        let mut fs = Fs::new("mac", Server::new(Box::new(accepting)), 2, false).unwrap();
        assert_eq!(fs.shared.hold, HOLD, "on unless the environment says not");
        let (_, q0) = Driver::new(&mem, BASE, 16);
        let (mut req, q1) = Driver::new(&mem, BASE + 0x2000, 16);
        let (_, q2) = Driver::new(&mem, BASE + 0x4000, 16);
        fs.activate(Activation {
            mem: mem.clone(),
            irq,
            queues: vec![q0, q1, q2],
            features: 0,
        });
        let mut ask = |fs: &mut Fs, bytes: &[u8], cap: u32| {
            mem.write(REQ, bytes).unwrap();
            req.add(
                &mem,
                &[(REQ, bytes.len() as u32, false), (REPLY, cap, true)],
            );
            fs.notify(1);
            let held = fs.hold();
            if let Some((_, flags)) = &held {
                assert!(flags.held.load(Ordering::SeqCst));
            }
            let used = wait_used(&mut req, &mem);
            if let Some((_, flags)) = &held {
                flags.held.store(false, Ordering::SeqCst);
            }
            (held, used)
        };

        let (held, _) = ask(&mut fs, &request(op::INIT, 0, &init_body()), 80);
        assert!(held.is_none());
        super::super::testing::wait_high(&chip);
        let (held, used) = ask(&mut fs, &request(op::LOOKUP, 1, b"file\0"), 144);
        assert_eq!(used.1, 144);
        let (limit, flags) = held.expect("held after a quick answer");
        assert_eq!(limit, HOLD);
        for _ in 0..2000 {
            if flags.owed.load(Ordering::SeqCst) {
                break;
            }
            std::thread::sleep(Duration::from_millis(1));
        }
        assert!(flags.owed.swap(false, Ordering::SeqCst), "left to the vCPU");
        let mut node = [0u8; 8];
        mem.read(REPLY + 16, &mut node).unwrap();
        let node = u64::from_le_bytes(node);

        let write = |flags: u32| {
            let mut w = Writer::default();
            w.u64(1).u64(0).u32(1).u32(flags).u64(0).u32(0).u32(0);
            w.0.push(b'x');
            request(op::WRITE, node, &w.0)
        };
        ask(&mut fs, &write(abi::WRITE_CACHE), 24);
        let (held, _) = ask(&mut fs, &write(0), 24);
        assert!(held.is_none(), "not after writeback");
        let (held, _) = ask(&mut fs, &request(op::GETATTR, node, &[0; 16]), 120);
        assert!(held.is_some(), "after a write that was waited for");

        fs.notify(0);
        assert!(fs.hold().is_none());
        fs.reset();
    }

    struct Stuck {
        entered: Arc<(Mutex<bool>, Condvar)>,
        takes: Mutex<Option<Duration>>,
        quick: bool,
    }

    impl Stuck {
        fn new(takes: Option<Duration>, quick: bool) -> (Self, Arc<(Mutex<bool>, Condvar)>) {
            let entered = Arc::new((Mutex::new(false), Condvar::new()));
            let stuck = Stuck {
                entered: entered.clone(),
                takes: Mutex::new(takes),
                quick,
            };
            (stuck, entered)
        }
    }

    fn wait_entered(entered: &(Mutex<bool>, Condvar)) {
        let mut inside = entered.0.lock().unwrap();
        while !*inside {
            inside = entered.1.wait(inside).unwrap();
        }
    }

    impl fileshare::Filesystem for Stuck {
        fn quick(&self, _: u64, _: fileshare::fs::Need) -> bool {
            self.quick
        }
        fn lookup(
            &self,
            _: &fileshare::fs::Ctx,
            _: u64,
            _: &[u8],
        ) -> fileshare::fs::Result<fileshare::fs::Entry> {
            Err(fileshare::Errno::ENOENT)
        }
        fn forget(&self, _: u64, _: u64) {}
        fn getattr(
            &self,
            _: &fileshare::fs::Ctx,
            _: u64,
            _: Option<u64>,
        ) -> fileshare::fs::Result<(fileshare::fs::Attr, Duration)> {
            *self.entered.0.lock().unwrap() = true;
            self.entered.1.notify_all();
            let takes = *self.takes.lock().unwrap();
            match takes {
                Some(takes) => {
                    std::thread::sleep(takes);
                    Err(fileshare::Errno::ENOENT)
                }
                None => loop {
                    std::thread::park();
                },
            }
        }
        fn readlink(&self, _: &fileshare::fs::Ctx, _: u64) -> fileshare::fs::Result<Vec<u8>> {
            Err(fileshare::Errno::EINVAL)
        }
        fn open(
            &self,
            _: &fileshare::fs::Ctx,
            _: u64,
            _: u32,
            _: bool,
        ) -> fileshare::fs::Result<(u64, u32)> {
            Err(fileshare::Errno::ENOENT)
        }
        fn read(
            &self,
            _: &fileshare::fs::Ctx,
            _: u64,
            _: u64,
            _: u64,
            _: IoVecs<'_>,
        ) -> fileshare::fs::Result<usize> {
            Err(fileshare::Errno::EBADF)
        }
        fn release(&self, _: u64, _: u64) {}
        fn opendir(
            &self,
            _: &fileshare::fs::Ctx,
            _: u64,
            _: u32,
        ) -> fileshare::fs::Result<(u64, u32)> {
            Err(fileshare::Errno::ENOENT)
        }
        fn readdir(
            &self,
            _: &fileshare::fs::Ctx,
            _: u64,
            _: u64,
            _: u64,
            _: &mut fileshare::fs::DirSink,
        ) -> fileshare::fs::Result<()> {
            Err(fileshare::Errno::EBADF)
        }
        fn releasedir(&self, _: u64, _: u64) {}
        fn statfs(
            &self,
            _: &fileshare::fs::Ctx,
            _: u64,
        ) -> fileshare::fs::Result<fileshare::fs::Statfs> {
            Err(fileshare::Errno::ENOENT)
        }
        fn destroy(&self) {}
    }

    #[test]
    fn a_worker_stuck_in_a_mac_call_does_not_stop_the_vmm_exiting() {
        let mem = Arc::new(memory());
        let (_chip, irq) = interrupt();
        let (stuck, entered) = Stuck::new(None, false);
        let server = Server::new(Box::new(stuck));
        let mut fs = Fs::new("mac", server, 1, false).unwrap();
        let (_hi, q0) = Driver::new(&mem, BASE, 16);
        let (mut req, q1) = Driver::new(&mem, BASE + 0x2000, 16);
        fs.activate(Activation {
            mem: mem.clone(),
            irq,
            queues: vec![q0, q1],
            features: 0,
        });
        let init = request(op::INIT, 0, &init_body());
        mem.write(REQ, &init).unwrap();
        req.add(&mem, &[(REQ, init.len() as u32, false), (REPLY, 80, true)]);
        fs.notify(1);
        wait_used(&mut req, &mem);
        let getattr = request(op::GETATTR, 1, &[0; 16]);
        mem.write(REQ, &getattr).unwrap();
        req.add(
            &mem,
            &[(REQ, getattr.len() as u32, false), (REPLY, 120, true)],
        );
        fs.notify(1);
        wait_entered(&entered);

        let (done, dropped) = std::sync::mpsc::channel();
        std::thread::spawn(move || {
            drop(fs);
            let _ = done.send(());
        });
        assert!(
            dropped.recv_timeout(Duration::from_secs(5)).is_ok(),
            "dropping the device waited for a worker that will never finish"
        );
    }

    #[test]
    fn a_vcpu_stuck_in_a_mac_call_does_not_stop_the_vmm_exiting() {
        let (stuck, entered) = Stuck::new(None, true);
        let mut rig = Rig::new(Box::new(stuck), |_| {});
        rig.send(&request(op::GETATTR, 1, &[0; 16]), 120);
        let gate = Arc::new(crate::pause::Gate::new());
        let (lanes, vgate) = (rig.lanes.clone(), gate.clone());
        std::thread::spawn(move || {
            vgate.attach();
            vgate.enter();
            lanes.notified(1);
            vgate.leave();
        });
        wait_entered(&entered);
        let (done, ended) = std::sync::mpsc::channel();
        std::thread::spawn(move || {
            gate.stop();
            let left = gate.ended(&|| {}, Duration::from_millis(100));
            drop(rig);
            let _ = done.send(left);
        });
        assert_eq!(
            ended.recv_timeout(Duration::from_secs(5)),
            Ok(1),
            "the machine's end waited for a vCPU that will never come back"
        );
    }

    struct Rig {
        mem: Arc<GuestMemory>,
        chip: Arc<crate::irq::testing::RecordingChip>,
        irq: Arc<Interrupt>,
        fs: Fs,
        req: Driver,
        hi: Driver,
        lanes: Arc<dyn super::super::Inline>,
    }

    impl Rig {
        fn new(
            filesystem: Box<dyn fileshare::Filesystem>,
            change: impl FnOnce(&mut Shared),
        ) -> Rig {
            let mem = Arc::new(memory());
            let (chip, irq) = interrupt();
            let mut fs = Fs::new("mac", Server::new(filesystem), 1, false)
                .unwrap()
                .with(|shared| {
                    shared.stats = Some(Stats::default());
                    shared.inline_max = INLINE_MAX;
                    shared.slow_after = SLOW;
                    change(shared);
                });
            let (hi, q0) = Driver::new(&mem, BASE, 16);
            let (req, mut q1) = Driver::new(&mem, BASE + 0x2000, 16);
            q1.event_idx = true;
            fs.activate(Activation {
                mem: mem.clone(),
                irq: irq.clone(),
                queues: vec![q0, q1],
                features: 0,
            });
            let lanes = fs.inline().expect("a vCPU serves unless switched off");
            let mut rig = Rig {
                mem,
                chip,
                irq,
                fs,
                req,
                hi,
                lanes,
            };
            rig.send(&request(op::INIT, 0, &init_body()), 80);
            rig.lanes.notified(1);
            wait_used(&mut rig.req, &rig.mem);
            assert_eq!(rig.counts(), (0, 1));
            rig
        }

        fn send(&mut self, bytes: &[u8], cap: u32) -> u16 {
            let at = self.req_at();
            self.mem.write(REQ + at, bytes).unwrap();
            self.req.add(
                &self.mem,
                &[
                    (REQ + at, bytes.len() as u32, false),
                    (REPLY + at * 16, cap, true),
                ],
            )
        }

        fn req_at(&self) -> u64 {
            (self.mem.read_u16(self.req.avail + 2).unwrap() as u64 % 8) * 0x200
        }

        fn counts(&self) -> (u64, u64) {
            let stats = self.fs.shared.stats.as_ref().unwrap();
            (
                stats.inline.load(Ordering::Relaxed),
                stats.handed.load(Ordering::Relaxed),
            )
        }

        fn slow(&self) -> u32 {
            self.fs.lanes.as_ref().unwrap().lanes[1]
                .slow
                .load(Ordering::Relaxed)
        }
    }

    fn quick_fake() -> Box<FakeFs> {
        let mut fake = FakeFs::new().accepting().quick();
        fake.add_file(b"file", b"content");
        Box::new(fake)
    }

    fn read_request(node: u64, size: u32) -> Vec<u8> {
        let mut read = Writer::default();
        read.u64(0).u64(0).u32(size).zeros(20);
        request(op::READ, node, &read.0)
    }

    #[test]
    fn a_vcpu_serves_a_quick_request_inside_its_notification() {
        let mut rig = Rig::new(quick_fake(), |_| {});
        crate::irq::enter_vcpu(2);
        rig.irq.ack(1);
        rig.mem.write_u16(rig.req.avail + 4 + 16 * 2, 1).unwrap();
        let head = rig.send(&request(op::LOOKUP, 1, b"file\0"), 144);
        rig.lanes.notified(1);
        assert_eq!(rig.req.take_used(&rig.mem), Some((head, 144)));
        assert_eq!(rig.counts(), (1, 1));
        super::super::testing::wait_high(&rig.chip);
        assert_eq!(rig.chip.routed(INTID).last(), Some(&2));
        rig.fs.reset();
    }

    #[test]
    fn a_request_the_server_does_not_call_quick_is_the_workers() {
        let mut fake = FakeFs::new().accepting();
        fake.add_file(b"file", b"content");
        let mut rig = Rig::new(Box::new(fake), |_| {});
        let head = rig.send(&request(op::LOOKUP, 1, b"file\0"), 144);
        rig.lanes.notified(1);
        assert_eq!(wait_used(&mut rig.req, &rig.mem), (head, 144));
        assert_eq!(rig.counts(), (0, 2));
        rig.fs.reset();
    }

    #[test]
    fn a_sync_a_listing_a_large_read_and_writeback_are_the_workers() {
        let mut rig = Rig::new(quick_fake(), |shared| shared.inline_max = 4096);
        rig.send(&request(op::LOOKUP, 1, b"file\0"), 144);
        rig.lanes.notified(1);
        let mut node = [0u8; 8];
        rig.mem.read(REPLY + 16 * 0x200 + 16, &mut node).unwrap();
        let node = u64::from_le_bytes(node);
        assert!(rig.req.take_used(&rig.mem).is_some());
        assert_eq!(rig.counts(), (1, 1));

        let write = |flags: u32, len: usize| {
            let mut w = Writer::default();
            w.u64(1)
                .u64(0)
                .u32(len as u32)
                .u32(flags)
                .u64(0)
                .u32(0)
                .u32(0);
            w.0.extend(std::iter::repeat_n(b'x', len));
            request(op::WRITE, node, &w.0)
        };
        let mut fsync = Writer::default();
        fsync.u64(0).u32(0).u32(0);
        let mut readdir = Writer::default();
        readdir.u64(0).u64(0).u32(4096).zeros(20);
        let theirs: Vec<(Vec<u8>, u32)> = vec![
            (request(op::FSYNC, node, &fsync.0), 16),
            (request(op::READDIRPLUS, 1, &readdir.0), 16 + 4096),
            (request(op::FSYNCDIR, 1, &fsync.0), 16),
            (request(op::FALLOCATE, node, &[0; 32]), 16),
            (read_request(node, 8192), 16 + 8192),
            (write(abi::WRITE_CACHE, 1), 24),
            (write(0, 4097), 24),
        ];
        let mut handed = 1;
        for (bytes, cap) in &theirs {
            rig.send(bytes, *cap);
            rig.lanes.notified(1);
            wait_used(&mut rig.req, &rig.mem);
            handed += 1;
            assert_eq!(rig.counts(), (1, handed), "opcode {:?}", &bytes[4..8]);
        }
        for (bytes, cap) in [(read_request(node, 4096), 16 + 4096), (write(0, 300), 24)] {
            rig.send(&bytes, cap);
            rig.lanes.notified(1);
            assert!(rig.req.take_used(&rig.mem).is_some(), "served where sent");
        }
        assert_eq!(rig.counts(), (3, handed));
        let forget = request(op::FORGET, node, &1u64.to_le_bytes());
        rig.mem.write(REQ + 0x1800, &forget).unwrap();
        rig.hi
            .add(&rig.mem, &[(REQ + 0x1800, forget.len() as u32, false)]);
        rig.lanes.notified(0);
        wait_used(&mut rig.hi, &rig.mem);
        assert_eq!(rig.counts(), (3, handed));
        rig.fs.reset();
    }

    #[test]
    fn a_vcpu_serves_one_request_and_wakes_the_worker_for_the_rest() {
        let mut rig = Rig::new(quick_fake(), |_| {});
        let first = rig.send(&request(op::LOOKUP, 1, b"file\0"), 144);
        let second = rig.send(&request(op::LOOKUP, 1, b"none\0"), 144);
        rig.lanes.notified(1);
        let mut seen = vec![rig.req.take_used(&rig.mem).expect("the first, at once").0];
        seen.push(wait_used(&mut rig.req, &rig.mem).0);
        seen.sort();
        assert_eq!(seen, [first, second]);
        assert_eq!(rig.counts().0, 1);
        rig.fs.reset();
    }

    #[test]
    fn after_a_slow_request_the_worker_serves_until_the_queue_is_quick_again() {
        let (stuck, _entered) = Stuck::new(Some(Duration::from_millis(3)), true);
        let mut rig = Rig::new(Box::new(stuck), |shared| {
            shared.slow_after = Duration::from_millis(1)
        });
        let getattr = request(op::GETATTR, 1, &[0; 16]);
        let lookup = request(op::LOOKUP, 1, b"none\0");
        rig.send(&getattr, 120);
        rig.lanes.notified(1);
        assert!(rig.req.take_used(&rig.mem).is_some(), "the vCPU's, slowly");
        assert_eq!((rig.counts().0, rig.slow()), (1, QUICK_AGAIN));
        rig.send(&getattr, 120);
        rig.lanes.notified(1);
        wait_used(&mut rig.req, &rig.mem);
        for left in (0..QUICK_AGAIN).rev() {
            assert_eq!(rig.slow(), left + 1);
            rig.send(&lookup, 144);
            rig.lanes.notified(1);
            wait_used(&mut rig.req, &rig.mem);
            for _ in 0..2000 {
                if rig.slow() == left {
                    break;
                }
                std::thread::sleep(Duration::from_millis(1));
            }
            assert_eq!(rig.slow(), left);
        }
        assert_eq!(rig.counts().0, 1, "none of those was a vCPU's");
        rig.send(&lookup, 144);
        rig.lanes.notified(1);
        assert!(rig.req.take_used(&rig.mem).is_some());
        assert_eq!(rig.counts().0, 2);
        rig.fs.reset();
    }

    #[test]
    fn with_the_switch_off_every_request_is_the_workers() {
        let mem = Arc::new(memory());
        let (_chip, irq) = interrupt();
        let mut fs = Fs::new("mac", Server::new(quick_fake()), 1, false)
            .unwrap()
            .with(|shared| shared.inline_max = 0);
        let (_hi, q0) = Driver::new(&mem, BASE, 16);
        let (_req, q1) = Driver::new(&mem, BASE + 0x2000, 16);
        fs.activate(Activation {
            mem,
            irq,
            queues: vec![q0, q1],
            features: 0,
        });
        assert!(fs.inline().is_none());
        fs.reset();
    }

    #[test]
    fn a_reset_waits_for_a_vcpu_inside_a_call_and_nothing_is_published_after() {
        let (stuck, entered) = Stuck::new(Some(Duration::from_millis(150)), true);
        let mut rig = Rig::new(Box::new(stuck), |shared| shared.slow_after = Duration::ZERO);
        rig.send(&request(op::GETATTR, 1, &[0; 16]), 120);
        let lanes = rig.lanes.clone();
        let vcpu = std::thread::spawn(move || lanes.notified(1));
        wait_entered(&entered);
        let start = Instant::now();
        rig.fs.reset();
        assert!(
            start.elapsed() >= Duration::from_millis(100),
            "the reset did not wait for the call"
        );
        vcpu.join().unwrap();
        assert_eq!(rig.req.take_used(&rig.mem), None);
        assert!(rig.fs.shared.server.negotiated().is_none());
        let reply = REPLY + rig.req_at() * 16;
        rig.mem.write(reply, &[0xee; 16]).unwrap();
        rig.send(&request(op::GETATTR, 1, &[0; 16]), 120);
        rig.lanes.notified(1);
        std::thread::sleep(Duration::from_millis(20));
        assert_eq!(rig.req.take_used(&rig.mem), None);
        let mut after = [0u8; 16];
        rig.mem.read(reply, &mut after).unwrap();
        assert_eq!(after, [0xee; 16], "a dead queue's request was answered");
    }

    #[test]
    fn a_pause_does_not_wait_for_a_vcpu_that_waits_in_a_reset() {
        let (stuck, entered) = Stuck::new(Some(Duration::from_millis(400)), false);
        let mut rig = Rig::new(Box::new(stuck), |_| {});
        rig.send(&request(op::GETATTR, 1, &[0; 16]), 120);
        rig.lanes.notified(1);
        wait_entered(&entered);
        let gate = Arc::new(crate::pause::Gate::new());
        let vgate = gate.clone();
        let (began_tx, began) = std::sync::mpsc::channel();
        let vcpu = std::thread::spawn(move || {
            vgate.attach();
            vgate.enter();
            began_tx.send(()).unwrap();
            let start = Instant::now();
            rig.fs.reset();
            let waited = start.elapsed();
            let total = vgate.back();
            vgate.leave();
            (waited, total)
        });
        began.recv().unwrap();
        std::thread::sleep(Duration::from_millis(30));
        let start = Instant::now();
        let paused = gate.pause(&|| {}, &|| 1000).expect("the pause is made");
        assert!(paused.changed);
        assert!(
            start.elapsed() < Duration::from_millis(200),
            "the pause waited for the reset, which waits for the Mac"
        );
        assert_eq!(gate.account().claimed, 1);
        std::thread::sleep(Duration::from_millis(450));
        assert_eq!(gate.resume(&|| 1600).unwrap().total, 600);
        let (waited, total) = vcpu.join().unwrap();
        assert!(
            waited >= Duration::from_millis(300),
            "the reset did not wait"
        );
        assert_eq!(total, Some(600), "the total it must apply");
    }

    #[test]
    fn a_pause_does_not_wait_for_a_vcpu_inside_a_call_on_the_mac() {
        let (stuck, entered) = Stuck::new(Some(Duration::from_millis(400)), true);
        let mut rig = Rig::new(Box::new(stuck), |_| {});
        rig.send(&request(op::GETATTR, 1, &[0; 16]), 120);
        let gate = Arc::new(crate::pause::Gate::new());
        let (lanes, vgate) = (rig.lanes.clone(), gate.clone());
        let back = Arc::new(AtomicBool::new(false));
        let returned = back.clone();
        let vcpu = std::thread::spawn(move || {
            vgate.attach();
            vgate.enter();
            lanes.notified(1);
            let total = vgate.back();
            returned.store(true, Ordering::SeqCst);
            vgate.leave();
            total
        });
        wait_entered(&entered);
        let start = Instant::now();
        let paused = gate.pause(&|| {}, &|| 1000).expect("the pause is made");
        assert!(paused.changed);
        assert!(
            start.elapsed() < Duration::from_millis(200),
            "the pause waited for the Mac's call"
        );
        assert_eq!(gate.account().claimed, 1);
        std::thread::sleep(Duration::from_millis(500));
        assert!(
            !back.load(Ordering::SeqCst),
            "the vCPU went on under a pause"
        );
        assert!(
            rig.req.take_used(&rig.mem).is_some(),
            "published as a worker would"
        );
        let resumed = gate.resume(&|| 1700).unwrap();
        assert_eq!(resumed.total, 700);
        assert_eq!(vcpu.join().unwrap(), Some(700), "the total it must apply");
        rig.fs.reset();
    }
}
