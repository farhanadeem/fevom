use std::os::fd::RawFd;
use std::time::Instant;

use super::*;

pub(super) struct Bulk {
    pub(super) since: u64,
    pub(super) taken: Instant,
    pub(super) st: Vec<Option<libc::stat>>,
}

const MAX_BULK: usize = 1 << 16;

const ATTR_CMN_RETURNED_ATTRS: u32 = 0x8000_0000;
const ATTR_CMN_DEVID: u32 = 0x0000_0002;
const ATTR_CMN_OBJTYPE: u32 = 0x0000_0008;
const ATTR_CMN_MODTIME: u32 = 0x0000_0400;
const ATTR_CMN_CHGTIME: u32 = 0x0000_0800;
const ATTR_CMN_ACCTIME: u32 = 0x0000_1000;
const ATTR_CMN_OWNERID: u32 = 0x0000_8000;
const ATTR_CMN_GRPID: u32 = 0x0001_0000;
const ATTR_CMN_ACCESSMASK: u32 = 0x0002_0000;
const ATTR_CMN_FLAGS: u32 = 0x0004_0000;
const ATTR_CMN_FILEID: u32 = 0x0200_0000;
const ATTR_FILE_LINKCOUNT: u32 = 0x0000_0001;
const ATTR_FILE_ALLOCSIZE: u32 = 0x0000_0004;
const ATTR_FILE_DATALENGTH: u32 = 0x0000_0200;
const FSOPT_PACK_INVAL_ATTRS: u64 = 0x0000_0008;
const UF_COMPRESSED: u32 = 0x0000_0020;

const COMMON: u32 = ATTR_CMN_RETURNED_ATTRS
    | ATTR_CMN_NAME
    | ATTR_CMN_DEVID
    | ATTR_CMN_OBJTYPE
    | ATTR_CMN_MODTIME
    | ATTR_CMN_CHGTIME
    | ATTR_CMN_ACCTIME
    | ATTR_CMN_OWNERID
    | ATTR_CMN_GRPID
    | ATTR_CMN_ACCESSMASK
    | ATTR_CMN_FLAGS
    | ATTR_CMN_FILEID;
const FILE: u32 = ATTR_FILE_LINKCOUNT | ATTR_FILE_ALLOCSIZE | ATTR_FILE_DATALENGTH;

fn d_type(vtype: u32) -> u8 {
    match vtype {
        1 => libc::DT_REG,
        2 => libc::DT_DIR,
        3 => libc::DT_BLK,
        4 => libc::DT_CHR,
        5 => libc::DT_LNK,
        6 => libc::DT_SOCK,
        7 => libc::DT_FIFO,
        _ => libc::DT_UNKNOWN,
    }
}

mod at {
    pub const RETURNED_COMMON: usize = 4;
    pub const RETURNED_FILE: usize = 16;
    pub const NAME: usize = 24;
    pub const DEVID: usize = 32;
    pub const OBJTYPE: usize = 36;
    pub const MODTIME: usize = 40;
    pub const CHGTIME: usize = 56;
    pub const ACCTIME: usize = 72;
    pub const OWNERID: usize = 88;
    pub const GRPID: usize = 92;
    pub const ACCESSMASK: usize = 96;
    pub const FLAGS: usize = 100;
    pub const FILEID: usize = 104;
    pub const LINKCOUNT: usize = 112;
    pub const ALLOCSIZE: usize = 116;
    pub const DATALENGTH: usize = 124;
    pub const FIXED: usize = 132;
}

impl Passthrough {
    pub(super) fn list_bulk(
        fd: RawFd,
        room: usize,
        dir: &libc::stat,
        since: u64,
    ) -> Option<Result<(Vec<DirEnt>, Bulk)>> {
        let (dev, blksize) = (dir.st_dev, dir.st_blksize);
        let list = AttrList {
            bitmapcount: ATTR_BIT_MAP_COUNT,
            reserved: 0,
            commonattr: COMMON,
            volattr: 0,
            dirattr: 0,
            fileattr: FILE,
            forkattr: 0,
        };
        let dup = unsafe { libc::fcntl(fd, libc::F_DUPFD_CLOEXEC, 0) };
        if dup < 0 {
            return Some(Err(Errno::last()));
        }
        let dup = unsafe { OwnedFd::from_raw_fd(dup) };
        if unsafe { libc::lseek(dup.as_raw_fd(), 0, libc::SEEK_SET) } < 0 {
            return None;
        }
        let taken = Instant::now();
        let mut entries = Vec::new();
        let mut st: Vec<Option<libc::stat>> = Vec::new();
        for name in [&b"."[..], b".."] {
            let ino = if name == b"." {
                dir.st_ino
            } else {
                let mut up = unsafe { std::mem::zeroed::<libc::stat>() };
                match unsafe { libc::fstatat(fd, c"..".as_ptr(), &mut up, 0) } {
                    0 => up.st_ino,
                    _ => dir.st_ino,
                }
            };
            entries.push(DirEnt {
                name: name.into(),
                ino,
                kind: libc::DT_DIR,
            });
            st.push(None);
        }
        let mut buf = vec![0u8; 64 << 10];
        loop {
            let got = unsafe {
                libc::getattrlistbulk(
                    dup.as_raw_fd(),
                    (&list as *const AttrList).cast_mut().cast(),
                    buf.as_mut_ptr().cast(),
                    buf.len(),
                    FSOPT_PACK_INVAL_ATTRS,
                )
            };
            if got < 0 {
                if std::io::Error::last_os_error().kind() == std::io::ErrorKind::Interrupted {
                    continue;
                }
                return if entries.len() == 2 {
                    None
                } else {
                    Some(Err(Errno::last()))
                };
            }
            if got == 0 {
                break;
            }
            let mut rest = &buf[..];
            for _ in 0..got {
                let u32_at = |b: &[u8], i: usize| -> Option<u32> {
                    Some(u32::from_le_bytes(b.get(i..i + 4)?.try_into().ok()?))
                };
                let i64_at = |b: &[u8], i: usize| -> Option<i64> {
                    Some(i64::from_le_bytes(b.get(i..i + 8)?.try_into().ok()?))
                };
                let bad = || Some(Err(Errno::EIO));
                let Some(len) = u32_at(rest, 0).map(|l| l as usize) else {
                    return bad();
                };
                if len < at::LINKCOUNT || len > rest.len() {
                    return bad();
                }
                let (e, later) = rest.split_at(len);
                rest = later;
                let field = |i: usize| u32_at(e, i).unwrap_or(0);
                let common = field(at::RETURNED_COMMON);
                if common & ATTR_CMN_NAME == 0 {
                    return bad();
                }
                let (off, nlen) = (field(at::NAME) as i32, field(at::NAME + 4) as usize);
                let start = at::NAME as i64 + off as i64;
                let Some(name) = usize::try_from(start)
                    .ok()
                    .and_then(|s| e.get(s..s.checked_add(nlen)?))
                    .and_then(|n| n.split(|b| *b == 0).next())
                else {
                    return bad();
                };
                if name.is_empty() || name.len() > NAME_MAX {
                    continue;
                }
                if entries.len() >= room {
                    return Some(Err(Errno::ENOMEM));
                }
                let vtype = field(at::OBJTYPE);
                let ino = i64_at(e, at::FILEID).unwrap_or(0) as u64;
                let want = COMMON & !ATTR_CMN_RETURNED_ATTRS;
                let whole = len >= at::FIXED
                    && common & want == want
                    && field(at::RETURNED_FILE) & FILE == FILE
                    && common & ATTR_CMN_OBJTYPE != 0
                    && vtype == 1
                    && field(at::DEVID) as i32 == dev
                    && field(at::FLAGS) & (UF_COMPRESSED | SF_DATALESS) == 0
                    && st.len() < MAX_BULK;
                st.push(whole.then(|| {
                    let mut s = unsafe { std::mem::zeroed::<libc::stat>() };
                    let time =
                        |i: usize| (i64_at(e, i).unwrap_or(0), i64_at(e, i + 8).unwrap_or(0));
                    s.st_dev = dev;
                    s.st_ino = ino;
                    s.st_mode = (S_IFREG | (field(at::ACCESSMASK) & 0o7777)) as libc::mode_t;
                    s.st_nlink = field(at::LINKCOUNT) as libc::nlink_t;
                    s.st_uid = field(at::OWNERID);
                    s.st_gid = field(at::GRPID);
                    (s.st_mtime, s.st_mtime_nsec) = time(at::MODTIME);
                    (s.st_ctime, s.st_ctime_nsec) = time(at::CHGTIME);
                    (s.st_atime, s.st_atime_nsec) = time(at::ACCTIME);
                    s.st_size = i64_at(e, at::DATALENGTH).unwrap_or(0);
                    s.st_blocks = i64_at(e, at::ALLOCSIZE).unwrap_or(0) / 512;
                    s.st_blksize = blksize;
                    s.st_flags = field(at::FLAGS);
                    s
                }));
                entries.push(DirEnt {
                    name: name.into(),
                    ino,
                    kind: d_type(vtype),
                });
            }
        }
        Some(Ok((entries, Bulk { since, taken, st })))
    }
}
