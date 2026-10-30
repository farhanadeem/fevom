use std::collections::{BTreeMap, HashMap};
use std::sync::Mutex;
use std::time::Duration;

use crate::abi::ROOT_ID;
use crate::errno::Errno;
use crate::fs::{Attr, Ctx, DirSink, Entry, Filesystem, Result, SetAttr, Statfs, Xattr};
use crate::iov::IoVecs;
use crate::server::VALID;

const S_IFDIR: u32 = 0o040000;
const S_IFREG: u32 = 0o100000;
const S_IFLNK: u32 = 0o120000;

enum Kind {
    Dir(BTreeMap<Vec<u8>, u64>),
    File(Vec<u8>),
    Symlink(Vec<u8>),
}

struct Inode {
    kind: Kind,
}

#[derive(Default)]
struct State {
    known: HashMap<u64, u64>,
    handles: HashMap<u64, u64>,
    next_handle: u64,
}

pub struct FakeFs {
    inodes: HashMap<u64, Inode>,
    next: u64,
    state: Mutex<State>,
    accept: bool,
    quick: bool,
}

impl Default for FakeFs {
    fn default() -> Self {
        Self::new()
    }
}

impl FakeFs {
    pub fn new() -> Self {
        let mut inodes = HashMap::new();
        inodes.insert(
            ROOT_ID,
            Inode {
                kind: Kind::Dir(BTreeMap::new()),
            },
        );
        Self {
            inodes,
            next: ROOT_ID + 1,
            state: Mutex::new(State::default()),
            accept: false,
            quick: false,
        }
    }

    pub fn quick(mut self) -> Self {
        self.quick = true;
        self
    }

    pub fn accepting(mut self) -> Self {
        self.accept = true;
        self
    }

    fn accepts(&self) -> Result<()> {
        if self.accept {
            Ok(())
        } else {
            Err(Errno::EROFS)
        }
    }

    fn any_file(&self) -> Result<Entry> {
        let id = self
            .inodes
            .iter()
            .filter(|(_, i)| matches!(i.kind, Kind::File(_)))
            .map(|(id, _)| *id)
            .min()
            .ok_or(Errno::EIO)?;
        Ok(self.remember(id))
    }

    fn add(&mut self, path: &[u8], kind: Kind) {
        let mut dir = ROOT_ID;
        let mut parts = path.split(|b| *b == b'/').peekable();
        while let Some(part) = parts.next() {
            let last = parts.peek().is_none();
            let Kind::Dir(children) = &self.inodes[&dir].kind else {
                panic!("not a directory on the way to {path:?}");
            };
            dir = match children.get(part) {
                Some(id) if !last => *id,
                Some(_) => panic!("{path:?} exists"),
                None if last => {
                    let id = self.next;
                    self.next += 1;
                    let Kind::Dir(children) = &mut self.inodes.get_mut(&dir).unwrap().kind else {
                        unreachable!()
                    };
                    children.insert(part.to_vec(), id);
                    self.inodes.insert(id, Inode { kind });
                    return;
                }
                None => panic!("no parent for {path:?}"),
            };
        }
    }

    pub fn add_dir(&mut self, path: &[u8]) {
        self.add(path, Kind::Dir(BTreeMap::new()));
    }

    pub fn add_file(&mut self, path: &[u8], data: &[u8]) {
        self.add(path, Kind::File(data.to_vec()));
    }

    pub fn add_symlink(&mut self, path: &[u8], target: &[u8]) {
        self.add(path, Kind::Symlink(target.to_vec()));
    }

    fn attr(&self, id: u64) -> Attr {
        let (mode, size) = match &self.inodes[&id].kind {
            Kind::Dir(c) => (S_IFDIR | 0o755, c.len() as u64),
            Kind::File(d) => (S_IFREG | 0o644, d.len() as u64),
            Kind::Symlink(t) => (S_IFLNK | 0o777, t.len() as u64),
        };
        Attr {
            ino: id,
            size,
            blocks: size.div_ceil(512),
            mode,
            nlink: 1,
            blksize: 4096,
            ..Attr::default()
        }
    }

    fn known(&self, id: u64) -> Result<&Inode> {
        if id != ROOT_ID && !self.state.lock().unwrap().known.contains_key(&id) {
            return Err(Errno::ESTALE);
        }
        self.inodes.get(&id).ok_or(Errno::ESTALE)
    }

    fn remember(&self, id: u64) -> Entry {
        *self.state.lock().unwrap().known.entry(id).or_insert(0) += 1;
        Entry {
            nodeid: id,
            generation: 0,
            attr: self.attr(id),
            entry_valid: VALID,
            attr_valid: VALID,
        }
    }

    fn open_handle(&self, node: u64) -> u64 {
        let mut s = self.state.lock().unwrap();
        s.next_handle += 1;
        let fh = s.next_handle;
        s.handles.insert(fh, node);
        fh
    }

    fn handle(&self, node: u64, fh: u64) -> Result<()> {
        match self.state.lock().unwrap().handles.get(&fh) {
            Some(n) if *n == node => Ok(()),
            _ => Err(Errno::EBADF),
        }
    }
}

impl Filesystem for FakeFs {
    fn quick(&self, _node: u64, _need: crate::fs::Need) -> bool {
        self.quick
    }

    fn lookup(&self, _ctx: &Ctx, parent: u64, name: &[u8]) -> Result<Entry> {
        let Kind::Dir(children) = &self.known(parent)?.kind else {
            return Err(Errno::ENOTDIR);
        };
        let id = *children.get(name).ok_or(Errno::ENOENT)?;
        Ok(self.remember(id))
    }

    fn forget(&self, node: u64, nlookup: u64) {
        let mut s = self.state.lock().unwrap();
        if let Some(count) = s.known.get_mut(&node) {
            *count = count.saturating_sub(nlookup);
            if *count == 0 {
                s.known.remove(&node);
            }
        }
    }

    fn getattr(&self, _ctx: &Ctx, node: u64, _fh: Option<u64>) -> Result<(Attr, Duration)> {
        self.known(node)?;
        Ok((self.attr(node), VALID))
    }

    fn readlink(&self, _ctx: &Ctx, node: u64) -> Result<Vec<u8>> {
        match &self.known(node)?.kind {
            Kind::Symlink(t) => Ok(t.clone()),
            _ => Err(Errno::EINVAL),
        }
    }

    fn open(&self, _ctx: &Ctx, node: u64, flags: u32, _kill: bool) -> Result<(u64, u32)> {
        match &self.known(node)?.kind {
            Kind::File(_) if flags & 3 == 0 || self.accept => Ok((self.open_handle(node), 0)),
            Kind::File(_) => Err(Errno::EROFS),
            Kind::Dir(_) => Err(Errno::EISDIR),
            Kind::Symlink(_) => Err(Errno::ELOOP),
        }
    }

    fn read(&self, _ctx: &Ctx, node: u64, fh: u64, offset: u64, data: IoVecs<'_>) -> Result<usize> {
        self.handle(node, fh)?;
        let Kind::File(content) = &self.inodes[&node].kind else {
            return Err(Errno::EISDIR);
        };
        let start = (offset.min(content.len() as u64)) as usize;
        let n = (content.len() - start).min(data.len());
        if !data.write_at(0, &content[start..start + n]) {
            return Err(Errno::EIO);
        }
        Ok(n)
    }

    fn release(&self, _node: u64, fh: u64) {
        self.state.lock().unwrap().handles.remove(&fh);
    }

    fn opendir(&self, _ctx: &Ctx, node: u64, _flags: u32) -> Result<(u64, u32)> {
        match &self.known(node)?.kind {
            Kind::Dir(_) => Ok((self.open_handle(node), 0)),
            _ => Err(Errno::ENOTDIR),
        }
    }

    fn readdir(
        &self,
        _ctx: &Ctx,
        node: u64,
        fh: u64,
        offset: u64,
        sink: &mut DirSink,
    ) -> Result<()> {
        self.handle(node, fh)?;
        let Kind::Dir(children) = &self.inodes[&node].kind else {
            return Err(Errno::ENOTDIR);
        };
        let skip = usize::try_from(offset).unwrap_or(usize::MAX);
        for (i, (name, id)) in children.iter().enumerate().skip(skip) {
            if !sink.fits(name.len()) {
                break;
            }
            let kind = self.attr(*id).mode >> 12;
            let entry = sink.plus().then(|| self.remember(*id));
            sink.add(name, *id, i as u64 + 1, kind, entry.as_ref());
        }
        Ok(())
    }

    fn releasedir(&self, _node: u64, fh: u64) {
        self.state.lock().unwrap().handles.remove(&fh);
    }

    fn statfs(&self, _ctx: &Ctx, node: u64) -> Result<Statfs> {
        self.known(node)?;
        Ok(Statfs {
            blocks: 1 << 20,
            bfree: 1 << 19,
            bavail: 1 << 19,
            files: self.inodes.len() as u64,
            ffree: 1 << 20,
            bsize: 4096,
            namelen: 255,
            frsize: 4096,
        })
    }

    fn destroy(&self) {
        *self.state.lock().unwrap() = State::default();
    }

    fn setattr(&self, _ctx: &Ctx, node: u64, _set: &SetAttr) -> Result<(Attr, Duration)> {
        self.accepts()?;
        self.known(node)?;
        Ok((self.attr(node), VALID))
    }

    fn mknod(&self, _: &Ctx, parent: u64, _: &[u8], _: u32, _: u32, _: u32) -> Result<Entry> {
        self.accepts()?;
        self.known(parent)?;
        self.any_file()
    }

    fn mkdir(&self, _: &Ctx, parent: u64, _: &[u8], _: u32, _: u32) -> Result<Entry> {
        self.accepts()?;
        self.known(parent)?;
        Ok(self.remember(ROOT_ID))
    }

    fn symlink(&self, _: &Ctx, parent: u64, _: &[u8], _: &[u8]) -> Result<Entry> {
        self.accepts()?;
        self.known(parent)?;
        self.any_file()
    }

    fn link(&self, _: &Ctx, node: u64, newparent: u64, _: &[u8]) -> Result<Entry> {
        self.accepts()?;
        self.known(newparent)?;
        self.known(node)?;
        Ok(self.remember(node))
    }

    fn unlink(&self, _: &Ctx, parent: u64, _: &[u8]) -> Result<()> {
        self.accepts()?;
        self.known(parent).map(|_| ())
    }

    fn rmdir(&self, _: &Ctx, parent: u64, _: &[u8]) -> Result<()> {
        self.accepts()?;
        self.known(parent).map(|_| ())
    }

    fn rename(&self, _: &Ctx, parent: u64, _: &[u8], new: u64, _: &[u8], _: u32) -> Result<()> {
        self.accepts()?;
        self.known(parent)?;
        self.known(new).map(|_| ())
    }

    fn create(
        &self,
        _: &Ctx,
        parent: u64,
        _: &[u8],
        _: u32,
        _: u32,
        _: u32,
    ) -> Result<(Entry, u64, u32)> {
        self.accepts()?;
        self.known(parent)?;
        let entry = self.any_file()?;
        Ok((entry, self.open_handle(entry.nodeid), 0))
    }

    fn write(
        &self,
        _: &Ctx,
        node: u64,
        fh: u64,
        _: u64,
        data: IoVecs<'_>,
        _: bool,
        _: bool,
    ) -> Result<usize> {
        self.accepts()?;
        self.handle(node, fh)?;
        let mut sink = vec![0u8; data.len().min(1 << 20)];
        if !data.read_at(0, &mut sink) {
            return Err(Errno::EIO);
        }
        Ok(sink.len())
    }

    fn fallocate(&self, _: &Ctx, node: u64, fh: u64, _: u32, _: u64, _: u64) -> Result<()> {
        self.accepts()?;
        self.handle(node, fh)
    }

    fn lseek(&self, _: &Ctx, node: u64, fh: u64, offset: u64, _: u32) -> Result<u64> {
        self.handle(node, fh)?;
        Ok(offset)
    }

    fn setxattr(&self, _: &Ctx, node: u64, _: &[u8], _: &[u8], _: u32) -> Result<()> {
        self.accepts()?;
        self.known(node).map(|_| ())
    }

    fn getxattr(&self, _: &Ctx, node: u64, name: &[u8], size: u32) -> Result<Xattr> {
        self.known(node)?;
        Ok(if size == 0 {
            Xattr::Size(name.len() as u32)
        } else {
            Xattr::Data(name.to_vec())
        })
    }

    fn listxattr(&self, _: &Ctx, node: u64, size: u32) -> Result<Xattr> {
        self.known(node)?;
        Ok(if size == 0 {
            Xattr::Size(7)
        } else {
            Xattr::Data(b"user.a\0".to_vec())
        })
    }

    fn removexattr(&self, _: &Ctx, node: u64, _: &[u8]) -> Result<()> {
        self.accepts()?;
        self.known(node).map(|_| ())
    }
}
