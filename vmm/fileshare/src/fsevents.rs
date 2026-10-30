use std::fmt::Write as _;

pub const HOST_PORT: u32 = 1027;
pub const VERSION: u16 = 1;
pub const MAGIC: [u8; 4] = *b"BFSE";

pub const TYPE_HELLO: u8 = 1;
pub const TYPE_WATCH: u8 = 2;
pub const TYPE_WATCHING: u8 = 3;
pub const TYPE_EVENTS: u8 = 4;

pub const HEADER_LEN: usize = 8;
pub const MAX_BUILD: usize = 128;
pub const MAX_HELLO: usize = 8 + MAX_BUILD;
pub const MAX_PATHS: usize = 1024;
pub const MAX_PATH: usize = 1023;
pub const MAX_WATCH: usize = 256 << 10;
pub const MAX_WATCHING: usize = 8 + MAX_PATHS;
pub const MAX_EVENTS: usize = 64 << 10;
pub const RECORD_LEN: usize = 40;
pub const MAX_NAME: usize = 255;
pub const QUEUE_MAX: usize = 256 << 10;

pub mod op {
    pub const CHANGE: u8 = 1;
    pub const CREATE: u8 = 2;
    pub const DELETE: u8 = 3;
    pub const RENAME: u8 = 4;
    pub const INVAL_ALL: u8 = 5;
}

pub mod flag {
    pub const ISDIR: u8 = 1 << 0;
    pub const LAST_LINK: u8 = 1 << 1;
    pub const QUIET: u8 = 1 << 2;
    pub const VICTIM_GONE: u8 = 1 << 3;
    pub const ALL: u8 = ISDIR | LAST_LINK | QUIET | VICTIM_GONE;
}

pub mod mask {
    pub const MODIFY: u8 = 1 << 0;
    pub const ATTRIB: u8 = 1 << 1;
    pub const CLOSE_WRITE: u8 = 1 << 2;
    pub const OPEN: u8 = 1 << 3;
    pub const ALL: u8 = MODIFY | ATTRIB | CLOSE_WRITE | OPEN;
    pub const CONTENT: u8 = OPEN | MODIFY | CLOSE_WRITE;
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
#[repr(u8)]
pub enum Status {
    Ok = 0,
    Outside = 1,
    Missing = 2,
    Failed = 3,
}

impl Status {
    fn from_wire(b: u8) -> Option<Status> {
        Some(match b {
            0 => Status::Ok,
            1 => Status::Outside,
            2 => Status::Missing,
            3 => Status::Failed,
            _ => return None,
        })
    }

    fn name(self) -> &'static str {
        match self {
            Status::Ok => "ok",
            Status::Outside => "outside",
            Status::Missing => "missing",
            Status::Failed => "failed",
        }
    }
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Event {
    pub op: u8,
    pub flags: u8,
    pub mask: u8,
    pub parent: u64,
    pub child: u64,
    pub parent2: u64,
    pub victim: u64,
    pub name: Vec<u8>,
    pub name2: Vec<u8>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Frame {
    Hello { version: u16, build: String },
    Watch { seq: u32, paths: Vec<String> },
    Watching { seq: u32, status: Vec<Status> },
    Events(Vec<Event>),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Side {
    Guest,
    Host,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Error {
    Short,
    Bad(&'static str),
}

type Refused = &'static str;

fn u16_at(b: &[u8], at: usize) -> u16 {
    u16::from_le_bytes([b[at], b[at + 1]])
}

fn u32_at(b: &[u8], at: usize) -> u32 {
    u32::from_le_bytes([b[at], b[at + 1], b[at + 2], b[at + 3]])
}

fn u64_at(b: &[u8], at: usize) -> u64 {
    let mut v = [0u8; 8];
    v.copy_from_slice(&b[at..at + 8]);
    u64::from_le_bytes(v)
}

pub fn valid_build(s: &[u8]) -> Result<(), Refused> {
    if s.len() > MAX_BUILD {
        return Err("build identifier too long");
    }
    if s.iter().any(|&c| !(0x21..=0x7e).contains(&c)) {
        return Err("build identifier is not printable ASCII");
    }
    Ok(())
}

pub fn valid_path(p: &[u8]) -> Result<(), Refused> {
    if p.is_empty() {
        return Err("empty path");
    }
    if p.len() > MAX_PATH {
        return Err("path too long");
    }
    if p[0] != b'/' {
        return Err("path is not absolute");
    }
    if std::str::from_utf8(p).is_err() {
        return Err("path is not UTF-8");
    }
    if p.contains(&0) {
        return Err("path contains a zero byte");
    }
    if p.len() == 1 {
        return Ok(());
    }
    if p[1..]
        .split(|&c| c == b'/')
        .any(|part| part.is_empty() || part == b"." || part == b"..")
    {
        return Err("path is not clean");
    }
    Ok(())
}

fn valid_name(n: &[u8]) -> Result<(), Refused> {
    if n.len() > MAX_NAME {
        return Err("name too long");
    }
    if n.contains(&b'/') || n.contains(&0) {
        return Err("name contains a slash or a zero byte");
    }
    if n == b"." || n == b".." {
        return Err("name is not a name");
    }
    Ok(())
}

impl Event {
    pub fn encoded_len(&self) -> usize {
        RECORD_LEN + self.name.len() + self.name2.len()
    }

    pub fn validate(&self) -> Result<(), Refused> {
        if self.flags & !flag::ALL != 0 {
            return Err("unknown flags");
        }
        if self.mask & !mask::ALL != 0 {
            return Err("unknown mask");
        }
        valid_name(&self.name)?;
        valid_name(&self.name2)?;
        let named = !self.name.is_empty();
        let mut allowed = flag::ISDIR | flag::QUIET;
        match self.op {
            op::CHANGE => {
                if self.child == 0 && (self.parent == 0 || !named) {
                    return Err("a change names neither an object nor a directory and a name");
                }
            }
            op::CREATE => {
                if self.parent == 0 || !named {
                    return Err("a create needs a directory and a name");
                }
                if self.child != 0 {
                    return Err("a create names no object");
                }
            }
            op::DELETE => {
                allowed |= flag::LAST_LINK;
                if !named || (self.parent == 0 && self.child == 0) {
                    return Err("a delete needs a name, and a directory or an object");
                }
            }
            op::RENAME => {
                allowed |= flag::VICTIM_GONE;
                if !named
                    || self.name2.is_empty()
                    || (self.parent == 0 && self.parent2 == 0 && self.child == 0)
                {
                    return Err("a rename needs both names, and a directory or an object");
                }
            }
            op::INVAL_ALL => {
                let alone = Event {
                    op: op::INVAL_ALL,
                    ..Event::default()
                };
                if *self != alone {
                    return Err("an invalidation carries nothing else");
                }
            }
            _ => return Err("unknown operation"),
        }
        if self.flags & !allowed != 0 {
            return Err("flags that do not belong to the operation");
        }
        if self.op != op::CHANGE && self.mask != 0 {
            return Err("only a change has a mask");
        }
        if self.op != op::RENAME
            && (self.parent2 != 0 || self.victim != 0 || !self.name2.is_empty())
        {
            return Err("only a rename has a second directory, a victim or a second name");
        }
        Ok(())
    }

    fn encode(&self, out: &mut Vec<u8>) {
        out.extend_from_slice(&[self.op, self.flags, self.mask, 0]);
        out.extend_from_slice(&(self.name.len() as u16).to_le_bytes());
        out.extend_from_slice(&(self.name2.len() as u16).to_le_bytes());
        for id in [self.parent, self.child, self.parent2, self.victim] {
            out.extend_from_slice(&id.to_le_bytes());
        }
        out.extend_from_slice(&self.name);
        out.extend_from_slice(&self.name2);
    }
}

pub fn hello_matches(version: u16, theirs: &str, ours: &str) -> bool {
    version == VERSION && (theirs.is_empty() || ours.is_empty() || theirs == ours)
}

#[derive(Clone, Debug)]
pub struct Order {
    hello: bool,
    seq: u32,
}

impl Order {
    #[allow(clippy::new_without_default)]
    pub fn new() -> Order {
        Order {
            hello: false,
            seq: 0,
        }
    }

    pub fn check(&mut self, frame: &Frame) -> Result<(), Refused> {
        let seq = match frame {
            Frame::Hello { .. } if self.hello => return Err("a second hello"),
            Frame::Hello { .. } => {
                self.hello = true;
                return Ok(());
            }
            _ if !self.hello => return Err("a frame before the hello"),
            Frame::Watch { seq, .. } | Frame::Watching { seq, .. } => *seq,
            Frame::Events(_) => return Ok(()),
        };
        if seq == 0 || seq != self.seq.wrapping_add(1) {
            return Err("a sequence number out of order");
        }
        self.seq = seq;
        Ok(())
    }
}

fn limit(from: Side, ty: u8) -> Option<usize> {
    match (ty, from) {
        (TYPE_HELLO, _) => Some(MAX_HELLO),
        (TYPE_WATCH, Side::Guest) => Some(MAX_WATCH),
        (TYPE_WATCHING, Side::Host) => Some(MAX_WATCHING),
        (TYPE_EVENTS, Side::Host) => Some(MAX_EVENTS),
        _ => None,
    }
}

pub fn header(from: Side, head: &[u8; HEADER_LEN]) -> Result<(usize, u8), Refused> {
    let (len, ty) = (u32_at(head, 0) as usize, head[4]);
    if head[5..] != [0, 0, 0] {
        return Err("non-zero padding in a frame header");
    }
    let Some(max) = limit(from, ty) else {
        return Err("a frame type this peer may not send");
    };
    if len > max {
        return Err("payload longer than its type allows");
    }
    Ok((len, ty))
}

pub fn decode(from: Side, buf: &[u8]) -> Result<(Frame, usize), Error> {
    let Some(head) = buf.first_chunk::<HEADER_LEN>() else {
        return Err(Error::Short);
    };
    let (len, ty) = header(from, head).map_err(Error::Bad)?;
    let Some(payload) = buf[HEADER_LEN..].get(..len) else {
        return Err(Error::Short);
    };
    let frame = decode_payload(ty, payload).map_err(Error::Bad)?;
    Ok((frame, HEADER_LEN + len))
}

pub fn decode_payload(ty: u8, p: &[u8]) -> Result<Frame, Refused> {
    match ty {
        TYPE_HELLO => decode_hello(p),
        TYPE_WATCH => decode_watch(p),
        TYPE_WATCHING => decode_watching(p),
        TYPE_EVENTS => decode_events(p),
        _ => Err("unknown frame type"),
    }
}

fn decode_hello(p: &[u8]) -> Result<Frame, Refused> {
    if p.len() < 8 {
        return Err("hello too short");
    }
    if p[..4] != MAGIC {
        return Err("not a change-feed hello");
    }
    let build = &p[8..];
    if u16_at(p, 6) as usize != build.len() {
        return Err("a hello whose length disagrees with its build identifier");
    }
    valid_build(build)?;
    Ok(Frame::Hello {
        version: u16_at(p, 4),
        build: String::from_utf8_lossy(build).into_owned(),
    })
}

fn counted(p: &[u8]) -> Result<(u32, usize, &[u8]), Refused> {
    if p.len() < 8 {
        return Err("watch frame too short");
    }
    let (seq, count) = (u32_at(p, 0), u16_at(p, 4) as usize);
    if seq == 0 {
        return Err("sequence number 0");
    }
    if u16_at(p, 6) != 0 {
        return Err("non-zero padding in a watch frame");
    }
    if count > MAX_PATHS {
        return Err("too many paths");
    }
    Ok((seq, count, &p[8..]))
}

fn decode_watch(p: &[u8]) -> Result<Frame, Refused> {
    let (seq, count, mut p) = counted(p)?;
    if count * 2 > p.len() {
        return Err("more paths than the payload holds");
    }
    let mut paths = Vec::with_capacity(count);
    for _ in 0..count {
        if p.len() < 2 {
            return Err("a watch set cut short");
        }
        let n = u16_at(p, 0) as usize;
        let Some(path) = p[2..].get(..n) else {
            return Err("a watch path cut short");
        };
        valid_path(path)?;
        let Ok(path) = std::str::from_utf8(path) else {
            return Err("path is not UTF-8");
        };
        paths.push(path.to_owned());
        p = &p[2 + n..];
    }
    if !p.is_empty() {
        return Err("bytes after a watch set");
    }
    Ok(Frame::Watch { seq, paths })
}

fn decode_watching(p: &[u8]) -> Result<Frame, Refused> {
    let (seq, count, p) = counted(p)?;
    if count != p.len() {
        return Err("an acknowledgement whose count disagrees with its length");
    }
    let mut status = Vec::with_capacity(count);
    for &b in p {
        status.push(Status::from_wire(b).ok_or("unknown status")?);
    }
    Ok(Frame::Watching { seq, status })
}

fn decode_events(p: &[u8]) -> Result<Frame, Refused> {
    if p.len() < 4 {
        return Err("events frame too short");
    }
    let count = u16_at(p, 0) as usize;
    if u16_at(p, 2) != 0 {
        return Err("non-zero padding in an events frame");
    }
    let mut p = &p[4..];
    if count == 0 || count * RECORD_LEN > p.len() {
        return Err("a record count the payload cannot hold");
    }
    let mut records = Vec::with_capacity(count);
    for _ in 0..count {
        if p.len() < RECORD_LEN {
            return Err("a record cut short");
        }
        if p[3] != 0 {
            return Err("non-zero padding in a record");
        }
        let (n, n2) = (u16_at(p, 4) as usize, u16_at(p, 6) as usize);
        if n > MAX_NAME || n2 > MAX_NAME {
            return Err("name too long");
        }
        let Some(names) = p[RECORD_LEN..].get(..n + n2) else {
            return Err("a record's names cut short");
        };
        let e = Event {
            op: p[0],
            flags: p[1],
            mask: p[2],
            parent: u64_at(p, 8),
            child: u64_at(p, 16),
            parent2: u64_at(p, 24),
            victim: u64_at(p, 32),
            name: names[..n].to_vec(),
            name2: names[n..].to_vec(),
        };
        e.validate()?;
        records.push(e);
        p = &p[RECORD_LEN + n + n2..];
    }
    if !p.is_empty() {
        return Err("bytes after the last record");
    }
    Ok(Frame::Events(records))
}

pub fn encode(frame: &Frame, out: &mut Vec<u8>) -> Result<(), Refused> {
    let start = out.len();
    let result = encode_frame(frame, out, start);
    if result.is_err() {
        out.truncate(start);
    }
    result
}

fn encode_frame(frame: &Frame, out: &mut Vec<u8>, start: usize) -> Result<(), Refused> {
    let (ty, max) = match frame {
        Frame::Hello { .. } => (TYPE_HELLO, MAX_HELLO),
        Frame::Watch { .. } => (TYPE_WATCH, MAX_WATCH),
        Frame::Watching { .. } => (TYPE_WATCHING, MAX_WATCHING),
        Frame::Events(_) => (TYPE_EVENTS, MAX_EVENTS),
    };
    out.extend_from_slice(&[0, 0, 0, 0, ty, 0, 0, 0]);
    match frame {
        Frame::Hello { version, build } => {
            valid_build(build.as_bytes())?;
            out.extend_from_slice(&MAGIC);
            out.extend_from_slice(&version.to_le_bytes());
            out.extend_from_slice(&(build.len() as u16).to_le_bytes());
            out.extend_from_slice(build.as_bytes());
        }
        Frame::Watch { seq, paths } => {
            if *seq == 0 {
                return Err("sequence number 0");
            }
            if paths.len() > MAX_PATHS {
                return Err("too many paths");
            }
            out.extend_from_slice(&seq.to_le_bytes());
            out.extend_from_slice(&(paths.len() as u16).to_le_bytes());
            out.extend_from_slice(&[0, 0]);
            for p in paths {
                valid_path(p.as_bytes())?;
                out.extend_from_slice(&(p.len() as u16).to_le_bytes());
                out.extend_from_slice(p.as_bytes());
            }
        }
        Frame::Watching { seq, status } => {
            if *seq == 0 {
                return Err("sequence number 0");
            }
            if status.len() > MAX_PATHS {
                return Err("too many paths");
            }
            out.extend_from_slice(&seq.to_le_bytes());
            out.extend_from_slice(&(status.len() as u16).to_le_bytes());
            out.extend_from_slice(&[0, 0]);
            out.extend(status.iter().map(|&s| s as u8));
        }
        Frame::Events(records) => {
            if records.is_empty() {
                return Err("an events frame carries at least one record");
            }
            if records.len() > MAX_EVENTS / RECORD_LEN {
                return Err("too many records for one frame");
            }
            out.extend_from_slice(&(records.len() as u16).to_le_bytes());
            out.extend_from_slice(&[0, 0]);
            for e in records {
                e.validate()?;
                e.encode(out);
            }
        }
    }
    let len = out.len() - start - HEADER_LEN;
    if len > max {
        return Err("payload longer than its type allows");
    }
    out[start..start + 4].copy_from_slice(&(len as u32).to_le_bytes());
    Ok(())
}

pub fn encode_events(mut records: &[Event], out: &mut Vec<u8>) -> Result<(), Refused> {
    let start = out.len();
    while !records.is_empty() {
        let (mut n, mut size) = (0, 4);
        while n < records.len() && size + records[n].encoded_len() <= MAX_EVENTS {
            size += records[n].encoded_len();
            n += 1;
        }
        let (batch, rest) = records.split_at(n);
        if let Err(e) = encode(&Frame::Events(batch.to_vec()), out) {
            out.truncate(start);
            return Err(e);
        }
        records = rest;
    }
    Ok(())
}

pub fn describe(frame: &Frame) -> String {
    let mut s = String::new();
    match frame {
        Frame::Hello { version, build } => {
            let _ = writeln!(
                s,
                "hello version={version} build={}",
                quote(build.as_bytes())
            );
        }
        Frame::Watch { seq, paths } => {
            let _ = writeln!(s, "watch seq={seq} paths={}", paths.len());
            for p in paths {
                let _ = writeln!(s, "  {}", quote(p.as_bytes()));
            }
        }
        Frame::Watching { seq, status } => {
            let _ = writeln!(s, "watching seq={seq} paths={}", status.len());
            for st in status {
                let _ = writeln!(s, "  {}", st.name());
            }
        }
        Frame::Events(records) => {
            let _ = writeln!(s, "events records={}", records.len());
            for e in records {
                let _ = writeln!(
                    s,
                    "  {} flags={} mask={} parent={} child={} parent2={} victim={} name={} name2={}",
                    op_name(e.op),
                    bits(e.flags, ["isdir", "last_link", "quiet", "victim_gone"]),
                    bits(e.mask, ["modify", "attrib", "close_write", "open"]),
                    e.parent,
                    e.child,
                    e.parent2,
                    e.victim,
                    quote(&e.name),
                    quote(&e.name2),
                );
            }
        }
    }
    s
}

fn op_name(o: u8) -> String {
    match o {
        op::CHANGE => "change".into(),
        op::CREATE => "create".into(),
        op::DELETE => "delete".into(),
        op::RENAME => "rename".into(),
        op::INVAL_ALL => "inval_all".into(),
        other => format!("op({other})"),
    }
}

fn bits(v: u8, names: [&str; 4]) -> String {
    let set: Vec<&str> = names
        .iter()
        .enumerate()
        .filter(|(i, _)| v & (1 << i) != 0)
        .map(|(_, name)| *name)
        .collect();
    if set.is_empty() {
        "0".into()
    } else {
        set.join("|")
    }
}

fn quote(bytes: &[u8]) -> String {
    let mut s = String::with_capacity(bytes.len() + 2);
    s.push('"');
    for &c in bytes {
        match c {
            b'"' | b'\\' => {
                s.push('\\');
                s.push(c as char);
            }
            0x20..=0x7e => s.push(c as char),
            _ => {
                let _ = write!(s, "\\x{c:02x}");
            }
        }
    }
    s.push('"');
    s
}

#[cfg(any(test, feature = "testing"))]
pub fn fuzz(data: &[u8]) {
    for from in [Side::Guest, Side::Host] {
        let (mut rest, mut order) = (data, Order::new());
        while let Ok((frame, n)) = decode(from, rest) {
            assert!((HEADER_LEN..=rest.len()).contains(&n));
            let mut again = Vec::new();
            encode(&frame, &mut again).expect("what decodes, encodes");
            assert_eq!(again, &rest[..n]);
            let _ = describe(&frame);
            let _ = order.check(&frame);
            rest = &rest[n..];
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::{Path, PathBuf};

    fn fixtures(sub: &str) -> Vec<(String, Vec<u8>)> {
        let dir = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../proto/fsevents/fixtures")
            .join(sub);
        let mut paths: Vec<PathBuf> = std::fs::read_dir(&dir)
            .unwrap_or_else(|e| panic!("{}: {e}", dir.display()))
            .map(|entry| entry.unwrap().path())
            .filter(|p| p.extension().is_some_and(|ext| ext == "bin"))
            .collect();
        paths.sort();
        paths
            .into_iter()
            .map(|p| {
                let name = p.file_stem().unwrap().to_str().unwrap().to_owned();
                (name, std::fs::read(&p).unwrap())
            })
            .collect()
    }

    fn side(name: &str) -> Side {
        if name.starts_with("guest-") {
            Side::Guest
        } else if name.starts_with("host-") {
            Side::Host
        } else {
            panic!("{name}: a fixture's name begins with guest- or host-")
        }
    }

    fn decode_all(from: Side, mut b: &[u8]) -> Result<Vec<Frame>, Error> {
        let mut out = Vec::new();
        while !b.is_empty() {
            let (frame, n) = decode(from, b)?;
            out.push(frame);
            b = &b[n..];
        }
        Ok(out)
    }

    fn text(name: &str) -> String {
        let path = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../proto/fsevents/fixtures")
            .join(format!("{name}.txt"));
        std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display()))
    }

    #[test]
    fn every_fixture_decodes_to_its_text_and_encodes_back_byte_for_byte() {
        let all = fixtures("");
        assert!(all.len() >= 18, "only {} fixtures found", all.len());
        for (name, bytes) in all {
            let frames =
                decode_all(side(&name), &bytes).unwrap_or_else(|e| panic!("{name}: {e:?}"));
            let said: String = frames.iter().map(describe).collect();
            assert_eq!(said, text(&name), "{name}");
            let mut again = Vec::new();
            for f in &frames {
                encode(f, &mut again).unwrap_or_else(|e| panic!("{name}: {e}"));
            }
            assert_eq!(again, bytes, "{name}");
        }
    }

    #[test]
    fn every_bad_fixture_is_a_protocol_error() {
        let all = fixtures("bad");
        assert!(all.len() >= 70, "only {} bad fixtures found", all.len());
        for (name, bytes) in all {
            match decode_all(side(&name), &bytes) {
                Err(Error::Bad(_)) => {}
                other => panic!("bad/{name}: {other:?}"),
            }
        }
    }

    fn in_order(frames: &[Frame]) -> Result<(), Refused> {
        let mut order = Order::new();
        frames.iter().try_for_each(|f| order.check(f))
    }

    #[test]
    fn order_is_part_of_the_format() {
        let mut sessions = 0;
        for (name, bytes) in fixtures("") {
            if name.ends_with("-session") {
                sessions += 1;
                let frames = decode_all(side(&name), &bytes).unwrap();
                assert_eq!(in_order(&frames), Ok(()), "{name}");
            }
        }
        assert!(sessions >= 2, "only {sessions} session fixtures found");
        let all = fixtures("order");
        assert!(all.len() >= 13, "only {} order fixtures found", all.len());
        for (name, bytes) in all {
            let frames =
                decode_all(side(&name), &bytes).unwrap_or_else(|e| panic!("order/{name}: {e:?}"));
            let (_, before) = frames.split_last().unwrap();
            assert_eq!(in_order(before), Ok(()), "order/{name}");
            assert!(in_order(&frames).is_err(), "order/{name}: accepted");
        }
        let mut order = Order::new();
        order
            .check(&Frame::Hello {
                version: VERSION,
                build: String::new(),
            })
            .unwrap();
        order.seq = u32::MAX;
        for seq in [0, 1] {
            let paths = vec![];
            assert!(order.check(&Frame::Watch { seq, paths }).is_err());
        }
    }

    #[test]
    fn every_prefix_of_a_frame_is_short() {
        for (name, bytes) in fixtures("") {
            let from = side(&name);
            let (_, n) = decode(from, &bytes).unwrap();
            for cut in 0..n {
                assert_eq!(
                    decode(from, &bytes[..cut]),
                    Err(Error::Short),
                    "{name} at {cut}"
                );
            }
        }
    }

    #[test]
    fn a_length_over_the_limit_is_refused_from_the_header_alone() {
        for (from, ty, max) in [
            (Side::Guest, TYPE_HELLO, MAX_HELLO),
            (Side::Guest, TYPE_WATCH, MAX_WATCH),
            (Side::Host, TYPE_WATCHING, MAX_WATCHING),
            (Side::Host, TYPE_EVENTS, MAX_EVENTS),
        ] {
            let mut head = [0u8; HEADER_LEN];
            head[..4].copy_from_slice(&(max as u32 + 1).to_le_bytes());
            head[4] = ty;
            assert!(header(from, &head).is_err());
            assert!(matches!(decode(from, &head), Err(Error::Bad(_))));
            head[..4].copy_from_slice(&(max as u32).to_le_bytes());
            assert_eq!(header(from, &head), Ok((max, ty)));
            assert_eq!(decode(from, &head), Err(Error::Short));
        }
    }

    #[test]
    fn the_encoder_refuses_what_the_decoder_would_and_leaves_the_buffer() {
        let rename = Event {
            op: op::RENAME,
            parent: 1,
            parent2: 1,
            name: vec![b'n'; MAX_NAME],
            name2: vec![b'n'; MAX_NAME],
            ..Event::default()
        };
        let refused = [
            Frame::Hello {
                version: VERSION,
                build: "v 1".into(),
            },
            Frame::Hello {
                version: VERSION,
                build: "b".repeat(MAX_BUILD + 1),
            },
            Frame::Watch {
                seq: 0,
                paths: vec![],
            },
            Frame::Watch {
                seq: 1,
                paths: vec!["/p".into(); MAX_PATHS + 1],
            },
            Frame::Watch {
                seq: 1,
                paths: vec![format!("/{}", "p".repeat(300)); MAX_PATHS],
            },
            Frame::Watch {
                seq: 1,
                paths: vec!["a".into()],
            },
            Frame::Watching {
                seq: 0,
                status: vec![],
            },
            Frame::Events(vec![]),
            Frame::Events(vec![Event {
                op: op::CREATE,
                ..Event::default()
            }]),
            Frame::Events(vec![rename.clone(); 200]),
        ];
        for frame in refused {
            let mut out = b"kept".to_vec();
            assert!(encode(&frame, &mut out).is_err(), "{}", describe(&frame));
            assert_eq!(out, b"kept");
        }
    }

    #[test]
    fn a_large_batch_is_split_in_order() {
        let records: Vec<Event> = (1..=500)
            .map(|i| Event {
                op: op::RENAME,
                parent: i,
                parent2: 1,
                name: vec![b'n'; MAX_NAME],
                name2: vec![b'n'; MAX_NAME],
                ..Event::default()
            })
            .collect();
        let mut out = Vec::new();
        encode_events(&records, &mut out).unwrap();
        let frames = decode_all(Side::Host, &out).unwrap();
        assert!(frames.len() > 1);
        let got: Vec<Event> = frames
            .into_iter()
            .flat_map(|f| match f {
                Frame::Events(r) => r,
                other => panic!("{other:?}"),
            })
            .collect();
        assert_eq!(got, records);

        let mut out = b"x".to_vec();
        encode_events(&[], &mut out).unwrap();
        assert_eq!(out, b"x");
        let bad = Event::default();
        assert!(encode_events(&[records[0].clone(), bad], &mut out).is_err());
        assert_eq!(out, b"x");
    }

    #[test]
    fn a_hello_matches_only_the_same_version_and_build() {
        assert!(hello_matches(VERSION, "v1", "v1"));
        assert!(hello_matches(VERSION, "", "v1"));
        assert!(hello_matches(VERSION, "v1", ""));
        assert!(!hello_matches(VERSION, "v2", "v1"));
        assert!(!hello_matches(VERSION + 1, "v1", "v1"));
    }

    #[test]
    fn mutated_fixtures_never_panic_and_what_decodes_encodes_back() {
        let mut seeds = fixtures("");
        seeds.extend(fixtures("bad"));
        seeds.extend(fixtures("order"));
        let mut state = 0x9e37_79b9_7f4a_7c15u64;
        let mut next = move || {
            state ^= state << 13;
            state ^= state >> 7;
            state ^= state << 17;
            state
        };
        for (_, bytes) in &seeds {
            fuzz(bytes);
            if bytes.is_empty() {
                continue;
            }
            for _ in 0..400 {
                let mut b = bytes.clone();
                for _ in 0..1 + next() % 3 {
                    let at = next() as usize % b.len();
                    match next() % 4 {
                        0 => b[at] ^= 1 << (next() % 8),
                        1 => b[at] = next() as u8,
                        2 => b.truncate(at),
                        _ => {
                            let (_, other) = &seeds[next() as usize % seeds.len()];
                            b.extend_from_slice(other);
                        }
                    }
                    if b.is_empty() {
                        break;
                    }
                }
                fuzz(&b);
            }
        }
    }
}
