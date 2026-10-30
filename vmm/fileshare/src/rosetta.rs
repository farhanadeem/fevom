use std::path::Path;

pub const TAG: &str = "rosetta";

pub const DIR: &str = "/Library/Apple/usr/libexec/oah/RosettaLinux";

pub const CACHE_NAME: &str = "fev.rosetta.cache";

const CACHE_ENV: &str = "FEV_VMM_ROSETTA_CACHE";

const KEY_LEN: usize = 69;

const DIGEST: [u8; 32] = unhex(b"203980d504722967f0f9ee2fc7a976689da91d715d302f80bfbdfbfa1ad4ff83");

const CACHE_LEN: usize = 128;
const CACHE_NAME_AT: usize = 1;
const CACHE_NAME_MAX: usize = 106;
const CACHE_ABSTRACT_AT: usize = 108;

const fn unhex(hex: &[u8; 64]) -> [u8; 32] {
    const fn digit(c: u8) -> u8 {
        match c {
            b'0'..=b'9' => c - b'0',
            b'a'..=b'f' => c - b'a' + 10,
            _ => panic!("not a lowercase hex digit"),
        }
    }
    let mut out = [0u8; 32];
    let mut i = 0;
    while i < 32 {
        out[i] = digit(hex[2 * i]) << 4 | digit(hex[2 * i + 1]);
        i += 1;
    }
    out
}

fn program() -> std::path::PathBuf {
    Path::new(DIR).join("rosetta")
}

pub fn installed() -> bool {
    std::fs::metadata(program()).is_ok_and(|m| m.is_file())
}

extern "C" {
    fn CC_SHA256(data: *const libc::c_void, len: u32, md: *mut u8) -> *mut u8;
}

fn sha256(data: &[u8]) -> [u8; 32] {
    let mut md = [0u8; 32];
    let len = u32::try_from(data.len()).expect("hashed data under 4 GiB");
    unsafe { CC_SHA256(data.as_ptr().cast(), len, md.as_mut_ptr()) };
    md
}

fn find<'a>(data: &'a [u8], len: usize, digest: &[u8; 32]) -> Option<&'a [u8]> {
    let mut run = 0usize;
    for (at, &byte) in data.iter().enumerate() {
        if byte != 0 {
            run += 1;
            continue;
        }
        if len > 0 && run >= len - 1 {
            let candidate = &data[at + 1 - len..=at];
            if sha256(candidate) == *digest {
                return Some(candidate);
            }
        }
        run = 0;
    }
    None
}

pub fn key() -> std::io::Result<Vec<u8>> {
    let path = program();
    let data = std::fs::read(&path).map_err(|e| {
        std::io::Error::new(e.kind(), format!("Rosetta at {}: {e}", path.display()))
    })?;
    match find(&data, KEY_LEN, &DIGEST) {
        Some(found) => Ok(found.to_vec()),
        None => Err(std::io::Error::other(format!(
            "the Rosetta at {} is not supported by this build of fev",
            path.display()
        ))),
    }
}

pub fn caching() -> Vec<u8> {
    let setting = std::env::var_os(CACHE_ENV);
    use std::os::unix::ffi::OsStrExt;
    cache_answer(setting.as_ref().map(|s| s.as_bytes()))
}

fn cache_answer(setting: Option<&[u8]>) -> Vec<u8> {
    let mut out = vec![0u8; CACHE_LEN];
    let (name, is_abstract) = match setting {
        None | Some(b"") => (CACHE_NAME.as_bytes(), true),
        Some(s) => {
            if let Some(name) = s.strip_prefix(b"abstract:") {
                (name, true)
            } else if let Some(path) = s.strip_prefix(b"unix:") {
                (path, false)
            } else {
                return out;
            }
        }
    };
    if name.is_empty() || name.len() > CACHE_NAME_MAX || name.contains(&0) {
        return out;
    }
    out[0] = 1;
    out[CACHE_NAME_AT..CACHE_NAME_AT + name.len()].copy_from_slice(name);
    out[CACHE_ABSTRACT_AT] = is_abstract as u8;
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::abi::{op, Writer, ROOT_ID};
    use crate::client::Client;
    use crate::errno::Errno;
    use crate::passthrough::{Config, Passthrough};
    use crate::server::Server;

    #[test]
    fn sha256_matches_the_standard_vector() {
        assert_eq!(
            sha256(b"abc"),
            unhex(b"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
        );
        assert_eq!(
            sha256(b""),
            unhex(b"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
        );
    }

    #[test]
    fn the_search_finds_the_tail_of_a_longer_terminated_string() {
        let digest = sha256(b"wxyz\0");
        let data = b"\0\0abc\0stuvwxyz\0more";
        assert_eq!(find(data, 5, &digest), Some(&b"wxyz\0"[..]));
        assert_eq!(find(b"wxyz\0", 5, &digest), Some(&b"wxyz\0"[..]));
        assert_eq!(find(b"\x01\x02wxyz\0", 5, &digest), Some(&b"wxyz\0"[..]));
    }

    #[test]
    fn the_search_refuses_what_cannot_be_the_string() {
        let digest = sha256(b"wxyz\0");
        assert_eq!(find(b"w\0xyz\0", 5, &digest), None);
        assert_eq!(find(b"xyz\0", 5, &digest), None);
        assert_eq!(find(b"wxyz!\0", 5, &digest), None);
        assert_eq!(find(b"\0wxyzwxyz", 5, &digest), None);
        assert_eq!(find(b"wxyzwxyzwxyz", 5, &digest), None);
        assert_eq!(find(b"abcd\0", 5, &digest), None);
        assert_eq!(find(b"", 5, &digest), None);
    }

    #[test]
    fn caching_by_default_offers_the_abstract_socket_nobody_listens_on() {
        let expected = {
            let mut out = vec![0u8; 128];
            out[0] = 1;
            out[1..1 + CACHE_NAME.len()].copy_from_slice(CACHE_NAME.as_bytes());
            out[108] = 1;
            out
        };
        assert_eq!(cache_answer(None), expected);
        assert_eq!(cache_answer(Some(b"")), expected);
        if std::env::var_os(CACHE_ENV).is_none() {
            assert_eq!(caching(), expected);
        }
    }

    #[test]
    fn caching_follows_the_setting() {
        let a = cache_answer(Some(b"abstract:other"));
        assert_eq!((a[0], &a[1..7], a[108]), (1, &b"other\0"[..], 1));
        assert!(a[7..108].iter().chain(&a[109..]).all(|&b| b == 0));

        let u = cache_answer(Some(b"unix:/tmp/c.sock"));
        assert_eq!((u[0], &u[1..13], u[108]), (1, &b"/tmp/c.sock\0"[..], 0));

        let longest = [b'n'; 106];
        let mut setting = b"abstract:".to_vec();
        setting.extend_from_slice(&longest);
        let l = cache_answer(Some(&setting));
        assert_eq!((l[0], &l[1..107], l[107], l[108]), (1, &longest[..], 0, 1));

        setting.push(b'n');
        let none = vec![0u8; 128];
        for refused in [
            &setting[..],
            b"abstract:",
            b"unix:",
            b"tcp:1.2.3.4",
            b"other",
            b"abstract:a\0b",
        ] {
            assert_eq!(cache_answer(Some(refused)), none, "{refused:?}");
        }
    }

    #[test]
    fn the_installed_rosetta_holds_the_key() {
        if !installed() {
            return;
        }
        let key = key().unwrap();
        assert_eq!(key.len(), KEY_LEN);
        assert_eq!(key[KEY_LEN - 1], 0);
        assert!(key[..KEY_LEN - 1].iter().all(|&b| b != 0));
        assert_eq!(sha256(&key), DIGEST);
    }

    fn share(rosetta: bool) -> Client {
        let mut cfg = Config::new(DIR);
        cfg.read_only = true;
        let mut fs = Passthrough::new(&cfg).unwrap();
        if rosetta {
            fs = fs.with_rosetta().unwrap();
        }
        let c = Client::new(Server::new(Box::new(fs)));
        c.init();
        c
    }

    fn opened(c: &Client, name: &[u8]) -> (u64, u64) {
        let node = c.lookup(ROOT_ID, name).unwrap().nodeid;
        (node, c.open(node, 0).unwrap())
    }

    #[test]
    fn rosettas_ioctls_are_answered_on_its_own_file_alone() {
        if !installed() {
            return;
        }
        let c = share(true);
        let (node, fh) = opened(&c, b"rosetta");

        let reply = |cmd: u32, size: u32| {
            let out = ioctl(&c, node, fh, cmd, size).unwrap();
            assert_eq!(out.len(), 16 + size as usize, "cmd {cmd:#x}");
            assert_eq!(&out[..4], &1u32.to_le_bytes(), "cmd {cmd:#x}'s result");
            assert!(out[4..16].iter().all(|&b| b == 0), "cmd {cmd:#x}");
            out[16..].to_vec()
        };
        for cmd in [0x6122, 0x6125] {
            let key = reply(cmd, KEY_LEN as u32);
            assert_eq!(sha256(&key), DIGEST, "cmd {cmd:#x}");
        }
        assert_eq!(reply(0x6123, 128), cache_answer(None));
        assert_eq!(reply(0x6124, 0), Vec::<u8>::new());

        assert_eq!(ioctl(&c, node, fh, 0x6122, 68), Err(Errno::EINVAL));
        assert_eq!(ioctl(&c, node, fh, 0x6123, 69), Err(Errno::EINVAL));
        assert_eq!(ioctl(&c, node, fh, 0x6124, 1), Err(Errno::EINVAL));
        assert_eq!(ioctl(&c, node, fh, 0x6122, 129), Err(Errno::ENOTTY));
        for cmd in [0x6121, 0x6126, 0x6222, 0x5422, 0] {
            assert_eq!(ioctl(&c, node, fh, cmd, 0), Err(Errno::ENOTTY), "{cmd:#x}");
        }
        assert_eq!(
            ioctl_with(&c, node, fh, 0, 0x6122, 4, KEY_LEN as u32),
            Err(Errno::ENOTTY)
        );
        assert_eq!(
            ioctl_with(&c, node, fh, 1, 0x6122, 0, KEY_LEN as u32),
            Err(Errno::ENOTTY)
        );

        let (other, other_fh) = opened(&c, b"rosettad");
        assert_eq!(
            ioctl(&c, other, other_fh, 0x6122, KEY_LEN as u32),
            Err(Errno::ENOTTY)
        );

        let plain = share(false);
        let (node, fh) = opened(&plain, b"rosetta");
        assert_eq!(
            ioctl(&plain, node, fh, 0x6122, KEY_LEN as u32),
            Err(Errno::ENOTTY)
        );
    }

    #[test]
    fn rosetta_is_never_answered_on_a_writable_share() {
        if !installed() {
            return;
        }
        let fs = Passthrough::new(&Config::new(DIR)).unwrap();
        assert!(fs.with_rosetta().is_err());
    }

    fn ioctl(c: &Client, node: u64, fh: u64, cmd: u32, out: u32) -> Result<Vec<u8>, Errno> {
        ioctl_with(c, node, fh, 0, cmd, 0, out)
    }

    fn ioctl_with(
        c: &Client,
        node: u64,
        fh: u64,
        flags: u32,
        cmd: u32,
        in_size: u32,
        out: u32,
    ) -> Result<Vec<u8>, Errno> {
        let mut w = Writer::default();
        w.u64(fh).u32(flags).u32(cmd).u64(0).u32(in_size).u32(out);
        c.call(op::IOCTL, node, &w.0)
    }
}
