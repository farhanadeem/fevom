use std::io;
use std::os::fd::RawFd;
use std::sync::mpsc;
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;

use crate::trace::{is_open, now_ns};
use crate::vmmctl::{
    self, code, method, Baseline, BudgetResult, Call, Empty, Message, PauseResult, Reset,
    ResumeResult, RpcError, Space, Stats,
};

pub trait Target: Send + Sync + 'static {
    fn pause(&self, arrived_ns: u64) -> Result<PauseResult, RpcError>;
    fn resume(&self, arrived_ns: u64) -> Result<ResumeResult, RpcError>;
    fn stats(&self) -> Stats;
    fn budget(&self, baseline: Baseline) -> BudgetResult;
    fn stopping(&self) -> bool;
    fn stop(&self);
    fn kill(&self);
}

struct Out {
    fd: RawFd,
    broken: Mutex<bool>,
}

impl Out {
    fn line(&self, line: &str) {
        let mut broken = self.broken.lock().unwrap();
        if *broken {
            return;
        }
        let mut rest = line.as_bytes();
        while !rest.is_empty() {
            let n = unsafe { libc::write(self.fd, rest.as_ptr().cast(), rest.len()) };
            if n > 0 {
                rest = &rest[n as usize..];
                continue;
            }
            match io::Error::last_os_error().kind() {
                io::ErrorKind::Interrupted => {}
                io::ErrorKind::WouldBlock => {
                    let mut p = libc::pollfd {
                        fd: self.fd,
                        events: libc::POLLOUT,
                        revents: 0,
                    };
                    unsafe { libc::poll(&mut p, 1, -1) };
                }
                _ => {
                    *broken = true;
                    return;
                }
            }
        }
    }
}

type Job = (String, Call, u64);

enum Note {
    Space(Space),
    Quit,
}

#[derive(Clone, Default)]
pub struct Notifier(Option<mpsc::Sender<Note>>);

impl Notifier {
    pub fn space(&self, space: Space) {
        if let Some(tx) = &self.0 {
            let _ = tx.send(Note::Space(space));
        }
    }
}

pub struct Control {
    out: Arc<Out>,
    notes: mpsc::Sender<Note>,
    wake: RawFd,
    threads: Vec<JoinHandle<()>>,
}

impl Control {
    pub fn start(fd: RawFd, target: Arc<dyn Target>) -> io::Result<Control> {
        if !is_open(fd) {
            return Err(io::Error::other("not an open descriptor"));
        }
        let mut pipe = [0; 2];
        if unsafe { libc::pipe(pipe.as_mut_ptr()) } != 0 {
            return Err(io::Error::last_os_error());
        }
        for end in pipe {
            unsafe { libc::fcntl(end, libc::F_SETFD, libc::FD_CLOEXEC) };
        }
        let (stop_r, wake) = (pipe[0], pipe[1]);
        let out = Arc::new(Out {
            fd,
            broken: Mutex::new(false),
        });
        let (jobs, queue) = mpsc::channel::<Job>();

        let worker = {
            let (out, target) = (out.clone(), target.clone());
            std::thread::Builder::new()
                .name("control-work".into())
                .spawn(move || {
                    for (id, call, arrived_ns) in queue {
                        out.line(&serve(&*target, &out, &id, call, arrived_ns));
                    }
                })?
        };
        let reader = {
            let out = out.clone();
            std::thread::Builder::new()
                .name("control".into())
                .spawn(move || {
                    read_loop(fd, stop_r, &out, &*target, &jobs);
                    unsafe { libc::close(stop_r) };
                })?
        };
        let (notes, noted) = mpsc::channel::<Note>();
        let notifier = {
            let out = out.clone();
            std::thread::Builder::new()
                .name("control-note".into())
                .spawn(move || {
                    for note in noted {
                        match note {
                            Note::Space(space) => {
                                out.line(&vmmctl::notification(method::SPACE, &space))
                            }
                            Note::Quit => return,
                        }
                    }
                })?
        };
        Ok(Control {
            out,
            notes,
            wake,
            threads: vec![reader, worker, notifier],
        })
    }

    pub fn notifier(&self) -> Notifier {
        Notifier(Some(self.notes.clone()))
    }

    pub fn reset(&self, count: u64, at_ns: u64) {
        self.out.line(&vmmctl::notification(
            method::RESET,
            &Reset { count, at_ns },
        ));
    }

    pub fn close(self) {
        unsafe { libc::close(self.wake) };
        let _ = self.notes.send(Note::Quit);
        for t in self.threads {
            let _ = t.join();
        }
    }
}

fn serve(target: &dyn Target, out: &Out, id: &str, call: Call, arrived_ns: u64) -> String {
    let answer = |r: Result<String, RpcError>| match r {
        Ok(line) => line,
        Err(e) => vmmctl::refusal(Some(id), &e),
    };
    match call {
        Call::Pause => answer(target.pause(arrived_ns).map(|r| vmmctl::reply(id, &r))),
        Call::Resume => answer(target.resume(arrived_ns).map(|r| vmmctl::reply(id, &r))),
        Call::Stop => {
            if target.stopping() {
                return vmmctl::refusal(Some(id), &RpcError::stopping());
            }
            out.line(&vmmctl::reply(id, &Empty));
            target.stop();
            String::new()
        }
        Call::Stats | Call::Kill | Call::Budget(_) => vmmctl::refusal(
            Some(id),
            &RpcError::new(code::INTERNAL_ERROR, "served out of place"),
        ),
    }
}

fn read_loop(fd: RawFd, stop: RawFd, out: &Out, target: &dyn Target, jobs: &mpsc::Sender<Job>) {
    let mut pending: Vec<u8> = Vec::new();
    let mut skipping = false;
    let mut buf = [0u8; 4096];
    loop {
        let mut fds = [
            libc::pollfd {
                fd,
                events: libc::POLLIN,
                revents: 0,
            },
            libc::pollfd {
                fd: stop,
                events: libc::POLLIN,
                revents: 0,
            },
        ];
        if unsafe { libc::poll(fds.as_mut_ptr(), 2, -1) } < 0 {
            if io::Error::last_os_error().kind() == io::ErrorKind::Interrupted {
                continue;
            }
            return;
        }
        if fds[1].revents != 0 {
            return;
        }
        let n = unsafe { libc::read(fd, buf.as_mut_ptr().cast(), buf.len()) };
        if n == 0 {
            return;
        }
        if n < 0 {
            match io::Error::last_os_error().kind() {
                io::ErrorKind::Interrupted | io::ErrorKind::WouldBlock => continue,
                _ => return,
            }
        }
        for &b in &buf[..n as usize] {
            if b != b'\n' {
                if skipping {
                    continue;
                }
                pending.push(b);
                if pending.len() > vmmctl::MAX_MESSAGE {
                    if let Err(e) = vmmctl::parse(&pending) {
                        out.line(&vmmctl::refusal(None, &e));
                    }
                    pending.clear();
                    skipping = true;
                }
                continue;
            }
            if skipping {
                skipping = false;
                continue;
            }
            let line = std::mem::take(&mut pending);
            if line.iter().all(|b| b.is_ascii_whitespace()) {
                continue;
            }
            handle(&line, out, target, jobs);
        }
    }
}

#[derive(Debug, PartialEq)]
enum Asked {
    Nothing,
    Refuse(String),
    Call(String, Call),
}

fn asked(line: &[u8]) -> Asked {
    let request = match vmmctl::parse(line) {
        Ok(Message::Request(r)) => r,
        Ok(Message::Reply { .. }) => return Asked::Nothing,
        Err(e) => return Asked::Refuse(vmmctl::refusal(None, &e)),
    };
    let call = request.call();
    let Some(id) = request.id else {
        return Asked::Nothing;
    };
    match call {
        Err(e) => Asked::Refuse(vmmctl::refusal(Some(&id), &e)),
        Ok(call) => Asked::Call(id, call),
    }
}

fn handle(line: &[u8], out: &Out, target: &dyn Target, jobs: &mpsc::Sender<Job>) {
    let arrived_ns = now_ns();
    match asked(line) {
        Asked::Nothing => {}
        Asked::Refuse(line) => out.line(&line),
        Asked::Call(id, Call::Stats) => out.line(&vmmctl::reply(&id, &target.stats())),
        Asked::Call(id, Call::Budget(baseline)) => {
            out.line(&vmmctl::reply(&id, &target.budget(baseline)))
        }
        Asked::Call(id, Call::Kill) => {
            out.line(&vmmctl::reply(&id, &Empty));
            target.kill();
        }
        Asked::Call(id, call) => {
            let _ = jobs.send((id, call, arrived_ns));
        }
    }
}

#[cfg(any(test, feature = "fuzzing"))]
pub fn fuzz(data: &[u8]) {
    let check = |written: &str, id: Option<&str>| {
        let body = written.strip_suffix('\n').expect("an answer ends its line");
        assert!(!body.contains('\n'), "an answer of two lines: {written:?}");
        match vmmctl::parse(body.as_bytes()) {
            Ok(Message::Reply { id: got, .. }) => {
                assert_eq!(got, id.unwrap_or("null"), "{written:?}")
            }
            other => panic!("wrote {written:?}, which reads back as {other:?}"),
        }
    };
    for line in data.split(|b| *b == b'\n') {
        match asked(line) {
            Asked::Nothing => {}
            Asked::Refuse(written) => {
                let body = written.trim_end();
                assert!(
                    matches!(
                        vmmctl::parse(body.as_bytes()),
                        Ok(Message::Reply { result: Err(_), .. })
                    ),
                    "{written:?}"
                );
                assert_eq!(written.matches('\n').count(), 1, "{written:?}");
            }
            Asked::Call(id, call) => {
                if let Call::Budget(b) = call {
                    let held_requests = b.available_bytes ^ b.image_bytes;
                    check(
                        &vmmctl::reply(&id, &BudgetResult { held_requests }),
                        Some(&id),
                    );
                }
                check(&vmmctl::reply(&id, &Empty), Some(&id));
                check(
                    &vmmctl::refusal(Some(&id), &RpcError::stopping()),
                    Some(&id),
                );
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::vmmctl::{json, State, Wire};
    use std::collections::VecDeque;
    use std::io::{BufRead, BufReader, Read, Write};
    use std::os::fd::AsRawFd;
    use std::os::unix::net::UnixStream;
    use std::path::PathBuf;
    use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
    use std::sync::Condvar;
    use std::time::Duration;

    #[derive(Default)]
    struct Script {
        pauses: Mutex<VecDeque<Result<PauseResult, RpcError>>>,
        resumes: Mutex<VecDeque<Result<ResumeResult, RpcError>>>,
        stats: Mutex<VecDeque<Stats>>,
        budgets: Mutex<VecDeque<BudgetResult>>,
        baselines: Mutex<Vec<Baseline>>,
        asked: Mutex<Vec<&'static str>>,
        stopped: AtomicBool,
        killed: AtomicBool,
        hold: Mutex<bool>,
        released: Condvar,
        arrived: AtomicU64,
    }

    impl Script {
        fn note(&self, what: &'static str) {
            self.asked.lock().unwrap().push(what);
        }
    }

    impl Target for Script {
        fn pause(&self, arrived_ns: u64) -> Result<PauseResult, RpcError> {
            self.note("pause");
            self.arrived.store(arrived_ns, Ordering::SeqCst);
            let held = self.hold.lock().unwrap();
            drop(self.released.wait_while(held, |h| *h).unwrap());
            if self.stopping() {
                return Err(RpcError::stopping());
            }
            self.pauses.lock().unwrap().pop_front().expect("a pause")
        }
        fn resume(&self, _arrived_ns: u64) -> Result<ResumeResult, RpcError> {
            self.note("resume");
            if self.stopping() {
                return Err(RpcError::stopping());
            }
            self.resumes.lock().unwrap().pop_front().expect("a resume")
        }
        fn stats(&self) -> Stats {
            self.note("stats");
            self.stats.lock().unwrap().pop_front().expect("a stats")
        }
        fn budget(&self, baseline: Baseline) -> BudgetResult {
            self.note("budget");
            self.baselines.lock().unwrap().push(baseline);
            self.budgets
                .lock()
                .unwrap()
                .pop_front()
                .unwrap_or(BudgetResult { held_requests: 0 })
        }
        fn stopping(&self) -> bool {
            self.stopped.load(Ordering::SeqCst)
        }
        fn stop(&self) {
            self.note("stop");
            self.stopped.store(true, Ordering::SeqCst);
        }
        fn kill(&self) {
            self.note("kill");
            self.killed.store(true, Ordering::SeqCst);
        }
    }

    fn pair() -> (UnixStream, UnixStream) {
        let (ours, theirs) = UnixStream::pair().unwrap();
        ours.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
        (ours, theirs)
    }

    fn read_line(r: &mut BufReader<UnixStream>) -> String {
        let mut line = String::new();
        r.read_line(&mut line).expect("an answer within 5 s");
        line
    }

    fn stats(state: State) -> Stats {
        Stats {
            api_version: vmmctl::API_VERSION,
            build: "test".into(),
            state,
            at_ns: 1,
            pauses: 0,
            total_offset_ns: 0,
            paused_at_ns: 0,
            resets: 0,
            vcpus: Vec::new(),
            memory: vmmctl::Memory {
                guest_bytes: 0,
                footprint_bytes: 0,
                resident_bytes: 0,
                compressed_bytes: 0,
                released_bytes: 0,
                released_ranges: 0,
            },
            devices: Default::default(),
            windows: None,
        }
    }

    fn paused(changed: bool) -> PauseResult {
        PauseResult {
            state: State::Paused,
            changed,
            vcpus: 2,
            took_ns: if changed { 5 } else { 0 },
            paused_at_ns: 77,
        }
    }

    fn result_of(line: &str) -> Option<json::Value> {
        match vmmctl::parse(line.as_bytes()).unwrap() {
            Message::Reply { result: Ok(v), .. } => Some(v),
            _ => None,
        }
    }

    #[test]
    fn serves_the_transcript_byte_for_byte() {
        let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../proto/vmmctl/fixtures/wire/session.jsonl");
        let text = std::fs::read_to_string(&path).unwrap();
        let lines: Vec<&str> = text.lines().collect();
        assert!(
            lines.len() >= 20,
            "the transcript has {} lines",
            lines.len()
        );

        let script = Arc::new(Script::default());
        let mut asked: Option<&str> = None;
        for line in &lines {
            if let Some(sent) = line.strip_prefix("> ") {
                asked = Some(if sent.contains("\"pause\"") {
                    "pause"
                } else if sent.contains("\"resume\"") {
                    "resume"
                } else if sent.contains("\"stats\"") {
                    "stats"
                } else if sent.contains("\"budget\"") {
                    "budget"
                } else {
                    "other"
                });
                continue;
            }
            let got = line.strip_prefix("< ").expect("a line is > or <");
            let Some(v) = result_of(got) else { continue };
            match asked.take() {
                Some("pause") => script
                    .pauses
                    .lock()
                    .unwrap()
                    .push_back(Ok(PauseResult::decode(&v).unwrap())),
                Some("resume") => script
                    .resumes
                    .lock()
                    .unwrap()
                    .push_back(Ok(ResumeResult::decode(&v).unwrap())),
                Some("stats") => script
                    .stats
                    .lock()
                    .unwrap()
                    .push_back(Stats::decode(&v).unwrap()),
                Some("budget") => script
                    .budgets
                    .lock()
                    .unwrap()
                    .push_back(BudgetResult::decode(&v).unwrap()),
                _ => {}
            }
        }

        let (ours, theirs) = pair();
        let control = Control::start(theirs.as_raw_fd(), script.clone()).unwrap();
        let mut reader = BufReader::new(ours.try_clone().unwrap());
        let mut writer = ours;
        let mut i = 0;
        while i < lines.len() {
            if let Some(sent) = lines[i].strip_prefix("> ") {
                writeln!(writer, "{sent}").unwrap();
                i += 1;
                continue;
            }
            let want = lines[i].strip_prefix("< ").unwrap();
            if want.contains("\"method\":\"reset\"") {
                let v = match vmmctl::parse(want.as_bytes()).unwrap() {
                    Message::Request(r) => r.params.unwrap(),
                    other => panic!("{other:?}"),
                };
                let reset = Reset::decode(&v).unwrap();
                control.reset(reset.count, reset.at_ns);
            }
            if want.contains("\"method\":\"space\"") {
                let v = match vmmctl::parse(want.as_bytes()).unwrap() {
                    Message::Request(r) => r.params.unwrap(),
                    other => panic!("{other:?}"),
                };
                control.notifier().space(Space::decode(&v).unwrap());
            }
            assert_eq!(
                read_line(&mut reader),
                format!("{want}\n"),
                "line {}",
                i + 1
            );
            i += 1;
        }
        assert!(script.stopped.load(Ordering::SeqCst), "stop reached nobody");
        let deadline = std::time::Instant::now() + Duration::from_secs(5);
        while !script.killed.load(Ordering::SeqCst) {
            assert!(std::time::Instant::now() < deadline, "kill reached nobody");
            std::thread::sleep(Duration::from_millis(1));
        }
        assert!(script.pauses.lock().unwrap().is_empty());
        assert_eq!(
            *script.baselines.lock().unwrap(),
            [
                Baseline {
                    available_bytes: 16106127360,
                    image_bytes: 4294967296,
                    margin_bytes: 5368709120
                },
                Baseline {
                    available_bytes: 1073741824,
                    image_bytes: 21474836480,
                    margin_bytes: 5368709120
                }
            ]
        );
        let late = control.notifier();
        control.close();
        late.space(Space {
            available_bytes: 1,
            image_bytes: 1,
            held_requests: 0,
            at_ns: 1,
        });
    }

    #[test]
    fn stats_is_answered_while_a_pause_is_under_way_and_the_rest_in_order() {
        let script = Arc::new(Script::default());
        *script.hold.lock().unwrap() = true;
        script.pauses.lock().unwrap().push_back(Ok(paused(true)));
        script
            .stats
            .lock()
            .unwrap()
            .push_back(stats(State::Running));
        script.resumes.lock().unwrap().push_back(Ok(ResumeResult {
            state: State::Running,
            changed: true,
            vcpus: 2,
            took_ns: 3,
            offset_ns: 9,
            total_offset_ns: 9,
        }));
        let (mut ours, theirs) = pair();
        let control = Control::start(theirs.as_raw_fd(), script.clone()).unwrap();
        let mut reader = BufReader::new(ours.try_clone().unwrap());
        let before = now_ns();
        ours.write_all(vmmctl::call(1, Call::Pause).as_bytes())
            .unwrap();
        ours.write_all(vmmctl::call(2, Call::Resume).as_bytes())
            .unwrap();
        ours.write_all(vmmctl::call(3, Call::Stats).as_bytes())
            .unwrap();
        let first = read_line(&mut reader);
        assert!(
            first.starts_with("{\"jsonrpc\":\"2.0\",\"id\":3,"),
            "{first}"
        );
        let deadline = std::time::Instant::now() + Duration::from_secs(5);
        while !script.asked.lock().unwrap().contains(&"pause") {
            assert!(
                std::time::Instant::now() < deadline,
                "the pause never began"
            );
            std::thread::sleep(Duration::from_millis(1));
        }
        std::thread::sleep(Duration::from_millis(20));
        let mut asked = script.asked.lock().unwrap().clone();
        asked.sort();
        assert_eq!(asked, ["pause", "stats"]);
        *script.hold.lock().unwrap() = false;
        script.released.notify_all();
        let second = read_line(&mut reader);
        assert!(
            second.starts_with("{\"jsonrpc\":\"2.0\",\"id\":1,"),
            "{second}"
        );
        let third = read_line(&mut reader);
        assert!(
            third.starts_with("{\"jsonrpc\":\"2.0\",\"id\":2,"),
            "{third}"
        );
        assert_eq!(script.asked.lock().unwrap().last(), Some(&"resume"));
        let arrived = script.arrived.load(Ordering::SeqCst);
        assert!(before <= arrived && arrived <= now_ns(), "{arrived}");
        control.close();
    }

    #[test]
    fn a_line_that_is_not_a_message_is_refused_and_the_next_is_served() {
        let script = Arc::new(Script::default());
        for _ in 0..3 {
            script.stats.lock().unwrap().push_back(stats(State::Paused));
        }
        let (mut ours, theirs) = pair();
        let control = Control::start(theirs.as_raw_fd(), script.clone()).unwrap();
        let mut reader = BufReader::new(ours.try_clone().unwrap());
        let parse_error = "{\"jsonrpc\":\"2.0\",\"id\":null,\"error\":{\"code\":-32700,";

        ours.write_all(b"not json\n\n").unwrap();
        assert!(read_line(&mut reader).starts_with(parse_error));
        ours.write_all(vmmctl::call(1, Call::Stats).as_bytes())
            .unwrap();
        assert!(read_line(&mut reader).contains("\"id\":1,\"result\""));

        let mut long = vec![b'x'; vmmctl::MAX_MESSAGE + 4096];
        long.extend_from_slice(vmmctl::call(7, Call::Stats).trim_end().as_bytes());
        long.push(b'\n');
        let mut w = ours.try_clone().unwrap();
        let writer = std::thread::spawn(move || w.write_all(&long).unwrap());
        assert!(read_line(&mut reader).starts_with(parse_error));
        writer.join().unwrap();
        ours.write_all(vmmctl::call(2, Call::Stats).as_bytes())
            .unwrap();
        assert!(read_line(&mut reader).contains("\"id\":2,\"result\""));

        ours.write_all(b"{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"pause\",\"params\":[1]}\n")
            .unwrap();
        assert!(read_line(&mut reader).contains("\"id\":3,\"error\":{\"code\":-32602,"));
        ours.write_all(b"{\"jsonrpc\":\"2.0\",\"id\":\"a\",\"method\":\"hotplug\"}\n")
            .unwrap();
        assert!(read_line(&mut reader).contains("\"id\":\"a\",\"error\":{\"code\":-32601,"));
        ours.write_all(b"{\"jsonrpc\":\"2.0\",\"method\":\"kill\"}\n")
            .unwrap();
        ours.write_all(b"{\"jsonrpc\":\"2.0\",\"id\":9,\"result\":{}}\n")
            .unwrap();
        ours.write_all(vmmctl::call(4, Call::Stats).as_bytes())
            .unwrap();
        assert!(read_line(&mut reader).contains("\"id\":4,\"result\""));
        assert!(
            !script.killed.load(Ordering::SeqCst),
            "a notification killed"
        );
        assert_eq!(*script.asked.lock().unwrap(), ["stats", "stats", "stats"]);
        control.close();
    }

    #[test]
    fn stop_replies_first_and_is_refused_once_stopping() {
        struct Ordered {
            peer: Mutex<BufReader<UnixStream>>,
            saw: Mutex<Option<String>>,
            stopped: AtomicBool,
        }
        impl Target for Ordered {
            fn pause(&self, _: u64) -> Result<PauseResult, RpcError> {
                Err(RpcError::stopping())
            }
            fn resume(&self, _: u64) -> Result<ResumeResult, RpcError> {
                Err(RpcError::stopping())
            }
            fn stats(&self) -> Stats {
                unreachable!()
            }
            fn budget(&self, _: Baseline) -> BudgetResult {
                unreachable!()
            }
            fn stopping(&self) -> bool {
                self.stopped.load(Ordering::SeqCst)
            }
            fn stop(&self) {
                let mut line = String::new();
                let _ = self.peer.lock().unwrap().read_line(&mut line);
                *self.saw.lock().unwrap() = Some(line);
                self.stopped.store(true, Ordering::SeqCst);
            }
            fn kill(&self) {}
        }
        let (mut ours, theirs) = pair();
        let target = Arc::new(Ordered {
            peer: Mutex::new(BufReader::new(ours.try_clone().unwrap())),
            saw: Mutex::new(None),
            stopped: AtomicBool::new(false),
        });
        let control = Control::start(theirs.as_raw_fd(), target.clone()).unwrap();
        ours.write_all(vmmctl::call(1, Call::Stop).as_bytes())
            .unwrap();
        ours.write_all(vmmctl::call(2, Call::Stop).as_bytes())
            .unwrap();
        ours.write_all(vmmctl::call(3, Call::Resume).as_bytes())
            .unwrap();
        let deadline = std::time::Instant::now() + Duration::from_secs(10);
        while !target.stopping() {
            assert!(std::time::Instant::now() < deadline, "never stopped");
            std::thread::sleep(Duration::from_millis(1));
        }
        let mut reader = BufReader::new(ours.try_clone().unwrap());
        let refused = read_line(&mut reader);
        assert_eq!(
            target.saw.lock().unwrap().as_deref(),
            Some("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n")
        );
        let stopping = ",\"error\":{\"code\":-32006,\"message\":\"the machine is stopping\"}}\n";
        assert_eq!(refused, format!("{{\"jsonrpc\":\"2.0\",\"id\":2{stopping}"));
        assert_eq!(
            read_line(&mut reader),
            format!("{{\"jsonrpc\":\"2.0\",\"id\":3{stopping}")
        );
        control.close();
    }

    #[test]
    fn close_stops_reading_and_leaves_the_descriptor_and_its_bytes() {
        let script = Arc::new(Script::default());
        script
            .stats
            .lock()
            .unwrap()
            .push_back(stats(State::Running));
        let (mut ours, mut theirs) = pair();
        let control = Control::start(theirs.as_raw_fd(), script.clone()).unwrap();
        ours.write_all(vmmctl::call(1, Call::Stats).as_bytes())
            .unwrap();
        let mut reader = BufReader::new(ours.try_clone().unwrap());
        assert!(read_line(&mut reader).contains("\"id\":1,"));
        control.close();
        let next = vmmctl::call(2, Call::Stats);
        ours.write_all(next.as_bytes()).unwrap();
        theirs
            .set_read_timeout(Some(Duration::from_secs(5)))
            .unwrap();
        let mut got = vec![0u8; next.len()];
        theirs.read_exact(&mut got).unwrap();
        assert_eq!(got, next.as_bytes());
        assert_eq!(*script.asked.lock().unwrap(), ["stats"]);
    }

    #[test]
    fn the_end_of_the_stream_stops_nothing() {
        let script = Arc::new(Script::default());
        let (ours, theirs) = pair();
        let control = Control::start(theirs.as_raw_fd(), script.clone()).unwrap();
        drop(ours);
        let _ = control.notes.send(Note::Quit);
        for t in control.threads {
            t.join().unwrap();
        }
        assert!(script.asked.lock().unwrap().is_empty());
        assert!(!script.stopping());
    }

    #[test]
    fn a_non_blocking_descriptor_is_served_and_a_full_socket_waited_for() {
        const CALLS: u64 = 400;
        let script = Arc::new(Script::default());
        for _ in 0..CALLS {
            script
                .stats
                .lock()
                .unwrap()
                .push_back(stats(State::Running));
        }
        let (ours, theirs) = pair();
        theirs.set_nonblocking(true).unwrap();
        let control = Control::start(theirs.as_raw_fd(), script).unwrap();
        let mut w = ours.try_clone().unwrap();
        let writer = std::thread::spawn(move || {
            for id in 1..=CALLS {
                w.write_all(vmmctl::call(id, Call::Stats).as_bytes())
                    .unwrap();
            }
        });
        std::thread::sleep(Duration::from_millis(100));
        let mut reader = BufReader::new(ours);
        for id in 1..=CALLS {
            let line = read_line(&mut reader);
            assert!(
                line.contains(&format!("\"id\":{id},\"result\"")),
                "answer {id}: {line}"
            );
        }
        writer.join().unwrap();
        control.close();
    }

    #[test]
    fn a_descriptor_that_is_not_open_is_refused() {
        let mut limit = libc::rlimit {
            rlim_cur: 0,
            rlim_max: 0,
        };
        unsafe { libc::getrlimit(libc::RLIMIT_NOFILE, &mut limit) };
        let fd = limit.rlim_cur.min(i32::MAX as u64 - 1) as RawFd;
        assert!(Control::start(fd, Arc::new(Script::default())).is_err());
    }
}
