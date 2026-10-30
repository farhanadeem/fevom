use std::sync::atomic::{AtomicU32, AtomicU64, Ordering};
use std::time::Duration;

use crate::abi::{self, op, Reader, Writer};
use crate::errno::Errno;
use crate::fs::{Attr, Entry, Statfs};
use crate::iov::{IoVecs, OwnedIov};
use crate::server::{head_len, Request, Server};

pub struct Client {
    pub server: Server,
    unique: AtomicU64,
    pub uid: AtomicU32,
    pub gid: AtomicU32,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DirEntry {
    pub ino: u64,
    pub off: u64,
    pub kind: u32,
    pub name: Vec<u8>,
    pub entry: Option<Entry>,
}

fn parse_attr(r: &mut Reader<'_>) -> Option<Attr> {
    let (ino, size, blocks) = (r.u64()?, r.u64()?, r.u64()?);
    let (at, mt, ct) = (r.u64()?, r.u64()?, r.u64()?);
    let (atn, mtn, ctn) = (r.u32()?, r.u32()?, r.u32()?);
    Some(Attr {
        ino,
        size,
        blocks,
        atime: (at, atn),
        mtime: (mt, mtn),
        ctime: (ct, ctn),
        mode: r.u32()?,
        nlink: r.u32()?,
        uid: r.u32()?,
        gid: r.u32()?,
        rdev: r.u32()?,
        blksize: r.u32()?,
        flags: r.u32()?,
    })
}

fn parse_entry(r: &mut Reader<'_>) -> Option<Entry> {
    let (nodeid, generation) = (r.u64()?, r.u64()?);
    let (ev, av) = (r.u64()?, r.u64()?);
    let (evn, avn) = (r.u32()?, r.u32()?);
    Some(Entry {
        nodeid,
        generation,
        entry_valid: Duration::new(ev, evn),
        attr_valid: Duration::new(av, avn),
        attr: parse_attr(r)?,
    })
}

impl Client {
    pub fn new(server: Server) -> Self {
        Self {
            server,
            unique: AtomicU64::new(1),
            uid: AtomicU32::new(0),
            gid: AtomicU32::new(0),
        }
    }

    pub fn raw(&self, opcode: u32, nodeid: u64, body: &[u8], cap: usize) -> (usize, Vec<u8>) {
        let unique = self.unique.fetch_add(1, Ordering::Relaxed);
        let mut w = Writer::with_capacity(abi::IN_HEADER_LEN + body.len());
        w.u32((abi::IN_HEADER_LEN + body.len()) as u32)
            .u32(opcode)
            .u64(unique)
            .u64(nodeid)
            .u32(self.uid.load(Ordering::Relaxed))
            .u32(self.gid.load(Ordering::Relaxed))
            .u32(4242)
            .u32(0);
        w.0.extend_from_slice(body);
        let head = head_len(opcode, w.0.len());
        let mut tail = w.0.split_off(head);
        let tail_iov = [libc::iovec {
            iov_base: tail.as_mut_ptr().cast(),
            iov_len: tail.len(),
        }];
        let reply = OwnedIov::new(cap, 4096);
        let n = self.server.handle(&Request {
            head: &w.0,
            payload: unsafe { IoVecs::new(&tail_iov) },
            reply: reply.iov(),
        });
        assert!(
            n <= cap,
            "the decoder claims {n} bytes of a {cap}-byte reply"
        );
        let out = reply.bytes(n);
        if n >= abi::OUT_HEADER_LEN {
            assert_eq!(
                u32::from_le_bytes(out[0..4].try_into().unwrap()) as usize,
                n
            );
            assert_eq!(u64::from_le_bytes(out[8..16].try_into().unwrap()), unique);
        }
        (n, out)
    }

    fn call_cap(
        &self,
        opcode: u32,
        nodeid: u64,
        body: &[u8],
        cap: usize,
    ) -> Result<Vec<u8>, Errno> {
        let (n, out) = self.raw(opcode, nodeid, body, cap);
        assert!(n >= abi::OUT_HEADER_LEN, "no reply to opcode {opcode}");
        match i32::from_le_bytes(out[4..8].try_into().unwrap()) {
            0 => Ok(out[abi::OUT_HEADER_LEN..].to_vec()),
            e => {
                assert_eq!(n, abi::OUT_HEADER_LEN, "an error reply carries no body");
                Err(Errno(-e))
            }
        }
    }

    pub fn call(&self, opcode: u32, nodeid: u64, body: &[u8]) -> Result<Vec<u8>, Errno> {
        self.call_cap(opcode, nodeid, body, 8192)
    }

    pub fn init_with(
        &self,
        major: u32,
        minor: u32,
        readahead: u32,
        flags: u64,
    ) -> Result<Vec<u8>, Errno> {
        let mut w = Writer::default();
        w.u32(major)
            .u32(minor)
            .u32(readahead)
            .u32(flags as u32)
            .u32((flags >> 32) as u32)
            .zeros(44);
        self.call(op::INIT, 0, &w.0)
    }

    pub fn init(&self) {
        self.init_with(abi::MAJOR, abi::MINOR, 128 << 10, !(1u64 << 31))
            .expect("INIT");
    }

    pub fn lookup(&self, parent: u64, name: &[u8]) -> Result<Entry, Errno> {
        let mut body = name.to_vec();
        body.push(0);
        let out = self.call(op::LOOKUP, parent, &body)?;
        Ok(parse_entry(&mut Reader::new(&out)).expect("entry_out"))
    }

    pub fn getattr_valid(&self, node: u64, fh: Option<u64>) -> Result<(Attr, Duration), Errno> {
        let mut w = Writer::default();
        w.u32(fh.is_some() as u32).u32(0).u64(fh.unwrap_or(0));
        let out = self.call(op::GETATTR, node, &w.0)?;
        let mut r = Reader::new(&out);
        let valid = Duration::new(r.u64().unwrap(), r.u32().unwrap());
        r.u32();
        Ok((parse_attr(&mut r).expect("attr_out"), valid))
    }

    pub fn getattr(&self, node: u64) -> Result<Attr, Errno> {
        self.getattr_valid(node, None).map(|(a, _)| a)
    }

    pub fn readlink(&self, node: u64) -> Result<Vec<u8>, Errno> {
        self.call(op::READLINK, node, &[])
    }

    fn open_op(&self, opcode: u32, node: u64, flags: u32) -> Result<u64, Errno> {
        let mut w = Writer::default();
        w.u32(flags).u32(0);
        let out = self.call(opcode, node, &w.0)?;
        Ok(Reader::new(&out).u64().expect("open_out"))
    }

    pub fn open(&self, node: u64, flags: u32) -> Result<u64, Errno> {
        self.open_op(op::OPEN, node, flags)
    }

    pub fn open_flags(&self, node: u64, flags: u32, dir: bool) -> Result<(u64, u32), Errno> {
        let mut w = Writer::default();
        w.u32(flags).u32(0);
        let opcode = if dir { op::OPENDIR } else { op::OPEN };
        let out = self.call(opcode, node, &w.0)?;
        let mut r = Reader::new(&out);
        Ok((r.u64().expect("open_out"), r.u32().expect("open_out")))
    }

    pub fn opendir(&self, node: u64) -> Result<u64, Errno> {
        self.open_op(op::OPENDIR, node, 0)
    }

    fn read_in(fh: u64, offset: u64, size: u32) -> Vec<u8> {
        let mut w = Writer::default();
        w.u64(fh).u64(offset).u32(size).zeros(20);
        w.0
    }

    pub fn read(&self, node: u64, fh: u64, offset: u64, size: u32) -> Result<Vec<u8>, Errno> {
        let cap = abi::OUT_HEADER_LEN + (size as usize).min(2 << 20);
        self.call_cap(op::READ, node, &Self::read_in(fh, offset, size), cap)
    }

    pub fn readdir(
        &self,
        node: u64,
        fh: u64,
        offset: u64,
        size: u32,
        plus: bool,
    ) -> Result<Vec<DirEntry>, Errno> {
        let opcode = if plus { op::READDIRPLUS } else { op::READDIR };
        let cap = abi::OUT_HEADER_LEN + size as usize;
        let out = self.call_cap(opcode, node, &Self::read_in(fh, offset, size), cap)?;
        assert!(out.len() <= size as usize);
        let mut entries = Vec::new();
        let mut at = 0;
        while at < out.len() {
            let mut r = Reader::new(&out[at..]);
            let entry = if plus {
                let e = parse_entry(&mut r).expect("direntplus");
                (e.nodeid != 0).then_some(e)
            } else {
                None
            };
            let (ino, off) = (r.u64().unwrap(), r.u64().unwrap());
            let (namelen, kind) = (r.u32().unwrap() as usize, r.u32().unwrap());
            let name = r.rest()[..namelen].to_vec();
            at += if plus { abi::ENTRY_OUT_LEN } else { 0 } + abi::dirent_len(namelen);
            entries.push(DirEntry {
                ino,
                off,
                kind,
                name,
                entry,
            });
        }
        Ok(entries)
    }

    pub fn list(&self, node: u64, plus: bool) -> Result<Vec<DirEntry>, Errno> {
        let fh = self.opendir(node)?;
        let mut all = Vec::new();
        let mut off = 0;
        let knows_the_last = plus
            && self
                .server
                .negotiated()
                .is_some_and(|i| i.flags_out & abi::init::FEV_REQUESTS != 0);
        let longest = abi::ENTRY_OUT_LEN + abi::dirent_len(255);
        loop {
            let batch = self.readdir(node, fh, off, 4096, plus);
            match batch {
                Ok(b) if b.is_empty() => break,
                Ok(b) => {
                    off = b.last().unwrap().off;
                    let used: usize = b
                        .iter()
                        .map(|e| abi::ENTRY_OUT_LEN + abi::dirent_len(e.name.len()))
                        .sum();
                    all.extend(b);
                    if knows_the_last && used + longest <= 4096 {
                        break;
                    }
                }
                Err(e) => {
                    let _ = self.releasedir(node, fh);
                    return Err(e);
                }
            }
        }
        self.releasedir(node, fh)?;
        Ok(all)
    }

    fn release_op(&self, opcode: u32, node: u64, fh: u64) -> Result<(), Errno> {
        let mut w = Writer::default();
        w.u64(fh).zeros(16);
        self.call(opcode, node, &w.0).map(|_| ())
    }

    pub fn release(&self, node: u64, fh: u64) -> Result<(), Errno> {
        self.release_op(op::RELEASE, node, fh)
    }

    pub fn releasedir(&self, node: u64, fh: u64) -> Result<(), Errno> {
        if fh == 0 {
            return Ok(());
        }
        self.release_op(op::RELEASEDIR, node, fh)
    }

    pub fn statfs(&self, node: u64) -> Result<Statfs, Errno> {
        let out = self.call(op::STATFS, node, &[])?;
        let mut r = Reader::new(&out);
        Ok(Statfs {
            blocks: r.u64().unwrap(),
            bfree: r.u64().unwrap(),
            bavail: r.u64().unwrap(),
            files: r.u64().unwrap(),
            ffree: r.u64().unwrap(),
            bsize: r.u32().unwrap(),
            namelen: r.u32().unwrap(),
            frsize: r.u32().unwrap(),
        })
    }

    fn named(args: &[u8], names: &[&[u8]]) -> Vec<u8> {
        let mut body = args.to_vec();
        for n in names {
            body.extend_from_slice(n);
            body.push(0);
        }
        body
    }

    fn entry(&self, opcode: u32, node: u64, body: &[u8]) -> Result<Entry, Errno> {
        let out = self.call(opcode, node, body)?;
        Ok(parse_entry(&mut Reader::new(&out)).expect("entry_out"))
    }

    pub fn create(
        &self,
        parent: u64,
        name: &[u8],
        mode: u32,
        flags: u32,
    ) -> Result<(Entry, u64), Errno> {
        let mut w = Writer::default();
        w.u32(flags).u32(mode).u32(0o022).u32(0);
        let out = self.call(op::CREATE, parent, &Self::named(&w.0, &[name]))?;
        let mut r = Reader::new(&out);
        let entry = parse_entry(&mut r).expect("entry_out");
        Ok((entry, r.u64().expect("open_out")))
    }

    pub fn mkdir(&self, parent: u64, name: &[u8], mode: u32) -> Result<Entry, Errno> {
        let mut w = Writer::default();
        w.u32(mode).u32(0o022);
        self.entry(op::MKDIR, parent, &Self::named(&w.0, &[name]))
    }

    pub fn mknod(&self, parent: u64, name: &[u8], mode: u32, rdev: u32) -> Result<Entry, Errno> {
        let mut w = Writer::default();
        w.u32(mode).u32(rdev).u32(0).u32(0);
        self.entry(op::MKNOD, parent, &Self::named(&w.0, &[name]))
    }

    pub fn symlink(&self, parent: u64, name: &[u8], target: &[u8]) -> Result<Entry, Errno> {
        self.entry(op::SYMLINK, parent, &Self::named(&[], &[name, target]))
    }

    pub fn link(&self, node: u64, newparent: u64, newname: &[u8]) -> Result<Entry, Errno> {
        self.entry(
            op::LINK,
            newparent,
            &Self::named(&node.to_le_bytes(), &[newname]),
        )
    }

    pub fn unlink(&self, parent: u64, name: &[u8]) -> Result<(), Errno> {
        self.call(op::UNLINK, parent, &Self::named(&[], &[name]))
            .map(|_| ())
    }

    pub fn rmdir(&self, parent: u64, name: &[u8]) -> Result<(), Errno> {
        self.call(op::RMDIR, parent, &Self::named(&[], &[name]))
            .map(|_| ())
    }

    pub fn rename(
        &self,
        parent: u64,
        name: &[u8],
        newparent: u64,
        newname: &[u8],
        flags: u32,
    ) -> Result<(), Errno> {
        let mut w = Writer::default();
        w.u64(newparent).u32(flags).u32(0);
        self.call(op::RENAME2, parent, &Self::named(&w.0, &[name, newname]))
            .map(|_| ())
    }

    pub fn write(&self, node: u64, fh: u64, offset: u64, data: &[u8]) -> Result<u32, Errno> {
        self.write_flags(node, fh, offset, data, 0)
    }

    pub fn write_flags(
        &self,
        node: u64,
        fh: u64,
        offset: u64,
        data: &[u8],
        write_flags: u32,
    ) -> Result<u32, Errno> {
        let mut w = Writer::default();
        w.u64(fh)
            .u64(offset)
            .u32(data.len() as u32)
            .u32(write_flags)
            .u64(0)
            .u32(0)
            .u32(0);
        w.0.extend_from_slice(data);
        let out = self.call(op::WRITE, node, &w.0)?;
        Ok(Reader::new(&out).u32().expect("write_out"))
    }

    #[allow(clippy::too_many_arguments)]
    pub fn setattr(
        &self,
        node: u64,
        valid: u32,
        fh: u64,
        size: u64,
        mode: u32,
        atime: (u64, u32),
        mtime: (u64, u32),
    ) -> Result<Attr, Errno> {
        let mut w = Writer::default();
        w.u32(valid).u32(0).u64(fh).u64(size).u64(0);
        w.u64(atime.0).u64(mtime.0).u64(0);
        w.u32(atime.1).u32(mtime.1).u32(0);
        w.u32(mode).u32(0).u32(4321).u32(8765).u32(0);
        let out = self.call(op::SETATTR, node, &w.0)?;
        let mut r = Reader::new(&out);
        r.skip(16);
        Ok(parse_attr(&mut r).expect("attr_out"))
    }

    pub fn chmod(&self, node: u64, mode: u32) -> Result<Attr, Errno> {
        self.setattr(node, abi::fattr::MODE, 0, 0, mode, (0, 0), (0, 0))
    }

    pub fn truncate(&self, node: u64, fh: Option<u64>, size: u64) -> Result<Attr, Errno> {
        let valid = abi::fattr::SIZE | if fh.is_some() { abi::fattr::FH } else { 0 };
        self.setattr(node, valid, fh.unwrap_or(0), size, 0, (0, 0), (0, 0))
    }

    pub fn fsync(&self, node: u64, fh: u64, dir: bool) -> Result<(), Errno> {
        let mut w = Writer::default();
        w.u64(fh).u32(0).u32(0);
        let opcode = if dir { op::FSYNCDIR } else { op::FSYNC };
        self.call(opcode, node, &w.0).map(|_| ())
    }

    pub fn fallocate(
        &self,
        node: u64,
        fh: u64,
        mode: u32,
        offset: u64,
        len: u64,
    ) -> Result<(), Errno> {
        let mut w = Writer::default();
        w.u64(fh).u64(offset).u64(len).u32(mode).u32(0);
        self.call(op::FALLOCATE, node, &w.0).map(|_| ())
    }

    pub fn lseek(&self, node: u64, fh: u64, offset: u64, whence: u32) -> Result<u64, Errno> {
        let mut w = Writer::default();
        w.u64(fh).u64(offset).u32(whence).u32(0);
        let out = self.call(op::LSEEK, node, &w.0)?;
        Ok(Reader::new(&out).u64().expect("lseek_out"))
    }

    pub fn setxattr(&self, node: u64, name: &[u8], value: &[u8], flags: u32) -> Result<(), Errno> {
        let mut w = Writer::default();
        w.u32(value.len() as u32).u32(flags).u32(0).u32(0);
        let mut body = Self::named(&w.0, &[name]);
        body.extend_from_slice(value);
        self.call_cap(op::SETXATTR, node, &body, 64).map(|_| ())
    }

    pub fn getxattr(&self, node: u64, name: &[u8], size: u32) -> Result<Vec<u8>, Errno> {
        let mut w = Writer::default();
        w.u32(size).u32(0);
        let out = self.call_cap(
            op::GETXATTR,
            node,
            &Self::named(&w.0, &[name]),
            16 + (size as usize).max(8),
        )?;
        Ok(if size == 0 { out[..4].to_vec() } else { out })
    }

    pub fn listxattr(&self, node: u64) -> Result<Vec<Vec<u8>>, Errno> {
        let mut w = Writer::default();
        w.u32(65536).u32(0);
        let out = self.call_cap(op::LISTXATTR, node, &w.0, 16 + 65536)?;
        Ok(out
            .split(|b| *b == 0)
            .filter(|n| !n.is_empty())
            .map(<[u8]>::to_vec)
            .collect())
    }

    pub fn removexattr(&self, node: u64, name: &[u8]) -> Result<(), Errno> {
        self.call(op::REMOVEXATTR, node, &Self::named(&[], &[name]))
            .map(|_| ())
    }

    pub fn forget(&self, node: u64, n: u64) {
        let (written, _) = self.raw(op::FORGET, node, &n.to_le_bytes(), 0);
        assert_eq!(written, 0);
    }

    pub fn batch_forget(&self, items: &[(u64, u64)]) {
        let mut w = Writer::default();
        w.u32(items.len() as u32).u32(0);
        for (node, n) in items {
            w.u64(*node).u64(*n);
        }
        let (written, _) = self.raw(op::BATCH_FORGET, 0, &w.0, 0);
        assert_eq!(written, 0);
    }
}
