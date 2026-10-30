use std::collections::VecDeque;
use std::io::Write;
use std::sync::{Arc, Condvar, Mutex};
use std::time::{Duration, Instant};

pub const MAX_BUFFERED: usize = 1 << 20;
const WRITE_CHUNK: usize = 64 << 10;

struct State {
    buf: VecDeque<u8>,
    stamp_from: Option<Instant>,
    at_line_start: bool,
    dropped: u64,
    writing: bool,
    closed: bool,
    on_first: Option<Box<dyn FnOnce() + Send>>,
}

struct Shared {
    state: Mutex<State>,
    ready: Condvar,
    drained: Condvar,
}

pub struct ConsoleOut {
    shared: Arc<Shared>,
}

impl ConsoleOut {
    pub fn new(out: Box<dyn Write + Send>, stamp_from: Option<Instant>) -> Self {
        let shared = Arc::new(Shared {
            state: Mutex::new(State {
                buf: VecDeque::new(),
                stamp_from,
                at_line_start: true,
                dropped: 0,
                writing: false,
                closed: false,
                on_first: None,
            }),
            ready: Condvar::new(),
            drained: Condvar::new(),
        });
        let for_writer = shared.clone();
        std::thread::Builder::new()
            .name("console-out".into())
            .spawn(move || run_writer(&for_writer, out))
            .expect("spawn console writer");
        Self { shared }
    }

    pub fn on_first_write(&self, f: impl FnOnce() + Send + 'static) {
        self.shared.state.lock().unwrap().on_first = Some(Box::new(f));
    }

    pub fn write(&self, data: &[u8]) {
        let mut state = self.shared.state.lock().unwrap();
        let first = match data.is_empty() {
            false => state.on_first.take(),
            true => None,
        };
        if let Some(first) = first {
            drop(state);
            first();
            state = self.shared.state.lock().unwrap();
        }
        state.queue(data);
        drop(state);
        self.shared.ready.notify_one();
    }

    pub fn flush(&self, limit: Duration) -> bool {
        let state = self.shared.state.lock().unwrap();
        let (state, _) = self
            .shared
            .drained
            .wait_timeout_while(state, limit, |s| !s.buf.is_empty() || s.writing)
            .unwrap();
        state.buf.is_empty() && !state.writing
    }
}

impl Drop for ConsoleOut {
    fn drop(&mut self) {
        self.shared.state.lock().unwrap().closed = true;
        self.shared.ready.notify_one();
    }
}

impl State {
    fn room(&self) -> usize {
        MAX_BUFFERED.saturating_sub(self.buf.len())
    }

    fn queue(&mut self, data: &[u8]) {
        if self.dropped > 0 {
            let marker = format!(
                "\r\n[fevvm: {} bytes of console output dropped]\r\n",
                self.dropped
            );
            if self.room() < marker.len() + data.len() {
                self.dropped += data.len() as u64;
                return;
            }
            self.buf.extend(marker.as_bytes());
            self.dropped = 0;
            self.at_line_start = true;
        }
        if self.room() < data.len() + self.stamp_overhead(data) {
            self.dropped += data.len() as u64;
            return;
        }
        let Some(from) = self.stamp_from else {
            self.buf.extend(data);
            return;
        };
        for line in data.split_inclusive(|b| *b == b'\n') {
            if self.at_line_start {
                let ms = from.elapsed().as_secs_f64() * 1000.0;
                self.buf.extend(format!("[{ms:9.1}ms] ").as_bytes());
            }
            self.buf.extend(line);
            self.at_line_start = line.ends_with(b"\n");
        }
    }

    fn stamp_overhead(&self, data: &[u8]) -> usize {
        const STAMP_MAX: usize = 32;
        if self.stamp_from.is_none() {
            return 0;
        }
        (data.iter().filter(|b| **b == b'\n').count() + 1) * STAMP_MAX
    }
}

fn run_writer(shared: &Shared, mut out: Box<dyn Write + Send>) {
    let mut chunk = Vec::with_capacity(WRITE_CHUNK);
    let mut broken = false;
    loop {
        {
            let mut state = shared.state.lock().unwrap();
            state.writing = false;
            if state.buf.is_empty() {
                shared.drained.notify_all();
            }
            while state.buf.is_empty() && !state.closed {
                state = shared.ready.wait(state).unwrap();
            }
            if state.buf.is_empty() {
                return;
            }
            let n = state.buf.len().min(WRITE_CHUNK);
            chunk.clear();
            chunk.extend(state.buf.drain(..n));
            state.writing = true;
        }
        if !broken && out.write_all(&chunk).and_then(|()| out.flush()).is_err() {
            broken = true;
        }
    }
}

#[cfg(test)]
pub mod testing {
    use super::*;

    struct Sink(Arc<Mutex<Vec<u8>>>);

    impl Write for Sink {
        fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
            self.0.lock().unwrap().extend_from_slice(buf);
            Ok(buf.len())
        }
        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }

    pub struct Capture {
        bytes: Arc<Mutex<Vec<u8>>>,
        out: Arc<ConsoleOut>,
    }

    impl Capture {
        pub fn text(&self) -> String {
            assert!(
                self.out.flush(Duration::from_secs(5)),
                "console did not drain"
            );
            String::from_utf8_lossy(&self.bytes.lock().unwrap()).into_owned()
        }
    }

    pub fn capture_with(stamp_from: Option<Instant>) -> (Capture, Arc<ConsoleOut>) {
        let bytes = Arc::new(Mutex::new(Vec::new()));
        let out = Arc::new(ConsoleOut::new(Box::new(Sink(bytes.clone())), stamp_from));
        (
            Capture {
                bytes,
                out: out.clone(),
            },
            out,
        )
    }

    pub fn capture() -> (Capture, Arc<ConsoleOut>) {
        capture_with(None)
    }
}

#[cfg(test)]
mod tests {
    use super::testing::*;
    use super::*;
    use std::sync::mpsc;

    #[test]
    fn passes_bytes_through_unchanged_and_in_order() {
        let (cap, out) = capture();
        out.write(b"one\r\ntw");
        out.write(b"o\r\n");
        assert_eq!(cap.text(), "one\r\ntwo\r\n");
        let big: Vec<u8> = (0..300_000u32).map(|i| b'a' + (i % 26) as u8).collect();
        out.write(&big);
        assert!(cap.text().ends_with(std::str::from_utf8(&big).unwrap()));
    }

    #[test]
    fn stamps_the_start_of_each_line_once() {
        let (cap, out) = capture_with(Some(Instant::now()));
        out.write(b"one\ntw");
        out.write(b"o\n");
        let text = cap.text();
        let lines: Vec<&str> = text.lines().collect();
        assert_eq!(lines.len(), 2);
        assert!(lines[0].starts_with('[') && lines[0].ends_with("ms] one"));
        assert!(lines[1].ends_with("ms] two"));
        assert_eq!(text.matches("ms] ").count(), 2);
    }

    struct Stuck {
        release: mpsc::Receiver<()>,
        got: Arc<Mutex<Vec<u8>>>,
        released: bool,
    }

    impl Write for Stuck {
        fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
            if !self.released {
                self.release.recv().unwrap();
                self.released = true;
            }
            self.got.lock().unwrap().extend_from_slice(buf);
            Ok(buf.len())
        }
        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }

    #[test]
    fn a_stuck_host_never_blocks_the_guest_and_the_gap_is_marked() {
        let (release, rx) = mpsc::channel();
        let got = Arc::new(Mutex::new(Vec::new()));
        let out = ConsoleOut::new(
            Box::new(Stuck {
                release: rx,
                got: got.clone(),
                released: false,
            }),
            None,
        );

        let line = [b'x'; 1024];
        let started = Instant::now();
        for _ in 0..3 * MAX_BUFFERED / line.len() {
            out.write(&line);
        }
        assert!(started.elapsed() < Duration::from_secs(5));
        assert!(
            !out.flush(Duration::from_millis(20)),
            "nothing can have drained"
        );

        release.send(()).unwrap();
        assert!(out.flush(Duration::from_secs(5)));
        out.write(b"after\n");
        assert!(out.flush(Duration::from_secs(5)));

        let got = got.lock().unwrap();
        let text = String::from_utf8_lossy(&got);
        assert!(got.len() <= MAX_BUFFERED + WRITE_CHUNK + 200);
        assert!(got.len() >= MAX_BUFFERED - line.len());
        assert!(text.contains("bytes of console output dropped]"));
        assert!(text.ends_with("after\n"));
    }

    #[test]
    fn the_first_byte_of_output_is_announced_once() {
        let (cap, out) = capture();
        let count = Arc::new(Mutex::new(0));
        let c = count.clone();
        out.on_first_write(move || *c.lock().unwrap() += 1);
        out.write(b"");
        assert_eq!(*count.lock().unwrap(), 0, "an empty write is not output");
        out.write(b"a");
        out.write(b"b\n");
        assert_eq!(*count.lock().unwrap(), 1);
        assert_eq!(cap.text(), "ab\n");
    }

    #[test]
    fn a_closed_host_side_does_not_fill_the_buffer() {
        struct Closed;
        impl Write for Closed {
            fn write(&mut self, _buf: &[u8]) -> std::io::Result<usize> {
                Err(std::io::ErrorKind::BrokenPipe.into())
            }
            fn flush(&mut self) -> std::io::Result<()> {
                Ok(())
            }
        }
        let out = ConsoleOut::new(Box::new(Closed), None);
        for _ in 0..4 {
            out.write(&vec![b'y'; MAX_BUFFERED / 2]);
            assert!(out.flush(Duration::from_secs(5)));
        }
    }
}
