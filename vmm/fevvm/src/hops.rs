use std::fs::File;
use std::io::Write;
use std::sync::atomic::{AtomicBool, Ordering::Relaxed};
use std::sync::{Mutex, OnceLock};

use crate::hvf::{host_ticks, ticks_to_ns};

static ON: AtomicBool = AtomicBool::new(false);
static FILE: OnceLock<Mutex<(File, Vec<u8>)>> = OnceLock::new();

pub const RELAY_PORT: u32 = 1026;
pub const VIEW_PORT: u32 = 2049;

pub fn port_recorded(port: u32) -> bool {
    port == RELAY_PORT || port == VIEW_PORT
}

pub fn marks_for(port: u32) -> (Marks, Marks) {
    let m = Marks {
        line_done: port != RELAY_PORT,
        ..Default::default()
    };
    let n = Marks {
        line_done: port != RELAY_PORT,
        ..Default::default()
    };
    (m, n)
}

pub fn init_from_env() {
    if let Some(path) = std::env::var_os("FEV_VMM_VSOCK_HOPS") {
        if let Ok(f) = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(path)
        {
            let _ = FILE.set(Mutex::new((f, Vec::with_capacity(8192))));
            ON.store(true, Relaxed);
        }
    }
}

#[inline]
pub fn on() -> bool {
    ON.load(Relaxed)
}

#[inline]
pub fn now() -> i64 {
    ticks_to_ns(host_ticks()) as i64
}

pub fn add(dir: u8, xid: u32, size: u32, t0: i64, t1: i64) {
    let Some(m) = FILE.get() else { return };
    let mut g = m.lock().unwrap();
    let (f, buf) = &mut *g;
    buf.extend_from_slice(&[b'V', dir, 0, 0]);
    buf.extend_from_slice(&xid.to_le_bytes());
    buf.extend_from_slice(&size.to_le_bytes());
    buf.extend_from_slice(&t0.to_le_bytes());
    buf.extend_from_slice(&t1.to_le_bytes());
    if buf.len() >= 28 * 256 {
        let _ = f.write_all(buf);
        buf.clear();
    }
}

#[derive(Default)]
pub struct Marks {
    line_done: bool,
    head: [u8; 8],
    nhead: usize,
    left: u32,
    mid: bool,
}

impl Marks {
    pub fn feed(&mut self, mut p: &[u8], mut each: impl FnMut(u32, u32)) {
        if !self.line_done {
            match p.iter().position(|&b| b == b'\n') {
                Some(i) => {
                    self.line_done = true;
                    p = &p[i + 1..];
                }
                None => return,
            }
        }
        while !p.is_empty() {
            if self.left > 0 {
                let n = (p.len() as u32).min(self.left);
                self.left -= n;
                p = &p[n as usize..];
                continue;
            }
            let want = if self.mid { 4 } else { 8 };
            let n = (want - self.nhead).min(p.len());
            self.head[self.nhead..self.nhead + n].copy_from_slice(&p[..n]);
            self.nhead += n;
            p = &p[n..];
            if self.nhead < want {
                return;
            }
            let mark = u32::from_be_bytes(self.head[..4].try_into().unwrap());
            let mut size = mark & 0x7fff_ffff;
            let last = mark & 0x8000_0000 != 0;
            if !self.mid {
                each(
                    u32::from_be_bytes(self.head[4..8].try_into().unwrap()),
                    size,
                );
                size -= size.min(4);
            }
            self.nhead = 0;
            self.left = size;
            self.mid = !last;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn every_xid_after_the_line() {
        let mut s = b"{\"kind\":\"view\"}\n".to_vec();
        for (xid, n) in [(7u32, 40u32), (8, 4200), (9, 8)] {
            s.extend_from_slice(&(n | 0x8000_0000).to_be_bytes());
            let mut body = vec![0u8; n as usize];
            body[..4].copy_from_slice(&xid.to_be_bytes());
            s.extend_from_slice(&body);
        }
        for cut in [1usize, 5, 8, 100, s.len()] {
            let mut m = Marks::default();
            let mut got = vec![];
            for c in s.chunks(cut) {
                m.feed(c, |x, _| got.push(x));
            }
            assert_eq!(got, vec![7, 8, 9], "cut {cut}");
        }
    }
}
