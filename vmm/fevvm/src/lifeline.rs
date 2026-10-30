use std::io;
use std::os::fd::RawFd;
use std::thread::JoinHandle;

use crate::trace::is_open;

pub fn watch(fd: RawFd, on_eof: impl FnOnce() + Send + 'static) -> io::Result<JoinHandle<()>> {
    if !is_open(fd) {
        return Err(io::Error::other("not an open descriptor"));
    }
    std::thread::Builder::new()
        .name("lifeline".into())
        .spawn(move || {
            let mut buf = [0u8; 64];
            loop {
                let n = unsafe { libc::read(fd, buf.as_mut_ptr().cast(), buf.len()) };
                if n > 0 {
                    continue;
                }
                if n == 0 {
                    break;
                }
                match io::Error::last_os_error().kind() {
                    io::ErrorKind::Interrupted => continue,
                    io::ErrorKind::WouldBlock => {
                        let mut p = libc::pollfd {
                            fd,
                            events: libc::POLLIN,
                            revents: 0,
                        };
                        let r = unsafe { libc::poll(&mut p, 1, -1) };
                        if r > 0 || io::Error::last_os_error().kind() == io::ErrorKind::Interrupted
                        {
                            continue;
                        }
                        break;
                    }
                    _ => break,
                }
            }
            on_eof();
        })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs::File;
    use std::io::Write;
    use std::os::fd::{AsRawFd, FromRawFd};
    use std::sync::mpsc;
    use std::time::Duration;

    fn pipe() -> (File, File) {
        let mut fds = [0; 2];
        assert_eq!(unsafe { libc::pipe(fds.as_mut_ptr()) }, 0);
        unsafe { (File::from_raw_fd(fds[0]), File::from_raw_fd(fds[1])) }
    }

    #[test]
    fn fires_at_end_of_file_and_not_on_data() {
        let (r, mut w) = pipe();
        let (tx, rx) = mpsc::channel();
        let t = watch(r.as_raw_fd(), move || tx.send(()).unwrap()).unwrap();
        w.write_all(b"still here").unwrap();
        assert!(
            rx.recv_timeout(Duration::from_millis(100)).is_err(),
            "data on the lifeline was taken for its end"
        );
        drop(w);
        rx.recv_timeout(Duration::from_secs(5))
            .expect("closing the lifeline did nothing");
        t.join().unwrap();
    }

    #[test]
    fn a_non_blocking_descriptor_with_nothing_to_read_is_not_at_its_end() {
        let (r, w) = pipe();
        let flags = unsafe { libc::fcntl(r.as_raw_fd(), libc::F_GETFL) };
        assert_eq!(
            unsafe { libc::fcntl(r.as_raw_fd(), libc::F_SETFL, flags | libc::O_NONBLOCK) },
            0
        );
        let (tx, rx) = mpsc::channel();
        let t = watch(r.as_raw_fd(), move || tx.send(()).unwrap()).unwrap();
        assert!(
            rx.recv_timeout(Duration::from_millis(100)).is_err(),
            "an empty non-blocking lifeline was taken for a closed one"
        );
        drop(w);
        rx.recv_timeout(Duration::from_secs(5))
            .expect("closing the lifeline did nothing");
        t.join().unwrap();
    }

    #[test]
    fn a_holder_that_is_already_gone_fires_at_once() {
        let (r, w) = pipe();
        drop(w);
        let (tx, rx) = mpsc::channel();
        watch(r.as_raw_fd(), move || tx.send(()).unwrap())
            .unwrap()
            .join()
            .unwrap();
        rx.try_recv().unwrap();
    }

    #[test]
    fn a_closed_descriptor_is_refused() {
        let mut limit = libc::rlimit {
            rlim_cur: 0,
            rlim_max: 0,
        };
        assert_eq!(
            unsafe { libc::getrlimit(libc::RLIMIT_NOFILE, &mut limit) },
            0
        );
        let fd = limit.rlim_cur.min(i32::MAX as u64 - 1) as RawFd;
        assert!(watch(fd, || {}).is_err());
    }
}
