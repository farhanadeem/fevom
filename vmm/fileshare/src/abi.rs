pub const MAJOR: u32 = 7;
pub const MINOR: u32 = 45;
pub const MIN_MINOR: u32 = 36;

pub const IN_HEADER_LEN: usize = 40;
pub const OUT_HEADER_LEN: usize = 16;
pub const ATTR_LEN: usize = 88;
pub const ENTRY_OUT_LEN: usize = 128;
pub const ATTR_OUT_LEN: usize = 104;
pub const OPEN_OUT_LEN: usize = 16;
pub const INIT_OUT_LEN: usize = 64;
pub const STATFS_OUT_LEN: usize = 80;
pub const DIRENT_LEN: usize = 24;
pub const WRITE_IN_LEN: usize = 40;

pub const ROOT_ID: u64 = 1;
pub const UNKNOWN_INO: u64 = 0xffff_ffff;

pub mod op {
    pub const LOOKUP: u32 = 1;
    pub const FORGET: u32 = 2;
    pub const GETATTR: u32 = 3;
    pub const SETATTR: u32 = 4;
    pub const READLINK: u32 = 5;
    pub const SYMLINK: u32 = 6;
    pub const MKNOD: u32 = 8;
    pub const MKDIR: u32 = 9;
    pub const UNLINK: u32 = 10;
    pub const RMDIR: u32 = 11;
    pub const RENAME: u32 = 12;
    pub const LINK: u32 = 13;
    pub const OPEN: u32 = 14;
    pub const READ: u32 = 15;
    pub const WRITE: u32 = 16;
    pub const STATFS: u32 = 17;
    pub const RELEASE: u32 = 18;
    pub const FSYNC: u32 = 20;
    pub const SETXATTR: u32 = 21;
    pub const GETXATTR: u32 = 22;
    pub const LISTXATTR: u32 = 23;
    pub const REMOVEXATTR: u32 = 24;
    pub const FLUSH: u32 = 25;
    pub const INIT: u32 = 26;
    pub const OPENDIR: u32 = 27;
    pub const READDIR: u32 = 28;
    pub const RELEASEDIR: u32 = 29;
    pub const FSYNCDIR: u32 = 30;
    pub const GETLK: u32 = 31;
    pub const SETLK: u32 = 32;
    pub const SETLKW: u32 = 33;
    pub const ACCESS: u32 = 34;
    pub const CREATE: u32 = 35;
    pub const INTERRUPT: u32 = 36;
    pub const BMAP: u32 = 37;
    pub const DESTROY: u32 = 38;
    pub const IOCTL: u32 = 39;
    pub const POLL: u32 = 40;
    pub const NOTIFY_REPLY: u32 = 41;
    pub const BATCH_FORGET: u32 = 42;
    pub const FALLOCATE: u32 = 43;
    pub const READDIRPLUS: u32 = 44;
    pub const RENAME2: u32 = 45;
    pub const LSEEK: u32 = 46;
    pub const COPY_FILE_RANGE: u32 = 47;
    pub const SETUPMAPPING: u32 = 48;
    pub const REMOVEMAPPING: u32 = 49;
    pub const SYNCFS: u32 = 50;
    pub const TMPFILE: u32 = 51;
    pub const STATX: u32 = 52;
    pub const COPY_FILE_RANGE_64: u32 = 53;
}

pub mod init {
    pub const ASYNC_READ: u64 = 1 << 0;
    pub const POSIX_LOCKS: u64 = 1 << 1;
    pub const ATOMIC_O_TRUNC: u64 = 1 << 3;
    pub const EXPORT_SUPPORT: u64 = 1 << 4;
    pub const BIG_WRITES: u64 = 1 << 5;
    pub const DONT_MASK: u64 = 1 << 6;
    pub const FLOCK_LOCKS: u64 = 1 << 10;
    pub const AUTO_INVAL_DATA: u64 = 1 << 12;
    pub const DO_READDIRPLUS: u64 = 1 << 13;
    pub const READDIRPLUS_AUTO: u64 = 1 << 14;
    pub const WRITEBACK_CACHE: u64 = 1 << 16;
    pub const NO_OPEN_SUPPORT: u64 = 1 << 17;
    pub const PARALLEL_DIROPS: u64 = 1 << 18;
    pub const POSIX_ACL: u64 = 1 << 20;
    pub const MAX_PAGES: u64 = 1 << 22;
    pub const CACHE_SYMLINKS: u64 = 1 << 23;
    pub const NO_OPENDIR_SUPPORT: u64 = 1 << 24;
    pub const MAP_ALIGNMENT: u64 = 1 << 26;
    pub const SUBMOUNTS: u64 = 1 << 27;
    pub const HANDLE_KILLPRIV_V2: u64 = 1 << 28;
    pub const SETXATTR_EXT: u64 = 1 << 29;
    pub const INIT_EXT: u64 = 1 << 30;
    pub const SECURITY_CTX: u64 = 1 << 32;
    pub const HAS_INODE_DAX: u64 = 1 << 33;
    pub const ALLOW_IDMAP: u64 = 1 << 40;
    pub const FEV_CACHE: u64 = 1 << 63;
    pub const FEV_REQUESTS: u64 = 1 << 62;
    pub const FEV_COMPLETE: u64 = 1 << 61;

    pub const WANTED: u64 = ASYNC_READ
        | BIG_WRITES
        | MAX_PAGES
        | INIT_EXT
        | PARALLEL_DIROPS
        | ATOMIC_O_TRUNC
        | DO_READDIRPLUS
        | AUTO_INVAL_DATA
        | DONT_MASK
        | HANDLE_KILLPRIV_V2
        | SETXATTR_EXT
        | CACHE_SYMLINKS
        | FEV_CACHE
        | FEV_REQUESTS
        | FEV_COMPLETE;
}

pub const ATTR_CALLER_OWNER: u32 = 1 << 31;
pub const ATTR_REAL_OWNER: u32 = 1 << 30;
pub const ATTR_NO_KEEP_CACHE: u32 = 1 << 29;
pub const FOPEN_FEV_FREE: u32 = 1 << 31;
pub const ATTR_NO_SEC: u32 = 1 << 28;
pub const ATTR_OPEN_FREE: u32 = 1 << 27;
pub const ATTR_COMPLETE: u32 = 1 << 26;

pub const SETATTR_IN_LEN: usize = 88;
pub const CREATE_IN_LEN: usize = 16;
pub const WRITE_OUT_LEN: usize = 8;

pub mod fattr {
    pub const MODE: u32 = 1 << 0;
    pub const UID: u32 = 1 << 1;
    pub const GID: u32 = 1 << 2;
    pub const SIZE: u32 = 1 << 3;
    pub const ATIME: u32 = 1 << 4;
    pub const MTIME: u32 = 1 << 5;
    pub const FH: u32 = 1 << 6;
    pub const ATIME_NOW: u32 = 1 << 7;
    pub const MTIME_NOW: u32 = 1 << 8;
    pub const KILL_SUIDGID: u32 = 1 << 11;
}

pub const WRITE_CACHE: u32 = 1 << 0;
pub const WRITE_KILL_SUIDGID: u32 = 1 << 2;
pub const OPEN_KILL_SUIDGID: u32 = 1 << 0;
pub const FSYNC_FDATASYNC: u32 = 1 << 0;
pub const GETATTR_FH: u32 = 1 << 0;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct InHeader {
    pub len: u32,
    pub opcode: u32,
    pub unique: u64,
    pub nodeid: u64,
    pub uid: u32,
    pub gid: u32,
    pub pid: u32,
}

impl InHeader {
    pub fn parse(b: &[u8]) -> Option<Self> {
        let mut r = Reader::new(b.get(..IN_HEADER_LEN)?);
        Some(Self {
            len: r.u32()?,
            opcode: r.u32()?,
            unique: r.u64()?,
            nodeid: r.u64()?,
            uid: r.u32()?,
            gid: r.u32()?,
            pid: r.u32()?,
        })
    }
}

pub struct Reader<'a>(&'a [u8]);

impl<'a> Reader<'a> {
    pub fn new(b: &'a [u8]) -> Self {
        Self(b)
    }

    fn take<const N: usize>(&mut self) -> Option<[u8; N]> {
        let (head, rest) = self.0.split_first_chunk::<N>()?;
        self.0 = rest;
        Some(*head)
    }

    pub fn u32(&mut self) -> Option<u32> {
        self.take().map(u32::from_le_bytes)
    }

    pub fn u64(&mut self) -> Option<u64> {
        self.take().map(u64::from_le_bytes)
    }

    pub fn skip(&mut self, n: usize) -> Option<()> {
        self.0 = self.0.get(n..)?;
        Some(())
    }

    pub fn rest(&self) -> &'a [u8] {
        self.0
    }
}

#[derive(Default)]
pub struct Writer(pub Vec<u8>);

impl Writer {
    pub fn with_capacity(n: usize) -> Self {
        Self(Vec::with_capacity(n))
    }
    pub fn u16(&mut self, v: u16) -> &mut Self {
        self.0.extend_from_slice(&v.to_le_bytes());
        self
    }
    pub fn u32(&mut self, v: u32) -> &mut Self {
        self.0.extend_from_slice(&v.to_le_bytes());
        self
    }
    pub fn u64(&mut self, v: u64) -> &mut Self {
        self.0.extend_from_slice(&v.to_le_bytes());
        self
    }
    pub fn zeros(&mut self, n: usize) -> &mut Self {
        self.0.resize(self.0.len() + n, 0);
        self
    }
}

pub fn out_header(len: u32, error: i32, unique: u64) -> [u8; OUT_HEADER_LEN] {
    let mut h = [0u8; OUT_HEADER_LEN];
    h[0..4].copy_from_slice(&len.to_le_bytes());
    h[4..8].copy_from_slice(&error.to_le_bytes());
    h[8..16].copy_from_slice(&unique.to_le_bytes());
    h
}

pub fn name(b: &[u8]) -> Option<&[u8]> {
    let (last, name) = b.split_last()?;
    (*last == 0 && !name.is_empty() && !name.contains(&0)).then_some(name)
}

pub fn dirent_len(namelen: usize) -> usize {
    (DIRENT_LEN + namelen).next_multiple_of(8)
}
