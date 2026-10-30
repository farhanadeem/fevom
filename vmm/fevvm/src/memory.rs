use std::fmt;
use std::io;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct OutOfRange {
    pub addr: u64,
    pub len: u64,
}

impl fmt::Display for OutOfRange {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "guest range {:#x}+{:#x} is outside RAM",
            self.addr, self.len
        )
    }
}

impl std::error::Error for OutOfRange {}

pub struct GuestMemory {
    host: *mut u8,
    base: u64,
    len: u64,
}

unsafe impl Send for GuestMemory {}
unsafe impl Sync for GuestMemory {}

impl GuestMemory {
    pub fn new(base: u64, len: u64) -> io::Result<Self> {
        let host = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                len as usize,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_PRIVATE | libc::MAP_ANON,
                -1,
                0,
            )
        };
        if host == libc::MAP_FAILED {
            return Err(io::Error::last_os_error());
        }
        Ok(Self {
            host: host.cast(),
            base,
            len,
        })
    }

    pub fn base(&self) -> u64 {
        self.base
    }

    pub fn len(&self) -> u64 {
        self.len
    }

    pub fn is_empty(&self) -> bool {
        self.len == 0
    }

    pub fn end(&self) -> u64 {
        self.base + self.len
    }

    pub fn host_base(&self) -> *mut u8 {
        self.host
    }

    pub fn host_range(&self, addr: u64, len: u64) -> Result<*mut u8, OutOfRange> {
        let err = OutOfRange { addr, len };
        let offset = addr.checked_sub(self.base).ok_or(err)?;
        let end = offset.checked_add(len).ok_or(err)?;
        if end > self.len {
            return Err(err);
        }
        Ok(unsafe { self.host.add(offset as usize) })
    }

    pub fn read(&self, addr: u64, buf: &mut [u8]) -> Result<(), OutOfRange> {
        let src = self.host_range(addr, buf.len() as u64)?;
        unsafe { std::ptr::copy_nonoverlapping(src, buf.as_mut_ptr(), buf.len()) };
        Ok(())
    }

    pub fn write(&self, addr: u64, buf: &[u8]) -> Result<(), OutOfRange> {
        let dst = self.host_range(addr, buf.len() as u64)?;
        unsafe { std::ptr::copy_nonoverlapping(buf.as_ptr(), dst, buf.len()) };
        Ok(())
    }

    fn read_scalar<const N: usize>(&self, addr: u64) -> Result<[u8; N], OutOfRange> {
        let p = self.host_range(addr, N as u64)?;
        Ok(unsafe { p.cast::<[u8; N]>().read_volatile() })
    }

    fn write_scalar<const N: usize>(&self, addr: u64, bytes: [u8; N]) -> Result<(), OutOfRange> {
        let p = self.host_range(addr, N as u64)?;
        unsafe { p.cast::<[u8; N]>().write_volatile(bytes) };
        Ok(())
    }

    pub fn read_u16(&self, addr: u64) -> Result<u16, OutOfRange> {
        self.read_scalar(addr).map(u16::from_le_bytes)
    }

    pub fn read_u32(&self, addr: u64) -> Result<u32, OutOfRange> {
        self.read_scalar(addr).map(u32::from_le_bytes)
    }

    pub fn read_u64(&self, addr: u64) -> Result<u64, OutOfRange> {
        self.read_scalar(addr).map(u64::from_le_bytes)
    }

    pub fn write_u16(&self, addr: u64, value: u16) -> Result<(), OutOfRange> {
        self.write_scalar(addr, value.to_le_bytes())
    }

    pub fn write_u32(&self, addr: u64, value: u32) -> Result<(), OutOfRange> {
        self.write_scalar(addr, value.to_le_bytes())
    }

    pub fn write_u64(&self, addr: u64, value: u64) -> Result<(), OutOfRange> {
        self.write_scalar(addr, value.to_le_bytes())
    }

    pub fn read_from_file(
        &self,
        file: &std::fs::File,
        offset: u64,
        addr: u64,
        len: u64,
    ) -> Result<bool, OutOfRange> {
        Ok(self.file_io(file, offset, addr, len, false)?.is_ok())
    }

    pub fn write_to_file(
        &self,
        file: &std::fs::File,
        offset: u64,
        addr: u64,
        len: u64,
    ) -> Result<bool, OutOfRange> {
        Ok(self.file_io(file, offset, addr, len, true)?.is_ok())
    }

    pub fn write_to_file_or(
        &self,
        file: &std::fs::File,
        offset: u64,
        addr: u64,
        len: u64,
    ) -> Result<Result<(), i32>, OutOfRange> {
        self.file_io(file, offset, addr, len, true)
    }

    fn file_io(
        &self,
        file: &std::fs::File,
        mut offset: u64,
        addr: u64,
        len: u64,
        write: bool,
    ) -> Result<Result<(), i32>, OutOfRange> {
        use std::os::fd::AsRawFd;
        let host = self.host_range(addr, len)?;
        let fd = file.as_raw_fd();
        let mut done = 0usize;
        while done < len as usize {
            let (p, n) = (unsafe { host.add(done) }, len as usize - done);
            let ret = unsafe {
                if write {
                    libc::pwrite(fd, p.cast(), n, offset as libc::off_t)
                } else {
                    libc::pread(fd, p.cast(), n, offset as libc::off_t)
                }
            };
            match ret {
                0 => return Ok(Err(if write { libc::ENOSPC } else { 0 })),
                r if r < 0 => {
                    let err = std::io::Error::last_os_error();
                    if err.kind() != std::io::ErrorKind::Interrupted {
                        return Ok(Err(err.raw_os_error().unwrap_or(libc::EIO)));
                    }
                }
                r => {
                    done += r as usize;
                    offset += r as u64;
                }
            }
        }
        Ok(Ok(()))
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct HostUsage {
    pub footprint: u64,
    pub resident: u64,
    pub compressed: u64,
}

impl GuestMemory {
    pub fn page_states(&self) -> std::io::Result<Vec<u8>> {
        let pages = (self.len / 16384) as usize;
        let mut vec = vec![0u8; pages];
        let ret =
            unsafe { libc::mincore(self.host.cast(), self.len as usize, vec.as_mut_ptr().cast()) };
        if ret != 0 {
            return Err(std::io::Error::last_os_error());
        }
        Ok(vec)
    }

    pub fn host_usage(&self) -> HostUsage {
        extern "C" {
            static mach_task_self_: u32;
            fn mach_vm_region(
                task: u32,
                address: *mut u64,
                size: *mut u64,
                flavor: i32,
                info: *mut u32,
                count: *mut u32,
                object_name: *mut u32,
            ) -> i32;
            fn proc_pid_rusage(pid: i32, flavor: i32, buffer: *mut libc::c_void) -> i32;
        }
        const VM_REGION_EXTENDED_INFO: i32 = 13;
        const HOST_PAGE: u64 = 16384;

        let mut rusage = [0u64; 12];
        let footprint =
            match unsafe { proc_pid_rusage(libc::getpid(), 0, rusage.as_mut_ptr().cast()) } {
                0 => rusage[2 + 7],
                _ => 0,
            };

        let end = self.host as u64 + self.len;
        let mut cursor = self.host as u64;
        let (mut resident, mut compressed) = (0u64, 0u64);
        while cursor < end {
            let mut size = 0u64;
            let mut info = [0u32; 16];
            let mut count = 9u32;
            let mut name = 0u32;
            let ret = unsafe {
                mach_vm_region(
                    mach_task_self_,
                    &mut cursor,
                    &mut size,
                    VM_REGION_EXTENDED_INFO,
                    info.as_mut_ptr(),
                    &mut count,
                    &mut name,
                )
            };
            if ret != 0 || cursor >= end || size == 0 {
                break;
            }
            resident += info[2] as u64 * HOST_PAGE;
            compressed += info[4] as u64 * HOST_PAGE;
            cursor += size;
        }
        HostUsage {
            footprint,
            resident,
            compressed,
        }
    }
}

impl GuestMemory {
    pub fn drop_host_translations(&self) -> bool {
        extern "C" {
            static mach_task_self_: u32;
            #[allow(clippy::too_many_arguments)]
            fn mach_vm_remap(
                target: u32,
                address: *mut u64,
                size: u64,
                mask: u64,
                flags: i32,
                src: u32,
                src_address: u64,
                copy: i32,
                cur_protection: *mut i32,
                max_protection: *mut i32,
                inheritance: u32,
            ) -> i32;
        }
        const VM_FLAGS_FIXED: i32 = 0;
        const VM_FLAGS_OVERWRITE: i32 = 0x4000;
        const VM_INHERIT_COPY: u32 = 1;
        if self.len == 0 {
            return true;
        }
        let mut addr = self.host as u64;
        let (mut cur, mut max) = (0i32, 0i32);
        let ret = unsafe {
            mach_vm_remap(
                mach_task_self_,
                &mut addr,
                self.len,
                0,
                VM_FLAGS_FIXED | VM_FLAGS_OVERWRITE,
                mach_task_self_,
                self.host as u64,
                0,
                &mut cur,
                &mut max,
                VM_INHERIT_COPY,
            )
        };
        ret == 0 && addr == self.host as u64
    }
}

impl Drop for GuestMemory {
    fn drop(&mut self) {
        unsafe { libc::munmap(self.host.cast(), self.len as usize) };
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    static FOOTPRINT: std::sync::Mutex<()> = std::sync::Mutex::new(());

    fn footprint_alone() -> std::sync::MutexGuard<'static, ()> {
        FOOTPRINT.lock().unwrap_or_else(|e| e.into_inner())
    }

    #[test]
    fn an_address_in_the_disk_window_is_not_guest_memory() {
        use crate::layout::{disk_window, RAM_BASE};
        let ram = 64u64 << 20;
        let mem = GuestMemory::new(RAM_BASE, ram).unwrap();
        let file = tempfile();
        for bits in [40, 36] {
            let (base, len) = disk_window(bits, mem.end(), 1 << 40).unwrap();
            for addr in [base, base + 4096, base + len - 4096] {
                let mut buf = [0u8; 16];
                assert!(mem.host_range(addr, 16).is_err(), "{bits} bits, {addr:#x}");
                assert!(mem.read(addr, &mut buf).is_err());
                assert!(mem.write(addr, &buf).is_err());
                assert!(mem.read_u16(addr).is_err() && mem.read_u32(addr).is_err());
                assert!(mem.read_u64(addr).is_err() && mem.write_u64(addr, 1).is_err());
                assert!(mem.write_u16(addr, 1).is_err() && mem.write_u32(addr, 1).is_err());
                assert!(mem.read_from_file(&file, 0, addr, 16).is_err());
                assert!(mem.write_to_file(&file, 0, addr, 16).is_err());
            }
            assert!(mem
                .host_range(mem.end() - 8, base - mem.end() + 16)
                .is_err());
            assert!(mem.host_range(mem.end() - 8, 16).is_err());
        }
        let whole = GuestMemory::new(RAM_BASE, 1 << 30).unwrap();
        let (base, _) = disk_window(36, whole.end(), 1 << 40).unwrap();
        assert_eq!(base, whole.end());
        assert!(whole.host_range(base, 1).is_err());
        assert!(whole.host_range(base - 1, 2).is_err());
        assert!(whole.host_range(base - 1, 1).is_ok());
    }

    fn tempfile() -> std::fs::File {
        let path = std::env::temp_dir().join(format!("fev-mem-window-{}", std::process::id()));
        let f = std::fs::File::options()
            .read(true)
            .write(true)
            .create(true)
            .truncate(true)
            .open(&path)
            .unwrap();
        let _ = std::fs::remove_file(&path);
        f.set_len(4096).unwrap();
        f
    }

    #[test]
    fn rejects_ranges_outside_ram() {
        let mem = GuestMemory::new(0x4000_0000, 0x10000).unwrap();
        assert!(mem.host_range(0x4000_0000, 0x10000).is_ok());
        assert!(mem.host_range(0x4000_0000, 0x10001).is_err());
        assert!(mem.host_range(0x3fff_ffff, 1).is_err());
        assert!(mem.host_range(0x4001_0000, 1).is_err());
        assert!(mem.host_range(0x4001_0000, 0).is_ok());
        assert!(mem.host_range(u64::MAX, 2).is_err());
        assert!(mem.host_range(0x4000_0001, u64::MAX).is_err());
    }

    #[test]
    fn memory_costs_nothing_until_touched() {
        let _alone = footprint_alone();
        const LEN: u64 = 256 << 20;
        let mem = GuestMemory::new(0x4000_0000, LEN).unwrap();
        let before = mem.host_usage();
        assert!(
            before.resident < 1 << 20,
            "untouched RAM is resident: {before:?}"
        );
        assert!(before.footprint > 0);

        for off in (0..64u64 << 20).step_by(16384) {
            mem.write(0x4000_0000 + off, &[1]).unwrap();
        }
        let after = mem.host_usage();
        let grew = after.resident + after.compressed - before.resident - before.compressed;
        assert!(
            (60 << 20..=68 << 20).contains(&grew),
            "touching 64 MiB grew guest RAM by {grew}"
        );
        assert!(
            after.footprint >= before.footprint + (56 << 20),
            "{before:?} -> {after:?}"
        );
    }

    #[test]
    fn dropping_our_translations_takes_them_off_the_footprint_and_keeps_the_memory() {
        let _alone = footprint_alone();
        const LEN: u64 = 64 << 20;
        let mem = GuestMemory::new(0x4000_0000, LEN).unwrap();
        for off in (0..LEN).step_by(16384) {
            mem.write(0x4000_0000 + off, &[(off >> 14) as u8]).unwrap();
        }
        let before = mem.host_usage();
        assert!(mem.drop_host_translations());
        let after = mem.host_usage();
        assert!(
            after.footprint + (56 << 20) <= before.footprint,
            "{before:?} -> {after:?}"
        );
        assert!(
            after.resident + after.compressed + (4 << 20) >= before.resident + before.compressed,
            "{before:?} -> {after:?}"
        );
        for off in (0..LEN).step_by(16384) {
            let mut b = [0u8];
            mem.read(0x4000_0000 + off, &mut b).unwrap();
            assert_eq!(b[0], (off >> 14) as u8, "at {off:#x}");
        }
        mem.write_u64(0x4000_0000 + LEN - 8, 0x1122_3344_5566_7788)
            .unwrap();
        assert!(mem.drop_host_translations(), "a second time");
        assert_eq!(
            mem.read_u64(0x4000_0000 + LEN - 8).unwrap(),
            0x1122_3344_5566_7788
        );
    }

    #[test]
    fn threads_using_the_memory_never_see_it_go() {
        let _alone = footprint_alone();
        use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
        use std::sync::Arc;
        const LEN: u64 = 16 << 20;
        let mem = Arc::new(GuestMemory::new(0x4000_0000, LEN).unwrap());
        let stop = Arc::new(AtomicBool::new(false));
        let passes: Arc<[AtomicU64; 3]> = Arc::new(Default::default());
        let users: Vec<_> = (0..3u64)
            .map(|t| {
                let (mem, stop, passes) = (mem.clone(), stop.clone(), passes.clone());
                std::thread::spawn(move || {
                    let mut rounds = 0u64;
                    while !stop.load(Ordering::Relaxed) {
                        for page in (t..LEN / 16384).step_by(3) {
                            let addr = 0x4000_0000 + page * 16384;
                            let seen = mem.read_u64(addr).unwrap();
                            assert_eq!(seen, rounds, "page {page} lost a write");
                            mem.write_u64(addr, rounds + 1).unwrap();
                        }
                        rounds += 1;
                        passes[t as usize].store(rounds, Ordering::SeqCst);
                    }
                    rounds
                })
            })
            .collect();
        assert!(mem.drop_host_translations());
        let from: Vec<u64> = passes.iter().map(|p| p.load(Ordering::SeqCst)).collect();
        let mut drops = 1;
        while drops < 500
            || passes
                .iter()
                .zip(&from)
                .any(|(p, f)| p.load(Ordering::SeqCst) < f + 2)
        {
            if users.iter().any(|u| u.is_finished()) {
                break;
            }
            assert!(mem.drop_host_translations());
            drops += 1;
        }
        assert!(mem.drop_host_translations());
        stop.store(true, Ordering::Relaxed);
        for (u, f) in users.into_iter().zip(from) {
            assert!(u.join().expect("a user of the memory faulted") >= f + 2);
        }
    }

    #[test]
    fn round_trips_little_endian() {
        let mem = GuestMemory::new(0x1000, 0x4000).unwrap();
        mem.write_u32(0x1ffe, 0xdead_beef).unwrap();
        assert_eq!(mem.read_u32(0x1ffe).unwrap(), 0xdead_beef);
        assert_eq!(mem.read_u16(0x1ffe).unwrap(), 0xbeef);
        assert_eq!(mem.read_u64(0x1ffe).unwrap(), 0xdead_beef);
        mem.write_u64(0x1001, 0x0102_0304_0506_0708).unwrap();
        assert_eq!(mem.read_u64(0x1001).unwrap(), 0x0102_0304_0506_0708);
        let mut raw = [0u8; 2];
        mem.read(0x1001, &mut raw).unwrap();
        assert_eq!(raw, [0x08, 0x07]);
    }

    #[test]
    fn file_io_goes_straight_to_guest_memory() {
        use std::os::unix::fs::FileExt;
        let path = std::env::temp_dir().join(format!("fev-mem-{}.bin", std::process::id()));
        let file = std::fs::File::options()
            .read(true)
            .write(true)
            .create(true)
            .truncate(true)
            .open(&path)
            .unwrap();
        file.write_all_at(b"0123456789", 0).unwrap();
        let mem = GuestMemory::new(0x1000, 0x4000).unwrap();
        assert_eq!(mem.read_from_file(&file, 2, 0x1100, 6), Ok(true));
        let mut got = [0u8; 6];
        mem.read(0x1100, &mut got).unwrap();
        assert_eq!(&got, b"234567");
        assert_eq!(mem.read_from_file(&file, 8, 0x1100, 6), Ok(false));
        assert!(mem.read_from_file(&file, 0, 0x4ffe, 6).is_err());
        assert_eq!(mem.write_to_file(&file, 10, 0x1100, 6), Ok(true));
        let mut back = [0u8; 16];
        file.read_exact_at(&mut back, 0).unwrap();
        assert_eq!(&back, b"0123456789894567");
        std::fs::remove_file(path).unwrap();
    }
}
