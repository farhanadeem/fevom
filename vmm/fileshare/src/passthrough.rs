use std::collections::HashMap;
use std::ffi::CString;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::os::unix::ffi::OsStrExt;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, RwLock, RwLockReadGuard};
use std::time::Duration;

use crate::abi::{ROOT_ID, UNKNOWN_INO};
use crate::errno::Errno;
use crate::fs::{Attr, Ctx, DirSink, Entry, Filesystem, Need, Result, SetAttr, Statfs, Xattr};
use crate::iov::IoVecs;
use crate::server::{LONG, VALID};

const NAME_MAX: usize = 255;
const RESERVED_DESCRIPTORS: u64 = 8192;
const MAX_HANDLES: usize = 32768;
const MAX_NODES: usize = 4 << 20;
const MAX_LISTED: usize = 2 << 20;
const IOV_BATCH: usize = 512;
const MAX_ABSENT: usize = 1 << 20;

const FOPEN_KEEP_CACHE: u32 = 1 << 1;
const FOPEN_CACHE_DIR: u32 = 1 << 3;

const S_IFMT: u32 = 0o170000;
const S_IFDIR: u32 = 0o040000;
const SF_DATALESS: u32 = 0x4000_0000;
const S_IFREG: u32 = 0o100000;
const S_IFLNK: u32 = 0o120000;
const S_IFCHR: u32 = 0o020000;
const S_IFBLK: u32 = 0o060000;

const S_IFIFO: u32 = 0o010000;
const S_IFSOCK: u32 = 0o140000;
const S_ISUID: u32 = 0o4000;
const S_ISGID: u32 = 0o2000;
const S_ISVTX: u32 = 0o1000;

const NODE_KEY: &[u8] = b"com.fev.node";
const PLACEHOLDER_MODE: u32 = 0o600 | S_ISVTX;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum Sync {
    Fsync,
    #[default]
    Barrier,
    Full,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Config {
    pub root: PathBuf,
    pub read_only: bool,
    pub sync: Sync,
    pub deny: Vec<PathBuf>,
    pub max_handles: Option<usize>,
    pub max_nodes: Option<usize>,
    pub max_listed: Option<usize>,
    pub no_volfs: bool,
}

pub type OnDenied = Arc<dyn Fn(&[u8]) + Send + std::marker::Sync>;

pub const MAX_DENIED: usize = 32;

impl Config {
    pub fn new(root: impl Into<PathBuf>) -> Self {
        Self {
            root: root.into(),
            read_only: false,
            sync: Sync::default(),
            deny: Vec::new(),
            max_handles: None,
            max_nodes: None,
            max_listed: None,
            no_volfs: false,
        }
    }
}

#[derive(Debug, Clone)]
struct Place {
    path: Vec<u8>,
    parent: Vec<u8>,
    name: Vec<u8>,
    dev: i32,
    parent_id: (i32, u64),
}

impl Place {
    fn of(path: &[u8]) -> Option<Place> {
        let cut = path.iter().rposition(|b| *b == b'/')?;
        let (parent, name) = (&path[..cut.max(1)], &path[cut + 1..]);
        if name.is_empty() {
            return None;
        }
        let st = lstat(parent).ok()?;
        Some(Place {
            path: path.to_vec(),
            parent: parent.to_vec(),
            name: name.to_vec(),
            dev: st.st_dev,
            parent_id: (st.st_dev, st.st_ino),
        })
    }

    fn may_be(&self, name: &[u8]) -> bool {
        !name.is_ascii() || !self.name.is_ascii() || name.eq_ignore_ascii_case(&self.name)
    }

    fn would_make(&self, name: &[u8], case_sensitive: bool) -> bool {
        if !name.is_ascii() || !self.name.is_ascii() {
            return true;
        }
        if case_sensitive {
            name == &self.name[..]
        } else {
            name.eq_ignore_ascii_case(&self.name)
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct Volume {
    volfs: bool,
    case_sensitive: bool,
    xattrs: bool,
    hidden: bool,
    local: bool,
    boot: bool,
    nosuid: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
struct FsKind {
    devfs: bool,
    local: bool,
    boot: bool,
    nosuid: bool,
}

fn fs_kind(path: &[u8]) -> FsKind {
    let mut s = unsafe { std::mem::zeroed::<libc::statfs>() };
    let asked = match open(path, libc::O_RDONLY | libc::O_DIRECTORY) {
        Ok(fd) => unsafe { libc::fstatfs(fd.as_raw_fd(), &mut s) },
        Err(_) => {
            let Ok(c) = cstr(path) else {
                return FsKind::default();
            };
            unsafe { libc::statfs(c.as_ptr(), &mut s) }
        }
    };
    if asked != 0 {
        return FsKind::default();
    }
    let text = |raw: &[libc::c_char]| -> Vec<u8> {
        raw.iter()
            .take_while(|b| **b != 0)
            .map(|b| *b as u8)
            .collect()
    };
    let name = text(&s.f_fstypename);
    let on = text(&s.f_mntonname);
    let local = s.f_flags & libc::MNT_LOCAL as u32 != 0;
    FsKind {
        devfs: name == b"devfs" || name == b"fdesc",
        local,
        boot: local && (on == b"/" || on.starts_with(b"/System/Volumes/")),
        nosuid: s.f_flags & libc::MNT_NOSUID as u32 != 0,
    }
}

fn on_devfs(path: &[u8]) -> bool {
    fs_kind(path).devfs
}

#[derive(Debug, Clone, PartialEq, Eq, Hash)]
enum Key {
    Vol { dev: i32, ino: u64 },
    Tree { parent: u64, name: Box<[u8]> },
    Gone,
}

struct Node {
    key: Key,
    kind: u32,
    dev: i32,
    nlookup: u64,
    children: u64,
    crossing: bool,
    open: u32,
    orphan: Option<Arc<OwnedFd>>,
    dirty: Option<Arc<AtomicBool>>,
    at: Option<(u64, Box<[u8]>)>,
    announced: bool,
    watch: (u64, bool),
    free: bool,
    dir_ok: bool,
    dataless: bool,
    mounts: bool,
    complete: bool,
}

type Names = HashMap<u64, HashMap<Box<[u8]>, u64>>;

struct Nodes {
    by_id: HashMap<u64, Node>,
    by_key: HashMap<Key, u64>,
    next: u64,
    max: usize,
    told: Names,
    displaced: Names,
    names: usize,
    orphans: Arc<AtomicUsize>,
    watch_roots: std::collections::HashSet<(i32, u64)>,
    watch_epoch: u64,
    fixed: bool,
    remote: std::collections::HashSet<i32>,
    absent: HashMap<u64, Folded>,
    absent_len: usize,
}

type Folded = HashMap<Box<[u8]>, Vec<Box<[u8]>>>;
type RootId = (Vec<u8>, (i32, u64));

fn fold(name: &[u8]) -> Box<[u8]> {
    if name.is_ascii() {
        name.to_ascii_lowercase().into()
    } else {
        Box::default()
    }
}

fn names_remove(map: &mut Names, parent: u64, name: &[u8]) -> Option<u64> {
    let names = map.get_mut(&parent)?;
    let id = names.remove(name);
    if names.is_empty() {
        map.remove(&parent);
    }
    id
}

impl Nodes {
    fn enter(&mut self, key: Key, kind: u32, dev: i32, crossing: bool) -> Result<u64> {
        if let Some(id) = self.by_key.get(&key) {
            let node = self.by_id.get_mut(id).expect("indexed node");
            node.nlookup = node.nlookup.saturating_add(1);
            (node.kind, node.dev, node.crossing) = (kind, dev, crossing);
            return Ok(*id);
        }
        if self.by_id.len() >= self.max {
            return Err(Errno::ENOMEM);
        }
        let id = self.next;
        self.next += 1;
        if let Key::Tree { parent, .. } = &key {
            if let Some(p) = self.by_id.get_mut(parent) {
                p.children += 1;
            }
        }
        self.by_key.insert(key.clone(), id);
        self.by_id.insert(
            id,
            Node {
                key,
                kind,
                dev,
                nlookup: 1,
                children: 0,
                crossing,
                open: 0,
                orphan: None,
                dirty: None,
                at: None,
                announced: false,
                watch: (0, false),
                free: false,
                dir_ok: false,
                dataless: false,
                mounts: false,
                complete: false,
            },
        );
        Ok(id)
    }

    fn tell(&mut self, parent: u64, name: &[u8], id: u64) {
        self.present(parent, name);
        let names = self.told.entry(parent).or_default();
        match names.get_mut(name) {
            Some(old) if *old == id => {
                let here = self
                    .by_id
                    .get(&id)
                    .and_then(|n| n.at.as_ref())
                    .is_some_and(|(p, n)| *p == parent && **n == *name);
                if !here {
                    if let Some(node) = self.by_id.get_mut(&id) {
                        node.at = Some((parent, name.into()));
                    }
                    self.moved(id);
                }
                return;
            }
            Some(old) => {
                let was = std::mem::replace(old, id);
                self.displace(parent, name, was);
                self.moved(was);
            }
            None if self.names >= self.max => return,
            None => {
                names.insert(name.into(), id);
                self.names += 1;
            }
        }
        if let Some(node) = self.by_id.get_mut(&id) {
            node.at = Some((parent, name.into()));
        }
        self.moved(id);
    }

    fn watched(&mut self, id: u64) -> bool {
        if self.fixed {
            return true;
        }
        if self.watch_roots.is_empty() {
            return false;
        }
        let mut chain: Vec<u64> = Vec::new();
        let mut cur = id;
        let verdict = loop {
            let Some(n) = self.by_id.get(&cur) else {
                break false;
            };
            if n.watch.0 == self.watch_epoch {
                break n.watch.1;
            }
            chain.push(cur);
            let Key::Vol { dev, ino } = n.key else {
                break false;
            };
            if self.remote.contains(&dev) {
                break false;
            }
            if self.watch_roots.contains(&(dev, ino)) {
                break true;
            }
            if chain.len() > 4096 {
                break false;
            }
            match &n.at {
                Some((p, name)) if self.told.get(p).and_then(|m| m.get(name)) == Some(&cur) => {
                    cur = *p
                }
                _ => break false,
            }
        };
        for c in chain {
            if let Some(n) = self.by_id.get_mut(&c) {
                n.watch = (self.watch_epoch, verdict);
            }
        }
        verdict
    }

    fn moved(&mut self, id: u64) {
        match self.by_id.get_mut(&id) {
            Some(n) if n.kind == S_IFDIR => self.watch_epoch += 1,
            Some(n) => n.watch = (0, false),
            None => {}
        }
    }

    fn absent(&mut self, parent: u64, name: &[u8]) -> bool {
        let names = self
            .absent
            .entry(parent)
            .or_default()
            .entry(fold(name))
            .or_default();
        if names.iter().any(|n| &**n == name) {
            return true;
        }
        if self.absent_len >= MAX_ABSENT {
            if names.is_empty() {
                self.forget_absent(parent, name);
            }
            return false;
        }
        names.push(name.into());
        self.absent_len += 1;
        true
    }

    fn forget_absent(&mut self, parent: u64, name: &[u8]) {
        if let Some(dir) = self.absent.get_mut(&parent) {
            let key = fold(name);
            if dir.get(&key).is_some_and(|n| n.is_empty()) {
                dir.remove(&key);
            }
            if dir.is_empty() {
                self.absent.remove(&parent);
            }
        }
    }

    fn present(&mut self, parent: u64, name: &[u8]) -> bool {
        let Some(names) = self
            .absent
            .get_mut(&parent)
            .and_then(|dir| dir.get_mut(&fold(name)))
        else {
            return false;
        };
        let before = names.len();
        names.retain(|n| &**n != name);
        let had = names.len() < before;
        if had {
            self.absent_len -= 1;
        }
        self.forget_absent(parent, name);
        had
    }

    fn appeared(&mut self, parent: u64, name: &[u8], most: usize) -> Option<Vec<Box<[u8]>>> {
        let Some(dir) = self.absent.get_mut(&parent) else {
            return Some(Vec::new());
        };
        let mut out = Vec::new();
        if name.is_ascii() {
            for key in [fold(name), Box::default()] {
                out.extend(dir.remove(&key).unwrap_or_default());
            }
        } else {
            out.extend(std::mem::take(dir).into_values().flatten());
        }
        if dir.is_empty() {
            self.absent.remove(&parent);
        }
        self.absent_len -= out.len();
        (out.len() <= most).then_some(out)
    }

    fn displace(&mut self, parent: u64, name: &[u8], was: u64) {
        if !self.by_id.contains_key(&was) || self.names >= self.max {
            return;
        }
        if self
            .displaced
            .entry(parent)
            .or_default()
            .insert(name.into(), was)
            .is_none()
        {
            self.names += 1;
        }
    }

    fn missed(&mut self, parent: u64, name: &[u8]) {
        if let Some(was) = self.untell(parent, name) {
            self.displace(parent, name, was);
        }
    }

    fn untell(&mut self, parent: u64, name: &[u8]) -> Option<u64> {
        let id = names_remove(&mut self.told, parent, name)?;
        self.names -= 1;
        self.moved(id);
        Some(id)
    }

    fn take_displaced(&mut self, parent: u64, name: &[u8]) -> Option<u64> {
        let id = names_remove(&mut self.displaced, parent, name)?;
        self.names -= 1;
        self.by_id.contains_key(&id).then_some(id)
    }

    fn orphaned(&mut self, id: u64) {
        let Some(node) = self.by_id.get_mut(&id) else {
            return;
        };
        let key = std::mem::replace(&mut node.key, Key::Gone);
        self.by_key.remove(&key);
        if let Key::Tree { parent, .. } = key {
            if let Some(p) = self.by_id.get_mut(&parent) {
                p.children = p.children.saturating_sub(1);
            }
            self.reap(parent);
        }
    }

    fn rekey(&mut self, id: u64, new: Key) {
        if let Some(old) = self.by_key.get(&new).copied() {
            if old == id {
                return;
            }
            self.orphaned(old);
        }
        let Some(node) = self.by_id.get_mut(&id) else {
            return;
        };
        let old = std::mem::replace(&mut node.key, new.clone());
        self.by_key.remove(&old);
        if let Key::Tree { parent, .. } = &new {
            if let Some(p) = self.by_id.get_mut(parent) {
                p.children += 1;
            }
        }
        self.by_key.insert(new, id);
        if let Key::Tree { parent, .. } = old {
            if let Some(p) = self.by_id.get_mut(&parent) {
                p.children = p.children.saturating_sub(1);
            }
            self.reap(parent);
        }
    }

    fn forget(&mut self, id: u64, n: u64) {
        let Some(node) = self.by_id.get_mut(&id) else {
            return;
        };
        node.nlookup = node.nlookup.saturating_sub(n);
        self.reap(id);
    }

    fn reap(&mut self, mut id: u64) {
        while id != ROOT_ID {
            match self.by_id.get(&id) {
                Some(n) if n.nlookup == 0 && n.children == 0 => {}
                _ => return,
            }
            let node = self.by_id.remove(&id).expect("checked above");
            if node.orphan.is_some() {
                self.orphans.fetch_sub(1, Ordering::Relaxed);
            }
            if node.key != Key::Gone {
                self.by_key.remove(&node.key);
            }
            if let Some((parent, name)) = &node.at {
                if self.told.get(parent).and_then(|n| n.get(name)) == Some(&id) {
                    self.untell(*parent, name);
                }
                if self.displaced.get(parent).and_then(|n| n.get(name)) == Some(&id) {
                    self.take_displaced(*parent, name);
                }
            }
            for map in [&mut self.told, &mut self.displaced] {
                if let Some(names) = map.remove(&id) {
                    self.names -= names.len();
                    self.watch_epoch += 1;
                }
            }
            if let Some(dir) = self.absent.remove(&id) {
                self.absent_len -= dir.values().map(Vec::len).sum::<usize>();
            }
            let Key::Tree { parent, .. } = node.key else {
                return;
            };
            match self.by_id.get_mut(&parent) {
                Some(p) => p.children = p.children.saturating_sub(1),
                None => return,
            }
            id = parent;
        }
    }
}

struct Handle {
    node: u64,
    fd: OwnedFd,
    dir: Option<Mutex<Option<Listing>>>,
    dirty: Option<Arc<AtomicBool>>,
    append: bool,
    caps_seen: AtomicU64,
}

struct Listing {
    entries: Vec<DirEnt>,
    bulk: Option<bulk::Bulk>,
    listed: Arc<AtomicUsize>,
}

impl Listing {
    fn weight(&self) -> usize {
        self.entries.len() + 2 * self.bulk.as_ref().map_or(0, |b| b.st.len())
    }
}

impl Drop for Listing {
    fn drop(&mut self) {
        self.listed.fetch_sub(self.weight(), Ordering::Relaxed);
    }
}

struct DirEnt {
    name: Box<[u8]>,
    ino: u64,
    kind: u8,
}

#[derive(Default)]
struct Listings {
    map: HashMap<(u64, u32), Arc<Listing>>,
    order: std::collections::VecDeque<(u64, u32)>,
}

const DIR_SLOTS: usize = 4;

const LISTING_PLACE_BITS: u32 = 21;
const LISTINGS_A_DIRECTORY: u32 = 1 << 10;

fn listing_offset(number: u32, next: usize) -> u64 {
    ((number as u64) << LISTING_PLACE_BITS) | (next as u64 & ((1 << LISTING_PLACE_BITS) - 1))
}

struct Handles {
    map: HashMap<u64, Arc<Handle>>,
    next: u64,
}

#[derive(Default)]
struct Closer {
    state: Mutex<Closing>,
    work: std::sync::Condvar,
}

#[derive(Default)]
struct Closing {
    fds: Vec<OwnedFd>,
    started: bool,
    stop: bool,
}

impl Closer {
    fn close(self: &Arc<Self>, fd: OwnedFd) {
        let mut state = self.state.lock().unwrap();
        if !state.started {
            let closer = self.clone();
            let spawned = std::thread::Builder::new()
                .name("fileshare-close".into())
                .spawn(move || closer.run());
            if spawned.is_err() {
                return;
            }
            state.started = true;
        }
        let asleep = state.fds.is_empty();
        state.fds.push(fd);
        drop(state);
        if asleep {
            self.work.notify_one();
        }
    }

    fn run(&self) {
        let mut state = self.state.lock().unwrap();
        loop {
            while state.fds.is_empty() && !state.stop {
                state = self.work.wait(state).unwrap();
            }
            if state.fds.is_empty() {
                return;
            }
            let fds = std::mem::take(&mut state.fds);
            drop(state);
            drop(fds);
            state = self.state.lock().unwrap();
        }
    }
}

impl Drop for Passthrough {
    fn drop(&mut self) {
        self.closer.state.lock().unwrap().stop = true;
        self.closer.work.notify_one();
    }
}

pub struct Passthrough {
    root_path: Vec<u8>,
    root_dev: i32,
    root_volfs: bool,
    tree: RwLock<()>,
    tree_gate: Mutex<()>,
    mac_uid: u32,
    read_only: bool,
    rosetta: Option<(i32, u64, Vec<u8>)>,
    sync: Sync,
    caps_gen: AtomicU64,
    orphans: Arc<AtomicUsize>,
    denied: Vec<(i32, u64)>,
    places: Vec<Place>,
    max_handles: usize,
    listed: Arc<AtomicUsize>,
    max_listed: usize,
    no_volfs: bool,
    nodes: Mutex<Nodes>,
    handles: Mutex<Handles>,
    volumes: Mutex<HashMap<i32, Volume>>,
    feeding: AtomicBool,
    seen: Mutex<HashMap<(i32, u64), notify::State>>,
    on_denied: std::sync::OnceLock<OnDenied>,
    told_denied: Mutex<Vec<Vec<u8>>>,
    home: Vec<u8>,
    class_parents: Vec<(i32, u64)>,
    watch_paths: Mutex<Vec<RootId>>,
    listings: Mutex<Listings>,
    free_opendir: AtomicBool,
    fewer_requests: AtomicBool,
    complete_dirs: AtomicBool,
    free_fds: Mutex<free::Fds>,
    dir_fds: Mutex<free::Fds>,
    dir_slots: Mutex<std::collections::VecDeque<(u64, OwnedFd)>>,
    closer: Arc<Closer>,
    changes: AtomicU64,
    batch_began: AtomicU64,
    overdue: AtomicBool,
    batch_at: AtomicU64,
    watch_at: AtomicU64,
    touched: Mutex<HashMap<u64, u64>>,
    touched_all: AtomicU64,
    #[cfg(test)]
    pub(crate) batch_delay: Mutex<Option<Duration>>,
    #[cfg(test)]
    rescan_max: Mutex<Option<usize>>,
    #[cfg(test)]
    after_stat: Mutex<Option<Box<dyn FnOnce() + Send>>>,
}

extern "C" {
    fn freadlink(fd: libc::c_int, buf: *mut libc::c_char, size: libc::size_t) -> libc::ssize_t;
}

fn cstr(path: &[u8]) -> Result<CString> {
    CString::new(path).map_err(|_| Errno::EINVAL)
}

const AT_SYMLINK_NOFOLLOW_ANY: i32 = 0x0800;

fn through_a_link(e: Errno) -> Errno {
    if e == Errno::ELOOP {
        Errno::ESTALE
    } else {
        e
    }
}

fn lstat(path: &[u8]) -> Result<libc::stat> {
    let c = cstr(path)?;
    let mut st = unsafe { std::mem::zeroed::<libc::stat>() };
    if unsafe { libc::fstatat(libc::AT_FDCWD, c.as_ptr(), &mut st, AT_SYMLINK_NOFOLLOW_ANY) } != 0 {
        return Err(through_a_link(Errno::last()));
    }
    Ok(st)
}

fn fstat(fd: RawFd) -> Result<libc::stat> {
    let mut st = unsafe { std::mem::zeroed::<libc::stat>() };
    if unsafe { libc::fstat(fd, &mut st) } != 0 {
        return Err(Errno::last());
    }
    Ok(st)
}

fn open(path: &[u8], flags: i32) -> Result<OwnedFd> {
    let c = cstr(path)?;
    loop {
        let fd = unsafe { libc::open(c.as_ptr(), flags | libc::O_CLOEXEC | libc::O_NOFOLLOW_ANY) };
        if fd >= 0 {
            return Ok(unsafe { OwnedFd::from_raw_fd(fd) });
        }
        if std::io::Error::last_os_error().kind() != std::io::ErrorKind::Interrupted {
            return Err(through_a_link(Errno::last()));
        }
    }
}

fn home_dir() -> Vec<u8> {
    let pw = unsafe { libc::getpwuid(libc::geteuid()) };
    if pw.is_null() || unsafe { (*pw).pw_dir.is_null() } {
        return Vec::new();
    }
    let mut home = unsafe { std::ffi::CStr::from_ptr((*pw).pw_dir) }
        .to_bytes()
        .to_vec();
    while home.len() > 1 && home.ends_with(b"/") {
        home.pop();
    }
    home
}

fn protected_folder(path: &[u8], home: &[u8], is_dir: bool) -> Vec<u8> {
    let below = |base: &[u8], keep: usize| -> Option<Vec<u8>> {
        let rest = path.strip_prefix(base)?;
        let rest = if base.ends_with(b"/") {
            rest
        } else {
            rest.strip_prefix(b"/")?
        };
        let parts: Vec<&[u8]> = rest
            .split(|b| *b == b'/')
            .filter(|p| !p.is_empty())
            .collect();
        if parts.len() < keep {
            return None;
        }
        let mut out = base.to_vec();
        for part in &parts[..keep] {
            if !out.ends_with(b"/") {
                out.push(b'/');
            }
            out.extend_from_slice(part);
        }
        Some(out)
    };
    if !home.is_empty() {
        for name in [
            "Desktop",
            "Documents",
            "Downloads",
            "Library/Mobile Documents",
        ] {
            let mut base = home.to_vec();
            base.push(b'/');
            base.extend_from_slice(name.as_bytes());
            if path == &base[..] || below(&base, 0).is_some() {
                return base;
            }
        }
        let mut cloud = home.to_vec();
        cloud.extend_from_slice(b"/Library/CloudStorage");
        if let Some(provider) = below(&cloud, 1) {
            return provider;
        }
    }
    if let Some(volume) = below(b"/Volumes", 1) {
        return volume;
    }
    if is_dir {
        return path.to_vec();
    }
    match path.iter().rposition(|b| *b == b'/') {
        Some(0) | None => b"/".to_vec(),
        Some(i) => path[..i].to_vec(),
    }
}

fn full_path(volfs: &[u8]) -> Option<Vec<u8>> {
    let list = AttrList {
        bitmapcount: ATTR_BIT_MAP_COUNT,
        reserved: 0,
        commonattr: ATTR_CMN_FULLPATH,
        volattr: 0,
        dirattr: 0,
        fileattr: 0,
        forkattr: 0,
    };
    let mut buf = [0u8; 12 + 1024];
    getattrlist(volfs, &list, &mut buf).ok()?;
    let offset = i32::from_le_bytes(buf[4..8].try_into().unwrap());
    let len = u32::from_le_bytes(buf[8..12].try_into().unwrap()) as usize;
    let start = 4usize.checked_add(usize::try_from(offset).ok()?)?;
    let path = buf.get(start..start.checked_add(len)?)?;
    Some(path.split(|b| *b == 0).next()?.to_vec())
}

#[repr(C)]
struct AttrList {
    bitmapcount: u16,
    reserved: u16,
    commonattr: u32,
    volattr: u32,
    dirattr: u32,
    fileattr: u32,
    forkattr: u32,
}

const ATTR_BIT_MAP_COUNT: u16 = 5;
const ATTR_CMN_NAME: u32 = 0x0000_0001;
const ATTR_CMN_FULLPATH: u32 = 0x0800_0000;
const ATTR_VOL_INFO: u32 = 0x8000_0000;
const ATTR_VOL_CAPABILITIES: u32 = 0x0002_0000;
const VOL_CAP_FMT_PERSISTENTOBJECTIDS: u32 = 0x0000_0001;
const VOL_CAP_FMT_CASE_SENSITIVE: u32 = 0x0000_0100;
const VOL_CAP_FMT_PATH_FROM_ID: u32 = 0x0002_0000;
const VOL_CAP_INT_EXTENDED_ATTR: u32 = 0x0000_4000;
const FSOPT_NOFOLLOW: u32 = 0x0000_0001;
const FSOPT_NOFOLLOW_ANY: u32 = 0x0000_0800;

fn getattrlist(path: &[u8], list: &AttrList, buf: &mut [u8]) -> Result<()> {
    let c = cstr(path)?;
    let ret = unsafe {
        libc::getattrlist(
            c.as_ptr(),
            (list as *const AttrList).cast_mut().cast(),
            buf.as_mut_ptr().cast(),
            buf.len(),
            FSOPT_NOFOLLOW | FSOPT_NOFOLLOW_ANY,
        )
    };
    if ret != 0 {
        return Err(Errno::last());
    }
    Ok(())
}

fn name_on_disk(path: &[u8]) -> Option<Vec<u8>> {
    let list = AttrList {
        bitmapcount: ATTR_BIT_MAP_COUNT,
        reserved: 0,
        commonattr: ATTR_CMN_NAME,
        volattr: 0,
        dirattr: 0,
        fileattr: 0,
        forkattr: 0,
    };
    let mut buf = [0u8; 12 + 1024];
    getattrlist(path, &list, &mut buf).ok()?;
    let offset = i32::from_le_bytes(buf[4..8].try_into().unwrap());
    let len = u32::from_le_bytes(buf[8..12].try_into().unwrap()) as usize;
    let start = 4usize.checked_add(usize::try_from(offset).ok()?)?;
    let name = buf.get(start..start.checked_add(len)?)?;
    Some(name.split(|b| *b == 0).next()?.to_vec())
}

fn volume_capabilities(path: &[u8]) -> Option<Volume> {
    let list = AttrList {
        bitmapcount: ATTR_BIT_MAP_COUNT,
        reserved: 0,
        commonattr: 0,
        volattr: ATTR_VOL_INFO | ATTR_VOL_CAPABILITIES,
        dirattr: 0,
        fileattr: 0,
        forkattr: 0,
    };
    let mut buf = [0u8; 4 + 32];
    getattrlist(path, &list, &mut buf).ok()?;
    let word = |i: usize| u32::from_le_bytes(buf[4 + i * 4..][..4].try_into().unwrap());
    let format = word(0) & word(4);
    let interfaces = word(1) & word(5);
    Some(Volume {
        volfs: format & VOL_CAP_FMT_PATH_FROM_ID != 0
            && format & VOL_CAP_FMT_PERSISTENTOBJECTIDS != 0,
        case_sensitive: format & VOL_CAP_FMT_CASE_SENSITIVE != 0,
        xattrs: interfaces & VOL_CAP_INT_EXTENDED_ATTR != 0,
        hidden: false,
        local: false,
        boot: false,
        nosuid: false,
    })
}

fn volfs_path(dev: i32, ino: u64) -> Vec<u8> {
    format!("/.vol/{}/{ino}", dev as u32).into_bytes()
}

fn linux_rdev(rdev: i32) -> u32 {
    let (major, minor) = ((rdev as u32) >> 24, (rdev as u32) & 0x00ff_ffff);
    (minor & 0xff) | (major << 8) | ((minor & !0xff) << 12)
}

impl Passthrough {
    fn handle_limit() -> usize {
        let mut lim = libc::rlimit {
            rlim_cur: 0,
            rlim_max: 0,
        };
        if unsafe { libc::getrlimit(libc::RLIMIT_NOFILE, &mut lim) } != 0 {
            return 64;
        }
        let want = (MAX_HANDLES as u64 + RESERVED_DESCRIPTORS).min(61_440);
        if lim.rlim_cur < want {
            let raised = libc::rlimit {
                rlim_cur: want.min(lim.rlim_max),
                rlim_max: lim.rlim_max,
            };
            if unsafe { libc::setrlimit(libc::RLIMIT_NOFILE, &raised) } == 0 {
                lim = raised;
            }
        }
        (lim.rlim_cur.min(want).saturating_sub(RESERVED_DESCRIPTORS) as usize)
            .clamp(64, MAX_HANDLES)
    }

    pub fn new(cfg: &Config) -> std::io::Result<Self> {
        let io = |e: Errno, what: &str, p: &std::path::Path| {
            std::io::Error::other(format!("{what} {}: Linux errno {}", p.display(), e.0))
        };
        let root_path = std::fs::canonicalize(&cfg.root)?
            .as_os_str()
            .as_bytes()
            .to_vec();
        let st = lstat(&root_path).map_err(|e| io(e, "share root", &cfg.root))?;
        if st.st_mode as u32 & S_IFMT != S_IFDIR {
            return Err(std::io::Error::other(format!(
                "share root {} is not a directory",
                cfg.root.display()
            )));
        }
        let mut denied = Vec::new();
        let mut places = Vec::new();
        for path in &cfg.deny {
            let meta = std::fs::metadata(path)
                .map_err(|e| std::io::Error::other(format!("deny={}: {e}", path.display())))?;
            use std::os::unix::fs::MetadataExt;
            denied.push((meta.dev() as i32, meta.ino()));
            let real = std::fs::canonicalize(path)
                .map_err(|e| std::io::Error::other(format!("deny={}: {e}", path.display())))?;
            if let Some(place) = Place::of(real.as_os_str().as_bytes()) {
                places.push(place);
            }
        }
        if let Ok(st) = lstat(b"/.vol") {
            denied.push((st.st_dev, st.st_ino));
        }

        let orphans = Arc::new(AtomicUsize::new(0));
        let mut fs = Self {
            root_path: root_path.clone(),
            root_dev: st.st_dev,
            root_volfs: false,
            tree: RwLock::new(()),
            tree_gate: Mutex::new(()),
            mac_uid: unsafe { libc::geteuid() },
            read_only: cfg.read_only,
            rosetta: None,
            sync: cfg.sync,
            caps_gen: AtomicU64::new(0),
            orphans: orphans.clone(),
            denied,
            places,
            max_handles: cfg.max_handles.unwrap_or_else(Self::handle_limit),
            listed: Arc::new(AtomicUsize::new(0)),
            max_listed: cfg.max_listed.unwrap_or(MAX_LISTED),
            no_volfs: cfg.no_volfs,
            nodes: Mutex::new(Nodes {
                by_id: HashMap::new(),
                by_key: HashMap::new(),
                next: ROOT_ID + 1,
                max: cfg.max_nodes.unwrap_or(MAX_NODES).max(1),
                told: HashMap::new(),
                displaced: HashMap::new(),
                names: 0,
                orphans,
                watch_roots: Default::default(),
                watch_epoch: 1,
                fixed: false,
                remote: Default::default(),
                absent: HashMap::new(),
                absent_len: 0,
            }),
            handles: Mutex::new(Handles {
                map: HashMap::new(),
                next: 1,
            }),
            volumes: Mutex::new(HashMap::new()),
            feeding: AtomicBool::new(false),
            seen: Mutex::new(HashMap::new()),
            on_denied: std::sync::OnceLock::new(),
            told_denied: Mutex::new(Vec::new()),
            home: home_dir(),
            class_parents: {
                let home = home_dir();
                let mut library = home.clone();
                library.extend_from_slice(b"/Library");
                [home, library]
                    .iter()
                    .filter(|p| !p.is_empty())
                    .filter_map(|p| lstat(p).ok())
                    .map(|st| (st.st_dev, st.st_ino))
                    .collect()
            },
            watch_paths: Mutex::new(Vec::new()),
            listings: Mutex::default(),
            free_opendir: AtomicBool::new(false),
            fewer_requests: AtomicBool::new(false),
            complete_dirs: AtomicBool::new(false),
            free_fds: Mutex::new(free::Fds::new(free::MAX_FREE_FDS)),
            dir_fds: Mutex::new(free::Fds::new(free::MAX_DIR_FDS)),
            dir_slots: Mutex::default(),
            closer: Arc::default(),
            changes: AtomicU64::new(0),
            batch_began: AtomicU64::new(0),
            overdue: AtomicBool::new(false),
            batch_at: AtomicU64::new(0),
            watch_at: AtomicU64::new(0),
            touched: Mutex::default(),
            touched_all: AtomicU64::new(0),
            #[cfg(test)]
            batch_delay: Mutex::new(None),
            #[cfg(test)]
            rescan_max: Mutex::new(None),
            #[cfg(test)]
            after_stat: Mutex::new(None),
        };
        fs.root_volfs = fs.install_root(&st);
        if fs.hidden(st.st_dev) {
            return Err(std::io::Error::other(format!(
                "share root {} is on the Mac's device filesystem, which is not shared",
                cfg.root.display()
            )));
        }
        Ok(fs)
    }

    fn install_root(&self, st: &libc::stat) -> bool {
        let vol = self.volume(st, &self.root_path);
        let key = if vol.volfs {
            Key::Vol {
                dev: st.st_dev,
                ino: st.st_ino,
            }
        } else {
            Key::Tree {
                parent: 0,
                name: Box::default(),
            }
        };
        let mut nodes = self.nodes.lock().unwrap();
        nodes.by_key.insert(key.clone(), ROOT_ID);
        nodes.by_id.insert(
            ROOT_ID,
            Node {
                key,
                kind: S_IFDIR,
                dev: st.st_dev,
                nlookup: 1,
                children: 0,
                crossing: false,
                open: 0,
                orphan: None,
                dirty: None,
                at: None,
                announced: true,
                watch: (0, false),
                free: false,
                dir_ok: false,
                dataless: false,
                mounts: false,
                complete: false,
            },
        );
        vol.volfs
    }

    fn tree_read(&self, node: u64) -> Option<RwLockReadGuard<'_, ()>> {
        let tree = matches!(
            self.nodes.lock().unwrap().by_id.get(&node).map(|n| &n.key),
            Some(Key::Tree { .. })
        );
        tree.then(|| {
            drop(self.tree_gate.lock().unwrap());
            self.tree.read().unwrap()
        })
    }

    fn tree_write(&self) -> std::sync::RwLockWriteGuard<'_, ()> {
        let _gate = self.tree_gate.lock().unwrap();
        self.tree.write().unwrap()
    }

    fn volume(&self, st: &libc::stat, path: &[u8]) -> Volume {
        if let Some(v) = self.volumes.lock().unwrap().get(&st.st_dev) {
            return *v;
        }
        let mut vol = volume_capabilities(path).unwrap_or(Volume {
            volfs: false,
            case_sensitive: false,
            xattrs: false,
            hidden: false,
            local: false,
            boot: false,
            nosuid: false,
        });
        let is_dir = st.st_mode as u32 & S_IFMT == S_IFDIR;
        if is_dir {
            let kind = fs_kind(path);
            (vol.hidden, vol.local, vol.boot, vol.nosuid) =
                (kind.devfs, kind.local, kind.boot, kind.nosuid);
        }
        if self.no_volfs {
            vol.volfs = false;
        }
        if !is_dir {
            vol.volfs = vol.volfs
                && matches!(
                    lstat(&volfs_path(st.st_dev, st.st_ino)),
                    Ok(again) if again.st_dev == st.st_dev && again.st_ino == st.st_ino
                );
            return vol;
        }
        if vol.volfs {
            vol.volfs = matches!(
                lstat(&volfs_path(st.st_dev, st.st_ino)),
                Ok(again) if again.st_dev == st.st_dev && again.st_ino == st.st_ino
            );
        }
        self.volumes.lock().unwrap().insert(st.st_dev, vol);
        vol
    }

    fn node_path(&self, id: u64) -> Result<(Vec<u8>, u32, i32)> {
        let nodes = self.nodes.lock().unwrap();
        let node = nodes.by_id.get(&id).ok_or(Errno::ESTALE)?;
        let (kind, dev) = (node.kind, node.dev);
        let mut names: Vec<&[u8]> = Vec::new();
        let mut cur = node;
        let mut path = loop {
            match &cur.key {
                Key::Vol { dev, ino } => break volfs_path(*dev, *ino),
                Key::Tree { parent: 0, .. } => break self.root_path.clone(),
                Key::Gone => return Err(Errno::ESTALE),
                Key::Tree { parent, name } => {
                    names.push(name);
                    cur = nodes.by_id.get(parent).ok_or(Errno::ESTALE)?;
                }
            }
        };
        for name in names.iter().rev() {
            path.push(b'/');
            path.extend_from_slice(name);
        }
        Ok((path, kind, dev))
    }

    fn vol_of(&self, dev: i32) -> Option<Volume> {
        self.volumes.lock().unwrap().get(&dev).copied()
    }

    fn hidden(&self, dev: i32) -> bool {
        self.volumes
            .lock()
            .unwrap()
            .get(&dev)
            .is_some_and(|v| v.hidden)
    }

    fn apple_double(&self, dev: i32, name: &[u8]) -> bool {
        name.starts_with(b"._")
            && self
                .volumes
                .lock()
                .unwrap()
                .get(&dev)
                .is_some_and(|v| !v.xattrs)
    }

    fn is_denied(&self, st: &libc::stat) -> bool {
        self.denied.contains(&(st.st_dev, st.st_ino))
    }

    fn is_place(&self, path: &[u8]) -> bool {
        self.places.iter().any(|p| p.path == path)
    }

    fn place_denied(&self, parent: u64, name: &[u8]) -> bool {
        if !self.places.iter().any(|p| p.may_be(name)) {
            return false;
        }
        let (key, dev) = {
            let nodes = self.nodes.lock().unwrap();
            match nodes.by_id.get(&parent) {
                Some(n) => (n.key.clone(), n.dev),
                None => return false,
            }
        };
        let mut dir: Option<Vec<u8>> = None;
        for p in self
            .places
            .iter()
            .filter(|p| p.dev == dev && p.may_be(name))
        {
            let here = match &key {
                Key::Vol { dev, ino } => (*dev, *ino) == p.parent_id,
                Key::Tree { .. } => {
                    if dir.is_none() {
                        dir = self.node_path(parent).ok().map(|d| d.0);
                    }
                    match (dir.as_deref().map(lstat), lstat(&p.parent)) {
                        (Some(Ok(a)), Ok(b)) => a.st_dev == b.st_dev && a.st_ino == b.st_ino,
                        _ => false,
                    }
                }
                Key::Gone => false,
            };
            if !here {
                continue;
            }
            if dir.is_none() {
                dir = self.node_path(parent).ok().map(|d| d.0);
            }
            let Some(d) = &dir else { return true };
            let mut child = d.clone();
            child.push(b'/');
            child.extend_from_slice(name);
            match (lstat(&child), lstat(&p.path)) {
                (Ok(a), Ok(b)) => {
                    if a.st_dev == b.st_dev
                        && a.st_ino == b.st_ino
                        && a.st_mode as u32 & S_IFMT == b.st_mode as u32 & S_IFMT
                    {
                        return true;
                    }
                }
                (Err(_), Err(_)) => {
                    let cs = self.vol_of(dev).is_some_and(|v| v.case_sensitive);
                    if p.would_make(name, cs) {
                        return true;
                    }
                }
                _ => {}
            }
        }
        false
    }

    fn present(&self, st: &libc::stat, path: &dyn Fn() -> Vec<u8>) -> (u32, u32) {
        let mode = st.st_mode as u32;
        let rdev = match mode & S_IFMT {
            S_IFCHR | S_IFBLK => linux_rdev(st.st_rdev),
            _ => 0,
        };
        if mode & S_IFMT != S_IFREG || mode & S_ISVTX == 0 || st.st_size != 0 {
            return (mode, rdev);
        }
        let (Ok(fd), Ok(key)) = (
            open(&path(), libc::O_EVTONLY | libc::O_NONBLOCK),
            cstr(NODE_KEY),
        ) else {
            return (mode, rdev);
        };
        if !fstat(fd.as_raw_fd())
            .is_ok_and(|now| now.st_dev == st.st_dev && now.st_ino == st.st_ino)
        {
            return (mode, rdev);
        }
        let mut raw = [0u8; 12];
        let n = unsafe {
            libc::fgetxattr(
                fd.as_raw_fd(),
                key.as_ptr(),
                raw.as_mut_ptr().cast(),
                raw.len(),
                0,
                0,
            )
        };
        if n != 12 {
            return (mode, rdev);
        }
        let word = |i: usize| u32::from_le_bytes(raw[i * 4..][..4].try_into().unwrap());
        let kind = word(0);
        if !matches!(kind, S_IFCHR | S_IFBLK | S_IFSOCK) {
            return (mode, rdev);
        }
        (kind | (word(2) & 0o7777), word(1))
    }

    fn placeholder(kind: u32, rdev: u32, perm: u32) -> [u8; 12] {
        let mut value = [0u8; 12];
        value[..4].copy_from_slice(&kind.to_le_bytes());
        value[4..8].copy_from_slice(&rdev.to_le_bytes());
        value[8..].copy_from_slice(&(perm & 0o7777).to_le_bytes());
        value
    }

    fn attr(&self, st: &libc::stat, node: u64, mode: u32, rdev: u32) -> Attr {
        let time = |s: i64, ns: i64| (s as u64, ns.clamp(0, 999_999_999) as u32);
        Attr {
            ino: if st.st_dev == self.root_dev && self.root_volfs {
                st.st_ino
            } else {
                (1 << 63) | node
            },
            size: st.st_size.max(0) as u64,
            blocks: st.st_blocks.max(0) as u64,
            atime: time(st.st_atime, st.st_atime_nsec),
            mtime: time(st.st_mtime, st.st_mtime_nsec),
            ctime: time(st.st_ctime, st.st_ctime_nsec),
            mode,
            nlink: st.st_nlink as u32,
            uid: st.st_uid,
            gid: st.st_gid,
            rdev,
            blksize: st.st_blksize.max(0) as u32,
            flags: if st.st_uid == self.mac_uid {
                crate::abi::ATTR_CALLER_OWNER
            } else {
                crate::abi::ATTR_REAL_OWNER
            },
        }
    }

    fn enter(
        &self,
        parent: (u64, i32),
        name: &[u8],
        st: &libc::stat,
        path: &dyn Fn() -> Vec<u8>,
        spelled: bool,
        since: u64,
    ) -> Result<Entry> {
        if self.is_denied(st) || self.place_denied(parent.0, name) {
            return Err(Errno::ENOENT);
        }
        let crossing = st.st_dev != parent.1;
        let known = self.volumes.lock().unwrap().get(&st.st_dev).copied();
        let vol = known.unwrap_or_else(|| self.volume(st, &path()));
        let mut on_disk = None;
        if !spelled && !crossing && (!vol.case_sensitive || !name.is_ascii()) {
            on_disk = name_on_disk(&path());
        }
        let exact = on_disk.as_deref().is_none_or(|d| d == name);
        let key = if vol.volfs {
            Key::Vol {
                dev: st.st_dev,
                ino: st.st_ino,
            }
        } else {
            Key::Tree {
                parent: parent.0,
                name: on_disk.as_deref().unwrap_or(name).into(),
            }
        };
        let (mode, rdev) = self.present(st, path);
        let (nodeid, name_watched, node_watched, free) = {
            let mut nodes = self.nodes.lock().unwrap();
            let changed = if !vol.local {
                nodes.remote.insert(st.st_dev)
            } else {
                !nodes.remote.is_empty() && nodes.remote.remove(&st.st_dev)
            };
            if changed {
                nodes.watch_epoch += 1;
            }
            let id = nodes.enter(key, mode & S_IFMT, st.st_dev, crossing)?;
            nodes.tell(parent.0, name, id);
            if crossing && !vol.boot {
                if let Some(p) = nodes.by_id.get_mut(&parent.0) {
                    p.mounts = true;
                }
            }
            let free = self.fewer_requests.load(Ordering::Relaxed)
                && mode & S_IFMT == S_IFREG
                && vol.volfs
                && vol.boot
                && !crossing
                && nodes.by_id.get(&parent.0).is_some_and(|p| p.dir_ok);
            if let Some(n) = nodes.by_id.get_mut(&id) {
                n.free = free;
                n.dataless = st.st_flags & SF_DATALESS != 0;
            }
            let current = self.changes.load(Ordering::SeqCst) == since && !self.feed_stuck();
            (
                id,
                current && nodes.watched(parent.0),
                current && nodes.watched(id),
                free,
            )
        };
        let attr = Self::marked(self.attr(st, nodeid, mode, rdev), node_watched, st);
        Ok(Entry {
            nodeid,
            generation: 0,
            attr: self.openable(attr, free),
            entry_valid: match (exact, name_watched) {
                (false, _) => Duration::ZERO,
                (true, true) => LONG,
                (true, false) => VALID,
            },
            attr_valid: Self::attr_valid(node_watched, st),
        })
    }

    fn attr_valid(watched: bool, st: &libc::stat) -> Duration {
        let one_name = st.st_mode as u32 & S_IFMT == S_IFDIR || st.st_nlink == 1;
        if watched && one_name {
            LONG
        } else {
            VALID
        }
    }

    fn marked(mut attr: Attr, watched: bool, st: &libc::stat) -> Attr {
        if Self::attr_valid(watched, st) != LONG {
            attr.flags |= crate::abi::ATTR_NO_KEEP_CACHE;
        }
        attr
    }

    fn feed_stuck(&self) -> bool {
        if self.overdue.load(Ordering::Relaxed) {
            return true;
        }
        let began = self.batch_began.load(Ordering::Relaxed);
        began != 0 && since_start().saturating_sub(began) > STUCK.as_nanos() as u64
    }

    fn open_flags(&self, node: u64, st: &libc::stat) -> u32 {
        let watched = !self.feed_stuck() && self.nodes.lock().unwrap().watched(node);
        if Self::attr_valid(watched, st) != LONG {
            0
        } else if st.st_mode as u32 & S_IFMT == S_IFDIR {
            FOPEN_KEEP_CACHE | FOPEN_CACHE_DIR
        } else {
            FOPEN_KEEP_CACHE
        }
    }

    fn handle(&self, node: u64, fh: u64) -> Result<Arc<Handle>> {
        match self.handles.lock().unwrap().map.get(&fh) {
            Some(h) if h.node == node => Ok(h.clone()),
            _ => Err(Errno::EBADF),
        }
    }

    fn add_handle(
        &self,
        node: u64,
        fd: OwnedFd,
        dir: bool,
        write: Option<bool>,
        fresh: bool,
    ) -> Result<u64> {
        let dirty = {
            let mut nodes = self.nodes.lock().unwrap();
            let n = nodes.by_id.get_mut(&node).ok_or(Errno::ESTALE)?;
            n.open += 1;
            write.map(|_| n.dirty.get_or_insert_with(Default::default).clone())
        };
        let mut handles = self.handles.lock().unwrap();
        if handles.map.len() + self.orphans.load(Ordering::Relaxed) >= self.max_handles {
            drop(handles);
            self.closed(node);
            return Err(Errno::ENFILE);
        }
        let fh = handles.next;
        handles.next += 1;
        handles.map.insert(
            fh,
            Arc::new(Handle {
                node,
                fd,
                dir: dir.then(|| Mutex::new(None)),
                dirty,
                append: write == Some(true),
                caps_seen: AtomicU64::new(if fresh {
                    self.caps_gen.load(Ordering::Relaxed) + 1
                } else {
                    0
                }),
            }),
        );
        Ok(fh)
    }

    fn closed(&self, node: u64) {
        if let Some(n) = self.nodes.lock().unwrap().by_id.get_mut(&node) {
            n.open = n.open.saturating_sub(1);
        }
    }

    fn remove_handle(&self, fh: u64) {
        let gone = self.handles.lock().unwrap().map.remove(&fh);
        if let Some(h) = gone {
            let written = h
                .dirty
                .as_ref()
                .is_some_and(|d| d.swap(false, Ordering::Relaxed));
            if written {
                if let Ok(st) = fstat(h.fd.as_raw_fd()) {
                    self.own(&st);
                }
            }
            self.closed(h.node);
            if written {
                if let Ok(h) = Arc::try_unwrap(h) {
                    self.closer.close(h.fd);
                }
            }
        }
    }

    fn orphan(&self, node: u64) -> Option<Arc<OwnedFd>> {
        self.nodes.lock().unwrap().by_id.get(&node)?.orphan.clone()
    }

    pub fn with_rosetta(mut self) -> std::io::Result<Self> {
        if !self.read_only || self.root_path != crate::rosetta::DIR.as_bytes() {
            return Err(std::io::Error::other(
                "Rosetta requires its own read-only share",
            ));
        }
        use std::os::unix::fs::MetadataExt;
        let meta = std::fs::metadata(std::path::Path::new(crate::rosetta::DIR).join("rosetta"))?;
        self.rosetta = Some((meta.dev() as i32, meta.ino(), crate::rosetta::key()?));
        self.nodes.get_mut().unwrap().fixed = true;
        Ok(self)
    }

    pub fn on_denied(&self, tell: OnDenied) {
        let _ = self.on_denied.set(tell);
    }

    fn report_denied(&self, path: &[u8], is_dir: bool) {
        let Some(tell) = self.on_denied.get() else {
            return;
        };
        let full = if path.starts_with(b"/.vol/") {
            match full_path(path) {
                Some(p) => p,
                None => return,
            }
        } else {
            path.to_vec()
        };
        let folder = protected_folder(&full, &self.home, is_dir);
        {
            let mut told = self.told_denied.lock().unwrap();
            if told.len() >= MAX_DENIED || told.contains(&folder) {
                return;
            }
            told.push(folder.clone());
        }
        tell(&folder);
    }

    fn open_node(&self, node: u64, want: u32, flags: i32) -> Result<OwnedFd> {
        let (path, kind, dev) = self.node_path(node)?;
        if kind != want {
            return Err(match kind {
                S_IFDIR => Errno::EISDIR,
                S_IFLNK => Errno::ELOOP,
                _ if want == S_IFDIR => Errno::ENOTDIR,
                _ => Errno::EOPNOTSUPP,
            });
        }
        let fd = open(&path, flags | libc::O_NONBLOCK).map_err(|e| match e {
            Errno::ENOENT if path.starts_with(b"/.vol/") => Errno::ESTALE,
            Errno::EPERM if flags & libc::O_ACCMODE != libc::O_WRONLY => {
                self.report_denied(&path, want == S_IFDIR);
                Errno::EPERM
            }
            e => e,
        })?;
        let st = fstat(fd.as_raw_fd())?;
        if st.st_mode as u32 & S_IFMT != want || st.st_dev != dev || self.is_denied(&st) {
            return Err(Errno::ESTALE);
        }
        Ok(fd)
    }

    fn list(fd: RawFd, room: usize) -> Result<Vec<DirEnt>> {
        let dup = unsafe { libc::fcntl(fd, libc::F_DUPFD_CLOEXEC, 0) };
        if dup < 0 {
            return Err(Errno::last());
        }
        let dir = unsafe { libc::fdopendir(dup) };
        if dir.is_null() {
            let e = Errno::last();
            unsafe { libc::close(dup) };
            return Err(e);
        }
        unsafe { libc::rewinddir(dir) };
        let mut entries = Vec::new();
        loop {
            let ent = unsafe { libc::readdir(dir) };
            if ent.is_null() {
                break;
            }
            let (ino, kind, len) =
                unsafe { ((*ent).d_ino, (*ent).d_type, (*ent).d_namlen as usize) };
            let mut name = vec![0u8; len.min(1023)];
            unsafe {
                std::ptr::copy_nonoverlapping(
                    (*ent).d_name.as_ptr().cast::<u8>(),
                    name.as_mut_ptr(),
                    name.len(),
                )
            };
            if name.len() > NAME_MAX {
                continue;
            }
            if entries.len() >= room {
                unsafe { libc::closedir(dir) };
                return Err(Errno::ENOMEM);
            }
            entries.push(DirEnt {
                name: name.into(),
                ino,
                kind,
            });
        }
        unsafe { libc::closedir(dir) };
        Ok(entries)
    }
}

pub(crate) const STUCK: Duration = Duration::from_secs(1);

fn since_start() -> u64 {
    static START: std::sync::OnceLock<std::time::Instant> = std::sync::OnceLock::new();
    START
        .get_or_init(std::time::Instant::now)
        .elapsed()
        .as_nanos() as u64
        + 1
}

fn check_name(name: &[u8]) -> Result<()> {
    if name.is_empty() || name == b"." || name == b".." || name.contains(&b'/') || name.contains(&0)
    {
        return Err(Errno::EINVAL);
    }
    if name.len() > NAME_MAX {
        return Err(Errno::ENAMETOOLONG);
    }
    Ok(())
}

impl Filesystem for Passthrough {
    fn lookup(&self, _ctx: &Ctx, parent: u64, name: &[u8]) -> Result<Entry> {
        check_name(name)?;
        let _tree = self.tree_read(parent);
        let (mut path, kind, dev) = self.node_path(parent)?;
        if kind != S_IFDIR {
            return Err(Errno::ENOTDIR);
        }
        if self.hidden(dev) || self.apple_double(dev, name) {
            return Err(Errno::ENOENT);
        }
        path.push(b'/');
        path.extend_from_slice(name);
        let since = self.changes.load(Ordering::SeqCst);
        let kept = self.dir_fds.lock().unwrap().get(parent);
        let looked = match (kept, cstr(name)) {
            (Some(dir), Ok(c)) => {
                let mut st = unsafe { std::mem::zeroed::<libc::stat>() };
                let flags = libc::AT_SYMLINK_NOFOLLOW;
                match unsafe { libc::fstatat(dir.as_raw_fd(), c.as_ptr(), &mut st, flags) } {
                    0 => Ok(st),
                    _ => Err(Errno::last()),
                }
            }
            _ => lstat(&path),
        };
        #[cfg(test)]
        {
            let hook = self.after_stat.lock().unwrap().take();
            if let Some(hook) = hook {
                hook();
            }
        }
        let st = match looked {
            Ok(st) => st,
            Err(e) => {
                if e == Errno::ENOENT {
                    let mut nodes = self.nodes.lock().unwrap();
                    nodes.missed(parent, name);
                    if self.changes.load(Ordering::SeqCst) == since
                        && !self.feed_stuck()
                        && nodes.watched(parent)
                        && nodes.absent(parent, name)
                    {
                        return Ok(Entry {
                            nodeid: 0,
                            generation: 0,
                            attr: Attr::default(),
                            entry_valid: LONG,
                            attr_valid: Duration::ZERO,
                        });
                    }
                }
                return Err(e);
            }
        };
        self.enter((parent, dev), name, &st, &|| path.clone(), false, since)
    }

    fn forget(&self, node: u64, nlookup: u64) {
        if node != ROOT_ID {
            let (free, dir, gone) = {
                let mut nodes = self.nodes.lock().unwrap();
                let (free, dir) = nodes
                    .by_id
                    .get(&node)
                    .map_or((false, false), |n| (n.free, n.kind == S_IFDIR));
                nodes.forget(node, nlookup);
                (free, dir, !nodes.by_id.contains_key(&node))
            };
            if gone && free {
                self.free_fds.lock().unwrap().remove(node);
            }
            if gone && dir {
                self.dir_fds.lock().unwrap().remove(node);
            }
        }
    }

    fn ioctl(&self, node: u64, fh: u64, cmd: u32, out_size: u32) -> Result<(i32, Vec<u8>)> {
        let Some((dev, ino, key)) = &self.rosetta else {
            return Err(Errno::ENOTTY);
        };
        let st = if fh == 0 {
            fstat(self.free_fd(node)?.as_raw_fd())?
        } else {
            fstat(self.handle(node, fh)?.fd.as_raw_fd())?
        };
        if st.st_dev != *dev || st.st_ino != *ino || (cmd >> 8) & 0xff != 0x61 {
            return Err(Errno::ENOTTY);
        }
        let data = match cmd & 0xff {
            0x22 | 0x25 => key.clone(),
            0x23 => crate::rosetta::caching(),
            0x24 => Vec::new(),
            _ => return Err(Errno::ENOTTY),
        };
        if data.len() != out_size as usize {
            return Err(Errno::EINVAL);
        }
        Ok((1, data))
    }

    fn getattr(&self, _ctx: &Ctx, node: u64, fh: Option<u64>) -> Result<(Attr, Duration)> {
        if let Some(fh) = fh {
            let st = fstat(self.handle(node, fh)?.fd.as_raw_fd())?;
            let free = self
                .nodes
                .lock()
                .unwrap()
                .by_id
                .get(&node)
                .is_some_and(|n| n.free);
            let attr = Self::marked(self.attr(&st, node, st.st_mode as u32, 0), false, &st);
            let mut attr = self.openable(attr, free);
            let gone =
                self.nodes.lock().unwrap().by_id.get(&node).map(|n| &n.key) == Some(&Key::Gone);
            if gone {
                attr.nlink = 0;
            }
            return Ok((attr, VALID));
        }
        if let Some(fd) = self.orphan(node) {
            let st = fstat(fd.as_raw_fd())?;
            let mut attr = Self::marked(self.attr(&st, node, st.st_mode as u32, 0), false, &st);
            attr.nlink = 0;
            return Ok((attr, VALID));
        }
        let _tree = self.tree_read(node);
        let (path, kind, _) = self.node_path(node)?;
        let looked = lstat(&path);
        if looked.is_err() && kind == S_IFREG {
            let kept = self.free_fds.lock().unwrap().get(node);
            if let Some(fd) = kept {
                let st = fstat(fd.as_raw_fd())?;
                let attr = Self::marked(self.attr(&st, node, st.st_mode as u32, 0), false, &st);
                return Ok((attr, VALID));
            }
        }
        let st = looked.map_err(|e| match e {
            Errno::ENOENT if path.starts_with(b"/.vol/") && kind == S_IFDIR => Errno::ENOENT,
            Errno::ENOENT if path.starts_with(b"/.vol/") => Errno::ESTALE,
            e => e,
        })?;
        let (mode, rdev) = self.present(&st, &|| path.clone());
        if mode & S_IFMT != kind {
            return Err(Errno::ESTALE);
        }
        let (watched, free) = {
            let mut nodes = self.nodes.lock().unwrap();
            (
                nodes.watched(node) && !self.feed_stuck(),
                nodes.by_id.get(&node).is_some_and(|n| n.free),
            )
        };
        let attr = Self::marked(self.attr(&st, node, mode, rdev), watched, &st);
        Ok((self.openable(attr, free), Self::attr_valid(watched, &st)))
    }

    fn readlink(&self, _ctx: &Ctx, node: u64) -> Result<Vec<u8>> {
        let _tree = self.tree_read(node);
        let (path, kind, _) = self.node_path(node)?;
        if kind != S_IFLNK {
            return Err(Errno::EINVAL);
        }
        let fd = open(&path, libc::O_EVTONLY | libc::O_SYMLINK).map_err(|e| match e {
            Errno::ENOENT if path.starts_with(b"/.vol/") => Errno::ESTALE,
            e => e,
        })?;
        let st = fstat(fd.as_raw_fd())?;
        if st.st_mode as u32 & S_IFMT != S_IFLNK || self.is_denied(&st) {
            return Err(Errno::ESTALE);
        }
        let mut buf = vec![0u8; 4096];
        let n = unsafe { freadlink(fd.as_raw_fd(), buf.as_mut_ptr().cast(), buf.len()) };
        if n < 0 {
            return Err(Errno::last());
        }
        buf.truncate(n as usize);
        Ok(buf)
    }

    fn open(&self, _ctx: &Ctx, node: u64, flags: u32, kill: bool) -> Result<(u64, u32)> {
        let _tree = self.tree_read(node);
        self.w_open(node, flags, kill)
    }

    fn read(&self, _ctx: &Ctx, node: u64, fh: u64, offset: u64, data: IoVecs<'_>) -> Result<usize> {
        let (handle, free);
        let fd = if fh == 0 {
            free = self.free_fd(node)?;
            free.as_raw_fd()
        } else {
            handle = self.handle(node, fh)?;
            if handle.dir.is_some() {
                return Err(Errno::EISDIR);
            }
            handle.fd.as_raw_fd()
        };
        let mut offset = i64::try_from(offset).map_err(|_| Errno::EINVAL)?;
        let mut total = 0usize;
        let mut rest: &[libc::iovec] = data.as_slice();
        while !rest.is_empty() {
            let (batch, later) = rest.split_at(rest.len().min(IOV_BATCH));
            rest = later;
            let want: usize = batch.iter().map(|b| b.iov_len).sum();
            if want == 0 {
                continue;
            }
            let n = unsafe { libc::preadv(fd, batch.as_ptr(), batch.len() as i32, offset) };
            if n < 0 {
                if std::io::Error::last_os_error().kind() == std::io::ErrorKind::Interrupted {
                    return self.read_rest(fd, offset, batch, rest, total);
                }
                return Err(Errno::last());
            }
            let n = n as usize;
            total += n;
            offset = offset.checked_add(n as i64).ok_or(Errno::EINVAL)?;
            if n == 0 {
                break;
            }
            if n < want {
                return self.read_rest(fd, offset, &skip_iov(batch, n), rest, total);
            }
        }
        Ok(total)
    }

    fn release(&self, _node: u64, fh: u64) {
        self.remove_handle(fh);
    }

    fn opendir(&self, _ctx: &Ctx, node: u64, _flags: u32) -> Result<(u64, u32)> {
        let _tree = self.tree_read(node);
        let fd = self.open_node(node, S_IFDIR, libc::O_RDONLY | libc::O_DIRECTORY)?;
        self.dir_opened(node);
        let flags = fstat(fd.as_raw_fd()).map_or(0, |st| self.open_flags(node, &st));
        if self.free_opendir.load(Ordering::Relaxed) {
            let boot = {
                let dev = self.nodes.lock().unwrap().by_id.get(&node).map(|n| n.dev);
                dev.and_then(|d| self.volumes.lock().unwrap().get(&d).copied())
                    .is_some_and(|v| v.boot)
            };
            if boot {
                let mut slots = self.dir_slots.lock().unwrap();
                slots.retain(|(n, _)| *n != node);
                if slots.len() >= DIR_SLOTS {
                    slots.pop_front();
                }
                slots.push_back((node, fd));
            }
            return Ok((0, flags | crate::abi::FOPEN_FEV_FREE));
        }
        Ok((self.add_handle(node, fd, true, None, false)?, flags))
    }

    fn readdir(
        &self,
        _ctx: &Ctx,
        node: u64,
        fh: u64,
        offset: u64,
        sink: &mut DirSink,
    ) -> Result<()> {
        if fh == 0 {
            return self.readdir_node(node, offset, sink);
        }
        let handle = self.handle(node, fh)?;
        let dir = handle.dir.as_ref().ok_or(Errno::ENOTDIR)?;
        let fd = handle.fd.as_raw_fd();
        let dir_dev = self
            .nodes
            .lock()
            .unwrap()
            .by_id
            .get(&node)
            .ok_or(Errno::ESTALE)?
            .dev;
        let mut listing = dir.lock().unwrap();
        if offset == 0 || listing.is_none() {
            *listing = None;
            *listing = Some(self.take_listing(fd, false, false)?);
        }
        let listing = listing.as_ref().expect("filled above");
        let skip = usize::try_from(offset).unwrap_or(usize::MAX);
        self.emit(node, dir_dev, fd, listing, skip, 0, sink);
        Ok(())
    }

    fn releasedir(&self, _node: u64, fh: u64) {
        self.remove_handle(fh);
    }

    fn statfs(&self, _ctx: &Ctx, node: u64) -> Result<Statfs> {
        let _tree = self.tree_read(node);
        let (mut path, kind, _) = self.node_path(node)?;
        let mut s = unsafe { std::mem::zeroed::<libc::statfs>() };
        let fd = if matches!(kind, S_IFREG | S_IFDIR) {
            lstat(&path)?;
            open(&path, libc::O_EVTONLY | libc::O_NONBLOCK).ok()
        } else {
            None
        };
        let asked = match &fd {
            Some(fd) => unsafe { libc::fstatfs(fd.as_raw_fd(), &mut s) },
            None => {
                path = self.root_path.clone();
                let c = cstr(&path)?;
                unsafe { libc::statfs(c.as_ptr(), &mut s) }
            }
        };
        if asked != 0 {
            return Err(Errno::last());
        }
        Ok(Statfs {
            blocks: s.f_blocks,
            bfree: s.f_bfree,
            bavail: s.f_bavail,
            files: s.f_files,
            ffree: s.f_ffree,
            bsize: s.f_bsize,
            namelen: NAME_MAX as u32,
            frsize: s.f_bsize,
        })
    }

    fn setattr(&self, _ctx: &Ctx, node: u64, set: &SetAttr) -> Result<(Attr, Duration)> {
        let _tree = self.tree_read(node);
        self.w_setattr(node, set)
    }

    fn mknod(
        &self,
        _ctx: &Ctx,
        parent: u64,
        name: &[u8],
        mode: u32,
        rdev: u32,
        umask: u32,
    ) -> Result<Entry> {
        let _tree = self.tree_read(parent);
        self.w_mknod(parent, name, mode, rdev, umask)
    }

    fn mkdir(&self, _ctx: &Ctx, parent: u64, name: &[u8], mode: u32, umask: u32) -> Result<Entry> {
        let _tree = self.tree_read(parent);
        self.w_mkdir(parent, name, mode, umask)
    }

    fn symlink(&self, _ctx: &Ctx, parent: u64, name: &[u8], target: &[u8]) -> Result<Entry> {
        let _tree = self.tree_read(parent);
        self.w_symlink(parent, name, target)
    }

    fn link(&self, _ctx: &Ctx, node: u64, newparent: u64, newname: &[u8]) -> Result<Entry> {
        self.w_link(node, newparent, newname)
    }

    fn unlink(&self, _ctx: &Ctx, parent: u64, name: &[u8]) -> Result<()> {
        self.w_remove(parent, name, false)
    }

    fn rmdir(&self, _ctx: &Ctx, parent: u64, name: &[u8]) -> Result<()> {
        self.w_remove(parent, name, true)
    }

    fn rename(
        &self,
        _ctx: &Ctx,
        parent: u64,
        name: &[u8],
        newparent: u64,
        newname: &[u8],
        flags: u32,
    ) -> Result<()> {
        self.w_rename(parent, name, newparent, newname, flags)
    }

    fn create(
        &self,
        _ctx: &Ctx,
        parent: u64,
        name: &[u8],
        mode: u32,
        umask: u32,
        flags: u32,
    ) -> Result<(Entry, u64, u32)> {
        let _tree = self.tree_read(parent);
        self.w_create(parent, name, mode, umask, flags)
    }

    fn write(
        &self,
        _ctx: &Ctx,
        node: u64,
        fh: u64,
        offset: u64,
        data: IoVecs<'_>,
        kill: bool,
        cache: bool,
    ) -> Result<usize> {
        self.w_write(node, fh, offset, data, kill, cache)
    }

    fn fsync(&self, _ctx: &Ctx, node: u64, fh: u64, _datasync: bool) -> Result<()> {
        if fh == 0 {
            let (_, kind, _) = self.node_path(node)?;
            if kind == S_IFREG {
                return write::sync_fd(self.free_fd(node)?.as_raw_fd(), self.sync);
            }
            let _tree = self.tree_read(node);
            let fd = self.open_node(node, S_IFDIR, libc::O_RDONLY | libc::O_DIRECTORY)?;
            return write::sync_fd(fd.as_raw_fd(), self.sync);
        }
        write::sync_fd(self.handle(node, fh)?.fd.as_raw_fd(), self.sync)
    }

    fn quick(&self, node: u64, need: Need) -> bool {
        let dev = {
            let nodes = self.nodes.lock().unwrap();
            let Some(n) = nodes.by_id.get(&node) else {
                return false;
            };
            if !matches!(n.key, Key::Vol { .. }) {
                return false;
            }
            let beside_opened = || {
                n.at.as_ref()
                    .and_then(|(p, _)| nodes.by_id.get(p))
                    .is_some_and(|p| p.dir_ok)
            };
            let let_in = match need {
                Need::Meta => true,
                Need::Lookup => !n.dataless && !n.mounts,
                Need::Handle(fh) if fh != 0 => true,
                _ if n.dataless => false,
                Need::Names => n.dir_ok,
                Need::File if n.kind == S_IFDIR => n.dir_ok,
                Need::File => n.free || beside_opened(),
                Need::Handle(_) => n.free,
            };
            if !let_in {
                return false;
            }
            n.dev
        };
        self.vol_of(dev).is_some_and(|v| v.boot && v.volfs)
    }

    fn mounted(&self, flags: u64) {
        let cache = flags & crate::abi::init::FEV_CACHE != 0;
        self.free_opendir.store(cache, Ordering::Relaxed);
        let fewer = cache && flags & crate::abi::init::FEV_REQUESTS != 0;
        self.fewer_requests.store(fewer, Ordering::Relaxed);
        self.complete_dirs.store(
            fewer && flags & crate::abi::init::FEV_COMPLETE != 0,
            Ordering::Relaxed,
        );
    }

    fn syncfs(&self, _ctx: &Ctx, _node: u64) -> Result<()> {
        let fd = open(&self.root_path, libc::O_RDONLY | libc::O_DIRECTORY)?;
        write::sync_fd(fd.as_raw_fd(), self.sync)
    }

    fn fallocate(
        &self,
        _ctx: &Ctx,
        node: u64,
        fh: u64,
        mode: u32,
        offset: u64,
        len: u64,
    ) -> Result<()> {
        self.w_fallocate(node, fh, mode, offset, len)
    }

    fn lseek(&self, _ctx: &Ctx, node: u64, fh: u64, offset: u64, whence: u32) -> Result<u64> {
        self.w_lseek(node, fh, offset, whence)
    }

    fn setxattr(&self, _ctx: &Ctx, node: u64, name: &[u8], value: &[u8], flags: u32) -> Result<()> {
        self.x_set(node, name, value, flags)
    }

    fn getxattr(&self, _ctx: &Ctx, node: u64, name: &[u8], size: u32) -> Result<Xattr> {
        self.x_get(node, name, size)
    }

    fn listxattr(&self, _ctx: &Ctx, node: u64, size: u32) -> Result<Xattr> {
        self.x_list(node, size)
    }

    fn removexattr(&self, _ctx: &Ctx, node: u64, name: &[u8]) -> Result<()> {
        self.x_remove(node, name)
    }

    fn destroy(&self) {
        *self.listings.lock().unwrap() = Listings::default();
        self.free_fds.lock().unwrap().clear();
        self.dir_fds.lock().unwrap().clear();
        self.dir_slots.lock().unwrap().clear();
        self.handles.lock().unwrap().map.clear();
        self.orphans.store(0, Ordering::Relaxed);
        let mut nodes = self.nodes.lock().unwrap();
        let root = nodes.by_id.remove(&ROOT_ID);
        nodes.by_id.clear();
        nodes.by_key.clear();
        nodes.told.clear();
        nodes.displaced.clear();
        nodes.names = 0;
        nodes.absent.clear();
        nodes.absent_len = 0;
        nodes.watch_epoch += 1;
        if let Some(mut root) = root {
            root.children = 0;
            root.open = 0;
            nodes.by_key.insert(root.key.clone(), ROOT_ID);
            nodes.by_id.insert(ROOT_ID, root);
        }
    }
}

fn skip_iov(iov: &[libc::iovec], mut n: usize) -> Vec<libc::iovec> {
    let mut out = Vec::new();
    for b in iov {
        if n >= b.iov_len {
            n -= b.iov_len;
            continue;
        }
        out.push(libc::iovec {
            iov_base: unsafe { b.iov_base.cast::<u8>().add(n) }.cast(),
            iov_len: b.iov_len - n,
        });
        n = 0;
    }
    out
}

impl Passthrough {
    fn read_rest(
        &self,
        fd: RawFd,
        mut offset: i64,
        first: &[libc::iovec],
        rest: &[libc::iovec],
        mut total: usize,
    ) -> Result<usize> {
        for piece in first.iter().chain(rest) {
            let mut done = 0;
            while done < piece.iov_len {
                let n = unsafe {
                    libc::pread(
                        fd,
                        piece.iov_base.cast::<u8>().add(done).cast(),
                        piece.iov_len - done,
                        offset,
                    )
                };
                if n < 0 {
                    if std::io::Error::last_os_error().kind() == std::io::ErrorKind::Interrupted {
                        continue;
                    }
                    return Err(Errno::last());
                }
                if n == 0 {
                    return Ok(total);
                }
                done += n as usize;
                total += n as usize;
                offset = offset.checked_add(n as i64).ok_or(Errno::EINVAL)?;
            }
        }
        Ok(total)
    }

    fn take_listing(&self, fd: RawFd, evict: bool, plus: bool) -> Result<Listing> {
        let room = if evict {
            self.max_listed.min((1 << LISTING_PLACE_BITS) - 2)
        } else {
            self.max_listed
                .saturating_sub(self.listed.load(Ordering::Relaxed))
        };
        let since = self.changes.load(Ordering::SeqCst);
        let bulk = match plus.then(|| fstat(fd)).transpose()? {
            Some(dir)
                if self
                    .vol_of(dir.st_dev)
                    .is_some_and(|v| v.local && !v.nosuid) =>
            {
                Self::list_bulk(fd, room, &dir, since)
            }
            _ => None,
        };
        let (entries, bulk) = match bulk {
            Some(listed) => {
                let (entries, bulk) = listed?;
                (entries, Some(bulk))
            }
            None => (Self::list(fd, room)?, None),
        };
        let mut new = Listing {
            entries,
            bulk,
            listed: self.listed.clone(),
        };
        let mut held = self.listed.fetch_add(new.weight(), Ordering::Relaxed);
        if new.bulk.is_some() && held + new.weight() > self.max_listed {
            self.listed.fetch_sub(new.weight(), Ordering::Relaxed);
            new.bulk = None;
            held = self.listed.fetch_add(new.weight(), Ordering::Relaxed);
        }
        if !evict && held + new.weight() > self.max_listed {
            return Err(Errno::ENOMEM);
        }
        Ok(new)
    }

    fn readdir_node(&self, node: u64, offset: u64, sink: &mut DirSink) -> Result<()> {
        let _tree = self.tree_read(node);
        let dir_dev = self
            .nodes
            .lock()
            .unwrap()
            .by_id
            .get(&node)
            .ok_or(Errno::ESTALE)?
            .dev;
        let place = (1u64 << LISTING_PLACE_BITS) - 1;
        let mut number = u32::try_from(offset >> LISTING_PLACE_BITS).unwrap_or(u32::MAX);
        let skip = (offset & place) as usize;
        let kept = match offset {
            0 => None,
            _ => self
                .listings
                .lock()
                .unwrap()
                .map
                .get(&(node, number))
                .cloned(),
        };
        if kept.as_ref().is_some_and(|l| skip >= l.entries.len()) {
            self.listings.lock().unwrap().map.remove(&(node, number));
            return Ok(());
        }
        let waiting = {
            let mut slots = self.dir_slots.lock().unwrap();
            let at = slots.iter().position(|(n, _)| *n == node);
            at.and_then(|i| slots.remove(i)).map(|(_, fd)| fd)
        };
        let fd = match waiting {
            Some(fd) => fd,
            None => self
                .open_node(node, S_IFDIR, libc::O_RDONLY | libc::O_DIRECTORY)
                .map_err(|e| match e {
                    Errno::ESTALE => Errno::ENOENT,
                    e => e,
                })?,
        };
        self.dir_opened(node);
        let listing = match kept {
            Some(listing) => listing,
            None => {
                let listing = Arc::new(self.take_listing(fd.as_raw_fd(), true, sink.plus())?);
                let mut all = self.listings.lock().unwrap();
                number = (0..LISTINGS_A_DIRECTORY)
                    .find(|n| !all.map.contains_key(&(node, *n)))
                    .or_else(|| {
                        all.order
                            .iter()
                            .find(|k| k.0 == node && all.map.contains_key(k))
                            .map(|k| k.1)
                    })
                    .unwrap_or(0);
                all.order.retain(|k| *k != (node, number));
                all.map.insert((node, number), listing.clone());
                all.order.push_back((node, number));
                while self.listed.load(Ordering::Relaxed) > self.max_listed && all.order.len() > 1 {
                    if let Some(old) = all.order.pop_front() {
                        all.map.remove(&old);
                    }
                }
                if all.order.len() > 2 * all.map.len() + 64 {
                    let map = &all.map;
                    let live: std::collections::VecDeque<_> = all
                        .order
                        .iter()
                        .filter(|k| map.contains_key(k))
                        .copied()
                        .collect();
                    all.order = live;
                }
                listing
            }
        };
        if skip >= listing.entries.len() {
            self.listings.lock().unwrap().map.remove(&(node, number));
            return Ok(());
        }
        let base = listing_offset(number, 0);
        let ended = self.emit(node, dir_dev, fd.as_raw_fd(), &listing, skip, base, sink);
        if ended
            && sink.plus()
            && sink.fits(NAME_MAX)
            && self.fewer_requests.load(Ordering::Relaxed)
        {
            self.listings.lock().unwrap().map.remove(&(node, number));
        }
        Ok(())
    }

    #[allow(clippy::too_many_arguments)]
    fn emit(
        &self,
        node: u64,
        dir_dev: i32,
        fd: RawFd,
        listing: &Listing,
        skip: usize,
        base: u64,
        sink: &mut DirSink,
    ) -> bool {
        let hidden = self.hidden(dir_dev);
        let doubles = self.apple_double(dir_dev, b"._");
        let bulk = listing.bulk.as_ref().and_then(|b| {
            let age = b.taken.elapsed();
            (age < VALID && self.untouched(node, b.since)).then_some((b, age))
        });
        for (i, e) in listing.entries.iter().enumerate().skip(skip) {
            if !sink.fits(e.name.len()) {
                return false;
            }
            let off = base + i as u64 + 1;
            let dots = &*e.name == b"." || &*e.name == b"..";
            if self.denied.contains(&(dir_dev, e.ino))
                || (hidden && !dots)
                || (doubles && e.name.starts_with(b"._"))
                || (!dots && self.place_denied(node, &e.name))
            {
                continue;
            }
            if !sink.plus() || dots {
                let ino = if dir_dev == self.root_dev && self.root_volfs {
                    e.ino
                } else {
                    UNKNOWN_INO
                };
                sink.add(&e.name, ino, off, e.kind as u32, None);
                continue;
            }
            let listed = bulk.and_then(|(b, age)| Some((b.st.get(i)?.as_ref()?, b.since, age)));
            let mut looked = unsafe { std::mem::zeroed::<libc::stat>() };
            let (st, since, age) = match listed {
                Some((st, since, age)) => (st, since, age),
                None => {
                    let Ok(c) = cstr(&e.name) else { continue };
                    let since = self.changes.load(Ordering::SeqCst);
                    let flags = libc::AT_SYMLINK_NOFOLLOW;
                    if unsafe { libc::fstatat(fd, c.as_ptr(), &mut looked, flags) } != 0 {
                        continue;
                    }
                    (&looked, since, Duration::ZERO)
                }
            };
            let path = || {
                let mut p = self.node_path(node).map(|(p, _, _)| p).unwrap_or_default();
                p.push(b'/');
                p.extend_from_slice(&e.name);
                p
            };
            match self.enter((node, dir_dev), &e.name, st, &path, true, since) {
                Ok(mut entry) => {
                    for valid in [&mut entry.entry_valid, &mut entry.attr_valid] {
                        if *valid == VALID {
                            *valid = VALID.saturating_sub(age);
                        }
                    }
                    sink.add(
                        &e.name,
                        entry.attr.ino,
                        off,
                        entry.attr.mode >> 12,
                        Some(&entry),
                    );
                }
                Err(Errno::ENOMEM) => {
                    sink.add(&e.name, UNKNOWN_INO, off, st.st_mode as u32 >> 12, None);
                }
                Err(_) => {}
            }
        }
        true
    }

    pub fn open_handles(&self) -> usize {
        self.handles.lock().unwrap().map.len()
    }

    pub fn orphans(&self) -> usize {
        self.orphans.load(Ordering::Relaxed)
    }

    pub fn known_nodes(&self) -> usize {
        self.nodes.lock().unwrap().by_id.len()
    }
}

mod bulk;
mod free;
mod notify;
mod write;
pub use notify::{fse, Change, WatchStatus};

#[cfg(test)]
mod tests;
