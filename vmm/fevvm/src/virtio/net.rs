use std::io::{self, Read};
use std::os::fd::{AsRawFd, RawFd};
use std::os::unix::net::UnixStream;
use std::sync::atomic::{AtomicBool, Ordering::SeqCst};
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;

use super::netlink::{self, Consumer, Producer, Region};
use super::queue::{Chain, Queue, QueueError};
use super::{read_config_bytes, Activation, Interrupt, VirtioDevice};
use crate::memory::GuestMemory;

const DEVICE_ID: u32 = 1;
const QUEUE_SIZES: [u16; 2] = [1024, 256];
const RX: u32 = 0;
const TX: u32 = 1;

const F_CSUM: u64 = 1 << 0;
const F_GUEST_CSUM: u64 = 1 << 1;
const F_MAC: u64 = 1 << 5;
const F_GUEST_TSO4: u64 = 1 << 7;
const F_HOST_TSO4: u64 = 1 << 11;
const F_MRG_RXBUF: u64 = 1 << 15;
const F_STATUS: u64 = 1 << 16;

pub const CONTROL_LEN: usize = 8;
pub const CONTROL_GUEST_ACCEPTS: u32 = 1;
pub const ACCEPTS_PARTIAL_CSUM: u32 = 1;
pub const ACCEPTS_TSO4: u32 = 2;

pub const NET_HDR_LEN: usize = 12;
pub const MAX_MESSAGE: usize = NET_HDR_LEN + 65550;

const S_LINK_UP: u16 = 1;

pub const GUEST_MAC: [u8; 6] = [0x02, 0x00, 0xbe, 0x27, 0x00, 0x02];

struct Link {
    stream: UnixStream,
    kq: RawFd,
    down: AtomicBool,
    stopping: AtomicBool,
    #[cfg(test)]
    rounds: std::sync::atomic::AtomicU64,
}

impl Link {
    fn is_down(&self) -> bool {
        self.down.load(SeqCst)
    }

    fn take_down(&self, why: &str) {
        if !self.down.swap(true, SeqCst) {
            crate::vmm_log!("virtio-net: link down: {why}");
        }
    }

    fn wake_netstack(&self) {
        netlink::wake(&self.stream);
    }

    fn wake_event_thread(&self) {
        let wake = libc::kevent {
            ident: 0,
            filter: libc::EVFILT_USER,
            flags: 0,
            fflags: libc::NOTE_TRIGGER,
            data: 0,
            udata: std::ptr::null_mut(),
        };
        unsafe { libc::kevent(self.kq, &wake, 1, std::ptr::null_mut(), 0, std::ptr::null()) };
    }
}

struct Active {
    mem: Arc<GuestMemory>,
    irq: Arc<Interrupt>,
    queue: Queue,
}

impl Active {
    fn finish(&self, result: Result<(), QueueError>) {
        if let Err(e) = result {
            self.irq.fail("virtio-net", &e);
        }
    }
}

struct Rx {
    active: Option<Active>,
    mergeable: bool,
    ring: Consumer,
    dropped: u64,
}

impl Rx {
    fn deliver(&mut self, link: &Link) {
        let Some(a) = self.active.as_mut() else {
            return;
        };
        let mut wake = false;
        let result = if self.mergeable {
            Self::deliver_merged(a, &mut self.ring, link, &mut wake)
        } else {
            Self::deliver_to(a, &mut self.ring, &mut self.dropped, link, &mut wake)
        };
        a.finish(result);
        if wake {
            link.wake_netstack();
        }
    }

    fn deliver_to(
        a: &mut Active,
        ring: &mut Consumer,
        dropped: &mut u64,
        link: &Link,
        wake: &mut bool,
    ) -> Result<(), QueueError> {
        while !link.is_down() {
            let len = match ring.peek() {
                Ok(Some(len)) => len,
                Ok(None) => {
                    if ring.sleep_if_empty() {
                        break;
                    }
                    continue;
                }
                Err(e) => {
                    link.take_down(e.0);
                    break;
                }
            };
            if !(NET_HDR_LEN..=MAX_MESSAGE).contains(&len) {
                link.take_down(&format!("the netstack sent a message of {len} bytes"));
                break;
            }
            let Some(chain) = a.queue.pop(&a.mem)? else {
                if a.queue.enable_notifications(&a.mem)? {
                    continue;
                }
                break;
            };
            let written = if chain.writable_len() >= len as u64 {
                copy_to_guest(ring, &a.mem, &chain, len) as u32
            } else {
                *dropped += 1;
                0
            };
            *wake |= ring.advance(len);
            a.queue.push_used(&a.mem, chain.head, written)?;
            if a.queue.needs_interrupt(&a.mem)? {
                a.irq.signal_used();
            }
        }
        Ok(())
    }

    fn deliver_merged(
        a: &mut Active,
        ring: &mut Consumer,
        link: &Link,
        wake: &mut bool,
    ) -> Result<(), QueueError> {
        let mut chains: Vec<Chain> = Vec::new();
        while !link.is_down() {
            let len = match ring.peek() {
                Ok(Some(len)) => len,
                Ok(None) => {
                    if ring.sleep_if_empty() {
                        break;
                    }
                    continue;
                }
                Err(e) => {
                    link.take_down(e.0);
                    break;
                }
            };
            if !(NET_HDR_LEN..=MAX_MESSAGE).contains(&len) {
                link.take_down(&format!("the netstack sent a message of {len} bytes"));
                break;
            }
            chains.clear();
            let mut room = 0u64;
            while room < len as u64 {
                match a.queue.pop(&a.mem)? {
                    Some(chain) => {
                        room += chain.writable_len();
                        chains.push(chain);
                    }
                    None => break,
                }
            }
            if room < len as u64 {
                let heads: Vec<u16> = chains.iter().map(|c| c.head).collect();
                a.queue.unpop(&heads);
                if a.queue.enable_notifications_past_available(&a.mem)? {
                    continue;
                }
                break;
            }
            let mut used = Vec::with_capacity(chains.len());
            let mut done = 0;
            for chain in &chains {
                let n = copy_chain_to_guest(ring, &a.mem, chain, done, len - done);
                done += n;
                used.push((chain.head, n as u32));
            }
            write_at(&a.mem, &chains[0], 10, &(chains.len() as u16).to_le_bytes());
            *wake |= ring.advance(len);
            a.queue.push_used_all(&a.mem, &used)?;
            if a.queue.needs_interrupt(&a.mem)? {
                a.irq.signal_used();
            }
        }
        Ok(())
    }

    fn discard(&mut self, link: &Link) {
        let mut wake = false;
        while let Ok(Some(len)) = self.ring.peek() {
            wake |= self.ring.advance(len);
        }
        if wake {
            link.wake_netstack();
        }
    }
}

fn copy_to_guest(ring: &Consumer, mem: &GuestMemory, chain: &Chain, len: usize) -> usize {
    copy_chain_to_guest(ring, mem, chain, 0, len)
}

fn copy_chain_to_guest(
    ring: &Consumer,
    mem: &GuestMemory,
    chain: &Chain,
    from: usize,
    len: usize,
) -> usize {
    let mut done = 0;
    for b in &chain.writable {
        if done == len {
            break;
        }
        let n = (b.len as usize).min(len - done);
        let Ok(dst) = mem.host_range(b.addr, n as u64) else {
            break;
        };
        unsafe { ring.copy_out(from + done, dst, n) };
        done += n;
    }
    done
}

fn write_at(mem: &GuestMemory, chain: &Chain, mut offset: u64, mut data: &[u8]) {
    for b in &chain.writable {
        if data.is_empty() {
            return;
        }
        if offset >= b.len as u64 {
            offset -= b.len as u64;
            continue;
        }
        let n = ((b.len as u64 - offset) as usize).min(data.len());
        if mem.write(b.addr + offset, &data[..n]).is_err() {
            return;
        }
        data = &data[n..];
        offset = 0;
    }
}

struct Tx {
    active: Option<Active>,
    ring: Producer,
    blocked: bool,
    announce: Option<u32>,
}

impl Tx {
    fn transmit(&mut self, link: &Link) {
        let mut wake = false;
        let result = self.transmit_some(link, &mut wake);
        if let Some(a) = self.active.as_ref() {
            a.finish(result);
        }
        if wake {
            link.wake_netstack();
        }
    }

    fn room(&mut self, link: &Link) -> bool {
        loop {
            match self.ring.has_room(MAX_MESSAGE) {
                Ok(true) => return true,
                Ok(false) => match self.ring.wait_for_room(MAX_MESSAGE) {
                    Ok(true) => {
                        self.blocked = true;
                        return false;
                    }
                    Ok(false) => {}
                    Err(e) => {
                        link.take_down(e.0);
                        return false;
                    }
                },
                Err(e) => {
                    link.take_down(e.0);
                    return false;
                }
            }
        }
    }

    fn transmit_some(&mut self, link: &Link, wake: &mut bool) -> Result<(), QueueError> {
        self.blocked = false;
        loop {
            let down = link.is_down();
            if !down && !self.room(link) && !link.is_down() {
                break;
            }
            let down = down || link.is_down();
            if let Some(accepts) = self.announce.take() {
                if !down {
                    let mut control = [0u8; CONTROL_LEN];
                    control[..4].copy_from_slice(&CONTROL_GUEST_ACCEPTS.to_le_bytes());
                    control[4..].copy_from_slice(&accepts.to_le_bytes());
                    self.ring.begin(CONTROL_LEN);
                    unsafe { self.ring.copy_in(0, control.as_ptr(), CONTROL_LEN) };
                    *wake |= self.ring.publish(CONTROL_LEN);
                }
                continue;
            }
            let Some(a) = self.active.as_mut() else {
                break;
            };
            let Some(chain) = a.queue.pop(&a.mem)? else {
                if a.queue.enable_notifications(&a.mem)? {
                    continue;
                }
                break;
            };
            let len = chain.readable_len();
            if !down && (NET_HDR_LEN as u64..=MAX_MESSAGE as u64).contains(&len) {
                let len = len as usize;
                self.ring.begin(len);
                if copy_from_guest(&mut self.ring, &a.mem, &chain) == len {
                    *wake |= self.ring.publish(len);
                }
            }
            a.queue.push_used(&a.mem, chain.head, 0)?;
            if a.queue.needs_interrupt(&a.mem)? {
                a.irq.signal_used();
            }
        }
        Ok(())
    }
}

fn copy_from_guest(ring: &mut Producer, mem: &GuestMemory, chain: &Chain) -> usize {
    let mut done = 0;
    for b in &chain.readable {
        let Ok(src) = mem.host_range(b.addr, b.len as u64) else {
            break;
        };
        unsafe { ring.copy_in(done, src, b.len as usize) };
        done += b.len as usize;
    }
    done
}

struct Shared {
    link: Link,
    tx: Mutex<Tx>,
    rx: Mutex<Rx>,
}

pub struct Net {
    shared: Arc<Shared>,
    thread: Option<JoinHandle<()>>,
    features: u64,
}

impl Net {
    pub fn new(stream: UnixStream) -> io::Result<Self> {
        let region = Region::create(netlink::RING_BYTES)?;
        region.announce(&stream)?;
        stream.set_nonblocking(true)?;
        let kq = unsafe { libc::kqueue() };
        if kq < 0 {
            return Err(io::Error::last_os_error());
        }
        for (ident, filter, flags) in [
            (stream.as_raw_fd() as usize, libc::EVFILT_READ, libc::EV_ADD),
            (0, libc::EVFILT_USER, libc::EV_ADD | libc::EV_CLEAR),
        ] {
            let change = libc::kevent {
                ident,
                filter,
                flags,
                fflags: 0,
                data: 0,
                udata: std::ptr::null_mut(),
            };
            unsafe { libc::kevent(kq, &change, 1, std::ptr::null_mut(), 0, std::ptr::null()) };
        }
        let shared = Arc::new(Shared {
            link: Link {
                stream,
                kq,
                down: AtomicBool::new(false),
                stopping: AtomicBool::new(false),
                #[cfg(test)]
                rounds: Default::default(),
            },
            tx: Mutex::new(Tx {
                active: None,
                ring: region.producer(netlink::GUEST_TO_NETSTACK),
                blocked: false,
                announce: None,
            }),
            rx: Mutex::new(Rx {
                active: None,
                mergeable: false,
                ring: region.consumer(netlink::NETSTACK_TO_GUEST),
                dropped: 0,
            }),
        });
        let for_thread = shared.clone();
        let thread = std::thread::Builder::new()
            .name("virtio-net".into())
            .spawn(move || run_events(&for_thread))?;
        let mut features = F_MAC | F_STATUS | F_CSUM | F_GUEST_CSUM | F_HOST_TSO4 | F_GUEST_TSO4;
        if std::env::var_os("FEV_VMM_NET_MRG").is_none_or(|v| v != "off") {
            features |= F_MRG_RXBUF;
        }
        Ok(Self {
            shared,
            thread: Some(thread),
            features,
        })
    }

    pub fn connect(path: &std::path::Path) -> io::Result<Self> {
        Self::new(UnixStream::connect(path)?)
    }

    #[cfg(any(test, feature = "fuzzing"))]
    pub fn deliver_now(&self) {
        self.shared.rx.lock().unwrap().deliver(&self.shared.link);
        self.shared.tx.lock().unwrap().transmit(&self.shared.link);
    }

    fn config(&self) -> [u8; 8] {
        let mut c = [0u8; 8];
        c[..6].copy_from_slice(&GUEST_MAC);
        let up = !self.shared.link.is_down();
        c[6..8].copy_from_slice(&(if up { S_LINK_UP } else { 0 }).to_le_bytes());
        c
    }
}

fn drain_wakeups(link: &Link) {
    let mut bytes = [0u8; 256];
    loop {
        match (&link.stream).read(&mut bytes) {
            Ok(0) => return link.take_down("the netstack closed the connection"),
            Ok(_) => {}
            Err(e) if e.kind() == io::ErrorKind::WouldBlock => return,
            Err(e) if e.kind() == io::ErrorKind::Interrupted => {}
            Err(e) => return link.take_down(&e.to_string()),
        }
    }
}

fn run_events(shared: &Shared) {
    let link = &shared.link;
    let mut events: [libc::kevent; 2] = unsafe { std::mem::zeroed() };
    loop {
        let n = unsafe {
            libc::kevent(
                link.kq,
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
        if link.stopping.load(SeqCst) {
            return;
        }
        #[cfg(test)]
        link.rounds.fetch_add(1, SeqCst);
        if events[..n as usize]
            .iter()
            .any(|ev| ev.filter == libc::EVFILT_READ)
        {
            if !link.is_down() {
                drain_wakeups(link);
            }
            if link.is_down() {
                let change = libc::kevent {
                    ident: link.stream.as_raw_fd() as usize,
                    filter: libc::EVFILT_READ,
                    flags: libc::EV_DELETE,
                    fflags: 0,
                    data: 0,
                    udata: std::ptr::null_mut(),
                };
                unsafe {
                    libc::kevent(
                        link.kq,
                        &change,
                        1,
                        std::ptr::null_mut(),
                        0,
                        std::ptr::null(),
                    )
                };
            }
        }
        shared.rx.lock().unwrap().deliver(link);
        let mut tx = shared.tx.lock().unwrap();
        if tx.blocked || link.is_down() {
            tx.transmit(link);
        }
    }
}

impl VirtioDevice for Net {
    fn device_id(&self) -> u32 {
        DEVICE_ID
    }

    fn features(&self) -> u64 {
        self.features
    }

    fn queue_max_sizes(&self) -> &[u16] {
        &QUEUE_SIZES
    }

    fn read_config(&self, offset: u64, data: &mut [u8]) {
        read_config_bytes(&self.config(), offset, data);
    }

    fn activate(&mut self, mut activation: Activation) {
        let txq = activation.queues.pop().unwrap();
        let rxq = activation.queues.pop().unwrap();
        let link = &self.shared.link;
        let mut tx = self.shared.tx.lock().unwrap();
        let mut rx = self.shared.rx.lock().unwrap();
        tx.active = Some(Active {
            mem: activation.mem.clone(),
            irq: activation.irq.clone(),
            queue: txq,
        });
        rx.active = Some(Active {
            mem: activation.mem,
            irq: activation.irq,
            queue: rxq,
        });
        rx.mergeable = activation.features & F_MRG_RXBUF != 0;
        drop(rx);
        let mut accepts = 0;
        if activation.features & F_GUEST_CSUM != 0 {
            accepts |= ACCEPTS_PARTIAL_CSUM;
        }
        if activation.features & F_GUEST_TSO4 != 0 {
            accepts |= ACCEPTS_TSO4;
        }
        tx.announce = Some(accepts);
        tx.transmit(link);
        drop(tx);
        link.wake_event_thread();
    }

    fn notify(&mut self, queue: u32) {
        let link = &self.shared.link;
        match queue {
            RX => link.wake_event_thread(),
            TX => self.shared.tx.lock().unwrap().transmit(link),
            _ => {}
        }
    }

    fn reset(&mut self) {
        let mut tx = self.shared.tx.lock().unwrap();
        let mut rx = self.shared.rx.lock().unwrap();
        tx.active = None;
        tx.announce = None;
        rx.active = None;
        rx.mergeable = false;
        rx.discard(&self.shared.link);
    }
}

impl Drop for Net {
    fn drop(&mut self) {
        self.shared.link.stopping.store(true, SeqCst);
        self.shared.link.wake_event_thread();
        if let Some(t) = self.thread.take() {
            let _ = t.join();
        }
        unsafe { libc::close(self.shared.link.kq) };
    }
}

#[cfg(test)]
mod tests {
    use super::super::netlink::Peer;
    use super::super::queue::testing::{memory, Driver};
    use super::super::testing::interrupt;
    use super::*;
    use std::time::{Duration, Instant};

    const BASE: u64 = 0x4000_0000;
    const RX_BUFS: u64 = BASE + 0x10000;
    const TX_BUFS: u64 = BASE + 0x80000;
    const RX_BUF_LEN: u32 = 2048;

    struct Fixture {
        mem: Arc<GuestMemory>,
        rx: Driver,
        tx: Driver,
        dev: Net,
        host: Peer,
        next_rx: u64,
    }

    fn fixture() -> Fixture {
        fixture_with(0)
    }

    fn fixture_with(features: u64) -> Fixture {
        let (ours, host) = UnixStream::pair().unwrap();
        host.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
        let mem = Arc::new(memory());
        let (rx, rxq) = Driver::new(&mem, BASE, 16);
        let (tx, txq) = Driver::new(&mem, BASE + 0x2000, 16);
        let (_chip, irq) = interrupt();
        let mut dev = Net::new(ours).unwrap();
        let host = Peer::accept(host).unwrap();
        dev.activate(Activation {
            mem: mem.clone(),
            irq,
            queues: vec![rxq, txq],
            features,
        });
        Fixture {
            mem,
            rx,
            tx,
            dev,
            host,
            next_rx: 0,
        }
    }

    fn message(frame: &[u8]) -> Vec<u8> {
        let mut m = vec![0u8; NET_HDR_LEN];
        m.extend(frame);
        m
    }

    impl Fixture {
        fn post_rx(&mut self, n: usize) {
            for _ in 0..n {
                let addr = RX_BUFS + (self.next_rx % 64) * RX_BUF_LEN as u64;
                self.next_rx += 1;
                self.rx.add(&self.mem, &[(addr, RX_BUF_LEN, true)]);
            }
            self.dev.notify(RX);
        }

        fn guest_recv(&mut self) -> Vec<u8> {
            let deadline = Instant::now() + Duration::from_secs(5);
            loop {
                if let Some((head, written)) = self.rx.take_used(&self.mem) {
                    let addr = self.mem.read_u64(self.rx.desc + head as u64 * 16).unwrap();
                    let mut data = vec![0u8; written as usize];
                    self.mem.read(addr, &mut data).unwrap();
                    return data.split_off(NET_HDR_LEN.min(data.len()));
                }
                assert!(Instant::now() < deadline, "no frame reached the guest");
                std::thread::sleep(Duration::from_millis(1));
            }
        }

        fn guest_send(&mut self, slot: u64, frame: &[u8]) {
            let addr = TX_BUFS + slot * 0x1000;
            self.mem.write(addr, &[0u8; NET_HDR_LEN]).unwrap();
            self.mem.write(addr + NET_HDR_LEN as u64, frame).unwrap();
            self.tx.add(
                &self.mem,
                &[
                    (addr, NET_HDR_LEN as u32, false),
                    (addr + NET_HDR_LEN as u64, frame.len() as u32, false),
                ],
            );
        }

        fn host_recv(&mut self) -> Vec<u8> {
            let mut body = self.host.recv();
            if body.len() == CONTROL_LEN {
                return self.host_recv();
            }
            assert_eq!(&body[..NET_HDR_LEN], &[0u8; NET_HDR_LEN]);
            body.split_off(NET_HDR_LEN)
        }
    }

    fn frame(seed: u8, len: usize) -> Vec<u8> {
        (0..len).map(|i| seed.wrapping_add(i as u8)).collect()
    }

    #[test]
    fn frames_flow_from_guest_to_host_in_order() {
        let mut f = fixture();
        for i in 0..5u8 {
            f.guest_send(i as u64, &frame(i, 60 + i as usize * 300));
        }
        f.dev.notify(TX);
        for i in 0..5u8 {
            assert_eq!(f.host_recv(), frame(i, 60 + i as usize * 300));
            assert!(f.tx.take_used(&f.mem).is_some());
        }
    }

    #[test]
    fn frames_flow_from_host_to_guest_in_order() {
        let mut f = fixture();
        f.post_rx(8);
        for i in 0..5u8 {
            f.host.send(&message(&frame(i, 64 + i as usize * 200)));
        }
        for i in 0..5u8 {
            assert_eq!(f.guest_recv(), frame(i, 64 + i as usize * 200));
        }
    }

    #[test]
    fn frames_survive_the_ring_wrapping_many_times() {
        let mut f = fixture();
        let count = 3 * netlink::RING_BYTES / 2000;
        for i in 0..count {
            f.post_rx(1);
            f.host.send(&message(&frame(i as u8, 1900 + i % 90)));
            assert!(
                f.guest_recv() == frame(i as u8, 1900 + i % 90),
                "frame {i} arrived wrong"
            );
        }
    }

    #[test]
    fn frames_wait_for_guest_buffers_and_none_are_lost() {
        let mut f = fixture();
        let count = 40u8;
        for i in 0..count {
            f.host.send(&message(&frame(i, 1200)));
        }
        std::thread::sleep(Duration::from_millis(30));
        assert_eq!(f.rx.take_used(&f.mem), None);
        for i in 0..count {
            f.post_rx(1);
            assert_eq!(f.guest_recv(), frame(i, 1200), "frame {i}");
        }
        assert_eq!(f.dev.shared.rx.lock().unwrap().dropped, 0);
    }

    #[test]
    fn a_frame_too_big_for_the_guests_buffer_is_dropped_not_truncated() {
        let mut f = fixture();
        f.post_rx(2);
        f.host.send(&message(&frame(1, 3000)));
        f.host.send(&message(&frame(2, 100)));
        assert_eq!(f.guest_recv(), Vec::<u8>::new());
        assert_eq!(f.guest_recv(), frame(2, 100));
        assert_eq!(f.dev.shared.rx.lock().unwrap().dropped, 1);
    }

    #[test]
    fn a_slow_netstack_pushes_back_and_everything_arrives() {
        let mut f = fixture();
        const IN_FLIGHT: usize = 6;
        let total = 3 * netlink::RING_BYTES / 1400;
        let mut sent = 0usize;
        let mut received = Vec::new();
        while received.len() < total {
            while sent < total && sent - f.completed() < IN_FLIGHT {
                f.guest_send((sent % 16) as u64, &frame(sent as u8, 1400));
                sent += 1;
            }
            f.dev.notify(TX);
            if sent - f.completed() >= IN_FLIGHT || sent == total {
                received.push(f.host_recv());
            }
        }
        for (i, got) in received.iter().enumerate() {
            assert!(
                got == &frame(i as u8, 1400),
                "frame {i} arrived wrong or out of order"
            );
        }
    }

    impl Fixture {
        fn completed(&mut self) -> usize {
            thread_local!(static DONE: std::cell::Cell<usize> = const { std::cell::Cell::new(0) });
            while self.tx.take_used(&self.mem).is_some() {
                DONE.with(|d| d.set(d.get() + 1));
            }
            DONE.with(|d| d.get())
        }
    }

    impl Fixture {
        fn wait_for_link_down(&mut self, what: &str) {
            let deadline = Instant::now() + Duration::from_secs(5);
            while self.dev.config()[6] != 0 {
                assert!(Instant::now() < deadline, "link still up after {what}");
                std::thread::sleep(Duration::from_millis(1));
            }
            self.guest_send(0, &frame(1, 100));
            self.dev.notify(TX);
            assert!(self.tx.take_used(&self.mem).is_some());
        }
    }

    #[test]
    fn a_malformed_length_from_the_netstack_takes_the_link_down() {
        for bad in [0usize, 5, MAX_MESSAGE + 1] {
            let mut f = fixture();
            f.post_rx(2);
            assert_eq!(f.dev.config()[6], 1, "link starts up");
            f.host.send(&vec![0u8; bad]);
            f.wait_for_link_down(&format!("a message of {bad} bytes"));
        }
    }

    #[test]
    fn a_netstack_that_corrupts_the_shared_memory_takes_the_link_down() {
        let mut f = fixture();
        f.post_rx(2);
        f.host.scribble(1024 + 128, &u64::MAX.to_le_bytes());
        netlink::wake(&f.host.stream);
        f.wait_for_link_down("an impossible index");

        for _ in 0..8 {
            netlink::wake(&f.host.stream);
        }
        std::thread::sleep(Duration::from_millis(50));
        let rounds = f.dev.shared.link.rounds.load(SeqCst);
        std::thread::sleep(Duration::from_millis(100));
        assert_eq!(f.dev.shared.link.rounds.load(SeqCst), rounds);

        let mut f = fixture();
        f.host.scribble(0, &(1u64 << 50).to_le_bytes());
        f.guest_send(1, &frame(2, 100));
        f.dev.notify(TX);
        f.wait_for_link_down("an impossible index");
    }

    #[test]
    fn the_netstack_going_away_takes_the_link_down() {
        let mut f = fixture();
        f.post_rx(2);
        f.host.stream.shutdown(std::net::Shutdown::Both).unwrap();
        f.wait_for_link_down("the netstack closing");
    }

    #[test]
    fn guest_frames_of_impossible_size_are_discarded() {
        let mut f = fixture();
        f.tx.add(&f.mem, &[(TX_BUFS, 4, false)]);
        f.guest_send(1, &frame(7, 80));
        f.dev.notify(TX);
        assert_eq!(f.host_recv(), frame(7, 80));
        assert_eq!(&f.dev.config()[..6], &GUEST_MAC);
    }

    #[test]
    fn a_frame_spans_mergeable_buffers_and_says_how_many() {
        let mut f = fixture_with(F_MRG_RXBUF);
        assert_ne!(f.dev.features() & F_MRG_RXBUF, 0);
        let big = frame(7, 5000);
        f.post_rx(1);
        f.host.send(&message(&big));
        std::thread::sleep(Duration::from_millis(100));
        assert!(
            f.rx.take_used(&f.mem).is_none(),
            "a frame of three buffers' size went into one"
        );
        f.post_rx(2);
        let deadline = Instant::now() + Duration::from_secs(5);
        let mut got = Vec::new();
        let mut buffers = None;
        while got.len() < NET_HDR_LEN + big.len() {
            assert!(
                Instant::now() < deadline,
                "the frame never reached the guest"
            );
            let Some((head, written)) = f.rx.take_used(&f.mem) else {
                std::thread::sleep(Duration::from_millis(1));
                continue;
            };
            let addr = f.mem.read_u64(f.rx.desc + head as u64 * 16).unwrap();
            let mut data = vec![0u8; written as usize];
            f.mem.read(addr, &mut data).unwrap();
            if buffers.is_none() {
                buffers = Some(u16::from_le_bytes([data[10], data[11]]));
            }
            got.extend(data);
        }
        assert_eq!(buffers, Some(3));
        assert_eq!(&got[NET_HDR_LEN..], &big[..]);
        f.post_rx(1);
        f.host.send(&message(&frame(9, 100)));
        let small = f.guest_recv();
        assert_eq!(small, frame(9, 100));
    }

    fn activated_with(features: u64) -> (Net, Peer) {
        let (ours, host) = UnixStream::pair().unwrap();
        host.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
        let mem = Arc::new(memory());
        let (_, rxq) = Driver::new(&mem, BASE, 16);
        let (_, txq) = Driver::new(&mem, BASE + 0x2000, 16);
        let (_chip, irq) = interrupt();
        let mut dev = Net::new(ours).unwrap();
        let host = Peer::accept(host).unwrap();
        dev.activate(Activation {
            mem,
            irq,
            queues: vec![rxq, txq],
            features,
        });
        (dev, host)
    }

    #[test]
    fn the_netstack_is_told_which_offloads_the_driver_accepts() {
        for (features, want) in [
            (0, 0),
            (F_GUEST_CSUM, ACCEPTS_PARTIAL_CSUM),
            (
                F_GUEST_CSUM | F_GUEST_TSO4 | F_CSUM | F_HOST_TSO4,
                ACCEPTS_PARTIAL_CSUM | ACCEPTS_TSO4,
            ),
            (F_CSUM | F_HOST_TSO4, 0),
        ] {
            let (dev, mut host) = activated_with(features);
            let msg = host.recv();
            assert_eq!(msg.len(), CONTROL_LEN);
            assert_eq!(
                u32::from_le_bytes(msg[0..4].try_into().unwrap()),
                CONTROL_GUEST_ACCEPTS
            );
            assert_eq!(
                u32::from_le_bytes(msg[4..8].try_into().unwrap()),
                want,
                "features {features:#x}"
            );
            assert_eq!(
                dev.features() & (F_CSUM | F_GUEST_CSUM | F_HOST_TSO4 | F_GUEST_TSO4),
                F_CSUM | F_GUEST_CSUM | F_HOST_TSO4 | F_GUEST_TSO4
            );
        }
    }

    #[test]
    fn the_offload_header_and_large_segments_pass_through_untouched() {
        let mut f = fixture();
        let mut hdr = [0u8; NET_HDR_LEN];
        hdr[0] = 1;
        hdr[1] = 1;
        hdr[2..4].copy_from_slice(&54u16.to_le_bytes());
        hdr[4..6].copy_from_slice(&1448u16.to_le_bytes());
        let big = frame(3, 40_000);
        f.mem.write(TX_BUFS, &hdr).unwrap();
        f.mem.write(TX_BUFS + 0x100, &big).unwrap();
        f.tx.add(
            &f.mem,
            &[
                (TX_BUFS, NET_HDR_LEN as u32, false),
                (TX_BUFS + 0x100, big.len() as u32, false),
            ],
        );
        f.dev.notify(TX);
        let mut body;
        loop {
            body = f.host.recv();
            if body.len() != CONTROL_LEN {
                break;
            }
        }
        assert!(
            body[..NET_HDR_LEN] == hdr && body[NET_HDR_LEN..] == big[..],
            "segment or header changed"
        );

        let bufs: Vec<(u64, u32, bool)> = (0..12)
            .map(|i| (RX_BUFS + i * 0x1000, 0x1000, true))
            .collect();
        f.rx.add(&f.mem, &bufs);
        f.dev.notify(RX);
        let mut msg = hdr.to_vec();
        msg.extend(&big);
        f.host.send(&msg);
        let deadline = Instant::now() + Duration::from_secs(5);
        let written = loop {
            if let Some((_, written)) = f.rx.take_used(&f.mem) {
                break written as usize;
            }
            assert!(
                Instant::now() < deadline,
                "the segment did not reach the guest"
            );
            std::thread::sleep(Duration::from_millis(1));
        };
        assert_eq!(written, NET_HDR_LEN + big.len());
        let mut got = vec![0u8; written];
        f.mem.read(RX_BUFS, &mut got).unwrap();
        assert!(
            got[..NET_HDR_LEN] == hdr && got[NET_HDR_LEN..] == big[..],
            "segment or header changed on the way in"
        );
    }

    #[test]
    fn reset_forgets_frames_in_flight() {
        let mut f = fixture();
        f.host.send(&message(&frame(1, 500)));
        std::thread::sleep(Duration::from_millis(20));
        f.dev.reset();
        let mut rx = f.dev.shared.rx.lock().unwrap();
        assert!(rx.active.is_none() && rx.ring.peek() == Ok(None));
    }

    #[test]
    fn a_vcpu_sending_is_not_held_up_by_frames_being_received() {
        let mut f = fixture();
        let shared = f.dev.shared.clone();
        let _receiving = shared.rx.lock().unwrap();
        f.guest_send(0, &frame(4, 300));
        f.dev.notify(TX);
        f.post_rx(1);
        assert_eq!(f.host.recv().len(), CONTROL_LEN);
        assert_eq!(f.host.recv()[NET_HDR_LEN..], frame(4, 300));
    }
}
