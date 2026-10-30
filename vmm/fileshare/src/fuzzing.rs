use crate::abi::{self, op};
use crate::client::Client;
use crate::fake::FakeFs;
use crate::iov::{IoVecs, OwnedIov};
use crate::server::{head_len, Request, Server};

const CAPS: [usize; 8] = [0, 15, 16, 80, 144, 4096, 65536 + 16, (1 << 20) + 16];

pub fn fake_server() -> Server {
    let mut fs = FakeFs::new().accepting();
    fs.add_dir(b"d");
    fs.add_file(b"d/file", &[0x5a; 9000]);
    fs.add_file(b"empty", b"");
    fs.add_symlink(b"link", b"d/file");
    for i in 0..40 {
        fs.add_file(format!("d/n{i}").as_bytes(), b"x");
    }
    let c = Client::new(Server::new(Box::new(fs)));
    c.init();
    let root = c.list(abi::ROOT_ID, true).expect("list the root");
    for e in root.iter().filter_map(|e| e.entry) {
        let _ = c.list(e.nodeid, true);
        let _ = c.open(e.nodeid, 0);
    }
    c.opendir(abi::ROOT_ID).expect("open the root");
    c.server
}

pub fn requests(server: &Server, data: &[u8]) {
    let mut rest = data;
    for _ in 0..64 {
        let Some((&[lo, hi, cap], after)) = rest.split_first_chunk::<3>() else {
            return;
        };
        let len = (u16::from_le_bytes([lo, hi]) as usize).min(after.len());
        let (req, after) = after.split_at(len);
        rest = after;
        one(
            server,
            req,
            CAPS[cap as usize % CAPS.len()],
            1 + (cap as usize >> 3) * 97,
        );
    }
}

fn one(server: &Server, req: &[u8], cap: usize, piece: usize) {
    let opcode = req
        .get(4..8)
        .map_or(0, |b| u32::from_le_bytes(b.try_into().unwrap()));
    let unique = req
        .get(8..16)
        .map(|b| u64::from_le_bytes(b.try_into().unwrap()));
    let head = head_len(opcode, req.len());
    let mut tail = req[head..].to_vec();
    let tail_iov = [libc::iovec {
        iov_base: tail.as_mut_ptr().cast(),
        iov_len: tail.len(),
    }];
    let reply = OwnedIov::new(cap, piece);
    let n = server.handle(&Request {
        head: &req[..head],
        payload: unsafe { IoVecs::new(&tail_iov) },
        reply: reply.iov(),
    });
    assert!(n <= cap, "{n} bytes written into {cap} for opcode {opcode}");
    if n == 0 {
        return;
    }
    let out = reply.bytes(n);
    assert!(n >= abi::OUT_HEADER_LEN);
    assert_eq!(
        u32::from_le_bytes(out[0..4].try_into().unwrap()) as usize,
        n
    );
    assert_eq!(
        Some(u64::from_le_bytes(out[8..16].try_into().unwrap())),
        unique
    );
    let error = i32::from_le_bytes(out[4..8].try_into().unwrap());
    assert!((-4095..=0).contains(&error), "error {error}");
    assert!(
        error == 0 || n == abi::OUT_HEADER_LEN,
        "an error with a body"
    );
    assert!(
        !matches!(
            opcode,
            op::FORGET | op::BATCH_FORGET | op::INTERRUPT | op::NOTIFY_REPLY
        ),
        "opcode {opcode} has no reply"
    );
}

pub fn fuzz_decoder(data: &[u8]) {
    requests(&fake_server(), data);
}

pub fn structured(seed: u64, records: usize) -> Vec<u8> {
    let mut state = seed | 1;
    let mut next = move || {
        state ^= state << 13;
        state ^= state >> 7;
        state ^= state << 17;
        state
    };
    let mut data = Vec::new();
    for _ in 0..records {
        let opcode = match next() % 10 {
            0 => next() as u32,
            1 => 60 + (next() % 8) as u32,
            _ => (next() % 54) as u32,
        };
        let body_len = match next() % 6 {
            0 => 0,
            1 => (next() % 9000) as usize,
            _ => (next() % 72) as usize,
        };
        let mut req = Vec::with_capacity(abi::IN_HEADER_LEN + body_len);
        req.extend_from_slice(&((abi::IN_HEADER_LEN + body_len) as u32).to_le_bytes());
        req.extend_from_slice(&opcode.to_le_bytes());
        req.extend_from_slice(&next().to_le_bytes());
        let nodeid = if next() % 5 == 0 { next() } else { next() % 48 };
        req.extend_from_slice(&nodeid.to_le_bytes());
        req.extend_from_slice(&[0; 16]);
        for i in 0..body_len {
            req.push(match (i % 8, next() % 4) {
                (0, _) => (next() % 50) as u8,
                (_, 0) => next() as u8,
                _ => 0,
            });
        }
        let names: [&[u8]; 8] = [
            b"d\0",
            b"file\0",
            b"link\0",
            b"..\0",
            b"n7\0",
            b"empty\0",
            b"new\0",
            b"user.a\0",
        ];
        match next() % 6 {
            0 | 1 if body_len > 0 => {
                req.truncate(abi::IN_HEADER_LEN);
                req.extend_from_slice(names[(next() % 8) as usize]);
            }
            2 | 3 => {
                let fixed = match opcode {
                    op::MKNOD | op::CREATE | op::SETXATTR | op::RENAME2 => 16,
                    op::MKDIR | op::RENAME | op::LINK | op::GETXATTR => 8,
                    op::WRITE => abi::WRITE_IN_LEN,
                    op::SETATTR => abi::SETATTR_IN_LEN,
                    _ => 0,
                };
                req.truncate(abi::IN_HEADER_LEN + fixed.min(body_len));
                req.resize(abi::IN_HEADER_LEN + fixed, (next() % 3) as u8);
                for _ in 0..(1 + next() % 2) {
                    req.extend_from_slice(names[(next() % 8) as usize]);
                }
                let extra = (next() % 40) as usize;
                req.resize(req.len() + extra, next() as u8);
            }
            _ => {}
        }
        if next() % 7 == 0 {
            req.truncate((next() % 48) as usize);
        }
        data.extend_from_slice(&(req.len() as u16).to_le_bytes());
        data.push(next() as u8);
        data.extend_from_slice(&req);
    }
    data
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_decoder_survives_hostile_requests() {
        for seed in 1..400u64 {
            fuzz_decoder(&structured(seed.wrapping_mul(0x9e37_79b9_7f4a_7c15), 48));
        }
        fuzz_decoder(&[]);
        fuzz_decoder(&[0xff; 7]);
        fuzz_decoder(&[0xff; 70_000]);
        fuzz_decoder(&[0; 4096]);
    }

    fn scratch_share(dir: &std::path::Path, read_only: bool) -> Client {
        let _ = std::fs::remove_dir_all(dir);
        std::fs::create_dir_all(dir.join("share/d")).unwrap();
        std::fs::write(dir.join("share/d/file"), [0x5a; 9000]).unwrap();
        std::fs::write(dir.join("share/empty"), b"").unwrap();
        std::fs::write(dir.join("outside"), b"not shared").unwrap();
        std::os::unix::fs::symlink("d/file", dir.join("share/link")).unwrap();
        std::os::unix::fs::symlink("..", dir.join("share/d/up")).unwrap();
        for i in 0..40 {
            std::fs::write(dir.join(format!("share/d/n{i}")), b"x").unwrap();
        }
        let mut cfg = crate::passthrough::Config::new(dir.join("share"));
        cfg.read_only = read_only;
        let fs = crate::passthrough::Passthrough::new(&cfg).unwrap();
        let c = Client::new(Server::new(Box::new(fs)));
        c.init();
        for e in c
            .list(abi::ROOT_ID, true)
            .unwrap()
            .iter()
            .filter_map(|e| e.entry)
        {
            let _ = c.list(e.nodeid, true);
            let _ = c.open(e.nodeid, 0);
            if !read_only {
                let _ = c.open(e.nodeid, 2);
            }
        }
        c
    }

    fn hostile_run(c: &Client, salt: u64) {
        for seed in 1..300u64 {
            requests(&c.server, &structured(seed.wrapping_mul(salt), 48));
            if c.server.negotiated().is_none() {
                c.init();
            }
        }
    }

    #[test]
    fn the_passthrough_survives_hostile_requests() {
        let dir = std::env::temp_dir().join(format!("fev-fs-hostile-{}", std::process::id()));
        let c = scratch_share(&dir, true);
        hostile_run(&c, 0x2545_f491_4f6c_dd1d);
        let share = dir.join("share");
        assert_eq!(std::fs::read(share.join("d/file")).unwrap(), [0x5a; 9000]);
        assert_eq!(std::fs::read_dir(share.join("d")).unwrap().count(), 42);
        assert_eq!(std::fs::read_dir(&share).unwrap().count(), 3);
        std::fs::remove_dir_all(&dir).unwrap();
    }

    #[test]
    fn hostile_writes_stay_inside_the_share() {
        let dir = std::env::temp_dir().join(format!("fev-fs-hostilew-{}", std::process::id()));
        let c = scratch_share(&dir, false);
        hostile_run(&c, 0x9e37_79b9_7f4a_7c15);
        hostile_run(&c, 0x2545_f491_4f6c_dd1d);
        assert_eq!(std::fs::read(dir.join("outside")).unwrap(), b"not shared");
        let mut beside: Vec<_> = std::fs::read_dir(&dir)
            .unwrap()
            .map(|e| e.unwrap().file_name())
            .collect();
        beside.sort();
        assert_eq!(beside, ["outside", "share"]);
        std::fs::remove_dir_all(&dir).unwrap();
    }
}
