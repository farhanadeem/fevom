use std::time::Duration;

use crate::errno::Errno;
use crate::iov::IoVecs;

pub type Result<T> = std::result::Result<T, Errno>;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct Ctx {
    pub uid: u32,
    pub gid: u32,
    pub pid: u32,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct Attr {
    pub ino: u64,
    pub size: u64,
    pub blocks: u64,
    pub atime: (u64, u32),
    pub mtime: (u64, u32),
    pub ctime: (u64, u32),
    pub mode: u32,
    pub nlink: u32,
    pub uid: u32,
    pub gid: u32,
    pub rdev: u32,
    pub blksize: u32,
    pub flags: u32,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Entry {
    pub nodeid: u64,
    pub generation: u64,
    pub attr: Attr,
    pub entry_valid: Duration,
    pub attr_valid: Duration,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct Statfs {
    pub blocks: u64,
    pub bfree: u64,
    pub bavail: u64,
    pub files: u64,
    pub ffree: u64,
    pub bsize: u32,
    pub namelen: u32,
    pub frsize: u32,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct SetAttr {
    pub mode: Option<u32>,
    pub uid: Option<u32>,
    pub gid: Option<u32>,
    pub size: Option<u64>,
    pub atime: Option<SetTime>,
    pub mtime: Option<SetTime>,
    pub fh: Option<u64>,
    pub kill_suidgid: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SetTime {
    Now,
    At(u64, u32),
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Xattr {
    Size(u32),
    Data(Vec<u8>),
}

pub struct DirSink {
    pub(crate) buf: Vec<u8>,
    pub(crate) cap: usize,
    pub(crate) plus: bool,
}

impl DirSink {
    pub fn plus(&self) -> bool {
        self.plus
    }

    pub fn fits(&self, namelen: usize) -> bool {
        let fixed = if self.plus {
            crate::abi::ENTRY_OUT_LEN
        } else {
            0
        };
        self.buf.len() + fixed + crate::abi::dirent_len(namelen) <= self.cap
    }

    pub fn add(
        &mut self,
        name: &[u8],
        ino: u64,
        off: u64,
        kind: u32,
        entry: Option<&Entry>,
    ) -> bool {
        if !self.fits(name.len()) {
            return false;
        }
        let mut w = crate::abi::Writer(std::mem::take(&mut self.buf));
        if self.plus {
            match entry {
                Some(e) => crate::server::put_entry(&mut w, e),
                None => {
                    w.zeros(crate::abi::ENTRY_OUT_LEN);
                }
            }
        }
        let start = w.0.len();
        w.u64(ino).u64(off).u32(name.len() as u32).u32(kind);
        w.0.extend_from_slice(name);
        let padded = start + crate::abi::dirent_len(name.len());
        w.0.resize(padded, 0);
        self.buf = w.0;
        true
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Need {
    Meta,
    Lookup,
    Names,
    File,
    Handle(u64),
}

pub trait Filesystem: Send + Sync {
    fn lookup(&self, ctx: &Ctx, parent: u64, name: &[u8]) -> Result<Entry>;
    fn forget(&self, node: u64, nlookup: u64);
    fn getattr(&self, ctx: &Ctx, node: u64, fh: Option<u64>) -> Result<(Attr, Duration)>;
    fn readlink(&self, ctx: &Ctx, node: u64) -> Result<Vec<u8>>;
    fn ioctl(&self, _node: u64, _fh: u64, _cmd: u32, _out_size: u32) -> Result<(i32, Vec<u8>)> {
        Err(Errno::ENOTTY)
    }

    fn open(&self, ctx: &Ctx, node: u64, flags: u32, kill_suidgid: bool) -> Result<(u64, u32)>;
    fn read(&self, ctx: &Ctx, node: u64, fh: u64, offset: u64, data: IoVecs<'_>) -> Result<usize>;
    fn release(&self, node: u64, fh: u64);
    fn opendir(&self, ctx: &Ctx, node: u64, flags: u32) -> Result<(u64, u32)>;
    fn readdir(&self, ctx: &Ctx, node: u64, fh: u64, offset: u64, sink: &mut DirSink)
        -> Result<()>;
    fn releasedir(&self, node: u64, fh: u64);
    fn statfs(&self, ctx: &Ctx, node: u64) -> Result<Statfs>;
    fn destroy(&self);

    fn setattr(&self, _ctx: &Ctx, _node: u64, _set: &SetAttr) -> Result<(Attr, Duration)> {
        Err(Errno::EROFS)
    }
    fn mknod(
        &self,
        _ctx: &Ctx,
        _parent: u64,
        _name: &[u8],
        _mode: u32,
        _rdev: u32,
        _umask: u32,
    ) -> Result<Entry> {
        Err(Errno::EROFS)
    }
    fn mkdir(
        &self,
        _ctx: &Ctx,
        _parent: u64,
        _name: &[u8],
        _mode: u32,
        _umask: u32,
    ) -> Result<Entry> {
        Err(Errno::EROFS)
    }
    fn symlink(&self, _ctx: &Ctx, _parent: u64, _name: &[u8], _target: &[u8]) -> Result<Entry> {
        Err(Errno::EROFS)
    }
    fn link(&self, _ctx: &Ctx, _node: u64, _newparent: u64, _newname: &[u8]) -> Result<Entry> {
        Err(Errno::EROFS)
    }
    fn unlink(&self, _ctx: &Ctx, _parent: u64, _name: &[u8]) -> Result<()> {
        Err(Errno::EROFS)
    }
    fn rmdir(&self, _ctx: &Ctx, _parent: u64, _name: &[u8]) -> Result<()> {
        Err(Errno::EROFS)
    }
    fn rename(
        &self,
        _ctx: &Ctx,
        _parent: u64,
        _name: &[u8],
        _newparent: u64,
        _newname: &[u8],
        _flags: u32,
    ) -> Result<()> {
        Err(Errno::EROFS)
    }
    fn create(
        &self,
        _ctx: &Ctx,
        _parent: u64,
        _name: &[u8],
        _mode: u32,
        _umask: u32,
        _flags: u32,
    ) -> Result<(Entry, u64, u32)> {
        Err(Errno::EROFS)
    }
    #[allow(clippy::too_many_arguments)]
    fn write(
        &self,
        _ctx: &Ctx,
        _node: u64,
        _fh: u64,
        _offset: u64,
        _data: IoVecs<'_>,
        _kill_suidgid: bool,
        _cache: bool,
    ) -> Result<usize> {
        Err(Errno::EROFS)
    }
    fn fsync(&self, _ctx: &Ctx, _node: u64, _fh: u64, _datasync: bool) -> Result<()> {
        Ok(())
    }
    fn syncfs(&self, _ctx: &Ctx, _node: u64) -> Result<()> {
        Ok(())
    }
    fn fallocate(
        &self,
        _ctx: &Ctx,
        _node: u64,
        _fh: u64,
        _mode: u32,
        _offset: u64,
        _len: u64,
    ) -> Result<()> {
        Err(Errno::EROFS)
    }
    fn lseek(&self, _ctx: &Ctx, _node: u64, _fh: u64, _offset: u64, _whence: u32) -> Result<u64> {
        Err(Errno::EOPNOTSUPP)
    }
    fn setxattr(
        &self,
        _ctx: &Ctx,
        _node: u64,
        _name: &[u8],
        _value: &[u8],
        _flags: u32,
    ) -> Result<()> {
        Err(Errno::EROFS)
    }
    fn getxattr(&self, _ctx: &Ctx, _node: u64, _name: &[u8], _size: u32) -> Result<Xattr> {
        Err(Errno::EOPNOTSUPP)
    }
    fn listxattr(&self, _ctx: &Ctx, _node: u64, _size: u32) -> Result<Xattr> {
        Err(Errno::EOPNOTSUPP)
    }
    fn removexattr(&self, _ctx: &Ctx, _node: u64, _name: &[u8]) -> Result<()> {
        Err(Errno::EROFS)
    }

    fn extra_init_flags(&self) -> u64 {
        0
    }

    fn mounted(&self, _flags: u64) {}

    fn quick(&self, _node: u64, _need: Need) -> bool {
        false
    }
}
