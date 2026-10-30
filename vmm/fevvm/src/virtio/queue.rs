use std::fmt;
use std::num::Wrapping;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use crate::memory::{GuestMemory, OutOfRange};

const DESC_SIZE: u64 = 16;
const DESC_F_NEXT: u16 = 1;
const DESC_F_WRITE: u16 = 2;
const DESC_F_INDIRECT: u16 = 4;
const AVAIL_F_NO_INTERRUPT: u16 = 1;
pub const MAX_INDIRECT: u32 = 1024;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum QueueError {
    Memory(OutOfRange),
    BadIndex(u16),
    Loop,
    ReadableAfterWritable,
    Indirect,
    IndirectTable(u32),
    AvailOverrun,
}

impl fmt::Display for QueueError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Memory(e) => write!(f, "{e}"),
            Self::BadIndex(i) => write!(f, "descriptor index {i} out of range"),
            Self::Loop => write!(f, "descriptor chain loops"),
            Self::ReadableAfterWritable => write!(f, "readable descriptor after writable"),
            Self::Indirect => write!(f, "indirect descriptor not allowed here"),
            Self::IndirectTable(len) => write!(f, "indirect table of {len} bytes is malformed"),
            Self::AvailOverrun => write!(f, "available index ahead by more than the queue size"),
        }
    }
}

impl std::error::Error for QueueError {}

impl From<OutOfRange> for QueueError {
    fn from(e: OutOfRange) -> Self {
        Self::Memory(e)
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Buf {
    pub addr: u64,
    pub len: u32,
}

#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct Chain {
    pub head: u16,
    pub readable: Vec<Buf>,
    pub writable: Vec<Buf>,
}

impl Chain {
    pub fn readable_len(&self) -> u64 {
        self.readable.iter().map(|b| b.len as u64).sum()
    }

    pub fn writable_len(&self) -> u64 {
        self.writable.iter().map(|b| b.len as u64).sum()
    }

    pub fn read_exact(&self, mem: &GuestMemory, out: &mut [u8]) -> bool {
        let mut done = 0;
        for b in &self.readable {
            if done == out.len() {
                break;
            }
            let n = (b.len as usize).min(out.len() - done);
            if mem.read(b.addr, &mut out[done..done + n]).is_err() {
                return false;
            }
            done += n;
        }
        done == out.len()
    }

    pub fn write(&self, mem: &GuestMemory, data: &[u8]) -> usize {
        let mut done = 0;
        for b in &self.writable {
            if done == data.len() {
                break;
            }
            let n = (b.len as usize).min(data.len() - done);
            if mem.write(b.addr, &data[done..done + n]).is_err() {
                break;
            }
            done += n;
        }
        done
    }
}

const WATCH_EVENTS: usize = 32;

static WATCHING: AtomicBool = AtomicBool::new(false);
static WATCHES: Mutex<Vec<Arc<Watch>>> = Mutex::new(Vec::new());

pub fn set_watching(on: bool) {
    WATCHING.store(on, Ordering::Relaxed);
}

pub struct Watch {
    label: String,
    avail_ring: u64,
    used_ring: u64,
    size: u16,
    next_avail: AtomicU64,
    next_used: AtomicU64,
    events: [AtomicU64; WATCH_EVENTS],
    count: AtomicU64,
    held: Vec<AtomicU64>,
    faults: AtomicU64,
}

impl fmt::Debug for Watch {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "Watch({})", self.label)
    }
}

impl Watch {
    fn note(&self, kind: u64, index: u16, head: u16, extra: u16) {
        let n = self.count.fetch_add(1, Ordering::Relaxed) as usize;
        self.events[n % WATCH_EVENTS].store(
            kind << 48 | (index as u64) << 32 | (head as u64) << 16 | extra as u64,
            Ordering::Relaxed,
        );
    }

    fn bit(&self, head: u16) -> Option<(&AtomicU64, u64)> {
        self.held
            .get(head as usize / 64)
            .map(|w| (w, 1u64 << (head % 64)))
    }

    fn fault(&self, what: fmt::Arguments<'_>) {
        if self.faults.fetch_add(1, Ordering::Relaxed) < 8 {
            crate::vmm_log!("queue {}: {what}", self.label);
        }
    }

    fn popped(&self, index: u16, head: u16, avail_idx: u16) {
        self.note(1, index, head, avail_idx);
        self.next_avail
            .store(index.wrapping_add(1) as u64, Ordering::Relaxed);
        if let Some((word, bit)) = self.bit(head) {
            if word.fetch_or(bit, Ordering::Relaxed) & bit != 0 {
                self.fault(format_args!(
                    "available entry {index} names descriptor {head}, which the device                      took before and has not returned (available index read as {avail_idx})"
                ));
            }
        }
    }

    fn unpopped(&self, index: u16, head: u16) {
        self.next_avail.store(index as u64, Ordering::Relaxed);
        if let Some((word, bit)) = self.bit(head) {
            word.fetch_and(!bit, Ordering::Relaxed);
        }
    }

    fn pushed(&self, index: u16, head: u16, written: u32) {
        self.note(2, index, head, written as u16);
        self.next_used
            .store(index.wrapping_add(1) as u64, Ordering::Relaxed);
        if let Some((word, bit)) = self.bit(head) {
            if word.fetch_and(!bit, Ordering::Relaxed) & bit == 0 {
                self.fault(format_args!(
                    "used entry {index} returns descriptor {head}, which the device does not hold"
                ));
            }
        }
    }

    fn report(&self, mem: &GuestMemory, out: &mut Vec<String>) -> bool {
        let size = self.size as u64;
        let u16_at = |addr: u64| mem.read_u16(addr).ok();
        let avail_idx = u16_at(self.avail_ring.wrapping_add(2));
        let used_idx = u16_at(self.used_ring.wrapping_add(2));
        let used_event = u16_at(self.avail_ring.wrapping_add(4 + size * 2));
        let avail_event = u16_at(self.used_ring.wrapping_add(4 + size * 8));
        let next_avail = self.next_avail.load(Ordering::Relaxed) as u16;
        let next_used = self.next_used.load(Ordering::Relaxed) as u16;
        let faults = self.faults.load(Ordering::Relaxed);
        let held: Vec<String> = (0..self.size)
            .filter(|h| {
                self.bit(*h)
                    .is_some_and(|(w, b)| w.load(Ordering::Relaxed) & b != 0)
            })
            .map(|h| h.to_string())
            .collect();
        let mut differ = Vec::new();
        if let (Some(avail), true) = (avail_idx, size != 0) {
            let span = (next_used as u64).min(size) as u16;
            for back in (1..=span).rev() {
                let n = next_used.wrapping_sub(back);
                if avail.wrapping_sub(n) > self.size {
                    continue;
                }
                let slot = n as u64 % size;
                let asked = u16_at(self.avail_ring.wrapping_add(4 + slot * 2));
                let answered = mem.read_u32(self.used_ring.wrapping_add(4 + slot * 8)).ok();
                if let (Some(a), Some(b)) = (asked, answered) {
                    if a as u32 != b {
                        differ.push(format!("{n}: asked {a}, answered {b}"));
                    }
                }
            }
        }
        let wrong = faults != 0
            || !differ.is_empty()
            || used_idx.is_some_and(|u| u != next_used)
            || avail_idx.is_some_and(|a| a.wrapping_sub(next_avail) > self.size);
        let show = |v: Option<u16>| v.map_or("unreadable".to_string(), |v| v.to_string());
        out.push(format!(
            "queue {}: size {}; the device has taken {next_avail} and returned {next_used};              in guest memory: available index {}, used index {}, used_event {}, avail_event {};              held [{}]; faults {faults}{}",
            self.label,
            self.size,
            show(avail_idx),
            show(used_idx),
            show(used_event),
            show(avail_event),
            held.join(" "),
            if differ.is_empty() {
                String::new()
            } else {
                format!("; entries that differ: {}", differ.join("; "))
            }
        ));
        let count = self.count.load(Ordering::Relaxed) as usize;
        let events: Vec<String> = (count.saturating_sub(WATCH_EVENTS)..count)
            .map(|n| {
                let e = self.events[n % WATCH_EVENTS].load(Ordering::Relaxed);
                let (index, head, extra) = (e >> 32 & 0xffff, e >> 16 & 0xffff, e & 0xffff);
                match e >> 48 {
                    1 => format!("took {index}=d{head} (saw {extra})"),
                    _ => format!("returned {index}=d{head}"),
                }
            })
            .collect();
        if wrong || !events.is_empty() {
            out.push(format!(
                "queue {}: last {} of {count}: {}",
                self.label,
                events.len(),
                events.join(", ")
            ));
        }
        wrong
    }
}

pub fn watch_report(mem: &GuestMemory, all: bool) -> Vec<String> {
    let watches = WATCHES.lock().unwrap_or_else(|e| e.into_inner()).clone();
    let mut out = Vec::new();
    for w in watches {
        let mut lines = Vec::new();
        if w.report(mem, &mut lines) || all {
            out.append(&mut lines);
        }
    }
    out
}

pub fn forget_watches(prefix: &str) {
    WATCHES
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .retain(|w| !w.label.starts_with(prefix));
}

#[derive(Debug, Clone)]
pub struct Queue {
    pub max_size: u16,
    pub size: u16,
    pub ready: bool,
    pub desc_table: u64,
    pub avail_ring: u64,
    pub used_ring: u64,
    pub event_idx: bool,
    pub indirect: bool,
    next_avail: Wrapping<u16>,
    next_used: Wrapping<u16>,
    signalled_used: Wrapping<u16>,
    watch: Option<Arc<Watch>>,
}

fn at(base: u64, offset: u64) -> Result<u64, QueueError> {
    base.checked_add(offset)
        .ok_or(QueueError::Memory(OutOfRange {
            addr: base,
            len: offset,
        }))
}

struct Desc {
    buf: Buf,
    flags: u16,
    next: u16,
}

fn read_desc(mem: &GuestMemory, table: u64, index: u64) -> Result<Desc, QueueError> {
    let d = at(table, index * DESC_SIZE)?;
    at(d, DESC_SIZE)?;
    Ok(Desc {
        buf: Buf {
            addr: mem.read_u64(d)?,
            len: mem.read_u32(d + 8)?,
        },
        flags: mem.read_u16(d + 12)?,
        next: mem.read_u16(d + 14)?,
    })
}

impl Chain {
    fn add(&mut self, d: &Desc) -> Result<(), QueueError> {
        if d.flags & DESC_F_WRITE != 0 {
            self.writable.push(d.buf);
        } else if self.writable.is_empty() {
            self.readable.push(d.buf);
        } else {
            return Err(QueueError::ReadableAfterWritable);
        }
        Ok(())
    }

    fn add_indirect(&mut self, mem: &GuestMemory, table: Buf) -> Result<(), QueueError> {
        let count = table.len / DESC_SIZE as u32;
        if table.len == 0 || !table.len.is_multiple_of(DESC_SIZE as u32) || count > MAX_INDIRECT {
            return Err(QueueError::IndirectTable(table.len));
        }
        let mut index = 0u16;
        for _ in 0..count {
            if index as u32 >= count {
                return Err(QueueError::BadIndex(index));
            }
            let d = read_desc(mem, table.addr, index as u64)?;
            if d.flags & DESC_F_INDIRECT != 0 {
                return Err(QueueError::Indirect);
            }
            self.add(&d)?;
            if d.flags & DESC_F_NEXT == 0 {
                return Ok(());
            }
            index = d.next;
        }
        Err(QueueError::Loop)
    }
}

impl Queue {
    pub fn new(max_size: u16) -> Self {
        Self {
            max_size,
            size: max_size,
            ready: false,
            desc_table: 0,
            avail_ring: 0,
            used_ring: 0,
            event_idx: false,
            indirect: false,
            next_avail: Wrapping(0),
            next_used: Wrapping(0),
            signalled_used: Wrapping(0),
            watch: None,
        }
    }

    pub fn watch(&mut self, label: String) {
        if !WATCHING.load(Ordering::Relaxed) || !self.is_valid() {
            return;
        }
        let watch = Arc::new(Watch {
            label,
            avail_ring: self.avail_ring,
            used_ring: self.used_ring,
            size: self.size,
            next_avail: AtomicU64::new(0),
            next_used: AtomicU64::new(0),
            events: std::array::from_fn(|_| AtomicU64::new(0)),
            count: AtomicU64::new(0),
            held: (0..self.size.div_ceil(64))
                .map(|_| AtomicU64::new(0))
                .collect(),
            faults: AtomicU64::new(0),
        });
        WATCHES
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .push(watch.clone());
        self.watch = Some(watch);
    }

    pub fn is_valid(&self) -> bool {
        self.ready && self.size != 0 && self.size <= self.max_size && self.size.is_power_of_two()
    }

    pub fn pop(&mut self, mem: &GuestMemory) -> Result<Option<Chain>, QueueError> {
        if !self.is_valid() {
            return Ok(None);
        }
        let avail_idx = Wrapping(mem.read_u16(at(self.avail_ring, 2)?)?);
        let pending = (avail_idx - self.next_avail).0;
        if pending == 0 {
            return Ok(None);
        }
        if pending > self.size {
            return Err(QueueError::AvailOverrun);
        }
        std::sync::atomic::fence(std::sync::atomic::Ordering::Acquire);
        let slot = (self.next_avail.0 % self.size) as u64;
        let head = mem.read_u16(at(self.avail_ring, 4 + slot * 2)?)?;

        let mut chain = Chain {
            head,
            ..Chain::default()
        };
        let mut index = head;
        for _ in 0..self.size {
            if index >= self.size {
                return Err(QueueError::BadIndex(index));
            }
            let d = read_desc(mem, self.desc_table, index as u64)?;
            if d.flags & DESC_F_INDIRECT != 0 {
                if !self.indirect || d.flags & DESC_F_NEXT != 0 {
                    return Err(QueueError::Indirect);
                }
                chain.add_indirect(mem, d.buf)?;
                self.took(head, avail_idx.0);
                return Ok(Some(chain));
            }
            chain.add(&d)?;
            if d.flags & DESC_F_NEXT == 0 {
                self.took(head, avail_idx.0);
                return Ok(Some(chain));
            }
            index = d.next;
        }
        Err(QueueError::Loop)
    }

    fn took(&mut self, head: u16, avail_idx: u16) {
        if let Some(w) = &self.watch {
            w.popped(self.next_avail.0, head, avail_idx);
        }
        self.next_avail += 1;
    }

    pub fn unpop(&mut self, heads: &[u16]) {
        for &head in heads.iter().rev() {
            self.next_avail -= 1;
            if let Some(w) = &self.watch {
                w.unpopped(self.next_avail.0, head);
            }
        }
    }

    pub fn has_available(&self, mem: &GuestMemory) -> Result<bool, QueueError> {
        if !self.is_valid() {
            return Ok(false);
        }
        Ok(mem.read_u16(at(self.avail_ring, 2)?)? != self.next_avail.0)
    }

    pub fn enable_notifications(&mut self, mem: &GuestMemory) -> Result<bool, QueueError> {
        if !self.is_valid() || !self.event_idx {
            return Ok(false);
        }
        let avail_event = at(self.used_ring, 4 + self.size as u64 * 8)?;
        mem.write_u16(avail_event, self.next_avail.0)?;
        std::sync::atomic::fence(std::sync::atomic::Ordering::SeqCst);
        Ok(mem.read_u16(at(self.avail_ring, 2)?)? != self.next_avail.0)
    }

    pub fn enable_notifications_past_available(
        &mut self,
        mem: &GuestMemory,
    ) -> Result<bool, QueueError> {
        if !self.is_valid() || !self.event_idx {
            return Ok(false);
        }
        let idx = mem.read_u16(at(self.avail_ring, 2)?)?;
        let avail_event = at(self.used_ring, 4 + self.size as u64 * 8)?;
        mem.write_u16(avail_event, idx)?;
        std::sync::atomic::fence(std::sync::atomic::Ordering::SeqCst);
        Ok(mem.read_u16(at(self.avail_ring, 2)?)? != idx)
    }

    pub fn needs_interrupt(&mut self, mem: &GuestMemory) -> Result<bool, QueueError> {
        if !self.is_valid() {
            return Ok(false);
        }
        let (old, new) = (self.signalled_used, self.next_used);
        self.signalled_used = new;
        if new == old {
            return Ok(false);
        }
        std::sync::atomic::fence(std::sync::atomic::Ordering::SeqCst);
        if self.event_idx {
            let used_event =
                Wrapping(mem.read_u16(at(self.avail_ring, 4 + self.size as u64 * 2)?)?);
            Ok((new - used_event - Wrapping(1)).0 < (new - old).0)
        } else {
            Ok(mem.read_u16(self.avail_ring)? & AVAIL_F_NO_INTERRUPT == 0)
        }
    }

    pub fn drain(
        &mut self,
        mem: &GuestMemory,
        mut serve: impl FnMut(&Chain) -> u32,
    ) -> Result<bool, QueueError> {
        loop {
            while let Some(chain) = self.pop(mem)? {
                let written = serve(&chain);
                self.push_used(mem, chain.head, written)?;
            }
            if !self.enable_notifications(mem)? {
                break;
            }
        }
        self.needs_interrupt(mem)
    }

    pub fn push_used(
        &mut self,
        mem: &GuestMemory,
        head: u16,
        written: u32,
    ) -> Result<(), QueueError> {
        self.push_used_all(mem, &[(head, written)])
    }

    pub fn push_used_all(
        &mut self,
        mem: &GuestMemory,
        used: &[(u16, u32)],
    ) -> Result<(), QueueError> {
        if !self.is_valid() {
            return Ok(());
        }
        for &(head, written) in used {
            let slot = (self.next_used.0 % self.size) as u64;
            let elem = at(self.used_ring, 4 + slot * 8)?;
            at(elem, 8)?;
            mem.write_u32(elem, head as u32)?;
            mem.write_u32(elem + 4, written)?;
            if let Some(w) = &self.watch {
                w.pushed(self.next_used.0, head, written);
            }
            self.next_used += 1;
        }
        std::sync::atomic::fence(std::sync::atomic::Ordering::SeqCst);
        mem.write_u16(at(self.used_ring, 2)?, self.next_used.0)?;
        Ok(())
    }
}

#[cfg(test)]
pub mod testing {
    use super::*;

    pub struct Driver {
        pub desc: u64,
        pub avail: u64,
        pub used: u64,
        pub size: u16,
        avail_idx: u16,
        next_desc: u16,
        used_seen: u16,
    }

    impl Driver {
        pub fn new(mem: &GuestMemory, at: u64, size: u16) -> (Self, Queue) {
            let desc = at;
            let avail = desc + size as u64 * 16;
            let used = (avail + 6 + size as u64 * 2).next_multiple_of(4);
            mem.write_u16(avail + 2, 0).unwrap();
            mem.write_u16(used + 2, 0).unwrap();
            let mut q = Queue::new(size);
            q.desc_table = desc;
            q.avail_ring = avail;
            q.used_ring = used;
            q.ready = true;
            (
                Self {
                    desc,
                    avail,
                    used,
                    size,
                    avail_idx: 0,
                    next_desc: 0,
                    used_seen: 0,
                },
                q,
            )
        }

        pub fn write_desc(
            &self,
            mem: &GuestMemory,
            i: u16,
            addr: u64,
            len: u32,
            flags: u16,
            next: u16,
        ) {
            let d = self.desc + i as u64 * 16;
            mem.write(d, &addr.to_le_bytes()).unwrap();
            mem.write_u32(d + 8, len).unwrap();
            mem.write_u16(d + 12, flags).unwrap();
            mem.write_u16(d + 14, next).unwrap();
        }

        pub fn publish(&mut self, mem: &GuestMemory, head: u16) {
            let slot = (self.avail_idx % self.size) as u64;
            mem.write_u16(self.avail + 4 + slot * 2, head).unwrap();
            self.avail_idx = self.avail_idx.wrapping_add(1);
            mem.write_u16(self.avail + 2, self.avail_idx).unwrap();
        }

        pub fn add(&mut self, mem: &GuestMemory, bufs: &[(u64, u32, bool)]) -> u16 {
            let head = self.next_desc;
            for (n, (addr, len, writable)) in bufs.iter().enumerate() {
                let i = (head + n as u16) % self.size;
                let last = n == bufs.len() - 1;
                let mut flags = if *writable { DESC_F_WRITE } else { 0 };
                if !last {
                    flags |= DESC_F_NEXT;
                }
                self.write_desc(mem, i, *addr, *len, flags, (i + 1) % self.size);
            }
            self.next_desc = (head + bufs.len() as u16) % self.size;
            self.publish(mem, head);
            head
        }

        pub fn add_indirect(
            &mut self,
            mem: &GuestMemory,
            table: u64,
            bufs: &[(u64, u32, bool)],
        ) -> u16 {
            for (n, (addr, len, writable)) in bufs.iter().enumerate() {
                let last = n == bufs.len() - 1;
                let mut flags = if *writable { DESC_F_WRITE } else { 0 };
                if !last {
                    flags |= DESC_F_NEXT;
                }
                let d = table + n as u64 * 16;
                mem.write_u64(d, *addr).unwrap();
                mem.write_u32(d + 8, *len).unwrap();
                mem.write_u16(d + 12, flags).unwrap();
                mem.write_u16(d + 14, n as u16 + 1).unwrap();
            }
            let head = self.next_desc;
            self.write_desc(mem, head, table, bufs.len() as u32 * 16, DESC_F_INDIRECT, 0);
            self.next_desc = (head + 1) % self.size;
            self.publish(mem, head);
            head
        }

        pub fn set_used_event(&self, mem: &GuestMemory, idx: u16) {
            mem.write_u16(self.avail + 4 + self.size as u64 * 2, idx)
                .unwrap();
        }

        pub fn avail_event(&self, mem: &GuestMemory) -> u16 {
            mem.read_u16(self.used + 4 + self.size as u64 * 8).unwrap()
        }

        pub fn set_no_interrupt(&self, mem: &GuestMemory, on: bool) {
            mem.write_u16(self.avail, on as u16).unwrap();
        }

        pub fn take_used(&mut self, mem: &GuestMemory) -> Option<(u16, u32)> {
            if mem.read_u16(self.used + 2).unwrap() == self.used_seen {
                return None;
            }
            let slot = (self.used_seen % self.size) as u64;
            let elem = self.used + 4 + slot * 8;
            self.used_seen = self.used_seen.wrapping_add(1);
            Some((
                mem.read_u32(elem).unwrap() as u16,
                mem.read_u32(elem + 4).unwrap(),
            ))
        }
    }

    pub fn memory() -> GuestMemory {
        GuestMemory::new(0x4000_0000, 1 << 20).unwrap()
    }
}

#[cfg(test)]
mod tests {
    use super::testing::*;
    use super::*;

    const BASE: u64 = 0x4000_0000;

    #[test]
    fn pops_chains_in_order_and_splits_by_direction() {
        let mem = memory();
        let (mut drv, mut q) = Driver::new(&mem, BASE, 8);
        assert_eq!(q.pop(&mem), Ok(None));
        assert_eq!(q.has_available(&mem), Ok(false));
        let h1 = drv.add(
            &mem,
            &[
                (BASE + 0x1000, 16, false),
                (BASE + 0x2000, 512, true),
                (BASE + 0x3000, 1, true),
            ],
        );
        let h2 = drv.add(&mem, &[(BASE + 0x4000, 8, false)]);

        let c = q.pop(&mem).unwrap().unwrap();
        assert_eq!(c.head, h1);
        assert_eq!(
            c.readable,
            vec![Buf {
                addr: BASE + 0x1000,
                len: 16
            }]
        );
        assert_eq!(c.writable.len(), 2);
        assert_eq!(c.writable_len(), 513);
        let c = q.pop(&mem).unwrap().unwrap();
        assert_eq!(c.head, h2);
        assert_eq!(q.pop(&mem), Ok(None));
    }

    #[test]
    fn publishes_used_elements_and_wraps() {
        let mem = memory();
        let (mut drv, mut q) = Driver::new(&mem, BASE, 4);
        for round in 0..10u32 {
            let head = drv.add(&mem, &[(BASE + 0x1000, 4, true)]);
            let c = q.pop(&mem).unwrap().unwrap();
            q.push_used(&mem, c.head, round).unwrap();
            assert_eq!(drv.take_used(&mem), Some((head, round)));
            assert_eq!(drv.take_used(&mem), None);
        }
    }

    #[test]
    fn rejects_malformed_chains() {
        let mem = memory();

        let (mut drv, mut q) = Driver::new(&mem, BASE, 4);
        drv.write_desc(&mem, 0, BASE + 0x1000, 4, DESC_F_NEXT, 1);
        drv.write_desc(&mem, 1, BASE + 0x1000, 4, DESC_F_NEXT, 0);
        drv.publish(&mem, 0);
        assert_eq!(q.pop(&mem), Err(QueueError::Loop));

        let (mut drv, mut q) = Driver::new(&mem, BASE, 4);
        drv.write_desc(&mem, 0, BASE + 0x1000, 4, DESC_F_NEXT, 9);
        drv.publish(&mem, 0);
        assert_eq!(q.pop(&mem), Err(QueueError::BadIndex(9)));

        let (mut drv, mut q) = Driver::new(&mem, BASE, 4);
        drv.publish(&mem, 7);
        assert_eq!(q.pop(&mem), Err(QueueError::BadIndex(7)));

        let (mut drv, mut q) = Driver::new(&mem, BASE, 4);
        drv.write_desc(&mem, 0, BASE + 0x1000, 4, DESC_F_WRITE | DESC_F_NEXT, 1);
        drv.write_desc(&mem, 1, BASE + 0x1000, 4, 0, 0);
        drv.publish(&mem, 0);
        assert_eq!(q.pop(&mem), Err(QueueError::ReadableAfterWritable));

        let (mut drv, mut q) = Driver::new(&mem, BASE, 4);
        drv.write_desc(&mem, 0, BASE + 0x1000, 4, DESC_F_INDIRECT, 0);
        drv.publish(&mem, 0);
        assert_eq!(q.pop(&mem), Err(QueueError::Indirect));

        let (drv, mut q) = Driver::new(&mem, BASE, 4);
        mem.write_u16(drv.avail + 2, 5).unwrap();
        assert_eq!(q.pop(&mem), Err(QueueError::AvailOverrun));
    }

    #[test]
    fn rings_outside_ram_are_errors_not_panics() {
        let mem = memory();
        let mut q = Queue::new(8);
        q.ready = true;
        q.desc_table = 0;
        q.avail_ring = u64::MAX - 2;
        q.used_ring = 0x10;
        assert!(matches!(q.pop(&mem), Err(QueueError::Memory(_))));
        assert!(matches!(
            q.push_used(&mem, 0, 0),
            Err(QueueError::Memory(_))
        ));

        let (mut drv, mut q) = Driver::new(&mem, BASE, 4);
        drv.add(&mem, &[(0xdead_0000, 16, false)]);
        let c = q.pop(&mem).unwrap().unwrap();
        assert!(!c.read_exact(&mem, &mut [0u8; 16]));
    }

    #[test]
    fn ring_addresses_that_overflow_are_errors() {
        let mem = memory();
        for top in [u64::MAX, u64::MAX - 1, u64::MAX - 5, u64::MAX - 40] {
            let mut q = Queue::new(8);
            q.ready = true;
            q.event_idx = true;
            q.indirect = true;
            (q.desc_table, q.avail_ring, q.used_ring) = (top, top, top);
            assert!(matches!(q.pop(&mem), Err(QueueError::Memory(_))));
            assert!(matches!(
                q.push_used(&mem, 0, 0),
                Err(QueueError::Memory(_))
            ));
            assert!(matches!(
                q.enable_notifications(&mem),
                Err(QueueError::Memory(_))
            ));

            let (mut drv, mut q) = Driver::new(&mem, BASE, 8);
            drv.publish(&mem, 7);
            q.desc_table = top;
            assert!(matches!(q.pop(&mem), Err(QueueError::Memory(_))));

            let (mut drv, mut q) = Driver::new(&mem, BASE, 8);
            q.indirect = true;
            drv.write_desc(&mem, 0, top, 32, DESC_F_INDIRECT, 0);
            drv.publish(&mem, 0);
            assert!(matches!(q.pop(&mem), Err(QueueError::Memory(_))));
        }
    }

    #[test]
    fn an_unready_or_oddly_sized_queue_is_inert() {
        let mem = memory();
        let (mut drv, mut q) = Driver::new(&mem, BASE, 4);
        drv.add(&mem, &[(BASE + 0x1000, 4, false)]);
        q.size = 3;
        assert_eq!(q.pop(&mem), Ok(None));
        q.size = 16;
        assert_eq!(q.pop(&mem), Ok(None));
        q.size = 4;
        q.ready = false;
        assert_eq!(q.pop(&mem), Ok(None));
    }

    #[test]
    fn follows_indirect_tables_when_negotiated() {
        let mem = memory();
        let (mut drv, mut q) = Driver::new(&mem, BASE, 4);
        let table = BASE + 0x6000;
        let bufs = [
            (BASE + 0x1000, 16, false),
            (BASE + 0x2000, 512, true),
            (BASE + 0x3000, 1, true),
        ];

        drv.add_indirect(&mem, table, &bufs);
        assert_eq!(q.pop(&mem), Err(QueueError::Indirect));

        let (mut drv, mut q) = Driver::new(&mem, BASE, 4);
        q.indirect = true;
        let many: Vec<_> = (0..40)
            .map(|i| (BASE + 0x8000 + i * 0x100, 8, i >= 20))
            .collect();
        let head = drv.add_indirect(&mem, table, &many);
        let c = q.pop(&mem).unwrap().unwrap();
        assert_eq!(c.head, head);
        assert_eq!((c.readable.len(), c.writable.len()), (20, 20));
        assert_eq!(
            c.readable[19],
            Buf {
                addr: BASE + 0x8000 + 19 * 0x100,
                len: 8
            }
        );
        assert_eq!(q.pop(&mem), Ok(None));
    }

    #[test]
    fn rejects_malformed_indirect_tables() {
        let mem = memory();
        let table = BASE + 0x6000;
        let fresh = || {
            let (drv, mut q) = Driver::new(&mem, BASE, 4);
            q.indirect = true;
            (drv, q)
        };

        for len in [8u32, 0, 17, (MAX_INDIRECT + 1) * 16] {
            let (mut drv, mut q) = fresh();
            drv.write_desc(&mem, 0, table, len, DESC_F_INDIRECT, 0);
            drv.publish(&mem, 0);
            assert_eq!(q.pop(&mem), Err(QueueError::IndirectTable(len)));
        }

        let (mut drv, mut q) = fresh();
        drv.write_desc(&mem, 0, table, 16, DESC_F_INDIRECT | DESC_F_NEXT, 1);
        drv.publish(&mem, 0);
        assert_eq!(q.pop(&mem), Err(QueueError::Indirect));

        let (mut drv, mut q) = fresh();
        drv.add_indirect(&mem, table, &[(BASE + 0x1000, 4, false)]);
        mem.write_u16(table + 12, DESC_F_INDIRECT).unwrap();
        assert_eq!(q.pop(&mem), Err(QueueError::Indirect));

        let (mut drv, mut q) = fresh();
        drv.add_indirect(
            &mem,
            table,
            &[(BASE + 0x1000, 4, false), (BASE + 0x1000, 4, false)],
        );
        mem.write_u16(table + 16 + 12, DESC_F_NEXT).unwrap();
        mem.write_u16(table + 16 + 14, 0).unwrap();
        assert_eq!(q.pop(&mem), Err(QueueError::Loop));

        let (mut drv, mut q) = fresh();
        drv.add_indirect(
            &mem,
            table,
            &[(BASE + 0x1000, 4, false), (BASE + 0x1000, 4, false)],
        );
        mem.write_u16(table + 14, 7).unwrap();
        assert_eq!(q.pop(&mem), Err(QueueError::BadIndex(7)));

        let (mut drv, mut q) = fresh();
        drv.write_desc(&mem, 0, 0x1000, 32, DESC_F_INDIRECT, 0);
        drv.publish(&mem, 0);
        assert!(matches!(q.pop(&mem), Err(QueueError::Memory(_))));
        let (mut drv, mut q) = fresh();
        drv.write_desc(&mem, 0, u64::MAX - 8, 32, DESC_F_INDIRECT, 0);
        drv.publish(&mem, 0);
        assert!(matches!(q.pop(&mem), Err(QueueError::Memory(_))));
    }

    #[test]
    fn without_event_index_interrupts_follow_the_drivers_flag() {
        let mem = memory();
        let (mut drv, mut q) = Driver::new(&mem, BASE, 4);
        assert_eq!(q.needs_interrupt(&mem), Ok(false), "nothing was pushed");
        drv.add(&mem, &[(BASE + 0x1000, 4, true)]);
        assert_eq!(q.drain(&mem, |_| 0), Ok(true));
        assert_eq!(q.needs_interrupt(&mem), Ok(false), "already accounted for");

        drv.set_no_interrupt(&mem, true);
        drv.add(&mem, &[(BASE + 0x1000, 4, true)]);
        assert_eq!(q.drain(&mem, |_| 0), Ok(false));
    }

    #[test]
    fn event_index_suppresses_interrupts_until_the_named_index() {
        let mem = memory();
        let (mut drv, mut q) = Driver::new(&mem, BASE, 8);
        q.event_idx = true;

        drv.set_used_event(&mem, 2);
        let mut interrupts = Vec::new();
        for _ in 0..4 {
            drv.add(&mem, &[(BASE + 0x1000, 4, true)]);
            interrupts.push(q.drain(&mem, |_| 0).unwrap());
        }
        assert_eq!(interrupts, [false, false, true, false]);

        drv.set_used_event(&mem, 5);
        for _ in 0..3 {
            drv.add(&mem, &[(BASE + 0x1000, 4, true)]);
        }
        assert_eq!(q.drain(&mem, |_| 0), Ok(true));
    }

    #[test]
    fn event_index_tells_the_driver_when_to_notify() {
        let mem = memory();
        let (mut drv, mut q) = Driver::new(&mem, BASE, 8);
        q.event_idx = true;
        drv.add(&mem, &[(BASE + 0x1000, 4, true)]);
        drv.add(&mem, &[(BASE + 0x1000, 4, true)]);
        let mut served = 0;
        q.drain(&mem, |_| {
            served += 1;
            0
        })
        .unwrap();
        assert_eq!(served, 2);
        assert_eq!(drv.avail_event(&mem), 2, "notify me about the next one");

        q.pop(&mem).unwrap();
        drv.add(&mem, &[(BASE + 0x1000, 4, true)]);
        assert_eq!(q.enable_notifications(&mem), Ok(true));
    }

    #[test]
    fn a_head_is_read_after_the_index_that_published_it() {
        use std::sync::atomic::{AtomicU64, Ordering};
        use std::sync::Arc;

        const SIZE: u16 = 8;
        const ROUNDS: u64 = 3_000_000;
        let head_for = |n: u64| ((n % SIZE as u64 + n / SIZE as u64) % SIZE as u64) as u16;

        let mem = Arc::new(memory());
        let (drv, q) = Driver::new(&mem, BASE, SIZE);
        for i in 0..SIZE {
            drv.write_desc(&mem, i, BASE + 0x1000, 4, 0, 0);
        }
        let avail = drv.avail;
        let taken = Arc::new(AtomicU64::new(0));

        let device = {
            let (mem, taken) = (mem.clone(), taken.clone());
            let mut q = q;
            std::thread::spawn(move || {
                let mut stale = 0u64;
                let mut n = 0u64;
                while n < ROUNDS {
                    match q.pop(&mem).unwrap() {
                        Some(chain) => {
                            if chain.head != head_for(n) {
                                stale += 1;
                            }
                            n += 1;
                            taken.store(n, Ordering::Release);
                        }
                        None => std::hint::spin_loop(),
                    }
                }
                stale
            })
        };

        for n in 0..ROUNDS {
            while n - taken.load(Ordering::Acquire) >= SIZE as u64 {
                std::hint::spin_loop();
            }
            let slot = n % SIZE as u64;
            mem.write_u16(avail + 4 + slot * 2, head_for(n)).unwrap();
            std::sync::atomic::fence(Ordering::SeqCst);
            mem.write_u16(avail + 2, (n + 1) as u16).unwrap();
        }
        let stale = device.join().unwrap();
        assert_eq!(
            stale, 0,
            "heads read from a lap before the index was published, in {ROUNDS}"
        );
    }

    #[test]
    fn chain_copies_across_buffers() {
        let mem = memory();
        let (mut drv, mut q) = Driver::new(&mem, BASE, 8);
        mem.write(BASE + 0x1000, b"abc").unwrap();
        mem.write(BASE + 0x2000, b"defgh").unwrap();
        drv.add(
            &mem,
            &[
                (BASE + 0x1000, 3, false),
                (BASE + 0x2000, 5, false),
                (BASE + 0x3000, 2, true),
                (BASE + 0x4000, 4, true),
            ],
        );
        let c = q.pop(&mem).unwrap().unwrap();
        let mut out = [0u8; 6];
        assert!(c.read_exact(&mem, &mut out));
        assert_eq!(&out, b"abcdef");
        assert!(!c.read_exact(&mem, &mut [0u8; 9]));
        assert_eq!(c.write(&mem, b"12345"), 5);
        assert_eq!(c.write(&mem, b"1234567"), 6);
        let mut back = [0u8; 4];
        mem.read(BASE + 0x4000, &mut back).unwrap();
        assert_eq!(&back, b"3456");
    }
}
