use std::collections::HashMap;
use std::sync::atomic::Ordering;
use std::time::Duration;

use super::*;
use crate::fsevents::{flag, mask, op, Event, Status};

pub type WatchStatus = Status;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Change {
    pub path: Vec<u8>,
    pub flags: u32,
    pub ino: u64,
}

pub mod fse {
    pub const MUST_SCAN_SUBDIRS: u32 = 0x1;
    pub const USER_DROPPED: u32 = 0x2;
    pub const KERNEL_DROPPED: u32 = 0x4;
    pub const IDS_WRAPPED: u32 = 0x8;
    pub const HISTORY_DONE: u32 = 0x10;
    pub const ROOT_CHANGED: u32 = 0x20;
    pub const MOUNT: u32 = 0x40;
    pub const UNMOUNT: u32 = 0x80;
    pub const CREATED: u32 = 0x100;
    pub const REMOVED: u32 = 0x200;
    pub const INODE_META: u32 = 0x400;
    pub const RENAMED: u32 = 0x800;
    pub const MODIFIED: u32 = 0x1000;
    pub const FINDER_INFO: u32 = 0x2000;
    pub const CHANGE_OWNER: u32 = 0x4000;
    pub const XATTR: u32 = 0x8000;
    pub const IS_DIR: u32 = 0x20000;
    pub const IS_HARDLINK: u32 = 0x100000;
    pub const IS_LAST_HARDLINK: u32 = 0x200000;
    pub const MARKS: u32 = HISTORY_DONE | MOUNT | UNMOUNT;
    pub const LOST: u32 =
        MUST_SCAN_SUBDIRS | USER_DROPPED | KERNEL_DROPPED | IDS_WRAPPED | ROOT_CHANGED;
    pub const META: u32 = INODE_META | FINDER_INFO | CHANGE_OWNER | XATTR;
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(super) struct State {
    size: i64,
    mtime: (i64, i64),
    ctime: (i64, i64),
    mode: u32,
}

impl State {
    fn of(st: &libc::stat) -> Self {
        Self {
            size: st.st_size,
            mtime: (st.st_mtime, st.st_mtime_nsec),
            ctime: (st.st_ctime, st.st_ctime_nsec),
            mode: st.st_mode as u32,
        }
    }
}

const FRESH: i64 = 2;

const MAX_APPEARED: usize = 256;

const MAX_SEEN: usize = 1 << 16;

pub(super) const RESCAN_MAX: usize = 100_000;

pub(super) const RESCAN_MARGIN: Duration = Duration::from_secs(1);

const MAX_TOUCHED: usize = 1 << 16;

fn unix_nanos(t: std::time::SystemTime) -> u64 {
    t.duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_nanos() as u64)
}

fn within(path: &[u8], root: &[u8]) -> bool {
    path == root
        || root == b"/" && path.starts_with(b"/")
        || path.starts_with(root) && path.get(root.len()) == Some(&b'/')
}

fn split(path: &[u8]) -> Option<(&[u8], &[u8])> {
    let at = path.iter().rposition(|b| *b == b'/')?;
    let (dir, name) = (&path[..at], &path[at + 1..]);
    if name.is_empty() || name.len() > NAME_MAX || name == b"." || name == b".." {
        return None;
    }
    Some((if dir.is_empty() { b"/" } else { dir }, name))
}

impl Passthrough {
    pub fn set_feeding(&self, on: bool) {
        self.feeding.store(on, Ordering::Relaxed);
        if !on {
            self.seen.lock().unwrap().clear();
        }
    }

    pub fn set_overdue(&self, on: bool) {
        self.overdue.store(on, Ordering::SeqCst);
    }

    pub(super) fn own(&self, st: &libc::stat) {
        if self.feeding.load(Ordering::Relaxed) {
            self.remember(st);
        }
    }

    fn remember(&self, st: &libc::stat) {
        let mut seen = self.seen.lock().unwrap();
        if seen.len() >= MAX_SEEN {
            seen.clear();
        }
        seen.insert((st.st_dev, st.st_ino), State::of(st));
    }

    pub fn set_watch(&self, roots: &[Vec<u8>]) {
        let mut ids = std::collections::HashSet::new();
        let mut paths = Vec::new();
        for path in roots {
            if !self.inside(path) {
                continue;
            }
            let Ok(st) = lstat(path) else { continue };
            if st.st_mode as u32 & S_IFMT != S_IFDIR || self.is_denied(&st) {
                continue;
            }
            let vol = self.volume(&st, path);
            if vol.volfs && vol.local && !vol.hidden {
                ids.insert((st.st_dev, st.st_ino));
                paths.push((path.clone(), (st.st_dev, st.st_ino)));
            }
        }
        *self.watch_paths.lock().unwrap() = paths;
        let mut nodes = self.nodes.lock().unwrap();
        if nodes.watch_roots != ids {
            nodes.watch_roots = ids;
            nodes.watch_epoch += 1;
            self.watch_at
                .store(unix_nanos(std::time::SystemTime::now()), Ordering::Relaxed);
        }
    }

    fn recheck_watch(&self) {
        let mut paths = self.watch_paths.lock().unwrap();
        let before = paths.len();
        paths.retain(|(path, id)| lstat(path).is_ok_and(|st| (st.st_dev, st.st_ino) == *id));
        if paths.len() == before {
            return;
        }
        let ids = paths.iter().map(|(_, id)| *id).collect();
        drop(paths);
        let mut nodes = self.nodes.lock().unwrap();
        nodes.watch_roots = ids;
        nodes.watch_epoch += 1;
    }

    pub fn watch_roots(&self) -> usize {
        self.nodes.lock().unwrap().watch_roots.len()
    }

    fn inside(&self, path: &[u8]) -> bool {
        let root = &self.root_path;
        path == &root[..]
            || root == b"/" && path.starts_with(b"/")
            || path.starts_with(root) && path.get(root.len()) == Some(&b'/')
    }

    pub fn watch_status(&self, path: &[u8]) -> (WatchStatus, Option<Vec<u8>>) {
        let Ok(c) = cstr(path) else {
            return (Status::Failed, None);
        };
        let mut buf = [0u8; libc::PATH_MAX as usize];
        if unsafe { libc::realpath(c.as_ptr(), buf.as_mut_ptr().cast()) }.is_null() {
            let e = std::io::Error::last_os_error().raw_os_error();
            if !self.inside(path) {
                return (Status::Outside, None);
            }
            return match e {
                Some(libc::ENOENT) | Some(libc::ENOTDIR) => (Status::Missing, None),
                _ => (Status::Failed, None),
            };
        }
        let real = &buf[..buf.iter().position(|b| *b == 0).unwrap_or(0)];
        if !self.inside(real) {
            return (Status::Outside, None);
        }
        let mut at = self.root_path.len().max(1);
        loop {
            let end = real[at..]
                .iter()
                .position(|b| *b == b'/')
                .map_or(real.len(), |i| at + i);
            if self.is_place(&real[..end]) {
                return (Status::Outside, None);
            }
            match lstat(&real[..end]) {
                Ok(st) if self.is_denied(&st) => return (Status::Outside, None),
                Ok(_) => {}
                Err(Errno::ENOENT) => return (Status::Missing, None),
                Err(_) => return (Status::Failed, None),
            }
            if end >= real.len() {
                break;
            }
            at = end + 1;
        }
        if on_devfs(real) {
            return (Status::Outside, None);
        }
        (Status::Ok, Some(real.to_vec()))
    }

    fn dir_node(&self, dir: &[u8]) -> Option<(u64, i32)> {
        if !self.inside(dir) {
            return None;
        }
        if dir == &self.root_path[..] {
            return Some((ROOT_ID, self.root_dev));
        }
        let st = lstat(dir).ok()?;
        if st.st_mode as u32 & S_IFMT != S_IFDIR || self.is_denied(&st) {
            return None;
        }
        let vol = self.volumes.lock().unwrap().get(&st.st_dev).copied()?;
        if vol.hidden {
            return None;
        }
        let key = if vol.volfs {
            Key::Vol {
                dev: st.st_dev,
                ino: st.st_ino,
            }
        } else {
            let (up, name) = split(dir)?;
            Key::Tree {
                parent: self.dir_node(up)?.0,
                name: name.into(),
            }
        };
        let id = *self.nodes.lock().unwrap().by_key.get(&key)?;
        Some((id, st.st_dev))
    }

    fn object_node(&self, st: &libc::stat, parent: u64, name: &[u8]) -> Option<u64> {
        let vol = self.volumes.lock().unwrap().get(&st.st_dev).copied()?;
        let key = if vol.volfs {
            Key::Vol {
                dev: st.st_dev,
                ino: st.st_ino,
            }
        } else {
            Key::Tree {
                parent,
                name: name.into(),
            }
        };
        self.nodes.lock().unwrap().by_key.get(&key).copied()
    }

    fn node_by_ino(&self, ino: u64) -> Option<u64> {
        if ino == 0 {
            return None;
        }
        let devs: Vec<i32> = self
            .volumes
            .lock()
            .unwrap()
            .iter()
            .filter(|(_, v)| v.volfs)
            .map(|(d, _)| *d)
            .collect();
        let nodes = self.nodes.lock().unwrap();
        devs.iter()
            .find_map(|dev| nodes.by_key.get(&Key::Vol { dev: *dev, ino }).copied())
    }

    fn path_now(&self, c: &Change, dir: &[u8]) -> Option<(Vec<u8>, libc::stat)> {
        if c.ino == 0 || c.flags & fse::IS_HARDLINK != 0 {
            return None;
        }
        let mut up = dir;
        let dev = loop {
            match lstat(up) {
                Ok(st) => break st.st_dev,
                Err(_) => up = split(up)?.0,
            }
            if !self.inside(up) {
                return None;
            }
        };
        if !self.volumes.lock().unwrap().get(&dev)?.volfs {
            return None;
        }
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
        getattrlist(&volfs_path(dev, c.ino), &list, &mut buf).ok()?;
        let offset = i32::from_le_bytes(buf[4..8].try_into().unwrap());
        let len = u32::from_le_bytes(buf[8..12].try_into().unwrap()) as usize;
        let start = 4usize.checked_add(usize::try_from(offset).ok()?)?;
        let path = buf.get(start..start.checked_add(len)?)?;
        let path = path.split(|b| *b == 0).next()?.to_vec();
        if path == c.path || !self.inside(&path) {
            return None;
        }
        let st = lstat(&path).ok()?;
        if st.st_ino != c.ino || st.st_dev != dev || self.is_denied(&st) {
            return None;
        }
        if st.st_mode as u32 & S_IFMT != S_IFDIR && st.st_nlink > 1 {
            return None;
        }
        Some((path, st))
    }

    fn node_fate(&self, id: u64) -> (bool, bool) {
        let nodes = self.nodes.lock().unwrap();
        let Some(n) = nodes.by_id.get(&id) else {
            return (true, false);
        };
        let dir = n.kind == S_IFDIR;
        match &n.key {
            Key::Vol { dev, ino } => {
                let path = volfs_path(*dev, *ino);
                drop(nodes);
                (lstat(&path).is_err(), dir)
            }
            _ => (true, dir),
        }
    }

    pub fn records(&self, batch: &[Change]) -> Vec<Event> {
        self.records_at(batch, std::time::SystemTime::now())
    }

    pub fn records_at(&self, batch: &[Change], at: std::time::SystemTime) -> Vec<Event> {
        self.changes.fetch_add(1, Ordering::SeqCst);
        let began = since_start();
        self.batch_began.store(began, Ordering::Relaxed);
        let mut out = self.build(batch);
        self.batch_began.store(0, Ordering::Relaxed);
        if since_start().saturating_sub(began) > STUCK.as_nanos() as u64 {
            out.push(Event {
                op: op::INVAL_ALL,
                ..Event::default()
            });
        }
        self.batch_at.store(unix_nanos(at), Ordering::Relaxed);
        out
    }

    fn build(&self, batch: &[Change]) -> Vec<Event> {
        #[cfg(test)]
        {
            let delay = self.batch_delay.lock().unwrap().take();
            if let Some(delay) = delay {
                std::thread::sleep(delay);
            }
        }
        let mut out = Vec::new();
        let mut rescan = Vec::new();
        let lost = self.pass(batch, &mut out, &mut rescan);
        if !rescan.is_empty() {
            if let Some(found) = self.rescan(&rescan) {
                self.pass(&found, &mut out, &mut Vec::new());
            }
        }
        if lost {
            out.push(Event {
                op: op::INVAL_ALL,
                ..Event::default()
            });
        }
        out
    }

    fn pass(&self, batch: &[Change], out: &mut Vec<Event>, rescan: &mut Vec<Vec<u8>>) -> bool {
        let stats: Vec<Option<libc::stat>> = batch
            .iter()
            .map(|c| {
                lstat(&c.path)
                    .ok()
                    .filter(|st| !self.is_denied(st) && !self.is_place(&c.path))
            })
            .collect();
        let mut source_of: HashMap<usize, usize> = HashMap::new();
        let mut is_source = vec![false; batch.len()];
        for (i, c) in batch.iter().enumerate() {
            let here = stats[i].as_ref().is_some_and(|st| st.st_ino == c.ino);
            if c.flags & fse::RENAMED == 0 || c.ino == 0 || !here {
                continue;
            }
            let from = batch.iter().enumerate().position(|(j, o)| {
                j != i
                    && !is_source[j]
                    && o.ino == c.ino
                    && o.flags & fse::RENAMED != 0
                    && !stats[j].as_ref().is_some_and(|st| st.st_ino == o.ino)
            });
            if let Some(j) = from {
                is_source[j] = true;
                source_of.insert(i, j);
            }
        }

        let mut lost = false;
        for (i, c) in batch.iter().enumerate() {
            if c.flags & fse::LOST != 0 {
                if !lost {
                    self.recheck_watch();
                    self.touched_all
                        .store(self.changes.load(Ordering::SeqCst), Ordering::SeqCst);
                }
                lost = true;
                if c.flags & fse::MUST_SCAN_SUBDIRS != 0
                    && !c.path.is_empty()
                    && self.inside(&c.path)
                    && !rescan.iter().any(|r| within(&c.path, r))
                {
                    rescan.retain(|r| !within(r, &c.path));
                    rescan.push(c.path.clone());
                }
                continue;
            }
            if is_source[i] || c.flags & (fse::HISTORY_DONE | fse::MOUNT | fse::UNMOUNT) != 0 {
                continue;
            }
            let Some((dir, name)) = split(&c.path) else {
                continue;
            };
            match &stats[i] {
                Some(st) => {
                    let from = source_of.get(&i).map(|j| &batch[*j]);
                    let folds = self
                        .volumes
                        .lock()
                        .unwrap()
                        .get(&st.st_dev)
                        .is_none_or(|v| !v.case_sensitive);
                    let spelled = if from.is_none() && (folds || !name.is_ascii()) {
                        name_on_disk(&c.path).filter(|d| d != name && d.len() <= NAME_MAX)
                    } else {
                        None
                    };
                    match spelled {
                        Some(now) if c.flags & fse::RENAMED != 0 => {
                            let mut to = dir.to_vec();
                            if to.last() != Some(&b'/') {
                                to.push(b'/');
                            }
                            to.extend_from_slice(&now);
                            let there = Change {
                                path: to,
                                flags: c.flags,
                                ino: c.ino,
                            };
                            self.present_change(&there, st, dir, &now, Some(c), out);
                        }
                        Some(now) => self.present_change(c, st, dir, &now, None, out),
                        None => self.present_change(c, st, dir, name, from, out),
                    }
                }
                None => {
                    if let Some((to, st)) = self.path_now(c, dir) {
                        if let Some((tdir, tname)) = split(&to) {
                            let there = Change {
                                path: to.clone(),
                                flags: c.flags,
                                ino: c.ino,
                            };
                            if tname == name && lstat(dir).is_err() {
                                self.present_change(&there, &st, tdir, tname, None, out);
                                continue;
                            }
                            if c.flags & fse::RENAMED != 0 && c.flags & fse::REMOVED == 0 {
                                self.present_change(&there, &st, tdir, tname, Some(c), out);
                                continue;
                            }
                        }
                    }
                    self.absent_change(c, dir, name, out)
                }
            }
        }
        lost
    }

    fn told_path(&self, nodes: &Nodes, id: u64) -> Option<Vec<u8>> {
        let mut names: Vec<&[u8]> = Vec::new();
        let mut cur = id;
        while cur != ROOT_ID {
            let (parent, name) = nodes.by_id.get(&cur)?.at.as_ref()?;
            names.push(name);
            cur = *parent;
            if names.len() > 1024 {
                return None;
            }
        }
        let mut path = self.root_path.clone();
        for name in names.iter().rev() {
            if path.last() != Some(&b'/') {
                path.push(b'/');
            }
            path.extend_from_slice(name);
        }
        Some(path)
    }

    fn rescan(&self, roots: &[Vec<u8>]) -> Option<Vec<Change>> {
        let since = self
            .batch_at
            .load(Ordering::Relaxed)
            .max(self.watch_at.load(Ordering::Relaxed));
        if since == 0 {
            return None;
        }
        let floor = Duration::from_nanos(since).saturating_sub(RESCAN_MARGIN);
        let floor = (floor.as_secs() as i64, floor.subsec_nanos() as i64);
        let young = |s: i64, ns: i64| (s, ns) >= floor;
        let mut most = RESCAN_MAX;
        #[cfg(test)]
        if let Some(m) = *self.rescan_max.lock().unwrap() {
            most = m;
        }
        type Dir = (u64, Vec<u8>, Vec<Box<[u8]>>, Vec<Box<[u8]>>);
        let dirs: Vec<Dir> = {
            let nodes = self.nodes.lock().unwrap();
            nodes
                .by_id
                .iter()
                .filter(|(_, n)| n.kind == S_IFDIR)
                .filter_map(|(id, _)| {
                    let path = self.told_path(&nodes, *id)?;
                    roots.iter().any(|r| within(&path, r)).then(|| {
                        let told = nodes
                            .told
                            .get(id)
                            .map(|m| m.keys().cloned().collect())
                            .unwrap_or_default();
                        let absent = nodes
                            .absent
                            .get(id)
                            .map(|f| f.keys().cloned().collect())
                            .unwrap_or_default();
                        (*id, path, told, absent)
                    })
                })
                .collect()
        };
        let mut found = Vec::new();
        for (id, path, told, absent) in dirs {
            let Ok(c) = cstr(&path) else { continue };
            let dir = unsafe { libc::opendir(c.as_ptr()) };
            if dir.is_null() {
                continue;
            }
            let mut seen: std::collections::HashSet<Box<[u8]>> = std::collections::HashSet::new();
            let mut full = path.clone();
            if full.last() != Some(&b'/') {
                full.push(b'/');
            }
            let base = full.len();
            loop {
                let ent = unsafe { libc::readdir(dir) };
                if ent.is_null() {
                    break;
                }
                let name = unsafe {
                    let e = &*ent;
                    std::slice::from_raw_parts(e.d_name.as_ptr().cast::<u8>(), e.d_namlen as usize)
                };
                if name == b"." || name == b".." || name.len() > NAME_MAX {
                    continue;
                }
                if most == 0 {
                    unsafe { libc::closedir(dir) };
                    return None;
                }
                most -= 1;
                full.truncate(base);
                full.extend_from_slice(name);
                let Ok(st) = lstat(&full) else { continue };
                seen.insert(name.into());
                let told_id = {
                    let nodes = self.nodes.lock().unwrap();
                    nodes.told.get(&id).and_then(|m| m.get(name)).copied()
                };
                let flags = match told_id {
                    Some(t) if self.object_node(&st, id, name) == Some(t) => {
                        let prior = self
                            .seen
                            .lock()
                            .unwrap()
                            .get(&(st.st_dev, st.st_ino))
                            .copied();
                        if prior.is_some_and(|p| p != State::of(&st))
                            || young(st.st_mtime, st.st_mtime_nsec)
                        {
                            fse::MODIFIED
                        } else if young(st.st_ctime, st.st_ctime_nsec) {
                            fse::INODE_META
                        } else {
                            continue;
                        }
                    }
                    Some(_) => fse::CREATED | fse::MODIFIED,
                    None => {
                        let believed_absent = absent.contains(&fold(name));
                        if young(st.st_ctime, st.st_ctime_nsec) || believed_absent {
                            fse::CREATED | fse::MODIFIED
                        } else {
                            continue;
                        }
                    }
                };
                found.push(Change {
                    path: full.clone(),
                    flags,
                    ino: st.st_ino,
                });
            }
            unsafe { libc::closedir(dir) };
            for name in told {
                if seen.contains(&name) {
                    continue;
                }
                full.truncate(base);
                full.extend_from_slice(&name);
                if let Ok(st) = lstat(&full) {
                    let told_id = {
                        let nodes = self.nodes.lock().unwrap();
                        nodes.told.get(&id).and_then(|m| m.get(&*name)).copied()
                    };
                    if told_id.is_some() && told_id == self.object_node(&st, id, &name) {
                        continue;
                    }
                }
                found.push(Change {
                    path: full.clone(),
                    flags: fse::REMOVED,
                    ino: 0,
                });
            }
        }
        Some(found)
    }

    fn touch(&self, node: u64) {
        let stamp = self.changes.load(Ordering::SeqCst);
        let mut map = self.touched.lock().unwrap();
        if map.len() >= MAX_TOUCHED {
            map.clear();
            self.touched_all.store(stamp, Ordering::SeqCst);
        }
        map.insert(node, stamp);
    }

    pub(super) fn untouched(&self, node: u64, since: u64) -> bool {
        self.touched_all.load(Ordering::SeqCst) <= since
            && self
                .touched
                .lock()
                .unwrap()
                .get(&node)
                .is_none_or(|t| *t <= since)
    }

    fn present_change(
        &self,
        c: &Change,
        st: &libc::stat,
        dir: &[u8],
        name: &[u8],
        from: Option<&Change>,
        out: &mut Vec<Event>,
    ) {
        let Some((parent, _)) = self.dir_node(dir) else {
            if let Some(from) = from {
                if let Some((fdir, fname)) = split(&from.path) {
                    self.absent_change(from, fdir, fname, out);
                }
            }
            return;
        };
        self.touch(parent);
        let isdir = st.st_mode as u32 & S_IFMT == S_IFDIR;
        let dirflag = if isdir { flag::ISDIR } else { 0 };
        let mut ident = self.object_node(st, parent, name);
        {
            let mut nodes = self.nodes.lock().unwrap();
            if nodes.by_id.get(&parent).is_some_and(|p| p.complete) {
                let own = ident.is_some_and(|id| {
                    nodes.told.get(&parent).and_then(|n| n.get(name)) == Some(&id)
                        && nodes.by_id.get(&id).is_some_and(|n| n.announced)
                });
                if !own {
                    if let Some(p) = nodes.by_id.get_mut(&parent) {
                        p.complete = false;
                    }
                    out.push(Event {
                        op: op::CREATE,
                        flags: flag::QUIET,
                        parent,
                        name: name.to_vec(),
                        ..Event::default()
                    });
                }
            }
        }
        match self
            .nodes
            .lock()
            .unwrap()
            .appeared(parent, name, MAX_APPEARED)
        {
            Some(names) => out.extend(names.into_iter().map(|n| Event {
                op: op::CREATE,
                flags: flag::QUIET,
                parent,
                name: n.into_vec(),
                ..Event::default()
            })),
            None => out.push(Event {
                op: op::INVAL_ALL,
                ..Event::default()
            }),
        }
        let (victim, announced) = {
            let mut nodes = self.nodes.lock().unwrap();
            let aside = nodes.take_displaced(parent, name);
            let told = nodes
                .told
                .get(&parent)
                .and_then(|n| n.get(name))
                .copied()
                .filter(|id| nodes.by_id.contains_key(id));
            let victim = aside
                .filter(|v| Some(*v) != ident)
                .or(told.filter(|t| Some(*t) != ident));
            let announced = ident
                .and_then(|id| nodes.by_id.get(&id))
                .is_some_and(|n| n.announced);
            (victim, announced)
        };
        let gone_flag = |v: u64, bit: u8| if self.node_fate(v).0 { bit } else { 0 };
        let mut made = false;
        let prior = self
            .seen
            .lock()
            .unwrap()
            .get(&(st.st_dev, st.st_ino))
            .copied();
        let announced = announced || prior.is_some();
        let recent = |s: i64| {
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map_or(0, |d| d.as_secs() as i64);
            (now - s).abs() <= FRESH
        };
        let born = recent(st.st_birthtime);
        let written = recent(st.st_mtime);

        if let Some((fdir, fname)) = from.and_then(|f| split(&f.path)) {
            let fparent = self.dir_node(fdir).map(|(id, _)| id).or_else(|| {
                let nodes = self.nodes.lock().unwrap();
                let at = nodes.by_id.get(&ident?)?.at.as_ref()?;
                (&*at.1 == fname && nodes.by_id.contains_key(&at.0)).then_some(at.0)
            });
            if let Some(fp) = fparent {
                self.touch(fp);
            }
            if ident.is_none() {
                if let Some(fp) = fparent {
                    let key = Key::Tree {
                        parent: fp,
                        name: fname.into(),
                    };
                    let mut nodes = self.nodes.lock().unwrap();
                    if let Some(id) = nodes.by_key.get(&key).copied() {
                        nodes.rekey(
                            id,
                            Key::Tree {
                                parent,
                                name: name.into(),
                            },
                        );
                        ident = Some(id);
                    }
                }
            }
            out.push(Event {
                op: op::RENAME,
                flags: dirflag | victim.map_or(0, |v| gone_flag(v, flag::VICTIM_GONE)),
                parent: fparent.unwrap_or(0),
                child: ident.unwrap_or(0),
                parent2: parent,
                victim: victim.unwrap_or(0),
                name: fname.to_vec(),
                name2: name.to_vec(),
                ..Event::default()
            });
            let mut nodes = self.nodes.lock().unwrap();
            let was_watched = ident.is_some_and(|id| nodes.watched(id));
            if let Some(fp) = fparent {
                nodes.untell(fp, fname);
                nodes.take_displaced(fp, fname);
            }
            nodes.untell(parent, name);
            if let Some(id) = ident {
                nodes.tell(parent, name, id);
            }
            if isdir && was_watched && !ident.is_some_and(|id| nodes.watched(id)) {
                out.push(Event {
                    op: op::INVAL_ALL,
                    ..Event::default()
                });
            }
            made = true;
        } else if let Some(v) = victim {
            let (gone, vdir) = self.node_fate(v);
            let gone = gone || vdir;
            out.push(Event {
                op: op::DELETE,
                flags: if gone { flag::LAST_LINK } else { 0 } | if vdir { flag::ISDIR } else { 0 },
                parent,
                child: v,
                name: name.to_vec(),
                ..Event::default()
            });
            out.push(Event {
                op: op::CREATE,
                flags: dirflag,
                parent,
                name: name.to_vec(),
                ..Event::default()
            });
            let mut nodes = self.nodes.lock().unwrap();
            if nodes.told.get(&parent).and_then(|n| n.get(name)) == Some(&v) {
                nodes.untell(parent, name);
            }
            made = true;
        } else if !announced
            && (c.flags & (fse::CREATED | fse::RENAMED) != 0 || (ident.is_none() && born))
        {
            out.push(Event {
                op: op::CREATE,
                flags: dirflag,
                parent,
                name: name.to_vec(),
                ..Event::default()
            });
            made = true;
        }
        if made {
            if let Some(id) = ident {
                if let Some(n) = self.nodes.lock().unwrap().by_id.get_mut(&id) {
                    n.announced = true;
                }
            }
            if !isdir && st.st_nlink > 1 {
                if let Some(id) = ident {
                    out.push(Event {
                        op: op::CHANGE,
                        mask: mask::ATTRIB,
                        child: id,
                        ..Event::default()
                    });
                }
            }
        }

        let state = State::of(st);
        let guest_wrote = ident
            .and_then(|id| self.nodes.lock().unwrap().by_id.get(&id)?.dirty.clone())
            .is_some_and(|d| d.swap(false, Ordering::Relaxed));
        if guest_wrote || prior == Some(state) {
            self.remember(st);
            return;
        }
        self.remember(st);
        let content = match (prior, from) {
            (Some(p), _) => p.size != state.size || p.mtime != state.mtime,
            (None, Some(from)) => {
                victim.is_some() || from.flags & (fse::MODIFIED | fse::CREATED) != 0
            }
            (None, None) => {
                c.flags & (fse::MODIFIED | fse::CREATED | fse::RENAMED) != 0 || made || written
            }
        };
        let what = if isdir || st.st_mode as u32 & S_IFMT == S_IFLNK {
            if c.flags & fse::META != 0 && !made {
                mask::ATTRIB
            } else {
                0
            }
        } else if content {
            mask::CONTENT
        } else if (c.flags & fse::META != 0 || prior.is_some()) && from.is_none() {
            mask::ATTRIB
        } else {
            0
        };
        if what != 0 {
            out.push(Event {
                op: op::CHANGE,
                flags: dirflag,
                mask: what,
                parent,
                child: ident.unwrap_or(0),
                name: name.to_vec(),
                ..Event::default()
            });
        }
    }

    fn absent_change(&self, c: &Change, dir: &[u8], name: &[u8], out: &mut Vec<Event>) {
        let parent = self.dir_node(dir).map(|(id, _)| id);
        let by_name = parent.and_then(|p| {
            let mut nodes = self.nodes.lock().unwrap();
            let aside = nodes.take_displaced(p, name);
            let told = nodes.untell(p, name);
            aside
                .or(told)
                .filter(|id| nodes.by_id.contains_key(id))
                .or_else(|| {
                    nodes
                        .by_key
                        .get(&Key::Tree {
                            parent: p,
                            name: name.into(),
                        })
                        .copied()
                })
        });
        let mut parent = parent;
        let child = by_name.or_else(|| {
            let id = self.node_by_ino(c.ino)?;
            let was_in = {
                let nodes = self.nodes.lock().unwrap();
                let at = nodes.by_id.get(&id)?.at.as_ref()?;
                if &*at.1 != name {
                    return None;
                }
                at.0
            };
            if parent.is_none_or(|p| p == was_in) {
                return Some(id);
            }
            if c.flags & fse::IS_HARDLINK == 0 {
                parent = None;
                return Some(id);
            }
            None
        });
        let parent = parent.or_else(|| {
            let nodes = self.nodes.lock().unwrap();
            let at = nodes.by_id.get(&child?)?.at.as_ref()?;
            (&*at.1 == name && nodes.by_id.contains_key(&at.0)).then_some(at.0)
        });
        if parent.is_none() && child.is_none() {
            return;
        }
        if child.is_none() && c.flags & (fse::REMOVED | fse::RENAMED) == 0 {
            return;
        }
        if let Some(p) = parent {
            self.touch(p);
        }
        let mut moved_away = false;
        let (gone, isdir) = match child {
            Some(id) => {
                let (gone, isdir) = self.node_fate(id);
                moved_away = isdir && !gone;
                (gone || isdir, isdir)
            }
            None => (
                c.flags & fse::IS_HARDLINK == 0 || c.flags & fse::IS_LAST_HARDLINK != 0,
                c.flags & fse::IS_DIR != 0,
            ),
        };
        out.push(Event {
            op: op::DELETE,
            flags: if gone { flag::LAST_LINK } else { 0 } | if isdir { flag::ISDIR } else { 0 },
            parent: parent.unwrap_or(0),
            child: child.unwrap_or(0),
            name: name.to_vec(),
            ..Event::default()
        });
        if let Some(id) = child {
            let mut nodes = self.nodes.lock().unwrap();
            if !gone {
                drop(nodes);
                out.push(Event {
                    op: op::CHANGE,
                    mask: mask::ATTRIB,
                    child: id,
                    ..Event::default()
                });
            } else if matches!(nodes.by_id.get(&id).map(|n| &n.key), Some(Key::Tree { .. })) {
                nodes.orphaned(id);
            }
        }
        if moved_away {
            out.push(Event {
                op: op::INVAL_ALL,
                ..Event::default()
            });
        }
    }
}
