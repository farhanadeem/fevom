use std::collections::VecDeque;
use std::fs::File;
use std::io;
use std::os::fd::{AsRawFd, RawFd};
use std::os::unix::fs::FileExt;
use std::sync::atomic::{AtomicU32, AtomicU64, Ordering};
use std::sync::{Arc, Condvar, Mutex};
use std::thread::JoinHandle;

use super::queue::{Buf, Chain, Queue};
use super::{read_config_bytes, Activation, VirtioDevice};
use crate::config::SyncMode;
use crate::irq::Sender;
use crate::memory::GuestMemory;
use crate::space::{no_space, Space};

const DEVICE_ID: u32 = 2;
const QUEUE_SIZE: u16 = 128;
pub const MAX_QUEUES: u16 = 8;
const SEG_MAX: u32 = 254;
const SECTOR: u64 = 512;

const F_SEG_MAX: u64 = 1 << 2;
const F_RO: u64 = 1 << 5;
const F_BLK_SIZE: u64 = 1 << 6;
const F_FLUSH: u64 = 1 << 9;
const F_TOPOLOGY: u64 = 1 << 10;
const F_MQ: u64 = 1 << 12;
const F_DISCARD: u64 = 1 << 13;
const F_WRITE_ZEROES: u64 = 1 << 14;
pub const F_FEV_WINDOW: u64 = 1 << 23;

const T_IN: u32 = 0;
const T_OUT: u32 = 1;
const T_FLUSH: u32 = 4;
const T_GET_ID: u32 = 8;
const T_DISCARD: u32 = 11;
const T_WRITE_ZEROES: u32 = 13;

const S_OK: u8 = 0;
const S_IOERR: u8 = 1;
const S_UNSUPP: u8 = 2;

const HEADER_LEN: u64 = 16;
const SEGMENT_LEN: usize = 16;
const ID_LEN: usize = 20;
const DISK_ID: &[u8] = b"fev";
const MAX_TRIM_SECTORS: u32 = 1 << 23;
const MAX_TRIM_SEGMENTS: u32 = 64;

fn unsupported(err: &io::Error) -> bool {
    !matches!(
        err.raw_os_error(),
        Some(libc::EIO | libc::ENOSPC | libc::EDQUOT)
    )
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Flushed {
    Fsync = 0,
    Barrier = 1,
    Full = 2,
}

fn sync_fd_how(fd: RawFd, mode: SyncMode) -> io::Result<Flushed> {
    let fcntl = |cmd| loop {
        if unsafe { libc::fcntl(fd, cmd) } != -1 {
            return Ok(true);
        }
        let err = io::Error::last_os_error();
        if err.kind() == io::ErrorKind::Interrupted {
            continue;
        }
        return if unsupported(&err) {
            Ok(false)
        } else {
            Err(err)
        };
    };
    match mode {
        SyncMode::Fsync => {}
        SyncMode::Barrier => {
            if fcntl(libc::F_BARRIERFSYNC)? {
                return Ok(Flushed::Barrier);
            }
            if fcntl(libc::F_FULLFSYNC)? {
                return Ok(Flushed::Full);
            }
        }
        SyncMode::Full => {
            if fcntl(libc::F_FULLFSYNC)? {
                return Ok(Flushed::Full);
            }
        }
        SyncMode::Paced => {
            if fcntl(libc::F_FULLFSYNC)? {
                return Ok(Flushed::Full);
            }
            if fcntl(libc::F_BARRIERFSYNC)? {
                return Ok(Flushed::Barrier);
            }
        }
    }
    loop {
        if unsafe { libc::fsync(fd) } == 0 {
            return Ok(Flushed::Fsync);
        }
        let err = io::Error::last_os_error();
        if err.kind() != io::ErrorKind::Interrupted {
            return Err(err);
        }
    }
}

pub struct FlushStats {
    by_call: [AtomicU64; 3],
    failed: AtomicU64,
    total_us: AtomicU64,
    max_us: AtomicU64,
    buckets: [AtomicU64; FLUSH_BUCKETS],
    written: AtomicU64,
    writes: AtomicU64,
    trims: AtomicU64,
    written_total: AtomicU64,
    writes_total: AtomicU64,
    trims_total: AtomicU64,
    last_end_ns: AtomicU64,
    last_full_ns: AtomicU64,
    by_rule: AtomicU64,
    opened: std::time::Instant,
    log_above: std::time::Duration,
    pushes: AtomicU64,
    push_us: AtomicU64,
    push_max_us: AtomicU64,
    push_failed: AtomicU64,
    push_error: std::sync::atomic::AtomicBool,
    push_no_space: std::sync::atomic::AtomicBool,
    pub no_space: AtomicU64,
}

const FLUSH_BUCKETS: usize = 24;

const FLUSH_LOG_ENV: &str = "FEV_VMM_FLUSH_LOG_MS";

impl Default for FlushStats {
    fn default() -> Self {
        let ms = std::env::var(FLUSH_LOG_ENV)
            .ok()
            .and_then(|v| v.parse().ok())
            .unwrap_or(100);
        Self {
            by_call: Default::default(),
            failed: Default::default(),
            total_us: Default::default(),
            max_us: Default::default(),
            buckets: Default::default(),
            written: Default::default(),
            writes: Default::default(),
            trims: Default::default(),
            written_total: Default::default(),
            writes_total: Default::default(),
            trims_total: Default::default(),
            last_end_ns: Default::default(),
            last_full_ns: Default::default(),
            by_rule: Default::default(),
            opened: std::time::Instant::now(),
            log_above: std::time::Duration::from_millis(ms),
            pushes: Default::default(),
            push_us: Default::default(),
            push_max_us: Default::default(),
            push_failed: Default::default(),
            push_error: Default::default(),
            push_no_space: Default::default(),
            no_space: AtomicU64::new(0),
        }
    }
}

impl FlushStats {
    fn note(&self, how: io::Result<Flushed>, took: std::time::Duration) {
        match how {
            Ok(how) => self.by_call[how as usize].fetch_add(1, Ordering::Relaxed),
            Err(_) => self.failed.fetch_add(1, Ordering::Relaxed),
        };
        let us = took.as_micros().min(u64::MAX as u128) as u64;
        self.total_us.fetch_add(us, Ordering::Relaxed);
        self.max_us.fetch_max(us, Ordering::Relaxed);
        let bucket = (64 - us.leading_zeros() as usize).min(FLUSH_BUCKETS - 1);
        self.buckets[bucket].fetch_add(1, Ordering::Relaxed);
    }

    fn wrote(&self, len: u64) -> u64 {
        self.written.fetch_add(len, Ordering::Relaxed);
        self.writes.fetch_add(1, Ordering::Relaxed);
        self.writes_total.fetch_add(1, Ordering::Relaxed);
        self.written_total.fetch_add(len, Ordering::Relaxed)
    }

    fn trimmed(&self) {
        self.trims.fetch_add(1, Ordering::Relaxed);
        self.trims_total.fetch_add(1, Ordering::Relaxed);
    }

    fn pushed(&self, took: std::time::Duration, ok: bool) {
        let us = took.as_micros().min(u64::MAX as u128) as u64;
        self.pushes.fetch_add(1, Ordering::Relaxed);
        self.push_us.fetch_add(us, Ordering::Relaxed);
        self.push_max_us.fetch_max(us, Ordering::Relaxed);
        if !ok {
            self.push_failed.fetch_add(1, Ordering::Relaxed);
            self.push_error.store(true, Ordering::Release);
        }
    }

    fn push_refused(&self, took: std::time::Duration) {
        let us = took.as_micros().min(u64::MAX as u128) as u64;
        self.pushes.fetch_add(1, Ordering::Relaxed);
        self.push_us.fetch_add(us, Ordering::Relaxed);
        self.push_max_us.fetch_max(us, Ordering::Relaxed);
        self.push_failed.fetch_add(1, Ordering::Relaxed);
        self.push_no_space.store(true, Ordering::Release);
    }

    pub fn pushes(&self) -> u64 {
        self.pushes.load(Ordering::Relaxed)
    }

    fn take_full(&self, now_ns: u64, every: std::time::Duration) -> bool {
        let now_ns = now_ns.max(1);
        let last = self.last_full_ns.load(Ordering::Relaxed);
        if last != 0 && now_ns.saturating_sub(last) < every.as_nanos() as u64 {
            return false;
        }
        let taken = self
            .last_full_ns
            .compare_exchange(last, now_ns, Ordering::Relaxed, Ordering::Relaxed)
            .is_ok();
        if taken {
            self.by_rule.fetch_add(1, Ordering::Relaxed);
        }
        taken
    }

    pub fn by_rule(&self) -> u64 {
        self.by_rule.load(Ordering::Relaxed)
    }

    pub fn by_call(&self, how: Flushed) -> u64 {
        self.by_call[how as usize].load(Ordering::Relaxed)
    }

    pub fn count(&self) -> u64 {
        self.by_call
            .iter()
            .map(|c| c.load(Ordering::Relaxed))
            .sum::<u64>()
            + self.failed.load(Ordering::Relaxed)
    }

    pub fn written(&self) -> u64 {
        self.written_total.load(Ordering::Relaxed)
    }

    pub fn report(&self) -> String {
        let load = |a: &AtomicU64| a.load(Ordering::Relaxed);
        let n = self.count();
        let mut hist = String::new();
        for (i, b) in self.buckets.iter().enumerate() {
            let c = load(b);
            if c > 0 {
                let under = 1u64 << i;
                let label = if i == FLUSH_BUCKETS - 1 {
                    format!(">={}ms", (under >> 1) / 1000)
                } else if under >= 1024 {
                    format!("<{}ms", under as f64 / 1000.0)
                } else {
                    format!("<{under}us")
                };
                hist.push_str(&format!(" {label}:{c}"));
            }
        }
        let pushes = load(&self.pushes);
        format!(
            "{n} flushes (fsync {}, barrier {}, full {}, failed {}, by the rule {}), mean {} us, max {} us;{hist}; written {} MiB in {} requests, {} discards; {pushes} pushes, mean {} us, max {} us, failed {}",
            load(&self.by_call[0]),
            load(&self.by_call[1]),
            load(&self.by_call[2]),
            load(&self.failed),
            load(&self.by_rule),
            load(&self.total_us).checked_div(n).unwrap_or(0),
            load(&self.max_us),
            load(&self.written_total) >> 20,
            load(&self.writes_total),
            load(&self.trims_total),
            load(&self.push_us).checked_div(pushes).unwrap_or(0),
            load(&self.push_max_us),
            load(&self.push_failed),
        )
    }
}

pub struct Writeback {
    since: AtomicU64,
    every: u64,
    count: WritebackCount,
    stream: u64,
    epoch: AtomicU32,
    counted: Box<[AtomicU32]>,
    signal: (Mutex<Signal>, Condvar),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum WritebackCount {
    Places,
    Written,
}

const WRITEBACK_ENV: &str = "FEV_VMM_WRITEBACK_MIB";
const WRITEBACK_COUNT_ENV: &str = "FEV_VMM_WRITEBACK_COUNT";
const WRITEBACK_EVERY: u64 = 32 << 20;
const WRITEBACK_STREAM: u64 = 1 << 20;
const WRITEBACK_STREAM_ENV: &str = "FEV_VMM_WRITEBACK_STREAM_KIB";
const WRITEBACK_CHUNK: u64 = 256 << 10;

const FULL_EVERY: std::time::Duration = std::time::Duration::from_secs(1);

fn sleep_proof_ns() -> u64 {
    let mut ts = libc::timespec {
        tv_sec: 0,
        tv_nsec: 0,
    };
    unsafe { libc::clock_gettime(libc::CLOCK_MONOTONIC, &mut ts) };
    ts.tv_sec as u64 * 1_000_000_000 + ts.tv_nsec as u64
}

impl Writeback {
    fn new(every: u64, count: WritebackCount, len: u64) -> Self {
        let chunks = if every == 0 || count == WritebackCount::Written {
            0
        } else {
            len.div_ceil(WRITEBACK_CHUNK) as usize
        };
        let zeroed = vec![0u32; chunks].into_boxed_slice();
        let counted = unsafe { Box::from_raw(Box::into_raw(zeroed) as *mut [AtomicU32]) };
        Self {
            since: AtomicU64::new(0),
            every,
            count,
            stream: WRITEBACK_STREAM,
            epoch: AtomicU32::new(1),
            counted,
            signal: (Mutex::new(Signal::default()), Condvar::new()),
        }
    }

    fn from_env(len: u64) -> Self {
        let every = std::env::var(WRITEBACK_ENV)
            .ok()
            .and_then(|v| v.parse::<u64>().ok())
            .map_or(WRITEBACK_EVERY, |mib| mib << 20);
        let count = match std::env::var(WRITEBACK_COUNT_ENV).as_deref() {
            Ok("written") => WritebackCount::Written,
            _ => WritebackCount::Places,
        };
        let mut w = Self::new(every, count, len);
        if let Some(kib) = std::env::var(WRITEBACK_STREAM_ENV)
            .ok()
            .and_then(|v| v.parse::<u64>().ok())
        {
            w.stream = kib << 10;
        }
        w
    }

    fn wrote(&self, offset: u64, len: u64) {
        if self.every == 0 || len == 0 {
            return;
        }
        let new = match self.count {
            WritebackCount::Written => len,
            WritebackCount::Places => {
                if self.stream != 0 && len >= self.stream {
                    return;
                }
                let epoch = self.epoch.load(Ordering::Relaxed);
                let first = (offset / WRITEBACK_CHUNK) as usize;
                let last = ((offset + len - 1) / WRITEBACK_CHUNK) as usize;
                let mut new = 0;
                for chunk in self.counted.iter().take(last + 1).skip(first) {
                    if chunk.load(Ordering::Relaxed) != epoch
                        && chunk.swap(epoch, Ordering::Relaxed) != epoch
                    {
                        new += WRITEBACK_CHUNK;
                    }
                }
                if new == 0 {
                    return;
                }
                new
            }
        };
        let seen = self.since.fetch_add(new, Ordering::Relaxed) + new;
        if seen >= self.every
            && self
                .since
                .compare_exchange(seen, 0, Ordering::Relaxed, Ordering::Relaxed)
                .is_ok()
        {
            self.signal.0.lock().unwrap().kicked = true;
            self.signal.1.notify_one();
        }
    }

    fn flushed(&self) {
        self.since.store(0, Ordering::Relaxed);
        self.epoch.fetch_add(1, Ordering::Relaxed);
    }
}

pub struct Disk {
    file: File,
    name: String,
    sectors: u64,
    read_only: bool,
    hole_align: u64,
    sync_mode: SyncMode,
    full_every: std::time::Duration,
    flushes: Arc<FlushStats>,
    writeback: Arc<Writeback>,
    space: Arc<Space>,
}

pub struct ImageWindow {
    host: *mut u8,
    len: u64,
}

unsafe impl Send for ImageWindow {}
unsafe impl Sync for ImageWindow {}

impl ImageWindow {
    pub fn host(&self) -> *mut u8 {
        self.host
    }

    pub fn len(&self) -> u64 {
        self.len
    }

    pub fn is_empty(&self) -> bool {
        self.len == 0
    }

    pub fn resident_pages(&self) -> io::Result<u64> {
        let mut vec = vec![0 as libc::c_char; (self.len / crate::layout::HOST_PAGE) as usize];
        if unsafe { libc::mincore(self.host.cast(), self.len as usize, vec.as_mut_ptr()) } != 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(vec
            .iter()
            .filter(|b| **b as i32 & libc::MINCORE_INCORE != 0)
            .count() as u64)
    }
}

impl Drop for ImageWindow {
    fn drop(&mut self) {
        unsafe { libc::munmap(self.host.cast(), self.len as usize) };
    }
}

impl Disk {
    pub fn map_window(&self, len: u64) -> io::Result<ImageWindow> {
        if self.read_only {
            return Err(io::Error::other("a read-only disk has no window"));
        }
        if len == 0 || !len.is_multiple_of(crate::layout::HOST_PAGE) || len > self.sectors * SECTOR
        {
            return Err(io::Error::other("not a length of this image"));
        }
        let host = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                len as usize,
                libc::PROT_READ,
                libc::MAP_SHARED,
                self.file.as_raw_fd(),
                0,
            )
        };
        if host == libc::MAP_FAILED {
            return Err(io::Error::last_os_error());
        }
        Ok(ImageWindow {
            host: host.cast(),
            len,
        })
    }

    pub fn len(&self) -> u64 {
        self.sectors * SECTOR
    }

    pub fn is_empty(&self) -> bool {
        self.sectors == 0
    }

    pub fn open(path: &std::path::Path, read_only: bool) -> io::Result<Self> {
        let file = File::options().read(true).write(!read_only).open(path)?;
        let meta = file.metadata()?;
        if !meta.is_file() {
            return Err(io::Error::other("not a regular file"));
        }
        let hole_align = std::os::unix::fs::MetadataExt::blksize(&meta).max(SECTOR);
        let space = if read_only {
            Space::off()
        } else {
            Space::new(
                crate::space::Params::from_env(),
                Box::new(crate::space::ImageVolume(file.try_clone()?)),
            )
        };
        Ok(Self {
            file,
            name: path
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or_default(),
            sectors: meta.len() / SECTOR,
            read_only,
            hole_align,
            sync_mode: SyncMode::default(),
            full_every: FULL_EVERY,
            flushes: Arc::default(),
            writeback: Arc::new(Writeback::from_env(meta.len())),
            space: Arc::new(space),
        })
    }

    pub fn with_full_every(mut self, every: std::time::Duration) -> Self {
        self.full_every = every;
        self
    }

    pub fn with_writeback(self, bytes: u64) -> Self {
        self.with_writeback_by(bytes, WritebackCount::Places)
    }

    pub fn with_writeback_by(mut self, bytes: u64, count: WritebackCount) -> Self {
        self.writeback = Arc::new(Writeback::new(bytes, count, self.len()));
        self
    }

    pub fn with_writeback_stream(mut self, bytes: u64, stream: u64) -> Self {
        let mut w = Writeback::new(bytes, WritebackCount::Places, self.len());
        w.stream = stream;
        self.writeback = Arc::new(w);
        self
    }

    pub fn start_writeback(self: &Arc<Self>) -> Option<JoinHandle<()>> {
        if self.read_only || self.writeback.every == 0 {
            return None;
        }
        {
            let mut s = self.writeback.signal.0.lock().unwrap();
            s.stop = false;
            s.kicked = false;
        }
        let disk = self.clone();
        std::thread::Builder::new()
            .name("virtio-blk-writeback".into())
            .spawn(move || run_writeback(disk))
            .ok()
    }

    pub fn stop_writeback(&self, thread: JoinHandle<()>) {
        self.writeback.signal.0.lock().unwrap().stop = true;
        self.writeback.signal.1.notify_one();
        let _ = thread.join();
    }

    fn wrote(&self, offset: u64, len: u64) {
        let before = self.flushes.wrote(len);
        self.writeback.wrote(offset, len);
        if self.space.crossed(before, len) {
            self.space.check();
        }
    }

    pub fn space(&self) -> &Arc<Space> {
        &self.space
    }

    pub fn with_space(mut self, space: Arc<Space>) -> Self {
        self.space = space;
        self
    }

    pub fn with_sync(mut self, mode: SyncMode) -> Self {
        self.sync_mode = mode;
        self
    }

    pub fn open_shared(other: &Disk) -> io::Result<Self> {
        Ok(Self {
            file: other.file.try_clone()?,
            name: other.name.clone(),
            sectors: other.sectors,
            read_only: other.read_only,
            hole_align: other.hole_align,
            sync_mode: other.sync_mode,
            full_every: other.full_every,
            flushes: other.flushes.clone(),
            writeback: other.writeback.clone(),
            space: other.space.clone(),
        })
    }

    pub fn sync(&self) -> io::Result<()> {
        let st = &*self.flushes;
        let written = st.written.swap(0, Ordering::Relaxed);
        let writes = st.writes.swap(0, Ordering::Relaxed);
        let trims = st.trims.swap(0, Ordering::Relaxed);
        self.writeback.flushed();
        let start = std::time::Instant::now();
        let depth = match self.sync_mode {
            SyncMode::Paced if st.take_full(sleep_proof_ns(), self.full_every) => SyncMode::Paced,
            SyncMode::Paced => SyncMode::Barrier,
            mode => mode,
        };
        let how = sync_fd_how(self.file.as_raw_fd(), depth);
        let how = match how {
            Ok(_) if st.push_error.swap(false, Ordering::AcqRel) => Err(io::Error::other(
                "an early writeback of the image failed since the last flush",
            )),
            Ok(_) if st.push_no_space.swap(false, Ordering::AcqRel) => {
                Err(io::Error::from_raw_os_error(libc::ENOSPC))
            }
            how => {
                st.push_error.store(false, Ordering::Release);
                how
            }
        };
        let took = start.elapsed();
        let done = how
            .as_ref()
            .map(|_| ())
            .map_err(|e| match e.raw_os_error() {
                Some(n) if no_space(n) => io::Error::from_raw_os_error(n),
                _ => io::Error::new(e.kind(), e.to_string()),
            });
        let call = match &how {
            Ok(call) => format!("{call:?}").to_lowercase(),
            Err(e) => format!("failed: {e}"),
        };
        st.note(how, took);
        let end_ns = (start + took).duration_since(st.opened).as_nanos() as u64;
        let last_end_ns = st.last_end_ns.swap(end_ns, Ordering::Relaxed);
        if took >= st.log_above {
            crate::vmm_log!(
                "{}: flush took {} ms ({call}) at {:.3} s, {:.1} MiB in {writes} writes and {trims} discards since the last flush, which ended {} ms before this one began",
                self.name,
                took.as_millis(),
                end_ns as f64 / 1e9,
                written as f64 / (1 << 20) as f64,
                (end_ns - took.as_nanos() as u64).saturating_sub(last_end_ns) / 1_000_000,
            );
        }
        done
    }

    pub fn flushes(&self) -> &Arc<FlushStats> {
        &self.flushes
    }

    pub fn is_read_only(&self) -> bool {
        self.read_only
    }

    fn span(&self, sector: u64, count: u64) -> Option<(u64, u64)> {
        let end = sector.checked_add(count)?;
        (end <= self.sectors).then(|| (sector * SECTOR, count * SECTOR))
    }

    fn zero_fill(&self, mut offset: u64, mut len: u64) -> io::Result<()> {
        let zeros = [0u8; 64 << 10];
        while len > 0 {
            let n = len.min(zeros.len() as u64) as usize;
            self.file.write_all_at(&zeros[..n], offset)?;
            offset += n as u64;
            len -= n as u64;
        }
        Ok(())
    }

    fn punch(&self, offset: u64, len: u64) -> io::Result<()> {
        let arg = libc::fpunchhole_t {
            fp_flags: 0,
            reserved: 0,
            fp_offset: offset as libc::off_t,
            fp_length: len as libc::off_t,
        };
        let ret = unsafe { libc::fcntl(self.file.as_raw_fd(), libc::F_PUNCHHOLE, &arg) };
        if ret == -1 {
            return Err(io::Error::last_os_error());
        }
        Ok(())
    }

    fn trim(&self, offset: u64, len: u64, must_zero: bool) -> io::Result<()> {
        let end = offset + len;
        let hole_start = offset.next_multiple_of(self.hole_align);
        let hole_end = end / self.hole_align * self.hole_align;
        if hole_start >= hole_end {
            return if must_zero {
                self.zero_fill(offset, len)
            } else {
                Ok(())
            };
        }
        match self.punch(hole_start, hole_end - hole_start) {
            Ok(()) => {}
            Err(e) if e.raw_os_error() == Some(libc::ENOTSUP) => {
                return if must_zero {
                    self.zero_fill(offset, len)
                } else {
                    Ok(())
                };
            }
            Err(e) => return Err(e),
        }
        if must_zero {
            self.zero_fill(offset, hole_start - offset)?;
            self.zero_fill(hole_end, end - hole_end)?;
        }
        Ok(())
    }
}

fn slice_bufs(bufs: &[Buf], mut skip: u64, mut len: u64) -> Vec<Buf> {
    let mut out = Vec::new();
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
        out.push(Buf {
            addr: b.addr.wrapping_add(skip),
            len: take as u32,
        });
        len -= take;
        skip = 0;
    }
    out
}

fn transfer(
    disk: &Disk,
    mem: &GuestMemory,
    bufs: &[Buf],
    mut offset: u64,
    write: bool,
) -> Result<(), i32> {
    for b in bufs {
        let len = b.len as u64;
        if write {
            #[cfg(test)]
            if disk.space.refusing() {
                return Err(libc::ENOSPC);
            }
            mem.write_to_file_or(&disk.file, offset, b.addr, len)
                .map_err(|_| 0)??;
        } else if mem.read_from_file(&disk.file, offset, b.addr, len) != Ok(true) {
            return Err(0);
        }
        offset += len;
    }
    Ok(())
}

fn trim_request(disk: &Disk, mem: &GuestMemory, payload: &[Buf], write_zeroes: bool) -> u8 {
    let total: u64 = payload.iter().map(|b| b.len as u64).sum();
    if total == 0
        || !total.is_multiple_of(SEGMENT_LEN as u64)
        || total / SEGMENT_LEN as u64 > MAX_TRIM_SEGMENTS as u64
    {
        return S_IOERR;
    }
    let chain = Chain {
        readable: payload.to_vec(),
        ..Chain::default()
    };
    let mut raw = vec![0u8; total as usize];
    if !chain.read_exact(mem, &mut raw) {
        return S_IOERR;
    }
    for seg in raw.as_chunks::<SEGMENT_LEN>().0 {
        let sector = u64::from_le_bytes(seg[0..8].try_into().unwrap());
        let count = u32::from_le_bytes(seg[8..12].try_into().unwrap());
        let flags = u32::from_le_bytes(seg[12..16].try_into().unwrap());
        if flags & !(write_zeroes as u32) != 0 {
            return S_UNSUPP;
        }
        if count > MAX_TRIM_SECTORS {
            return S_IOERR;
        }
        let Some((offset, len)) = disk.span(sector, count as u64) else {
            return S_IOERR;
        };
        if disk.trim(offset, len, write_zeroes).is_err() {
            return S_IOERR;
        }
        disk.flushes.trimmed();
        disk.space.discarded(len);
    }
    disk.space.room();
    S_OK
}

type Header = [u8; HEADER_LEN as usize];

fn read_header(mem: &GuestMemory, chain: &Chain) -> Option<Header> {
    let mut header = [0u8; HEADER_LEN as usize];
    chain.read_exact(mem, &mut header).then_some(header)
}

pub fn handle(disk: &Disk, mem: &GuestMemory, chain: &Chain) -> u32 {
    serve(disk, mem, chain, read_header(mem, chain).as_ref(), false).unwrap_or(0)
}

#[derive(Debug)]
struct NoSpace;

fn serve(
    disk: &Disk,
    mem: &GuestMemory,
    chain: &Chain,
    header: Option<&Header>,
    hold: bool,
) -> Result<u32, NoSpace> {
    let writable_len = chain.writable_len();
    if writable_len == 0 {
        return Ok(0);
    }
    let data_in = slice_bufs(&chain.writable, 0, writable_len - 1);
    let data_in_len = writable_len - 1;
    let status_buf = slice_bufs(&chain.writable, writable_len - 1, 1);

    let mut written = 0u64;
    let status = if let Some(header) = header {
        let kind = u32::from_le_bytes(header[0..4].try_into().unwrap());
        let sector = u64::from_le_bytes(header[8..16].try_into().unwrap());
        let data_out_len = chain.readable_len() - HEADER_LEN;
        let data_out = slice_bufs(&chain.readable, HEADER_LEN, data_out_len);
        match kind {
            T_IN => match disk.span(sector, data_in_len / SECTOR) {
                Some((offset, _)) if data_in_len.is_multiple_of(SECTOR) => {
                    if transfer(disk, mem, &data_in, offset, false).is_ok() {
                        written = data_in_len;
                        S_OK
                    } else {
                        S_IOERR
                    }
                }
                _ => S_IOERR,
            },
            T_OUT if disk.read_only => S_IOERR,
            T_OUT => match disk.span(sector, data_out_len / SECTOR) {
                Some((offset, _)) if data_out_len.is_multiple_of(SECTOR) => {
                    match transfer(disk, mem, &data_out, offset, true) {
                        Ok(()) => {
                            disk.wrote(offset, data_out_len);
                            S_OK
                        }
                        Err(errno) if hold && no_space(errno) => return Err(NoSpace),
                        Err(errno) => {
                            if no_space(errno) {
                                disk.flushes.no_space.fetch_add(1, Ordering::Relaxed);
                            }
                            S_IOERR
                        }
                    }
                }
                _ => S_IOERR,
            },
            T_FLUSH => match disk.sync() {
                Ok(()) => S_OK,
                Err(e) if hold && e.raw_os_error().is_some_and(no_space) => return Err(NoSpace),
                Err(e) => {
                    if e.raw_os_error().is_some_and(no_space) {
                        disk.flushes.no_space.fetch_add(1, Ordering::Relaxed);
                    }
                    S_IOERR
                }
            },
            T_GET_ID => {
                let mut id = [0u8; ID_LEN];
                id[..DISK_ID.len()].copy_from_slice(DISK_ID);
                let dst = Chain {
                    writable: data_in.clone(),
                    ..Chain::default()
                };
                written = dst.write(mem, &id) as u64;
                S_OK
            }
            T_DISCARD | T_WRITE_ZEROES if disk.read_only => S_IOERR,
            T_DISCARD => trim_request(disk, mem, &data_out, false),
            T_WRITE_ZEROES => trim_request(disk, mem, &data_out, true),
            _ => S_UNSUPP,
        }
    } else {
        S_IOERR
    };

    Ok(match status_buf.first() {
        Some(b) if mem.write(b.addr, &[status]).is_ok() => (written + 1) as u32,
        _ => 0,
    })
}

#[derive(Default)]
struct Signal {
    kicked: bool,
    stop: bool,
    room: bool,
}

const INLINE_MAX: u64 = 64 << 10;
const SLOW: std::time::Duration = std::time::Duration::from_micros(100);
const QUICK_AGAIN: u32 = 8;
const INLINE_ENV: &str = "FEV_VMM_INLINE";
const INLINE_MAX_ENV: &str = "FEV_VMM_INLINE_KIB";

fn inline_max_from_env() -> u64 {
    if std::env::var_os(INLINE_ENV).is_some_and(|v| v == "off") {
        return 0;
    }
    std::env::var(INLINE_MAX_ENV)
        .ok()
        .and_then(|v| v.parse::<u64>().ok())
        .map_or(INLINE_MAX, |kib| kib << 10)
}

struct Lane {
    state: Mutex<LaneState>,
    signal: (Mutex<Signal>, Condvar),
    sender: Sender,
    slow: AtomicU32,
}

struct LaneState {
    q: Queue,
    live: bool,
    handed: VecDeque<Chain>,
    held: VecDeque<Chain>,
}

struct Lanes {
    disk: Arc<Disk>,
    mem: Arc<GuestMemory>,
    irq: Arc<super::Interrupt>,
    lanes: Vec<Lane>,
    inline_max: u64,
    slow_after: std::time::Duration,
    #[cfg(test)]
    slipped: Mutex<Option<Box<dyn FnMut() + Send>>>,
    #[cfg(test)]
    stall: Mutex<Option<Box<dyn FnMut() + Send>>>,
    #[cfg(test)]
    turns: AtomicU64,
}

impl Lanes {
    fn small(&self, chain: &Chain, header: Option<&Header>) -> bool {
        let Some(header) = header else { return false };
        let len = match u32::from_le_bytes(header[0..4].try_into().unwrap()) {
            T_IN => chain.writable_len(),
            T_OUT if !self.disk.read_only => chain.readable_len(),
            _ => return false,
        };
        len <= self.inline_max.saturating_add(HEADER_LEN)
    }

    fn kick(&self, lane: &Lane) {
        crate::stage::mark(crate::stage::Stage::Kick);
        lane.signal.0.lock().unwrap().kicked = true;
        lane.signal.1.notify_one();
    }

    #[allow(clippy::too_many_arguments)]
    fn complete(
        &self,
        lane: &Lane,
        st: &mut LaneState,
        chain: &Chain,
        header: Option<&Header>,
        small: bool,
        by: Option<u32>,
        worker: bool,
    ) -> Result<bool, super::queue::QueueError> {
        use crate::stage::{mark, Stage};
        let start = small.then(std::time::Instant::now);
        let hold = self.disk.space.params.hold;
        let written = if worker {
            serve(&self.disk, &self.mem, chain, header, hold)
        } else {
            crate::pause::away(|| {
                #[cfg(test)]
                if let Some(stall) = self.stall.lock().unwrap().as_mut() {
                    stall();
                }
                serve(&self.disk, &self.mem, chain, header, hold)
            })
        };
        let Ok(written) = written else {
            return Ok(false);
        };
        let took = start.map(|s| s.elapsed());
        mark(Stage::Serve);
        st.q.push_used(&self.mem, chain.head, written)?;
        mark(Stage::Used);
        if st.q.needs_interrupt(&self.mem)? {
            self.irq.signal_used_for(by);
        }
        mark(Stage::Raise);
        if let Some(took) = took {
            if took > self.slow_after {
                lane.slow.store(QUICK_AGAIN, Ordering::Relaxed);
            } else if worker {
                let n = lane.slow.load(Ordering::Relaxed);
                if n != 0 {
                    lane.slow.store(n - 1, Ordering::Relaxed);
                }
            }
        }
        Ok(true)
    }
}

impl super::Inline for Lanes {
    fn notified(&self, queue: u32) {
        use crate::stage::{mark, Stage};
        let Some(lane) = self.lanes.get(queue as usize) else {
            return;
        };
        lane.sender.note();
        if lane.slow.load(Ordering::Relaxed) != 0 || self.disk.space.held() != 0 {
            return self.kick(lane);
        }
        let Ok(mut st) = lane.state.try_lock() else {
            return self.kick(lane);
        };
        if !st.live {
            return;
        }
        if !st.handed.is_empty() {
            drop(st);
            return self.kick(lane);
        }
        mark(Stage::Kick);
        let result = (|| {
            let Some(chain) = st.q.pop(&self.mem)? else {
                return st.q.enable_notifications(&self.mem);
            };
            mark(Stage::Pop);
            let header = read_header(&self.mem, &chain);
            if !self.small(&chain, header.as_ref()) || st.q.has_available(&self.mem)? {
                crate::stage::inline(false);
                st.handed.push_back(chain);
                return Ok(true);
            }
            crate::stage::inline(true);
            let by = crate::irq::current_vcpu();
            if !self.complete(lane, &mut st, &chain, header.as_ref(), true, by, false)? {
                st.handed.push_back(chain);
                return Ok(true);
            }
            #[cfg(test)]
            if let Some(slip) = self.slipped.lock().unwrap().as_mut() {
                slip();
            }
            st.q.enable_notifications(&self.mem)
        })();
        drop(st);
        match result {
            Ok(false) => {}
            Ok(true) => self.kick(lane),
            Err(e) => self.irq.fail("virtio-blk", &e),
        }
    }
}

pub struct Block {
    disk: Arc<Disk>,
    queue_sizes: Vec<u16>,
    lanes: Option<Arc<Lanes>>,
    workers: Vec<JoinHandle<()>>,
    writeback: Option<JoinHandle<()>>,
    window: Option<(u64, u64)>,
    inline_max: u64,
}

impl Block {
    pub fn new(disk: Arc<Disk>, queues: u16) -> Self {
        Self {
            disk,
            queue_sizes: vec![QUEUE_SIZE; queues.clamp(1, MAX_QUEUES) as usize],
            lanes: None,
            workers: Vec::new(),
            writeback: None,
            window: None,
            inline_max: inline_max_from_env(),
        }
    }

    pub fn with_inline(mut self, bytes: u64) -> Self {
        self.inline_max = bytes;
        self
    }

    pub fn with_window(mut self, base: u64, len: u64) -> Self {
        if !self.disk.read_only && len != 0 && len <= self.disk.len() {
            self.window = Some((base, len));
        }
        self
    }

    fn lanes_for(&self, activation: Activation) -> Arc<Lanes> {
        Arc::new(Lanes {
            disk: self.disk.clone(),
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
                        held: VecDeque::new(),
                    }),
                    signal: (Mutex::new(Signal::default()), Condvar::new()),
                    sender: Sender::default(),
                    slow: AtomicU32::new(0),
                })
                .collect(),
            inline_max: self.inline_max,
            slow_after: SLOW,
            #[cfg(test)]
            slipped: Mutex::new(None),
            #[cfg(test)]
            turns: AtomicU64::new(0),
            #[cfg(test)]
            stall: Mutex::new(None),
        })
    }

    fn config(&self) -> [u8; 60] {
        let mut c = [0u8; 60];
        c[0..8].copy_from_slice(&self.disk.sectors.to_le_bytes());
        c[12..16].copy_from_slice(&SEG_MAX.to_le_bytes());
        c[20..24].copy_from_slice(&(SECTOR as u32).to_le_bytes());
        c[24] = (self.disk.hole_align / SECTOR).trailing_zeros() as u8;
        c[34..36].copy_from_slice(&(self.queue_sizes.len() as u16).to_le_bytes());
        let align = (self.disk.hole_align / SECTOR) as u32;
        c[36..40].copy_from_slice(&MAX_TRIM_SECTORS.to_le_bytes());
        c[40..44].copy_from_slice(&MAX_TRIM_SEGMENTS.to_le_bytes());
        c[44..48].copy_from_slice(&align.to_le_bytes());
        c[48..52].copy_from_slice(&MAX_TRIM_SECTORS.to_le_bytes());
        c[52..56].copy_from_slice(&MAX_TRIM_SEGMENTS.to_le_bytes());
        c[56] = 1;
        c
    }
}

impl Lanes {
    fn drain(&self, n: usize, retry: bool) -> Result<bool, super::queue::QueueError> {
        let lane = &self.lanes[n];
        let mut st = lane.state.lock().unwrap();
        if !st.live {
            return Ok(false);
        }
        if retry {
            let mut done = 0;
            while let Some(chain) = st.held.pop_front() {
                let header = read_header(&self.mem, &chain);
                self.disk.space.retried();
                let by = lane.sender.get();
                if self.complete(lane, &mut st, &chain, header.as_ref(), false, by, true)? {
                    done += 1;
                } else {
                    st.held.push_front(chain);
                    break;
                }
            }
            self.disk.space.let_go(done, true);
        }
        loop {
            loop {
                let chain = match st.handed.pop_front() {
                    Some(chain) => chain,
                    None => match st.q.pop(&self.mem)? {
                        Some(chain) => chain,
                        None => break,
                    },
                };
                crate::stage::mark(crate::stage::Stage::Pop);
                let header = read_header(&self.mem, &chain);
                let small = self.small(&chain, header.as_ref());
                let by = lane.sender.get();
                if !self.complete(lane, &mut st, &chain, header.as_ref(), small, by, true)? {
                    st.held.push_back(chain);
                    self.disk.space.keep();
                }
            }
            if !st.q.enable_notifications(&self.mem)? {
                return Ok(!st.held.is_empty());
            }
        }
    }
}

fn run_worker(lanes: Arc<Lanes>, n: usize) {
    use std::time::Instant;
    let (lock, cv) = &lanes.lanes[n].signal;
    let params = lanes.disk.space.params;
    let mut wait = params.backoff_first;
    let mut next_try: Option<Instant> = None;
    loop {
        let retry;
        {
            let mut s = lock.lock().unwrap();
            let mut due = false;
            while !s.kicked && !s.stop && !s.room {
                match next_try {
                    Some(at) => {
                        let now = Instant::now();
                        if now >= at {
                            due = true;
                            break;
                        }
                        s = cv.wait_timeout(s, at - now).unwrap().0;
                    }
                    None => s = cv.wait(s).unwrap(),
                }
            }
            if s.stop {
                return;
            }
            retry = next_try.is_some() && (s.room || due);
            if due {
                wait = (wait * 2).min(params.backoff_max);
            }
            s.kicked = false;
            s.room = false;
        }
        crate::stage::mark(crate::stage::Stage::Wake);
        #[cfg(test)]
        lanes.turns.fetch_add(1, Ordering::Relaxed);
        match lanes.drain(n, retry) {
            Ok(false) => {
                next_try = None;
                wait = params.backoff_first;
            }
            Ok(true) if next_try.is_none() => {
                wait = params.backoff_first;
                next_try = Some(Instant::now() + wait);
            }
            Ok(true) if retry => next_try = Some(Instant::now() + wait),
            Ok(true) => {}
            Err(e) => return lanes.irq.fail("virtio-blk", &e),
        }
    }
}

fn run_writeback(disk: Arc<Disk>) {
    let (lock, cv) = &disk.writeback.signal;
    loop {
        {
            let mut s = lock.lock().unwrap();
            while !s.kicked && !s.stop {
                s = cv.wait(s).unwrap();
            }
            if s.stop {
                return;
            }
            s.kicked = false;
        }
        let start = std::time::Instant::now();
        let mut no_room = false;
        let ok = loop {
            if unsafe { libc::fsync(disk.file.as_raw_fd()) } == 0 {
                break true;
            }
            let err = io::Error::last_os_error();
            if err.raw_os_error().is_some_and(no_space) {
                no_room = true;
                break false;
            }
            if err.kind() != io::ErrorKind::Interrupted {
                break false;
            }
        };
        if no_room {
            disk.flushes.push_refused(start.elapsed());
            continue;
        }
        disk.flushes.pushed(start.elapsed(), ok);
    }
}

impl VirtioDevice for Block {
    fn device_id(&self) -> u32 {
        DEVICE_ID
    }

    fn features(&self) -> u64 {
        let mut f = F_SEG_MAX | F_BLK_SIZE | F_TOPOLOGY | F_FLUSH;
        if self.queue_sizes.len() > 1 {
            f |= F_MQ;
        }
        if self.window.is_some() {
            f |= F_FEV_WINDOW;
        }
        if self.disk.read_only {
            f | F_RO
        } else {
            f | F_DISCARD | F_WRITE_ZEROES
        }
    }

    fn shm_region(&self, id: u8) -> Option<(u64, u64)> {
        self.window.filter(|_| id == 0)
    }

    fn queue_max_sizes(&self) -> &[u16] {
        &self.queue_sizes
    }

    fn read_config(&self, offset: u64, data: &mut [u8]) {
        read_config_bytes(&self.config(), offset, data);
    }

    fn activate(&mut self, activation: Activation) {
        self.writeback = self.disk.start_writeback();
        let lanes = self.lanes_for(activation);
        for n in 0..lanes.lanes.len() {
            let lanes = lanes.clone();
            let thread = std::thread::Builder::new()
                .name(format!("virtio-blk-q{n}"))
                .spawn(move || run_worker(lanes, n))
                .expect("spawn virtio-blk worker");
            self.workers.push(thread);
        }
        let weak = Arc::downgrade(&lanes);
        self.disk.space.on_room(move || {
            if let Some(lanes) = weak.upgrade() {
                for lane in &lanes.lanes {
                    lane.signal.0.lock().unwrap().room = true;
                    lane.signal.1.notify_one();
                }
            }
        });
        self.lanes = Some(lanes);
    }

    fn inline(&mut self) -> Option<Arc<dyn super::Inline>> {
        match &self.lanes {
            Some(lanes) if self.inline_max != 0 => Some(lanes.clone()),
            _ => None,
        }
    }

    fn notify(&mut self, queue: u32) {
        if let Some(lanes) = &self.lanes {
            if let Some(lane) = lanes.lanes.get(queue as usize) {
                lane.sender.note();
                lanes.kick(lane);
            }
        }
    }

    fn reset(&mut self) {
        if let Some(lanes) = self.lanes.take() {
            for lane in &lanes.lanes {
                lane.signal.0.lock().unwrap().stop = true;
                lane.signal.1.notify_one();
            }
            let workers = std::mem::take(&mut self.workers);
            crate::pause::away(|| {
                for thread in workers {
                    let _ = thread.join();
                }
                let mut dropped = 0;
                for lane in &lanes.lanes {
                    let mut st = lane.state.lock().unwrap();
                    st.live = false;
                    dropped += st.held.len() as u64;
                    st.held.clear();
                }
                self.disk.space.let_go(dropped, false);
            });
            self.disk.space.forget_wakers();
        }
        if let Some(t) = self.writeback.take() {
            self.disk.stop_writeback(t);
        }
    }
}

impl Drop for Block {
    fn drop(&mut self) {
        self.reset();
    }
}

#[cfg(test)]
mod tests {
    use super::super::queue::testing::{memory, Driver};
    use super::super::testing::{interrupt, wait_high, INTID};
    use super::*;
    use std::os::unix::fs::MetadataExt;
    use std::path::PathBuf;

    const BASE: u64 = 0x4000_0000;
    const HDR: u64 = BASE + 0x8000;
    const DATA: u64 = BASE + 0x10000;
    const STATUS: u64 = BASE + 0x9000;

    struct TempImage(PathBuf);

    impl TempImage {
        fn new(name: &str, len: u64) -> Self {
            let path =
                std::env::temp_dir().join(format!("fev-blk-{name}-{}.img.raw", std::process::id()));
            let f = File::create(&path).unwrap();
            f.set_len(len).unwrap();
            Self(path)
        }
        fn disk(&self, read_only: bool) -> Disk {
            Disk::open(&self.0, read_only).unwrap()
        }
        fn allocated(&self) -> u64 {
            std::fs::metadata(&self.0).unwrap().blocks() * 512
        }
    }

    impl Drop for TempImage {
        fn drop(&mut self) {
            let _ = std::fs::remove_file(&self.0);
        }
    }

    fn header(mem: &GuestMemory, kind: u32, sector: u64) {
        mem.write_u32(HDR, kind).unwrap();
        mem.write_u32(HDR + 4, 0).unwrap();
        mem.write(HDR + 8, &sector.to_le_bytes()).unwrap();
    }

    fn request(
        disk: &Disk,
        mem: &GuestMemory,
        kind: u32,
        sector: u64,
        data: Option<(u32, bool)>,
    ) -> (u8, u32) {
        let (mut drv, mut q) = Driver::new(mem, BASE, 8);
        header(mem, kind, sector);
        mem.write(STATUS, &[0xff]).unwrap();
        let mut bufs = vec![(HDR, 16, false)];
        if let Some((len, writable)) = data {
            bufs.push((DATA, len, writable));
        }
        bufs.push((STATUS, 1, true));
        drv.add(mem, &bufs);
        let chain = q.pop(mem).unwrap().unwrap();
        let written = handle(disk, mem, &chain);
        let mut status = [0u8];
        mem.read(STATUS, &mut status).unwrap();
        (status[0], written)
    }

    fn segment(mem: &GuestMemory, sector: u64, count: u32, flags: u32) {
        mem.write(DATA, &sector.to_le_bytes()).unwrap();
        mem.write_u32(DATA + 8, count).unwrap();
        mem.write_u32(DATA + 12, flags).unwrap();
    }

    #[test]
    fn writes_then_reads_back() {
        let img = TempImage::new("rw", 1 << 20);
        let disk = img.disk(false);
        let mem = memory();
        let pattern: Vec<u8> = (0..1024u32).map(|i| (i * 7) as u8).collect();
        mem.write(DATA, &pattern).unwrap();
        assert_eq!(
            request(&disk, &mem, T_OUT, 3, Some((1024, false))),
            (S_OK, 1)
        );

        mem.write(DATA, &[0u8; 1024]).unwrap();
        assert_eq!(
            request(&disk, &mem, T_IN, 3, Some((1024, true))),
            (S_OK, 1025)
        );
        let mut back = vec![0u8; 1024];
        mem.read(DATA, &mut back).unwrap();
        assert_eq!(back, pattern);

        let mut on_disk = vec![0u8; 1024];
        disk.file.read_exact_at(&mut on_disk, 3 * 512).unwrap();
        assert_eq!(on_disk, pattern);
    }

    #[test]
    fn rejects_requests_outside_the_disk() {
        let img = TempImage::new("bounds", 1 << 20);
        let disk = img.disk(false);
        let mem = memory();
        let last = (1 << 20) / 512 - 1;
        assert_eq!(request(&disk, &mem, T_IN, last, Some((512, true))).0, S_OK);
        assert_eq!(
            request(&disk, &mem, T_IN, last, Some((1024, true))).0,
            S_IOERR
        );
        assert_eq!(
            request(&disk, &mem, T_OUT, last + 1, Some((512, false))).0,
            S_IOERR
        );
        assert_eq!(
            request(&disk, &mem, T_IN, u64::MAX, Some((512, true))).0,
            S_IOERR
        );
        assert_eq!(request(&disk, &mem, T_IN, 0, Some((100, true))).0, S_IOERR);
        assert_eq!(request(&disk, &mem, 99, 0, None).0, S_UNSUPP);
    }

    #[test]
    fn a_data_buffer_outside_ram_is_an_io_error() {
        let img = TempImage::new("oob", 1 << 20);
        let disk = img.disk(false);
        let mem = memory();
        let (mut drv, mut q) = Driver::new(&mem, BASE, 8);
        header(&mem, T_IN, 0);
        drv.add(
            &mem,
            &[(HDR, 16, false), (0x1000, 512, true), (STATUS, 1, true)],
        );
        let chain = q.pop(&mem).unwrap().unwrap();
        handle(&disk, &mem, &chain);
        let mut status = [0u8];
        mem.read(STATUS, &mut status).unwrap();
        assert_eq!(status[0], S_IOERR);

        let (mut drv, mut q) = Driver::new(&mem, BASE, 8);
        drv.add(&mem, &[(HDR, 16, false)]);
        let chain = q.pop(&mem).unwrap().unwrap();
        assert_eq!(handle(&disk, &mem, &chain), 0);
    }

    #[test]
    fn flush_and_identify() {
        let img = TempImage::new("flush", 1 << 20);
        let disk = img.disk(false);
        let mem = memory();
        assert_eq!(request(&disk, &mem, T_FLUSH, 0, None), (S_OK, 1));
        assert_eq!(
            request(&disk, &mem, T_GET_ID, 0, Some((20, true))),
            (S_OK, 21)
        );
        let mut id = [0u8; DISK_ID.len()];
        mem.read(DATA, &mut id).unwrap();
        assert_eq!(&id, DISK_ID);
    }

    fn window_bytes(w: &ImageWindow, offset: usize, len: usize) -> Vec<u8> {
        assert!(offset + len <= w.len() as usize);
        unsafe { std::slice::from_raw_parts(w.host().add(offset), len).to_vec() }
    }

    #[test]
    fn the_window_shows_what_requests_wrote_and_zeros_where_they_discarded() {
        let img = TempImage::new("window", 64 << 20);
        let disk = img.disk(false);
        let mem = memory();
        let w = disk.map_window(64 << 20).unwrap();
        assert_eq!((w.len(), w.is_empty()), (64 << 20, false));
        assert_eq!(window_bytes(&w, 5 << 20, 4096), vec![0; 4096]);
        assert_eq!(img.allocated(), 0);
        let pattern: Vec<u8> = (0..4096u32).map(|i| (i * 13 + 5) as u8).collect();
        mem.write(DATA, &pattern).unwrap();
        let sector = (33u64 << 20) / SECTOR;
        assert_eq!(
            request(&disk, &mem, T_OUT, sector, Some((4096, false))),
            (S_OK, 1)
        );
        assert_eq!(window_bytes(&w, 33 << 20, 4096), pattern);
        mem.write(DATA, &[0x77; 4096]).unwrap();
        assert_eq!(
            request(&disk, &mem, T_OUT, sector, Some((4096, false))),
            (S_OK, 1)
        );
        assert_eq!(window_bytes(&w, 33 << 20, 4096), vec![0x77; 4096]);
        let mb = vec![0x5au8; 1 << 20];
        disk.file.write_all_at(&mb, 40 << 20).unwrap();
        assert_eq!(window_bytes(&w, 40 << 20, 1 << 20), mb);
        segment(
            &mem,
            (40u64 << 20) / SECTOR,
            ((1u64 << 20) / SECTOR) as u32,
            0,
        );
        assert_eq!(
            request(&disk, &mem, T_DISCARD, 0, Some((16, false))),
            (S_OK, 1)
        );
        assert_eq!(window_bytes(&w, 40 << 20, 1 << 20), vec![0; 1 << 20]);
        mem.write(DATA, &pattern).unwrap();
        assert_eq!(
            request(
                &disk,
                &mem,
                T_OUT,
                (40u64 << 20) / SECTOR + 8,
                Some((4096, false))
            ),
            (S_OK, 1)
        );
        assert_eq!(window_bytes(&w, (40 << 20) + 4096, 4096), pattern);
        assert_eq!(window_bytes(&w, 40 << 20, 4096), vec![0; 4096]);
        assert!(w.resident_pages().unwrap() > 0);
    }

    #[test]
    fn a_disk_has_no_window_unless_it_is_writable_and_was_given_one() {
        let img = TempImage::new("nowindow", 1 << 20);
        let dev = Block::new(Arc::new(img.disk(false)), 1);
        assert_eq!(dev.features() & F_FEV_WINDOW, 0);
        assert_eq!(dev.shm_region(0), None);
        let dev = Block::new(Arc::new(img.disk(false)), 1).with_window(1 << 38, 1 << 20);
        assert_ne!(dev.features() & F_FEV_WINDOW, 0);
        assert_eq!(dev.shm_region(0), Some((1 << 38, 1 << 20)));
        assert_eq!(dev.shm_region(1), None);
        assert_eq!(dev.shm_region(255), None);
        let ro = img.disk(true);
        assert!(ro.map_window(1 << 20).is_err());
        let dev = Block::new(Arc::new(ro), 1).with_window(1 << 38, 1 << 20);
        assert_eq!(dev.features() & F_FEV_WINDOW, 0);
        assert_eq!(dev.shm_region(0), None);
        let rw = img.disk(false);
        for len in [0, 4096, (1 << 20) + 16384, 2 << 20] {
            assert!(rw.map_window(len).is_err(), "{len}");
        }
        assert!(rw.map_window(1 << 20).is_ok());
        assert!(rw.map_window(16384).is_ok());
        let dev = Block::new(Arc::new(rw), 1).with_window(1 << 38, 2 << 20);
        assert_eq!(dev.features() & F_FEV_WINDOW, 0);
        assert_eq!(
            Block::new(Arc::new(img.disk(false)), 1)
                .with_window(1 << 38, 0)
                .shm_region(0),
            None
        );
    }

    #[test]
    fn a_request_that_names_window_memory_as_its_buffer_is_refused() {
        let img = TempImage::new("winbuf", 1 << 20);
        let disk = img.disk(false);
        disk.file.write_all_at(&[0x11; 4096], 0).unwrap();
        let mem = memory();
        let window = crate::layout::DISK_WINDOW_BASE;
        for (kind, writable) in [(T_IN, true), (T_OUT, false), (T_GET_ID, true)] {
            let (mut drv, mut q) = Driver::new(&mem, BASE, 8);
            header(&mem, kind, 0);
            mem.write(STATUS, &[0xff]).unwrap();
            drv.add(
                &mem,
                &[
                    (HDR, 16, false),
                    (window + 4096, 4096, writable),
                    (STATUS, 1, true),
                ],
            );
            let chain = q.pop(&mem).unwrap().unwrap();
            let written = handle(&disk, &mem, &chain);
            let mut status = [0u8];
            mem.read(STATUS, &mut status).unwrap();
            if kind == T_GET_ID {
                assert_eq!(written, 1, "kind {kind}");
            } else {
                assert_eq!((status[0], written), (S_IOERR, 1), "kind {kind}");
            }
        }
        let (mut drv, mut q) = Driver::new(&mem, BASE, 8);
        header(&mem, T_IN, 0);
        drv.add(
            &mem,
            &[(HDR, 16, false), (DATA, 512, true), (window, 1, true)],
        );
        let chain = q.pop(&mem).unwrap().unwrap();
        assert_eq!(handle(&disk, &mem, &chain), 0);
        let mut back = [0u8; 4096];
        disk.file.read_exact_at(&mut back, 0).unwrap();
        assert_eq!(back, [0x11; 4096], "the image is as it was");
    }

    #[test]
    fn read_only_disks_refuse_writes() {
        let img = TempImage::new("ro", 1 << 20);
        let disk = img.disk(true);
        let mem = memory();
        assert_eq!(
            request(&disk, &mem, T_OUT, 0, Some((512, false))).0,
            S_IOERR
        );
        segment(&mem, 0, 8, 0);
        assert_eq!(
            request(&disk, &mem, T_DISCARD, 0, Some((16, false))).0,
            S_IOERR
        );
        assert_eq!(request(&disk, &mem, T_IN, 0, Some((512, true))).0, S_OK);
        let dev = Block::new(Arc::new(disk), 1);
        assert_ne!(dev.features() & F_RO, 0);
        assert_eq!(dev.features() & (F_DISCARD | F_WRITE_ZEROES), 0);
    }

    #[test]
    fn the_writeback_thread_pushes_after_the_threshold_of_writes() {
        let img = TempImage::new("writeback", 8 << 20);
        let mem = memory();
        let disk = Arc::new(img.disk(false).with_writeback(16 * WRITEBACK_CHUNK));
        let thread = disk
            .start_writeback()
            .expect("a writable disk has a thread");
        mem.write(DATA, &[0x5a; 4096]).unwrap();
        let chunk = WRITEBACK_CHUNK / SECTOR;
        for i in 0..15u64 {
            assert_eq!(
                request(&disk, &mem, T_OUT, i * chunk, Some((4096, false))),
                (S_OK, 1)
            );
        }
        std::thread::sleep(std::time::Duration::from_millis(50));
        assert_eq!(disk.flushes().pushes(), 0);
        assert_eq!(disk.flushes().written(), 15 * 4096);
        let shared = Disk::open_shared(&disk).unwrap();
        assert_eq!(
            request(&shared, &mem, T_OUT, 15 * chunk, Some((4096, false))),
            (S_OK, 1)
        );
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
        while disk.flushes().pushes() == 0 && std::time::Instant::now() < deadline {
            std::thread::sleep(std::time::Duration::from_millis(5));
        }
        assert_eq!(disk.flushes().pushes(), 1, "{}", disk.flushes().report());
        assert_eq!(disk.flushes().written(), 16 * 4096);
        assert_eq!(request(&disk, &mem, T_FLUSH, 0, None), (S_OK, 1));
        for i in 0..15u64 {
            assert_eq!(
                request(&disk, &mem, T_OUT, i * chunk, Some((4096, false))),
                (S_OK, 1)
            );
        }
        std::thread::sleep(std::time::Duration::from_millis(50));
        assert_eq!(disk.flushes().pushes(), 1);
        let line = disk.flushes().report();
        assert!(line.contains("; 1 pushes, mean "), "{line}");
        assert!(
            line.contains("written 0 MiB in 31 requests, 0 discards"),
            "{line}"
        );
        disk.stop_writeback(thread);
        assert!(Arc::new(img.disk(false).with_writeback(0))
            .start_writeback()
            .is_none());
        assert!(Arc::new(img.disk(true)).start_writeback().is_none());
    }

    #[test]
    fn one_worker_takes_each_interval_of_the_rule() {
        const ROUNDS: u64 = 20_000;
        let every = std::time::Duration::from_millis(1);
        let st = Arc::new(FlushStats::default());
        let go = Arc::new(std::sync::Barrier::new(8));
        let threads: Vec<_> = (0..8)
            .map(|_| {
                let (st, go) = (st.clone(), go.clone());
                std::thread::spawn(move || {
                    let mut taken = 0u64;
                    for round in 1..=ROUNDS {
                        go.wait();
                        if st.take_full(round * every.as_nanos() as u64, every) {
                            taken += 1;
                        }
                    }
                    taken
                })
            })
            .collect();
        let taken: u64 = threads.into_iter().map(|t| t.join().unwrap()).sum();
        assert_eq!(taken, ROUNDS);
        assert_eq!(st.by_rule(), ROUNDS);
    }

    #[test]
    fn a_failed_push_fails_the_guest_flush_that_follows() {
        let img = TempImage::new("pushfail", 1 << 20);
        let mem = memory();
        let disk = img.disk(false);
        let shared = Disk::open_shared(&disk).unwrap();
        assert_eq!(request(&disk, &mem, T_FLUSH, 0, None), (S_OK, 1));
        disk.flushes()
            .pushed(std::time::Duration::from_millis(1), true);
        assert_eq!(request(&disk, &mem, T_FLUSH, 0, None), (S_OK, 1));
        disk.flushes()
            .pushed(std::time::Duration::from_millis(1), false);
        assert_eq!(request(&shared, &mem, T_FLUSH, 0, None), (S_IOERR, 1));
        assert_eq!(request(&disk, &mem, T_FLUSH, 0, None), (S_OK, 1));
        let line = disk.flushes().report();
        assert!(line.contains(", failed 1, by the rule "), "{line}");
        assert!(
            line.ends_with("2 pushes, mean 1000 us, max 1000 us, failed 1"),
            "{line}"
        );
    }

    #[test]
    fn rewrites_in_place_do_not_push_again_until_a_flush() {
        let img = TempImage::new("inplace", 8 << 20);
        let mem = memory();
        let disk = Arc::new(img.disk(false).with_writeback(2 * WRITEBACK_CHUNK));
        let thread = disk.start_writeback().expect("a thread");
        mem.write(DATA, &[0x5a; 4096]).unwrap();
        let chunk = WRITEBACK_CHUNK / SECTOR;
        let write = |c: u64| {
            assert_eq!(
                request(&disk, &mem, T_OUT, c * chunk + 8, Some((4096, false))),
                (S_OK, 1)
            )
        };
        let pushes_reach = |n: u64| {
            let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
            while disk.flushes().pushes() < n && std::time::Instant::now() < deadline {
                std::thread::sleep(std::time::Duration::from_millis(5));
            }
            std::thread::sleep(std::time::Duration::from_millis(50));
            assert_eq!(disk.flushes().pushes(), n, "{}", disk.flushes().report());
        };
        write(0);
        write(1);
        pushes_reach(1);
        for _ in 0..200 {
            write(0);
            write(1);
        }
        pushes_reach(1);
        write(2);
        write(3);
        pushes_reach(2);
        assert_eq!(request(&disk, &mem, T_FLUSH, 0, None), (S_OK, 1));
        for _ in 0..50 {
            write(0);
            write(1);
        }
        pushes_reach(3);
        mem.write(DATA, &[0x33; 8192]).unwrap();
        assert_eq!(
            request(&disk, &mem, T_OUT, 9 * chunk - 8, Some((8192, false))),
            (S_OK, 1)
        );
        pushes_reach(4);
        disk.stop_writeback(thread);
        let disk = Arc::new(
            img.disk(false)
                .with_writeback_stream(2 * WRITEBACK_CHUNK, 8192),
        );
        let thread = disk.start_writeback().expect("a thread");
        for c in 0..8 {
            assert_eq!(
                request(&disk, &mem, T_OUT, c * chunk, Some((8192, false))),
                (S_OK, 1)
            );
        }
        std::thread::sleep(std::time::Duration::from_millis(100));
        assert_eq!(disk.flushes().pushes(), 0, "{}", disk.flushes().report());
        for c in 8..10 {
            assert_eq!(
                request(&disk, &mem, T_OUT, c * chunk, Some((4096, false))),
                (S_OK, 1)
            );
        }
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
        while disk.flushes().pushes() < 1 && std::time::Instant::now() < deadline {
            std::thread::sleep(std::time::Duration::from_millis(5));
        }
        assert_eq!(disk.flushes().pushes(), 1);
        disk.stop_writeback(thread);
        let disk = Arc::new(
            img.disk(false)
                .with_writeback_by(8192, WritebackCount::Written),
        );
        let thread = disk.start_writeback().expect("a thread");
        for n in 1..=2 {
            for _ in 0..2 {
                assert_eq!(
                    request(&disk, &mem, T_OUT, 0, Some((4096, false))),
                    (S_OK, 1)
                );
            }
            let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
            while disk.flushes().pushes() < n && std::time::Instant::now() < deadline {
                std::thread::sleep(std::time::Duration::from_millis(5));
            }
            assert_eq!(disk.flushes().pushes(), n);
        }
        disk.stop_writeback(thread);
    }

    #[test]
    fn a_flush_succeeds_in_every_sync_mode() {
        let img = TempImage::new("syncmodes", 1 << 20);
        let mem = memory();
        for mode in [SyncMode::Fsync, SyncMode::Barrier, SyncMode::Full] {
            let disk = img.disk(false).with_sync(mode);
            disk.file.write_all_at(&[0x5a; 4096], 0).unwrap();
            assert_eq!(request(&disk, &mem, T_FLUSH, 0, None), (S_OK, 1));
            assert_eq!(Disk::open_shared(&disk).unwrap().sync_mode, mode);
        }
        assert_eq!(img.disk(false).sync_mode, SyncMode::Paced);
        let disk = img.disk(false).with_sync(SyncMode::Barrier);
        let shared = Disk::open_shared(&disk).unwrap();
        assert_eq!(request(&disk, &mem, T_FLUSH, 0, None), (S_OK, 1));
        assert_eq!(request(&shared, &mem, T_FLUSH, 0, None), (S_OK, 1));
        assert_eq!(disk.flushes().count(), 2);
        let line = shared.flushes().report();
        assert!(
            line.starts_with(
                "2 flushes (fsync 0, barrier 2, full 0, failed 0, by the rule 0), mean "
            ),
            "{line}"
        );
        assert!(line.contains("us:") || line.contains("ms:"), "{line}");
        assert_eq!(
            sync_fd_how(disk.file.as_raw_fd(), SyncMode::Fsync).unwrap(),
            Flushed::Fsync
        );
        assert_eq!(
            sync_fd_how(disk.file.as_raw_fd(), SyncMode::Full).unwrap(),
            Flushed::Full
        );
        for mode in [SyncMode::Fsync, SyncMode::Barrier, SyncMode::Full] {
            assert!(sync_fd_how(-1, mode).is_err());
        }
        for errno in [libc::EIO, libc::ENOSPC, libc::EDQUOT] {
            assert!(!unsupported(&io::Error::from_raw_os_error(errno)));
        }
        for errno in [libc::ENOTSUP, libc::ENOTTY, libc::EINVAL] {
            assert!(unsupported(&io::Error::from_raw_os_error(errno)));
        }
    }

    #[test]
    fn the_first_flush_after_an_interval_is_full_and_the_rest_are_barriers() {
        let img = TempImage::new("paced", 1 << 20);
        let mem = memory();
        let every = std::time::Duration::from_secs(1);
        let disk = Arc::new(img.disk(false).with_full_every(every));
        assert_eq!(disk.sync_mode, SyncMode::Paced);
        let st = disk.flushes();
        let counts = || (st.by_call(Flushed::Full), st.by_call(Flushed::Barrier));
        assert_eq!(request(&disk, &mem, T_FLUSH, 0, None), (S_OK, 1));
        assert_eq!(counts(), (1, 0));
        for _ in 0..3 {
            assert_eq!(request(&disk, &mem, T_FLUSH, 0, None), (S_OK, 1));
        }
        assert_eq!(counts(), (1, 3));
        std::thread::sleep(every);
        let shared = Disk::open_shared(&disk).unwrap();
        assert_eq!(request(&shared, &mem, T_FLUSH, 0, None), (S_OK, 1));
        assert_eq!(request(&disk, &mem, T_FLUSH, 0, None), (S_OK, 1));
        assert_eq!(counts(), (2, 4));
        std::thread::sleep(every);
        let go = Arc::new(std::sync::Barrier::new(8));
        let threads: Vec<_> = (0..8)
            .map(|_| {
                let handle = Disk::open_shared(&disk).unwrap();
                let go = go.clone();
                std::thread::spawn(move || {
                    go.wait();
                    handle.sync().unwrap();
                })
            })
            .collect();
        for t in threads {
            t.join().unwrap();
        }
        assert_eq!(counts(), (3, 11));
        assert_eq!(st.by_rule(), 3);
        assert_eq!(st.count(), 14);
        let line = st.report();
        assert!(
            line.starts_with(
                "14 flushes (fsync 0, barrier 11, full 3, failed 0, by the rule 3), mean "
            ),
            "{line}"
        );
        for mode in [SyncMode::Fsync, SyncMode::Barrier, SyncMode::Full] {
            let disk = img.disk(false).with_sync(mode).with_full_every(every);
            assert_eq!(request(&disk, &mem, T_FLUSH, 0, None), (S_OK, 1));
            assert_eq!(request(&disk, &mem, T_FLUSH, 0, None), (S_OK, 1));
            assert_eq!(disk.flushes().by_rule(), 0);
            let expect = match mode {
                SyncMode::Fsync => Flushed::Fsync,
                SyncMode::Barrier => Flushed::Barrier,
                _ => Flushed::Full,
            };
            assert_eq!(disk.flushes().by_call(expect), 2, "{mode:?}");
        }
        assert!(sync_fd_how(-1, SyncMode::Paced).is_err());
    }

    #[test]
    #[ignore = "a measurement, run by hand"]
    fn sync_mode_cost() {
        const ROUNDS: usize = 300;
        let img = match std::env::var_os("FEV_SYNC_DIR") {
            Some(dir) => {
                let path =
                    PathBuf::from(dir).join(format!("fev-synccost-{}.img", std::process::id()));
                File::create(&path).unwrap().set_len(64 << 20).unwrap();
                TempImage(path)
            }
            None => TempImage::new("synccost", 64 << 20),
        };
        for mode in [SyncMode::Fsync, SyncMode::Barrier, SyncMode::Full] {
            let disk = img.disk(false).with_sync(mode);
            let mut us: Vec<u128> = (0..ROUNDS)
                .map(|i| {
                    let offset = (i as u64 * 7919 % 16000) * 4096;
                    disk.file.write_all_at(&[i as u8; 4096], offset).unwrap();
                    let t = std::time::Instant::now();
                    disk.sync().unwrap();
                    t.elapsed().as_micros()
                })
                .collect();
            us.sort();
            println!(
                "{mode:?}: median {} us, p95 {} us, max {} us over {ROUNDS} flushes; {}",
                us[ROUNDS / 2],
                us[ROUNDS * 95 / 100],
                us[ROUNDS - 1],
                disk.flushes().report()
            );
        }
    }

    #[test]
    fn discard_returns_space_to_the_host_filesystem() {
        let len = 8u64 << 20;
        let img = TempImage::new("discard", len);
        let disk = img.disk(false);
        let mem = memory();
        disk.file
            .write_all_at(&vec![0xabu8; len as usize], 0)
            .unwrap();
        disk.sync().unwrap();
        let full = img.allocated();
        assert!(full >= len, "image should be fully allocated, has {full}");

        segment(&mem, 2048, 6 * 2048, 0);
        assert_eq!(
            request(&disk, &mem, T_DISCARD, 0, Some((16, false))).0,
            S_OK
        );
        let after = img.allocated();
        assert!(
            after <= full - (6 << 20),
            "expected 6 MiB back, allocated went {full} -> {after}"
        );

        let mut edge = [0u8; 512];
        disk.file.read_exact_at(&mut edge, (1 << 20) - 512).unwrap();
        assert_eq!(edge, [0xab; 512], "data before the range must survive");
        disk.file.read_exact_at(&mut edge, 7 << 20).unwrap();
        assert_eq!(edge, [0xab; 512], "data after the range must survive");
    }

    #[test]
    fn write_zeroes_zeroes_exactly_the_range_even_when_unaligned() {
        let len = 1u64 << 20;
        let img = TempImage::new("wz", len);
        let disk = img.disk(false);
        let mem = memory();
        disk.file
            .write_all_at(&vec![0xcdu8; len as usize], 0)
            .unwrap();

        segment(&mem, 3, 98, 1);
        assert_eq!(
            request(&disk, &mem, T_WRITE_ZEROES, 0, Some((16, false))).0,
            S_OK
        );
        let mut all = vec![0u8; len as usize];
        disk.file.read_exact_at(&mut all, 0).unwrap();
        for (i, b) in all.iter().enumerate() {
            let want = if (3 * 512..101 * 512).contains(&i) {
                0
            } else {
                0xcd
            };
            assert_eq!(*b, want, "byte {i}");
        }
    }

    #[test]
    fn trim_requests_are_validated() {
        let img = TempImage::new("trimbad", 1 << 20);
        let disk = img.disk(false);
        let mem = memory();
        segment(&mem, 2040, 64, 0);
        assert_eq!(
            request(&disk, &mem, T_DISCARD, 0, Some((16, false))).0,
            S_IOERR
        );
        segment(&mem, 0, 8, 1);
        assert_eq!(
            request(&disk, &mem, T_DISCARD, 0, Some((16, false))).0,
            S_UNSUPP
        );
        assert_eq!(
            request(&disk, &mem, T_DISCARD, 0, Some((12, false))).0,
            S_IOERR
        );
        assert_eq!(request(&disk, &mem, T_DISCARD, 0, None).0, S_IOERR);
    }

    fn wait_used(drv: &mut Driver, mem: &GuestMemory) -> (u16, u32) {
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
        loop {
            if let Some(u) = drv.take_used(mem) {
                return u;
            }
            assert!(
                std::time::Instant::now() < deadline,
                "worker did not respond"
            );
            std::thread::yield_now();
        }
    }

    #[test]
    fn each_queue_has_a_worker_that_serves_it_and_stops_on_reset() {
        let img = TempImage::new("worker", 1 << 20);
        let mem = Arc::new(memory());
        let (mut drv0, q0) = Driver::new(&mem, BASE, 8);
        let (mut drv1, q1) = Driver::new(&mem, BASE + 0x2000, 8);
        let (chip, irq) = interrupt();
        let irq_dev = irq.clone();
        let mut dev = Block::new(Arc::new(img.disk(false)), 2);
        assert_eq!(dev.queue_max_sizes().len(), 2);
        assert_ne!(dev.features() & F_MQ, 0);
        let config = dev.config();
        assert_eq!(config[0..8], 2048u64.to_le_bytes());
        assert_eq!(config[34..36], 2u16.to_le_bytes());
        dev.activate(Activation {
            mem: mem.clone(),
            irq: irq_dev,
            queues: vec![q0, q1],
            features: 0,
        });
        assert_eq!(dev.workers.len(), 2);

        header(&mem, T_OUT, 0);
        mem.write(DATA, &[0x5a; 512]).unwrap();
        let head = drv1.add(
            &mem,
            &[(HDR, 16, false), (DATA, 512, false), (STATUS, 1, true)],
        );
        crate::irq::enter_vcpu(2);
        dev.notify(1);
        assert_eq!(wait_used(&mut drv1, &mem), (head, 1));
        wait_high(&chip);
        assert_eq!(chip.routed(INTID), [2]);
        irq.ack(super::super::INT_USED_RING);
        crate::irq::enter_vcpu(1);
        assert_eq!(drv0.take_used(&mem), None);

        header(&mem, T_IN, 0);
        mem.write(DATA, &[0; 512]).unwrap();
        let head = drv0.add(
            &mem,
            &[(HDR, 16, false), (DATA, 512, true), (STATUS, 1, true)],
        );
        dev.notify(0);
        assert_eq!(wait_used(&mut drv0, &mem), (head, 513));
        let mut back = [0u8; 512];
        mem.read(DATA, &mut back).unwrap();
        assert_eq!(back, [0x5a; 512]);

        wait_high(&chip);
        assert_eq!(chip.routed(INTID), [2, 1]);

        dev.notify(7);
        dev.reset();
        assert!(dev.workers.is_empty());
        assert!(chip.level(INTID));
    }

    #[test]
    fn queue_count_is_clamped_and_one_queue_does_not_claim_multiqueue() {
        let img = TempImage::new("clamp", 1 << 20);
        let disk = Arc::new(img.disk(false));
        assert_eq!(Block::new(disk.clone(), 0).queue_max_sizes().len(), 1);
        assert_eq!(
            Block::new(disk.clone(), 64).queue_max_sizes().len(),
            MAX_QUEUES as usize
        );
        assert_eq!(Block::new(disk, 1).features() & F_MQ, 0);
    }

    #[test]
    fn a_request_in_an_indirect_table_can_exceed_the_queue_size() {
        let img = TempImage::new("indirect", 1 << 20);
        let disk = img.disk(false);
        let mem = memory();
        let (mut drv, mut q) = Driver::new(&mem, BASE, 4);
        q.indirect = true;
        header(&mem, T_OUT, 8);
        let mut bufs = vec![(HDR, 16, false)];
        for i in 0..16u64 {
            mem.write(DATA + i * 512, &[i as u8 + 1; 512]).unwrap();
            bufs.push((DATA + i * 512, 512, false));
        }
        bufs.push((STATUS, 1, true));
        drv.add_indirect(&mem, BASE + 0x6000, &bufs);
        let chain = q.pop(&mem).unwrap().unwrap();
        assert_eq!(handle(&disk, &mem, &chain), 1);
        let mut on_disk = vec![0u8; 16 * 512];
        disk.file.read_exact_at(&mut on_disk, 8 * 512).unwrap();
        for (i, sector) in on_disk.chunks(512).enumerate() {
            assert_eq!(sector, [i as u8 + 1; 512]);
        }
    }

    struct Rig {
        _img: TempImage,
        mem: Arc<GuestMemory>,
        drv: Driver,
        lanes: Arc<Lanes>,
        chip: Arc<crate::irq::testing::RecordingChip>,
        irq: Arc<super::super::Interrupt>,
    }

    fn rig(name: &str, read_only: bool, event_idx: bool) -> Rig {
        use super::super::Inline;
        let img = TempImage::new(name, 1 << 20);
        let mem = Arc::new(memory());
        let (drv, mut q) = Driver::new(&mem, BASE, 8);
        q.event_idx = event_idx;
        let (chip, irq) = interrupt();
        let dev = Block::new(Arc::new(img.disk(read_only)), 1).with_inline(INLINE_MAX);
        let mut lanes = dev.lanes_for(Activation {
            mem: mem.clone(),
            irq: irq.clone(),
            queues: vec![q],
            features: 0,
        });
        Arc::get_mut(&mut lanes).unwrap().slow_after = std::time::Duration::from_secs(60);
        let _: &dyn Inline = &*lanes;
        Rig {
            _img: img,
            mem,
            drv,
            lanes,
            chip,
            irq,
        }
    }

    impl Rig {
        fn put(&mut self, n: u64, kind: u32, sector: u64, data: Option<(u32, bool)>) -> u16 {
            let (hdr, status, at) = (HDR + n * 0x100, STATUS + n * 0x10, DATA + n * 0x40000);
            let mut h = [0u8; 16];
            h[0..4].copy_from_slice(&kind.to_le_bytes());
            h[8..16].copy_from_slice(&sector.to_le_bytes());
            self.mem.write(hdr, &h).unwrap();
            self.mem.write(status, &[0xff]).unwrap();
            let mut bufs = vec![(hdr, 16, false)];
            if let Some((len, writable)) = data {
                bufs.push((at, len, writable));
            }
            bufs.push((status, 1, true));
            self.drv.add(&self.mem, &bufs)
        }

        fn notified(&self) {
            use super::super::Inline;
            self.lanes.notified(0);
        }

        fn kicked(&self) -> bool {
            std::mem::take(&mut self.lanes.lanes[0].signal.0.lock().unwrap().kicked)
        }

        fn handed(&self) -> usize {
            self.lanes.lanes[0].state.lock().unwrap().handed.len()
        }

        fn status(&self, n: u64) -> u8 {
            let mut b = [0u8];
            self.mem.read(STATUS + n * 0x10, &mut b).unwrap();
            b[0]
        }
    }

    #[test]
    fn a_small_request_alone_on_its_queue_is_served_by_the_vcpu_that_notified() {
        let mut r = rig("inline-small", false, true);
        crate::irq::enter_vcpu(3);
        r.mem.write(DATA, &[0x5a; 4096]).unwrap();
        let head = r.put(0, T_OUT, 8, Some((4096, false)));
        r.notified();
        assert_eq!(r.drv.take_used(&r.mem), Some((head, 1)));
        assert_eq!(r.status(0), S_OK);
        assert!(
            !r.kicked(),
            "the worker was woken for a request already served"
        );
        assert_eq!(r.handed(), 0);
        assert!(r.chip.level(INTID));
        assert_eq!(r.chip.routed(INTID), [3]);
        r.irq.ack(super::super::INT_USED_RING);

        r.mem.write(DATA, &[0; 4096]).unwrap();
        let head = r.put(0, T_IN, 8, Some((4096, true)));
        r.notified();
        assert_eq!(r.drv.take_used(&r.mem), Some((head, 4097)));
        let mut back = [0u8; 4096];
        r.mem.read(DATA, &mut back).unwrap();
        assert_eq!(back, [0x5a; 4096]);
        let head = r.put(0, T_IN, 0, Some((INLINE_MAX as u32, true)));
        r.notified();
        assert_eq!(r.drv.take_used(&r.mem), Some((head, INLINE_MAX as u32 + 1)));
        assert!(!r.kicked());

        r.notified();
        assert_eq!(r.drv.take_used(&r.mem), None);
        assert!(!r.kicked());
    }

    #[test]
    fn a_flush_a_trim_and_a_large_request_are_left_to_the_worker() {
        let big = INLINE_MAX as u32 + 512;
        type Case = (&'static str, u32, Option<(u32, bool)>, u32);
        let cases: [Case; 7] = [
            ("flush", T_FLUSH, None, 1),
            ("discard", T_DISCARD, Some((16, false)), 1),
            ("write-zeroes", T_WRITE_ZEROES, Some((16, false)), 1),
            ("identify", T_GET_ID, Some((20, true)), 21),
            ("large write", T_OUT, Some((big, false)), 1),
            ("large read", T_IN, Some((big, true)), big + 1),
            ("unknown", 99, None, 1),
        ];
        let mut r = rig("inline-not", false, true);
        for (what, kind, data, written) in cases {
            let mut seg = [0u8; 16];
            seg[8..12].copy_from_slice(&8u32.to_le_bytes());
            r.mem.write(DATA, &seg).unwrap();
            let head = r.put(0, kind, 0, data);
            r.notified();
            assert_eq!(r.drv.take_used(&r.mem), None, "{what}: served by the vCPU");
            assert_eq!(r.status(0), 0xff, "{what}");
            assert_eq!(r.handed(), 1, "{what}: not left for the worker");
            assert!(r.kicked(), "{what}: the worker was not woken");
            r.lanes.drain(0, false).unwrap();
            assert_eq!(r.drv.take_used(&r.mem), Some((head, written)), "{what}");
            assert_eq!(r.handed(), 0);
        }

        let head = r
            .drv
            .add(&r.mem, &[(u64::MAX - 64, 16, false), (STATUS, 1, true)]);
        r.notified();
        assert_eq!(r.drv.take_used(&r.mem), None);
        assert!(r.kicked());
        r.lanes.drain(0, false).unwrap();
        assert_eq!(r.drv.take_used(&r.mem), Some((head, 1)));
        assert_eq!(r.status(0), S_IOERR);

        let mut ro = rig("inline-ro", true, true);
        let head = ro.put(0, T_OUT, 0, Some((512, false)));
        ro.notified();
        assert_eq!(ro.drv.take_used(&ro.mem), None);
        assert!(ro.kicked());
        ro.lanes.drain(0, false).unwrap();
        assert_eq!(ro.drv.take_used(&ro.mem), Some((head, 1)));
        assert_eq!(ro.status(0), S_IOERR);
        let head = ro.put(0, T_IN, 0, Some((512, true)));
        ro.notified();
        assert_eq!(ro.drv.take_used(&ro.mem), Some((head, 513)));
    }

    #[test]
    fn a_queue_holding_more_than_one_request_is_the_workers_in_order() {
        let mut r = rig("inline-two", false, true);
        r.mem.write(DATA, &[1; 512]).unwrap();
        r.mem.write(DATA + 0x40000, &[2; 512]).unwrap();
        let first = r.put(0, T_OUT, 0, Some((512, false)));
        let second = r.put(1, T_OUT, 0, Some((512, false)));
        r.notified();
        assert_eq!(r.drv.take_used(&r.mem), None);
        assert_eq!(r.handed(), 1);
        assert!(r.kicked());
        let third = r.put(2, T_IN, 0, Some((512, true)));
        r.notified();
        assert_eq!(r.drv.take_used(&r.mem), None);
        assert!(r.kicked());
        r.lanes.drain(0, false).unwrap();
        assert_eq!(r.drv.take_used(&r.mem), Some((first, 1)));
        assert_eq!(r.drv.take_used(&r.mem), Some((second, 1)));
        assert_eq!(r.drv.take_used(&r.mem), Some((third, 513)));
        let mut back = [0u8; 512];
        r.mem.read(DATA + 0x80000, &mut back).unwrap();
        assert_eq!(back, [2; 512]);

        let flush = r.put(0, T_FLUSH, 0, None);
        r.notified();
        assert_eq!((r.handed(), r.kicked()), (1, true));
        let small = r.put(1, T_IN, 0, Some((512, true)));
        r.notified();
        assert_eq!(
            r.drv.take_used(&r.mem),
            None,
            "served ahead of a request sent before it"
        );
        assert!(r.kicked());
        r.lanes.drain(0, false).unwrap();
        assert_eq!(r.drv.take_used(&r.mem), Some((flush, 1)));
        assert_eq!(r.drv.take_used(&r.mem), Some((small, 513)));
    }

    #[test]
    fn a_vcpu_does_not_wait_for_a_worker_that_has_the_queue() {
        let mut r = rig("inline-busy", false, true);
        let head = r.put(0, T_OUT, 0, Some((512, false)));
        let lanes = r.lanes.clone();
        let serving = lanes.lanes[0].state.lock().unwrap();
        let start = std::time::Instant::now();
        r.notified();
        assert!(start.elapsed() < std::time::Duration::from_secs(1));
        assert!(r.kicked(), "the worker was not woken");
        assert_eq!(r.drv.take_used(&r.mem), None);
        drop(serving);
        assert_eq!(r.handed(), 0, "the vCPU touched a queue it did not have");
        r.lanes.drain(0, false).unwrap();
        assert_eq!(r.drv.take_used(&r.mem), Some((head, 1)));
    }

    #[test]
    fn a_request_that_slips_in_behind_one_served_by_a_vcpu_is_not_stranded() {
        let mut r = rig("inline-slip", false, true);
        let first = r.put(0, T_OUT, 0, Some((512, false)));
        let (mem, avail) = (r.mem.clone(), r.drv.avail);
        let second = {
            let mut h = [0u8; 16];
            h[0..4].copy_from_slice(&T_IN.to_le_bytes());
            mem.write(HDR + 0x100, &h).unwrap();
            r.drv.write_desc(&mem, 5, HDR + 0x100, 16, 1, 6);
            r.drv.write_desc(&mem, 6, DATA + 0x40000, 512, 3, 7);
            r.drv.write_desc(&mem, 7, STATUS + 0x10, 1, 2, 0);
            5u16
        };
        *r.lanes.slipped.lock().unwrap() = Some(Box::new(move || {
            mem.write_u16(avail + 4 + 2, second).unwrap();
            mem.write_u16(avail + 2, 2).unwrap();
        }));
        r.notified();
        assert_eq!(r.drv.take_used(&r.mem), Some((first, 1)));
        assert_eq!(
            r.drv.take_used(&r.mem),
            None,
            "the vCPU served a second request"
        );
        assert!(
            r.kicked(),
            "the request that slipped in was left with nobody"
        );
        *r.lanes.slipped.lock().unwrap() = None;
        r.lanes.drain(0, false).unwrap();
        assert_eq!(r.drv.take_used(&r.mem), Some((second, 513)));
    }

    #[test]
    fn after_a_slow_request_the_worker_serves_until_the_queue_is_quick_again() {
        let mut r = rig("inline-slow", false, true);
        Arc::get_mut(&mut r.lanes).unwrap().slow_after = std::time::Duration::ZERO;
        let head = r.put(0, T_OUT, 0, Some((512, false)));
        r.notified();
        assert_eq!(r.drv.take_used(&r.mem), Some((head, 1)));
        assert_eq!(r.lanes.lanes[0].slow.load(Ordering::Relaxed), QUICK_AGAIN);
        Arc::get_mut(&mut r.lanes).unwrap().slow_after = std::time::Duration::from_secs(60);

        for left in (0..QUICK_AGAIN).rev() {
            let head = r.put(0, T_OUT, 0, Some((512, false)));
            r.notified();
            assert_eq!(r.drv.take_used(&r.mem), None, "served by a vCPU too soon");
            assert_eq!(r.handed(), 0, "a vCPU took a request off a slow queue");
            assert!(r.kicked());
            r.lanes.drain(0, false).unwrap();
            assert_eq!(r.drv.take_used(&r.mem), Some((head, 1)));
            assert_eq!(r.lanes.lanes[0].slow.load(Ordering::Relaxed), left);
            let flush = r.put(1, T_FLUSH, 0, None);
            r.lanes.lanes[0].sender.note();
            r.lanes.kick(&r.lanes.lanes[0]);
            assert!(r.kicked());
            r.lanes.drain(0, false).unwrap();
            assert_eq!(r.drv.take_used(&r.mem), Some((flush, 1)));
            assert_eq!(r.lanes.lanes[0].slow.load(Ordering::Relaxed), left);
        }
        let head = r.put(0, T_OUT, 0, Some((512, false)));
        r.notified();
        assert_eq!(r.drv.take_used(&r.mem), Some((head, 1)));
        assert!(!r.kicked());
    }

    #[test]
    fn a_pause_does_not_wait_for_a_vcpu_inside_a_disk_call() {
        use super::super::Inline;
        let mut r = rig("inline-pause", false, true);
        let (inside, in_call) = std::sync::mpsc::channel();
        *r.lanes.stall.lock().unwrap() = Some(Box::new(move || {
            let _ = inside.send(());
            std::thread::sleep(std::time::Duration::from_millis(400));
        }));
        let head = r.put(0, T_OUT, 0, Some((512, false)));
        let gate = Arc::new(crate::pause::Gate::new());
        let (lanes, vgate) = (r.lanes.clone(), gate.clone());
        let back = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let returned = back.clone();
        let vcpu = std::thread::spawn(move || {
            vgate.attach();
            vgate.enter();
            lanes.notified(0);
            let total = vgate.back();
            returned.store(true, Ordering::SeqCst);
            vgate.leave();
            total
        });
        in_call
            .recv_timeout(std::time::Duration::from_secs(5))
            .expect("the vCPU made the Mac's call");
        let start = std::time::Instant::now();
        let paused = gate.pause(&|| {}, &|| 1000).expect("the pause is made");
        assert!(paused.changed);
        assert!(
            start.elapsed() < std::time::Duration::from_millis(200),
            "the pause waited for the Mac's call"
        );
        assert_eq!(gate.account().claimed, 1);
        std::thread::sleep(std::time::Duration::from_millis(500));
        assert!(
            !back.load(Ordering::SeqCst),
            "the vCPU went on under a pause"
        );
        assert_eq!(gate.resume(&|| 1700).unwrap().total, 700);
        assert_eq!(vcpu.join().unwrap(), Some(700), "the total it must apply");
        assert_eq!(r.drv.take_used(&r.mem), Some((head, 1)));
    }

    #[test]
    fn a_pause_does_not_wait_for_a_vcpu_that_waits_in_a_disk_reset() {
        let img = TempImage::new("inline-reset-pause", 1 << 20);
        let mem = Arc::new(memory());
        let (_drv, q) = Driver::new(&mem, BASE, 8);
        let (_chip, irq) = interrupt();
        let mut dev = Block::new(Arc::new(img.disk(false)), 1).with_inline(INLINE_MAX);
        dev.activate(Activation {
            mem: mem.clone(),
            irq,
            queues: vec![q],
            features: 0,
        });
        let lanes = dev.lanes.clone().unwrap();
        let (held, release) = std::sync::mpsc::channel::<()>();
        let (has_it, had_it) = std::sync::mpsc::channel::<()>();
        let serving = std::thread::spawn(move || {
            let _serving = lanes.lanes[0].state.lock().unwrap();
            has_it.send(()).unwrap();
            let _ = release.recv();
        });
        had_it.recv().unwrap();
        let gate = Arc::new(crate::pause::Gate::new());
        let vgate = gate.clone();
        let (began_tx, began) = std::sync::mpsc::channel();
        let resetting = std::thread::spawn(move || {
            vgate.attach();
            vgate.enter();
            began_tx.send(()).unwrap();
            dev.reset();
            let total = vgate.back();
            vgate.leave();
            total
        });
        began.recv().unwrap();
        std::thread::sleep(std::time::Duration::from_millis(30));
        let start = std::time::Instant::now();
        let paused = gate.pause(&|| {}, &|| 1000);
        let took = start.elapsed();
        let claimed = gate.account().claimed;
        let _ = gate.resume(&|| 1600);
        held.send(()).unwrap();
        serving.join().unwrap();
        let total = resetting.join().unwrap();
        assert!(paused.is_ok(), "{paused:?}");
        assert!(
            took < std::time::Duration::from_millis(200),
            "the pause waited for the reset, which waits for the Mac: {took:?}"
        );
        assert_eq!(claimed, 1);
        assert_eq!(total, Some(600), "the total it must apply");
    }

    #[test]
    fn a_reset_waits_for_a_vcpu_that_is_serving_and_nothing_is_served_after_it() {
        let img = TempImage::new("inline-reset", 1 << 20);
        let mem = Arc::new(memory());
        let (mut drv, q) = Driver::new(&mem, BASE, 8);
        let (chip, irq) = interrupt();
        let mut dev = Block::new(Arc::new(img.disk(false)), 1).with_inline(INLINE_MAX);
        dev.activate(Activation {
            mem: mem.clone(),
            irq,
            queues: vec![q],
            features: 0,
        });
        let server = dev
            .inline()
            .expect("an active disk takes its notifications inline");
        let lanes = dev.lanes.clone().unwrap();

        let (held, release) = std::sync::mpsc::channel::<()>();
        let (has_it, had_it) = std::sync::mpsc::channel::<()>();
        let vcpu = {
            let lanes = lanes.clone();
            std::thread::spawn(move || {
                let _serving = lanes.lanes[0].state.lock().unwrap();
                has_it.send(()).unwrap();
                release.recv().unwrap();
            })
        };
        had_it.recv().unwrap();
        let (done, was_done) = std::sync::mpsc::channel::<()>();
        let resetting = std::thread::spawn(move || {
            dev.reset();
            done.send(()).unwrap();
            dev
        });
        assert!(
            was_done
                .recv_timeout(std::time::Duration::from_millis(100))
                .is_err(),
            "the reset did not wait for the vCPU"
        );
        held.send(()).unwrap();
        vcpu.join().unwrap();
        was_done.recv().unwrap();
        let mut dev = resetting.join().unwrap();
        assert!(dev.inline().is_none());

        header(&mem, T_OUT, 0);
        mem.write(STATUS, &[0xff]).unwrap();
        drv.add(
            &mem,
            &[(HDR, 16, false), (DATA, 512, false), (STATUS, 1, true)],
        );
        server.notified(0);
        assert_eq!(drv.take_used(&mem), None);
        let mut status = [0u8];
        mem.read(STATUS, &mut status).unwrap();
        assert_eq!(status, [0xff]);
        assert!(!chip.level(INTID));
        assert!(!lanes.lanes[0].signal.0.lock().unwrap().kicked);
    }

    #[test]
    fn with_inline_service_off_every_request_is_the_workers() {
        let img = TempImage::new("inline-off", 1 << 20);
        let mem = Arc::new(memory());
        let (mut drv, q) = Driver::new(&mem, BASE, 8);
        let (_chip, irq) = interrupt();
        let mut dev = Block::new(Arc::new(img.disk(false)), 1).with_inline(0);
        dev.activate(Activation {
            mem: mem.clone(),
            irq,
            queues: vec![q],
            features: 0,
        });
        assert!(dev.inline().is_none());
        header(&mem, T_OUT, 0);
        let head = drv.add(
            &mem,
            &[(HDR, 16, false), (DATA, 512, false), (STATUS, 1, true)],
        );
        dev.notify(0);
        assert_eq!(wait_used(&mut drv, &mem), (head, 1));
    }

    type SpaceRig = (Rig, Arc<Space>, Arc<Mutex<Vec<crate::vmmctl::Space>>>);

    fn space_rig(name: &str, params: crate::space::Params) -> SpaceRig {
        use super::super::Inline;
        let img = TempImage::new(name, 1 << 20);
        let mem = Arc::new(memory());
        let (drv, mut q) = Driver::new(&mem, BASE, 8);
        q.event_idx = true;
        let (chip, irq) = interrupt();
        let (space, _volume, seen) = crate::space::tests::fake(params, 32 << 20, 1 << 30);
        let disk = img.disk(false).with_space(space.clone());
        let dev = Block::new(Arc::new(disk), 1).with_inline(INLINE_MAX);
        let mut lanes = dev.lanes_for(Activation {
            mem: mem.clone(),
            irq: irq.clone(),
            queues: vec![q],
            features: 0,
        });
        Arc::get_mut(&mut lanes).unwrap().slow_after = std::time::Duration::from_secs(60);
        let _: &dyn Inline = &*lanes;
        (
            Rig {
                _img: img,
                mem,
                drv,
                lanes,
                chip,
                irq,
            },
            space,
            seen,
        )
    }

    impl Rig {
        fn held(&self) -> usize {
            self.lanes.lanes[0].state.lock().unwrap().held.len()
        }
    }

    #[test]
    fn a_write_the_mac_has_no_room_for_is_kept_and_answered_when_it_has() {
        let (mut r, space, seen) = space_rig("held", crate::space::Params::default());
        crate::irq::enter_vcpu(2);
        space.refuse(true);
        r.mem.write(DATA, &[0x11; 4096]).unwrap();
        let first = r.put(0, T_OUT, 8, Some((4096, false)));
        r.notified();
        assert_eq!(r.drv.take_used(&r.mem), None, "answered on the vCPU");
        assert_eq!(r.status(0), 0xff);
        assert_eq!((r.handed(), r.held()), (1, 0), "a vCPU kept it");
        assert!(r.kicked(), "the worker was not woken for it");
        assert!(r.lanes.drain(0, false).unwrap(), "the worker holds nothing");
        assert_eq!((r.handed(), r.held(), space.held()), (0, 1, 1));
        assert_eq!(r.drv.take_used(&r.mem), None, "answered by the worker");
        assert_eq!(r.status(0), 0xff, "a status was written for a kept request");
        assert!(!r.chip.level(INTID), "an interrupt for nothing");
        {
            let seen = seen.lock().unwrap();
            assert_eq!(seen.len(), 1, "the daemon is told of the first");
            assert_eq!(seen[0].held_requests, 1);
        }

        let read = r.put(1, T_IN, 0, Some((512, true)));
        r.notified();
        assert!(r.kicked());
        assert_eq!(r.drv.take_used(&r.mem), None);
        assert!(r.lanes.drain(0, false).unwrap());
        assert_eq!(r.drv.take_used(&r.mem), Some((read, 513)));
        assert_eq!(r.status(1), S_OK);
        r.irq.ack(super::super::INT_USED_RING);

        r.mem.write(DATA + 2 * 0x40000, &[0x22; 4096]).unwrap();
        let second = r.put(2, T_OUT, 16, Some((4096, false)));
        r.notified();
        assert!(r.kicked());
        assert!(r.lanes.drain(0, false).unwrap());
        assert_eq!((r.held(), space.held()), (2, 2));
        assert_eq!(seen.lock().unwrap().len(), 1);

        let before = space.counters()[4].1;
        assert!(r.lanes.drain(0, true).unwrap());
        assert_eq!(space.counters()[4].1, before + 1);
        assert_eq!((r.held(), space.held()), (2, 2));
        assert_eq!(r.drv.take_used(&r.mem), None);

        space.refuse(false);
        assert!(!r.lanes.drain(0, true).unwrap());
        assert_eq!(r.drv.take_used(&r.mem), Some((first, 1)));
        assert_eq!(r.drv.take_used(&r.mem), Some((second, 1)));
        assert_eq!((r.status(0), r.status(2)), (S_OK, S_OK));
        assert_eq!((r.held(), space.held()), (0, 0));
        let mut on_disk = [0u8; 4096];
        r.lanes
            .disk
            .file
            .read_exact_at(&mut on_disk, 8 * 512)
            .unwrap();
        assert_eq!(on_disk, [0x11; 4096]);
        r.lanes
            .disk
            .file
            .read_exact_at(&mut on_disk, 16 * 512)
            .unwrap();
        assert_eq!(on_disk, [0x22; 4096]);
        {
            let seen = seen.lock().unwrap();
            assert_eq!(seen.len(), 2);
            assert_eq!(seen[1].held_requests, 0);
        }
        r.irq.ack(super::super::INT_USED_RING);
        let head = r.put(0, T_OUT, 8, Some((4096, false)));
        r.notified();
        assert_eq!(r.drv.take_used(&r.mem), Some((head, 1)));
        assert!(!r.kicked());
    }

    #[test]
    fn a_flush_the_mac_has_no_room_for_is_kept_too() {
        let (mut r, space, _) = space_rig("held-flush", crate::space::Params::default());
        r.lanes.disk.flushes.push_refused(std::time::Duration::ZERO);
        let flush = r.put(0, T_FLUSH, 0, None);
        r.notified();
        assert!(r.lanes.drain(0, false).unwrap());
        assert_eq!((r.held(), space.held()), (1, 1));
        assert_eq!(r.status(0), 0xff);
        assert!(!r.lanes.drain(0, true).unwrap());
        assert_eq!(r.drv.take_used(&r.mem), Some((flush, 1)));
        assert_eq!(r.status(0), S_OK);
        assert_eq!(r.lanes.disk.flushes.push_failed.load(Ordering::Relaxed), 1);
    }

    #[test]
    fn with_the_hold_off_a_write_with_no_room_is_an_io_error() {
        let params = crate::space::Params {
            hold: false,
            ..Default::default()
        };
        let (mut r, space, seen) = space_rig("held-off", params);
        space.refuse(true);
        let head = r.put(0, T_OUT, 8, Some((4096, false)));
        r.notified();
        assert_eq!(r.drv.take_used(&r.mem), Some((head, 1)));
        assert_eq!(r.status(0), S_IOERR);
        assert_eq!(space.held(), 0);
        assert!(seen.lock().unwrap().is_empty());
        assert_eq!(r.lanes.disk.flushes.no_space.load(Ordering::Relaxed), 1);
    }

    struct Live {
        _img: TempImage,
        mem: Arc<GuestMemory>,
        drv: Driver,
        dev: Block,
        space: Arc<Space>,
    }

    fn live(name: &str, params: crate::space::Params) -> Live {
        let img = TempImage::new(name, 1 << 20);
        let mem = Arc::new(memory());
        let (drv, q) = Driver::new(&mem, BASE, 8);
        let (_chip, irq) = interrupt();
        let (space, _, _) = crate::space::tests::fake(params, 32 << 20, 1 << 30);
        let mut dev = Block::new(Arc::new(img.disk(false).with_space(space.clone())), 1);
        dev.activate(Activation {
            mem: mem.clone(),
            irq,
            queues: vec![q],
            features: 0,
        });
        Live {
            _img: img,
            mem,
            drv,
            dev,
            space,
        }
    }

    impl Live {
        fn write(&mut self, n: u64) -> u16 {
            let (hdr, status, at) = (HDR + n * 0x100, STATUS + n * 0x10, DATA + n * 0x40000);
            let mut h = [0u8; 16];
            h[0..4].copy_from_slice(&T_OUT.to_le_bytes());
            h[8..16].copy_from_slice(&(8 * n).to_le_bytes());
            self.mem.write(hdr, &h).unwrap();
            self.mem.write(status, &[0xff]).unwrap();
            let head = self.drv.add(
                &self.mem,
                &[(hdr, 16, false), (at, 4096, false), (status, 1, true)],
            );
            self.dev.notify(0);
            head
        }

        fn until(&self, what: &str, done: impl Fn() -> bool) {
            let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
            while !done() {
                assert!(std::time::Instant::now() < deadline, "{what}");
                std::thread::sleep(std::time::Duration::from_millis(1));
            }
        }

        fn turns(&self) -> u64 {
            self.dev
                .lanes
                .as_ref()
                .unwrap()
                .turns
                .load(Ordering::Relaxed)
        }
    }

    #[test]
    fn a_held_write_is_released_by_a_budget_a_discard_and_the_back_off() {
        let never = std::time::Duration::from_secs(3600);
        let mut l = live(
            "held-budget",
            crate::space::Params {
                backoff_first: never,
                backoff_max: never,
                ..Default::default()
            },
        );
        l.space.refuse(true);
        let head = l.write(0);
        l.until("the write was never kept", || l.space.held() == 1);
        assert_eq!(l.drv.take_used(&l.mem), None);
        let tried = l.space.counters()[4].1;
        assert_eq!(l.space.baseline(32 << 20, 1 << 30, 0), 1);
        l.until("the budget did not make the worker try", || {
            l.space.counters()[4].1 > tried
        });
        assert_eq!(l.space.held(), 1);
        l.space.refuse(false);
        assert_eq!(l.space.baseline(900 << 20, 1 << 30, 0), 1);
        assert_eq!(wait_used(&mut l.drv, &l.mem), (head, 1));
        l.until("still counted as held", || l.space.held() == 0);

        l.space.refuse(true);
        let head = l.write(1);
        l.until("the write was never kept", || l.space.held() == 1);
        l.space.refuse(false);
        let mut seg = [0u8; 16];
        seg[8..12].copy_from_slice(&8u32.to_le_bytes());
        l.mem.write(DATA + 2 * 0x40000, &seg).unwrap();
        let mut h = [0u8; 16];
        h[0..4].copy_from_slice(&T_DISCARD.to_le_bytes());
        l.mem.write(HDR + 0x200, &h).unwrap();
        let trim = l.drv.add(
            &l.mem,
            &[
                (HDR + 0x200, 16, false),
                (DATA + 2 * 0x40000, 16, false),
                (STATUS + 0x20, 1, true),
            ],
        );
        l.dev.notify(0);
        let mut done = vec![
            wait_used(&mut l.drv, &l.mem).0,
            wait_used(&mut l.drv, &l.mem).0,
        ];
        done.sort();
        let mut want = vec![head, trim];
        want.sort();
        assert_eq!(done, want);
        l.until("still counted as held", || l.space.held() == 0);
        l.dev.reset();

        let mut l = live(
            "held-backoff",
            crate::space::Params {
                backoff_first: std::time::Duration::from_millis(5),
                backoff_max: std::time::Duration::from_millis(20),
                ..Default::default()
            },
        );
        std::thread::sleep(std::time::Duration::from_millis(150));
        assert_eq!(l.turns(), 0, "a worker that holds nothing woke by itself");
        l.space.refuse(true);
        let head = l.write(0);
        l.until("the write was never kept", || l.space.held() == 1);
        let tried = l.space.counters()[4].1;
        l.until("the worker did not try again by itself", || {
            l.space.counters()[4].1 >= tried + 3
        });
        assert_eq!(l.drv.take_used(&l.mem), None);
        l.space.refuse(false);
        assert_eq!(wait_used(&mut l.drv, &l.mem), (head, 1));
        l.until("still counted as held", || l.space.held() == 0);
        std::thread::sleep(std::time::Duration::from_millis(50));
        let turns = l.turns();
        std::thread::sleep(std::time::Duration::from_millis(150));
        assert_eq!(l.turns(), turns, "the back-off outlived the hold");
        l.dev.reset();
    }

    #[test]
    fn a_reset_with_a_write_held_returns_at_once_and_answers_nothing() {
        let never = std::time::Duration::from_secs(3600);
        let mut l = live(
            "held-reset",
            crate::space::Params {
                backoff_first: never,
                backoff_max: never,
                ..Default::default()
            },
        );
        let seen = Arc::new(Mutex::new(Vec::new()));
        let sink = seen.clone();
        l.space.on_report(move |s| sink.lock().unwrap().push(s));
        l.space.refuse(true);
        l.write(0);
        l.write(1);
        l.until("the writes were never kept", || l.space.held() == 2);
        let start = std::time::Instant::now();
        l.dev.reset();
        assert!(
            start.elapsed() < std::time::Duration::from_secs(2),
            "the reset waited {:?} with requests held",
            start.elapsed()
        );
        assert!(l.dev.workers.is_empty());
        assert_eq!(l.space.held(), 0);
        assert_eq!(
            l.drv.take_used(&l.mem),
            None,
            "a dropped request was answered"
        );
        let seen = seen.lock().unwrap();
        assert_eq!(seen.last().map(|s| s.held_requests), Some(0));
        l.space.room();
    }

    #[test]
    fn the_volume_is_read_once_an_interval_of_bytes_written() {
        let img = TempImage::new("space-count", 4 << 20);
        let params = crate::space::Params {
            every: 1 << 20,
            ..Default::default()
        };
        let (space, volume, seen) = crate::space::tests::fake(params, 8 << 30, 0);
        let disk = img.disk(false).with_space(space.clone());
        let mem = memory();
        let opened = volume.reads.load(Ordering::Relaxed);
        mem.write(DATA, &[1u8; 4096]).unwrap();
        for _ in 0..768 {
            assert_eq!(request(&disk, &mem, T_OUT, 0, Some((4096, false))).0, S_OK);
        }
        assert_eq!(volume.reads.load(Ordering::Relaxed) - opened, 3);
        assert_eq!(space.counters()[0].1, 3);
        for _ in 0..600 {
            request(&disk, &mem, T_IN, 0, Some((4096, true)));
        }
        request(&disk, &mem, T_FLUSH, 0, None);
        assert_eq!(volume.reads.load(Ordering::Relaxed) - opened, 3);
        assert!(seen.lock().unwrap().is_empty());
        volume.set(7 << 30, 0);
        for _ in 0..256 {
            request(&disk, &mem, T_OUT, 0, Some((4096, false)));
        }
        assert_eq!(seen.lock().unwrap().len(), 1);
        let other = Disk::open_shared(&disk).unwrap();
        for _ in 0..256 {
            request(&other, &mem, T_OUT, 0, Some((4096, false)));
        }
        assert_eq!(space.counters()[0].1, 5);
        let ro = img.disk(true);
        assert!(!ro.space().crossed(0, 1 << 40));
    }
}
