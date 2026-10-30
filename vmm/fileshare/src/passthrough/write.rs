use std::ffi::CString;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::Duration;

use super::*;
use crate::fs::{SetAttr, SetTime, Xattr};
use crate::server::{MAX_XATTR, VALID};

const L_O_EXCL: u32 = 0o200;
const L_O_TRUNC: u32 = 0o1000;
const L_O_APPEND: u32 = 0o2000;
const L_RENAME_NOREPLACE: u32 = 1;
const L_RENAME_EXCHANGE: u32 = 2;
const L_FALLOC_KEEP_SIZE: u32 = 1;
const L_FALLOC_PUNCH_HOLE: u32 = 2;
const L_SEEK_DATA: u32 = 3;
const L_SEEK_HOLE: u32 = 4;
const L_XATTR_CREATE: u32 = 1;
const L_XATTR_REPLACE: u32 = 2;
const E2BIG: Errno = Errno(7);
const EEXIST: Errno = Errno(17);
const EXDEV: Errno = Errno(18);

const RENAME_NOFOLLOW_ANY: libc::c_uint = 0x10;
const XATTR_NAME_MAX: usize = 127;
const CAPABILITY: &[u8] = b"security.capability";
const O_SEARCH: i32 = 0x4000_0000 | libc::O_DIRECTORY;
const ATTR_CMN_PARENTID: u32 = 0x0400_0000;

fn parent_and_name(path: &[u8]) -> Option<(u64, Vec<u8>)> {
    let list = AttrList {
        bitmapcount: ATTR_BIT_MAP_COUNT,
        reserved: 0,
        commonattr: ATTR_CMN_NAME | ATTR_CMN_PARENTID,
        volattr: 0,
        dirattr: 0,
        fileattr: 0,
        forkattr: 0,
    };
    let mut buf = [0u8; 20 + 1024];
    getattrlist(path, &list, &mut buf).ok()?;
    let offset = i32::from_le_bytes(buf[4..8].try_into().unwrap());
    let len = u32::from_le_bytes(buf[8..12].try_into().unwrap()) as usize;
    let parent = u64::from_le_bytes(buf[12..20].try_into().unwrap());
    let start = 4usize.checked_add(usize::try_from(offset).ok()?)?;
    let name = buf.get(start..start.checked_add(len)?)?;
    let name = name.split(|b| *b == 0).next()?.to_vec();
    (!name.is_empty() && !name.contains(&b'/')).then_some((parent, name))
}

pub(super) struct At {
    dir: Option<Arc<OwnedFd>>,
    path: CString,
    full: Vec<u8>,
    dev: i32,
    tree: bool,
    denied: bool,
}

impl At {
    fn makeable(&self) -> Result<()> {
        if !self.denied {
            return Ok(());
        }
        Err(if self.stat().is_ok() {
            EEXIST
        } else {
            Errno::EACCES
        })
    }

    fn fd(&self) -> RawFd {
        self.dir.as_ref().map_or(libc::AT_FDCWD, |d| d.as_raw_fd())
    }

    fn stat(&self) -> Result<libc::stat> {
        let mut st = unsafe { std::mem::zeroed::<libc::stat>() };
        let flags = AT_SYMLINK_NOFOLLOW_ANY;
        if unsafe { libc::fstatat(self.fd(), self.path.as_ptr(), &mut st, flags) } != 0 {
            return Err(Errno::last());
        }
        Ok(st)
    }

    fn open(&self, flags: i32, mode: u32) -> Result<OwnedFd> {
        loop {
            let fd = unsafe {
                libc::openat(
                    self.fd(),
                    self.path.as_ptr(),
                    flags | libc::O_CLOEXEC | libc::O_NOFOLLOW_ANY | libc::O_NONBLOCK,
                    mode as libc::c_uint,
                )
            };
            if fd >= 0 {
                return Ok(unsafe { OwnedFd::from_raw_fd(fd) });
            }
            if std::io::Error::last_os_error().kind() != std::io::ErrorKind::Interrupted {
                return Err(Errno::last());
            }
        }
    }

    fn handle(&self) -> Result<OwnedFd> {
        self.open(libc::O_EVTONLY | libc::O_SYMLINK, 0)
    }

    fn chmod(&self, mode: u32) -> Result<()> {
        let flags = libc::AT_SYMLINK_NOFOLLOW;
        check(unsafe { libc::fchmodat(self.fd(), self.path.as_ptr(), mode as libc::mode_t, flags) })
    }
}

fn check(ret: libc::c_int) -> Result<()> {
    if ret == 0 {
        Ok(())
    } else {
        Err(Errno::last())
    }
}

pub(super) fn sync_fd(fd: RawFd, mode: Sync) -> Result<()> {
    let fcntl = |cmd| loop {
        if unsafe { libc::fcntl(fd, cmd) } != -1 {
            return true;
        }
        if std::io::Error::last_os_error().kind() != std::io::ErrorKind::Interrupted {
            return false;
        }
    };
    let done = match mode {
        Sync::Fsync => false,
        Sync::Barrier => fcntl(libc::F_BARRIERFSYNC),
        Sync::Full => fcntl(libc::F_FULLFSYNC),
    };
    if done {
        return Ok(());
    }
    loop {
        if unsafe { libc::fsync(fd) } == 0 {
            return Ok(());
        }
        if std::io::Error::last_os_error().kind() != std::io::ErrorKind::Interrupted {
            return Err(Errno::last());
        }
    }
}

fn kill_suidgid(fd: RawFd) {
    let Ok(st) = fstat(fd) else { return };
    let mode = st.st_mode as u32;
    let mut kill = mode & S_ISUID;
    if mode & 0o010 != 0 {
        kill |= mode & S_ISGID;
    }
    if kill != 0 {
        unsafe { libc::fchmod(fd, (mode & 0o7777 & !kill) as libc::mode_t) };
    }
}

fn drop_caps(fd: RawFd) {
    let name = c"security.capability";
    unsafe { libc::fremovexattr(fd, name.as_ptr(), 0) };
}

impl Passthrough {
    fn node_free(&self, node: u64) -> bool {
        self.nodes
            .lock()
            .unwrap()
            .by_id
            .get(&node)
            .is_some_and(|n| n.free)
    }

    fn writable(&self) -> Result<()> {
        if self.read_only {
            Err(Errno::EROFS)
        } else {
            Ok(())
        }
    }

    fn vol(&self, dev: i32) -> Option<Volume> {
        self.volumes.lock().unwrap().get(&dev).copied()
    }

    fn checked_dir(&self, path: &[u8], dev: i32, ino: Option<u64>) -> Result<OwnedFd> {
        let fd = open(path, O_SEARCH).map_err(|e| match e {
            Errno::ENOENT if path.starts_with(b"/.vol/") => Errno::ESTALE,
            e => e,
        })?;
        let st = fstat(fd.as_raw_fd())?;
        if st.st_mode as u32 & S_IFMT != S_IFDIR
            || st.st_dev != dev
            || ino.is_some_and(|i| st.st_ino != i)
            || self.is_denied(&st)
        {
            return Err(Errno::ESTALE);
        }
        Ok(fd)
    }

    fn tree_dir(&self, path: &[u8], dev: i32) -> Result<OwnedFd> {
        self.checked_dir(path, dev, None)
    }

    fn child_at(&self, parent: u64, name: &[u8]) -> Result<At> {
        check_name(name)?;
        let (key, kind, dev) = {
            let nodes = self.nodes.lock().unwrap();
            let n = nodes.by_id.get(&parent).ok_or(Errno::ESTALE)?;
            (n.key.clone(), n.kind, n.dev)
        };
        if kind != S_IFDIR {
            return Err(Errno::ENOTDIR);
        }
        if self.hidden(dev) || self.apple_double(dev, name) {
            return Err(Errno::EPERM);
        }
        let denied = self.place_denied(parent, name);
        match key {
            Key::Vol { dev, ino } => {
                let mut full = volfs_path(dev, ino);
                let kept = self.dir_fds.lock().unwrap().get(parent);
                let dir = match kept {
                    Some(dir) => dir,
                    None => {
                        let dir = Arc::new(self.checked_dir(&full, dev, Some(ino))?);
                        if self.vol(dev).is_some_and(|v| v.boot) {
                            self.dir_fds.lock().unwrap().insert(parent, dir.clone());
                        }
                        dir
                    }
                };
                full.push(b'/');
                full.extend_from_slice(name);
                Ok(At {
                    dir: Some(dir),
                    path: cstr(name)?,
                    full,
                    dev,
                    tree: false,
                    denied,
                })
            }
            Key::Tree { .. } => {
                let (mut full, _, _) = self.node_path(parent)?;
                let dir = self.tree_dir(&full, dev)?;
                full.push(b'/');
                full.extend_from_slice(name);
                Ok(At {
                    dir: Some(Arc::new(dir)),
                    path: cstr(name)?,
                    full,
                    dev,
                    tree: true,
                    denied,
                })
            }
            Key::Gone => Err(Errno::ESTALE),
        }
    }

    fn node_at(&self, node: u64) -> Result<(At, u32)> {
        let (key, kind, dev, crossing) = {
            let nodes = self.nodes.lock().unwrap();
            let n = nodes.by_id.get(&node).ok_or(Errno::ESTALE)?;
            (n.key.clone(), n.kind, n.dev, n.crossing)
        };
        let at = match key {
            Key::Vol { .. } if node == ROOT_ID => At {
                dir: None,
                path: cstr(&self.root_path)?,
                full: self.root_path.clone(),
                dev,
                tree: false,
                denied: false,
            },
            Key::Vol { dev, ino } if crossing => {
                let full = volfs_path(dev, ino);
                At {
                    dir: Some(Arc::new(self.checked_dir(&full, dev, Some(ino))?)),
                    path: cstr(b".")?,
                    full,
                    dev,
                    tree: false,
                    denied: false,
                }
            }
            Key::Vol { dev, ino } => {
                let full = volfs_path(dev, ino);
                let (parent, name) = parent_and_name(&full).ok_or(Errno::ESTALE)?;
                let dir = self.checked_dir(&volfs_path(dev, parent), dev, Some(parent))?;
                let at = At {
                    dir: Some(Arc::new(dir)),
                    path: cstr(&name)?,
                    full,
                    dev,
                    tree: false,
                    denied: false,
                };
                match at.stat() {
                    Ok(st) if st.st_dev == dev && st.st_ino == ino => at,
                    _ => return Err(Errno::ESTALE),
                }
            }
            Key::Tree { parent: 0, .. } => At {
                dir: None,
                path: cstr(&self.root_path)?,
                full: self.root_path.clone(),
                dev,
                tree: true,
                denied: false,
            },
            Key::Tree { parent, name } => {
                let (mut full, _, pdev) = self.node_path(parent)?;
                let dir = self.tree_dir(&full, pdev)?;
                full.push(b'/');
                full.extend_from_slice(&name);
                At {
                    dir: Some(Arc::new(dir)),
                    path: cstr(&name)?,
                    full,
                    dev,
                    tree: true,
                    denied: false,
                }
            }
            Key::Gone => return Err(Errno::ESTALE),
        };
        Ok((at, kind))
    }

    fn tree_name(&self, at: &At, name: &[u8]) -> Box<[u8]> {
        let folds = self.vol(at.dev).is_none_or(|v| !v.case_sensitive);
        if folds || !name.is_ascii() {
            if let Some(n) = name_on_disk(&at.full) {
                return n.into();
            }
        }
        name.into()
    }

    fn known(&self, st: &libc::stat, parent: u64, at: &At, name: &[u8]) -> Option<u64> {
        let key = if at.tree {
            Key::Tree {
                parent,
                name: self.tree_name(at, name),
            }
        } else {
            Key::Vol {
                dev: st.st_dev,
                ino: st.st_ino,
            }
        };
        self.nodes.lock().unwrap().by_key.get(&key).copied()
    }

    fn keep_orphan(&self, node: Option<u64>, st: &libc::stat, at: &At) {
        let Some(node) = node else { return };
        if st.st_mode as u32 & S_IFMT != S_IFREG || st.st_nlink > 1 {
            return;
        }
        {
            let nodes = self.nodes.lock().unwrap();
            match nodes.by_id.get(&node) {
                Some(n) if n.open > 0 && n.orphan.is_none() => {}
                Some(n) if n.free && n.orphan.is_none() => {
                    if self.free_fds.lock().unwrap().get(node).is_none() {
                        return;
                    }
                }
                _ => return,
            }
        }
        let held = self.handles.lock().unwrap().map.len() + self.orphans.load(Ordering::Relaxed);
        if held >= self.max_handles {
            return;
        }
        let kept = self.free_fds.lock().unwrap().get(node);
        let fd = match kept {
            Some(fd) => fd,
            None => {
                let Ok(fd) = at.open(libc::O_RDONLY, 0) else {
                    return;
                };
                match fstat(fd.as_raw_fd()) {
                    Ok(now) if now.st_dev == st.st_dev && now.st_ino == st.st_ino => {}
                    _ => return,
                }
                Arc::new(fd)
            }
        };
        let mut nodes = self.nodes.lock().unwrap();
        if let Some(n) = nodes.by_id.get_mut(&node) {
            if n.orphan.is_none() {
                n.orphan = Some(fd);
                self.orphans.fetch_add(1, Ordering::Relaxed);
            }
        }
    }

    fn made(&self, parent: u64, name: &[u8], at: &At, spelled: bool) -> Result<Entry> {
        let since = self.changes.load(Ordering::SeqCst);
        let st = at.stat()?;
        let entry = self.enter(
            (parent, at.dev),
            name,
            &st,
            &|| at.full.clone(),
            spelled,
            since,
        )?;
        if let Some(n) = self.nodes.lock().unwrap().by_id.get_mut(&entry.nodeid) {
            n.announced = true;
        }
        self.own(&st);
        Ok(entry)
    }

    pub(super) fn w_open(&self, node: u64, flags: u32, kill: bool) -> Result<(u64, u32)> {
        let access = flags & 3;
        let trunc = flags & L_O_TRUNC != 0;
        if access == 3 {
            return Err(Errno::EINVAL);
        }
        if access != 0 || trunc {
            self.writable()?;
        }
        let fd = self.open_node(node, S_IFREG, access as i32)?;
        if trunc {
            let cutter = if access == 0 {
                Some(self.open_node(node, S_IFREG, libc::O_WRONLY)?)
            } else {
                None
            };
            let wfd = cutter.as_ref().unwrap_or(&fd).as_raw_fd();
            check(unsafe { libc::ftruncate(wfd, 0) })?;
            if kill {
                kill_suidgid(wfd);
            }
            drop_caps(wfd);
        }
        let write = (access != 0).then_some(flags & L_O_APPEND != 0);
        let keep = match fstat(fd.as_raw_fd()) {
            Ok(st) if !trunc => self.open_flags(node, &st),
            _ => 0,
        };
        let parent = {
            let nodes = self.nodes.lock().unwrap();
            nodes
                .by_id
                .get(&node)
                .and_then(|n| n.at.as_ref().map(|a| a.0))
        };
        if let Some(parent) = parent {
            self.dir_opened(parent);
        }
        Ok((self.add_handle(node, fd, false, write, false)?, keep))
    }

    pub(super) fn w_create(
        &self,
        parent: u64,
        name: &[u8],
        mode: u32,
        umask: u32,
        flags: u32,
    ) -> Result<(Entry, u64, u32)> {
        self.writable()?;
        let access = flags & 3;
        if access == 3 {
            return Err(Errno::EINVAL);
        }
        if std::str::from_utf8(name).is_err() {
            return Err(Errno::EILSEQ);
        }
        let at = self.child_at(parent, name)?;
        if at.denied {
            return Err(Errno::EACCES);
        }
        let perm = mode & 0o7777 & !umask;
        let (fd, created) =
            match at.open(access as i32 | libc::O_CREAT | libc::O_EXCL, perm | 0o600) {
                Ok(fd) => (fd, true),
                Err(EEXIST) if flags & L_O_EXCL == 0 => (at.open(access as i32, 0)?, false),
                Err(e) => return Err(e),
            };
        let st = fstat(fd.as_raw_fd())?;
        if st.st_mode as u32 & S_IFMT != S_IFREG || st.st_dev != at.dev || self.is_denied(&st) {
            return Err(if created { Errno::EIO } else { Errno::EACCES });
        }
        if created {
            if st.st_mode as u32 & 0o7777 != perm {
                unsafe { libc::fchmod(fd.as_raw_fd(), perm as libc::mode_t) };
            }
        } else if flags & L_O_TRUNC != 0 {
            let cutter = if access == 0 {
                Some(at.open(libc::O_WRONLY, 0)?)
            } else {
                None
            };
            let wfd = cutter.as_ref().unwrap_or(&fd).as_raw_fd();
            let same = fstat(wfd)?;
            if same.st_ino != st.st_ino || same.st_dev != st.st_dev {
                return Err(Errno::ESTALE);
            }
            check(unsafe { libc::ftruncate(wfd, 0) })?;
            kill_suidgid(wfd);
            drop_caps(wfd);
        }
        self.dir_opened(parent);
        let since = self.changes.load(Ordering::SeqCst);
        let st = if (created && st.st_mode as u32 & 0o7777 == perm)
            || (!created && flags & L_O_TRUNC == 0)
        {
            st
        } else {
            fstat(fd.as_raw_fd())?
        };
        let mut entry = self.enter(
            (parent, at.dev),
            name,
            &st,
            &|| at.full.clone(),
            created,
            since,
        )?;
        if created {
            if let Some(n) = self.nodes.lock().unwrap().by_id.get_mut(&entry.nodeid) {
                n.announced = true;
            }
            entry.attr.flags |= crate::abi::ATTR_NO_SEC;
        }
        self.own(&st);
        let write = (access != 0).then_some(flags & L_O_APPEND != 0);
        let keep = if created || flags & L_O_TRUNC != 0 {
            0
        } else {
            self.open_flags(entry.nodeid, &st)
        };
        match self.add_handle(entry.nodeid, fd, false, write, created) {
            Ok(fh) => Ok((entry, fh, keep)),
            Err(e) => {
                self.nodes.lock().unwrap().forget(entry.nodeid, 1);
                Err(e)
            }
        }
    }

    pub(super) fn w_mkdir(&self, parent: u64, name: &[u8], mode: u32, umask: u32) -> Result<Entry> {
        self.writable()?;
        let at = self.child_at(parent, name)?;
        at.makeable()?;
        let perm = mode & 0o7777 & !umask;
        let since = self.changes.load(Ordering::SeqCst);
        check(unsafe { libc::mkdirat(at.fd(), at.path.as_ptr(), perm as libc::mode_t) })?;
        if at
            .stat()
            .map_or(true, |st| st.st_mode as u32 & 0o7777 != perm)
        {
            let _ = at.chmod(perm);
        }
        #[cfg(test)]
        {
            let hook = self.after_stat.lock().unwrap().take();
            if let Some(hook) = hook {
                hook();
            }
        }
        let mut entry = self.made(parent, name, &at, true)?;
        {
            let mut nodes = self.nodes.lock().unwrap();
            let within = nodes.by_id.get_mut(&parent).is_some_and(|p| {
                p.dir_ok = true;
                !matches!(p.key, Key::Vol { dev, ino } if self.class_parents.contains(&(dev, ino)))
            });
            if let Some(n) = nodes.by_id.get_mut(&entry.nodeid) {
                n.dir_ok = within;
            }
        }
        if self.complete_dirs.load(Ordering::Relaxed)
            && entry.entry_valid == LONG
            && entry.attr_valid == LONG
        {
            let mut nodes = self.nodes.lock().unwrap();
            if let Some(n) = nodes.by_id.get_mut(&entry.nodeid) {
                n.complete = true;
                if self.changes.load(Ordering::SeqCst) == since {
                    entry.attr.flags |= crate::abi::ATTR_COMPLETE;
                } else {
                    n.complete = false;
                }
            }
        }
        Ok(entry)
    }

    pub(super) fn w_symlink(&self, parent: u64, name: &[u8], target: &[u8]) -> Result<Entry> {
        self.writable()?;
        let at = self.child_at(parent, name)?;
        at.makeable()?;
        let target = cstr(target)?;
        check(unsafe { libc::symlinkat(target.as_ptr(), at.fd(), at.path.as_ptr()) })?;
        self.made(parent, name, &at, true)
    }

    pub(super) fn w_mknod(
        &self,
        parent: u64,
        name: &[u8],
        mode: u32,
        rdev: u32,
        umask: u32,
    ) -> Result<Entry> {
        self.writable()?;
        let at = self.child_at(parent, name)?;
        at.makeable()?;
        let perm = mode & 0o7777 & !umask;
        match mode & S_IFMT {
            0 | S_IFREG => {
                let fd = at.open(libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL, perm | 0o600)?;
                unsafe { libc::fchmod(fd.as_raw_fd(), perm as libc::mode_t) };
            }
            S_IFIFO => {
                check(unsafe { libc::mkfifoat(at.fd(), at.path.as_ptr(), perm as libc::mode_t) })?;
                let _ = at.chmod(perm);
            }
            kind @ (S_IFCHR | S_IFBLK | S_IFSOCK) => {
                if !self.vol(at.dev).is_some_and(|v| v.xattrs) {
                    return Err(Errno::EPERM);
                }
                let fd = at.open(libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL, 0o600)?;
                let value = Self::placeholder(kind, rdev, perm);
                let key = cstr(NODE_KEY)?;
                let set = unsafe {
                    libc::fsetxattr(
                        fd.as_raw_fd(),
                        key.as_ptr(),
                        value.as_ptr().cast(),
                        value.len(),
                        0,
                        0,
                    )
                };
                let marked = set == 0
                    && unsafe { libc::fchmod(fd.as_raw_fd(), PLACEHOLDER_MODE as libc::mode_t) }
                        == 0;
                if !marked {
                    unsafe { libc::unlinkat(at.fd(), at.path.as_ptr(), 0) };
                    return Err(Errno::EPERM);
                }
            }
            _ => return Err(Errno::EINVAL),
        }
        self.made(parent, name, &at, true)
    }

    pub(super) fn w_link(&self, node: u64, newparent: u64, newname: &[u8]) -> Result<Entry> {
        self.writable()?;
        let (src, kind) = self.node_at(node)?;
        if src.tree || kind == S_IFDIR {
            return Err(Errno::EPERM);
        }
        let dst = self.child_at(newparent, newname)?;
        dst.makeable()?;
        let was = src.stat()?;
        check(unsafe {
            libc::linkat(src.fd(), src.path.as_ptr(), dst.fd(), dst.path.as_ptr(), 0)
        })?;
        match dst.stat() {
            Ok(st) if st.st_dev == was.st_dev && st.st_ino == was.st_ino => {}
            _ => {
                unsafe { libc::unlinkat(dst.fd(), dst.path.as_ptr(), 0) };
                return Err(Errno::ESTALE);
            }
        }
        self.made(newparent, newname, &dst, true)
    }

    pub(super) fn w_remove(&self, parent: u64, name: &[u8], dir: bool) -> Result<()> {
        self.writable()?;
        let mut at = self.child_at(parent, name)?;
        let _tree = if at.tree {
            let guard = self.tree_write();
            at = self.child_at(parent, name)?;
            Some(guard)
        } else {
            None
        };
        let st = at.stat()?;
        if at.denied || self.is_denied(&st) {
            return Err(Errno::ENOENT);
        }
        let victim = self.known(&st, parent, &at, name);
        if !dir {
            self.keep_orphan(victim, &st, &at);
        }
        let flags = if dir { libc::AT_REMOVEDIR } else { 0 };
        check(unsafe { libc::unlinkat(at.fd(), at.path.as_ptr(), flags) })?;
        let mut nodes = self.nodes.lock().unwrap();
        nodes.untell(parent, name);
        nodes.take_displaced(parent, name);
        if at.tree {
            if let Some(id) = victim {
                nodes.orphaned(id);
            }
        }
        Ok(())
    }

    pub(super) fn w_rename(
        &self,
        parent: u64,
        name: &[u8],
        newparent: u64,
        newname: &[u8],
        flags: u32,
    ) -> Result<()> {
        self.writable()?;
        let exchange = flags == L_RENAME_EXCHANGE;
        let mac_flags = match flags {
            0 => 0,
            L_RENAME_NOREPLACE => libc::RENAME_EXCL,
            L_RENAME_EXCHANGE => libc::RENAME_SWAP,
            _ => return Err(Errno::EINVAL),
        };
        let mut from = self.child_at(parent, name)?;
        let mut to = self.child_at(newparent, newname)?;
        let _tree = if from.tree || to.tree {
            let guard = self.tree_write();
            from = self.child_at(parent, name)?;
            to = self.child_at(newparent, newname)?;
            Some(guard)
        } else {
            None
        };
        if from.tree != to.tree {
            return Err(EXDEV);
        }
        let st = from.stat()?;
        if from.denied || self.is_denied(&st) {
            return Err(Errno::ENOENT);
        }
        let there = to.stat().ok();
        if to.denied && there.is_none() {
            return Err(Errno::EACCES);
        }
        if to.denied || there.as_ref().is_some_and(|t| self.is_denied(t)) {
            return Err(if flags == L_RENAME_NOREPLACE {
                EEXIST
            } else {
                Errno::EACCES
            });
        }
        let moved = self.known(&st, parent, &from, name);
        let victim = there
            .as_ref()
            .and_then(|t| self.known(t, newparent, &to, newname));
        if let (Some(t), false) = (&there, exchange) {
            self.keep_orphan(victim, t, &to);
        }
        let rename = |flags: libc::c_uint| unsafe {
            libc::renameatx_np(
                from.fd(),
                from.path.as_ptr(),
                to.fd(),
                to.path.as_ptr(),
                flags | RENAME_NOFOLLOW_ANY,
            )
        };
        if rename(mac_flags) != 0 {
            let e = Errno::last();
            if e != Errno::EOPNOTSUPP || flags != L_RENAME_NOREPLACE {
                return Err(if e == Errno::EOPNOTSUPP {
                    Errno::EINVAL
                } else {
                    e
                });
            }
            if there.is_some() {
                return Err(EEXIST);
            }
            check(rename(0))?;
        }
        let new_key = to.tree.then(|| Key::Tree {
            parent: newparent,
            name: self.tree_name(&to, newname),
        });
        let old_key = from.tree.then(|| Key::Tree {
            parent,
            name: match &moved {
                Some(_) => self
                    .nodes
                    .lock()
                    .unwrap()
                    .by_id
                    .get(&moved.unwrap())
                    .and_then(|n| match &n.key {
                        Key::Tree { name, .. } => Some(name.clone()),
                        _ => None,
                    })
                    .unwrap_or_else(|| name.into()),
                None => name.into(),
            },
        });
        let mut nodes = self.nodes.lock().unwrap();
        let a = nodes.untell(parent, name);
        let b = nodes.untell(newparent, newname);
        nodes.take_displaced(parent, name);
        nodes.take_displaced(newparent, newname);
        if let Some(id) = a.or(moved) {
            nodes.tell(newparent, newname, id);
        }
        if exchange {
            if let Some(id) = b.or(victim) {
                nodes.tell(parent, name, id);
            }
        }
        if let (Some(new_key), Some(old_key)) = (new_key, old_key) {
            match (moved, victim, exchange) {
                (Some(m), Some(v), true) => {
                    nodes.by_key.remove(&new_key);
                    nodes.by_key.remove(&old_key);
                    for (id, key) in [(m, new_key), (v, old_key)] {
                        if let Some(n) = nodes.by_id.get_mut(&id) {
                            let was = std::mem::replace(&mut n.key, key.clone());
                            nodes.by_key.insert(key.clone(), id);
                            let moved_parent = match (&was, &key) {
                                (Key::Tree { parent: p, .. }, Key::Tree { parent: q, .. }) => {
                                    (p != q).then_some((*p, *q))
                                }
                                _ => None,
                            };
                            if let Some((p, q)) = moved_parent {
                                if let Some(n) = nodes.by_id.get_mut(&q) {
                                    n.children += 1;
                                }
                                if let Some(n) = nodes.by_id.get_mut(&p) {
                                    n.children = n.children.saturating_sub(1);
                                }
                            }
                        }
                    }
                }
                (Some(m), None, true) => nodes.rekey(m, new_key),
                (None, Some(v), true) => nodes.rekey(v, old_key),
                (Some(m), _, false) => nodes.rekey(m, new_key),
                (None, Some(v), false) => nodes.orphaned(v),
                (None, None, _) => {}
            }
        }
        Ok(())
    }

    pub(super) fn w_setattr(&self, node: u64, set: &SetAttr) -> Result<(Attr, Duration)> {
        self.writable()?;
        let handle = set.fh.map(|fh| self.handle(node, fh)).transpose()?;
        let orphan = if handle.is_none() {
            self.orphan(node)
        } else {
            None
        };
        let fd = handle
            .as_ref()
            .map(|h| h.fd.as_raw_fd())
            .or(orphan.as_ref().map(|o| o.as_raw_fd()));
        let at = match fd {
            Some(_) => None,
            None => Some(self.node_at(node)?),
        };
        if let Some(size) = set.size {
            let size = i64::try_from(size).map_err(|_| Errno::EINVAL)?;
            let opened;
            let wfd = match (&handle, &at) {
                (Some(h), _) => h.fd.as_raw_fd(),
                (None, Some((at, kind))) => {
                    if *kind != S_IFREG {
                        return Err(if *kind == S_IFDIR {
                            Errno::EISDIR
                        } else {
                            Errno::EINVAL
                        });
                    }
                    opened = at.open(libc::O_WRONLY, 0)?;
                    let st = fstat(opened.as_raw_fd())?;
                    if st.st_mode as u32 & S_IFMT != S_IFREG
                        || st.st_dev != at.dev
                        || self.is_denied(&st)
                    {
                        return Err(Errno::ESTALE);
                    }
                    opened.as_raw_fd()
                }
                (None, None) => return Err(Errno::EBADF),
            };
            check(unsafe { libc::ftruncate(wfd, size) })?;
            if set.kill_suidgid {
                kill_suidgid(wfd);
            }
            drop_caps(wfd);
            if let Some(h) = &handle {
                if let Some(d) = &h.dirty {
                    d.store(true, Ordering::Relaxed);
                }
            }
        }

        if set.uid.is_some() || set.gid.is_some() {
            match (fd, &at) {
                (Some(fd), _) => {
                    kill_suidgid(fd);
                    drop_caps(fd);
                }
                (None, Some((at, kind))) if !at.tree && *kind == S_IFREG => {
                    if let Ok(st) = at.stat() {
                        let mode = st.st_mode as u32;
                        if mode & (S_ISUID | S_ISGID) != 0 && set.mode.is_none() {
                            let mut kill = mode & S_ISUID;
                            if mode & 0o010 != 0 {
                                kill |= mode & S_ISGID;
                            }
                            let _ = at.chmod(mode & 0o7777 & !kill);
                        }
                    }
                    if let Ok(fd) = at.handle() {
                        drop_caps(fd.as_raw_fd());
                    }
                }
                _ => {}
            }
        }

        if let Some(mode) = set.mode {
            let perm = mode & 0o7777;
            match (fd, &at) {
                (Some(fd), _) => check(unsafe { libc::fchmod(fd, perm as libc::mode_t) })?,
                (None, Some((at, kind))) if matches!(*kind, S_IFCHR | S_IFBLK | S_IFSOCK) => {
                    let st = at.stat()?;
                    let (_, rdev) = self.present(&st, &|| at.full.clone());
                    let value = Self::placeholder(*kind, rdev, perm);
                    let key = cstr(NODE_KEY)?;
                    if at.tree {
                        return Err(Errno::EPERM);
                    }
                    let fd = at.handle()?;
                    check(unsafe {
                        libc::fsetxattr(
                            fd.as_raw_fd(),
                            key.as_ptr(),
                            value.as_ptr().cast(),
                            value.len(),
                            0,
                            libc::XATTR_REPLACE,
                        )
                    })?;
                }
                (None, Some((at, _))) => at.chmod(perm)?,
                (None, None) => {}
            }
        }

        if set.atime.is_some() || set.mtime.is_some() {
            let spec = |t: Option<SetTime>| match t {
                None => libc::timespec {
                    tv_sec: 0,
                    tv_nsec: libc::UTIME_OMIT,
                },
                Some(SetTime::Now) => libc::timespec {
                    tv_sec: 0,
                    tv_nsec: libc::UTIME_NOW,
                },
                Some(SetTime::At(s, ns)) => libc::timespec {
                    tv_sec: s as i64,
                    tv_nsec: ns as i64,
                },
            };
            let times = [spec(set.atime), spec(set.mtime)];
            match (fd, &at) {
                (Some(fd), _) => check(unsafe { libc::futimens(fd, times.as_ptr()) })?,
                (None, Some((at, _))) => check(unsafe {
                    libc::utimensat(
                        at.fd(),
                        at.path.as_ptr(),
                        times.as_ptr(),
                        libc::AT_SYMLINK_NOFOLLOW,
                    )
                })?,
                (None, None) => {}
            }
        }

        match (fd, &at) {
            (Some(fd), _) => {
                let st = fstat(fd)?;
                self.own(&st);
                let free = self.node_free(node);
                let attr = Self::marked(self.attr(&st, node, st.st_mode as u32, 0), false, &st);
                let mut attr = self.openable(attr, free);
                if orphan.is_some() {
                    attr.nlink = 0;
                }
                Ok((attr, VALID))
            }
            (None, Some((at, _))) => {
                let st = at.stat()?;
                self.own(&st);
                let (mode, rdev) = self.present(&st, &|| at.full.clone());
                let watched = !self.feed_stuck() && self.nodes.lock().unwrap().watched(node);
                let attr = Self::marked(self.attr(&st, node, mode, rdev), watched, &st);
                Ok((
                    self.openable(attr, self.node_free(node)),
                    Self::attr_valid(watched, &st),
                ))
            }
            (None, None) => Err(Errno::ESTALE),
        }
    }

    pub(super) fn w_write(
        &self,
        node: u64,
        fh: u64,
        offset: u64,
        data: IoVecs<'_>,
        kill: bool,
        cache: bool,
    ) -> Result<usize> {
        let handle = self.handle(node, fh)?;
        if handle.dir.is_some() {
            return Err(Errno::EISDIR);
        }
        let Some(dirty) = &handle.dirty else {
            return Err(Errno::EBADF);
        };
        let fd = handle.fd.as_raw_fd();
        if kill {
            kill_suidgid(fd);
        }
        let gen = self.caps_gen.load(Ordering::Relaxed) + 1;
        if handle.caps_seen.swap(gen, Ordering::Relaxed) != gen {
            drop_caps(fd);
        }
        dirty.store(true, Ordering::Relaxed);
        let mut offset = if handle.append && !cache {
            fstat(fd)?.st_size
        } else {
            i64::try_from(offset).map_err(|_| Errno::EINVAL)?
        };
        let mut total = 0usize;
        let mut pending: Vec<libc::iovec> = Vec::new();
        let mut rest: &[libc::iovec] = data.as_slice();
        loop {
            if pending.is_empty() {
                if rest.is_empty() {
                    break;
                }
                let (batch, later) = rest.split_at(rest.len().min(IOV_BATCH));
                rest = later;
                pending = batch.iter().filter(|b| b.iov_len > 0).copied().collect();
                continue;
            }
            let n = unsafe { libc::pwritev(fd, pending.as_ptr(), pending.len() as i32, offset) };
            if n < 0 {
                if std::io::Error::last_os_error().kind() == std::io::ErrorKind::Interrupted {
                    continue;
                }
                if total > 0 {
                    break;
                }
                return Err(Errno::last());
            }
            if n == 0 {
                break;
            }
            let n = n as usize;
            total += n;
            offset = offset.checked_add(n as i64).ok_or(Errno::EINVAL)?;
            pending = skip_iov(&pending, n);
        }
        Ok(total)
    }

    pub(super) fn w_fallocate(
        &self,
        node: u64,
        fh: u64,
        mode: u32,
        offset: u64,
        len: u64,
    ) -> Result<()> {
        self.writable()?;
        let handle = self.handle(node, fh)?;
        if handle.dir.is_some() {
            return Err(Errno::EISDIR);
        }
        let fd = handle.fd.as_raw_fd();
        let (offset, len) = (
            i64::try_from(offset).map_err(|_| Errno::EINVAL)?,
            i64::try_from(len).map_err(|_| Errno::EINVAL)?,
        );
        let end = offset.checked_add(len).ok_or(Errno(27))?;
        if len == 0 {
            return Err(Errno::EINVAL);
        }
        kill_suidgid(fd);
        drop_caps(fd);
        let st = fstat(fd)?;
        let touched = || {
            if let Some(d) = &handle.dirty {
                d.store(true, Ordering::Relaxed);
            }
        };
        match mode {
            0 | L_FALLOC_KEEP_SIZE => {
                let allocated = st.st_blocks.max(0) * 512;
                if end > allocated {
                    let mut store = libc::fstore_t {
                        fst_flags: libc::F_ALLOCATEALL,
                        fst_posmode: libc::F_PEOFPOSMODE,
                        fst_offset: 0,
                        fst_length: end - allocated,
                        fst_bytesalloc: 0,
                    };
                    if unsafe { libc::fcntl(fd, libc::F_PREALLOCATE, &mut store) } == -1 {
                        let e = Errno::last();
                        if e == Errno(28) {
                            return Err(e);
                        }
                    }
                }
                if mode == 0 && end > st.st_size {
                    check(unsafe { libc::ftruncate(fd, end) })?;
                    touched();
                }
                Ok(())
            }
            m if m == L_FALLOC_PUNCH_HOLE | L_FALLOC_KEEP_SIZE => {
                let end = end.min(st.st_size);
                if offset >= end {
                    return Ok(());
                }
                let block = (st.st_blksize as i64).max(512);
                let first = ((offset + block - 1) / block * block).min(end);
                let last = (end / block * block).max(first);
                if last > first {
                    let hole = libc::fpunchhole_t {
                        fp_flags: 0,
                        reserved: 0,
                        fp_offset: first,
                        fp_length: last - first,
                    };
                    if unsafe { libc::fcntl(fd, libc::F_PUNCHHOLE, &hole) } == -1 {
                        return Err(Errno::EOPNOTSUPP);
                    }
                }
                let zeros = vec![0u8; block as usize];
                for (from, to) in [(offset, first), (last, end)] {
                    let mut at = from;
                    while at < to {
                        let n = ((to - at) as usize).min(zeros.len());
                        let w = unsafe { libc::pwrite(fd, zeros.as_ptr().cast(), n, at) };
                        if w <= 0 {
                            return Err(Errno::last());
                        }
                        at += w as i64;
                    }
                }
                touched();
                Ok(())
            }
            _ => Err(Errno::EOPNOTSUPP),
        }
    }

    pub(super) fn w_lseek(&self, node: u64, fh: u64, offset: u64, whence: u32) -> Result<u64> {
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
        let whence = match whence {
            L_SEEK_DATA => libc::SEEK_DATA,
            L_SEEK_HOLE => libc::SEEK_HOLE,
            _ => return Err(Errno::EINVAL),
        };
        let offset = i64::try_from(offset).map_err(|_| Errno(6))?;
        let at = unsafe { libc::lseek(fd, offset, whence) };
        if at < 0 {
            return Err(Errno::last());
        }
        Ok(at as u64)
    }

    fn x_fd(&self, node: u64) -> Result<OwnedFd> {
        let (path, kind, dev) = self.node_path(node)?;
        if !path.starts_with(b"/.vol/") || !self.vol(dev).is_some_and(|v| v.xattrs) {
            return Err(Errno::EOPNOTSUPP);
        }
        let on_mac = lstat(&path).map_err(|e| match e {
            Errno::ENOENT => Errno::ESTALE,
            e => e,
        })?;
        if !matches!(on_mac.st_mode as u32 & S_IFMT, S_IFREG | S_IFDIR | S_IFLNK) {
            return Err(Errno::EOPNOTSUPP);
        }
        let _ = kind;
        let fd = open(&path, libc::O_EVTONLY | libc::O_SYMLINK | libc::O_NONBLOCK)?;
        let st = fstat(fd.as_raw_fd())?;
        if st.st_dev != on_mac.st_dev || st.st_ino != on_mac.st_ino || self.is_denied(&st) {
            return Err(Errno::ESTALE);
        }
        Ok(fd)
    }

    fn x_none(&self, node: u64) -> bool {
        self.node_path(node).is_ok_and(|(path, _, dev)| {
            path.starts_with(b"/.vol/") && self.vol(dev).is_some_and(|v| v.xattrs)
        })
    }

    fn x_name(name: &[u8]) -> Result<Option<CString>> {
        if name.is_empty() {
            return Err(Errno::EINVAL);
        }
        if name.len() > XATTR_NAME_MAX {
            return Err(Errno::ERANGE);
        }
        if name.starts_with(b"com.apple.") || name == NODE_KEY {
            return Ok(None);
        }
        Ok(Some(cstr(name)?))
    }

    pub(super) fn x_get(&self, node: u64, name: &[u8], size: u32) -> Result<Xattr> {
        let Some(name) = Self::x_name(name)? else {
            return Err(Errno::ENODATA);
        };
        let fd = match self.x_fd(node) {
            Ok(fd) => fd,
            Err(Errno::EOPNOTSUPP) if self.x_none(node) => return Err(Errno::ENODATA),
            Err(e) => return Err(e),
        };
        let get = |buf: &mut [u8]| {
            let n = unsafe {
                libc::fgetxattr(
                    fd.as_raw_fd(),
                    name.as_ptr(),
                    if buf.is_empty() {
                        std::ptr::null_mut()
                    } else {
                        buf.as_mut_ptr().cast()
                    },
                    buf.len(),
                    0,
                    0,
                )
            };
            if n < 0 {
                Err(Errno::last())
            } else {
                Ok(n as usize)
            }
        };
        if size == 0 {
            let n = get(&mut [])?;
            return if n > MAX_XATTR {
                Err(E2BIG)
            } else {
                Ok(Xattr::Size(n as u32))
            };
        }
        let mut buf = vec![0u8; (size as usize).min(MAX_XATTR)];
        let n = get(&mut buf)?;
        buf.truncate(n);
        Ok(Xattr::Data(buf))
    }

    pub(super) fn x_list(&self, node: u64, size: u32) -> Result<Xattr> {
        let fd = match self.x_fd(node) {
            Ok(fd) => fd,
            Err(Errno::EOPNOTSUPP) => {
                return Ok(if size == 0 {
                    Xattr::Size(0)
                } else {
                    Xattr::Data(Vec::new())
                })
            }
            Err(e) => return Err(e),
        };
        let list = |buf: &mut [u8]| {
            let n = unsafe {
                libc::flistxattr(
                    fd.as_raw_fd(),
                    if buf.is_empty() {
                        std::ptr::null_mut()
                    } else {
                        buf.as_mut_ptr().cast()
                    },
                    buf.len(),
                    0,
                )
            };
            if n < 0 {
                Err(Errno::last())
            } else {
                Ok(n as usize)
            }
        };
        let need = list(&mut [])?;
        if need > 1 << 20 {
            return Err(E2BIG);
        }
        let mut buf = vec![0u8; need + 256];
        let n = list(&mut buf)?;
        buf.truncate(n);
        let mut out = Vec::with_capacity(n);
        for name in buf.split(|b| *b == 0).filter(|n| !n.is_empty()) {
            if name.starts_with(b"com.apple.") || name == NODE_KEY {
                continue;
            }
            out.extend_from_slice(name);
            out.push(0);
        }
        if size == 0 {
            Ok(Xattr::Size(out.len().min(MAX_XATTR) as u32))
        } else if out.len() > size as usize {
            Err(Errno::ERANGE)
        } else {
            Ok(Xattr::Data(out))
        }
    }

    pub(super) fn x_set(&self, node: u64, name: &[u8], value: &[u8], flags: u32) -> Result<()> {
        self.writable()?;
        let fd = self.x_fd(node)?;
        let Some(cname) = Self::x_name(name)? else {
            return Err(Errno::EOPNOTSUPP);
        };
        let mut mac = 0;
        if flags & L_XATTR_CREATE != 0 {
            mac |= libc::XATTR_CREATE;
        }
        if flags & L_XATTR_REPLACE != 0 {
            mac |= libc::XATTR_REPLACE;
        }
        if name == CAPABILITY {
            self.caps_gen.fetch_add(1, Ordering::Relaxed);
        }
        check(unsafe {
            libc::fsetxattr(
                fd.as_raw_fd(),
                cname.as_ptr(),
                value.as_ptr().cast(),
                value.len(),
                0,
                mac,
            )
        })
    }

    pub(super) fn x_remove(&self, node: u64, name: &[u8]) -> Result<()> {
        self.writable()?;
        let fd = self.x_fd(node)?;
        let Some(name) = Self::x_name(name)? else {
            return Err(Errno::ENODATA);
        };
        check(unsafe { libc::fremovexattr(fd.as_raw_fd(), name.as_ptr(), 0) })
    }
}
