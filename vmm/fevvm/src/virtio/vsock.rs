use std::collections::{HashMap, HashSet, VecDeque};
use std::io::{self, Write};
use std::num::Wrapping;
use std::os::fd::{AsRawFd, RawFd};
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;

use super::queue::{Chain, Queue, QueueError};
use super::{read_config_bytes, Activation, Interrupt, VirtioDevice};
use crate::memory::GuestMemory;

const DEVICE_ID: u32 = 19;
const QUEUE_SIZES: [u16; 3] = [128, 128, 16];
const RX: u32 = 0;
const TX: u32 = 1;

pub const HOST_CID: u64 = 2;
pub const GUEST_CID: u64 = 3;

const HEADER_LEN: usize = 44;
const TYPE_STREAM: u16 = 1;

const OP_REQUEST: u16 = 1;
const OP_RESPONSE: u16 = 2;
const OP_RST: u16 = 3;
const OP_SHUTDOWN: u16 = 4;
const OP_RW: u16 = 5;
const OP_CREDIT_UPDATE: u16 = 6;
const OP_CREDIT_REQUEST: u16 = 7;

const SHUTDOWN_RCV: u32 = 1;
const SHUTDOWN_SEND: u32 = 2;

const BUF_ALLOC: u32 = 256 << 10;
const VIEW_GUEST_PORT: u32 = 2049;
const VIEW_BUF_ALLOC: u32 = 2 << 20;
const DEFER_MIN: u64 = 16 << 10;
const KEPT_MAX_CONN: usize = 16;
const KEPT_MAX_CONN_BYTES: usize = 2 * SEND_BUFFER as usize;
const KEPT_MAX: usize = QUEUE_SIZES[1] as usize / 4;

fn buf_alloc_for(key: Key) -> u32 {
    if key.guest_port == VIEW_GUEST_PORT && key.host_port >= FIRST_EPHEMERAL_PORT {
        VIEW_BUF_ALLOC
    } else {
        BUF_ALLOC
    }
}

fn credit_update_threshold(key: Key) -> u32 {
    buf_alloc_for(key) / 4
}
const MAX_CONNECTIONS: usize = 4096;
const MAX_PENDING_CONTROL: usize = 2 * MAX_CONNECTIONS;
const LISTEN_BACKLOG: libc::c_int = 1024;
const RESERVED_DESCRIPTORS: usize = 128;
const FIRST_EPHEMERAL_PORT: u32 = 1 << 30;
const SEND_BUFFER: libc::c_int = 256 << 10;
const MAX_SOCKET_PATH: usize = 103;

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Header {
    pub src_cid: u64,
    pub dst_cid: u64,
    pub src_port: u32,
    pub dst_port: u32,
    pub len: u32,
    pub kind: u16,
    pub op: u16,
    pub flags: u32,
    pub buf_alloc: u32,
    pub fwd_cnt: u32,
}

impl Header {
    pub fn parse(b: &[u8; HEADER_LEN]) -> Self {
        let u64_at = |o: usize| u64::from_le_bytes(b[o..o + 8].try_into().unwrap());
        let u32_at = |o: usize| u32::from_le_bytes(b[o..o + 4].try_into().unwrap());
        let u16_at = |o: usize| u16::from_le_bytes(b[o..o + 2].try_into().unwrap());
        Self {
            src_cid: u64_at(0),
            dst_cid: u64_at(8),
            src_port: u32_at(16),
            dst_port: u32_at(20),
            len: u32_at(24),
            kind: u16_at(28),
            op: u16_at(30),
            flags: u32_at(32),
            buf_alloc: u32_at(36),
            fwd_cnt: u32_at(40),
        }
    }

    pub fn bytes(&self) -> [u8; HEADER_LEN] {
        let mut b = [0u8; HEADER_LEN];
        b[0..8].copy_from_slice(&self.src_cid.to_le_bytes());
        b[8..16].copy_from_slice(&self.dst_cid.to_le_bytes());
        b[16..20].copy_from_slice(&self.src_port.to_le_bytes());
        b[20..24].copy_from_slice(&self.dst_port.to_le_bytes());
        b[24..28].copy_from_slice(&self.len.to_le_bytes());
        b[28..30].copy_from_slice(&self.kind.to_le_bytes());
        b[30..32].copy_from_slice(&self.op.to_le_bytes());
        b[32..36].copy_from_slice(&self.flags.to_le_bytes());
        b[36..40].copy_from_slice(&self.buf_alloc.to_le_bytes());
        b[40..44].copy_from_slice(&self.fwd_cnt.to_le_bytes());
        b
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
struct Key {
    host_port: u32,
    guest_port: u32,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum State {
    Connecting,
    Established,
}

fn set_send_buffer(stream: &UnixStream) {
    let size = SEND_BUFFER;
    unsafe {
        libc::setsockopt(
            stream.as_raw_fd(),
            libc::SOL_SOCKET,
            libc::SO_SNDBUF,
            (&raw const size).cast(),
            std::mem::size_of::<libc::c_int>() as libc::socklen_t,
        );
    }
}

pub type Answer = Box<dyn Fn(UnixStream) -> bool + Send>;

struct Conn {
    stream: UnixStream,
    state: State,
    out: VecDeque<u8>,
    fwd_cnt: Wrapping<u32>,
    fwd_cnt_sent: Wrapping<u32>,
    update_queued: bool,
    tx_cnt: Wrapping<u32>,
    peer_buf_alloc: u32,
    peer_fwd_cnt: Wrapping<u32>,
    guest_done_sending: bool,
    guest_done_receiving: bool,
    host_eof: bool,
    host_closed: bool,
    reading: bool,
    hops: Option<Box<(crate::hops::Marks, crate::hops::Marks)>>,
    pending: VecDeque<(Chain, usize)>,
    pending_bytes: usize,
}

impl Conn {
    fn new(stream: UnixStream, state: State) -> Self {
        Self {
            stream,
            state,
            out: VecDeque::new(),
            fwd_cnt: Wrapping(0),
            fwd_cnt_sent: Wrapping(0),
            update_queued: false,
            tx_cnt: Wrapping(0),
            peer_buf_alloc: 0,
            peer_fwd_cnt: Wrapping(0),
            guest_done_sending: false,
            guest_done_receiving: false,
            host_eof: false,
            host_closed: false,
            reading: false,
            hops: None,
            pending: VecDeque::new(),
            pending_bytes: 0,
        }
    }

    fn peer_credit(&self) -> u32 {
        let in_flight = (self.tx_cnt - self.peer_fwd_cnt).0;
        self.peer_buf_alloc.saturating_sub(in_flight)
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct Control {
    key: Key,
    op: u16,
    flags: u32,
}

struct Active {
    mem: Arc<GuestMemory>,
    irq: Arc<Interrupt>,
    rx: Queue,
    tx: Queue,
}

#[derive(Debug, Clone, Copy)]
enum Source {
    Listener { guest_port: u32 },
    Conn(Key),
}

struct Inner {
    dir: PathBuf,
    kq: RawFd,
    active: Option<Active>,
    listeners: Vec<(UnixListener, u32)>,
    answers: HashMap<u32, Answer>,
    conns: HashMap<Key, Conn>,
    sources: HashMap<RawFd, Source>,
    control: VecDeque<Control>,
    starved: HashSet<Key>,
    next_host_port: u32,
    max_conns: usize,
    stopping: bool,
    hop_t0: i64,
    hop_calls: Vec<(u32, u32)>,
    kept: bool,
    tx_done: bool,
    kept_total: usize,
}

fn kevent_change(kq: RawFd, fd: RawFd, filter: i16, flags: u16) {
    let change = libc::kevent {
        ident: fd as usize,
        filter,
        flags,
        fflags: 0,
        data: 0,
        udata: std::ptr::null_mut(),
    };
    unsafe { libc::kevent(kq, &change, 1, std::ptr::null_mut(), 0, std::ptr::null()) };
}

impl Inner {
    fn set_reading(&mut self, key: Key, on: bool) {
        let Some(conn) = self.conns.get_mut(&key) else {
            return;
        };
        if conn.reading != on {
            conn.reading = on;
            let flags = if on {
                libc::EV_ENABLE
            } else {
                libc::EV_DISABLE
            };
            kevent_change(self.kq, conn.stream.as_raw_fd(), libc::EVFILT_READ, flags);
        }
    }

    fn insert(&mut self, key: Key, conn: Conn) -> bool {
        if self.conns.len() >= self.max_conns || self.conns.contains_key(&key) {
            return false;
        }
        let fd = conn.stream.as_raw_fd();
        kevent_change(
            self.kq,
            fd,
            libc::EVFILT_READ,
            libc::EV_ADD | libc::EV_DISABLE,
        );
        self.sources.insert(fd, Source::Conn(key));
        self.conns.insert(key, conn);
        true
    }

    fn complete_tx(&mut self, head: u16) {
        self.kept_total = self.kept_total.saturating_sub(1);
        let Some(a) = self.active.as_mut() else {
            return;
        };
        if let Err(e) = a.tx.push_used(&a.mem, head, 0) {
            a.irq.fail("virtio-vsock", &e);
            return;
        }
        self.tx_done = true;
    }

    fn remove(&mut self, key: Key) {
        if let Some(conn) = self.conns.remove(&key) {
            self.sources.remove(&conn.stream.as_raw_fd());
            for (chain, _) in conn.pending {
                self.complete_tx(chain.head);
            }
        }
        self.starved.remove(&key);
        self.control.retain(|c| c.key != key || c.op == OP_RST);
    }

    fn queue_control(&mut self, key: Key, op: u16, flags: u32) -> bool {
        if self.control.len() < MAX_PENDING_CONTROL {
            self.control.push_back(Control { key, op, flags });
            return true;
        }
        false
    }

    fn queue_vital(&mut self, key: Key, op: u16, flags: u32) {
        if !self.queue_control(key, op, flags) {
            self.remove(key);
        }
    }

    fn queue_credit_update(&mut self, key: Key) {
        let Some(conn) = self.conns.get(&key) else {
            return;
        };
        if !conn.update_queued && self.queue_control(key, OP_CREDIT_UPDATE, 0) {
            self.conns.get_mut(&key).unwrap().update_queued = true;
        }
    }

    fn credit_arrived(&mut self, key: Key) {
        let Some(conn) = self.conns.get(&key) else {
            return;
        };
        if !conn.reading
            && conn.state == State::Established
            && !conn.host_eof
            && !conn.guest_done_receiving
            && conn.peer_credit() > 0
            && !self.starved.contains(&key)
        {
            self.set_reading(key, true);
        }
    }

    fn reset(&mut self, key: Key) {
        self.remove(key);
        self.queue_control(key, OP_RST, 0);
    }

    fn header_for(&mut self, key: Key, op: u16, flags: u32, len: u32) -> Header {
        let fwd_cnt = match self.conns.get_mut(&key) {
            Some(conn) => {
                conn.fwd_cnt_sent = conn.fwd_cnt;
                conn.fwd_cnt.0
            }
            None => 0,
        };
        Header {
            src_cid: HOST_CID,
            dst_cid: GUEST_CID,
            src_port: key.host_port,
            dst_port: key.guest_port,
            len,
            kind: TYPE_STREAM,
            op,
            flags,
            buf_alloc: buf_alloc_for(key),
            fwd_cnt,
        }
    }

    fn deliver_control(&mut self) -> Result<(), QueueError> {
        while let Some(&c) = self.control.front() {
            let Some(a) = self.active.as_mut() else {
                return Ok(());
            };
            let Some(chain) = a.rx.pop(&a.mem)? else {
                return Ok(());
            };
            self.control.pop_front();
            if c.op == OP_CREDIT_UPDATE {
                if let Some(conn) = self.conns.get_mut(&c.key) {
                    conn.update_queued = false;
                }
            }
            let header = self.header_for(c.key, c.op, c.flags, 0);
            let a = self.active.as_mut().unwrap();
            let n = chain.write(&a.mem, &header.bytes());
            let written = if n == HEADER_LEN { n as u32 } else { 0 };
            a.rx.push_used(&a.mem, chain.head, written)?;
        }
        Ok(())
    }

    fn pump_rx(&mut self, key: Key) -> Result<(), QueueError> {
        loop {
            self.deliver_control()?;
            if !self.control.is_empty() {
                self.starved.insert(key);
                self.set_reading(key, false);
                return Ok(());
            }
            let Some(conn) = self.conns.get(&key) else {
                return Ok(());
            };
            if conn.state != State::Established || conn.host_eof || conn.guest_done_receiving {
                return Ok(());
            }
            let credit = conn.peer_credit();
            let fd = conn.stream.as_raw_fd();
            match peek(fd) {
                ReadResult::Data(_) if credit == 0 => {
                    if crate::hops::on() && crate::hops::port_recorded(key.guest_port) {
                        let t = crate::hops::now();
                        crate::hops::add(b'K', 0, 0, t, t);
                    }
                    self.set_reading(key, false);
                    return Ok(());
                }
                ReadResult::Data(_) => {}
                ReadResult::WouldBlock => return Ok(()),
                ReadResult::Eof => {
                    let conn = self.conns.get_mut(&key).unwrap();
                    conn.host_eof = true;
                    let finished =
                        conn.host_closed || conn.guest_done_sending && conn.out.is_empty();
                    self.set_reading(key, false);
                    if finished {
                        self.reset(key);
                    } else {
                        self.queue_vital(key, OP_SHUTDOWN, SHUTDOWN_SEND);
                    }
                    return Ok(());
                }
                ReadResult::Error => {
                    self.reset(key);
                    return Ok(());
                }
            }
            let Some(a) = self.active.as_mut() else {
                return Ok(());
            };
            let Some(chain) = a.rx.pop(&a.mem)? else {
                if crate::hops::on() && crate::hops::port_recorded(key.guest_port) {
                    let t = crate::hops::now();
                    crate::hops::add(b'S', 0, 0, t, t);
                }
                self.starved.insert(key);
                self.set_reading(key, false);
                return Ok(());
            };
            let len = match read_into_chain(fd, &a.mem, &chain, credit) {
                ReadResult::Data(n) => n,
                _ => {
                    a.rx.push_used(&a.mem, chain.head, 0)?;
                    self.reset(key);
                    return Ok(());
                }
            };
            self.conns.get_mut(&key).unwrap().tx_cnt += len;
            if crate::hops::on() && crate::hops::port_recorded(key.guest_port) {
                let mut bytes = vec![0u8; len as usize];
                chain_read_at(&a.mem, &chain, HEADER_LEN as u64, &mut bytes);
                let conn = self.conns.get_mut(&key).unwrap();
                if let Some(h) = conn.hops.as_mut() {
                    let calls = &mut self.hop_calls;
                    h.0.feed(&bytes, |x, n| calls.push((x, n)));
                }
            }
            let header = self.header_for(key, OP_RW, 0, len);
            let a = self.active.as_mut().unwrap();
            chain.write(&a.mem, &header.bytes());
            a.rx.push_used(&a.mem, chain.head, HEADER_LEN as u32 + len)?;
        }
    }

    fn flush_out(&mut self, key: Key) -> bool {
        let Some(conn) = self.conns.get_mut(&key) else {
            return false;
        };
        if conn.host_closed {
            if conn.guest_done_receiving {
                self.reset(key);
                return false;
            }
            return true;
        }
        let mem = self.active.as_ref().map(|a| a.mem.clone());
        let mut done_heads: Vec<u16> = Vec::new();
        let mut broken = false;
        loop {
            if conn.out.is_empty() && !conn.pending.is_empty() {
                let Some(mem) = mem.as_ref() else { break };
                while let Some((chain, 0)) = conn.pending.front() {
                    done_heads.push(chain.head);
                    conn.pending.pop_front();
                }
                if conn.pending.is_empty() {
                    continue;
                }
                let Some(mut wrote) = write_kept(conn.stream.as_raw_fd(), mem, &conn.pending)
                else {
                    broken = true;
                    break;
                };
                if wrote == 0 {
                    match spill(mem, conn) {
                        Some(given) => done_heads.extend(given),
                        None => broken = true,
                    }
                    break;
                }
                while wrote > 0 {
                    let Some((chain, len)) = conn.pending.pop_front() else {
                        break;
                    };
                    conn.pending_bytes -= len;
                    let took = wrote.min(len);
                    wrote -= took;
                    conn.fwd_cnt += took as u32;
                    done_heads.push(chain.head);
                    if took < len
                        && !copy_payload_from(mem, &chain, took, len - took, &mut conn.out)
                    {
                        broken = true;
                        break;
                    }
                }
                if broken {
                    break;
                }
                continue;
            }
            if conn.out.is_empty() {
                break;
            }
            let (front, _) = conn.out.as_slices();
            match conn.stream.write(front) {
                Ok(0) => {
                    broken = true;
                    break;
                }
                Ok(n) => {
                    conn.out.drain(..n);
                    conn.fwd_cnt += n as u32;
                }
                Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                    if !conn.pending.is_empty() {
                        match mem.as_ref().and_then(|m| spill(m, conn)) {
                            Some(given) => done_heads.extend(given),
                            None => broken = true,
                        }
                    }
                    break;
                }
                Err(e) if e.kind() == io::ErrorKind::Interrupted => {}
                Err(_) => {
                    broken = true;
                    break;
                }
            }
        }
        let fd = conn.stream.as_raw_fd();
        for head in done_heads {
            self.complete_tx(head);
        }
        if broken {
            return self.host_stopped_reading(key);
        }
        let Some(conn) = self.conns.get_mut(&key) else {
            return false;
        };
        if !conn.out.is_empty() || !conn.pending.is_empty() {
            kevent_change(
                self.kq,
                fd,
                libc::EVFILT_WRITE,
                libc::EV_ADD | libc::EV_ONESHOT,
            );
        } else if conn.guest_done_sending {
            if conn.guest_done_receiving || conn.host_eof {
                self.reset(key);
                return false;
            }
            let _ = conn.stream.shutdown(std::net::Shutdown::Write);
        }
        if let Some(conn) = self.conns.get(&key) {
            if (conn.fwd_cnt - conn.fwd_cnt_sent).0 >= credit_update_threshold(key) {
                self.queue_credit_update(key);
            }
        }
        true
    }

    fn host_stopped_reading(&mut self, key: Key) -> bool {
        let Some(conn) = self.conns.get_mut(&key) else {
            return false;
        };
        conn.host_closed = true;
        conn.fwd_cnt += conn.out.len() as u32;
        conn.out = VecDeque::new();
        conn.fwd_cnt += conn.pending_bytes as u32;
        conn.pending_bytes = 0;
        let kept: Vec<u16> = conn.pending.drain(..).map(|(c, _)| c.head).collect();
        for head in kept {
            self.complete_tx(head);
        }
        let Some(conn) = self.conns.get_mut(&key) else {
            return false;
        };
        if conn.host_eof || conn.guest_done_receiving {
            self.reset(key);
            return false;
        }
        self.queue_vital(key, OP_SHUTDOWN, SHUTDOWN_RCV);
        self.conns.contains_key(&key)
    }

    fn accept(&mut self, listener: usize) {
        loop {
            let (ref l, guest_port) = self.listeners[listener];
            let stream = match l.accept() {
                Ok((s, _)) => s,
                Err(_) => return,
            };
            if stream.set_nonblocking(true).is_err() {
                continue;
            }
            set_send_buffer(&stream);
            let mut key = None;
            for _ in 0..=self.max_conns {
                let host_port = self.next_host_port;
                self.next_host_port = self
                    .next_host_port
                    .checked_add(1)
                    .unwrap_or(FIRST_EPHEMERAL_PORT);
                let k = Key {
                    host_port,
                    guest_port,
                };
                if !self.conns.contains_key(&k) {
                    key = Some(k);
                    break;
                }
            }
            let Some(key) = key else { continue };
            let mut conn = Conn::new(stream, State::Connecting);
            if crate::hops::on() && crate::hops::port_recorded(key.guest_port) {
                conn.hops = Some(Box::new(crate::hops::marks_for(key.guest_port)));
            }
            if self.insert(key, conn) {
                self.queue_vital(key, OP_REQUEST, 0);
            }
        }
    }

    fn guest_packet(&mut self, mem: &GuestMemory, chain: &Chain) {
        let mut raw = [0u8; HEADER_LEN];
        if !chain.read_exact(mem, &mut raw) {
            return;
        }
        let h = Header::parse(&raw);
        let key = Key {
            host_port: h.dst_port,
            guest_port: h.src_port,
        };
        if h.src_cid != GUEST_CID || h.dst_cid != HOST_CID || h.kind != TYPE_STREAM {
            if h.op != OP_RST {
                self.queue_control(key, OP_RST, 0);
            }
            return;
        }

        if h.op == OP_REQUEST {
            self.guest_connect(key, &h);
            return;
        }
        let Some(conn) = self.conns.get_mut(&key) else {
            if h.op != OP_RST {
                self.queue_control(key, OP_RST, 0);
            }
            return;
        };
        conn.peer_buf_alloc = h.buf_alloc;
        conn.peer_fwd_cnt = Wrapping(h.fwd_cnt);

        match h.op {
            OP_RESPONSE if conn.state == State::Connecting => {
                conn.state = State::Established;
                self.set_reading(key, true);
            }
            OP_RW if conn.state == State::Established && !conn.guest_done_sending => {
                let payload = chain.readable_len().saturating_sub(HEADER_LEN as u64);
                let len = h.len as u64;
                if len > payload
                    || (conn.out.len() + conn.pending_bytes) as u64 + len
                        > buf_alloc_for(key) as u64
                {
                    self.reset(key);
                    return;
                }
                let mut replies = Vec::new();
                if crate::hops::port_recorded(key.guest_port) {
                    if let Some(h) = conn.hops.as_mut() {
                        let mut bytes = VecDeque::new();
                        copy_payload(mem, chain, len as usize, &mut bytes);
                        h.1.feed(bytes.make_contiguous(), |x, n| replies.push((x, n)));
                    }
                }
                if conn.host_closed {
                    conn.fwd_cnt += len as u32;
                } else {
                    let want = buf_alloc_for(key) == VIEW_BUF_ALLOC
                        && (len >= DEFER_MIN || !conn.pending.is_empty());
                    let room = conn.out.is_empty()
                        && conn.pending.len() < KEPT_MAX_CONN
                        && conn.pending_bytes + len as usize <= KEPT_MAX_CONN_BYTES
                        && self.kept_total < KEPT_MAX;
                    if want && room {
                        conn.pending.push_back((chain.clone(), len as usize));
                        conn.pending_bytes += len as usize;
                        self.kept_total += 1;
                        kevent_change(
                            self.kq,
                            conn.stream.as_raw_fd(),
                            libc::EVFILT_WRITE,
                            libc::EV_ADD | libc::EV_ONESHOT,
                        );
                        self.kept = true;
                        self.record_replies(replies);
                        return self.credit_arrived(key);
                    }
                    let Some(given) = spill(mem, conn) else {
                        self.reset(key);
                        return;
                    };
                    for head in given {
                        self.complete_tx(head);
                    }
                    let conn = self.conns.get_mut(&key).unwrap();
                    let wrote = if conn.out.is_empty() {
                        write_payload(conn.stream.as_raw_fd(), mem, chain, len as usize)
                            .unwrap_or(0)
                    } else {
                        0
                    };
                    conn.fwd_cnt += wrote as u32;
                    if !copy_payload_from(mem, chain, wrote, len as usize - wrote, &mut conn.out) {
                        self.reset(key);
                        return;
                    }
                }
                self.flush_out(key);
                self.record_replies(replies);
            }
            OP_SHUTDOWN if conn.state == State::Established => {
                if h.flags & SHUTDOWN_SEND != 0 {
                    conn.guest_done_sending = true;
                }
                if h.flags & SHUTDOWN_RCV != 0 {
                    conn.guest_done_receiving = true;
                }
                let stop_reading = conn.guest_done_receiving;
                if stop_reading {
                    self.set_reading(key, false);
                }
                self.flush_out(key);
            }
            OP_CREDIT_UPDATE => {}
            OP_CREDIT_REQUEST => self.queue_credit_update(key),
            OP_RST => {
                self.remove(key);
                return;
            }
            _ => {
                self.reset(key);
                return;
            }
        }
        self.credit_arrived(key);
    }

    fn record_replies(&self, replies: Vec<(u32, u32)>) {
        if !replies.is_empty() {
            let t1 = crate::hops::now();
            for (x, n) in replies {
                crate::hops::add(b'R', x, n, self.hop_t0, t1);
            }
        }
    }

    fn guest_connect(&mut self, key: Key, h: &Header) {
        if self.conns.contains_key(&key) {
            self.reset(key);
            return;
        }
        let stream = match self.answers.get(&key.host_port) {
            Some(answer) => UnixStream::pair().and_then(|(ours, theirs)| {
                ours.set_nonblocking(true)?;
                if answer(theirs) {
                    Ok(ours)
                } else {
                    Err(io::Error::other("refused"))
                }
            }),
            None => {
                let path = self.dir.join(format!("host_{}.sock", key.host_port));
                UnixStream::connect(&path).and_then(|s| {
                    s.set_nonblocking(true)?;
                    Ok(s)
                })
            }
        };
        if let Ok(s) = &stream {
            set_send_buffer(s);
        }
        let Ok(stream) = stream else {
            self.queue_control(key, OP_RST, 0);
            return;
        };
        let mut conn = Conn::new(stream, State::Established);
        conn.peer_buf_alloc = h.buf_alloc;
        conn.peer_fwd_cnt = Wrapping(h.fwd_cnt);
        if !self.insert(key, conn) {
            self.queue_control(key, OP_RST, 0);
            return;
        }
        self.queue_vital(key, OP_RESPONSE, 0);
        self.set_reading(key, true);
    }

    fn settle(&mut self) {
        let result = (|| {
            loop {
                self.deliver_control()?;
                if self.control.is_empty() {
                    break;
                }
                let Some(a) = self.active.as_mut() else {
                    break;
                };
                if !a.rx.enable_notifications(&a.mem)? {
                    break;
                }
            }
            if self.control.is_empty() && !self.starved.is_empty() {
                let has_buffers = match self.active.as_mut() {
                    Some(a) => a.rx.has_available(&a.mem)? || a.rx.enable_notifications(&a.mem)?,
                    None => false,
                };
                if has_buffers {
                    for key in std::mem::take(&mut self.starved) {
                        let live = self.conns.get(&key).is_some_and(|c| {
                            c.state == State::Established && !c.host_eof && !c.guest_done_receiving
                        });
                        if live {
                            self.set_reading(key, true);
                        }
                    }
                }
            }
            match self.active.as_mut() {
                Some(a) => a.rx.needs_interrupt(&a.mem),
                None => Ok(false),
            }
        })();
        if let Some(a) = self.active.as_ref() {
            match result {
                Ok(true) => a.irq.signal_used(),
                Ok(false) => {}
                Err(e) => a.irq.fail("virtio-vsock", &e),
            }
        }
        if std::mem::take(&mut self.tx_done) {
            if let Some(a) = self.active.as_mut() {
                match a.tx.needs_interrupt(&a.mem) {
                    Ok(true) => a.irq.signal_used(),
                    Ok(false) => {}
                    Err(e) => a.irq.fail("virtio-vsock", &e),
                }
            }
        }
        if !self.hop_calls.is_empty() {
            let t1 = crate::hops::now();
            for (x, n) in std::mem::take(&mut self.hop_calls) {
                crate::hops::add(b'C', x, n, self.hop_t0, t1);
            }
        }
    }

    fn fail(&self, e: QueueError) {
        if let Some(a) = self.active.as_ref() {
            a.irq.fail("virtio-vsock", &e);
        }
    }
}

enum ReadResult {
    Data(u32),
    WouldBlock,
    Eof,
    Error,
}

fn peek(fd: RawFd) -> ReadResult {
    let mut byte = 0u8;
    loop {
        let flags = libc::MSG_PEEK | libc::MSG_DONTWAIT;
        let n = unsafe { libc::recv(fd, (&raw mut byte).cast(), 1, flags) };
        return match n {
            0 => ReadResult::Eof,
            n if n > 0 => ReadResult::Data(1),
            _ => match io::Error::last_os_error().kind() {
                io::ErrorKind::WouldBlock => ReadResult::WouldBlock,
                io::ErrorKind::Interrupted => continue,
                _ => ReadResult::Error,
            },
        };
    }
}

fn read_into_chain(fd: RawFd, mem: &GuestMemory, chain: &Chain, limit: u32) -> ReadResult {
    let mut iov: Vec<libc::iovec> = Vec::with_capacity(chain.writable.len());
    let mut skip = HEADER_LEN as u64;
    let mut room = limit as u64;
    for b in &chain.writable {
        let len = b.len as u64;
        if skip >= len {
            skip -= len;
            continue;
        }
        let take = (len - skip).min(room);
        if take == 0 {
            break;
        }
        let Some(addr) = b.addr.checked_add(skip) else {
            return ReadResult::Error;
        };
        let Ok(host) = mem.host_range(addr, take) else {
            return ReadResult::Error;
        };
        iov.push(libc::iovec {
            iov_base: host.cast(),
            iov_len: take as usize,
        });
        room -= take;
        skip = 0;
        if iov.len() == libc::IOV_MAX as usize {
            break;
        }
    }
    if iov.is_empty() {
        return ReadResult::Error;
    }
    loop {
        let n = unsafe { libc::readv(fd, iov.as_ptr(), iov.len() as i32) };
        return match n {
            0 => ReadResult::Eof,
            n if n > 0 => ReadResult::Data(n as u32),
            _ => match io::Error::last_os_error().kind() {
                io::ErrorKind::WouldBlock => ReadResult::WouldBlock,
                io::ErrorKind::Interrupted => continue,
                _ => ReadResult::Error,
            },
        };
    }
}

fn chain_read_at(mem: &GuestMemory, chain: &Chain, mut skip: u64, out: &mut [u8]) {
    let mut done = 0usize;
    for b in &chain.writable {
        let len = b.len as u64;
        if skip >= len {
            skip -= len;
            continue;
        }
        let n = ((len - skip) as usize).min(out.len() - done);
        if n == 0 {
            break;
        }
        if let Some(addr) = b.addr.checked_add(skip) {
            let _ = mem.read(addr, &mut out[done..done + n]);
        }
        done += n;
        skip = 0;
    }
}

fn write_payload(fd: RawFd, mem: &GuestMemory, chain: &Chain, len: usize) -> Option<usize> {
    let mut iov: Vec<libc::iovec> = Vec::with_capacity(chain.readable.len());
    let mut skip = HEADER_LEN as u64;
    let mut left = len as u64;
    for b in &chain.readable {
        let blen = b.len as u64;
        if skip >= blen {
            skip -= blen;
            continue;
        }
        let take = (blen - skip).min(left);
        if take == 0 {
            break;
        }
        let host = mem.host_range(b.addr.checked_add(skip)?, take).ok()?;
        iov.push(libc::iovec {
            iov_base: host.cast(),
            iov_len: take as usize,
        });
        left -= take;
        skip = 0;
        if iov.len() == libc::IOV_MAX as usize {
            break;
        }
    }
    loop {
        let n = unsafe { libc::writev(fd, iov.as_ptr(), iov.len() as i32) };
        if n >= 0 {
            return Some(n as usize);
        }
        match io::Error::last_os_error().kind() {
            io::ErrorKind::WouldBlock => return Some(0),
            io::ErrorKind::Interrupted => continue,
            _ => return None,
        }
    }
}

fn spill(mem: &GuestMemory, conn: &mut Conn) -> Option<Vec<u16>> {
    let mut given = Vec::with_capacity(conn.pending.len());
    while let Some((chain, len)) = conn.pending.pop_front() {
        conn.pending_bytes -= len;
        given.push(chain.head);
        if !copy_payload(mem, &chain, len, &mut conn.out) {
            given.extend(conn.pending.drain(..).map(|(c, _)| c.head));
            conn.pending_bytes = 0;
            return None;
        }
    }
    Some(given)
}

fn write_kept(fd: RawFd, mem: &GuestMemory, kept: &VecDeque<(Chain, usize)>) -> Option<usize> {
    let mut iov: Vec<libc::iovec> = Vec::new();
    'chains: for (chain, len) in kept {
        let mut skip = HEADER_LEN as u64;
        let mut left = *len as u64;
        for b in &chain.readable {
            let blen = b.len as u64;
            if skip >= blen {
                skip -= blen;
                continue;
            }
            let take = (blen - skip).min(left);
            if take == 0 {
                break;
            }
            let host = mem.host_range(b.addr.checked_add(skip)?, take).ok()?;
            iov.push(libc::iovec {
                iov_base: host.cast(),
                iov_len: take as usize,
            });
            left -= take;
            skip = 0;
            if iov.len() == libc::IOV_MAX as usize {
                break 'chains;
            }
        }
    }
    if iov.is_empty() {
        return Some(0);
    }
    loop {
        let n = unsafe { libc::writev(fd, iov.as_ptr(), iov.len() as i32) };
        if n >= 0 {
            return Some(n as usize);
        }
        match io::Error::last_os_error().kind() {
            io::ErrorKind::WouldBlock => return Some(0),
            io::ErrorKind::Interrupted => continue,
            _ => return None,
        }
    }
}

fn copy_payload(mem: &GuestMemory, chain: &Chain, len: usize, out: &mut VecDeque<u8>) -> bool {
    copy_payload_from(mem, chain, 0, len, out)
}

fn copy_payload_from(
    mem: &GuestMemory,
    chain: &Chain,
    from: usize,
    len: usize,
    out: &mut VecDeque<u8>,
) -> bool {
    let mut skip = HEADER_LEN as u64 + from as u64;
    let mut left = len;
    for b in &chain.readable {
        let blen = b.len as u64;
        if skip >= blen {
            skip -= blen;
            continue;
        }
        let off = skip;
        skip = 0;
        if off < blen && left > 0 {
            let n = ((blen - off) as usize).min(left);
            let Some(addr) = b.addr.checked_add(off) else {
                return false;
            };
            let Ok(host) = mem.host_range(addr, n as u64) else {
                return false;
            };
            out.extend(unsafe { std::slice::from_raw_parts(host as *const u8, n) });
            left -= n;
        }
    }
    left == 0
}

pub struct Vsock {
    inner: Arc<Mutex<Inner>>,
    kq: RawFd,
    thread: Option<JoinHandle<()>>,
}

fn connection_limit() -> usize {
    let want = (MAX_CONNECTIONS + RESERVED_DESCRIPTORS) as libc::rlim_t;
    let mut lim = libc::rlimit {
        rlim_cur: 0,
        rlim_max: 0,
    };
    if unsafe { libc::getrlimit(libc::RLIMIT_NOFILE, &mut lim) } != 0 {
        return MAX_CONNECTIONS.min(256 - RESERVED_DESCRIPTORS);
    }
    if lim.rlim_cur < want {
        let raised = libc::rlimit {
            rlim_cur: want.min(lim.rlim_max),
            rlim_max: lim.rlim_max,
        };
        if unsafe { libc::setrlimit(libc::RLIMIT_NOFILE, &raised) } == 0 {
            lim = raised;
        }
    }
    let usable = (lim.rlim_cur.min(want) as usize).saturating_sub(RESERVED_DESCRIPTORS);
    if usable < MAX_CONNECTIONS {
        crate::vmm_log!(
            "vsock: the open-file limit of {} allows {usable} connections, not {MAX_CONNECTIONS}",
            lim.rlim_cur
        );
    }
    usable.min(MAX_CONNECTIONS)
}

pub fn guest_port_path(dir: &Path, port: u32) -> PathBuf {
    dir.join(format!("vsock_{port}.sock"))
}

impl Vsock {
    pub fn new(dir: &Path, guest_ports: &[u32]) -> io::Result<Self> {
        let kq = unsafe { libc::kqueue() };
        if kq < 0 {
            return Err(io::Error::last_os_error());
        }
        let mut listeners = Vec::new();
        let mut sources = HashMap::new();
        for &port in guest_ports {
            let path = guest_port_path(dir, port);
            if path.as_os_str().len() > MAX_SOCKET_PATH {
                return Err(io::Error::other(format!(
                    "socket path {} is longer than the {MAX_SOCKET_PATH} bytes macOS allows; \
                     use a shorter --vsock-dir",
                    path.display()
                )));
            }
            let _ = std::fs::remove_file(&path);
            let listener = UnixListener::bind(&path)?;
            listener.set_nonblocking(true)?;
            let fd = listener.as_raw_fd();
            if unsafe { libc::listen(fd, LISTEN_BACKLOG) } != 0 {
                return Err(io::Error::last_os_error());
            }
            kevent_change(kq, fd, libc::EVFILT_READ, libc::EV_ADD);
            sources.insert(fd, Source::Listener { guest_port: port });
            listeners.push((listener, port));
        }
        kevent_change(kq, 0, libc::EVFILT_USER, libc::EV_ADD | libc::EV_CLEAR);

        let inner = Arc::new(Mutex::new(Inner {
            dir: dir.to_path_buf(),
            kq,
            active: None,
            listeners,
            answers: HashMap::new(),
            conns: HashMap::new(),
            sources,
            control: VecDeque::new(),
            starved: HashSet::new(),
            next_host_port: FIRST_EPHEMERAL_PORT,
            max_conns: connection_limit(),
            stopping: false,
            hop_t0: 0,
            hop_calls: Vec::new(),
            kept: false,
            tx_done: false,
            kept_total: 0,
        }));
        let for_thread = inner.clone();
        let thread = std::thread::Builder::new()
            .name("virtio-vsock".into())
            .spawn(move || run_events(kq, &for_thread))?;
        Ok(Self {
            inner,
            kq,
            thread: Some(thread),
        })
    }

    pub fn answer(&mut self, port: u32, answer: Answer) {
        self.inner.lock().unwrap().answers.insert(port, answer);
    }

    fn config() -> [u8; 8] {
        GUEST_CID.to_le_bytes()
    }
}

fn run_events(kq: RawFd, inner: &Mutex<Inner>) {
    let mut events: [libc::kevent; 64] = unsafe { std::mem::zeroed() };
    loop {
        let n = unsafe {
            libc::kevent(
                kq,
                std::ptr::null(),
                0,
                events.as_mut_ptr(),
                events.len() as i32,
                std::ptr::null(),
            )
        };
        if n < 0 {
            if io::Error::last_os_error().kind() == io::ErrorKind::Interrupted {
                continue;
            }
            return;
        }
        let mut inner = inner.lock().unwrap();
        if inner.stopping {
            return;
        }
        if crate::hops::on() {
            inner.hop_t0 = crate::hops::now();
        }
        for ev in &events[..n as usize] {
            if ev.filter == libc::EVFILT_USER {
                continue;
            }
            let Some(&source) = inner.sources.get(&(ev.ident as RawFd)) else {
                continue;
            };
            match source {
                Source::Listener { guest_port } => {
                    if let Some(i) = inner.listeners.iter().position(|(_, p)| *p == guest_port) {
                        inner.accept(i);
                    }
                }
                Source::Conn(key) if ev.filter == libc::EVFILT_WRITE => {
                    inner.flush_out(key);
                }
                Source::Conn(key) => {
                    if let Err(e) = inner.pump_rx(key) {
                        inner.fail(e);
                    }
                }
            }
        }
        inner.settle();
    }
}

impl VirtioDevice for Vsock {
    fn device_id(&self) -> u32 {
        DEVICE_ID
    }

    fn features(&self) -> u64 {
        0
    }

    fn queue_max_sizes(&self) -> &[u16] {
        &QUEUE_SIZES
    }

    fn read_config(&self, offset: u64, data: &mut [u8]) {
        read_config_bytes(&Self::config(), offset, data);
    }

    fn activate(&mut self, mut activation: Activation) {
        activation.queues.truncate(2);
        let tx = activation.queues.pop().unwrap();
        let rx = activation.queues.pop().unwrap();
        let mut inner = self.inner.lock().unwrap();
        inner.active = Some(Active {
            mem: activation.mem,
            irq: activation.irq,
            rx,
            tx,
        });
        inner.settle();
    }

    fn notify(&mut self, queue: u32) {
        let mut inner = self.inner.lock().unwrap();
        if crate::hops::on() {
            inner.hop_t0 = crate::hops::now();
        }
        if queue == TX {
            let result = (|| loop {
                loop {
                    let Some(a) = inner.active.as_mut() else {
                        return Ok(false);
                    };
                    let Some(chain) = a.tx.pop(&a.mem)? else {
                        break;
                    };
                    let mem = a.mem.clone();
                    inner.kept = false;
                    inner.guest_packet(&mem, &chain);
                    if inner.kept {
                        continue;
                    }
                    let a = inner.active.as_mut().unwrap();
                    a.tx.push_used(&a.mem, chain.head, 0)?;
                }
                let a = inner.active.as_mut().unwrap();
                if !a.tx.enable_notifications(&a.mem)? {
                    return a.tx.needs_interrupt(&a.mem);
                }
            })();
            match (result, inner.active.as_ref()) {
                (Ok(true), Some(a)) => a.irq.signal_used(),
                (Err(e), _) => inner.fail(e),
                _ => {}
            }
        }
        if queue == RX || queue == TX {
            inner.settle();
        }
    }

    fn reset(&mut self) {
        let mut inner = self.inner.lock().unwrap();
        inner.active = None;
        let keys: Vec<Key> = inner.conns.keys().copied().collect();
        for key in keys {
            inner.remove(key);
        }
        inner.control.clear();
        inner.starved.clear();
    }
}

impl Drop for Vsock {
    fn drop(&mut self) {
        {
            let mut inner = self.inner.lock().unwrap();
            inner.stopping = true;
            for (_, port) in &inner.listeners {
                let _ = std::fs::remove_file(guest_port_path(&inner.dir, *port));
            }
        }
        let wake = libc::kevent {
            ident: 0,
            filter: libc::EVFILT_USER,
            flags: 0,
            fflags: libc::NOTE_TRIGGER,
            data: 0,
            udata: std::ptr::null_mut(),
        };
        unsafe { libc::kevent(self.kq, &wake, 1, std::ptr::null_mut(), 0, std::ptr::null()) };
        if let Some(t) = self.thread.take() {
            let _ = t.join();
        }
        unsafe { libc::close(self.kq) };
    }
}

#[cfg(test)]
mod tests {
    use super::super::queue::testing::{memory, Driver};
    use super::super::testing::interrupt;
    use super::*;
    use std::io::Read;
    use std::time::{Duration, Instant};

    const BASE: u64 = 0x4000_0000;
    const RX_BUFS: u64 = BASE + 0x10000;
    const TX_BUF: u64 = BASE + 0x40000;
    const RX_BUF_LEN: u32 = 4096;

    struct Guest {
        mem: Arc<GuestMemory>,
        rx: Driver,
        tx: Driver,
        dev: Vsock,
        dir: PathBuf,
        next_rx: u64,
    }

    fn short_dir(tag: &str) -> PathBuf {
        let dir = PathBuf::from(format!("/tmp/bv-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    impl Guest {
        fn new(tag: &str, ports: &[u32]) -> Self {
            Self::with(tag, ports, false, 8)
        }

        fn with(tag: &str, ports: &[u32], event_idx: bool, posted: usize) -> Self {
            Self::with_queues(tag, ports, event_idx, posted, 16)
        }

        fn with_queues(
            tag: &str,
            ports: &[u32],
            event_idx: bool,
            posted: usize,
            tx_size: u16,
        ) -> Self {
            let dir = short_dir(tag);
            let mem = Arc::new(memory());
            let (rx, mut rxq) = Driver::new(&mem, BASE, 16);
            rxq.event_idx = event_idx;
            let (tx, txq) = Driver::new(&mem, BASE + 0x2000, tx_size);
            let (_, evq) = Driver::new(&mem, BASE + 0x4000, 16);
            let (_chip, irq) = interrupt();
            let mut dev = Vsock::new(&dir, ports).unwrap();
            dev.activate(Activation {
                mem: mem.clone(),
                irq,
                queues: vec![rxq, txq, evq],
                features: 0,
            });
            let mut g = Self {
                mem,
                rx,
                tx,
                dev,
                dir,
                next_rx: 0,
            };
            if posted > 0 {
                g.post_rx(posted);
            }
            g
        }

        fn post_rx_quietly(&mut self, n: usize) {
            let old = self.mem.read_u16(self.rx.avail + 2).unwrap();
            for _ in 0..n {
                let addr = RX_BUFS + (self.next_rx % 32) * RX_BUF_LEN as u64;
                self.next_rx += 1;
                self.rx.add(&self.mem, &[(addr, RX_BUF_LEN, true)]);
            }
            let new = self.mem.read_u16(self.rx.avail + 2).unwrap();
            let event = self.rx.avail_event(&self.mem);
            if new.wrapping_sub(event).wrapping_sub(1) < new.wrapping_sub(old) {
                self.dev.notify(RX);
            }
        }

        fn post_rx(&mut self, n: usize) {
            for _ in 0..n {
                let addr = RX_BUFS + (self.next_rx % 32) * RX_BUF_LEN as u64;
                self.next_rx += 1;
                self.rx.add(&self.mem, &[(addr, RX_BUF_LEN, true)]);
            }
            self.dev.notify(RX);
        }

        fn send(&mut self, h: Header, payload: &[u8]) {
            self.mem.write(TX_BUF, &h.bytes()).unwrap();
            self.mem.write(TX_BUF + 44, payload).unwrap();
            self.tx
                .add(&self.mem, &[(TX_BUF, 44 + payload.len() as u32, false)]);
            self.dev.notify(TX);
            assert!(self.tx.take_used(&self.mem).is_some());
        }

        fn packet(&self, op: u16, host_port: u32, guest_port: u32) -> Header {
            Header {
                src_cid: GUEST_CID,
                dst_cid: HOST_CID,
                src_port: guest_port,
                dst_port: host_port,
                kind: TYPE_STREAM,
                op,
                buf_alloc: 64 << 10,
                ..Header::default()
            }
        }

        fn recv(&mut self) -> (Header, Vec<u8>) {
            let deadline = Instant::now() + Duration::from_secs(5);
            loop {
                if let Some((head, written)) = self.rx.take_used(&self.mem) {
                    let addr = self.mem.read_u64(self.rx.desc + head as u64 * 16).unwrap();
                    let mut raw = [0u8; 44];
                    self.mem.read(addr, &mut raw).unwrap();
                    let h = Header::parse(&raw);
                    assert_eq!(written as usize, 44 + h.len as usize);
                    let mut payload = vec![0u8; h.len as usize];
                    self.mem.read(addr + 44, &mut payload).unwrap();
                    return (h, payload);
                }
                assert!(Instant::now() < deadline, "no packet from the device");
                std::thread::sleep(Duration::from_millis(1));
            }
        }

        fn expect_nothing(&mut self) {
            std::thread::sleep(Duration::from_millis(30));
            assert_eq!(self.rx.take_used(&self.mem), None);
        }
    }

    impl Drop for Guest {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.dir);
        }
    }

    fn read_exact_timeout(s: &mut UnixStream, n: usize) -> Vec<u8> {
        s.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
        let mut buf = vec![0u8; n];
        s.read_exact(&mut buf).unwrap();
        buf
    }

    #[test]
    fn header_round_trips() {
        let h = Header {
            src_cid: 3,
            dst_cid: 2,
            src_port: 0x1234_5678,
            dst_port: 1024,
            len: 77,
            kind: 1,
            op: 5,
            flags: 3,
            buf_alloc: 0x4_0000,
            fwd_cnt: 0xffff_fff0,
        };
        assert_eq!(Header::parse(&h.bytes()), h);
    }

    #[test]
    fn host_connects_to_a_guest_port_and_data_flows_both_ways() {
        let mut g = Guest::new("h2g", &[1024]);
        let mut host = UnixStream::connect(guest_port_path(&g.dir, 1024)).unwrap();

        let (req, _) = g.recv();
        assert_eq!(
            (req.op, req.dst_port, req.src_cid, req.dst_cid),
            (OP_REQUEST, 1024, 2, 3)
        );
        assert_eq!(req.buf_alloc, BUF_ALLOC);
        let hp = req.src_port;
        g.send(g.packet(OP_RESPONSE, hp, 1024), &[]);

        host.write_all(b"hello guest").unwrap();
        let (rw, payload) = g.recv();
        assert_eq!(rw.op, OP_RW);
        assert_eq!(payload, b"hello guest");

        let mut h = g.packet(OP_RW, hp, 1024);
        h.len = 10;
        h.fwd_cnt = 11;
        g.send(h, b"hello host");
        assert_eq!(read_exact_timeout(&mut host, 10), b"hello host");
    }

    #[test]
    fn the_views_large_packets_go_from_the_guests_buffers_in_order() {
        let mut g = Guest::new("view", &[VIEW_GUEST_PORT]);
        let mut host = UnixStream::connect(guest_port_path(&g.dir, VIEW_GUEST_PORT)).unwrap();
        let (req, _) = g.recv();
        assert_eq!(req.buf_alloc, VIEW_BUF_ALLOC);
        let hp = req.src_port;
        g.send(g.packet(OP_RESPONSE, hp, VIEW_GUEST_PORT), &[]);

        let big: Vec<u8> = (0..40_000u32).map(|i| (i % 251) as u8).collect();
        let mut h = g.packet(OP_RW, hp, VIEW_GUEST_PORT);
        h.len = big.len() as u32;
        let at = TX_BUF + 0x1000;
        g.mem.write(at, &h.bytes()).unwrap();
        g.mem.write(at + 44, &big).unwrap();
        g.tx.add(&g.mem, &[(at, 44 + big.len() as u32, false)]);
        let mut s = g.packet(OP_RW, hp, VIEW_GUEST_PORT);
        s.len = 5;
        g.mem.write(TX_BUF, &s.bytes()).unwrap();
        g.mem.write(TX_BUF + 44, b"after").unwrap();
        g.tx.add(&g.mem, &[(TX_BUF, 49, false)]);
        g.dev.notify(TX);

        let mut want = big.clone();
        want.extend_from_slice(b"after");
        assert_eq!(read_exact_timeout(&mut host, want.len()), want);
        let deadline = Instant::now() + Duration::from_secs(5);
        let mut back = 0;
        while back < 2 {
            if g.tx.take_used(&g.mem).is_some() {
                back += 1;
                continue;
            }
            assert!(Instant::now() < deadline, "{back} of 2 chains given back");
            std::thread::sleep(Duration::from_millis(1));
        }
    }

    fn view_conn(g: &mut Guest) -> (UnixStream, u32) {
        let host = UnixStream::connect(guest_port_path(&g.dir, VIEW_GUEST_PORT)).unwrap();
        let (req, _) = g.recv();
        let hp = req.src_port;
        g.send(g.packet(OP_RESPONSE, hp, VIEW_GUEST_PORT), &[]);
        (host, hp)
    }

    fn big_at(i: usize, len: usize) -> u64 {
        TX_BUF + 0x2000 + (i * (HEADER_LEN + len).next_multiple_of(0x1000)) as u64
    }

    fn queue_rw(g: &mut Guest, hp: u32, at: u64, len: usize, seed: usize) -> Vec<u8> {
        let data: Vec<u8> = (0..len)
            .map(|i| ((i * 7 + seed * 13) % 251) as u8)
            .collect();
        let mut h = g.packet(OP_RW, hp, VIEW_GUEST_PORT);
        h.len = len as u32;
        g.mem.write(at, &h.bytes()).unwrap();
        g.mem.write(at + HEADER_LEN as u64, &data).unwrap();
        g.tx.add(&g.mem, &[(at, (HEADER_LEN + len) as u32, false)]);
        data
    }

    fn hand_over(g: &mut Guest) -> (usize, usize, usize) {
        let mut inner = g.dev.inner.lock().unwrap();
        loop {
            let a = inner.active.as_mut().unwrap();
            let Some(chain) = a.tx.pop(&a.mem).unwrap() else {
                break;
            };
            let mem = a.mem.clone();
            inner.kept = false;
            inner.guest_packet(&mem, &chain);
            if inner.kept {
                continue;
            }
            let a = inner.active.as_mut().unwrap();
            a.tx.push_used(&a.mem, chain.head, 0).unwrap();
        }
        let bytes = inner
            .conns
            .values()
            .map(|c| c.pending_bytes)
            .max()
            .unwrap_or(0);
        let most = inner
            .conns
            .values()
            .map(|c| c.pending.len())
            .max()
            .unwrap_or(0);
        let all = inner.conns.values().map(|c| c.pending.len()).sum();
        (bytes, most, all)
    }

    fn chains_back(g: &mut Guest, n: usize, within: Duration) -> usize {
        let deadline = Instant::now() + within;
        let mut back = 0;
        while back < n && Instant::now() < deadline {
            if g.tx.take_used(&g.mem).is_some() {
                back += 1;
            } else {
                std::thread::sleep(Duration::from_millis(1));
            }
        }
        back
    }

    #[test]
    fn a_stalled_view_reader_holds_none_of_the_guests_buffers() {
        let mut g = Guest::new("vstall", &[VIEW_GUEST_PORT]);
        let (mut host, hp) = view_conn(&mut g);
        let len = 56 << 10;
        let mut want = Vec::new();
        for i in 0..12 {
            want.extend(queue_rw(&mut g, hp, big_at(i, len), len, i));
        }
        g.dev.notify(TX);
        let back = chains_back(&mut g, 12, Duration::from_secs(2));
        assert_eq!(
            back, 12,
            "{back} of 12 chains given back with the reader stopped"
        );
        assert_eq!(read_exact_timeout(&mut host, want.len()), want);
    }

    #[test]
    fn a_view_connection_keeps_no_more_than_its_cap() {
        let mut g = Guest::new("vcap", &[VIEW_GUEST_PORT]);
        let (mut host, hp) = view_conn(&mut g);
        let len = 56 << 10;
        let mut want = Vec::new();
        for i in 0..12 {
            want.extend(queue_rw(&mut g, hp, big_at(i, len), len, i));
        }
        let (bytes, most, _) = hand_over(&mut g);
        assert!(
            bytes <= KEPT_MAX_CONN_BYTES && most <= KEPT_MAX_CONN,
            "one connection keeps {most} packets, {bytes} bytes"
        );
        assert_eq!(read_exact_timeout(&mut host, want.len()), want);
        assert_eq!(chains_back(&mut g, 12, Duration::from_secs(5)), 12);
    }

    #[test]
    fn view_connections_keep_no_more_than_the_cap_in_all() {
        let mut g = Guest::with_queues("vcapall", &[VIEW_GUEST_PORT], false, 8, 64);
        let len = 16 << 10;
        let mut conns = Vec::new();
        for _ in 0..3 {
            conns.push(view_conn(&mut g));
        }
        let mut want = vec![Vec::new(); 3];
        for i in 0..36 {
            let hp = conns[i % 3].1;
            want[i % 3].extend(queue_rw(&mut g, hp, big_at(i, len), len, i));
        }
        let (_, _, all) = hand_over(&mut g);
        assert!(all <= KEPT_MAX, "{all} packets kept in all");
        for (c, w) in conns.iter_mut().zip(&want) {
            assert_eq!(read_exact_timeout(&mut c.0, w.len()), *w);
        }
        assert_eq!(chains_back(&mut g, 36, Duration::from_secs(5)), 36);
    }

    #[test]
    fn other_ports_keep_the_ordinary_window() {
        let mut g = Guest::new("noview", &[1024]);
        let _host = UnixStream::connect(guest_port_path(&g.dir, 1024)).unwrap();
        let (req, _) = g.recv();
        assert_eq!(req.buf_alloc, BUF_ALLOC);
    }

    #[test]
    fn a_port_the_vmm_answers_needs_no_socket_and_may_refuse() {
        let mut g = Guest::new("answer", &[]);
        let (tx, rx) = std::sync::mpsc::channel::<UnixStream>();
        let taken = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let flag = taken.clone();
        g.dev.answer(
            1027,
            Box::new(move |stream| {
                if flag.swap(true, std::sync::atomic::Ordering::SeqCst) {
                    return false;
                }
                tx.send(stream).is_ok()
            }),
        );
        g.send(g.packet(OP_REQUEST, 1027, 5555), &[]);
        assert_eq!(g.recv().0.op, OP_RESPONSE);
        let mut ours = rx.recv().unwrap();
        let mut rw = g.packet(OP_RW, 1027, 5555);
        rw.len = 5;
        g.send(rw, b"hello");
        assert_eq!(read_exact_timeout(&mut ours, 5), b"hello");
        ours.write_all(b"records").unwrap();
        let (h, data) = g.recv();
        assert_eq!((h.op, &data[..]), (OP_RW, &b"records"[..]));
        g.send(g.packet(OP_REQUEST, 1027, 5556), &[]);
        assert_eq!(g.recv().0.op, OP_RST);
        assert!(!g.dir.join("host_1027.sock").exists());
        let mut size: libc::c_int = 0;
        let mut len = std::mem::size_of::<libc::c_int>() as libc::socklen_t;
        let inner = g.dev.inner.lock().unwrap();
        let conn = inner.conns.values().next().unwrap();
        unsafe {
            libc::getsockopt(
                conn.stream.as_raw_fd(),
                libc::SOL_SOCKET,
                libc::SO_SNDBUF,
                (&raw mut size).cast(),
                &mut len,
            )
        };
        assert_eq!(size, SEND_BUFFER);
    }

    #[test]
    fn guest_connects_to_a_host_listener() {
        let mut g = Guest::new("g2h", &[]);
        let listener = UnixListener::bind(g.dir.join("host_7000.sock")).unwrap();
        g.send(g.packet(OP_REQUEST, 7000, 5555), &[]);
        let (resp, _) = g.recv();
        assert_eq!(
            (resp.op, resp.src_port, resp.dst_port),
            (OP_RESPONSE, 7000, 5555)
        );
        let (mut host, _) = listener.accept().unwrap();

        let mut h = g.packet(OP_RW, 7000, 5555);
        h.len = 4;
        g.send(h, b"ping");
        assert_eq!(read_exact_timeout(&mut host, 4), b"ping");
        host.write_all(b"pong").unwrap();
        let (rw, payload) = g.recv();
        assert_eq!((rw.op, payload.as_slice()), (OP_RW, &b"pong"[..]));
    }

    #[test]
    fn a_burst_of_short_connections_all_complete() {
        const N: usize = 300;
        let mut g = Guest::new("burst", &[1026]);
        let path = guest_port_path(&g.dir, 1026);
        let client = std::thread::spawn(move || {
            let mut conns = Vec::new();
            let deadline = Instant::now() + Duration::from_secs(10);
            for _ in 0..N {
                conns.push(loop {
                    match UnixStream::connect(&path) {
                        Ok(conn) => break conn,
                        Err(e)
                            if e.kind() == std::io::ErrorKind::ConnectionRefused
                                && Instant::now() < deadline =>
                        {
                            std::thread::sleep(Duration::from_millis(1))
                        }
                        Err(e) => panic!("connect failed in a burst: {e}"),
                    }
                });
            }
            for (i, c) in conns.iter_mut().enumerate() {
                c.set_read_timeout(Some(Duration::from_secs(10))).unwrap();
                let mut byte = [0u8; 1];
                c.read_exact(&mut byte).unwrap();
                assert_eq!(byte[0], i as u8, "connections got crossed");
            }
        });
        let mut first = None;
        for _ in 0..N {
            g.post_rx(1);
            let (req, _) = g.recv();
            assert_eq!(req.op, OP_REQUEST);
            let base = *first.get_or_insert(req.src_port);
            g.send(g.packet(OP_RESPONSE, req.src_port, 1026), &[]);
            let mut h = g.packet(OP_RW, req.src_port, 1026);
            h.len = 1;
            g.send(h, &[(req.src_port - base) as u8]);
        }
        client.join().unwrap();
        assert_eq!(g.dev.inner.lock().unwrap().conns.len(), N);
    }

    #[test]
    fn a_connection_that_cannot_be_announced_is_closed_not_left_hanging() {
        let g = Guest::new("full", &[1026]);
        {
            let mut inner = g.dev.inner.lock().unwrap();
            inner.active.as_mut().unwrap().rx = Driver::new(&g.mem, BASE + 0x6000, 16).1;
            let key = Key {
                host_port: 1,
                guest_port: 1,
            };
            while inner.queue_control(key, OP_RST, 0) {}
            assert_eq!(inner.control.len(), MAX_PENDING_CONTROL);
        }
        let mut host = UnixStream::connect(guest_port_path(&g.dir, 1026)).unwrap();
        host.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
        let mut byte = [0u8; 1];
        assert_eq!(host.read(&mut byte).unwrap(), 0, "expected end of file");
        assert!(g.dev.inner.lock().unwrap().conns.is_empty());
    }

    #[test]
    fn the_connection_limit_fits_the_open_file_limit() {
        let limit = connection_limit();
        let mut lim = libc::rlimit {
            rlim_cur: 0,
            rlim_max: 0,
        };
        assert_eq!(unsafe { libc::getrlimit(libc::RLIMIT_NOFILE, &mut lim) }, 0);
        assert!(limit <= MAX_CONNECTIONS);
        assert!(limit as u64 + RESERVED_DESCRIPTORS as u64 <= lim.rlim_cur);
        assert_eq!(limit, MAX_CONNECTIONS);
    }

    #[test]
    fn a_connection_to_a_port_nobody_listens_on_is_refused() {
        let mut g = Guest::new("refuse", &[]);
        g.send(g.packet(OP_REQUEST, 9, 5555), &[]);
        let (rst, _) = g.recv();
        assert_eq!((rst.op, rst.src_port, rst.dst_port), (OP_RST, 9, 5555));
        g.send(g.packet(OP_RW, 9, 5555), &[]);
        assert_eq!(g.recv().0.op, OP_RST);
        g.send(g.packet(OP_RST, 9, 5555), &[]);
        g.expect_nothing();
    }

    #[test]
    fn wrong_addresses_and_socket_types_are_reset() {
        let mut g = Guest::new("addr", &[]);
        let mut h = g.packet(OP_REQUEST, 7000, 5555);
        h.dst_cid = 99;
        g.send(h, &[]);
        assert_eq!(g.recv().0.op, OP_RST);
        let mut h = g.packet(OP_REQUEST, 7000, 5555);
        h.kind = 2;
        g.send(h, &[]);
        assert_eq!(g.recv().0.op, OP_RST);
    }

    #[test]
    fn host_data_respects_the_guests_credit() {
        let mut g = Guest::new("credit", &[1024]);
        let mut host = UnixStream::connect(guest_port_path(&g.dir, 1024)).unwrap();
        let hp = g.recv().0.src_port;
        let mut resp = g.packet(OP_RESPONSE, hp, 1024);
        resp.buf_alloc = 6;
        g.send(resp, &[]);

        host.write_all(b"0123456789").unwrap();
        let (_, first) = g.recv();
        assert_eq!(first, b"012345");
        g.expect_nothing();

        let mut upd = g.packet(OP_CREDIT_UPDATE, hp, 1024);
        upd.buf_alloc = 6;
        upd.fwd_cnt = 6;
        g.send(upd, &[]);
        let (_, rest) = g.recv();
        assert_eq!(rest, b"6789");
    }

    #[test]
    fn host_data_waits_for_guest_receive_buffers() {
        let mut g = Guest::new("starve", &[1024]);
        let mut host = UnixStream::connect(guest_port_path(&g.dir, 1024)).unwrap();
        let hp = g.recv().0.src_port;
        g.send(g.packet(OP_RESPONSE, hp, 1024), &[]);

        let data: Vec<u8> = (0..40_000u32).map(|i| i as u8).collect();
        let to_send = data.clone();
        let writer = std::thread::spawn(move || host.write_all(&to_send).map(|()| host));
        let mut got = Vec::new();
        for _ in 0..7 {
            got.extend(g.recv().1);
        }
        g.expect_nothing();
        while got.len() < data.len() {
            g.post_rx(1);
            got.extend(g.recv().1);
        }
        assert_eq!(got, data);
        writer.join().unwrap().unwrap();
    }

    #[test]
    fn a_guest_that_exceeds_our_buffer_is_reset() {
        let mut g = Guest::new("overrun", &[]);
        let listener = UnixListener::bind(g.dir.join("host_7000.sock")).unwrap();
        g.send(g.packet(OP_REQUEST, 7000, 5555), &[]);
        assert_eq!(g.recv().0.op, OP_RESPONSE);
        let (_host, _) = listener.accept().unwrap();

        let mut h = g.packet(OP_RW, 7000, 5555);
        h.len = 5000;
        g.send(h, b"short");
        assert_eq!(g.recv().0.op, OP_RST);
    }

    #[test]
    fn host_close_is_a_shutdown_and_guest_close_finishes_it() {
        let mut g = Guest::new("hclose", &[1024]);
        let host = UnixStream::connect(guest_port_path(&g.dir, 1024)).unwrap();
        let hp = g.recv().0.src_port;
        g.send(g.packet(OP_RESPONSE, hp, 1024), &[]);
        drop(host);

        let (sd, _) = g.recv();
        assert_eq!((sd.op, sd.flags), (OP_SHUTDOWN, SHUTDOWN_SEND));
        let mut h = g.packet(OP_SHUTDOWN, hp, 1024);
        h.flags = SHUTDOWN_RCV | SHUTDOWN_SEND;
        g.send(h, &[]);
        assert_eq!(g.recv().0.op, OP_RST);
        assert!(g.dev.inner.lock().unwrap().conns.is_empty());
    }

    #[test]
    fn guest_shutdown_reaches_the_host_as_end_of_file() {
        let mut g = Guest::new("gclose", &[]);
        let listener = UnixListener::bind(g.dir.join("host_7000.sock")).unwrap();
        g.send(g.packet(OP_REQUEST, 7000, 5555), &[]);
        assert_eq!(g.recv().0.op, OP_RESPONSE);
        let (mut host, _) = listener.accept().unwrap();

        let mut h = g.packet(OP_RW, 7000, 5555);
        h.len = 3;
        g.send(h, b"bye");
        let mut h = g.packet(OP_SHUTDOWN, 7000, 5555);
        h.flags = SHUTDOWN_SEND;
        g.send(h, &[]);

        host.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
        let mut all = Vec::new();
        host.read_to_end(&mut all).unwrap();
        assert_eq!(all, b"bye");

        host.write_all(b"ok").unwrap();
        assert_eq!(g.recv().1, b"ok");
        drop(host);
        assert_eq!(g.recv().0.op, OP_RST);
        assert!(g.dev.inner.lock().unwrap().conns.is_empty());
    }

    #[test]
    fn a_slow_host_reader_gets_everything_in_order() {
        let mut g = Guest::new("slow", &[]);
        let listener = UnixListener::bind(g.dir.join("host_7000.sock")).unwrap();
        g.send(g.packet(OP_REQUEST, 7000, 5555), &[]);
        assert_eq!(g.recv().0.op, OP_RESPONSE);
        let (mut host, _) = listener.accept().unwrap();

        let chunk: Vec<u8> = (0..16_384u32).map(|i| (i * 3) as u8).collect();
        let total = BUF_ALLOC as usize;
        for _ in 0..total / chunk.len() {
            let mut h = g.packet(OP_RW, 7000, 5555);
            h.len = chunk.len() as u32;
            g.send(h, &chunk);
        }
        let got = read_exact_timeout(&mut host, total);
        for (i, piece) in got.chunks(chunk.len()).enumerate() {
            assert_eq!(piece, chunk, "chunk {i}");
        }
        let mut saw_update = false;
        for _ in 0..8 {
            let (h, _) = g.recv();
            if h.op == OP_CREDIT_UPDATE {
                assert!(h.fwd_cnt > 0);
                saw_update = true;
                break;
            }
        }
        assert!(saw_update);
    }

    #[test]
    fn reset_drops_every_connection() {
        let mut g = Guest::new("reset", &[1024]);
        let mut host = UnixStream::connect(guest_port_path(&g.dir, 1024)).unwrap();
        let hp = g.recv().0.src_port;
        g.send(g.packet(OP_RESPONSE, hp, 1024), &[]);
        g.dev.reset();
        let _ = host.set_read_timeout(Some(Duration::from_secs(5)));
        match host.read(&mut [0u8; 8]) {
            Ok(0) => {}
            Err(e) if e.kind() == io::ErrorKind::ConnectionReset => {}
            other => panic!("host did not see the close: {other:?}"),
        }
    }

    #[test]
    fn replies_and_data_waiting_for_a_receive_buffer_go_when_the_guest_posts_one() {
        let mut g = Guest::with("replies", &[1024], true, 2);
        let mut host = UnixStream::connect(guest_port_path(&g.dir, 1024)).unwrap();
        let hp = g.recv().0.src_port;
        g.send(g.packet(OP_RESPONSE, hp, 1024), &[]);
        host.write_all(b"first").unwrap();
        assert_eq!(g.recv().1, b"first");
        for port in [7001, 7002, 7003] {
            g.send(g.packet(OP_RW, port, 5555), &[]);
        }
        host.write_all(b"unsubscribe").unwrap();
        std::thread::sleep(Duration::from_millis(50));
        g.post_rx_quietly(4);
        for port in [7001, 7002, 7003] {
            let (h, _) = g.recv();
            assert_eq!((h.op, h.src_port), (OP_RST, port));
        }
        let (h, data) = g.recv();
        assert_eq!((h.op, &data[..]), (OP_RW, &b"unsubscribe"[..]));
    }

    #[test]
    fn host_data_goes_on_when_any_guest_packet_gives_credit_back() {
        let mut g = Guest::new("credit-rw", &[1024]);
        let mut host = UnixStream::connect(guest_port_path(&g.dir, 1024)).unwrap();
        let hp = g.recv().0.src_port;
        let mut resp = g.packet(OP_RESPONSE, hp, 1024);
        resp.buf_alloc = 6;
        g.send(resp, &[]);
        host.write_all(b"0123456789").unwrap();
        assert_eq!(g.recv().1, b"012345");
        g.expect_nothing();
        let mut rw = g.packet(OP_RW, hp, 1024);
        rw.buf_alloc = 6;
        rw.fwd_cnt = 6;
        rw.len = 2;
        g.send(rw, b"ok");
        assert_eq!(read_exact_timeout(&mut host, 2), b"ok");
        assert_eq!(g.recv().1, b"6789");
    }

    #[test]
    fn one_credit_update_waits_for_a_buffer_however_much_the_guest_sends() {
        let mut g = Guest::with("one-update", &[], true, 1);
        let listener = UnixListener::bind(g.dir.join("host_7000.sock")).unwrap();
        g.send(g.packet(OP_REQUEST, 7000, 5555), &[]);
        assert_eq!(g.recv().0.op, OP_RESPONSE);
        let (mut host, _) = listener.accept().unwrap();
        let chunk = vec![7u8; 4096];
        for _ in 0..40 {
            let mut h = g.packet(OP_RW, 7000, 5555);
            h.len = chunk.len() as u32;
            g.send(h, &chunk);
        }
        let updates = {
            let inner = g.dev.inner.lock().unwrap();
            inner
                .control
                .iter()
                .filter(|c| c.op == OP_CREDIT_UPDATE)
                .count()
        };
        assert_eq!(updates, 1, "credit updates waiting");
        assert_eq!(read_exact_timeout(&mut host, 40 * 4096).len(), 40 * 4096);
        g.post_rx_quietly(4);
        let (h, _) = g.recv();
        assert_eq!((h.op, h.fwd_cnt), (OP_CREDIT_UPDATE, 40 * 4096));
        g.expect_nothing();
    }

    #[test]
    fn bytes_the_host_wrote_before_closing_reach_the_guest_though_a_write_to_it_failed() {
        let mut g = Guest::new("unread", &[1024]);
        let mut host = UnixStream::connect(guest_port_path(&g.dir, 1024)).unwrap();
        let hp = g.recv().0.src_port;
        let mut resp = g.packet(OP_RESPONSE, hp, 1024);
        resp.buf_alloc = 6;
        g.send(resp, &[]);
        let mut rw = g.packet(OP_RW, hp, 1024);
        rw.buf_alloc = 6;
        rw.len = 9;
        g.send(rw, b"stdin-eof");
        host.write_all(b"exit 0\n\n\n\n").unwrap();
        assert_eq!(g.recv().1, b"exit 0");
        drop(host);
        g.expect_nothing();
        let mut rw = g.packet(OP_RW, hp, 1024);
        rw.buf_alloc = 6;
        rw.fwd_cnt = 6;
        rw.len = 4;
        g.send(rw, b"more");
        let (h, _) = g.recv();
        assert_eq!(
            (h.op, h.flags),
            (OP_SHUTDOWN, SHUTDOWN_RCV),
            "the guest is told the Mac takes nothing more"
        );
        let (h, data) = g.recv();
        assert_eq!((h.op, &data[..]), (OP_RW, &b"\n\n\n\n"[..]));
        assert_eq!(g.recv().0.op, OP_RST);
        assert!(g.dev.inner.lock().unwrap().conns.is_empty());
    }

    #[test]
    fn guest_data_after_the_host_stopped_reading_is_dropped_not_a_reset() {
        let mut g = Guest::new("unread2", &[1024]);
        let mut host = UnixStream::connect(guest_port_path(&g.dir, 1024)).unwrap();
        let hp = g.recv().0.src_port;
        let mut resp = g.packet(OP_RESPONSE, hp, 1024);
        resp.buf_alloc = 2;
        g.send(resp, &[]);
        host.write_all(b"abcd").unwrap();
        assert_eq!(g.recv().1, b"ab");
        drop(host);
        std::thread::sleep(Duration::from_millis(30));
        for _ in 0..3 {
            let mut rw = g.packet(OP_RW, hp, 1024);
            rw.buf_alloc = 2;
            rw.len = 1;
            g.send(rw, b"x");
        }
        assert_eq!(g.recv().0.op, OP_SHUTDOWN);
        g.expect_nothing();
        {
            let inner = g.dev.inner.lock().unwrap();
            let conn = inner.conns.values().next().expect("still open");
            assert!(conn.out.is_empty());
            assert_eq!(conn.fwd_cnt.0, 3, "what was dropped counts as taken");
        }
        let mut upd = g.packet(OP_CREDIT_UPDATE, hp, 1024);
        upd.buf_alloc = 2;
        upd.fwd_cnt = 2;
        g.send(upd, &[]);
        assert_eq!(g.recv().1, b"cd");
        assert_eq!(g.recv().0.op, OP_RST);
    }

    #[test]
    fn refuses_a_socket_directory_too_long_for_macos() {
        let long = PathBuf::from(format!("/tmp/{}", "x".repeat(120)));
        let err = Vsock::new(&long, &[1]).err().unwrap();
        assert!(err.to_string().contains("shorter --vsock-dir"));
    }
}
