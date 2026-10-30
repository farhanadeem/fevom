use std::io;
use std::os::fd::RawFd;
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Mark {
    Main,
    MemoryMapped,
    DevicesReady,
    KernelLoaded,
    GuestEntry,
    ConsoleFirst,
    Exit,
}

impl Mark {
    const COUNT: u32 = 7;

    pub fn name(self) -> &'static str {
        match self {
            Mark::Main => "vmm.main",
            Mark::MemoryMapped => "vmm.memory_mapped",
            Mark::DevicesReady => "vmm.devices_ready",
            Mark::KernelLoaded => "vmm.kernel_loaded",
            Mark::GuestEntry => "vmm.guest_entry",
            Mark::ConsoleFirst => "guest.console_first",
            Mark::Exit => "vmm.exit",
        }
    }
}

extern "C" {
    fn clock_gettime_nsec_np(clock_id: libc::clockid_t) -> u64;
}

pub fn now_ns() -> u64 {
    unsafe { clock_gettime_nsec_np(libc::CLOCK_UPTIME_RAW) }
}

pub fn is_open(fd: RawFd) -> bool {
    unsafe { libc::fcntl(fd, libc::F_GETFD) != -1 }
}

pub struct Trace {
    fd: Option<RawFd>,
    written: AtomicU32,
    broken: AtomicBool,
    writing: std::sync::Mutex<()>,
}

impl Trace {
    pub fn new(fd: Option<RawFd>) -> io::Result<Self> {
        if let Some(fd) = fd {
            if !is_open(fd) {
                return Err(io::Error::other("not an open descriptor"));
            }
        }
        const { assert!(Mark::COUNT <= u32::BITS) };
        Ok(Self {
            fd,
            written: AtomicU32::new(0),
            broken: AtomicBool::new(false),
            writing: std::sync::Mutex::new(()),
        })
    }

    pub fn mark(&self, mark: Mark) {
        if self.fd.is_some() {
            self.line(mark, now_ns(), None);
        }
    }

    pub fn main(&self, ns: u64, build: &str) {
        self.line(Mark::Main, ns, Some(("build", build)));
    }

    pub fn exit(&self, outcome: &str) {
        self.line(Mark::Exit, now_ns(), Some(("outcome", outcome)));
    }

    pub fn denied(&self, folder: &[u8]) {
        let Some(fd) = self.fd else { return };
        if self.broken.load(Ordering::Relaxed) {
            return;
        }
        let line = format!(
            "{{\"mark\":\"fs.denied\",\"ns\":{},\"path\":\"{}\"}}\n",
            now_ns(),
            escape(&String::from_utf8_lossy(folder))
        );
        self.write(fd, line.as_bytes());
    }

    fn line(&self, mark: Mark, ns: u64, extra: Option<(&str, &str)>) {
        let Some(fd) = self.fd else { return };
        let bit = 1u32 << mark as u32;
        if self.written.fetch_or(bit, Ordering::Relaxed) & bit != 0
            || self.broken.load(Ordering::Relaxed)
        {
            return;
        }
        let mut line = format!("{{\"mark\":\"{}\",\"ns\":{ns}", mark.name());
        if let Some((key, value)) = extra {
            line.push_str(&format!(",\"{key}\":\"{}\"", escape(value)));
        }
        line.push_str("}\n");
        self.write(fd, line.as_bytes());
    }

    fn write(&self, fd: RawFd, bytes: &[u8]) {
        let _one = self.writing.lock().unwrap_or_else(|e| e.into_inner());
        let n = loop {
            let n = unsafe { libc::write(fd, bytes.as_ptr().cast(), bytes.len()) };
            if n >= 0 || io::Error::last_os_error().kind() != io::ErrorKind::Interrupted {
                break n;
            }
        };
        if n != bytes.len() as isize {
            self.broken.store(true, Ordering::Relaxed);
        }
    }
}

fn escape(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs::File;
    use std::io::Read;
    use std::os::fd::{AsRawFd, FromRawFd};

    fn pipe() -> (File, File) {
        let mut fds = [0; 2];
        assert_eq!(unsafe { libc::pipe(fds.as_mut_ptr()) }, 0);
        unsafe { (File::from_raw_fd(fds[0]), File::from_raw_fd(fds[1])) }
    }

    #[test]
    fn a_denied_folder_is_one_whole_line_with_its_path_escaped() {
        let (mut r, w) = pipe();
        let trace = Trace::new(Some(w.as_raw_fd())).unwrap();
        trace.denied(b"/Users/some\"one/Documents");
        trace.denied(&[b'/', 0xff, b'x']);
        drop(w);
        drop(trace);
        let mut text = String::new();
        r.read_to_string(&mut text).unwrap();
        let lines: Vec<&str> = text.lines().collect();
        assert_eq!(lines.len(), 2, "{text}");
        assert!(
            lines[0].starts_with("{\"mark\":\"fs.denied\",\"ns\":"),
            "{text}"
        );
        assert!(
            lines[0].ends_with(",\"path\":\"/Users/some\\\"one/Documents\"}"),
            "{text}"
        );
        assert!(lines[1].contains("\"path\":\"/\u{fffd}x\"}"), "{text}");
        Trace::new(None).unwrap().denied(b"/x");
    }

    #[test]
    fn writes_one_json_line_per_mark_on_the_uptime_clock() {
        let (mut r, w) = pipe();
        let before = now_ns();
        let trace = Trace::new(Some(w.as_raw_fd())).unwrap();
        trace.main(before, "v1-2-gabc\"def");
        trace.mark(Mark::MemoryMapped);
        trace.mark(Mark::GuestEntry);
        trace.exit("power_off");
        let after = now_ns();
        drop(w);
        let mut text = String::new();
        r.read_to_string(&mut text).unwrap();
        let lines: Vec<&str> = text.lines().collect();
        assert_eq!(
            lines[0],
            format!("{{\"mark\":\"vmm.main\",\"ns\":{before},\"build\":\"v1-2-gabc\\\"def\"}}")
        );
        assert!(lines[1].starts_with("{\"mark\":\"vmm.memory_mapped\",\"ns\":"));
        assert!(lines[2].starts_with("{\"mark\":\"vmm.guest_entry\",\"ns\":"));
        assert!(lines[3].ends_with(",\"outcome\":\"power_off\"}"));
        assert_eq!(lines.len(), 4);
        let mut last = 0;
        for line in lines {
            let rest = line.split("\"ns\":").nth(1).unwrap();
            let ns: u64 = rest[..rest.find([',', '}']).unwrap()].parse().unwrap();
            assert!(ns >= before && ns <= after && ns >= last, "{line}");
            last = ns;
        }
    }

    #[test]
    fn a_mark_is_written_once() {
        let (mut r, w) = pipe();
        let trace = Trace::new(Some(w.as_raw_fd())).unwrap();
        trace.mark(Mark::ConsoleFirst);
        trace.mark(Mark::ConsoleFirst);
        drop(w);
        let mut text = String::new();
        r.read_to_string(&mut text).unwrap();
        assert_eq!(text.lines().count(), 1);
    }

    #[test]
    fn a_reader_that_went_away_is_not_an_error() {
        let (r, w) = pipe();
        let trace = Trace::new(Some(w.as_raw_fd())).unwrap();
        drop(r);
        trace.mark(Mark::MemoryMapped);
        trace.mark(Mark::GuestEntry);
        assert!(trace.broken.load(Ordering::Relaxed));
    }

    #[test]
    fn no_descriptor_means_no_work_and_a_closed_one_is_refused() {
        let trace = Trace::new(None).unwrap();
        trace.mark(Mark::GuestEntry);
        trace.exit("stopped");
        let mut limit = libc::rlimit {
            rlim_cur: 0,
            rlim_max: 0,
        };
        unsafe { libc::getrlimit(libc::RLIMIT_NOFILE, &mut limit) };
        let fd = limit.rlim_cur.min(i32::MAX as u64 - 1) as RawFd;
        assert!(Trace::new(Some(fd)).is_err());
    }
}
