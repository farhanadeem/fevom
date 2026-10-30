use std::collections::{HashSet, VecDeque};
use std::io::{Read, Write};
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::os::unix::net::UnixStream;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Condvar, Mutex, RwLock};
use std::time::{Duration, Instant};

use crate::fsevents::{self, op, Event, Frame, Order, Side, Status};
use crate::passthrough::{Change, Passthrough};
use crate::watcher::{self, Stream};

const MAX_STREAMS: usize = 16;

const LIVE_LIMIT: Duration = Duration::from_secs(2);

const LINGER: Duration = Duration::from_secs(crate::server::LONG.as_secs() + 60);

type ChangeKey = (u8, u8, u64, u64, Vec<u8>);

#[derive(Default)]
struct Queue {
    events: VecDeque<(Vec<u8>, Vec<ChangeKey>)>,
    events_len: usize,
    waiting: HashSet<ChangeKey>,
    ack: Option<Vec<u8>>,
    acked: u64,
    closed: bool,
    overflowed: bool,
}

struct Out {
    q: Mutex<Queue>,
    cv: Condvar,
    watch: Watch,
}

struct Watch {
    shares: Vec<Arc<Passthrough>>,
    state: Mutex<WatchState>,
}

#[derive(Default)]
struct WatchState {
    acked: Vec<Vec<u8>>,
    behind: bool,
}

impl Watch {
    fn change(&self, change: impl FnOnce(&mut WatchState)) {
        let mut state = self.state.lock().unwrap();
        change(&mut state);
        let none = Vec::new();
        let roots = if state.behind { &none } else { &state.acked };
        for share in &self.shares {
            share.set_watch(roots);
        }
    }
}

impl Out {
    fn close(&self) {
        self.q.lock().unwrap().closed = true;
        self.cv.notify_all();
    }

    fn records(&self, records: &[Event]) -> Option<String> {
        if records.is_empty() {
            return None;
        }
        let mut why = None;
        let all = [Event {
            op: op::INVAL_ALL,
            ..Event::default()
        }];
        let mut q = self.q.lock().unwrap();
        if q.closed {
            return None;
        }
        let mut keep = Vec::with_capacity(records.len());
        let mut keys = Vec::new();
        for r in records {
            if r.op != op::CHANGE {
                q.waiting.clear();
                keys.clear();
                keep.push(r.clone());
                continue;
            }
            let key = (r.mask, r.flags, r.parent, r.child, r.name.clone());
            if q.waiting.insert(key.clone()) {
                keys.push(key);
                keep.push(r.clone());
            }
        }
        if keep.is_empty() {
            return None;
        }
        let mut frame = Vec::new();
        if let Err(refused) = fsevents::encode_events(&keep, &mut frame) {
            let bad = keep.iter().find(|r| r.validate().is_err());
            why = Some(format!("a record the format refuses ({refused}): {bad:?}"));
            frame.clear();
            for key in keys.drain(..) {
                q.waiting.remove(&key);
            }
            let _ = fsevents::encode_events(&all, &mut frame);
        }
        let full = q.events_len + frame.len() > fsevents::QUEUE_MAX;
        if full && !q.overflowed {
            q.overflowed = true;
            self.watch.change(|w| w.behind = true);
        }
        if full {
            why = Some(format!(
                "{} bytes of records were waiting for the guest to read",
                q.events_len + frame.len()
            ));
            q.events.clear();
            q.waiting.clear();
            keys.clear();
            frame.clear();
            let _ = fsevents::encode_events(&all, &mut frame);
            q.events_len = 0;
        }
        q.events_len += frame.len();
        q.events.push_back((frame, keys));
        drop(q);
        self.cv.notify_all();
        why
    }
}

fn under(path: &[u8], root: &[u8]) -> bool {
    path == root
        || root == b"/" && path.starts_with(b"/")
        || path.starts_with(root) && path.get(root.len()) == Some(&b'/')
}

fn write_loop(out: &Out, mut stream: UnixStream) {
    loop {
        let (frame, is_ack) = {
            let mut q = out.q.lock().unwrap();
            loop {
                if q.closed {
                    return;
                }
                if let Some(ack) = q.ack.take() {
                    break (ack, true);
                }
                if let Some((f, keys)) = q.events.pop_front() {
                    q.events_len -= f.len();
                    for key in &keys {
                        q.waiting.remove(key);
                    }
                    break (f, false);
                }
                q = out.cv.wait(q).unwrap();
            }
        };
        if stream.write_all(&frame).is_err() {
            out.close();
            return;
        }
        let mut q = out.q.lock().unwrap();
        if is_ack {
            q.acked += 1;
            out.cv.notify_all();
        } else if q.overflowed && q.events.is_empty() {
            q.overflowed = false;
            out.watch.change(|w| w.behind = false);
        }
    }
}

fn read_frame_by(
    stream: &mut UnixStream,
    order: &mut Order,
    until: Option<Instant>,
) -> Result<Option<Frame>, &'static str> {
    let mut head = [0u8; fsevents::HEADER_LEN];
    let mut got = 0;
    if let Some(until) = until {
        let left = until.saturating_duration_since(Instant::now());
        if left.is_zero() {
            return Ok(None);
        }
        let _ = stream.set_read_timeout(Some(left));
        let first = loop {
            match stream.read(&mut head) {
                Err(e) if e.kind() == std::io::ErrorKind::Interrupted => {}
                other => break other,
            }
        };
        let _ = stream.set_read_timeout(None);
        match first {
            Ok(0) => return Err("closed"),
            Ok(n) => got = n,
            Err(e)
                if matches!(
                    e.kind(),
                    std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut
                ) =>
            {
                return Ok(None)
            }
            Err(_) => return Err("closed"),
        }
    }
    stream.read_exact(&mut head[got..]).map_err(|_| "closed")?;
    read_rest(stream, order, &head).map(Some)
}

fn read_frame(stream: &mut UnixStream, order: &mut Order) -> Result<Frame, &'static str> {
    let mut head = [0u8; fsevents::HEADER_LEN];
    stream.read_exact(&mut head).map_err(|_| "closed")?;
    read_rest(stream, order, &head)
}

fn read_rest(
    stream: &mut UnixStream,
    order: &mut Order,
    head: &[u8; fsevents::HEADER_LEN],
) -> Result<Frame, &'static str> {
    let head = *head;
    let (len, ty) = fsevents::header(Side::Guest, &head)?;
    let mut payload = vec![0u8; len];
    stream.read_exact(&mut payload).map_err(|_| "closed")?;
    let frame = fsevents::decode_payload(ty, &payload)?;
    order.check(&frame)?;
    Ok(frame)
}

struct Watchdog {
    kq: OwnedFd,
    shares: Vec<Arc<Passthrough>>,
    state: Mutex<(u64, bool)>,
}

const DOG_TIMER: usize = 1;
const DOG_STOP: usize = 2;

impl Watchdog {
    fn new(shares: Vec<Arc<Passthrough>>) -> std::io::Result<Self> {
        let kq = unsafe { libc::kqueue() };
        if kq < 0 {
            return Err(std::io::Error::last_os_error());
        }
        let kq = unsafe { OwnedFd::from_raw_fd(kq) };
        let dog = Self {
            kq,
            shares,
            state: Mutex::new((0, false)),
        };
        dog.change(
            DOG_STOP,
            libc::EVFILT_USER,
            libc::EV_ADD | libc::EV_CLEAR,
            0,
            0,
            0,
        )?;
        Ok(dog)
    }

    fn change(
        &self,
        ident: usize,
        filter: i16,
        flags: u16,
        fflags: u32,
        data: isize,
        udata: u64,
    ) -> std::io::Result<()> {
        let ev = libc::kevent {
            ident,
            filter,
            flags,
            fflags,
            data,
            udata: udata as *mut libc::c_void,
        };
        let n = unsafe {
            libc::kevent(
                self.kq.as_raw_fd(),
                &ev,
                1,
                std::ptr::null_mut(),
                0,
                std::ptr::null(),
            )
        };
        if n < 0 {
            return Err(std::io::Error::last_os_error());
        }
        Ok(())
    }

    fn arm(&self) {
        let mut state = self.state.lock().unwrap();
        state.0 += 1;
        state.1 = true;
        let _ = self.change(
            DOG_TIMER,
            libc::EVFILT_TIMER,
            libc::EV_ADD | libc::EV_ONESHOT,
            0,
            crate::passthrough::STUCK.as_millis() as isize,
            state.0,
        );
    }

    fn disarm(&self) {
        let mut state = self.state.lock().unwrap();
        state.1 = false;
        for share in &self.shares {
            share.set_overdue(false);
        }
        let _ = self.change(
            DOG_TIMER,
            libc::EVFILT_TIMER,
            libc::EV_DELETE,
            0,
            0,
            state.0,
        );
    }

    fn stop(&self) {
        let _ = self.change(DOG_STOP, libc::EVFILT_USER, 0, libc::NOTE_TRIGGER, 0, 0);
    }

    fn run(&self, ring: impl Fn()) {
        loop {
            let mut ev: libc::kevent = unsafe { std::mem::zeroed() };
            let n = unsafe {
                libc::kevent(
                    self.kq.as_raw_fd(),
                    std::ptr::null(),
                    0,
                    &mut ev,
                    1,
                    std::ptr::null(),
                )
            };
            if n < 0 {
                if std::io::Error::last_os_error().kind() == std::io::ErrorKind::Interrupted {
                    continue;
                }
                return;
            }
            if n == 0 {
                continue;
            }
            if ev.filter == libc::EVFILT_USER {
                return;
            }
            if ev.filter != libc::EVFILT_TIMER || ev.ident != DOG_TIMER {
                continue;
            }
            let state = self.state.lock().unwrap();
            if !(state.1 && state.0 == ev.udata as u64) {
                continue;
            }
            for share in &self.shares {
                share.set_overdue(true);
            }
            ring();
        }
    }
}

pub struct Feed {
    shares: Vec<Arc<Passthrough>>,
    build: String,
    busy: AtomicBool,
    log: Box<dyn Fn(&str) + Send + Sync>,
    out: Mutex<Option<Arc<Out>>>,
    linger: Duration,
}

impl Feed {
    pub fn new(
        shares: Vec<Arc<Passthrough>>,
        build: &str,
        log: Box<dyn Fn(&str) + Send + Sync>,
    ) -> Arc<Self> {
        Self::with_linger(shares, build, log, LINGER)
    }

    pub fn with_linger(
        shares: Vec<Arc<Passthrough>>,
        build: &str,
        log: Box<dyn Fn(&str) + Send + Sync>,
        linger: Duration,
    ) -> Arc<Self> {
        Arc::new(Self {
            shares,
            build: build.into(),
            busy: AtomicBool::new(false),
            log,
            out: Mutex::new(None),
            linger,
        })
    }

    pub fn resumed(&self) {
        let out = self.out.lock().unwrap().clone();
        if let Some(out) = out {
            out.records(&[Event {
                op: op::INVAL_ALL,
                ..Event::default()
            }]);
        }
    }

    pub fn serve(self: &Arc<Self>, stream: UnixStream) -> bool {
        if self.busy.swap(true, Ordering::AcqRel) {
            return false;
        }
        let me = self.clone();
        let spawned = std::thread::Builder::new()
            .name("fs-feed".into())
            .spawn(move || {
                me.run(stream);
                me.busy.store(false, Ordering::Release);
            });
        if spawned.is_err() {
            self.busy.store(false, Ordering::Release);
            return false;
        }
        true
    }

    pub fn connected(&self) -> bool {
        self.busy.load(Ordering::Acquire)
    }

    fn run(self: &Arc<Self>, mut stream: UnixStream) {
        let mut order = Order::new();
        let hello = read_frame(&mut stream, &mut order);
        let mut reply = Vec::new();
        let ours = Frame::Hello {
            version: fsevents::VERSION,
            build: self.build.clone(),
        };
        if fsevents::encode(&ours, &mut reply).is_err() || stream.write_all(&reply).is_err() {
            return;
        }
        match hello {
            Ok(Frame::Hello { version, build })
                if fsevents::hello_matches(version, &build, &self.build) => {}
            Ok(Frame::Hello { version, build }) => {
                (self.log)(&format!(
                    "change feed: the guest's helper is version {version} of build '{build}', \
                     not {} of '{}'; refused",
                    fsevents::VERSION,
                    self.build
                ));
                return;
            }
            _ => return,
        }
        let Ok(writer_stream) = stream.try_clone() else {
            return;
        };
        let out = Arc::new(Out {
            q: Mutex::new(Queue::default()),
            cv: Condvar::new(),
            watch: Watch {
                shares: self.shares.clone(),
                state: Mutex::default(),
            },
        });
        let writer = {
            let out = out.clone();
            std::thread::Builder::new()
                .name("fs-feed-out".into())
                .spawn(move || write_loop(&out, writer_stream))
        };
        let Ok(writer) = writer else { return };
        let dog = Watchdog::new(self.shares.clone()).map_err(|e| e.to_string());
        let dog = dog.and_then(|dog| {
            let dog = Arc::new(dog);
            let (me, out, theirs) = (self.clone(), out.clone(), dog.clone());
            std::thread::Builder::new()
                .name("fs-feed-dog".into())
                .spawn(move || {
                    theirs.run(|| {
                        (me.log)(&format!(
                            "change feed: a batch has been in hand for {:?}; the guest is \
                                 told to drop its caches, and nothing is answered for long \
                                 until the batch is through",
                            crate::passthrough::STUCK
                        ));
                        out.records(&[Event {
                            op: op::INVAL_ALL,
                            ..Event::default()
                        }]);
                    })
                })
                .map(|thread| (dog, thread))
                .map_err(|e| e.to_string())
        });
        let (dog, dog_thread) = match dog {
            Ok((dog, thread)) => (Some(dog), Some(thread)),
            Err(e) => {
                (self.log)(&format!(
                    "change feed: no watchdog for the record builder ({e}); a batch that \
                     stays in hand is bounded only when it ends"
                ));
                (None, None)
            }
        };
        let work = Arc::new(Work {
            q: Mutex::default(),
            cv: Condvar::new(),
            streams: AtomicUsize::new(0),
        });
        let builder = {
            let (me, work, out, dog) = (self.clone(), work.clone(), out.clone(), dog.clone());
            std::thread::Builder::new()
                .name("fs-feed-rec".into())
                .spawn(move || me.build(&work, &out, dog.as_deref()))
        };
        let end_dog = |dog: Option<Arc<Watchdog>>, thread: Option<std::thread::JoinHandle<()>>| {
            if let Some(dog) = dog {
                dog.stop();
            }
            if let Some(thread) = thread {
                let _ = thread.join();
            }
        };
        let Ok(builder) = builder else {
            end_dog(dog, dog_thread);
            out.close();
            let _ = writer.join();
            return;
        };
        for s in &self.shares {
            s.set_feeding(true);
        }
        *self.out.lock().unwrap() = Some(out.clone());
        let mut streams: Vec<(Stream, Vec<Vec<u8>>)> = Vec::new();
        let wanted: Arc<RwLock<Vec<Vec<u8>>>> = Arc::default();
        let recent: Arc<Mutex<watcher::Recent>> = Arc::default();
        let mut acks = 0u64;
        let mut current: Vec<Vec<u8>> = Vec::new();
        let mut lingering: Vec<(Vec<u8>, Instant)> = Vec::new();
        let inval_all = [Event {
            op: op::INVAL_ALL,
            ..Event::default()
        }];
        loop {
            let until = lingering.iter().map(|(_, until)| *until).min();
            let (seq, paths) = match read_frame_by(&mut stream, &mut order, until) {
                Ok(Some(Frame::Watch { seq, paths })) => (seq, paths),
                Ok(None) => {
                    let now = Instant::now();
                    lingering.retain(|(_, until)| *until > now);
                    streams.retain(|(_, had)| {
                        had.iter()
                            .any(|r| current.contains(r) || lingering.iter().any(|(p, _)| p == r))
                    });
                    let mut reported = current.clone();
                    reported.extend(lingering.iter().map(|(p, _)| p.clone()));
                    *wanted.write().unwrap() = reported;
                    work.streams.store(streams.len(), Ordering::Relaxed);
                    continue;
                }
                Ok(Some(_)) => break,
                Err(why) => {
                    if why != "closed" {
                        (self.log)(&format!("change feed: {why}; the connection is closed"));
                    }
                    break;
                }
            };
            let (mut status, reals) = self.resolve(&paths);
            let mut roots: Vec<Vec<u8>> = Vec::new();
            for real in reals.iter().flatten() {
                if !roots.contains(real) {
                    roots.push(real.clone());
                }
            }
            let now = Instant::now();
            lingering.retain(|(p, until)| *until > now && !roots.contains(p));
            if !self.linger.is_zero() {
                for p in current.iter().filter(|p| !roots.contains(p)) {
                    lingering.push((p.clone(), now + self.linger));
                }
            }
            current.retain(|p| roots.contains(p));
            out.watch.change(|w| w.acked = current.clone());
            streams.retain(|(_, had)| {
                had.iter()
                    .any(|r| roots.contains(r) || lingering.iter().any(|(p, _)| p == r))
            });
            let mut add: Vec<Vec<u8>> = roots
                .iter()
                .filter(|r| !streams.iter().any(|(_, had)| had.contains(r)))
                .cloned()
                .collect();
            let mut blind = false;
            if !add.is_empty() && streams.len() >= MAX_STREAMS && !lingering.is_empty() {
                lingering.clear();
                streams.retain(|(_, had)| had.iter().any(|r| roots.contains(r)));
                blind = true;
            }
            if !add.is_empty() && streams.len() >= MAX_STREAMS {
                streams.clear();
                add = roots.clone();
                current.clear();
                out.watch.change(|w| w.acked.clear());
                blind = true;
            }
            let mut reported = roots.clone();
            reported.extend(lingering.iter().map(|(p, _)| p.clone()));
            *wanted.write().unwrap() = reported;
            if !add.is_empty() {
                let since = watcher::current_event_id();
                match Stream::start(&add, since, recent.clone(), self.sink(&work, &wanted)) {
                    Ok(s) => {
                        if !s.wait_live(LIVE_LIMIT) {
                            (self.log)(
                                "change feed: the Mac's event stream did not say it was ready; \
                                 the first changes under the new paths may go unreported",
                            );
                        }
                        streams.push((s, add))
                    }
                    Err(e) => {
                        (self.log)(&format!("change feed: {e}"));
                        for (s, real) in status.iter_mut().zip(&reals) {
                            if real.as_ref().is_some_and(|r| add.contains(r)) {
                                *s = Status::Failed;
                            }
                        }
                        wanted.write().unwrap().retain(|r| !add.contains(r));
                        roots.retain(|r| !add.contains(r));
                    }
                }
            }
            if blind {
                out.records(&inval_all);
            }
            work.streams.store(streams.len(), Ordering::Relaxed);
            current = roots;
            out.watch.change(|w| w.acked = current.clone());
            let mut ack = Vec::new();
            if fsevents::encode(&Frame::Watching { seq, status }, &mut ack).is_err() {
                break;
            }
            acks += 1;
            let mut q = out.q.lock().unwrap();
            q.ack = Some(ack);
            out.cv.notify_all();
            while q.acked < acks && !q.closed {
                q = out.cv.wait(q).unwrap();
            }
            if q.closed {
                break;
            }
        }
        out.watch.change(|w| w.acked.clear());
        *self.out.lock().unwrap() = None;
        drop(streams);
        work.close();
        let _ = builder.join();
        end_dog(dog, dog_thread);
        out.close();
        let _ = stream.shutdown(std::net::Shutdown::Both);
        let _ = writer.join();
        for s in &self.shares {
            s.set_feeding(false);
        }
    }

    fn resolve(&self, paths: &[String]) -> (Vec<Status>, Vec<Option<Vec<u8>>>) {
        paths
            .iter()
            .map(|path| {
                let mut answer = Status::Outside;
                for share in &self.shares {
                    match share.watch_status(path.as_bytes()) {
                        (Status::Ok, Some(real)) => return (Status::Ok, Some(real)),
                        (Status::Outside, _) => {}
                        (other, _) => answer = other,
                    }
                }
                (answer, None)
            })
            .unzip()
    }

    fn sink(
        self: &Arc<Self>,
        work: &Arc<Work>,
        wanted: &Arc<RwLock<Vec<Vec<u8>>>>,
    ) -> watcher::Sink {
        let (work, wanted) = (work.clone(), wanted.clone());
        Box::new(move |mut batch: Vec<Change>, arrival: watcher::Arrival| {
            {
                use crate::passthrough::fse;
                let wanted = wanted.read().unwrap();
                batch.retain(|c| {
                    c.flags & fse::LOST != 0
                        || (c.flags & !fse::MARKS != 0
                            && wanted.iter().any(|root| under(&c.path, root)))
                });
            }
            if !batch.is_empty() {
                work.push(batch, arrival);
            }
        })
    }

    fn build(self: &Arc<Self>, work: &Work, out: &Out, dog: Option<&Watchdog>) {
        while let Some((batch, arrival, waiting)) = work.take() {
            if let Some(dog) = dog {
                dog.arm();
            }
            if let Some(c) = batch
                .iter()
                .find(|c| c.flags & crate::passthrough::fse::LOST != 0)
            {
                let late = arrival.at.elapsed().unwrap_or_default();
                (self.log)(&format!(
                    "change feed: the Mac's event stream lost events (flags {:#x} at {}); the \
                     guest is told to drop its caches; the callback came {} after the one \
                     before, which took {:?}; the longest such wait in this stream's {} \
                     callbacks {:?}; {} events in it, the Mac {} ahead, at QoS {:#x}; {} \
                     streams; the builder took it {:?} later with {} batches waiting",
                    c.flags,
                    String::from_utf8_lossy(&c.path),
                    arrival
                        .gap
                        .map_or("first".to_string(), |g| format!("{g:?}")),
                    arrival.prev_took,
                    arrival.callbacks,
                    arrival.worst_gap,
                    arrival.events,
                    arrival.behind,
                    arrival.qos,
                    work.streams.load(Ordering::Relaxed),
                    late,
                    waiting,
                ));
            }
            for share in &self.shares {
                let began = Instant::now();
                let records = share.records_at(&batch, arrival.at);
                let took = began.elapsed();
                if took > crate::passthrough::STUCK {
                    (self.log)(&format!(
                        "change feed: a batch of {} changes took {:?} to turn into {} records \
                         (the Mac {} ahead when it arrived, {} callbacks in the stream); the \
                         guest is told to drop its caches",
                        batch.len(),
                        took,
                        records.len(),
                        arrival.behind,
                        arrival.callbacks,
                    ));
                }
                if let Some(why) = out.records(&records) {
                    (self.log)(&format!(
                        "change feed: {why}; the guest is told to drop its caches instead"
                    ));
                }
            }
            if let Some(dog) = dog {
                dog.disarm();
            }
        }
    }
}

struct Work {
    q: Mutex<WorkQueue>,
    cv: Condvar,
    streams: AtomicUsize,
}

#[derive(Default)]
struct WorkQueue {
    batches: VecDeque<(Vec<Change>, watcher::Arrival)>,
    changes: usize,
    closed: bool,
}

const WORK_MAX: usize = 65536;

impl Work {
    fn push(&self, batch: Vec<Change>, arrival: watcher::Arrival) {
        let mut q = self.q.lock().unwrap();
        if q.closed {
            return;
        }
        if q.changes + batch.len() > WORK_MAX {
            q.batches.clear();
            q.changes = 1;
            q.batches.push_back((
                vec![Change {
                    path: Vec::new(),
                    flags: crate::passthrough::fse::USER_DROPPED,
                    ino: 0,
                }],
                arrival,
            ));
        } else {
            q.changes += batch.len();
            q.batches.push_back((batch, arrival));
        }
        drop(q);
        self.cv.notify_one();
    }

    fn take(&self) -> Option<(Vec<Change>, watcher::Arrival, usize)> {
        let mut q = self.q.lock().unwrap();
        loop {
            if let Some((batch, arrival)) = q.batches.pop_front() {
                q.changes -= batch.len().min(q.changes);
                let waiting = q.batches.len();
                return Some((batch, arrival, waiting));
            }
            if q.closed {
                return None;
            }
            q = self.cv.wait(q).unwrap();
        }
    }

    fn close(&self) {
        self.q.lock().unwrap().closed = true;
        self.cv.notify_all();
    }
}

#[cfg(test)]
mod tests {

    use std::os::unix::fs::MetadataExt;
    use std::path::{Path, PathBuf};
    use std::process::Command;
    use std::time::{Duration, Instant};

    use super::*;
    use crate::abi::ROOT_ID;
    use crate::client::Client;
    use crate::fsevents::{flag, mask};
    use crate::passthrough::{fse, Config};
    use crate::server::Server;
    use crate::server::{LONG, VALID};

    struct Rig {
        dir: PathBuf,
        fs: Arc<Passthrough>,
        c: Client,
    }

    impl Rig {
        fn new(name: &str) -> Self {
            Self::with(name, |_| {})
        }

        fn with(name: &str, tune: impl FnOnce(&mut Config)) -> Self {
            let dir = std::env::temp_dir().join(format!("fev-feed-{name}-{}", std::process::id()));
            let _ = std::fs::remove_dir_all(&dir);
            std::fs::create_dir_all(dir.join("w")).unwrap();
            let dir = std::fs::canonicalize(dir).unwrap();
            let mut cfg = Config::new(&dir);
            tune(&mut cfg);
            let fs = Arc::new(Passthrough::new(&cfg).unwrap());
            let c = Client::new(Server::shared(fs.clone()));
            c.init();
            Self { dir, fs, c }
        }

        fn path(&self, rel: &str) -> PathBuf {
            self.dir.join(rel)
        }

        fn w(&self) -> u64 {
            self.c.lookup(ROOT_ID, b"w").unwrap().nodeid
        }

        fn change(&self, rel: &str, flags: u32, ino: u64) -> Change {
            Change {
                path: self.path(rel).into_os_string().into_encoded_bytes(),
                flags,
                ino,
            }
        }

        fn ino(&self, rel: &str) -> u64 {
            std::fs::symlink_metadata(self.path(rel)).unwrap().ino()
        }

        fn feed(&self) -> Arc<Feed> {
            Feed::new(vec![self.fs.clone()], "test-build", Box::new(|_| {}))
        }

        fn feed_lingering(&self, linger: Duration) -> Arc<Feed> {
            Feed::with_linger(
                vec![self.fs.clone()],
                "test-build",
                Box::new(|_| {}),
                linger,
            )
        }
    }

    impl Drop for Rig {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.dir);
        }
    }

    struct Helper {
        s: UnixStream,
        order: Order,
        seq: u32,
        buf: Vec<u8>,
    }

    impl Helper {
        fn connect(feed: &Arc<Feed>) -> Option<Self> {
            let (ours, theirs) = UnixStream::pair().unwrap();
            ours.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
            feed.serve(theirs).then_some(Self {
                s: ours,
                order: Order::new(),
                seq: 0,
                buf: Vec::new(),
            })
        }

        fn send(&mut self, frame: &Frame) {
            let mut out = Vec::new();
            fsevents::encode(frame, &mut out).unwrap();
            self.s.write_all(&out).unwrap();
        }

        fn recv(&mut self) -> Option<Frame> {
            loop {
                match fsevents::decode(Side::Host, &self.buf) {
                    Ok((frame, used)) => {
                        self.buf.drain(..used);
                        self.order.check(&frame).expect("frames in order");
                        return Some(frame);
                    }
                    Err(fsevents::Error::Short) => {}
                    Err(fsevents::Error::Bad(why)) => panic!("the VMM sent a bad frame: {why}"),
                }
                let mut chunk = [0u8; 65536];
                match self.s.read(&mut chunk) {
                    Ok(0) | Err(_) => return None,
                    Ok(n) => self.buf.extend_from_slice(&chunk[..n]),
                }
            }
        }

        fn hello(&mut self, build: &str) -> Option<Frame> {
            self.send(&Frame::Hello {
                version: fsevents::VERSION,
                build: build.into(),
            });
            self.recv()
        }

        fn watch(&mut self, paths: &[&Path]) -> Vec<Status> {
            self.seq += 1;
            self.send(&Frame::Watch {
                seq: self.seq,
                paths: paths.iter().map(|p| p.to_str().unwrap().into()).collect(),
            });
            loop {
                match self.recv().expect("an acknowledgement") {
                    Frame::Watching { seq, status } => {
                        assert_eq!(seq, self.seq);
                        return status;
                    }
                    Frame::Events(_) => {}
                    other => panic!("{other:?}"),
                }
            }
        }

        fn until(&mut self, done: impl Fn(&Event) -> bool) -> Vec<Event> {
            let mut all = Vec::new();
            let start = Instant::now();
            while start.elapsed() < Duration::from_secs(5) {
                match self.recv() {
                    Some(Frame::Events(records)) => {
                        let hit = records.iter().any(&done);
                        all.extend(records);
                        if hit {
                            return all;
                        }
                    }
                    Some(other) => panic!("{other:?}"),
                    None => break,
                }
            }
            panic!("the record never came; got {all:?}");
        }
    }

    fn mac(dir: &Path, line: &str) {
        let ok = Command::new("/bin/sh")
            .arg("-c")
            .arg(line)
            .current_dir(dir)
            .status()
            .unwrap()
            .success();
        assert!(ok, "{line}");
    }

    fn named<'a>(records: &'a [Event], opcode: u8, name: &[u8]) -> Option<&'a Event> {
        records
            .iter()
            .find(|r| r.op == opcode && (r.name == name || r.name2 == name))
    }

    #[test]
    fn records_name_the_nodes_the_guest_holds() {
        let r = Rig::new("records");
        std::fs::write(r.path("w/file"), b"one").unwrap();
        std::fs::create_dir(r.path("w/sub")).unwrap();
        let w = r.w();
        let file = r.c.lookup(w, b"file").unwrap().nodeid;
        let sub = r.c.lookup(w, b"sub").unwrap().nodeid;

        std::fs::write(r.path("w/file"), b"two!").unwrap();
        let got =
            r.fs.records(&[r.change("w/file", fse::MODIFIED, r.ino("w/file"))]);
        assert_eq!(
            got,
            [Event {
                op: op::CHANGE,
                mask: mask::CONTENT,
                parent: w,
                child: file,
                name: b"file".to_vec(),
                ..Event::default()
            }]
        );
        assert_eq!(
            r.fs.records(&[r.change("w/file", fse::MODIFIED, r.ino("w/file"))]),
            []
        );
        mac(&r.dir, "chmod 600 w/file");
        let got =
            r.fs.records(&[r.change("w/file", fse::INODE_META, r.ino("w/file"))]);
        assert_eq!(
            (got.len(), got[0].op, got[0].mask),
            (1, op::CHANGE, mask::ATTRIB)
        );
        std::fs::write(r.path("w/file"), b"three").unwrap();
        r.c.getattr(file).unwrap();
        r.c.lookup(w, b"file").unwrap();
        let got =
            r.fs.records(&[r.change("w/file", fse::MODIFIED, r.ino("w/file"))]);
        assert_eq!((got.len(), got[0].mask), (1, mask::CONTENT));

        std::fs::write(r.path("w/new"), b"x").unwrap();
        let got =
            r.fs.records(&[r.change("w/new", fse::CREATED | fse::MODIFIED, r.ino("w/new"))]);
        assert_eq!(got.len(), 2, "{got:?}");
        assert_eq!((got[0].op, got[0].parent, got[0].child), (op::CREATE, w, 0));
        assert_eq!(
            (got[1].op, got[1].mask, got[1].child),
            (op::CHANGE, mask::CONTENT, 0)
        );
        std::fs::create_dir(r.path("w/sub/deep")).unwrap();
        let got = r.fs.records(&[r.change(
            "w/sub/deep",
            fse::CREATED | fse::IS_DIR,
            r.ino("w/sub/deep"),
        )]);
        assert_eq!(got.len(), 1);
        assert_eq!(
            (got[0].op, got[0].flags, got[0].parent),
            (op::CREATE, flag::ISDIR, sub)
        );
        std::fs::write(r.path("w/sub/deep/f"), b"").unwrap();
        assert_eq!(
            r.fs.records(&[r.change("w/sub/deep/f", fse::CREATED, r.ino("w/sub/deep/f"))]),
            []
        );

        let ino = r.ino("w/file");
        std::fs::remove_file(r.path("w/file")).unwrap();
        let got = r.fs.records(&[r.change("w/file", fse::REMOVED, ino)]);
        assert_eq!(
            got,
            [Event {
                op: op::DELETE,
                flags: flag::LAST_LINK,
                parent: w,
                child: file,
                name: b"file".to_vec(),
                ..Event::default()
            }]
        );
        std::fs::write(r.path("w/a"), b"").unwrap();
        std::fs::hard_link(r.path("w/a"), r.path("w/b")).unwrap();
        let a = r.c.lookup(w, b"a").unwrap().nodeid;
        let ino = r.ino("w/a");
        std::fs::remove_file(r.path("w/a")).unwrap();
        let got =
            r.fs.records(&[r.change("w/a", fse::REMOVED | fse::IS_HARDLINK, ino)]);
        assert_eq!(got.len(), 2, "{got:?}");
        assert_eq!((got[0].op, got[0].flags, got[0].child), (op::DELETE, 0, a));
        assert_eq!(
            (got[1].op, got[1].mask, got[1].child, got[1].name.len()),
            (op::CHANGE, mask::ATTRIB, a, 0)
        );
        for rec in &got {
            rec.validate().unwrap();
        }
    }

    #[test]
    fn a_tree_removed_and_made_again_is_told_to_the_directory_that_went() {
        let r = Rig::new("remade");
        std::fs::create_dir(r.path("w/src")).unwrap();
        std::fs::write(r.path("w/src/a"), b"1").unwrap();
        let w = r.w();
        let old = r.c.lookup(w, b"src").unwrap().nodeid;
        let a = r.c.lookup(old, b"a").unwrap().nodeid;
        let (ino_a, ino_old) = (r.ino("w/src/a"), r.ino("w/src"));
        std::fs::remove_dir_all(r.path("w/src")).unwrap();
        std::fs::create_dir(r.path("w/src")).unwrap();
        let new = r.c.lookup(w, b"src").unwrap().nodeid;
        assert_ne!(new, old);
        assert_ne!(r.ino("w/src"), ino_old);
        let got = r.fs.records(&[
            r.change("w/src/a", fse::REMOVED, ino_a),
            r.change(
                "w/src",
                fse::REMOVED | fse::CREATED | fse::IS_DIR,
                r.ino("w/src"),
            ),
        ]);
        let gone = named(&got, op::DELETE, b"a").expect("the file's removal");
        assert_eq!((gone.parent, gone.child), (old, a), "{got:?}");
        assert_eq!(gone.flags & flag::LAST_LINK, flag::LAST_LINK);
        let dir = named(&got, op::DELETE, b"src").expect("the directory's removal");
        assert_eq!((dir.parent, dir.child), (w, old), "{got:?}");
        assert_eq!(
            dir.flags,
            flag::ISDIR | flag::LAST_LINK,
            "a removed directory is gone"
        );
        let made = named(&got, op::CREATE, b"src").expect("the new directory");
        assert_eq!((made.parent, made.flags), (w, flag::ISDIR));
        assert!(
            got.iter().all(|e| e.parent != new && e.child != new),
            "nothing happened to the new directory: {got:?}"
        );
        for rec in &got {
            rec.validate().unwrap();
        }
        std::fs::create_dir(r.path("w/keep")).unwrap();
        std::fs::write(r.path("w/keep/b"), b"").unwrap();
        std::fs::create_dir(r.path("w/tmp")).unwrap();
        std::fs::hard_link(r.path("w/keep/b"), r.path("w/tmp/b")).unwrap();
        let keep = r.c.lookup(w, b"keep").unwrap().nodeid;
        let b = r.c.lookup(keep, b"b").unwrap().nodeid;
        let tmp = r.c.lookup(w, b"tmp").unwrap().nodeid;
        let ino_b = r.ino("w/keep/b");
        std::fs::remove_file(r.path("w/tmp/b")).unwrap();
        let got =
            r.fs.records(&[r.change("w/tmp/b", fse::REMOVED | fse::IS_HARDLINK, ino_b)]);
        assert!(
            got.iter().all(|e| e.parent != keep && e.child != b),
            "the name in the other directory is still there: {got:?}"
        );
        assert!(got.iter().all(|e| e.parent == tmp), "{got:?}");
    }

    #[test]
    fn a_change_under_a_directory_renamed_since_is_told_where_the_file_is() {
        let r = Rig::new("under-renamed");
        std::fs::create_dir(r.path("w/d")).unwrap();
        std::fs::write(r.path("w/d/f"), b"1").unwrap();
        let w = r.w();
        let d = r.c.lookup(w, b"d").unwrap().nodeid;
        let f = r.c.lookup(d, b"f").unwrap().nodeid;
        std::fs::write(r.path("w/d/f"), b"22").unwrap();
        std::fs::write(r.path("w/d/new"), b"n").unwrap();
        let (ino_f, ino_new, ino_d) = (r.ino("w/d/f"), r.ino("w/d/new"), r.ino("w/d"));
        std::fs::rename(r.path("w/d"), r.path("w/d2")).unwrap();
        let got = r.fs.records(&[
            r.change("w/d/f", fse::MODIFIED, ino_f),
            r.change("w/d/new", fse::CREATED | fse::MODIFIED, ino_new),
            r.change("w/d", fse::RENAMED | fse::IS_DIR, ino_d),
            r.change("w/d2", fse::RENAMED | fse::IS_DIR, ino_d),
        ]);
        assert!(
            got.iter().all(|e| e.op != op::DELETE),
            "nothing was removed: {got:?}"
        );
        let edit = got
            .iter()
            .find(|e| e.op == op::CHANGE && e.name == b"f")
            .expect("the edit");
        assert_eq!(
            (edit.parent, edit.child, edit.mask),
            (d, f, mask::CONTENT),
            "{got:?}"
        );
        let made = named(&got, op::CREATE, b"new").expect("the new file");
        assert_eq!(made.parent, d, "{got:?}");
        let moved: Vec<_> = got.iter().filter(|e| e.op == op::RENAME).collect();
        assert_eq!(moved.len(), 1, "{got:?}");
        assert_eq!(
            (moved[0].parent, moved[0].parent2, moved[0].child),
            (w, w, d)
        );
        assert_eq!(
            (&moved[0].name[..], &moved[0].name2[..]),
            (&b"d"[..], &b"d2"[..])
        );
        for rec in &got {
            rec.validate().unwrap();
        }
        let ino_f = r.ino("w/d2/f");
        std::fs::remove_file(r.path("w/d2/f")).unwrap();
        std::fs::rename(r.path("w/d2"), r.path("w/d3")).unwrap();
        let got = r.fs.records(&[r.change("w/d2/f", fse::REMOVED, ino_f)]);
        let gone = named(&got, op::DELETE, b"f").expect("the removal");
        assert_eq!((gone.parent, gone.child), (d, f), "{got:?}");
        std::fs::write(r.path("w/d3/x"), b"x").unwrap();
        let x = r.c.lookup(d, b"x").unwrap().nodeid;
        let ino_x = r.ino("w/d3/x");
        std::fs::rename(r.path("w/d3/x"), r.path("w/d3/y")).unwrap();
        std::fs::rename(r.path("w/d3"), r.path("w/d4")).unwrap();
        let got = r.fs.records(&[r.change("w/d3/x", fse::RENAMED, ino_x)]);
        assert_eq!(got.len(), 1, "{got:?}");
        assert_eq!(
            (got[0].op, got[0].parent, got[0].parent2, got[0].child),
            (op::RENAME, d, d, x),
            "{got:?}"
        );
        assert_eq!(
            (&got[0].name[..], &got[0].name2[..]),
            (&b"x"[..], &b"y"[..])
        );
        assert_eq!(
            r.fs.records(&[r.change("w/d3/y", fse::RENAMED, ino_x)]),
            [],
            "the other half of a rename already told"
        );
    }

    #[test]
    fn renames_and_atomic_saves_are_one_record_with_the_replaced_node() {
        let r = Rig::new("rename");
        std::fs::write(r.path("w/a"), b"a").unwrap();
        std::fs::write(r.path("w/target"), b"old").unwrap();
        std::fs::create_dir_all(r.path("w/dir/inner")).unwrap();
        std::fs::create_dir(r.path("w/other")).unwrap();
        let w = r.w();
        let a = r.c.lookup(w, b"a").unwrap().nodeid;
        let old = r.c.lookup(w, b"target").unwrap().nodeid;
        let dir = r.c.lookup(w, b"dir").unwrap().nodeid;
        let other = r.c.lookup(w, b"other").unwrap().nodeid;

        let ino = r.ino("w/a");
        std::fs::rename(r.path("w/a"), r.path("w/other/b")).unwrap();
        let got = r.fs.records(&[
            r.change("w/a", fse::RENAMED, ino),
            r.change("w/other/b", fse::RENAMED, ino),
        ]);
        assert_eq!(
            got,
            [Event {
                op: op::RENAME,
                parent: w,
                child: a,
                parent2: other,
                name: b"a".to_vec(),
                name2: b"b".to_vec(),
                ..Event::default()
            }]
        );
        assert_eq!(r.c.lookup(other, b"b").unwrap().nodeid, a);

        std::fs::write(r.path("w/.target.tmp"), b"new content").unwrap();
        let ino = r.ino("w/.target.tmp");
        std::fs::rename(r.path("w/.target.tmp"), r.path("w/target")).unwrap();
        let save = [
            r.change(
                "w/.target.tmp",
                fse::CREATED | fse::RENAMED | fse::MODIFIED,
                ino,
            ),
            r.change("w/target", fse::RENAMED, ino),
        ];
        let got = r.fs.records(&save);
        assert_eq!(got.len(), 2, "{got:?}");
        assert_eq!(
            got[0],
            Event {
                op: op::RENAME,
                flags: flag::VICTIM_GONE,
                parent: w,
                parent2: w,
                victim: old,
                name: b".target.tmp".to_vec(),
                name2: b"target".to_vec(),
                ..Event::default()
            }
        );
        assert_eq!(
            (got[1].op, got[1].mask, &got[1].name[..]),
            (op::CHANGE, mask::CONTENT, &b"target"[..])
        );

        let now = r.c.lookup(w, b"target").unwrap().nodeid;
        assert_ne!(now, old);
        std::fs::write(r.path("w/.t2"), b"newer").unwrap();
        let ino = r.ino("w/.t2");
        std::fs::rename(r.path("w/.t2"), r.path("w/target")).unwrap();
        let newest = r.c.lookup(w, b"target").unwrap().nodeid;
        r.c.list(w, true).unwrap();
        let got = r.fs.records(&[
            r.change("w/.t2", fse::CREATED | fse::RENAMED, ino),
            r.change("w/target", fse::RENAMED, ino),
        ]);
        assert_eq!(
            (got[0].op, got[0].victim, got[0].child),
            (op::RENAME, now, newest)
        );
        assert_eq!(got[0].flags, flag::VICTIM_GONE);
        let ino = r.ino("w/target");
        std::fs::remove_file(r.path("w/target")).unwrap();
        assert!(r.c.lookup(w, b"target").is_err());
        let got = r.fs.records(&[r.change("w/target", fse::REMOVED, ino)]);
        assert_eq!(
            (got.len(), got[0].op, got[0].child),
            (1, op::DELETE, newest)
        );
        assert_eq!(got[0].flags, flag::LAST_LINK);
        std::fs::write(r.path("w/born"), b"x").unwrap();
        let born = r.c.lookup(w, b"born").unwrap().nodeid;
        let got =
            r.fs.records(&[r.change("w/born", fse::CREATED | fse::MODIFIED, r.ino("w/born"))]);
        assert_eq!(
            (got[0].op, got[1].op, got[1].child),
            (op::CREATE, op::CHANGE, born)
        );
        std::fs::write(r.path("w/born"), b"xy").unwrap();
        let got =
            r.fs.records(&[r.change("w/born", fse::CREATED | fse::MODIFIED, r.ino("w/born"))]);
        assert_eq!((got.len(), got[0].op), (1, op::CHANGE));

        let ino = r.ino("w/dir");
        std::fs::rename(r.path("w/dir"), r.path("w/other/moved")).unwrap();
        let got = r.fs.records(&[
            r.change("w/dir", fse::RENAMED | fse::IS_DIR, ino),
            r.change("w/other/moved", fse::RENAMED | fse::IS_DIR, ino),
        ]);
        assert_eq!(
            got,
            [Event {
                op: op::RENAME,
                flags: flag::ISDIR,
                parent: w,
                child: dir,
                parent2: other,
                name: b"dir".to_vec(),
                name2: b"moved".to_vec(),
                ..Event::default()
            }]
        );
        let ino = r.ino("w/other/b");
        std::fs::create_dir(r.path("unseen")).unwrap();
        std::fs::rename(r.path("w/other/b"), r.path("unseen/b")).unwrap();
        let got = r.fs.records(&[
            r.change("w/other/b", fse::RENAMED, ino),
            r.change("unseen/b", fse::RENAMED, ino),
        ]);
        assert_eq!(
            (got[0].op, got[0].parent, got[0].child),
            (op::DELETE, other, a)
        );
        assert_eq!(
            got[0].flags & flag::LAST_LINK,
            0,
            "it still exists, elsewhere"
        );
        for rec in &got {
            rec.validate().unwrap();
        }
    }

    #[test]
    fn a_rename_whose_halves_come_apart_is_still_one_record() {
        let r = Rig::new("halves");
        std::fs::write(r.path("w/a"), b"a").unwrap();
        std::fs::write(r.path("w/target"), b"old").unwrap();
        std::fs::create_dir(r.path("w/dir")).unwrap();
        let w = r.w();
        let a = r.c.lookup(w, b"a").unwrap().nodeid;
        let old = r.c.lookup(w, b"target").unwrap().nodeid;
        let dir = r.c.lookup(w, b"dir").unwrap().nodeid;

        let ino = r.ino("w/a");
        std::fs::rename(r.path("w/a"), r.path("w/b")).unwrap();
        r.c.list(w, true).unwrap();
        let got = r.fs.records(&[r.change("w/a", fse::RENAMED, ino)]);
        assert_eq!(got.len(), 1, "{got:?}");
        assert_eq!(
            (got[0].op, got[0].child, &got[0].name[..], &got[0].name2[..]),
            (op::RENAME, a, &b"a"[..], &b"b"[..])
        );
        assert_eq!(r.fs.records(&[r.change("w/b", fse::RENAMED, ino)]), []);

        std::fs::write(r.path("w/.tmp"), b"new content").unwrap();
        let ino = r.ino("w/.tmp");
        std::fs::rename(r.path("w/.tmp"), r.path("w/target")).unwrap();
        let got =
            r.fs.records(&[r.change("w/.tmp", fse::CREATED | fse::RENAMED | fse::MODIFIED, ino)]);
        assert_eq!(got.len(), 2, "{got:?}");
        assert_eq!(
            (got[0].op, got[0].victim, got[0].flags),
            (op::RENAME, old, flag::VICTIM_GONE)
        );
        assert_eq!((got[1].op, got[1].mask), (op::CHANGE, mask::CONTENT));
        assert_eq!(r.fs.records(&[r.change("w/target", fse::RENAMED, ino)]), []);
        let new = r.c.lookup(w, b"target").unwrap().nodeid;
        std::fs::write(r.path("w/target"), b"edited").unwrap();
        let got =
            r.fs.records(&[r.change("w/target", fse::CREATED | fse::RENAMED | fse::MODIFIED, ino)]);
        assert_eq!((got.len(), got[0].op, got[0].child), (1, op::CHANGE, new));

        let ino = r.ino("w/dir");
        std::fs::create_dir(r.path("w/into")).unwrap();
        let into = r.c.lookup(w, b"into").unwrap().nodeid;
        std::fs::rename(r.path("w/dir"), r.path("w/into/dir")).unwrap();
        let got =
            r.fs.records(&[r.change("w/dir", fse::RENAMED | fse::IS_DIR, ino)]);
        assert_eq!(
            (got[0].op, got[0].child, got[0].parent2, got[0].flags),
            (op::RENAME, dir, into, flag::ISDIR)
        );
        if std::fs::metadata(r.path("W")).is_ok() {
            std::fs::write(r.path("w/readme"), b"").unwrap();
            let readme = r.c.lookup(w, b"readme").unwrap().nodeid;
            let ino = r.ino("w/readme");
            std::fs::rename(r.path("w/readme"), r.path("w/README")).unwrap();
            let got = r.fs.records(&[
                r.change("w/readme", fse::RENAMED, ino),
                r.change("w/README", fse::RENAMED, ino),
            ]);
            assert_eq!(got.len(), 1, "{got:?}");
            assert_eq!(
                (got[0].op, got[0].child, &got[0].name[..], &got[0].name2[..]),
                (op::RENAME, readme, &b"readme"[..], &b"README"[..])
            );
        }
        std::fs::write(r.path("w/one"), b"").unwrap();
        std::fs::hard_link(r.path("w/one"), r.path("w/two")).unwrap();
        let one = r.c.lookup(w, b"one").unwrap().nodeid;
        let ino = r.ino("w/one");
        std::fs::remove_file(r.path("w/one")).unwrap();
        let got =
            r.fs.records(&[r.change("w/one", fse::RENAMED | fse::REMOVED, ino)]);
        assert_eq!(
            (got[0].op, got[0].child, got[0].flags),
            (op::DELETE, one, 0)
        );
        std::fs::write(r.path("w/brief"), b"").unwrap();
        let brief = r.c.lookup(w, b"brief").unwrap().nodeid;
        let ino = r.ino("w/brief");
        std::fs::rename(r.path("w/brief"), r.path("w/brief2")).unwrap();
        std::fs::remove_file(r.path("w/brief2")).unwrap();
        let got = r.fs.records(&[r.change("w/brief", fse::RENAMED, ino)]);
        assert_eq!(
            (got[0].op, got[0].child, got[0].flags),
            (op::DELETE, brief, flag::LAST_LINK)
        );
    }

    #[test]
    fn the_guest_s_own_changes_and_lost_events() {
        let r = Rig::new("echo");
        let w = r.w();
        r.fs.set_feeding(true);
        let (e, fh) = r.c.create(w, b"mine", 0o644, 1).unwrap();
        r.c.write(e.nodeid, fh, 0, b"guest data").unwrap();
        let mine = r.change("w/mine", fse::CREATED | fse::MODIFIED, r.ino("w/mine"));
        assert_eq!(r.fs.records(std::slice::from_ref(&mine)), []);
        r.c.chmod(e.nodeid, 0o600).unwrap();
        assert_eq!(
            r.fs.records(&[r.change("w/mine", fse::INODE_META, r.ino("w/mine"))]),
            []
        );
        r.c.release(e.nodeid, fh).unwrap();
        mac(&r.dir, "echo more >> w/mine");
        let got = r.fs.records(std::slice::from_ref(&mine));
        assert_eq!(
            (got.len(), got[0].op, got[0].mask),
            (1, op::CHANGE, mask::CONTENT)
        );

        for lost in [
            fse::MUST_SCAN_SUBDIRS,
            fse::USER_DROPPED,
            fse::KERNEL_DROPPED,
            fse::IDS_WRAPPED,
            fse::ROOT_CHANGED,
        ] {
            mac(&r.dir, "echo again >> w/mine");
            let got = r.fs.records(&[
                r.change("w", lost, 0),
                r.change("w", lost | fse::MUST_SCAN_SUBDIRS, 0),
                mine.clone(),
            ]);
            assert_eq!(got.len(), 2, "{lost:#x}: {got:?}");
            assert_eq!(
                (got[0].op, got[0].name.as_slice()),
                (op::CHANGE, &b"mine"[..])
            );
            assert_eq!(
                got[1],
                Event {
                    op: op::INVAL_ALL,
                    ..Event::default()
                }
            );
            got[1].validate().unwrap();
        }
        assert_eq!(
            r.fs.records(&[
                Change {
                    path: b"/etc/hosts".to_vec(),
                    flags: fse::MODIFIED,
                    ino: 1
                },
                r.change("w", fse::HISTORY_DONE, 0),
                Change {
                    path: b"relative".to_vec(),
                    flags: fse::MODIFIED,
                    ino: 1
                },
                Change {
                    path: b"/".to_vec(),
                    flags: fse::MODIFIED,
                    ino: 2
                },
            ]),
            []
        );
    }

    #[test]
    fn scheme_two_nodes_follow_a_mac_rename_through_the_feed() {
        let r = Rig::with("tree", |cfg| cfg.no_volfs = true);
        std::fs::create_dir_all(r.path("w/dir")).unwrap();
        std::fs::write(r.path("w/dir/f"), b"content").unwrap();
        let w = r.w();
        let dir = r.c.lookup(w, b"dir").unwrap().nodeid;
        let f = r.c.lookup(dir, b"f").unwrap().nodeid;
        let ino = r.ino("w/dir");
        std::fs::rename(r.path("w/dir"), r.path("w/renamed")).unwrap();
        assert_eq!(r.c.getattr(f).err(), Some(crate::Errno::ENOENT));
        let got = r.fs.records(&[
            r.change("w/dir", fse::RENAMED | fse::IS_DIR, ino),
            r.change("w/renamed", fse::RENAMED | fse::IS_DIR, ino),
        ]);
        assert_eq!(
            (got[0].op, got[0].child, got[0].flags),
            (op::RENAME, dir, flag::ISDIR)
        );
        assert_eq!(r.c.getattr(f).unwrap().size, 7);
        assert_eq!(r.c.lookup(w, b"renamed").unwrap().nodeid, dir);
        std::fs::remove_file(r.path("w/renamed/f")).unwrap();
        let got = r.fs.records(&[r.change("w/renamed/f", fse::REMOVED, 0)]);
        assert_eq!(
            (got[0].op, got[0].parent, got[0].child),
            (op::DELETE, dir, f)
        );
        assert_eq!(got[0].flags, flag::LAST_LINK);
    }

    #[test]
    fn the_connection_opens_with_a_hello_and_takes_one_helper() {
        let r = Rig::new("hello");
        let feed = r.feed();
        let mut h = Helper::connect(&feed).unwrap();
        assert_eq!(
            h.hello("another-build"),
            Some(Frame::Hello {
                version: fsevents::VERSION,
                build: "test-build".into()
            })
        );
        assert_eq!(h.recv(), None, "closed after the answer");
        let wait = |feed: &Arc<Feed>| {
            let start = Instant::now();
            while feed.connected() && start.elapsed() < Duration::from_secs(5) {
                std::thread::yield_now();
            }
        };
        wait(&feed);
        for build in ["test-build", ""] {
            let mut h = Helper::connect(&feed).unwrap();
            assert!(h.hello(build).is_some());
            assert!(Helper::connect(&feed).is_none());
            assert_eq!(h.watch(&[]), []);
            drop(h);
            wait(&feed);
        }
        let mut h = Helper::connect(&feed).unwrap();
        h.send(&Frame::Watch {
            seq: 1,
            paths: vec![],
        });
        assert!(matches!(h.recv(), Some(Frame::Hello { .. })));
        assert_eq!(h.recv(), None);
        wait(&feed);
        for bad in [
            Frame::Hello {
                version: fsevents::VERSION,
                build: String::new(),
            },
            Frame::Watch {
                seq: 2,
                paths: vec![],
            },
            Frame::Watching {
                seq: 1,
                status: vec![],
            },
        ] {
            let mut h = Helper::connect(&feed).unwrap();
            assert!(h.hello("").is_some());
            h.send(&bad);
            assert_eq!(h.recv(), None, "{bad:?}");
            wait(&feed);
        }
        let mut h = Helper::connect(&feed).unwrap();
        assert!(h.hello("").is_some());
        h.s.write_all(&[0xff; 8]).unwrap();
        assert_eq!(h.recv(), None);
        wait(&feed);
        assert!(!feed.connected());
    }

    #[test]
    fn a_watch_set_is_answered_path_by_path() {
        let r = Rig::with("status", |cfg| {
            std::fs::create_dir_all(cfg.root.join("data")).unwrap();
            cfg.deny = vec![cfg.root.join("data")];
        });
        std::fs::create_dir_all(r.path("data/inner")).unwrap();
        std::os::unix::fs::symlink("w", r.path("link")).unwrap();
        std::fs::write(r.path("w/file"), b"").unwrap();
        let feed = r.feed();
        let mut h = Helper::connect(&feed).unwrap();
        h.hello("test-build").unwrap();
        let got = h.watch(&[
            &r.path("w"),
            &r.path("absent"),
            Path::new("/usr/share"),
            &r.path("data"),
            &r.path("data/inner"),
            &r.path("link"),
            &r.path("w/file"),
            &r.dir,
            Path::new("/"),
            Path::new("/.vol"),
            Path::new("/no-such-directory-on-this-mac/x"),
        ]);
        use Status::*;
        assert_eq!(
            got,
            [Ok, Missing, Outside, Outside, Outside, Ok, Ok, Ok, Outside, Outside, Outside]
        );
        assert_eq!(h.watch(&[&r.path("w")]), [Ok]);
        assert_eq!(h.watch(&[]), []);
    }

    #[test]
    fn mac_changes_reach_the_helper_and_the_guest_s_own_do_not() {
        let r = Rig::new("stream");
        std::fs::write(r.path("w/file"), b"one").unwrap();
        let w = r.w();
        let file = r.c.lookup(w, b"file").unwrap().nodeid;
        let feed = r.feed();
        let mut h = Helper::connect(&feed).unwrap();
        h.hello("test-build").unwrap();
        assert_eq!(h.watch(&[&r.path("w")]), [Status::Ok]);

        let fh = r.c.open(file, 1).unwrap();
        r.c.write(file, fh, 0, b"guest").unwrap();
        r.c.release(file, fh).unwrap();
        let (made, mfh) = r.c.create(w, b"guest-made", 0o644, 1).unwrap();
        r.c.release(made.nodeid, mfh).unwrap();
        mac(&r.dir, "echo mac >> w/file");
        let got = h.until(|e| e.op == op::CHANGE && e.name == b"file");
        for e in &got {
            if e.op == op::CREATE && e.name == b"w" {
                eprintln!("a CREATE for the watched directory itself, replayed: {e:?}");
                continue;
            }
            assert!(
                e.name == b"file",
                "an event for the guest's own change: {e:?} in {got:?}"
            );
        }
        assert_eq!(
            got.iter().filter(|e| e.name == b"file").count(),
            1,
            "{got:?}"
        );
        let e = named(&got, op::CHANGE, b"file").unwrap();
        assert_eq!((e.parent, e.child, e.mask), (w, file, mask::CONTENT));

        mac(&r.dir, "echo x > w/new");
        let got = h.until(|e| e.op == op::CHANGE && e.name == b"new");
        assert!(named(&got, op::CREATE, b"new").is_some(), "{got:?}");
        mac(&r.dir, "echo saved > w/.file.tmp && mv w/.file.tmp w/file");
        let got = h.until(|e| e.op == op::RENAME && e.name2 == b"file");
        let e = named(&got, op::RENAME, b"file").unwrap();
        assert_eq!((e.victim, e.flags), (file, flag::VICTIM_GONE), "{got:?}");
        let new = r.c.lookup(w, b"new").unwrap().nodeid;
        mac(&r.dir, "mv w/new w/renamed");
        let got = h.until(|e| e.op == op::RENAME && e.name2 == b"renamed");
        assert_eq!(named(&got, op::RENAME, b"renamed").unwrap().child, new);
        mac(&r.dir, "rm w/renamed");
        let got = h.until(|e| e.op == op::DELETE && e.name == b"renamed");
        let e = named(&got, op::DELETE, b"renamed").unwrap();
        assert_eq!((e.child, e.flags), (new, flag::LAST_LINK));

        drop(h);
        let start = Instant::now();
        while feed.connected() && start.elapsed() < Duration::from_secs(5) {
            std::thread::yield_now();
        }
        assert!(!feed.connected());
    }

    #[test]
    fn a_change_made_as_soon_as_a_set_is_acknowledged_is_reported() {
        let r = Rig::new("ack");
        std::fs::create_dir(r.path("w2")).unwrap();
        let w = r.w();
        r.c.lookup(ROOT_ID, b"w2").unwrap();
        let _ = w;
        let feed = r.feed();
        let mut h = Helper::connect(&feed).unwrap();
        h.hello("").unwrap();
        let rounds: usize = std::env::var("FEV_ACK_ROUNDS")
            .ok()
            .and_then(|v| v.parse().ok())
            .unwrap_or(40);
        for i in 0..rounds {
            let (w1, w2) = (r.path("w"), r.path("w2"));
            let set: Vec<&Path> = if i % 2 == 0 {
                vec![&w1]
            } else {
                vec![&w1, &w2]
            };
            assert!(h.watch(&set).iter().all(|s| *s == Status::Ok));
            let name = format!("f{i}");
            let ok = Command::new("/usr/bin/touch")
                .arg(r.path("w").join(&name))
                .status()
                .unwrap()
                .success();
            assert!(ok);
            let got = h.until(|e| e.op == op::CREATE && e.name == name.as_bytes());
            assert!(!got.is_empty(), "round {i}");
        }
    }

    #[test]
    fn a_new_watch_set_reports_nothing_from_before_it() {
        let r = Rig::new("history");
        std::fs::create_dir(r.path("w2")).unwrap();
        r.w();
        r.c.lookup(ROOT_ID, b"w2").unwrap();
        let feed = r.feed();
        let mut h = Helper::connect(&feed).unwrap();
        h.hello("").unwrap();
        assert_eq!(h.watch(&[&r.path("w")]), [Status::Ok]);
        mac(
            &r.dir,
            "for i in 1 2 3 4 5; do echo old > w2/before$i; done; sleep 1",
        );
        assert_eq!(h.watch(&[&r.path("w2")]), [Status::Ok]);
        mac(&r.dir, "sleep 0.3; echo new > w2/after");
        let got = h.until(|e| e.name == b"after");
        let stale: Vec<_> = got
            .iter()
            .filter(|e| e.name.starts_with(b"before"))
            .collect();
        assert!(stale.is_empty(), "history was replayed: {stale:?}");
    }

    #[test]
    fn a_change_made_as_the_set_changes_is_reported_once() {
        let r = Rig::new("handover");
        std::fs::create_dir(r.path("w2")).unwrap();
        r.w();
        r.c.lookup(ROOT_ID, b"w2").unwrap();
        let feed = r.feed();
        let mut h = Helper::connect(&feed).unwrap();
        h.hello("").unwrap();
        let (w1, w2) = (r.path("w"), r.path("w2"));
        assert_eq!(h.watch(&[&w1]), [Status::Ok]);
        let rounds = 30;
        let mut seen: Vec<Event> = Vec::new();
        for i in 0..rounds {
            let ok = Command::new("/usr/bin/touch")
                .arg(w1.join(format!("g{i}")))
                .status()
                .unwrap()
                .success();
            assert!(ok);
            let set: Vec<&Path> = if i % 2 == 0 {
                vec![&w1, &w2]
            } else {
                vec![&w1]
            };
            h.seq += 1;
            h.send(&Frame::Watch {
                seq: h.seq,
                paths: set.iter().map(|p| p.to_str().unwrap().into()).collect(),
            });
            loop {
                match h.recv().expect("an acknowledgement") {
                    Frame::Watching { status, .. } => {
                        assert!(status.iter().all(|s| *s == Status::Ok));
                        break;
                    }
                    Frame::Events(records) => seen.extend(records),
                    other => panic!("{other:?}"),
                }
            }
        }
        mac(&r.dir, "sleep 0.3; echo done > w/last");
        seen.extend(h.until(|e| e.name == b"last"));
        assert!(!seen.iter().any(|e| e.op == op::INVAL_ALL), "{seen:?}");
        for i in 0..rounds {
            let name = format!("g{i}").into_bytes();
            let n = seen
                .iter()
                .filter(|e| e.op == op::CREATE && e.name == name)
                .count();
            assert_eq!(n, 1, "g{i} was announced {n} times");
        }
    }

    #[test]
    fn sets_that_grow_shrink_and_overlap_report_each_change_once() {
        let r = Rig::new("sets");
        for d in ["w2", "w/in"] {
            std::fs::create_dir(r.path(d)).unwrap();
        }
        let w = r.w();
        r.c.lookup(ROOT_ID, b"w2").unwrap();
        r.c.lookup(w, b"in").unwrap();
        let feed = r.feed_lingering(Duration::ZERO);
        let mut h = Helper::connect(&feed).unwrap();
        h.hello("").unwrap();
        let (top, w1, w2, inner) = (r.dir.clone(), r.path("w"), r.path("w2"), r.path("w/in"));
        let creates = |got: &[Event], name: &[u8]| {
            got.iter()
                .filter(|e| e.op == op::CREATE && e.name == name)
                .count()
        };
        assert_eq!(h.watch(&[&inner]), [Status::Ok]);
        assert_eq!(h.watch(&[&inner, &w1]), [Status::Ok, Status::Ok]);
        assert_eq!(h.watch(&[&inner, &w1, &top]), [Status::Ok; 3]);
        mac(&r.dir, "echo x > w/in/nested; sleep 0.2; echo x > w/mark1");
        let got = h.until(|e| e.name == b"mark1");
        assert_eq!(creates(&got, b"nested"), 1, "{got:?}");
        assert_eq!(h.watch(&[&w1, &w2]), [Status::Ok; 2]);
        assert_eq!(h.watch(&[&w1]), [Status::Ok]);
        mac(&r.dir, "echo x > w2/unwatched; sleep 0.2; echo x > w/mark2");
        let got = h.until(|e| e.name == b"mark2");
        assert_eq!(creates(&got, b"unwatched"), 0, "{got:?}");
        assert_eq!(h.watch(&[&w1, &w2]), [Status::Ok; 2]);
        mac(&r.dir, "echo x > w2/back; sleep 0.2; echo x > w/mark3");
        let got = h.until(|e| e.name == b"mark3");
        assert_eq!(creates(&got, b"back"), 1, "{got:?}");
        assert_eq!(creates(&got, b"unwatched"), 0, "{got:?}");
        let mut dirs = Vec::new();
        for i in 0..MAX_STREAMS + 3 {
            let d = r.path(&format!("d{i}"));
            std::fs::create_dir(&d).unwrap();
            r.c.lookup(ROOT_ID, format!("d{i}").as_bytes()).unwrap();
            dirs.push(d);
            let set: Vec<&Path> = dirs.iter().map(|d| d.as_path()).collect();
            assert!(h.watch(&set).iter().all(|s| *s == Status::Ok));
        }
        mac(
            &r.dir,
            "echo x > d0/first; echo x > d7/middle; sleep 0.2; echo x > d18/mark4",
        );
        let got = h.until(|e| e.name == b"mark4");
        assert_eq!(
            (creates(&got, b"first"), creates(&got, b"middle")),
            (1, 1),
            "{got:?}"
        );
    }

    #[test]
    fn a_new_file_is_announced_whatever_the_stream_s_flags_say() {
        let r = Rig::new("flags");
        let w = r.w();
        std::fs::write(r.path("w/fresh"), b"x").unwrap();
        let got =
            r.fs.records(&[r.change("w/fresh", fse::INODE_META, r.ino("w/fresh"))]);
        assert_eq!(got.len(), 2, "{got:?}");
        assert_eq!((got[0].op, got[0].parent), (op::CREATE, w));
        assert_eq!((got[1].op, got[1].mask), (op::CHANGE, mask::CONTENT));
        assert_eq!(
            r.fs.records(&[r.change("w/fresh", fse::INODE_META, r.ino("w/fresh"))]),
            []
        );
        std::fs::write(r.path("w/old"), b"x").unwrap();
        mac(&r.dir, "touch -t 200001010000 w/old && chmod 600 w/old");
        let got =
            r.fs.records(&[r.change("w/old", fse::INODE_META, r.ino("w/old"))]);
        assert_eq!(got.len(), 1, "{got:?}");
        assert_eq!((got[0].op, got[0].mask), (op::CHANGE, mask::ATTRIB));
    }

    #[test]
    fn a_burst_straight_after_a_set_changes_drops_nothing() {
        let qos = std::env::var("FEV_FSEVENTS_QOS")
            .ok()
            .and_then(|v| u32::from_str_radix(v.trim_start_matches("0x"), 16).ok());
        if let Some(qos) = qos {
            watcher::set_queue_qos(qos);
        }
        let drops_allowed = qos == Some(0);
        let r = Rig::new("burst");
        std::fs::create_dir(r.path("w2")).unwrap();
        r.w();
        r.c.lookup(ROOT_ID, b"w2").unwrap();
        let lost = Arc::new(Mutex::new(Vec::<String>::new()));
        let log = lost.clone();
        let feed = Feed::new(
            vec![r.fs.clone()],
            "test-build",
            Box::new(move |line| log.lock().unwrap().push(line.into())),
        );
        let mut h = Helper::connect(&feed).unwrap();
        h.hello("").unwrap();
        assert_eq!(h.watch(&[&r.path("w")]), [Status::Ok]);
        assert_eq!(h.watch(&[&r.path("w"), &r.path("w2")]), [Status::Ok; 2]);
        let maker = {
            let dir = r.dir.clone();
            std::thread::spawn(move || {
                mac(
                    &dir,
                    "cd w2 && i=0 && while [ $i -lt 4000 ]; do echo x > f$i; i=$((i+1)); done; \
                     sleep 0.3; echo done > ../w/last",
                )
            })
        };
        let got = h.until(|e| e.name == b"last");
        maker.join().unwrap();
        if drops_allowed {
            for line in lost.lock().unwrap().iter() {
                eprintln!("{line}");
            }
        } else {
            assert!(
                lost.lock().unwrap().is_empty(),
                "{:?}",
                lost.lock().unwrap()
            );
            assert!(!got.iter().any(|e| e.op == op::INVAL_ALL));
        }
        let mut names: Vec<&[u8]> = got
            .iter()
            .filter(|e| {
                e.op == op::CREATE && e.flags & flag::QUIET == 0 && e.name.starts_with(b"f")
            })
            .map(|e| e.name.as_slice())
            .collect();
        let made = names.len();
        names.sort_unstable();
        names.dedup();
        assert_eq!(
            (made, names.len()),
            (4000, 4000),
            "{made} CREATEs for {} files",
            names.len()
        );
    }

    #[test]
    fn a_file_rewritten_faster_than_the_guest_reads_is_queued_once() {
        let r = Rig::new("repeat");
        for i in 0..20 {
            std::fs::write(r.path(&format!("w/f{i}")), b"0").unwrap();
        }
        let w = r.w();
        r.c.list(w, true).unwrap();
        let feed = r.feed();
        let mut h = Helper::connect(&feed).unwrap();
        h.hello("").unwrap();
        assert_eq!(h.watch(&[&r.path("w")]), [Status::Ok]);
        mac(
            &r.dir,
            "cd w && n=0 && while [ $n -lt 300 ]; do for i in 0 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19; do echo $n > f$i; done; n=$((n+1)); done",
        );
        mac(&r.dir, "sleep 0.3; echo done > w/last");
        let got = h.until(|e| e.name == b"last");
        assert!(
            !got.iter().any(|e| e.op == op::INVAL_ALL),
            "the queue overflowed on repeats"
        );
        for i in 0..20 {
            let name = format!("f{i}").into_bytes();
            let n = got
                .iter()
                .filter(|e| e.op == op::CHANGE && e.name == name)
                .count();
            assert!(n >= 1, "f{i} has no change record");
        }
        let bytes: usize = got.iter().map(Event::encoded_len).sum();
        assert!(bytes < 200 << 10, "{bytes} bytes of records for 20 files");
        assert_eq!(std::fs::read(r.path("w/f7")).unwrap(), b"299\n");
    }

    #[test]
    fn a_guest_that_does_not_read_costs_a_bounded_queue_and_one_inval_all() {
        let r = Rig::new("overflow");
        r.w();
        let feed = r.feed();
        let mut h = Helper::connect(&feed).unwrap();
        h.hello("").unwrap();
        assert_eq!(h.watch(&[&r.path("w")]), [Status::Ok]);
        mac(
            &r.dir,
            "cd w && i=0 && while [ $i -lt 4000 ]; do echo x > long-file-name-number-$i; i=$((i+1)); done",
        );
        mac(&r.dir, "sleep 0.5; echo done > w/last");
        let got = h.until(|e| e.name == b"last");
        let bytes: usize = got.iter().map(Event::encoded_len).sum();
        assert!(
            got.iter().any(|e| e.op == op::INVAL_ALL),
            "no INVAL_ALL in {} records",
            got.len()
        );
        assert!(
            bytes <= fsevents::QUEUE_MAX + (128 << 10),
            "{bytes} bytes of records were held for a guest that was not reading"
        );
        assert_eq!(h.watch(&[&r.path("w")]), [Status::Ok]);
        mac(&r.dir, "echo x > w/after");
        h.until(|e| e.name == b"after");
    }

    #[test]
    fn nothing_is_cached_for_long_while_the_guest_is_behind() {
        let r = Rig::new("behind");
        let w = r.w();
        let feed = r.feed();
        let mut h = Helper::connect(&feed).unwrap();
        h.hello("").unwrap();
        assert_eq!(h.watch(&[&r.path("w")]), [Status::Ok]);
        assert_eq!(r.fs.watch_roots(), 1);
        std::fs::write(r.path("w/known"), b"x").unwrap();
        assert_eq!(r.c.lookup(w, b"known").unwrap().attr_valid, LONG);
        mac(
            &r.dir,
            "cd w && i=0 && while [ $i -lt 4000 ]; do echo x > long-file-name-number-$i; i=$((i+1)); done",
        );
        let start = Instant::now();
        while r.fs.watch_roots() != 0 {
            assert!(
                start.elapsed() < Duration::from_secs(10),
                "the queue never overflowed"
            );
            std::thread::sleep(Duration::from_millis(10));
        }
        assert_eq!(r.c.lookup(w, b"known").unwrap().attr_valid, VALID);
        mac(&r.dir, "sleep 0.3; echo done > w/last");
        let got = h.until(|e| e.name == b"last");
        assert!(got.iter().any(|e| e.op == op::INVAL_ALL));
        let start = Instant::now();
        while r.fs.watch_roots() != 1 {
            assert!(
                start.elapsed() < Duration::from_secs(5),
                "the watch never counted again"
            );
            std::thread::sleep(Duration::from_millis(10));
        }
        assert_eq!(r.c.lookup(w, b"known").unwrap().attr_valid, LONG);
    }

    #[test]
    fn the_shares_are_told_what_is_watched_and_a_path_that_leaves_lingers() {
        let r = Rig::new("linger");
        std::fs::create_dir(r.path("w2")).unwrap();
        let w = r.w();
        let w2 = r.c.lookup(ROOT_ID, b"w2").unwrap().nodeid;
        std::fs::write(r.path("w2/file"), b"x").unwrap();
        let file = r.c.lookup(w2, b"file").unwrap();
        assert_eq!(file.attr_valid, VALID, "no feed, nothing watched");
        let feed = r.feed_lingering(Duration::from_millis(1500));
        let mut h = Helper::connect(&feed).unwrap();
        h.hello("").unwrap();
        assert_eq!(h.watch(&[&r.path("w"), &r.path("w2")]), [Status::Ok; 2]);
        assert_eq!(r.fs.watch_roots(), 2);
        assert_eq!(r.c.getattr_valid(file.nodeid, None).unwrap().1, LONG);
        let left = Instant::now();
        assert_eq!(h.watch(&[&r.path("w")]), [Status::Ok]);
        assert_eq!(r.fs.watch_roots(), 1);
        assert_eq!(r.c.getattr_valid(file.nodeid, None).unwrap().1, VALID);
        assert_eq!(r.c.lookup(w2, b"nothing"), Err(crate::Errno::ENOENT));
        mac(
            &r.dir,
            "echo changed >> w2/file; sleep 0.2; echo x > w/mark1",
        );
        let got = h.until(|e| e.name == b"mark1");
        assert!(
            got.iter()
                .any(|e| e.op == op::CHANGE && e.child == file.nodeid),
            "{got:?}"
        );
        assert!(
            left.elapsed() < Duration::from_millis(1400),
            "too slow a Mac for this test"
        );
        assert_eq!(h.watch(&[&r.path("w"), &r.path("w2")]), [Status::Ok; 2]);
        assert_eq!(r.c.getattr_valid(file.nodeid, None).unwrap().1, LONG);
        assert_eq!(h.watch(&[&r.path("w")]), [Status::Ok]);
        std::thread::sleep(Duration::from_millis(1800));
        mac(&r.dir, "echo again >> w2/file; sleep 0.3; echo x > w/mark2");
        let got = h.until(|e| e.name == b"mark2");
        assert!(!got.iter().any(|e| e.child == file.nodeid), "{got:?}");
        let _ = w;
        drop(h);
        let start = Instant::now();
        while feed.connected() {
            assert!(start.elapsed() < Duration::from_secs(5));
            std::thread::sleep(Duration::from_millis(5));
        }
        assert_eq!(r.fs.watch_roots(), 0);
    }

    #[test]
    fn a_resume_has_the_guest_drop_everything() {
        let r = Rig::new("resume");
        r.w();
        let feed = r.feed();
        feed.resumed();
        let mut h = Helper::connect(&feed).unwrap();
        h.hello("").unwrap();
        assert_eq!(h.watch(&[&r.path("w")]), [Status::Ok]);
        feed.resumed();
        let got = h.until(|e| e.op == op::INVAL_ALL);
        for e in &got {
            assert!(
                e.op == op::INVAL_ALL || (e.op == op::CREATE && e.name == b"w"),
                "{got:?}"
            );
        }
        feed.resumed();
        assert!(matches!(h.recv(), Some(Frame::Events(r)) if r[0].op == op::INVAL_ALL));
        assert_eq!(r.fs.watch_roots(), 1);
    }

    #[test]
    fn a_batch_stuck_for_a_second_has_the_guest_drop_everything_while_it_is_stuck() {
        let r = Rig::new("watchdog");
        std::fs::write(r.path("w/file"), b"one").unwrap();
        let w = r.w();
        let feed = r.feed();
        let mut h = Helper::connect(&feed).unwrap();
        h.hello("test-build").unwrap();
        assert_eq!(h.watch(&[&r.path("w")]), [Status::Ok]);
        let file = r.c.lookup(w, b"file").unwrap();
        assert_eq!(file.attr_valid, LONG);
        let held = crate::passthrough::STUCK + Duration::from_secs(2);
        *r.fs.batch_delay.lock().unwrap() = Some(held);
        let changed = Instant::now();
        mac(&r.dir, "echo two >w/file");
        std::thread::sleep(Duration::from_millis(300));
        mac(&r.dir, "echo new >w/beside");
        let got = h.until(|e| e.op == op::INVAL_ALL);
        let after = changed.elapsed();
        assert!(
            after < crate::passthrough::STUCK + Duration::from_millis(900),
            "the INVAL_ALL came {after:?} after the change, the batch being held {held:?}: {got:?}"
        );
        for e in &got {
            assert!(
                e.op == op::INVAL_ALL || (e.op == op::CREATE && e.name == b"w"),
                "{got:?}"
            );
        }
        assert_eq!(r.c.getattr_valid(file.nodeid, None).unwrap().1, VALID);
        let got = h.until(|e| named(std::slice::from_ref(e), op::CREATE, b"beside").is_some());
        assert!(changed.elapsed() >= held, "{got:?}");
        mac(&r.dir, "echo three >w/file");
        h.s.set_read_timeout(Some(crate::passthrough::STUCK + Duration::from_millis(500)))
            .unwrap();
        let mut later = Vec::new();
        while let Some(Frame::Events(records)) = h.recv() {
            later.extend(records);
        }
        assert!(
            later.iter().any(|e| e.op == op::CHANGE) && later.iter().all(|e| e.op != op::INVAL_ALL),
            "{later:?}"
        );
        drop(h);
    }

    #[test]
    fn a_name_the_guest_believes_absent_is_taken_back_when_something_appears() {
        let r = Rig::new("appears");
        let w = r.w();
        r.fs.set_watch(&[r.path("w").into_os_string().into_encoded_bytes()]);
        let quiet = |records: &[Event], name: &[u8]| {
            records
                .iter()
                .position(|e| e.op == op::CREATE && e.flags & flag::QUIET != 0 && e.name == name)
        };
        for name in [&b"Makefile"[..], b"makefile", b"MAKEFILE", b"other"] {
            assert_eq!(r.c.lookup(w, name).unwrap().nodeid, 0, "a negative entry");
        }
        std::fs::write(r.path("w/makefile"), b"all:\n").unwrap();
        let got =
            r.fs.records(&[r.change("w/makefile", fse::CREATED, r.ino("w/makefile"))]);
        for name in [&b"Makefile"[..], b"makefile", b"MAKEFILE"] {
            assert!(quiet(&got, name).is_some(), "{name:?} in {got:?}");
        }
        assert!(quiet(&got, b"other").is_none(), "{got:?}");
        let create = got
            .iter()
            .position(|e| e.op == op::CREATE && e.flags & flag::QUIET == 0)
            .expect("the create itself");
        assert!(quiet(&got, b"makefile").unwrap() < create);
        let again =
            r.fs.records(&[r.change("w/makefile", fse::MODIFIED, r.ino("w/makefile"))]);
        assert!(quiet(&again, b"makefile").is_none(), "{again:?}");

        std::fs::write(r.path("w/seen"), b"x").unwrap();
        r.fs.records(&[r.change("w/seen", fse::CREATED, r.ino("w/seen"))]);
        std::fs::rename(r.path("w/seen"), r.path("elsewhere")).unwrap();
        let ino = r.ino("elsewhere");
        r.fs.records(&[r.change("w/seen", fse::RENAMED, ino)]);
        assert_eq!(r.c.lookup(w, b"back").unwrap().nodeid, 0);
        std::fs::rename(r.path("elsewhere"), r.path("w/back")).unwrap();
        let got = r.fs.records(&[r.change("w/back", fse::INODE_META, ino)]);
        assert!(quiet(&got, b"back").is_some(), "{got:?}");

        for name in ["caf\u{e9}", "cafe\u{301}", "plain"] {
            assert_eq!(r.c.lookup(w, name.as_bytes()).unwrap().nodeid, 0);
        }
        std::fs::write(r.path("w/caf\u{e9}"), b"x").unwrap();
        let got =
            r.fs.records(&[r.change("w/caf\u{e9}", fse::CREATED, r.ino("w/caf\u{e9}"))]);
        for name in ["caf\u{e9}", "cafe\u{301}", "plain"] {
            assert!(quiet(&got, name.as_bytes()).is_some(), "{name} in {got:?}");
        }
        assert_eq!(r.fs.records(&[]).len(), 0);

        for i in 0..300 {
            let name = format!("K\u{212a}-{i}");
            assert_eq!(r.c.lookup(w, name.as_bytes()).unwrap().nodeid, 0);
        }
        std::fs::write(r.path("w/k-new"), b"x").unwrap();
        let got =
            r.fs.records(&[r.change("w/k-new", fse::CREATED, r.ino("w/k-new"))]);
        assert_eq!(got[0].op, op::INVAL_ALL, "{:?}", &got[..2.min(got.len())]);
        let got =
            r.fs.records(&[r.change("w/k-new", fse::MODIFIED, r.ino("w/k-new"))]);
        assert!(got.iter().all(|e| e.flags & flag::QUIET == 0), "{got:?}");
    }

    #[test]
    fn a_directory_moved_out_of_reach_has_the_guest_drop_everything() {
        let r = Rig::new("movedaway");
        let w = r.w();
        for d in ["w/kept", "w/trashed", "w/removed"] {
            std::fs::create_dir(r.path(d)).unwrap();
        }
        std::fs::write(r.path("w/trashed/file"), b"x").unwrap();
        r.fs.set_watch(&[r.path("w").into_os_string().into_encoded_bytes()]);
        let trashed = r.c.lookup(w, b"trashed").unwrap();
        let file = r.c.lookup(trashed.nodeid, b"file").unwrap();
        let removed = r.c.lookup(w, b"removed").unwrap();
        assert_eq!(file.attr_valid, LONG);
        std::fs::remove_dir(r.path("w/removed")).unwrap();
        let got =
            r.fs.records(&[r.change("w/removed", fse::REMOVED | fse::IS_DIR, removed.attr.ino)]);
        assert!(named(&got, op::DELETE, b"removed").is_some(), "{got:?}");
        assert!(got.iter().all(|e| e.op != op::INVAL_ALL), "{got:?}");
        std::fs::rename(r.path("w/trashed"), r.path("trash")).unwrap();
        let got =
            r.fs.records(&[r.change("w/trashed", fse::RENAMED | fse::IS_DIR, trashed.attr.ino)]);
        assert!(named(&got, op::RENAME, b"trash").is_some(), "{got:?}");
        assert!(got.iter().any(|e| e.op == op::INVAL_ALL), "{got:?}");
        assert_eq!(r.c.getattr_valid(file.nodeid, None).unwrap().1, VALID);
        assert_eq!(r.c.getattr_valid(trashed.nodeid, None).unwrap().1, VALID);
        let kept = r.c.lookup(w, b"kept").unwrap();
        std::fs::create_dir(r.path("w/into")).unwrap();
        let into = r.c.lookup(w, b"into").unwrap();
        std::fs::rename(r.path("w/kept"), r.path("w/into/kept")).unwrap();
        let got =
            r.fs.records(&[r.change("w/kept", fse::RENAMED | fse::IS_DIR, kept.attr.ino)]);
        assert!(named(&got, op::RENAME, b"kept").is_some(), "{got:?}");
        assert!(got.iter().all(|e| e.op != op::INVAL_ALL), "{got:?}");
        assert_eq!(r.c.getattr_valid(kept.nodeid, None).unwrap().1, LONG);
        let outside = std::env::temp_dir().join(format!("fev-feed-outside-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&outside);
        std::fs::rename(r.path("w/into"), &outside).unwrap();
        let got =
            r.fs.records(&[r.change("w/into", fse::RENAMED | fse::IS_DIR, into.attr.ino)]);
        let valid = r.c.getattr_valid(kept.nodeid, None);
        std::fs::remove_dir_all(&outside).unwrap();
        assert!(named(&got, op::DELETE, b"into").is_some(), "{got:?}");
        assert_eq!(got.last().unwrap().op, op::INVAL_ALL, "{got:?}");
        assert_eq!(valid.unwrap().1, VALID);
    }
}
