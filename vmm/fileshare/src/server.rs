use std::sync::Mutex;
use std::time::Duration;

use crate::abi::{self, init, op, InHeader, Reader, Writer};
use crate::errno::Errno;
use crate::fs::{Attr, Ctx, DirSink, Entry, Filesystem, Need, SetAttr, SetTime, Xattr};
use crate::iov::IoVecs;

pub const MAX_HEAD: usize = 8192 + 256;
pub const MAX_IO: u32 = 1 << 20;
pub const MAX_PAGES: u16 = (MAX_IO / 4096) as u16;
const MAX_BATCH_FORGET: usize = (MAX_HEAD - abi::IN_HEADER_LEN - 8) / 16;

pub fn head_len(opcode: u32, total: usize) -> usize {
    let limit = match opcode {
        op::WRITE => abi::IN_HEADER_LEN + abi::WRITE_IN_LEN,
        _ => MAX_HEAD,
    };
    total.min(limit)
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct InitInfo {
    pub major: u32,
    pub minor: u32,
    pub flags_in: u64,
    pub flags_out: u64,
    pub max_readahead: u32,
    pub max_write: u32,
    pub max_pages: u16,
}

pub struct Request<'a> {
    pub head: &'a [u8],
    pub payload: IoVecs<'a>,
    pub reply: IoVecs<'a>,
}

pub const MAX_XATTR: usize = 64 << 10;

pub struct Server {
    fs: std::sync::Arc<dyn Filesystem>,
    init: Mutex<Option<InitInfo>>,
}

fn put_attr(w: &mut Writer, a: &Attr) {
    w.u64(a.ino)
        .u64(a.size)
        .u64(a.blocks)
        .u64(a.atime.0)
        .u64(a.mtime.0)
        .u64(a.ctime.0)
        .u32(a.atime.1)
        .u32(a.mtime.1)
        .u32(a.ctime.1)
        .u32(a.mode)
        .u32(a.nlink)
        .u32(a.uid)
        .u32(a.gid)
        .u32(a.rdev)
        .u32(a.blksize)
        .u32(a.flags);
}

pub(crate) fn put_entry(w: &mut Writer, e: &Entry) {
    w.u64(e.nodeid)
        .u64(e.generation)
        .u64(e.entry_valid.as_secs())
        .u64(e.attr_valid.as_secs())
        .u32(e.entry_valid.subsec_nanos())
        .u32(e.attr_valid.subsec_nanos());
    put_attr(w, &e.attr);
}

type Reply = Result<Vec<u8>, Errno>;

fn entry_out(e: &Entry) -> Vec<u8> {
    let mut w = Writer::with_capacity(abi::ENTRY_OUT_LEN);
    put_entry(&mut w, e);
    w.0
}

fn attr_out(attr: &Attr, valid: Duration) -> Vec<u8> {
    let mut w = Writer::with_capacity(abi::ATTR_OUT_LEN);
    w.u64(valid.as_secs()).u32(valid.subsec_nanos()).u32(0);
    put_attr(&mut w, attr);
    w.0
}

fn two_names(b: &[u8]) -> Option<(&[u8], &[u8])> {
    let end = b.iter().position(|c| *c == 0)?;
    let second = abi::name(&b[end + 1..])?;
    (end > 0).then_some((&b[..end], second))
}

impl Server {
    pub fn new(fs: Box<dyn Filesystem>) -> Self {
        Self::shared(fs.into())
    }

    pub fn shared(fs: std::sync::Arc<dyn Filesystem>) -> Self {
        Self {
            fs,
            init: Mutex::new(None),
        }
    }

    pub fn negotiated(&self) -> Option<InitInfo> {
        *self.init.lock().unwrap()
    }

    pub fn reset(&self) {
        *self.init.lock().unwrap() = None;
        self.fs.destroy();
    }

    pub fn quick(&self, head: &[u8]) -> bool {
        let Some(h) = InHeader::parse(head) else {
            return false;
        };
        let mut r = Reader::new(&head[abi::IN_HEADER_LEN..]);
        let fs = &self.fs;
        match h.opcode {
            op::GETATTR => fs.quick(h.nodeid, Need::Meta),
            op::LOOKUP => fs.quick(h.nodeid, Need::Lookup),
            op::CREATE | op::MKDIR | op::MKNOD | op::SYMLINK | op::UNLINK | op::RMDIR => {
                fs.quick(h.nodeid, Need::Names)
            }
            op::RENAME | op::RENAME2 => {
                fs.quick(h.nodeid, Need::Names)
                    && r.u64().is_some_and(|newdir| fs.quick(newdir, Need::Names))
            }
            op::LINK => {
                fs.quick(h.nodeid, Need::Names)
                    && r.u64().is_some_and(|old| fs.quick(old, Need::File))
            }
            op::OPEN | op::OPENDIR | op::READLINK | op::GETXATTR | op::LISTXATTR => {
                fs.quick(h.nodeid, Need::File)
            }
            op::SETATTR => match (r.u32(), r.u32(), r.u64()) {
                (Some(valid), Some(_), Some(fh)) if valid & abi::fattr::FH != 0 => {
                    fs.quick(h.nodeid, Need::Handle(fh))
                }
                (Some(_), Some(_), Some(_)) => fs.quick(h.nodeid, Need::File),
                _ => false,
            },
            op::READ | op::WRITE | op::RELEASE | op::RELEASEDIR | op::LSEEK => r
                .u64()
                .is_some_and(|fh| fs.quick(h.nodeid, Need::Handle(fh))),
            _ => false,
        }
    }

    pub fn handle(&self, req: &Request<'_>) -> usize {
        let Some(h) = InHeader::parse(req.head) else {
            return 0;
        };
        let body = &req.head[abi::IN_HEADER_LEN..];
        let ctx = Ctx {
            uid: h.uid,
            gid: h.gid,
            pid: h.pid,
        };
        let result = match h.opcode {
            op::FORGET => {
                if self.negotiated().is_some() {
                    if let Some(n) = Reader::new(body).u64() {
                        self.fs.forget(h.nodeid, n);
                    }
                }
                return 0;
            }
            op::BATCH_FORGET => {
                if self.negotiated().is_some() {
                    self.batch_forget(body);
                }
                return 0;
            }
            op::INTERRUPT | op::NOTIFY_REPLY => return 0,
            op::INIT => self.init(body),
            _ if self.negotiated().is_none() => Err(Errno::EIO),
            op::READ => return self.read(&ctx, &h, body, req),
            op::WRITE => self.write(&ctx, &h, body, req),
            _ => self.dispatch(&ctx, &h, body, req),
        };
        match result {
            Ok(body) => reply(req, h.unique, &body),
            Err(e) => reply_error(req, h.unique, e),
        }
    }

    fn batch_forget(&self, body: &[u8]) {
        let mut r = Reader::new(body);
        let Some(count) = r.u32() else { return };
        let _ = r.u32();
        for _ in 0..(count as usize).min(MAX_BATCH_FORGET) {
            let (Some(node), Some(n)) = (r.u64(), r.u64()) else {
                return;
            };
            self.fs.forget(node, n);
        }
    }

    fn init(&self, body: &[u8]) -> Reply {
        let mut r = Reader::new(body);
        let (major, minor) = (r.u32().ok_or(Errno::EINVAL)?, r.u32().ok_or(Errno::EINVAL)?);
        if major > abi::MAJOR {
            let mut w = Writer::with_capacity(abi::INIT_OUT_LEN);
            w.u32(abi::MAJOR)
                .u32(abi::MINOR)
                .zeros(abi::INIT_OUT_LEN - 8);
            return Ok(w.0);
        }
        if major < abi::MAJOR || minor < abi::MIN_MINOR {
            return Err(Errno::EPROTO);
        }
        let max_readahead = r.u32().ok_or(Errno::EINVAL)?;
        let mut flags_in = r.u32().ok_or(Errno::EINVAL)? as u64;
        if flags_in & init::INIT_EXT != 0 {
            flags_in |= (r.u32().ok_or(Errno::EINVAL)? as u64) << 32;
        }
        let flags_out = flags_in & (init::WANTED | self.fs.extra_init_flags());
        let max_pages = if flags_out & init::MAX_PAGES != 0 {
            MAX_PAGES
        } else {
            0
        };
        let info = InitInfo {
            major,
            minor,
            flags_in,
            flags_out,
            max_readahead,
            max_write: MAX_IO,
            max_pages,
        };
        if self.init.lock().unwrap().replace(info).is_some() {
            self.fs.destroy();
        }
        self.fs.mounted(flags_out);

        let mut w = Writer::with_capacity(abi::INIT_OUT_LEN);
        w.u32(abi::MAJOR)
            .u32(abi::MINOR)
            .u32(max_readahead)
            .u32(flags_out as u32)
            .u16(0)
            .u16(0)
            .u32(MAX_IO)
            .u32(1)
            .u16(max_pages)
            .u16(0)
            .u32((flags_out >> 32) as u32)
            .zeros(abi::INIT_OUT_LEN - 36);
        Ok(w.0)
    }

    fn dispatch(&self, ctx: &Ctx, h: &InHeader, body: &[u8], req: &Request<'_>) -> Reply {
        let fs = &*self.fs;
        let mut r = Reader::new(body);
        let mut u32_ = || r.u32().ok_or(Errno::EINVAL);
        match h.opcode {
            op::LOOKUP => {
                let name = abi::name(body).ok_or(Errno::EINVAL)?;
                let answer = fs.lookup(ctx, h.nodeid, name);
                Ok(entry_out(&answer?))
            }
            op::GETATTR => {
                let flags = u32_()?;
                let _ = u32_()?;
                let fh = r.u64().ok_or(Errno::EINVAL)?;
                let fh = (flags & abi::GETATTR_FH != 0 && fh != 0).then_some(fh);
                let (attr, valid) = fs.getattr(ctx, h.nodeid, fh)?;
                Ok(attr_out(&attr, valid))
            }
            op::READLINK => fs.readlink(ctx, h.nodeid),
            op::OPEN | op::OPENDIR => {
                let flags = u32_()?;
                let (fh, open_flags) = if h.opcode == op::OPEN {
                    let kill = u32_()? & abi::OPEN_KILL_SUIDGID != 0;
                    fs.open(ctx, h.nodeid, flags, kill)?
                } else {
                    fs.opendir(ctx, h.nodeid, flags)?
                };
                let mut w = Writer::with_capacity(abi::OPEN_OUT_LEN);
                w.u64(fh).u32(open_flags).u32(0);
                Ok(w.0)
            }
            op::RELEASE | op::RELEASEDIR => {
                let fh = r.u64().ok_or(Errno::EINVAL)?;
                if h.opcode == op::RELEASE {
                    fs.release(h.nodeid, fh);
                } else {
                    fs.releasedir(h.nodeid, fh);
                }
                Ok(Vec::new())
            }
            op::READDIR | op::READDIRPLUS => {
                let fh = r.u64().ok_or(Errno::EINVAL)?;
                let offset = r.u64().ok_or(Errno::EINVAL)?;
                let size = r.u32().ok_or(Errno::EINVAL)?;
                let cap = (size.min(MAX_IO) as usize)
                    .min(req.reply.len().saturating_sub(abi::OUT_HEADER_LEN));
                let mut sink = DirSink {
                    buf: Vec::new(),
                    cap,
                    plus: h.opcode == op::READDIRPLUS,
                };
                fs.readdir(ctx, h.nodeid, fh, offset, &mut sink)?;
                Ok(sink.buf)
            }
            op::STATFS => {
                let s = fs.statfs(ctx, h.nodeid)?;
                let mut w = Writer::with_capacity(abi::STATFS_OUT_LEN);
                w.u64(s.blocks)
                    .u64(s.bfree)
                    .u64(s.bavail)
                    .u64(s.files)
                    .u64(s.ffree)
                    .u32(s.bsize)
                    .u32(s.namelen)
                    .u32(s.frsize)
                    .zeros(28);
                Ok(w.0)
            }
            op::DESTROY => {
                *self.init.lock().unwrap() = None;
                fs.destroy();
                Ok(Vec::new())
            }
            op::SETATTR => {
                let valid = u32_()?;
                let _ = u32_()?;
                let mut u64_ = || r.u64().ok_or(Errno::EINVAL);
                let (fh, size, _lock) = (u64_()?, u64_()?, u64_()?);
                let (atime, mtime, _ctime) = (u64_()?, u64_()?, u64_()?);
                let mut u32_ = || r.u32().ok_or(Errno::EINVAL);
                let (atimensec, mtimensec, _ctimensec) = (u32_()?, u32_()?, u32_()?);
                let (mode, _, uid, gid) = (u32_()?, u32_()?, u32_()?, u32_()?);
                let has = |bit| valid & bit != 0;
                let time = |set, now, s, ns: u32| {
                    if has(now) {
                        Some(SetTime::Now)
                    } else {
                        has(set).then_some(SetTime::At(s, ns.min(999_999_999)))
                    }
                };
                let set = SetAttr {
                    mode: has(abi::fattr::MODE).then_some(mode),
                    uid: has(abi::fattr::UID).then_some(uid),
                    gid: has(abi::fattr::GID).then_some(gid),
                    size: has(abi::fattr::SIZE).then_some(size),
                    atime: time(abi::fattr::ATIME, abi::fattr::ATIME_NOW, atime, atimensec),
                    mtime: time(abi::fattr::MTIME, abi::fattr::MTIME_NOW, mtime, mtimensec),
                    fh: (has(abi::fattr::FH) && fh != 0).then_some(fh),
                    kill_suidgid: has(abi::fattr::KILL_SUIDGID),
                };
                let (attr, valid) = fs.setattr(ctx, h.nodeid, &set)?;
                Ok(attr_out(&attr, valid))
            }
            op::SYMLINK => {
                let (name, target) = two_names(body).ok_or(Errno::EINVAL)?;
                Ok(entry_out(&fs.symlink(ctx, h.nodeid, name, target)?))
            }
            op::MKNOD => {
                let (mode, rdev, umask, _) = (u32_()?, u32_()?, u32_()?, u32_()?);
                let name = abi::name(r.rest()).ok_or(Errno::EINVAL)?;
                Ok(entry_out(
                    &fs.mknod(ctx, h.nodeid, name, mode, rdev, umask)?,
                ))
            }
            op::MKDIR => {
                let (mode, umask) = (u32_()?, u32_()?);
                let name = abi::name(r.rest()).ok_or(Errno::EINVAL)?;
                Ok(entry_out(&fs.mkdir(ctx, h.nodeid, name, mode, umask)?))
            }
            op::UNLINK | op::RMDIR => {
                let name = abi::name(body).ok_or(Errno::EINVAL)?;
                if h.opcode == op::UNLINK {
                    fs.unlink(ctx, h.nodeid, name)?;
                } else {
                    fs.rmdir(ctx, h.nodeid, name)?;
                }
                Ok(Vec::new())
            }
            op::RENAME | op::RENAME2 => {
                let newdir = r.u64().ok_or(Errno::EINVAL)?;
                let flags = if h.opcode == op::RENAME2 {
                    let flags = r.u32().ok_or(Errno::EINVAL)?;
                    r.u32().ok_or(Errno::EINVAL)?;
                    flags
                } else {
                    0
                };
                let (old, new) = two_names(r.rest()).ok_or(Errno::EINVAL)?;
                fs.rename(ctx, h.nodeid, old, newdir, new, flags)?;
                Ok(Vec::new())
            }
            op::LINK => {
                let old = r.u64().ok_or(Errno::EINVAL)?;
                let name = abi::name(r.rest()).ok_or(Errno::EINVAL)?;
                Ok(entry_out(&fs.link(ctx, old, h.nodeid, name)?))
            }
            op::CREATE => {
                let (flags, mode, umask, _open_flags) = (u32_()?, u32_()?, u32_()?, u32_()?);
                let name = abi::name(r.rest()).ok_or(Errno::EINVAL)?;
                let (entry, fh, open_flags) = fs.create(ctx, h.nodeid, name, mode, umask, flags)?;
                let mut w = Writer::with_capacity(abi::ENTRY_OUT_LEN + abi::OPEN_OUT_LEN);
                put_entry(&mut w, &entry);
                w.u64(fh).u32(open_flags).u32(0);
                Ok(w.0)
            }
            op::FSYNC | op::FSYNCDIR => {
                let fh = r.u64().ok_or(Errno::EINVAL)?;
                let datasync = r.u32().ok_or(Errno::EINVAL)? & abi::FSYNC_FDATASYNC != 0;
                fs.fsync(ctx, h.nodeid, fh, datasync)?;
                Ok(Vec::new())
            }
            op::SYNCFS => {
                fs.syncfs(ctx, h.nodeid)?;
                Ok(Vec::new())
            }
            op::FALLOCATE => {
                let mut u64_ = || r.u64().ok_or(Errno::EINVAL);
                let (fh, offset, len) = (u64_()?, u64_()?, u64_()?);
                let mode = r.u32().ok_or(Errno::EINVAL)?;
                fs.fallocate(ctx, h.nodeid, fh, mode, offset, len)?;
                Ok(Vec::new())
            }
            op::LSEEK => {
                let (fh, offset) = (r.u64().ok_or(Errno::EINVAL)?, r.u64().ok_or(Errno::EINVAL)?);
                let whence = r.u32().ok_or(Errno::EINVAL)?;
                let at = fs.lseek(ctx, h.nodeid, fh, offset, whence)?;
                Ok(at.to_le_bytes().to_vec())
            }
            op::SETXATTR => {
                let (size, flags) = (u32_()? as usize, u32_()?);
                let ext = self
                    .negotiated()
                    .is_some_and(|i| i.flags_out & init::SETXATTR_EXT != 0);
                if ext {
                    r.skip(8).ok_or(Errno::EINVAL)?;
                }
                let rest = r.rest();
                let end = rest.iter().position(|b| *b == 0).ok_or(Errno::EINVAL)?;
                let name = &rest[..end];
                if size > MAX_XATTR {
                    return Err(Errno(7));
                }
                let mut value = rest[end + 1..].to_vec();
                if value.len() < size {
                    let have = value.len();
                    value.resize(size, 0);
                    if !req.payload.read_at(0, &mut value[have..]) {
                        return Err(Errno::EINVAL);
                    }
                }
                value.truncate(size);
                fs.setxattr(ctx, h.nodeid, name, &value, flags)?;
                Ok(Vec::new())
            }
            op::GETXATTR | op::LISTXATTR => {
                let size = u32_()?;
                let _ = u32_()?;
                let got = if h.opcode == op::GETXATTR {
                    let name = abi::name(r.rest()).ok_or(Errno::EINVAL)?;
                    fs.getxattr(ctx, h.nodeid, name, size)?
                } else {
                    fs.listxattr(ctx, h.nodeid, size)?
                };
                match got {
                    Xattr::Size(n) => {
                        let mut w = Writer::with_capacity(8);
                        w.u32(n).u32(0);
                        Ok(w.0)
                    }
                    Xattr::Data(d) if d.len() <= size as usize => Ok(d),
                    Xattr::Data(_) => Err(Errno::ERANGE),
                }
            }
            op::IOCTL => {
                let fh = r.u64().ok_or(Errno::EINVAL)?;
                let flags = r.u32().ok_or(Errno::EINVAL)?;
                let cmd = r.u32().ok_or(Errno::EINVAL)?;
                let _arg = r.u64().ok_or(Errno::EINVAL)?;
                let in_size = r.u32().ok_or(Errno::EINVAL)?;
                let out_size = r.u32().ok_or(Errno::EINVAL)?;
                if flags != 0 || in_size != 0 || out_size > 128 {
                    return Err(Errno::ENOTTY);
                }
                let (result, data) = fs.ioctl(h.nodeid, fh, cmd, out_size)?;
                if data.len() != out_size as usize {
                    return Err(Errno::EINVAL);
                }
                let mut w = Writer::with_capacity(16 + data.len());
                w.u32(result as u32).zeros(12);
                w.0.extend_from_slice(&data);
                Ok(w.0)
            }
            op::REMOVEXATTR => {
                let name = abi::name(body).ok_or(Errno::EINVAL)?;
                fs.removexattr(ctx, h.nodeid, name)?;
                Ok(Vec::new())
            }
            _ => Err(Errno::ENOSYS),
        }
    }

    fn write(&self, ctx: &Ctx, h: &InHeader, body: &[u8], req: &Request<'_>) -> Reply {
        let mut r = Reader::new(body);
        let (fh, offset) = (r.u64().ok_or(Errno::EINVAL)?, r.u64().ok_or(Errno::EINVAL)?);
        let (size, flags) = (r.u32().ok_or(Errno::EINVAL)?, r.u32().ok_or(Errno::EINVAL)?);
        if body.len() < abi::WRITE_IN_LEN {
            return Err(Errno::EINVAL);
        }
        let size = size as usize;
        if size > MAX_IO as usize || size > req.payload.len() {
            return Err(Errno::EINVAL);
        }
        let data = req.payload.sub(0, size);
        let data = unsafe { IoVecs::new(&data) };
        let done = self.fs.write(
            ctx,
            h.nodeid,
            fh,
            offset,
            data,
            flags & abi::WRITE_KILL_SUIDGID != 0,
            flags & abi::WRITE_CACHE != 0,
        )?;
        let mut w = Writer::with_capacity(abi::WRITE_OUT_LEN);
        w.u32(done.min(size) as u32).u32(0);
        Ok(w.0)
    }

    fn read(&self, ctx: &Ctx, h: &InHeader, body: &[u8], req: &Request<'_>) -> usize {
        let mut r = Reader::new(body);
        let (Some(fh), Some(offset), Some(size)) = (r.u64(), r.u64(), r.u32()) else {
            return reply_error(req, h.unique, Errno::EINVAL);
        };
        let want =
            (size.min(MAX_IO) as usize).min(req.reply.len().saturating_sub(abi::OUT_HEADER_LEN));
        let data = req.reply.sub(abi::OUT_HEADER_LEN, want);
        let data = unsafe { IoVecs::new(&data) };
        match self.fs.read(ctx, h.nodeid, fh, offset, data) {
            Ok(got) => {
                let got = got.min(want);
                let len = (abi::OUT_HEADER_LEN + got) as u32;
                if req.reply.write_at(0, &abi::out_header(len, 0, h.unique)) {
                    len as usize
                } else {
                    0
                }
            }
            Err(e) => reply_error(req, h.unique, e),
        }
    }
}

fn reply(req: &Request<'_>, unique: u64, body: &[u8]) -> usize {
    let len = abi::OUT_HEADER_LEN + body.len();
    if len > req.reply.len() {
        return reply_error(req, unique, Errno::EIO);
    }
    if req
        .reply
        .write_at(0, &abi::out_header(len as u32, 0, unique))
        && req.reply.write_at(abi::OUT_HEADER_LEN, body)
    {
        len
    } else {
        0
    }
}

fn reply_error(req: &Request<'_>, unique: u64, e: Errno) -> usize {
    let h = abi::out_header(abi::OUT_HEADER_LEN as u32, -e.0, unique);
    if req.reply.write_at(0, &h) {
        abi::OUT_HEADER_LEN
    } else {
        0
    }
}

pub const VALID: Duration = Duration::from_secs(1);
pub const LONG: Duration = Duration::from_secs(3600);

#[cfg(test)]
mod tests {
    use super::*;
    use crate::client::Client;
    use crate::fake::FakeFs;

    fn client() -> Client {
        let mut fs = FakeFs::new();
        fs.add_dir(b"dir");
        fs.add_file(b"dir/hello", b"hello, guest\n");
        fs.add_file(b"big", &vec![7u8; 300_000]);
        fs.add_symlink(b"link", b"dir/hello");
        Client::new(Server::new(Box::new(fs)))
    }

    #[test]
    fn init_negotiates_the_design_s_flags_and_nothing_else() {
        let c = client();
        let offered = !(1u64 << 31);
        let out = c.init_with(7, 45, 128 << 10, offered).unwrap();
        let info = c.server.negotiated().unwrap();
        assert_eq!(info.flags_out, init::WANTED);
        assert_eq!(info.flags_in, offered);
        assert_eq!((info.max_pages, info.max_write), (256, 1 << 20));
        assert_eq!(out.len(), abi::INIT_OUT_LEN);
        for declined in [
            init::POSIX_LOCKS,
            init::FLOCK_LOCKS,
            init::WRITEBACK_CACHE,
            init::NO_OPEN_SUPPORT,
            init::NO_OPENDIR_SUPPORT,
            init::EXPORT_SUPPORT,
            init::POSIX_ACL,
            init::MAP_ALIGNMENT,
            init::HAS_INODE_DAX,
            init::SECURITY_CTX,
            init::ALLOW_IDMAP,
            init::SUBMOUNTS,
        ] {
            assert_eq!(info.flags_out & declined, 0, "{declined:#x}");
        }
        let c = client();
        c.init_with(7, 45, 4096, init::ASYNC_READ | init::POSIX_LOCKS)
            .unwrap();
        let info = c.server.negotiated().unwrap();
        assert_eq!(info.flags_out, init::ASYNC_READ);
        assert_eq!(info.max_pages, 0);
    }

    #[test]
    fn version_mismatches_are_answered_as_the_protocol_says() {
        let c = client();
        let out = c.init_with(8, 0, 0, 0).unwrap();
        assert_eq!(&out[..8], &[7, 0, 0, 0, 45, 0, 0, 0]);
        assert!(c.server.negotiated().is_none());
        assert_eq!(c.init_with(6, 9, 0, 0), Err(Errno::EPROTO));
        assert_eq!(c.init_with(7, 30, 0, 0), Err(Errno::EPROTO));
        assert_eq!(c.getattr(abi::ROOT_ID).err(), Some(Errno::EIO));
    }

    #[test]
    fn lookup_getattr_read_and_readlink_round_trip() {
        let c = client();
        c.init();
        let root = c.getattr(abi::ROOT_ID).unwrap();
        assert_eq!(root.mode & 0o170000, 0o040000);
        let dir = c.lookup(abi::ROOT_ID, b"dir").unwrap();
        let hello = c.lookup(dir.nodeid, b"hello").unwrap();
        assert_eq!(hello.attr.size, 13);
        assert_eq!(hello.entry_valid, VALID);
        let fh = c.open(hello.nodeid, 0).unwrap();
        assert_eq!(
            c.read(hello.nodeid, fh, 0, 4096).unwrap(),
            b"hello, guest\n"
        );
        assert_eq!(c.read(hello.nodeid, fh, 7, 3).unwrap(), b"gue");
        assert_eq!(c.read(hello.nodeid, fh, 100, 10).unwrap(), b"");
        c.release(hello.nodeid, fh).unwrap();
        assert_eq!(c.read(hello.nodeid, fh, 0, 10), Err(Errno::EBADF));

        let link = c.lookup(abi::ROOT_ID, b"link").unwrap();
        assert_eq!(c.readlink(link.nodeid).unwrap(), b"dir/hello");
        assert_eq!(c.lookup(abi::ROOT_ID, b"absent").err(), Some(Errno::ENOENT));
    }

    #[test]
    fn a_large_read_fills_scattered_reply_buffers() {
        let c = client();
        c.init();
        let big = c.lookup(abi::ROOT_ID, b"big").unwrap();
        let fh = c.open(big.nodeid, 0).unwrap();
        let got = c.read(big.nodeid, fh, 0, 1 << 20).unwrap();
        assert_eq!(got.len(), 300_000);
        assert!(got.iter().all(|b| *b == 7));
        assert_eq!(c.read(big.nodeid, fh, 0, u32::MAX).unwrap().len(), 300_000);
    }

    #[test]
    fn readdir_resumes_from_an_offset_and_plus_counts_lookups() {
        let mut fs = FakeFs::new();
        for i in 0..200 {
            fs.add_file(format!("f{i:03}").as_bytes(), b"x");
        }
        let c = Client::new(Server::new(Box::new(fs)));
        c.init();
        let fh = c.opendir(abi::ROOT_ID).unwrap();
        let mut names = Vec::new();
        let mut off = 0;
        loop {
            let batch = c.readdir(abi::ROOT_ID, fh, off, 512, false).unwrap();
            if batch.is_empty() {
                break;
            }
            off = batch.last().unwrap().off;
            names.extend(batch.into_iter().map(|e| e.name));
        }
        assert_eq!(names.len(), 200);
        assert_eq!(names[0], b"f000");
        assert_eq!(names[199], b"f199");

        let plus = c.readdir(abi::ROOT_ID, fh, 0, 1 << 20, true).unwrap();
        assert_eq!(plus.len(), 200);
        let e = plus[5].entry.unwrap();
        assert_ne!(e.nodeid, 0);
        assert_eq!(e.attr.size, 1);
        assert!(c.getattr(e.nodeid).is_ok());
        c.forget(e.nodeid, 1);
        assert_eq!(c.getattr(e.nodeid).err(), Some(Errno::ESTALE));
        c.releasedir(abi::ROOT_ID, fh).unwrap();
    }

    #[test]
    fn forget_and_batch_forget_drop_nodes_and_have_no_reply() {
        let c = client();
        c.init();
        let a = c.lookup(abi::ROOT_ID, b"big").unwrap().nodeid;
        let a2 = c.lookup(abi::ROOT_ID, b"big").unwrap().nodeid;
        assert_eq!(a, a2);
        c.forget(a, 1);
        assert!(c.getattr(a).is_ok(), "one lookup is still counted");
        let d = c.lookup(abi::ROOT_ID, b"dir").unwrap().nodeid;
        c.batch_forget(&[(a, 1), (d, 1)]);
        assert_eq!(c.getattr(a).err(), Some(Errno::ESTALE));
        assert_eq!(c.getattr(d).err(), Some(Errno::ESTALE));
        let mut body = Vec::new();
        body.extend_from_slice(&u32::MAX.to_le_bytes());
        body.extend_from_slice(&[0; 4]);
        assert_eq!(c.raw(op::BATCH_FORGET, 0, &body, 64).0, 0);
    }

    #[test]
    fn a_filesystem_that_only_reads_refuses_writes_without_latching() {
        let c = client();
        c.init();
        let big = c.lookup(abi::ROOT_ID, b"big").unwrap().nodeid;
        let ro = Some(Errno::EROFS);
        assert_eq!(c.create(abi::ROOT_ID, b"n", 0o644, 1).err(), ro);
        assert_eq!(c.mkdir(abi::ROOT_ID, b"n", 0o755).err(), ro);
        assert_eq!(c.mknod(abi::ROOT_ID, b"n", 0o010644, 0).err(), ro);
        assert_eq!(c.symlink(abi::ROOT_ID, b"n", b"t").err(), ro);
        assert_eq!(c.link(big, abi::ROOT_ID, b"n").err(), ro);
        assert_eq!(c.unlink(abi::ROOT_ID, b"big").err(), ro);
        assert_eq!(c.rmdir(abi::ROOT_ID, b"dir").err(), ro);
        assert_eq!(
            c.rename(abi::ROOT_ID, b"big", abi::ROOT_ID, b"n", 0).err(),
            ro
        );
        assert_eq!(c.chmod(big, 0o600).err(), ro);
        assert_eq!(c.write(big, 1, 0, b"x").err(), ro);
        assert_eq!(c.fallocate(big, 1, 0, 0, 1).err(), ro);
        assert_eq!(c.setxattr(big, b"user.a", b"b", 0).err(), ro);
        assert_eq!(c.removexattr(big, b"user.a").err(), ro);
        assert!(c.call(op::SYNCFS, abi::ROOT_ID, &[0; 8]).is_ok());
        assert!(c.fsync(big, 1, false).is_ok());
    }

    #[test]
    fn opcodes_the_server_does_without_are_enosys_for_the_connection() {
        let c = client();
        c.init();
        for opcode in [
            op::FLUSH,
            op::ACCESS,
            op::GETLK,
            op::SETLK,
            op::SETLKW,
            op::POLL,
            op::BMAP,
            op::COPY_FILE_RANGE,
            op::COPY_FILE_RANGE_64,
            op::SETUPMAPPING,
            op::REMOVEMAPPING,
            op::TMPFILE,
            op::STATX,
            7,
            19,
            54,
            9999,
        ] {
            assert_eq!(
                c.call(opcode, abi::ROOT_ID, &[0; 64]).err(),
                Some(Errno::ENOSYS),
                "{opcode}"
            );
        }
    }

    #[test]
    fn every_write_opcode_is_decoded_and_answered() {
        let mut fs = FakeFs::new().accepting();
        fs.add_dir(b"dir");
        fs.add_file(b"file", b"content");
        let c = Client::new(Server::new(Box::new(fs)));
        c.init();
        let file = c.lookup(abi::ROOT_ID, b"file").unwrap().nodeid;
        let (e, fh) = c.create(abi::ROOT_ID, b"new", 0o644, 2).unwrap();
        assert_eq!(e.nodeid, file);
        assert_eq!(c.write(file, fh, 0, &[7; 70_000]).unwrap(), 70_000);
        assert!(c.mkdir(abi::ROOT_ID, b"d", 0o755).is_ok());
        assert!(c.mknod(abi::ROOT_ID, b"n", 0o020600, 0x103).is_ok());
        assert!(c.symlink(abi::ROOT_ID, b"s", b"a/long/target").is_ok());
        assert_eq!(c.link(file, abi::ROOT_ID, b"l").unwrap().nodeid, file);
        c.unlink(abi::ROOT_ID, b"x").unwrap();
        c.rmdir(abi::ROOT_ID, b"x").unwrap();
        c.rename(abi::ROOT_ID, b"a", abi::ROOT_ID, b"b", 1).unwrap();
        assert_eq!(c.chmod(file, 0o600).unwrap().size, 7);
        c.fsync(file, fh, false).unwrap();
        c.fallocate(file, fh, 0, 0, 4096).unwrap();
        assert_eq!(c.lseek(file, fh, 12, 3).unwrap(), 12);
        c.setxattr(file, b"user.name", &[9; 40_000], 0).unwrap();
        assert_eq!(
            c.getxattr(file, b"user.name", 0).unwrap(),
            9u32.to_le_bytes()
        );
        assert_eq!(c.getxattr(file, b"user.name", 64).unwrap(), b"user.name");
        assert_eq!(c.getxattr(file, b"user.name", 4).err(), Some(Errno::ERANGE));
        assert_eq!(c.listxattr(file).unwrap(), [b"user.a".to_vec()]);
        c.removexattr(file, b"user.name").unwrap();
        let mut body = abi::ROOT_ID.to_le_bytes().to_vec();
        body.extend_from_slice(b"a\0b\0");
        assert!(c.call(op::RENAME, abi::ROOT_ID, &body).is_ok());

        for opcode in [
            op::SETATTR,
            op::MKNOD,
            op::MKDIR,
            op::RENAME,
            op::RENAME2,
            op::LINK,
            op::WRITE,
            op::CREATE,
            op::FSYNC,
            op::FALLOCATE,
            op::LSEEK,
            op::SETXATTR,
            op::GETXATTR,
            op::LISTXATTR,
        ] {
            for len in [0, 3, 7] {
                assert_eq!(
                    c.call(opcode, abi::ROOT_ID, &vec![0; len]).err(),
                    Some(Errno::EINVAL),
                    "opcode {opcode} with {len} bytes"
                );
            }
        }
        for body in [&b"one\0"[..], b"\0two\0", b"one\0\0", b"one\0two", b""] {
            assert_eq!(
                c.call(op::SYMLINK, abi::ROOT_ID, body).err(),
                Some(Errno::EINVAL),
                "{body:?}"
            );
        }
        let mut w = Writer::default();
        w.u64(fh).u64(0).u32(100).u32(0).u64(0).u32(0).u32(0);
        w.0.extend_from_slice(&[1; 50]);
        assert_eq!(c.call(op::WRITE, file, &w.0).err(), Some(Errno::EINVAL));
        let mut w = Writer::default();
        w.u64(fh).u64(0).u32(u32::MAX).u32(0).u64(0).u32(0).u32(0);
        assert_eq!(c.call(op::WRITE, file, &w.0).err(), Some(Errno::EINVAL));
        let mut w = Writer::default();
        w.u32(1 << 20).u32(0).u32(0).u32(0);
        w.0.extend_from_slice(b"user.big\0");
        assert_eq!(c.call(op::SETXATTR, file, &w.0).err(), Some(Errno(7)));
        let mut w = Writer::default();
        w.u32(5000).u32(0).u32(0).u32(0);
        w.0.extend_from_slice(b"user.short\0abc");
        assert_eq!(c.call(op::SETXATTR, file, &w.0).err(), Some(Errno::EINVAL));
    }

    #[test]
    fn destroy_forgets_everything_and_a_new_init_starts_clean() {
        let c = client();
        c.init();
        let n = c.lookup(abi::ROOT_ID, b"big").unwrap().nodeid;
        let fh = c.open(n, 0).unwrap();
        c.call(op::DESTROY, 0, &[]).unwrap();
        assert_eq!(c.getattr(abi::ROOT_ID).err(), Some(Errno::EIO));
        c.init();
        assert_eq!(c.getattr(n).err(), Some(Errno::ESTALE));
        assert_eq!(c.read(n, fh, 0, 10), Err(Errno::EBADF));
        assert!(c.lookup(abi::ROOT_ID, b"big").is_ok());
    }

    #[test]
    fn malformed_requests_get_an_error_or_nothing_never_a_panic() {
        let c = client();
        c.init();
        assert_eq!(
            c.call(op::GETATTR, abi::ROOT_ID, &[0; 3]).err(),
            Some(Errno::EINVAL)
        );
        assert_eq!(
            c.call(op::OPEN, abi::ROOT_ID, &[]).err(),
            Some(Errno::EINVAL)
        );
        assert_eq!(
            c.call(op::READ, abi::ROOT_ID, &[0; 19]).err(),
            Some(Errno::EINVAL)
        );
        for name in [&b"abc"[..], b"\0", b"", b"a\0b\0"] {
            assert_eq!(
                c.call(op::LOOKUP, abi::ROOT_ID, name).err(),
                Some(Errno::EINVAL)
            );
        }
        for len in [0, 8, 36, 39] {
            let o = crate::iov::OwnedIov::new(64, 64);
            let req = Request {
                head: &[1u8; 39][..len],
                payload: IoVecs::empty(),
                reply: o.iov(),
            };
            assert_eq!(c.server.handle(&req), 0);
        }
        let o = crate::iov::OwnedIov::new(64, 64);
        let req = Request {
            head: &[1, 2, 3],
            payload: IoVecs::empty(),
            reply: o.iov(),
        };
        assert_eq!(c.server.handle(&req), 0);
        assert_eq!(c.raw(op::GETATTR, abi::ROOT_ID, &[0; 16], 15).0, 0);
        let (n, out) = c.raw(op::GETATTR, abi::ROOT_ID, &[0; 16], 40);
        assert_eq!(n, 16);
        assert_eq!(
            i32::from_le_bytes(out[4..8].try_into().unwrap()),
            -Errno::EIO.0
        );
    }

    #[test]
    fn head_len_leaves_write_data_in_place() {
        assert_eq!(head_len(op::WRITE, 1 << 20), 80);
        assert_eq!(head_len(op::WRITE, 50), 50);
        assert_eq!(head_len(op::LOOKUP, 60), 60);
        assert_eq!(head_len(op::LOOKUP, 1 << 20), MAX_HEAD);
    }

    struct Asked {
        asked: Mutex<Vec<(u64, Need)>>,
        slow: u64,
    }

    impl Filesystem for Asked {
        fn quick(&self, node: u64, need: Need) -> bool {
            self.asked.lock().unwrap().push((node, need));
            node != self.slow
        }
        fn lookup(&self, _: &Ctx, _: u64, _: &[u8]) -> crate::fs::Result<Entry> {
            Err(Errno::ENOENT)
        }
        fn forget(&self, _: u64, _: u64) {}
        fn getattr(&self, _: &Ctx, _: u64, _: Option<u64>) -> crate::fs::Result<(Attr, Duration)> {
            Err(Errno::ENOENT)
        }
        fn readlink(&self, _: &Ctx, _: u64) -> crate::fs::Result<Vec<u8>> {
            Err(Errno::EINVAL)
        }
        fn open(&self, _: &Ctx, _: u64, _: u32, _: bool) -> crate::fs::Result<(u64, u32)> {
            Err(Errno::ENOENT)
        }
        fn read(&self, _: &Ctx, _: u64, _: u64, _: u64, _: IoVecs<'_>) -> crate::fs::Result<usize> {
            Err(Errno::EBADF)
        }
        fn release(&self, _: u64, _: u64) {}
        fn opendir(&self, _: &Ctx, _: u64, _: u32) -> crate::fs::Result<(u64, u32)> {
            Err(Errno::ENOENT)
        }
        fn readdir(
            &self,
            _: &Ctx,
            _: u64,
            _: u64,
            _: u64,
            _: &mut DirSink,
        ) -> crate::fs::Result<()> {
            Err(Errno::EBADF)
        }
        fn releasedir(&self, _: u64, _: u64) {}
        fn statfs(&self, _: &Ctx, _: u64) -> crate::fs::Result<crate::fs::Statfs> {
            Err(Errno::ENOENT)
        }
        fn destroy(&self) {}
    }

    #[test]
    fn quick_asks_about_every_node_a_request_names_and_refuses_what_is_not_one_call() {
        let fs = std::sync::Arc::new(Asked {
            asked: Mutex::new(Vec::new()),
            slow: 66,
        });
        let server = Server::shared(fs.clone());
        let head = |opcode: u32, node: u64, body: &[u8]| {
            let mut w = Writer::default();
            w.u32((abi::IN_HEADER_LEN + body.len()) as u32)
                .u32(opcode)
                .u64(9)
                .u64(node)
                .u32(0)
                .u32(0)
                .u32(1)
                .u32(0);
            w.0.extend_from_slice(body);
            w.0
        };
        let ask = |opcode: u32, node: u64, body: &[u8]| {
            fs.asked.lock().unwrap().clear();
            let quick = server.quick(&head(opcode, node, body));
            (quick, fs.asked.lock().unwrap().clone())
        };
        let u64s = |v: &[u64]| v.iter().flat_map(|x| x.to_le_bytes()).collect::<Vec<u8>>();
        assert_eq!(ask(op::LOOKUP, 5, b"n\0"), (true, vec![(5, Need::Lookup)]));
        assert_eq!(ask(op::GETATTR, 5, &[0; 16]), (true, vec![(5, Need::Meta)]));
        for opcode in [
            op::CREATE,
            op::MKDIR,
            op::MKNOD,
            op::SYMLINK,
            op::UNLINK,
            op::RMDIR,
        ] {
            assert_eq!(ask(opcode, 5, &[0; 16]), (true, vec![(5, Need::Names)]));
        }
        for opcode in [
            op::OPEN,
            op::OPENDIR,
            op::READLINK,
            op::GETXATTR,
            op::LISTXATTR,
        ] {
            assert_eq!(ask(opcode, 5, &[0; 8]), (true, vec![(5, Need::File)]));
        }
        for opcode in [op::READ, op::WRITE, op::RELEASE, op::RELEASEDIR, op::LSEEK] {
            assert_eq!(
                ask(opcode, 5, &u64s(&[77, 0])),
                (true, vec![(5, Need::Handle(77))])
            );
            assert_eq!(ask(opcode, 5, &[0; 4]), (false, vec![]));
        }
        for opcode in [op::RENAME, op::RENAME2] {
            let body = [u64s(&[6, 0]), b"a\0b\0".to_vec()].concat();
            assert_eq!(
                ask(opcode, 5, &body),
                (true, vec![(5, Need::Names), (6, Need::Names)])
            );
            let body = [u64s(&[66, 0]), b"a\0b\0".to_vec()].concat();
            assert!(!ask(opcode, 5, &body).0);
            assert!(!ask(opcode, 66, &u64s(&[6, 0])).0);
            assert!(!ask(opcode, 5, &[0; 4]).0, "no second directory");
        }
        assert_eq!(
            ask(op::LINK, 5, &[u64s(&[6]), b"n\0".to_vec()].concat()),
            (true, vec![(5, Need::Names), (6, Need::File)])
        );
        let setattr = |valid: u32, fh: u64| {
            let mut w = Writer::default();
            w.u32(valid).u32(0).u64(fh).zeros(72);
            w.0
        };
        assert_eq!(
            ask(
                op::SETATTR,
                5,
                &setattr(abi::fattr::FH | abi::fattr::SIZE, 77)
            ),
            (true, vec![(5, Need::Handle(77))])
        );
        assert_eq!(
            ask(op::SETATTR, 5, &setattr(abi::fattr::MODE, 77)),
            (true, vec![(5, Need::File)])
        );
        assert_eq!(ask(op::SETATTR, 5, &[0; 8]), (false, vec![]));
        for opcode in [
            op::FSYNC,
            op::FSYNCDIR,
            op::SYNCFS,
            op::READDIR,
            op::READDIRPLUS,
            op::FALLOCATE,
            op::COPY_FILE_RANGE,
            op::SETXATTR,
            op::REMOVEXATTR,
            op::STATFS,
            op::INIT,
            op::DESTROY,
            op::FORGET,
            op::BATCH_FORGET,
            op::FLUSH,
            op::GETLK,
            op::SETLK,
            op::SETLKW,
            op::IOCTL,
            op::TMPFILE,
            999,
        ] {
            assert_eq!(ask(opcode, 5, &[0; 64]), (false, vec![]), "opcode {opcode}");
        }
        assert!(!server.quick(&[0; 12]));
    }
}
