use std::collections::{HashSet, VecDeque};
use std::ffi::{c_char, c_void, CString};
use std::sync::atomic::{AtomicU32, AtomicU64, Ordering};
use std::sync::{Arc, Condvar, Mutex};
use std::time::{Duration, Instant, SystemTime};

use crate::passthrough::Change;

type CFRef = *const c_void;

#[repr(C)]
struct Context {
    version: isize,
    info: *mut c_void,
    retain: *const c_void,
    release: *const c_void,
    copy_description: *const c_void,
}

type Callback = extern "C" fn(
    stream: CFRef,
    info: *mut c_void,
    count: usize,
    paths: *mut c_void,
    flags: *const u32,
    ids: *const u64,
);

const USE_CF_TYPES: u32 = 0x01;
const NO_DEFER: u32 = 0x02;
const WATCH_ROOT: u32 = 0x04;
const IGNORE_SELF: u32 = 0x08;
const FILE_EVENTS: u32 = 0x10;
const USE_EXTENDED_DATA: u32 = 0x40;
const UTF8: u32 = 0x0800_0100;
const NUMBER_SINT64: isize = 4;
const LATENCY: f64 = 0.02;
const HISTORY_DONE: u32 = 0x10;
const MARKS: u32 = 0x10 | 0x20 | 0x40 | 0x80;
const LOST: u32 = 0x1 | 0x2 | 0x4 | 0x8;

#[link(name = "CoreServices", kind = "framework")]
extern "C" {
    fn FSEventStreamCreate(
        allocator: CFRef,
        callback: Callback,
        context: *const Context,
        paths: CFRef,
        since: u64,
        latency: f64,
        flags: u32,
    ) -> *mut c_void;
    fn FSEventStreamSetDispatchQueue(stream: *mut c_void, queue: *mut c_void);
    fn FSEventStreamStart(stream: *mut c_void) -> u8;
    fn FSEventStreamStop(stream: *mut c_void);
    fn FSEventStreamInvalidate(stream: *mut c_void);
    fn FSEventStreamRelease(stream: *mut c_void);
    fn FSEventsGetCurrentEventId() -> u64;
}

const PATH_KEY: &[u8] = b"path";
const FILE_ID_KEY: &[u8] = b"fileID";

fn cfstring(bytes: &[u8]) -> CFRef {
    unsafe {
        CFStringCreateWithBytes(
            std::ptr::null(),
            bytes.as_ptr(),
            bytes.len() as isize,
            UTF8,
            0,
        )
    }
}

struct Info {
    sink: Sink,
    path_key: CFRef,
    file_id_key: CFRef,
    recent: Arc<Mutex<Recent>>,
    live: (Mutex<bool>, Condvar),
    born: Instant,
    prev_began: AtomicU64,
    prev_took: AtomicU64,
    worst_gap: AtomicU64,
    callbacks: AtomicU64,
}

const RECENT: usize = 16384;

#[derive(Default)]
pub struct Recent {
    order: VecDeque<(u64, u32)>,
    ids: HashSet<(u64, u32)>,
}

impl Recent {
    fn insert(&mut self, id: u64, flags: u32) -> bool {
        let id = (id, flags);
        if !self.ids.insert(id) {
            return false;
        }
        self.order.push_back(id);
        if self.order.len() > RECENT {
            if let Some(old) = self.order.pop_front() {
                self.ids.remove(&old);
            }
        }
        true
    }
}

#[link(name = "CoreFoundation", kind = "framework")]
extern "C" {
    static kCFTypeArrayCallBacks: c_void;
    fn CFStringCreateWithBytes(
        allocator: CFRef,
        bytes: *const u8,
        len: isize,
        encoding: u32,
        external: u8,
    ) -> CFRef;
    fn CFArrayCreate(
        allocator: CFRef,
        values: *const CFRef,
        count: isize,
        callbacks: *const c_void,
    ) -> CFRef;
    fn CFArrayGetCount(array: CFRef) -> isize;
    fn CFArrayGetValueAtIndex(array: CFRef, index: isize) -> CFRef;
    fn CFDictionaryGetValue(dict: CFRef, key: CFRef) -> CFRef;
    fn CFStringGetCString(string: CFRef, buf: *mut c_char, len: isize, encoding: u32) -> u8;
    fn CFNumberGetValue(number: CFRef, kind: isize, value: *mut c_void) -> u8;
    fn CFRelease(object: CFRef);
}

extern "C" {
    fn dispatch_queue_create(label: *const c_char, attr: *const c_void) -> *mut c_void;
    fn dispatch_queue_attr_make_with_qos_class(
        attr: *const c_void,
        qos: u32,
        relative: i32,
    ) -> *const c_void;
    fn dispatch_release(object: *mut c_void);
    fn dispatch_sync_f(queue: *mut c_void, context: *mut c_void, work: extern "C" fn(*mut c_void));
    fn qos_class_self() -> u32;
}

pub const QOS_USER_INTERACTIVE: u32 = 0x21;
pub const QOS_USER_INITIATED: u32 = 0x19;
pub const QOS_DEFAULT: u32 = 0x15;
pub const QOS_UTILITY: u32 = 0x11;
pub const QOS_UNSPECIFIED: u32 = 0;

static QUEUE_QOS: AtomicU32 = AtomicU32::new(u32::MAX);

fn queue_qos() -> u32 {
    match QUEUE_QOS.load(Ordering::Relaxed) {
        u32::MAX => {
            let qos = std::env::var("FEV_FSEVENTS_QOS")
                .ok()
                .and_then(|v| u32::from_str_radix(v.trim_start_matches("0x"), 16).ok())
                .unwrap_or(QOS_USER_INITIATED);
            QUEUE_QOS.store(qos, Ordering::Relaxed);
            qos
        }
        qos => qos,
    }
}

pub fn set_queue_qos(qos: u32) {
    QUEUE_QOS.store(qos, Ordering::Relaxed);
}

#[derive(Clone, Copy, Debug)]
pub struct Arrival {
    pub last: u64,
    pub at: SystemTime,
    pub gap: Option<Duration>,
    pub prev_took: Duration,
    pub worst_gap: Duration,
    pub callbacks: u64,
    pub events: usize,
    pub qos: u32,
    pub behind: u64,
}

pub type Sink = Box<dyn Fn(Vec<Change>, Arrival) + Send + Sync>;

pub struct Stream {
    stream: *mut c_void,
    queue: *mut c_void,
    info: *mut Info,
}

unsafe impl Send for Stream {}

pub fn current_event_id() -> u64 {
    unsafe { FSEventsGetCurrentEventId() }
}

extern "C" fn nothing(_: *mut c_void) {}

extern "C" fn on_events(
    _stream: CFRef,
    info: *mut c_void,
    count: usize,
    paths: *mut c_void,
    flags: *const u32,
    ids: *const u64,
) {
    let info = unsafe { &*(info as *const Info) };
    let began = info.born.elapsed().as_nanos() as u64 + 1;
    let before = info.prev_began.swap(began, Ordering::Relaxed);
    let gap = (before != 0).then(|| began.saturating_sub(before));
    let worst = info
        .worst_gap
        .fetch_max(gap.unwrap_or(0), Ordering::Relaxed)
        .max(gap.unwrap_or(0));
    let arrival = Arrival {
        last: 0,
        at: SystemTime::now(),
        gap: gap.map(Duration::from_nanos),
        prev_took: Duration::from_nanos(info.prev_took.load(Ordering::Relaxed)),
        worst_gap: Duration::from_nanos(worst),
        callbacks: info.callbacks.fetch_add(1, Ordering::Relaxed) + 1,
        events: count,
        qos: unsafe { qos_class_self() },
        behind: 0,
    };
    let mut recent = info.recent.lock().unwrap();
    let paths = paths as CFRef;
    let count = count.min(unsafe { CFArrayGetCount(paths) }.max(0) as usize);
    let mut batch = Vec::with_capacity(count);
    let mut last = 0;
    let mut lost = false;
    for i in 0..count {
        let (flags, id) = unsafe { (*flags.add(i), *ids.add(i)) };
        if flags & HISTORY_DONE != 0 {
            *info.live.0.lock().unwrap() = true;
            info.live.1.notify_all();
        }
        lost |= flags & LOST != 0;
        if flags & (MARKS | LOST) == 0 && !recent.insert(id, flags) {
            continue;
        }
        last = last.max(id);
        let dict = unsafe { CFArrayGetValueAtIndex(paths, i as isize) };
        let mut buf = [0 as c_char; 4096];
        let path = unsafe { CFDictionaryGetValue(dict, info.path_key) };
        if path.is_null()
            || unsafe { CFStringGetCString(path, buf.as_mut_ptr(), buf.len() as isize, UTF8) } == 0
        {
            continue;
        }
        let len = buf.iter().position(|b| *b == 0).unwrap_or(0);
        let path: Vec<u8> = buf[..len].iter().map(|b| *b as u8).collect();
        let mut ino: i64 = 0;
        let number = unsafe { CFDictionaryGetValue(dict, info.file_id_key) };
        if !number.is_null() {
            unsafe { CFNumberGetValue(number, NUMBER_SINT64, (&mut ino as *mut i64).cast()) };
        }
        batch.push(Change {
            path,
            flags,
            ino: ino as u64,
        });
    }
    if !batch.is_empty() {
        let behind = if lost {
            current_event_id().saturating_sub(last)
        } else {
            0
        };
        (info.sink)(
            batch,
            Arrival {
                last,
                behind,
                ..arrival
            },
        );
    }
    drop(recent);
    let took = (info.born.elapsed().as_nanos() as u64 + 1).saturating_sub(began);
    info.prev_took.store(took, Ordering::Relaxed);
}

unsafe fn release_info(info: *mut Info) {
    let info = Box::from_raw(info);
    CFRelease(info.path_key);
    CFRelease(info.file_id_key);
}

impl Stream {
    pub fn start(
        paths: &[Vec<u8>],
        since: u64,
        recent: Arc<Mutex<Recent>>,
        sink: Sink,
    ) -> std::io::Result<Stream> {
        let fail = |what: &str| std::io::Error::other(format!("FSEvents: {what}"));
        let strings: Vec<CFRef> = paths.iter().map(|p| cfstring(p)).collect();
        let release = |strings: &[CFRef]| {
            for s in strings.iter().filter(|s| !s.is_null()) {
                unsafe { CFRelease(*s) };
            }
        };
        if strings.iter().any(|s| s.is_null()) {
            release(&strings);
            return Err(fail("a path is not valid UTF-8"));
        }
        let array = unsafe {
            CFArrayCreate(
                std::ptr::null(),
                strings.as_ptr(),
                strings.len() as isize,
                &raw const kCFTypeArrayCallBacks,
            )
        };
        release(&strings);
        if array.is_null() {
            return Err(fail("no array"));
        }
        let info = Box::into_raw(Box::new(Info {
            sink,
            path_key: cfstring(PATH_KEY),
            file_id_key: cfstring(FILE_ID_KEY),
            recent,
            live: (Mutex::new(false), Condvar::new()),
            born: Instant::now(),
            prev_began: AtomicU64::new(0),
            prev_took: AtomicU64::new(0),
            worst_gap: AtomicU64::new(0),
            callbacks: AtomicU64::new(0),
        }));
        let context = Context {
            version: 0,
            info: info.cast(),
            retain: std::ptr::null(),
            release: std::ptr::null(),
            copy_description: std::ptr::null(),
        };
        let stream = unsafe {
            FSEventStreamCreate(
                std::ptr::null(),
                on_events,
                &context,
                array,
                since,
                LATENCY,
                USE_CF_TYPES
                    | NO_DEFER
                    | WATCH_ROOT
                    | IGNORE_SELF
                    | FILE_EVENTS
                    | USE_EXTENDED_DATA,
            )
        };
        unsafe { CFRelease(array) };
        let label = CString::new("fev.fileshare.events").expect("no NUL");
        let attr = match queue_qos() {
            QOS_UNSPECIFIED => std::ptr::null(),
            qos => unsafe { dispatch_queue_attr_make_with_qos_class(std::ptr::null(), qos, 0) },
        };
        let queue = unsafe { dispatch_queue_create(label.as_ptr(), attr) };
        if stream.is_null() || queue.is_null() {
            unsafe {
                if !stream.is_null() {
                    FSEventStreamRelease(stream);
                }
                if !queue.is_null() {
                    dispatch_release(queue);
                }
                release_info(info);
            }
            return Err(fail("the stream could not be created"));
        }
        let me = Stream {
            stream,
            queue,
            info,
        };
        unsafe { FSEventStreamSetDispatchQueue(stream, queue) };
        if unsafe { FSEventStreamStart(stream) } == 0 {
            return Err(fail("the stream could not be started"));
        }
        Ok(me)
    }
}

impl Stream {
    pub fn wait_live(&self, limit: Duration) -> bool {
        let info = unsafe { &*self.info };
        let live = info.live.0.lock().unwrap();
        let (live, _) = info
            .live
            .1
            .wait_timeout_while(live, limit, |live| !*live)
            .unwrap();
        *live
    }
}

impl Drop for Stream {
    fn drop(&mut self) {
        unsafe {
            FSEventStreamStop(self.stream);
            FSEventStreamInvalidate(self.stream);
            dispatch_sync_f(self.queue, std::ptr::null_mut(), nothing);
            FSEventStreamRelease(self.stream);
            dispatch_release(self.queue);
            release_info(self.info);
        }
    }
}
