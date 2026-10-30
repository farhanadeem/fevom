use std::io::{self, Write};
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::os::unix::net::UnixStream;
use std::sync::atomic::{AtomicU32, AtomicU64, Ordering::SeqCst};
use std::sync::Arc;

pub const MAGIC: [u8; 8] = *b"BRTHNET2";
pub const HELLO_LEN: usize = 16;
pub const HEADER_LEN: usize = 16 << 10;
pub const RING_BYTES: usize = 1 << 20;

pub const GUEST_TO_NETSTACK: usize = 0;
pub const NETSTACK_TO_GUEST: usize = 1;

const RING_HEADER_STRIDE: usize = 1024;
const HEAD: usize = 0;
const TAIL: usize = 128;
const CONSUMER_WAITING: usize = 256;
const PRODUCER_WAITING: usize = 384;

const LEN_PREFIX: usize = 4;

fn span(len: usize) -> u64 {
    ((LEN_PREFIX + len) as u64).next_multiple_of(8)
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Corrupt(pub &'static str);

pub struct Region {
    base: *mut u8,
    len: usize,
    capacity: usize,
    fd: OwnedFd,
}

unsafe impl Send for Region {}
unsafe impl Sync for Region {}

impl Region {
    pub fn create(capacity: usize) -> io::Result<Arc<Self>> {
        assert!(capacity.is_power_of_two());
        static COUNTER: AtomicU32 = AtomicU32::new(0);
        let fd = loop {
            let name = format!(
                "/fev-net-{}-{}\0",
                std::process::id(),
                COUNTER.fetch_add(1, SeqCst)
            );
            let name = name.as_ptr().cast::<libc::c_char>();
            let fd = unsafe {
                libc::shm_open(
                    name,
                    libc::O_CREAT | libc::O_EXCL | libc::O_RDWR,
                    0o600 as libc::c_uint,
                )
            };
            if fd >= 0 {
                unsafe { libc::shm_unlink(name) };
                break fd;
            }
            let e = io::Error::last_os_error();
            if e.kind() != io::ErrorKind::AlreadyExists {
                return Err(e);
            }
        };
        let fd = unsafe { OwnedFd::from_raw_fd(fd) };
        let len = HEADER_LEN + 2 * capacity;
        if unsafe { libc::ftruncate(fd.as_raw_fd(), len as libc::off_t) } != 0 {
            return Err(io::Error::last_os_error());
        }
        Self::map(fd, capacity)
    }

    pub fn open(fd: OwnedFd, capacity: usize) -> io::Result<Arc<Self>> {
        let bad = |why| io::Error::new(io::ErrorKind::InvalidData, why);
        if !capacity.is_power_of_two() || !(1 << 16..=1 << 26).contains(&capacity) {
            return Err(bad("ring capacity out of range"));
        }
        let mut st: libc::stat = unsafe { std::mem::zeroed() };
        if unsafe { libc::fstat(fd.as_raw_fd(), &mut st) } != 0 {
            return Err(io::Error::last_os_error());
        }
        if (st.st_size as u64) < (HEADER_LEN + 2 * capacity) as u64 {
            return Err(bad("shared region smaller than its rings"));
        }
        Self::map(fd, capacity)
    }

    fn map(fd: OwnedFd, capacity: usize) -> io::Result<Arc<Self>> {
        let len = HEADER_LEN + 2 * capacity;
        let base = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                len,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_SHARED,
                fd.as_raw_fd(),
                0,
            )
        };
        if base == libc::MAP_FAILED {
            return Err(io::Error::last_os_error());
        }
        Ok(Arc::new(Self {
            base: base.cast(),
            len,
            capacity,
            fd,
        }))
    }

    pub fn capacity(&self) -> usize {
        self.capacity
    }

    fn ring(self: &Arc<Self>, index: usize) -> Ring {
        assert!(index < 2);
        Ring {
            header: unsafe { self.base.add(index * RING_HEADER_STRIDE) },
            data: unsafe { self.base.add(HEADER_LEN + index * self.capacity) },
            capacity: self.capacity as u64,
            _region: self.clone(),
        }
    }

    pub fn producer(self: &Arc<Self>, index: usize) -> Producer {
        Producer {
            ring: self.ring(index),
            tail: 0,
            waiting: false,
        }
    }

    pub fn consumer(self: &Arc<Self>, index: usize) -> Consumer {
        Consumer {
            ring: self.ring(index),
            head: 0,
            waiting: false,
        }
    }

    pub fn announce(&self, stream: &UnixStream) -> io::Result<()> {
        let mut hello = [0u8; HELLO_LEN];
        hello[..8].copy_from_slice(&MAGIC);
        hello[8..12].copy_from_slice(&(self.capacity as u32).to_le_bytes());
        let mut iov = libc::iovec {
            iov_base: hello.as_mut_ptr().cast(),
            iov_len: HELLO_LEN,
        };
        let mut control = [0u64; 4];
        let space = unsafe { libc::CMSG_SPACE(size_of::<RawFd>() as u32) } as usize;
        assert!(space <= size_of_val(&control));
        let mut msg: libc::msghdr = unsafe { std::mem::zeroed() };
        msg.msg_iov = &mut iov;
        msg.msg_iovlen = 1;
        msg.msg_control = control.as_mut_ptr().cast();
        msg.msg_controllen = space as _;
        unsafe {
            let cmsg = libc::CMSG_FIRSTHDR(&msg);
            (*cmsg).cmsg_level = libc::SOL_SOCKET;
            (*cmsg).cmsg_type = libc::SCM_RIGHTS;
            (*cmsg).cmsg_len = libc::CMSG_LEN(size_of::<RawFd>() as u32) as _;
            libc::CMSG_DATA(cmsg)
                .cast::<RawFd>()
                .write_unaligned(self.fd.as_raw_fd());
        }
        loop {
            let n = unsafe { libc::sendmsg(stream.as_raw_fd(), &msg, 0) };
            if n == HELLO_LEN as isize {
                return Ok(());
            }
            let e = io::Error::last_os_error();
            if n >= 0 {
                return Err(io::Error::other("short hello"));
            }
            if e.kind() != io::ErrorKind::Interrupted {
                return Err(e);
            }
        }
    }
}

impl Drop for Region {
    fn drop(&mut self) {
        unsafe { libc::munmap(self.base.cast(), self.len) };
    }
}

struct Ring {
    header: *mut u8,
    data: *mut u8,
    capacity: u64,
    _region: Arc<Region>,
}

unsafe impl Send for Ring {}

impl Ring {
    fn u64_at(&self, offset: usize) -> &AtomicU64 {
        unsafe { &*self.header.add(offset).cast::<AtomicU64>() }
    }

    fn u32_at(&self, offset: usize) -> &AtomicU32 {
        unsafe { &*self.header.add(offset).cast::<AtomicU32>() }
    }

    unsafe fn copy_out(&self, position: u64, dst: *mut u8, n: usize) {
        let offset = (position & (self.capacity - 1)) as usize;
        let first = n.min(self.capacity as usize - offset);
        std::ptr::copy_nonoverlapping(self.data.add(offset), dst, first);
        std::ptr::copy_nonoverlapping(self.data, dst.add(first), n - first);
    }

    unsafe fn copy_in(&self, position: u64, src: *const u8, n: usize) {
        let offset = (position & (self.capacity - 1)) as usize;
        let first = n.min(self.capacity as usize - offset);
        std::ptr::copy_nonoverlapping(src, self.data.add(offset), first);
        std::ptr::copy_nonoverlapping(src.add(first), self.data, n - first);
    }
}

pub struct Consumer {
    ring: Ring,
    head: u64,
    waiting: bool,
}

impl Consumer {
    pub fn peek(&mut self) -> Result<Option<usize>, Corrupt> {
        if self.waiting {
            self.waiting = false;
            self.ring.u32_at(CONSUMER_WAITING).store(0, SeqCst);
        }
        let available = self.ring.u64_at(TAIL).load(SeqCst).wrapping_sub(self.head);
        if available == 0 {
            return Ok(None);
        }
        if available > self.ring.capacity || !available.is_multiple_of(8) {
            return Err(Corrupt("the producer's index is impossible"));
        }
        let mut prefix = [0u8; LEN_PREFIX];
        unsafe {
            self.ring
                .copy_out(self.head, prefix.as_mut_ptr(), LEN_PREFIX)
        };
        let len = u32::from_le_bytes(prefix) as usize;
        if len as u64 > available || span(len) > available {
            return Err(Corrupt("a message is longer than what was produced"));
        }
        Ok(Some(len))
    }

    pub unsafe fn copy_out(&self, offset: usize, dst: *mut u8, n: usize) {
        self.ring
            .copy_out(self.head + (LEN_PREFIX + offset) as u64, dst, n)
    }

    pub fn read(&self, len: usize) -> Vec<u8> {
        let mut out = vec![0u8; len];
        unsafe { self.copy_out(0, out.as_mut_ptr(), len) };
        out
    }

    #[must_use]
    pub fn advance(&mut self, len: usize) -> bool {
        self.head = self.head.wrapping_add(span(len));
        self.ring.u64_at(HEAD).store(self.head, SeqCst);
        self.ring.u32_at(PRODUCER_WAITING).swap(0, SeqCst) == 1
    }

    pub fn sleep_if_empty(&mut self) -> bool {
        self.ring.u32_at(CONSUMER_WAITING).store(1, SeqCst);
        if self.ring.u64_at(TAIL).load(SeqCst) == self.head {
            self.waiting = true;
            return true;
        }
        self.ring.u32_at(CONSUMER_WAITING).store(0, SeqCst);
        false
    }
}

pub struct Producer {
    ring: Ring,
    tail: u64,
    waiting: bool,
}

impl Producer {
    fn room(&self, len: usize) -> Result<bool, Corrupt> {
        let used = self.tail.wrapping_sub(self.ring.u64_at(HEAD).load(SeqCst));
        if used > self.ring.capacity {
            return Err(Corrupt("the consumer's index is impossible"));
        }
        Ok(self.ring.capacity - used >= span(len))
    }

    pub fn has_room(&mut self, len: usize) -> Result<bool, Corrupt> {
        if self.waiting {
            self.waiting = false;
            self.ring.u32_at(PRODUCER_WAITING).store(0, SeqCst);
        }
        self.room(len)
    }

    pub fn wait_for_room(&mut self, len: usize) -> Result<bool, Corrupt> {
        self.ring.u32_at(PRODUCER_WAITING).store(1, SeqCst);
        if !self.room(len)? {
            self.waiting = true;
            return Ok(true);
        }
        self.ring.u32_at(PRODUCER_WAITING).store(0, SeqCst);
        Ok(false)
    }

    pub fn begin(&mut self, len: usize) {
        let prefix = (len as u32).to_le_bytes();
        unsafe { self.ring.copy_in(self.tail, prefix.as_ptr(), LEN_PREFIX) };
    }

    pub unsafe fn copy_in(&mut self, offset: usize, src: *const u8, n: usize) {
        self.ring
            .copy_in(self.tail + (LEN_PREFIX + offset) as u64, src, n)
    }

    #[must_use]
    pub fn publish(&mut self, len: usize) -> bool {
        self.tail = self.tail.wrapping_add(span(len));
        self.ring.u64_at(TAIL).store(self.tail, SeqCst);
        self.ring.u32_at(CONSUMER_WAITING).swap(0, SeqCst) == 1
    }

    pub fn put(&mut self, message: &[u8]) -> Result<Option<bool>, Corrupt> {
        if !self.has_room(message.len())? {
            return Ok(None);
        }
        self.begin(message.len());
        unsafe { self.copy_in(0, message.as_ptr(), message.len()) };
        Ok(Some(self.publish(message.len())))
    }
}

pub fn wake(mut stream: &UnixStream) {
    let _ = stream.write(&[1]);
}

#[cfg(any(test, feature = "fuzzing"))]
pub struct Peer {
    pub stream: UnixStream,
    pub region: Arc<Region>,
    pub from_guest: Consumer,
    pub to_guest: Producer,
}

#[cfg(any(test, feature = "fuzzing"))]
impl Peer {
    pub fn accept(stream: UnixStream) -> io::Result<Self> {
        let mut hello = [0u8; HELLO_LEN];
        let mut iov = libc::iovec {
            iov_base: hello.as_mut_ptr().cast(),
            iov_len: HELLO_LEN,
        };
        let mut control = [0u64; 4];
        let mut msg: libc::msghdr = unsafe { std::mem::zeroed() };
        msg.msg_iov = &mut iov;
        msg.msg_iovlen = 1;
        msg.msg_control = control.as_mut_ptr().cast();
        msg.msg_controllen = size_of_val(&control) as _;
        let n = unsafe { libc::recvmsg(stream.as_raw_fd(), &mut msg, 0) };
        if n != HELLO_LEN as isize || hello[..8] != MAGIC {
            return Err(io::Error::other("no hello"));
        }
        let cmsg = unsafe { libc::CMSG_FIRSTHDR(&msg) };
        if cmsg.is_null() || unsafe { (*cmsg).cmsg_type } != libc::SCM_RIGHTS {
            return Err(io::Error::other("no descriptor with the hello"));
        }
        let fd = unsafe { libc::CMSG_DATA(cmsg).cast::<RawFd>().read_unaligned() };
        let fd = unsafe { OwnedFd::from_raw_fd(fd) };
        let capacity = u32::from_le_bytes(hello[8..12].try_into().unwrap()) as usize;
        let region = Region::open(fd, capacity)?;
        Ok(Self {
            from_guest: region.consumer(GUEST_TO_NETSTACK),
            to_guest: region.producer(NETSTACK_TO_GUEST),
            region,
            stream,
        })
    }

    fn wait(&mut self) -> bool {
        use std::io::Read;
        let mut byte = [0u8; 64];
        matches!(self.stream.read(&mut byte), Ok(n) if n > 0)
    }

    pub fn send(&mut self, message: &[u8]) {
        loop {
            match self.to_guest.put(message).expect("ring corrupt") {
                Some(true) => return wake(&self.stream),
                Some(false) => return,
                None => {}
            }
            if self.to_guest.wait_for_room(message.len()).unwrap() {
                assert!(self.wait(), "the device never made room");
            }
        }
    }

    pub fn try_recv(&mut self) -> Option<Vec<u8>> {
        let len = self.from_guest.peek().expect("ring corrupt")?;
        let message = self.from_guest.read(len);
        if self.from_guest.advance(len) {
            wake(&self.stream);
        }
        Some(message)
    }

    pub fn recv(&mut self) -> Vec<u8> {
        loop {
            if let Some(message) = self.try_recv() {
                return message;
            }
            if self.from_guest.sleep_if_empty() {
                assert!(self.wait(), "nothing came from the guest");
            }
        }
    }

    pub fn scribble(&self, offset: usize, bytes: &[u8]) {
        let offset = offset % self.region.len;
        let n = bytes.len().min(self.region.len - offset);
        unsafe { std::ptr::copy_nonoverlapping(bytes.as_ptr(), self.region.base.add(offset), n) };
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn pair(capacity: usize) -> (Producer, Consumer) {
        let region = Region::create(capacity).unwrap();
        (region.producer(0), region.consumer(0))
    }

    #[test]
    fn messages_come_out_as_they_went_in_across_the_wrap() {
        let (mut p, mut c) = pair(1 << 16);
        for round in 0..2000usize {
            let message: Vec<u8> = (0..(round * 37) % 5000)
                .map(|i| (i + round) as u8)
                .collect();
            assert_eq!(p.put(&message), Ok(Some(false)));
            let len = c.peek().unwrap().unwrap();
            assert_eq!(c.read(len), message, "round {round}");
            assert!(!c.advance(len));
        }
        assert_eq!(c.peek(), Ok(None));
    }

    #[test]
    fn a_full_ring_refuses_and_says_when_to_wake_the_producer() {
        let (mut p, mut c) = pair(1 << 16);
        let message = [7u8; 4092];
        let mut held = 0;
        while p.put(&message).unwrap().is_some() {
            held += 1;
        }
        assert_eq!(held, 16);
        assert_eq!(p.wait_for_room(message.len()), Ok(true));
        let len = c.peek().unwrap().unwrap();
        assert!(c.advance(len), "the consumer must wake the producer");
        assert_eq!(p.has_room(message.len()), Ok(true));
        let len = c.peek().unwrap().unwrap();
        assert!(!c.advance(len));
    }

    #[test]
    fn an_empty_ring_says_when_to_wake_the_consumer() {
        let (mut p, mut c) = pair(1 << 16);
        assert_eq!(c.peek(), Ok(None));
        assert!(c.sleep_if_empty());
        assert_eq!(p.put(b"one"), Ok(Some(true)), "the consumer was asleep");
        assert_eq!(p.put(b"two"), Ok(Some(false)), "one wake-up is enough");
        assert_eq!(c.peek(), Ok(Some(3)));
        let _ = c.advance(3);
        let _ = c.advance(3);
        assert_eq!(p.put(b"three"), Ok(Some(false)));
        assert!(!c.sleep_if_empty());
    }

    #[test]
    fn impossible_indices_and_lengths_are_reported_not_believed() {
        let region = Region::create(1 << 16).unwrap();
        let (mut p, mut c) = (region.producer(0), region.consumer(0));
        let ring = region.ring(0);
        ring.u64_at(TAIL).store((1 << 16) + 8, SeqCst);
        assert!(c.peek().is_err());
        ring.u64_at(TAIL).store(13, SeqCst);
        assert!(c.peek().is_err());
        ring.u64_at(TAIL).store(16, SeqCst);
        unsafe { ring.copy_in(0, 4000u32.to_le_bytes().as_ptr(), 4) };
        assert!(c.peek().is_err());
        unsafe { ring.copy_in(0, u32::MAX.to_le_bytes().as_ptr(), 4) };
        assert!(c.peek().is_err());
        ring.u64_at(HEAD).store(1 << 40, SeqCst);
        assert!(p.has_room(8).is_err());
    }

    #[test]
    fn the_region_reaches_another_holder_of_the_socket() {
        let (ours, theirs) = UnixStream::pair().unwrap();
        let region = Region::create(1 << 16).unwrap();
        region.announce(&ours).unwrap();
        let mut device_end = region.producer(GUEST_TO_NETSTACK);
        let mut peer = Peer::accept(theirs).unwrap();
        assert_eq!(peer.region.capacity(), 1 << 16);
        assert_eq!(device_end.put(b"through shared memory"), Ok(Some(false)));
        assert_eq!(peer.try_recv().unwrap(), b"through shared memory");
        let small = Region::create(1 << 16).unwrap();
        let fd = small.fd.try_clone().unwrap();
        assert!(Region::open(fd, 1 << 20).is_err());
    }
}
