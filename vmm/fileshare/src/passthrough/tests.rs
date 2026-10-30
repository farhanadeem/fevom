use std::os::unix::fs::{symlink, MetadataExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::atomic::Ordering;
use std::time::Duration;

use super::*;
use crate::abi::{ATTR_CALLER_OWNER, ATTR_NO_KEEP_CACHE, ATTR_OPEN_FREE, ATTR_REAL_OWNER};
use crate::client::Client;
use crate::server::Server;

struct Scratch(PathBuf);

impl Scratch {
    fn new(name: &str) -> Self {
        let dir = std::env::temp_dir().join(format!("fev-fs-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        Self(std::fs::canonicalize(dir).unwrap())
    }
    fn path(&self, rel: &str) -> PathBuf {
        self.0.join(rel)
    }
    fn write(&self, rel: &str, data: &[u8]) {
        let p = self.path(rel);
        std::fs::create_dir_all(p.parent().unwrap()).unwrap();
        std::fs::write(p, data).unwrap();
    }
    fn client(&self) -> Client {
        self.client_with(|_| {})
    }
    fn shared(&self) -> (Arc<Passthrough>, Client) {
        let fs = Arc::new(Passthrough::new(&Config::new(&self.0)).unwrap());
        let c = Client::new(Server::shared(fs.clone()));
        c.init();
        (fs, c)
    }
    fn client_with(&self, tune: impl FnOnce(&mut Config)) -> Client {
        let mut cfg = Config::new(&self.0);
        tune(&mut cfg);
        let c = Client::new(Server::new(Box::new(Passthrough::new(&cfg).unwrap())));
        c.init();
        c
    }
    fn client_with_handles(&self, tune: impl FnOnce(&mut Config)) -> Client {
        let mut cfg = Config::new(&self.0);
        tune(&mut cfg);
        let c = Client::new(Server::new(Box::new(Passthrough::new(&cfg).unwrap())));
        let offered = !(1u64 << 31) & !crate::abi::init::FEV_CACHE;
        c.init_with(crate::abi::MAJOR, crate::abi::MINOR, 128 << 10, offered)
            .unwrap();
        c
    }
}

fn opendir_err(c: &Client, node: u64) -> Option<Errno> {
    let fh = match c.opendir(node) {
        Ok(fh) => fh,
        Err(e) => return Some(e),
    };
    c.readdir(node, fh, 0, 4096, false).err()
}

impl Drop for Scratch {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

fn walk(c: &Client, path: &str) -> std::result::Result<Entry, Errno> {
    let mut node = ROOT_ID;
    let mut last = None;
    for part in path.split('/') {
        let e = c.lookup(node, part.as_bytes())?;
        node = e.nodeid;
        last = Some(e);
    }
    Ok(last.unwrap())
}

fn read_all(c: &Client, node: u64) -> Vec<u8> {
    let fh = c.open(node, 0).unwrap();
    let mut out = Vec::new();
    loop {
        let chunk = c.read(node, fh, out.len() as u64, 1 << 20).unwrap();
        if chunk.is_empty() {
            break;
        }
        out.extend(chunk);
    }
    c.release(node, fh).unwrap();
    out
}

fn names(entries: &[crate::client::DirEntry]) -> Vec<String> {
    let mut n: Vec<String> = entries
        .iter()
        .map(|e| String::from_utf8_lossy(&e.name).into_owned())
        .filter(|n| n != "." && n != "..")
        .collect();
    n.sort();
    n
}

fn case_sensitive(dir: &Path) -> bool {
    std::fs::write(dir.join("CaseProbe"), b"").unwrap();
    let folded = dir.join("caseprobe").exists();
    std::fs::remove_file(dir.join("CaseProbe")).unwrap();
    !folded
}

#[test]
fn files_read_back_with_the_mac_s_attributes() {
    let s = Scratch::new("read");
    let data: Vec<u8> = (0..3_000_000u32).map(|i| ((i * 31) >> 3) as u8).collect();
    s.write("dir/sub/blob", &data);
    s.write("empty", b"");
    std::fs::set_permissions(
        s.path("dir/sub/blob"),
        std::fs::Permissions::from_mode(0o640),
    )
    .unwrap();
    let c = s.client();

    let e = walk(&c, "dir/sub/blob").unwrap();
    let meta = std::fs::metadata(s.path("dir/sub/blob")).unwrap();
    assert_eq!(e.attr.size, 3_000_000);
    assert_eq!(e.attr.mode, 0o100640);
    assert_eq!(
        e.attr.ino,
        meta.ino(),
        "the Mac's inode number on the root volume"
    );
    assert_eq!(
        e.attr.mtime,
        (meta.mtime() as u64, meta.mtime_nsec() as u32)
    );
    assert_eq!(e.attr.nlink, 1);
    assert_eq!(e.attr.blocks, meta.blocks());
    assert_eq!(e.entry_valid, Duration::from_secs(1));
    assert_eq!(read_all(&c, e.nodeid), data);

    let fh = c.open(e.nodeid, 0).unwrap();
    assert_eq!(
        c.read(e.nodeid, fh, 2_999_990, 4096).unwrap(),
        &data[2_999_990..]
    );
    assert_eq!(c.read(e.nodeid, fh, 3_000_000, 4096).unwrap(), b"");
    assert_eq!(c.read(e.nodeid, fh, u64::MAX, 4096), Err(Errno::EINVAL));
    assert_eq!(
        c.getattr_valid(e.nodeid, Some(fh)).unwrap().0.size,
        3_000_000
    );
    c.release(e.nodeid, fh).unwrap();

    let empty = walk(&c, "empty").unwrap();
    assert_eq!(read_all(&c, empty.nodeid), b"");
    assert_eq!(c.lookup(ROOT_ID, b"absent").err(), Some(Errno::ENOENT));
    let root = c.getattr(ROOT_ID).unwrap();
    assert_eq!(root.mode & S_IFMT, S_IFDIR);
    let fsinfo = c.statfs(ROOT_ID).unwrap();
    assert!(fsinfo.blocks > 0 && fsinfo.bsize > 0);
    assert_eq!(fsinfo.namelen, 255);
}

#[test]
fn listings_match_the_mac_in_both_forms() {
    let s = Scratch::new("list");
    for i in 0..300 {
        s.write(&format!("many/f{i:03}"), format!("{i}").as_bytes());
    }
    std::fs::create_dir(s.path("many/subdir")).unwrap();
    symlink("f000", s.path("many/link")).unwrap();
    let c = s.client();
    let dir = walk(&c, "many").unwrap().nodeid;

    let mut want: Vec<String> = std::fs::read_dir(s.path("many"))
        .unwrap()
        .map(|e| e.unwrap().file_name().into_string().unwrap())
        .collect();
    want.sort();
    let plain = c.list(dir, false).unwrap();
    assert_eq!(names(&plain), want);
    assert!(
        plain.iter().any(|e| e.name == b"."),
        "dot entries are listed"
    );
    let by_name = |name: &[u8]| plain.iter().find(|e| e.name == name).unwrap().clone();
    assert_eq!(by_name(b"subdir").kind, 4);
    assert_eq!(by_name(b"link").kind, 10);
    assert_eq!(by_name(b"f007").kind, 8);
    assert_eq!(
        by_name(b"f007").ino,
        std::fs::metadata(s.path("many/f007")).unwrap().ino()
    );

    let plus = c.list(dir, true).unwrap();
    assert_eq!(names(&plus), want);
    for e in &plus {
        if e.name == b"." || e.name == b".." {
            assert!(e.entry.is_none());
            continue;
        }
        let entry = e.entry.expect("a lookup result per entry");
        let meta =
            std::fs::symlink_metadata(s.path("many").join(std::str::from_utf8(&e.name).unwrap()))
                .unwrap();
        assert_eq!(entry.attr.size, meta.len());
        assert_eq!(entry.attr.ino, meta.ino());
        assert_eq!(entry.attr.mode, meta.mode());
        assert_eq!(c.lookup(dir, &e.name).unwrap().nodeid, entry.nodeid);
    }
    s.write("many/late", b"x");
    assert!(names(&c.list(dir, true).unwrap()).contains(&"late".to_string()));
}

#[test]
fn symlinks_are_read_not_followed_and_hard_links_are_one_node() {
    let s = Scratch::new("links");
    s.write("target", b"content");
    symlink("target", s.path("rel")).unwrap();
    symlink("/etc/hosts", s.path("abs")).unwrap();
    symlink("nowhere", s.path("dangling")).unwrap();
    std::fs::hard_link(s.path("target"), s.path("second")).unwrap();
    std::fs::create_dir(s.path("d")).unwrap();
    symlink("d", s.path("dirlink")).unwrap();
    let c = s.client();

    for (name, target) in [
        ("rel", "target"),
        ("abs", "/etc/hosts"),
        ("dangling", "nowhere"),
    ] {
        let e = walk(&c, name).unwrap();
        assert_eq!(e.attr.mode & S_IFMT, S_IFLNK, "{name}");
        assert_eq!(e.attr.size, target.len() as u64);
        assert_eq!(c.readlink(e.nodeid).unwrap(), target.as_bytes());
        assert_eq!(c.open(e.nodeid, 0).err(), Some(Errno::ELOOP));
        assert_eq!(opendir_err(&c, e.nodeid), Some(Errno::ELOOP));
    }
    let dirlink = walk(&c, "dirlink").unwrap().nodeid;
    assert_eq!(c.lookup(dirlink, b"anything").err(), Some(Errno::ENOTDIR));
    let abs = walk(&c, "abs").unwrap().nodeid;
    assert_eq!(c.lookup(abs, b"hosts").err(), Some(Errno::ENOTDIR));

    let (a, b) = (walk(&c, "target").unwrap(), walk(&c, "second").unwrap());
    assert_eq!(a.nodeid, b.nodeid);
    assert_eq!(a.attr.nlink, 2);
    assert_eq!(c.readlink(a.nodeid).err(), Some(Errno::EINVAL));
}

#[test]
fn a_node_follows_its_file_through_a_mac_rename_and_dies_with_its_last_link() {
    let s = Scratch::new("ident");
    s.write("a/b/file", b"one");
    let c = s.client();
    let file = walk(&c, "a/b/file").unwrap().nodeid;
    let dir = walk(&c, "a/b").unwrap().nodeid;

    std::fs::rename(s.path("a/b/file"), s.path("a/b/renamed")).unwrap();
    std::fs::rename(s.path("a"), s.path("z")).unwrap();
    assert_eq!(c.getattr(file).unwrap().size, 3);
    assert_eq!(read_all(&c, file), b"one");
    assert_eq!(c.lookup(dir, b"renamed").unwrap().nodeid, file);
    assert_eq!(c.lookup(dir, b"file").err(), Some(Errno::ENOENT));

    let fh = c.open(file, 0).unwrap();
    std::fs::remove_file(s.path("z/b/renamed")).unwrap();
    assert_eq!(c.getattr(file).err(), Some(Errno::ESTALE));
    assert_eq!(c.open(file, 0).err(), Some(Errno::ESTALE));
    assert_eq!(c.read(file, fh, 0, 16).unwrap(), b"one");
    assert_eq!(c.getattr_valid(file, Some(fh)).unwrap().0.nlink, 0);
    c.release(file, fh).unwrap();

    s.write("z/b/renamed", b"two!");
    let again = c.lookup(dir, b"renamed").unwrap();
    assert_ne!(again.nodeid, file);
    assert_eq!(again.attr.size, 4);
}

#[test]
fn forget_drops_nodes_and_unknown_ids_are_stale() {
    let s = Scratch::new("forget");
    s.write("f", b"x");
    let c = s.client();
    let a = c.lookup(ROOT_ID, b"f").unwrap().nodeid;
    assert_eq!(c.lookup(ROOT_ID, b"f").unwrap().nodeid, a);
    c.forget(a, 1);
    assert!(c.getattr(a).is_ok());
    c.forget(a, 1);
    assert_eq!(c.getattr(a).err(), Some(Errno::ESTALE));
    c.forget(a, u64::MAX);
    c.forget(ROOT_ID, u64::MAX);
    c.batch_forget(&[(0, 1), (u64::MAX, 1), (12345, 9)]);
    assert!(c.getattr(ROOT_ID).is_ok());
    for id in [0, 2, 99, u64::MAX, 1 << 63] {
        assert_eq!(c.getattr(id).err(), Some(Errno::ESTALE), "{id}");
        assert_eq!(c.lookup(id, b"f").err(), Some(Errno::ESTALE));
        assert_eq!(opendir_err(&c, id), Some(Errno::ESTALE));
    }
    let listed = c.list(ROOT_ID, true).unwrap();
    let f = listed
        .iter()
        .find(|e| e.name == b"f")
        .unwrap()
        .entry
        .unwrap()
        .nodeid;
    c.forget(f, 1);
    assert_eq!(c.getattr(f).err(), Some(Errno::ESTALE));
}

#[test]
fn names_the_guest_may_not_use() {
    let s = Scratch::new("names");
    s.write("ok", b"");
    std::fs::create_dir(s.path("d")).unwrap();
    let c = s.client();
    for name in [&b"."[..], b"..", b"a/b", b"/", b"../ok", b"d/../ok"] {
        assert_eq!(
            c.lookup(ROOT_ID, name).err(),
            Some(Errno::EINVAL),
            "{name:?}"
        );
    }
    assert_eq!(
        c.lookup(ROOT_ID, b"bad\xff\xfename").err(),
        Some(Errno::ENOENT)
    );
    assert_eq!(
        c.lookup(ROOT_ID, &[b'x'; 256]).err(),
        Some(Errno::ENAMETOOLONG)
    );
    assert_eq!(c.lookup(ROOT_ID, &[b'x'; 255]).err(), Some(Errno::ENOENT));
}

#[test]
fn names_longer_than_linux_allows_are_hidden() {
    let s = Scratch::new("long");
    let long = "長".repeat(100);
    let fits = "長".repeat(85);
    s.write(&long, b"hidden");
    s.write(&fits, b"shown");
    s.write("plain", b"");
    let c = s.client();
    assert_eq!(names(&c.list(ROOT_ID, false).unwrap()), ["plain", &fits]);
    assert_eq!(names(&c.list(ROOT_ID, true).unwrap()), ["plain", &fits]);
    assert_eq!(
        c.lookup(ROOT_ID, long.as_bytes()).err(),
        Some(Errno::ENAMETOOLONG)
    );
    assert_eq!(
        read_all(&c, c.lookup(ROOT_ID, fits.as_bytes()).unwrap().nodeid),
        b"shown"
    );
}

#[test]
fn another_spelling_of_a_name_finds_the_file_and_is_not_cached() {
    let s = Scratch::new("case");
    s.write("Makefile", b"all:");
    s.write("caf\u{e9}", b"nfc");
    let c = s.client();
    let exact = c.lookup(ROOT_ID, b"Makefile").unwrap();
    assert_eq!(exact.entry_valid, Duration::from_secs(1));

    let nfd = c.lookup(ROOT_ID, "cafe\u{301}".as_bytes()).unwrap();
    let nfc = c.lookup(ROOT_ID, "caf\u{e9}".as_bytes()).unwrap();
    assert_eq!(nfd.nodeid, nfc.nodeid);
    assert_eq!(
        nfd.entry_valid,
        Duration::ZERO,
        "the other normalisation is not cached"
    );
    assert_eq!(nfc.entry_valid, Duration::from_secs(1));

    if case_sensitive(&s.0) {
        assert_eq!(c.lookup(ROOT_ID, b"makefile").err(), Some(Errno::ENOENT));
    } else {
        let alias = c.lookup(ROOT_ID, b"makefile").unwrap();
        assert_eq!(alias.nodeid, exact.nodeid, "one file, one node");
        assert_eq!(alias.entry_valid, Duration::ZERO, "an alias is not cached");
        assert_eq!(alias.attr.size, 4);
    }
    assert!(names(&c.list(ROOT_ID, true).unwrap()).contains(&"Makefile".to_string()));
}

#[test]
fn denied_paths_are_hidden_however_they_are_reached() {
    let s = Scratch::new("deny");
    s.write("data/disk.img", b"secret");
    s.write("data/inner/x", b"");
    s.write("run/sock", b"");
    s.write("visible/file", b"ok");
    std::fs::hard_link(s.path("data/disk.img"), s.path("visible/alias")).unwrap();
    symlink(s.path("run"), s.path("runlink")).unwrap();
    let c = s.client_with(|cfg| {
        cfg.deny = vec![s.path("data/disk.img"), s.path("runlink")];
    });

    assert_eq!(walk(&c, "data/disk.img").err(), Some(Errno::ENOENT));
    assert_eq!(walk(&c, "visible/alias").err(), Some(Errno::ENOENT));
    assert_eq!(
        walk(&c, "run").err(),
        Some(Errno::ENOENT),
        "the symlink's target is denied"
    );
    assert!(
        walk(&c, "runlink").is_ok(),
        "the link itself is only a name"
    );
    let data = walk(&c, "data").unwrap().nodeid;
    assert_eq!(names(&c.list(data, false).unwrap()), ["inner"]);
    assert_eq!(names(&c.list(data, true).unwrap()), ["inner"]);
    assert_eq!(
        names(&c.list(ROOT_ID, true).unwrap()),
        ["data", "runlink", "visible"]
    );
    let visible = walk(&c, "visible").unwrap().nodeid;
    assert_eq!(names(&c.list(visible, true).unwrap()), ["file"]);

    std::fs::rename(s.path("run"), s.path("visible/moved")).unwrap();
    assert_eq!(walk(&c, "visible/moved").err(), Some(Errno::ENOENT));
    if !case_sensitive(&s.0) {
        assert_eq!(walk(&c, "DATA/DISK.IMG").err(), Some(Errno::ENOENT));
    }
    let mut cfg = Config::new(&s.0);
    cfg.deny = vec![s.path("no-such")];
    assert!(Passthrough::new(&cfg).is_err());
}

#[test]
fn the_mac_s_volfs_is_refused_at_the_root() {
    let c = Client::new(Server::new(Box::new(
        Passthrough::new(&Config::new("/")).unwrap(),
    )));
    c.init();
    for name in [&b".vol"[..], b".VOL", b".Vol"] {
        assert_eq!(
            c.lookup(ROOT_ID, name).err(),
            Some(Errno::ENOENT),
            "{name:?}"
        );
    }
    let listed = names(&c.list(ROOT_ID, true).unwrap());
    assert!(
        !listed.iter().any(|n| n.eq_ignore_ascii_case(".vol")),
        "{listed:?}"
    );
    assert!(!names(&c.list(ROOT_ID, false).unwrap())
        .iter()
        .any(|n| n.eq_ignore_ascii_case(".vol")));
    let hosts = walk(&c, "etc").unwrap();
    assert_eq!(hosts.attr.mode & S_IFMT, S_IFLNK);
    let sh = walk(&c, "bin/sh").unwrap();
    let fh = c.open(sh.nodeid, 0).unwrap();
    assert_eq!(c.read(sh.nodeid, fh, 0, 4).unwrap().len(), 4);
    c.release(sh.nodeid, fh).unwrap();
}

#[test]
fn open_handles_are_bounded_and_released() {
    let s = Scratch::new("handles");
    s.write("f", b"data");
    std::fs::create_dir(s.path("d")).unwrap();
    let c = s.client_with_handles(|cfg| cfg.max_handles = Some(8));
    let f = c.lookup(ROOT_ID, b"f").unwrap().nodeid;
    let d = c.lookup(ROOT_ID, b"d").unwrap().nodeid;
    let mut held: Vec<u64> = (0..6).map(|_| c.open(f, 0).unwrap()).collect();
    let dirs: Vec<u64> = (0..2).map(|_| c.opendir(d).unwrap()).collect();
    assert_eq!(c.open(f, 0).err(), Some(Errno::ENFILE));
    assert_eq!(c.opendir(d).err(), Some(Errno::ENFILE));
    c.release(f, held.pop().unwrap()).unwrap();
    held.push(c.open(f, 0).unwrap());

    assert_eq!(c.read(d, held[0], 0, 4), Err(Errno::EBADF));
    assert_eq!(c.read(d, dirs[0], 0, 4), Err(Errno::EISDIR));
    assert_eq!(
        c.readdir(f, held[0], 0, 4096, false).err(),
        Some(Errno::ENOTDIR)
    );
    assert_eq!(c.readdir(d, 999, 0, 4096, false).err(), Some(Errno::EBADF));
    c.release(f, held[0]).unwrap();
    c.release(f, held[0]).unwrap();
    c.release(f, 12345).unwrap();
    assert_eq!(c.read(f, held[0], 0, 4), Err(Errno::EBADF));

    let before = fds_under(&s.0);
    for i in 0..500 {
        s.write(&format!("many/{i}"), b"");
    }
    let many = walk(&c, "many").unwrap().nodeid;
    assert_eq!(c.list(many, true).unwrap().len(), 502);
    assert_eq!(
        fds_under(&s.0),
        before,
        "listing 500 files left descriptors open"
    );
}

#[test]
fn a_guest_that_never_forgets_meets_a_bound_not_the_vmm_s_memory() {
    let s = Scratch::new("nodes");
    for i in 0..20 {
        s.write(&format!("f{i:02}"), b"");
    }
    let c = s.client_with(|cfg| cfg.max_nodes = Some(10));
    let got: Vec<_> = (0..20)
        .map(|i| c.lookup(ROOT_ID, format!("f{i:02}").as_bytes()))
        .collect();
    assert_eq!(got.iter().filter(|r| r.is_ok()).count(), 9);
    assert_eq!(got[9].as_ref().err(), Some(&Errno::ENOMEM));
    assert!(c.lookup(ROOT_ID, b"f00").is_ok());
    c.forget(got[0].as_ref().unwrap().nodeid, 2);
    assert!(c.lookup(ROOT_ID, b"f15").is_ok());
    assert_eq!(names(&c.list(ROOT_ID, false).unwrap()).len(), 20);
    let plus = c.list(ROOT_ID, true).unwrap();
    assert!(plus.iter().filter(|e| e.entry.is_some()).count() <= 9);
}

fn fds_under(dir: &Path) -> usize {
    open_under(dir, false)
}

fn dirs_under(dir: &Path) -> usize {
    open_under(dir, true)
}

fn open_under(dir: &Path, directories: bool) -> usize {
    let dir = dir.as_os_str().as_bytes();
    (0..16384)
        .filter(|fd| {
            let mut buf = [0u8; 1024];
            if unsafe { libc::fcntl(*fd, libc::F_GETPATH, buf.as_mut_ptr()) } != 0 {
                return false;
            }
            let end = buf.iter().position(|b| *b == 0).unwrap_or(0);
            buf[..end].starts_with(dir)
                && fstat(*fd).is_ok_and(|st| (st.st_mode as u32 & S_IFMT == S_IFDIR) == directories)
        })
        .count()
}

fn fds_settled(dir: &Path, want: usize) -> usize {
    for _ in 0..2000 {
        if fds_under(dir) <= want {
            break;
        }
        std::thread::sleep(Duration::from_millis(1));
    }
    fds_under(dir)
}

#[test]
fn the_share_root_must_be_a_directory() {
    let s = Scratch::new("root");
    s.write("file", b"");
    assert!(Passthrough::new(&Config::new(s.path("file"))).is_err());
    assert!(Passthrough::new(&Config::new(s.path("absent"))).is_err());
    symlink(&s.0, s.path("self")).unwrap();
    let fs = Passthrough::new(&Config::new(s.path("self"))).unwrap();
    assert_eq!(fs.known_nodes(), 1);
}

#[test]
fn device_numbers_are_recoded_for_linux() {
    assert_eq!(linux_rdev((3 << 24) | 2), 0x302);
    assert_eq!(
        linux_rdev((1 << 24) | 0x1234),
        0x100 | 0x34 | (0x1200 << 12)
    );
}

#[test]
fn a_file_the_vmm_holds_open_is_not_reachable_through_dev_fd() {
    let s = Scratch::new("devfd");
    s.write("data/disk.img", b"the VM's own disk");
    s.write("data/inner/x", b"inner");
    let held = std::fs::File::open(s.path("data/disk.img")).unwrap();
    let held_dir = std::fs::File::open(s.path("data")).unwrap();
    let mut cfg = Config::new("/");
    cfg.deny = vec![s.path("data")];
    let c = Client::new(Server::new(Box::new(Passthrough::new(&cfg).unwrap())));
    c.init();

    let dev = walk(&c, "dev").unwrap();
    assert_eq!(dev.attr.mode & S_IFMT, S_IFDIR);
    assert_eq!(names(&c.list(dev.nodeid, false).unwrap()), [""; 0]);
    assert_eq!(names(&c.list(dev.nodeid, true).unwrap()), [""; 0]);
    for name in ["fd", "null", "stdin"] {
        assert_eq!(
            c.lookup(dev.nodeid, name.as_bytes()).err(),
            Some(Errno::ENOENT),
            "{name}"
        );
    }
    use std::os::fd::AsRawFd;
    for fd in [held.as_raw_fd(), held_dir.as_raw_fd()] {
        assert_eq!(walk(&c, &format!("dev/fd/{fd}")).err(), Some(Errno::ENOENT));
        assert_eq!(
            walk(&c, &format!("dev/fd/{fd}/inner/x")).err(),
            Some(Errno::ENOENT)
        );
    }
}

#[test]
fn an_open_lands_on_the_node_s_own_device_or_fails() {
    let s = Scratch::new("opendev");
    s.write("secret", b"not shared");
    let held = std::fs::File::open(s.path("secret")).unwrap();
    use std::os::fd::AsRawFd;
    let name = held.as_raw_fd().to_string();
    let fs = Passthrough::new(&Config::new("/dev/fd"));
    let Ok(fs) = fs else { return };
    let c = Client::new(Server::new(Box::new(fs)));
    c.init();
    if let Ok(e) = c.lookup(ROOT_ID, name.as_bytes()) {
        assert!(
            c.open(e.nodeid, 0).is_err(),
            "the VMM's descriptor was handed over"
        );
    }
}

#[test]
fn statfs_of_a_symlink_does_not_follow_it() {
    let s = Scratch::new("statfs");
    symlink("/dev", s.path("elsewhere")).unwrap();
    let c = s.client();
    let here = c.statfs(ROOT_ID).unwrap();
    let link = walk(&c, "elsewhere").unwrap().nodeid;
    let got = c.statfs(link).unwrap();
    assert_eq!((got.blocks, got.bsize), (here.blocks, here.bsize));
}

#[test]
fn listings_held_open_are_bounded_too() {
    let s = Scratch::new("listed");
    for i in 0..60 {
        s.write(&format!("d/f{i:02}"), b"");
    }
    let c = s.client_with_handles(|cfg| cfg.max_listed = Some(150));
    let d = c.lookup(ROOT_ID, b"d").unwrap().nodeid;
    let a = c.opendir(d).unwrap();
    let b = c.opendir(d).unwrap();
    let third = c.opendir(d).unwrap();
    assert_eq!(c.readdir(d, a, 0, 1 << 16, false).unwrap().len(), 62);
    assert_eq!(c.readdir(d, b, 0, 1 << 16, false).unwrap().len(), 62);
    assert_eq!(
        c.readdir(d, third, 0, 1 << 16, false).err(),
        Some(Errno::ENOMEM)
    );
    assert_eq!(c.readdir(d, a, 0, 1 << 16, true).unwrap().len(), 62);
    c.releasedir(d, b).unwrap();
    assert_eq!(c.readdir(d, third, 0, 1 << 16, false).unwrap().len(), 62);
    let small = s.client_with_handles(|cfg| cfg.max_listed = Some(10));
    let d = small.lookup(ROOT_ID, b"d").unwrap().nodeid;
    let h = small.opendir(d).unwrap();
    assert_eq!(
        small.readdir(d, h, 0, 1 << 16, false).err(),
        Some(Errno::ENOMEM)
    );
    c.call(crate::abi::op::DESTROY, 0, &[]).unwrap();
    c.init();
    let d = c.lookup(ROOT_ID, b"d").unwrap().nodeid;
    let (a, b) = (c.opendir(d).unwrap(), c.opendir(d).unwrap());
    assert_eq!(c.readdir(d, a, 0, 1 << 16, false).unwrap().len(), 62);
    assert_eq!(c.readdir(d, b, 0, 1 << 16, false).unwrap().len(), 62);
}

fn tree_client(s: &Scratch) -> Client {
    s.client_with(|cfg| cfg.no_volfs = true)
}

#[test]
fn scheme_two_reads_a_tree_and_drops_parents_with_their_last_child() {
    let s = Scratch::new("tree");
    s.write("a/b/c/file", b"deep");
    symlink("file", s.path("a/b/c/link")).unwrap();
    let fs = Passthrough::new(&{
        let mut cfg = Config::new(&s.0);
        cfg.no_volfs = true;
        cfg
    })
    .unwrap();
    let c = Client::new(Server::new(Box::new(fs)));
    c.init();
    let a = c.lookup(ROOT_ID, b"a").unwrap().nodeid;
    let b = c.lookup(a, b"b").unwrap().nodeid;
    let dir = c.lookup(b, b"c").unwrap().nodeid;
    let file = c.lookup(dir, b"file").unwrap();
    assert_eq!(read_all(&c, file.nodeid), b"deep");
    let link = c.lookup(dir, b"link").unwrap().nodeid;
    assert_eq!(c.readlink(link).unwrap(), b"file");
    assert_eq!(names(&c.list(dir, true).unwrap()), ["file", "link"]);
    c.forget(a, 1);
    c.forget(b, 1);
    assert_eq!(c.getattr(file.nodeid).unwrap().size, 4);
    assert!(c.getattr(a).is_ok());
    c.forget(file.nodeid, 2);
    c.forget(link, 2);
    c.forget(dir, 1);
    for id in [a, b, dir, file.nodeid, link] {
        assert_eq!(c.getattr(id).err(), Some(Errno::ESTALE), "{id}");
    }
    s.write("a/b/c/file", b"deep");
    let file = walk(&c, "a/b/c/file").unwrap().nodeid;
    std::fs::rename(s.path("a"), s.path("z")).unwrap();
    assert_eq!(c.getattr(file).err(), Some(Errno::ENOENT));
    assert_eq!(
        read_all(&c, walk(&c, "z/b/c/file").unwrap().nodeid),
        b"deep"
    );
}

#[test]
fn scheme_two_survives_a_name_changing_its_type() {
    let s = Scratch::new("retype");
    s.write("thing", b"a file");
    let c = tree_client(&s);
    let was = c.lookup(ROOT_ID, b"thing").unwrap();
    assert_eq!(was.attr.mode & S_IFMT, S_IFREG);
    std::fs::remove_file(s.path("thing")).unwrap();
    s.write("thing/inside", b"now a directory");
    assert_eq!(c.getattr(was.nodeid).err(), Some(Errno::ESTALE));
    let now = c.lookup(ROOT_ID, b"thing").unwrap();
    assert_eq!(now.attr.mode & S_IFMT, S_IFDIR);
    assert_eq!(c.getattr(now.nodeid).unwrap().mode & S_IFMT, S_IFDIR);
    assert_eq!(names(&c.list(now.nodeid, true).unwrap()), ["inside"]);
    let inside = c.lookup(now.nodeid, b"inside").unwrap().nodeid;
    assert_eq!(read_all(&c, inside), b"now a directory");
}

#[test]
fn a_listing_names_what_it_has_no_node_left_for() {
    let s = Scratch::new("fullnames");
    for i in 0..20 {
        s.write(&format!("f{i:02}"), b"");
    }
    let c = s.client_with(|cfg| cfg.max_nodes = Some(10));
    let plus = c.list(ROOT_ID, true).unwrap();
    assert_eq!(names(&plus).len(), 20);
    assert_eq!(plus.iter().filter(|e| e.entry.is_some()).count(), 9);
}

#[test]
fn scheme_two_walks_through_no_link_put_in_its_path() {
    let s = Scratch::new("swap");
    s.write("a/b/x", b"plain");
    s.write("data/b/secret", b"hidden");
    s.write("data/b/x", b"other");
    symlink("secret", s.path("data/b/link")).unwrap();
    let c = s.client_with_handles(|cfg| {
        cfg.no_volfs = true;
        cfg.deny = vec![s.path("data")];
    });
    let b = walk(&c, "a/b").unwrap().nodeid;
    let x = c.lookup(b, b"x").unwrap().nodeid;
    let dir = c.opendir(b).unwrap();
    std::fs::rename(s.path("a"), s.path("gone")).unwrap();
    symlink("data", s.path("a")).unwrap();

    assert_eq!(c.lookup(b, b"secret").err(), Some(Errno::ESTALE));
    assert_eq!(c.getattr(x).err(), Some(Errno::ESTALE));
    assert_eq!(c.open(x, 0).err(), Some(Errno::ESTALE));
    assert_eq!(opendir_err(&c, b), Some(Errno::ESTALE));
    assert_eq!(c.statfs(b).err(), Some(Errno::ESTALE));
    assert_eq!(names(&c.list(ROOT_ID, false).unwrap()), ["a", "gone"]);
    assert_eq!(
        names(&c.readdir(b, dir, 0, 4096, true).unwrap()),
        ["x"],
        "the open directory is the old one"
    );
    let a = c.lookup(ROOT_ID, b"a").unwrap();
    assert_eq!(a.attr.mode & S_IFMT, S_IFLNK);
    assert_eq!(c.readlink(a.nodeid).unwrap(), b"data");
}

#[test]
fn a_read_into_thousands_of_small_buffers_is_whole_and_in_order() {
    let s = Scratch::new("pieces");
    let data: Vec<u8> = (0..10_000u32).map(|i| (i * 7 + i / 251) as u8).collect();
    s.write("f", &data);
    let fs = Passthrough::new(&Config::new(&s.0)).unwrap();
    let ctx = Ctx::default();
    let node = fs.lookup(&ctx, ROOT_ID, b"f").unwrap().nodeid;
    let (fh, _) = fs.open(&ctx, node, 0, false).unwrap();
    for (len, piece, offset) in [
        (10_000usize, 3usize, 0u64),
        (10_000, 1, 0),
        (9_000, 7, 500),
        (4_000, 5, 7_000),
        (4_000, 5, 9_999),
        (100, 1, 10_000),
    ] {
        let buf = crate::iov::OwnedIov::new(len, piece);
        let got = fs.read(&ctx, node, fh, offset, buf.iov()).unwrap();
        let want = &data[offset as usize..data.len().min(offset as usize + len)];
        assert_eq!(
            got,
            want.len(),
            "{len} bytes in pieces of {piece} at {offset}"
        );
        assert_eq!(buf.bytes(got), want);
    }
}

const O_WRONLY: u32 = 1;
const O_RDWR: u32 = 2;
const O_EXCL: u32 = 0o200;
const O_TRUNC: u32 = 0o1000;
const O_APPEND: u32 = 0o2000;

fn owner_bits(a: &Attr) -> u32 {
    a.flags & (ATTR_CALLER_OWNER | ATTR_REAL_OWNER)
}

#[test]
fn every_reply_that_carries_attributes_says_whose_the_file_is() {
    let s = Scratch::new("ownerflag");
    s.write("mine", b"x");
    let c = s.client();
    let me = unsafe { libc::geteuid() };
    let mut seen: Vec<(&str, Attr)> = Vec::new();

    seen.push(("LOOKUP", c.lookup(ROOT_ID, b"mine").unwrap().attr));
    let (made, fh) = c.create(ROOT_ID, b"made", 0o644, O_WRONLY).unwrap();
    seen.push(("CREATE", made.attr));
    seen.push((
        "MKNOD fifo",
        c.mknod(ROOT_ID, b"fifo", S_IFIFO | 0o644, 0).unwrap().attr,
    ));
    seen.push((
        "MKNOD device",
        c.mknod(ROOT_ID, b"dev", S_IFCHR | 0o600, 0x103)
            .unwrap()
            .attr,
    ));
    seen.push(("MKDIR", c.mkdir(ROOT_ID, b"dir", 0o755).unwrap().attr));
    seen.push(("SYMLINK", c.symlink(ROOT_ID, b"sym", b"mine").unwrap().attr));
    seen.push(("LINK", c.link(made.nodeid, ROOT_ID, b"again").unwrap().attr));
    for e in c.list(ROOT_ID, true).unwrap() {
        if let Some(entry) = e.entry {
            seen.push(("READDIRPLUS entry", entry.attr));
        }
    }
    seen.push(("GETATTR of the root", c.getattr(ROOT_ID).unwrap()));
    seen.push(("GETATTR", c.getattr(made.nodeid).unwrap()));
    seen.push((
        "GETATTR by handle",
        c.getattr_valid(made.nodeid, Some(fh)).unwrap().0,
    ));
    seen.push(("SETATTR", c.chmod(made.nodeid, 0o600).unwrap()));
    seen.push((
        "SETATTR by handle",
        c.truncate(made.nodeid, Some(fh), 5).unwrap(),
    ));
    assert!(seen.len() >= 18, "{}", seen.len());
    for (what, attr) in &seen {
        assert_eq!(owner_bits(attr), ATTR_CALLER_OWNER, "{what}");
        assert_eq!(attr.uid, me, "{what}");
    }
    for opcode in [crate::abi::op::TMPFILE, crate::abi::op::STATX] {
        assert_eq!(c.call(opcode, ROOT_ID, &[0; 64]).err(), Some(Errno::ENOSYS));
    }
    for uid in [0, 1000, 65534] {
        c.uid.store(uid, Ordering::Relaxed);
        let (attr, valid) = c.getattr_valid(made.nodeid, None).unwrap();
        assert_eq!((attr.uid, owner_bits(&attr)), (me, ATTR_CALLER_OWNER));
        assert_eq!(valid, Duration::from_secs(1));
        let e = c.lookup(ROOT_ID, b"mine").unwrap();
        assert_eq!(e.attr_valid, Duration::from_secs(1));
    }

    let other = Client::new(Server::new(Box::new(
        Passthrough::new(&Config::new("/usr/bin")).unwrap(),
    )));
    other.init();
    let mut theirs = vec![
        other.getattr(ROOT_ID).unwrap(),
        other.lookup(ROOT_ID, b"true").unwrap().attr,
    ];
    theirs.push(
        other
            .getattr(other.lookup(ROOT_ID, b"true").unwrap().nodeid)
            .unwrap(),
    );
    theirs.extend(
        other
            .list(ROOT_ID, true)
            .unwrap()
            .iter()
            .filter_map(|e| e.entry)
            .take(20)
            .map(|e| e.attr),
    );
    assert_eq!(theirs[1].uid, 0);
    for attr in &theirs {
        assert_eq!(owner_bits(attr), ATTR_REAL_OWNER);
        assert_ne!(attr.uid, me, "the Mac's own ids are shown");
    }
}

#[test]
fn files_are_made_written_cut_and_read_back() {
    let s = Scratch::new("write");
    let c = s.client();
    let (e, fh) = c.create(ROOT_ID, b"new", 0o640, O_RDWR | O_EXCL).unwrap();
    assert_eq!(
        e.attr.mode, 0o100640,
        "the guest's mode, not the VMM's umask"
    );
    assert_eq!(c.write(e.nodeid, fh, 0, b"hello, ").unwrap(), 7);
    assert_eq!(c.write(e.nodeid, fh, 7, b"mac").unwrap(), 3);
    assert_eq!(std::fs::read(s.path("new")).unwrap(), b"hello, mac");
    assert_eq!(c.read(e.nodeid, fh, 0, 64).unwrap(), b"hello, mac");
    let big: Vec<u8> = (0..1 << 20).map(|i| ((i * 13) >> 2) as u8).collect();
    assert_eq!(c.write(e.nodeid, fh, 100, &big).unwrap(), 1 << 20);
    let back = std::fs::read(s.path("new")).unwrap();
    assert_eq!(back.len(), 100 + (1 << 20));
    assert_eq!(&back[100..], &big[..]);
    assert!(back[10..100].iter().all(|b| *b == 0));
    assert_eq!(c.truncate(e.nodeid, Some(fh), 4).unwrap().size, 4);
    assert_eq!(std::fs::read(s.path("new")).unwrap(), b"hell");
    assert_eq!(c.truncate(e.nodeid, None, 2).unwrap().size, 2);
    assert_eq!(c.truncate(e.nodeid, None, 6).unwrap().size, 6);
    assert_eq!(std::fs::read(s.path("new")).unwrap(), b"he\0\0\0\0");
    c.fsync(e.nodeid, fh, false).unwrap();
    c.release(e.nodeid, fh).unwrap();
    assert_eq!(c.write(e.nodeid, fh, 0, b"x"), Err(Errno::EBADF));

    assert_eq!(
        c.create(ROOT_ID, b"new", 0o600, O_WRONLY | O_EXCL).err(),
        Some(Errno::EEXIST)
    );
    let (again, fh) = c.create(ROOT_ID, b"new", 0o600, O_WRONLY).unwrap();
    assert_eq!((again.nodeid, again.attr.size), (e.nodeid, 6));
    assert_eq!(again.attr.mode, 0o100640, "an existing file keeps its mode");
    c.release(e.nodeid, fh).unwrap();
    let fh = c.open(e.nodeid, O_WRONLY | O_TRUNC).unwrap();
    assert_eq!(c.getattr(e.nodeid).unwrap().size, 0);
    c.write(e.nodeid, fh, 0, b"again").unwrap();
    let cut = c.open(e.nodeid, O_TRUNC).unwrap();
    assert_eq!(c.getattr(e.nodeid).unwrap().size, 0);
    c.release(e.nodeid, cut).unwrap();
    c.write(e.nodeid, fh, 0, b"again").unwrap();
    let (again, cut) = c.create(ROOT_ID, b"new", 0o600, O_TRUNC).unwrap();
    assert_eq!(again.attr.size, 0);
    c.release(e.nodeid, cut).unwrap();
    assert_eq!(c.read(e.nodeid, fh, 0, 4), Err(Errno::EBADF));
    c.release(e.nodeid, fh).unwrap();
    let ro = c.open(e.nodeid, 0).unwrap();
    assert_eq!(c.write(e.nodeid, ro, 0, b"x"), Err(Errno::EBADF));
    c.release(e.nodeid, ro).unwrap();

    std::fs::write(s.path("log"), b"one\n").unwrap();
    let log = c.lookup(ROOT_ID, b"log").unwrap().nodeid;
    let fh = c.open(log, O_WRONLY | O_APPEND).unwrap();
    std::fs::OpenOptions::new()
        .append(true)
        .open(s.path("log"))
        .and_then(|mut f| std::io::Write::write_all(&mut f, b"two\n"))
        .unwrap();
    assert_eq!(c.write(log, fh, 4, b"three\n").unwrap(), 6);
    assert_eq!(std::fs::read(s.path("log")).unwrap(), b"one\ntwo\nthree\n");
    assert_eq!(
        c.write_flags(log, fh, 0, b"ONE", crate::abi::WRITE_CACHE)
            .unwrap(),
        3
    );
    assert_eq!(std::fs::read(s.path("log")).unwrap(), b"ONE\ntwo\nthree\n");
    c.fsync(log, fh, false).unwrap();
    c.release(log, fh).unwrap();

    assert_eq!(
        c.create(ROOT_ID, b"bad\xff\xfe", 0o644, O_WRONLY).err(),
        Some(Errno::EILSEQ)
    );
    assert_eq!(
        c.create(ROOT_ID, &[b'n'; 256], 0o644, O_WRONLY).err(),
        Some(Errno::ENAMETOOLONG)
    );
    for name in [&b".."[..], b".", b"a/b"] {
        assert_eq!(c.mkdir(ROOT_ID, name, 0o755).err(), Some(Errno::EINVAL));
        assert_eq!(c.unlink(ROOT_ID, name).err(), Some(Errno::EINVAL));
    }
}

#[test]
fn a_file_unlinked_while_open_lives_until_its_last_handle() {
    let s = Scratch::new("unlinked");
    let c = s.client();
    let (e, fh) = c.create(ROOT_ID, b"tmp", 0o600, O_RDWR).unwrap();
    c.write(e.nodeid, fh, 0, b"still here").unwrap();
    c.unlink(ROOT_ID, b"tmp").unwrap();
    assert!(!s.path("tmp").exists());
    assert_eq!(c.lookup(ROOT_ID, b"tmp").err(), Some(Errno::ENOENT));
    assert_eq!(c.read(e.nodeid, fh, 0, 64).unwrap(), b"still here");
    c.write(e.nodeid, fh, 10, b"!").unwrap();
    let by_fh = c.getattr_valid(e.nodeid, Some(fh)).unwrap().0;
    assert_eq!((by_fh.size, by_fh.nlink), (11, 0));
    let by_node = c.getattr(e.nodeid).unwrap();
    assert_eq!((by_node.size, by_node.nlink), (11, 0));
    assert_eq!(c.chmod(e.nodeid, 0o400).unwrap().mode, 0o100400);
    let t = c
        .setattr(
            e.nodeid,
            crate::abi::fattr::MTIME,
            0,
            0,
            0,
            (0, 0),
            (1_000_000, 7),
        )
        .unwrap();
    assert_eq!(t.mtime, (1_000_000, 7));
    assert_eq!(c.truncate(e.nodeid, Some(fh), 3).unwrap().size, 3);
    assert_eq!(fds_under(&s.0), 2, "the guest's handle and the one kept");
    c.release(e.nodeid, fh).unwrap();
    assert_eq!(fds_settled(&s.0, 1), 1);
    c.forget(e.nodeid, 1);
    assert_eq!(fds_under(&s.0), 0);
    assert_eq!(c.getattr(e.nodeid).err(), Some(Errno::ESTALE));

    let (old, fh) = c.create(ROOT_ID, b"target", 0o644, O_RDWR).unwrap();
    c.write(old.nodeid, fh, 0, b"old").unwrap();
    let (new, nfh) = c.create(ROOT_ID, b"target.tmp", 0o644, O_WRONLY).unwrap();
    c.write(new.nodeid, nfh, 0, b"new content").unwrap();
    c.release(new.nodeid, nfh).unwrap();
    c.rename(ROOT_ID, b"target.tmp", ROOT_ID, b"target", 0)
        .unwrap();
    assert_eq!(std::fs::read(s.path("target")).unwrap(), b"new content");
    assert_eq!(c.lookup(ROOT_ID, b"target").unwrap().nodeid, new.nodeid);
    assert_eq!(c.read(old.nodeid, fh, 0, 16).unwrap(), b"old");
    assert_eq!(c.getattr(old.nodeid).unwrap().nlink, 0);
    c.release(old.nodeid, fh).unwrap();
    let (two, fh) = c.create(ROOT_ID, b"two", 0o644, O_RDWR).unwrap();
    c.link(two.nodeid, ROOT_ID, b"two-b").unwrap();
    c.unlink(ROOT_ID, b"two").unwrap();
    assert_eq!(c.getattr(two.nodeid).unwrap().nlink, 1);
    c.release(two.nodeid, fh).unwrap();
}

#[test]
fn directories_links_and_renames() {
    let s = Scratch::new("names2");
    let c = s.client();
    let d = c.mkdir(ROOT_ID, b"d", 0o2775).unwrap();
    assert_eq!(d.attr.mode, 0o042755, "mode less the guest's umask of 022");
    assert_eq!(c.mkdir(ROOT_ID, b"d", 0o755).err(), Some(Errno::EEXIST));
    let (f, fh) = c.create(d.nodeid, b"f", 0o644, O_WRONLY).unwrap();
    c.write(f.nodeid, fh, 0, b"content").unwrap();
    c.release(f.nodeid, fh).unwrap();
    assert_eq!(c.rmdir(ROOT_ID, b"d").err(), Some(Errno(39)), "ENOTEMPTY");

    let l = c.symlink(d.nodeid, b"l", b"../../etc/passwd").unwrap();
    assert_eq!(l.attr.mode & S_IFMT, S_IFLNK);
    assert_eq!(c.readlink(l.nodeid).unwrap(), b"../../etc/passwd");
    assert_eq!(
        std::fs::read_link(s.path("d/l")).unwrap().to_str(),
        Some("../../etc/passwd")
    );
    let h = c.link(f.nodeid, ROOT_ID, b"hard").unwrap();
    assert_eq!((h.nodeid, h.attr.nlink), (f.nodeid, 2));
    let hl = c.link(l.nodeid, ROOT_ID, b"hard-l").unwrap();
    assert_eq!(hl.attr.mode & S_IFMT, S_IFLNK);
    assert_eq!(
        c.link(d.nodeid, ROOT_ID, b"hard-d").err(),
        Some(Errno::EPERM)
    );
    assert_eq!(
        c.link(f.nodeid, ROOT_ID, b"hard").err(),
        Some(Errno::EEXIST)
    );

    c.rename(d.nodeid, b"f", ROOT_ID, b"moved", 0).unwrap();
    assert_eq!(c.lookup(ROOT_ID, b"moved").unwrap().nodeid, f.nodeid);
    assert_eq!(c.lookup(d.nodeid, b"f").err(), Some(Errno::ENOENT));
    s.write("other", b"other");
    let other = c.lookup(ROOT_ID, b"other").unwrap().nodeid;
    assert_eq!(
        c.rename(ROOT_ID, b"moved", ROOT_ID, b"other", 1).err(),
        Some(Errno::EEXIST)
    );
    c.rename(ROOT_ID, b"moved", ROOT_ID, b"other", 2).unwrap();
    assert_eq!(std::fs::read(s.path("moved")).unwrap(), b"other");
    assert_eq!(std::fs::read(s.path("other")).unwrap(), b"content");
    assert_eq!(c.lookup(ROOT_ID, b"moved").unwrap().nodeid, other);
    assert_eq!(c.lookup(ROOT_ID, b"other").unwrap().nodeid, f.nodeid);
    c.rename(ROOT_ID, b"moved", ROOT_ID, b"fresh", 1).unwrap();
    c.rename(ROOT_ID, b"fresh", ROOT_ID, b"other", 0).unwrap();
    assert_eq!(std::fs::read(s.path("other")).unwrap(), b"other");
    assert_eq!(
        c.getattr(f.nodeid).unwrap().nlink,
        1,
        "its other name is left"
    );
    for flags in [4, 3, 8] {
        assert_eq!(
            c.rename(ROOT_ID, b"other", ROOT_ID, b"x", flags).err(),
            Some(Errno::EINVAL)
        );
    }
    assert_eq!(
        c.rename(ROOT_ID, b"absent", ROOT_ID, b"x", 0).err(),
        Some(Errno::ENOENT)
    );
    let sub = c.mkdir(d.nodeid, b"sub", 0o755).unwrap().nodeid;
    c.rename(ROOT_ID, b"d", ROOT_ID, b"e", 0).unwrap();
    assert_eq!(c.lookup(ROOT_ID, b"e").unwrap().nodeid, d.nodeid);
    assert_eq!(c.lookup(d.nodeid, b"sub").unwrap().nodeid, sub);
    assert_eq!(
        c.rename(ROOT_ID, b"other", ROOT_ID, b"e", 0).err(),
        Some(Errno::EISDIR)
    );

    c.unlink(d.nodeid, b"l").unwrap();
    c.rmdir(d.nodeid, b"sub").unwrap();
    c.rmdir(ROOT_ID, b"e").unwrap();
    assert!(!s.path("e").exists());
    assert_eq!(c.getattr(d.nodeid).err(), Some(Errno::ENOENT));
}

#[test]
fn modes_times_and_a_chown_that_stores_nothing() {
    let s = Scratch::new("attrs");
    s.write("f", b"data");
    std::fs::create_dir(s.path("d")).unwrap();
    symlink("f", s.path("l")).unwrap();
    let c = s.client();
    let f = c.lookup(ROOT_ID, b"f").unwrap().nodeid;
    let me = unsafe { libc::geteuid() };
    for mode in [0o600, 0o4755, 0o2711, 0o1777, 0o000, 0o644] {
        assert_eq!(c.chmod(f, mode).unwrap().mode, 0o100000 | mode);
        assert_eq!(
            std::fs::metadata(s.path("f")).unwrap().mode() & 0o7777,
            mode,
            "a guest chmod is a chmod on the Mac"
        );
    }
    let d = c.lookup(ROOT_ID, b"d").unwrap().nodeid;
    assert_eq!(c.chmod(d, 0o1770).unwrap().mode, 0o041770);
    use crate::abi::fattr;
    let a = c
        .setattr(
            f,
            fattr::ATIME | fattr::MTIME,
            0,
            0,
            0,
            (1_600_000_000, 123),
            (1_500_000_000, 456_789),
        )
        .unwrap();
    assert_eq!(
        (a.atime, a.mtime),
        ((1_600_000_000, 123), (1_500_000_000, 456_789))
    );
    let a = c
        .setattr(f, fattr::MTIME, 0, 0, 0, (0, 0), (1_400_000_000, 1))
        .unwrap();
    assert_eq!(
        (a.atime, a.mtime),
        ((1_600_000_000, 123), (1_400_000_000, 1))
    );
    let a = c
        .setattr(f, fattr::MTIME | fattr::MTIME_NOW, 0, 0, 0, (0, 0), (0, 0))
        .unwrap();
    assert!(a.mtime.0 > 1_700_000_000);
    let old = -86_400i64 * 365;
    let a = c
        .setattr(f, fattr::MTIME, 0, 0, 0, (0, 0), (old as u64, 5))
        .unwrap();
    assert_eq!(std::fs::metadata(s.path("f")).unwrap().mtime(), old);
    assert_eq!(a.mtime, (old as u64, 5));
    assert_eq!(c.getattr(f).unwrap().mtime, (old as u64, 5));
    c.setattr(f, fattr::MTIME, 0, 0, 0, (0, 0), (1_400_000_000, 1))
        .unwrap();
    let l = c.lookup(ROOT_ID, b"l").unwrap().nodeid;
    let before = c.getattr(f).unwrap().mtime;
    let a = c
        .setattr(l, fattr::MTIME, 0, 0, 0, (0, 0), (1_300_000_000, 0))
        .unwrap();
    assert_eq!(a.mtime, (1_300_000_000, 0));
    assert_eq!(c.getattr(f).unwrap().mtime, before);

    c.chmod(f, 0o6755).unwrap();
    let a = c
        .setattr(f, fattr::UID | fattr::GID, 0, 0, 0, (0, 0), (0, 0))
        .unwrap();
    assert_eq!((a.uid, owner_bits(&a)), (me, ATTR_CALLER_OWNER));
    assert_eq!(a.mode, 0o100755);
    assert_eq!(std::fs::metadata(s.path("f")).unwrap().uid(), me);

    c.chmod(f, 0o6755).unwrap();
    let fh = c.open(f, O_WRONLY).unwrap();
    c.write_flags(f, fh, 0, b"y", crate::abi::WRITE_KILL_SUIDGID)
        .unwrap();
    assert_eq!(c.getattr(f).unwrap().mode, 0o100755);
    c.release(f, fh).unwrap();
    c.chmod(f, 0o4755).unwrap();
    c.setattr(
        f,
        fattr::SIZE | fattr::KILL_SUIDGID,
        0,
        1,
        0,
        (0, 0),
        (0, 0),
    )
    .unwrap();
    assert_eq!(c.getattr(f).unwrap().mode, 0o100755);
    assert_eq!(c.truncate(d, None, 0).err(), Some(Errno::EISDIR));
}

#[test]
fn special_files_are_real_fifos_and_placeholders_for_the_rest() {
    let s = Scratch::new("special");
    let c = s.client();
    let fifo = c.mknod(ROOT_ID, b"fifo", S_IFIFO | 0o640, 0).unwrap();
    assert_eq!(fifo.attr.mode, S_IFIFO | 0o640);
    assert_eq!(
        std::fs::symlink_metadata(s.path("fifo")).unwrap().mode() & S_IFMT,
        S_IFIFO,
        "a real FIFO on the Mac"
    );
    let reg = c.mknod(ROOT_ID, b"reg", S_IFREG | 0o604, 0).unwrap();
    assert_eq!((reg.attr.mode, reg.attr.size), (0o100604, 0));

    for (name, kind, rdev) in [
        (&b"null"[..], S_IFCHR, 0x103u32),
        (b"disk", S_IFBLK, 0x800),
        (b"sock", S_IFSOCK, 0),
    ] {
        let e = c.mknod(ROOT_ID, name, kind | 0o660, rdev).unwrap();
        assert_eq!((e.attr.mode, e.attr.rdev), (kind | 0o660, rdev));
        let again = c.lookup(ROOT_ID, name).unwrap();
        assert_eq!(again.nodeid, e.nodeid);
        assert_eq!((again.attr.mode, again.attr.rdev), (kind | 0o660, rdev));
        let by_node = c.getattr(e.nodeid).unwrap();
        assert_eq!((by_node.mode, by_node.rdev), (kind | 0o660, rdev));
        assert_eq!(c.open(e.nodeid, 0).err(), Some(Errno::EOPNOTSUPP));
        for mode in [0o1604, 0o111, 0o000, 0o4755] {
            assert_eq!(c.chmod(e.nodeid, mode).unwrap().mode, kind | mode);
            assert_eq!(c.lookup(ROOT_ID, name).unwrap().attr.mode, kind | mode);
            assert_eq!(c.getattr(e.nodeid).unwrap().rdev, rdev);
        }
        let path = s.path(std::str::from_utf8(name).unwrap());
        let meta = std::fs::symlink_metadata(&path).unwrap();
        assert_eq!((meta.mode() & S_IFMT, meta.len()), (S_IFREG, 0));
        assert_eq!(c.listxattr(e.nodeid).unwrap(), Vec::<Vec<u8>>::new());
        assert_eq!(
            c.getxattr(e.nodeid, NODE_KEY, 64).err(),
            Some(Errno::ENODATA)
        );
    }
    let listed = c.list(ROOT_ID, true).unwrap();
    let null = listed.iter().find(|e| e.name == b"null").unwrap();
    assert_eq!(null.entry.unwrap().attr.mode & S_IFMT, S_IFCHR);
    assert_eq!(null.kind, S_IFCHR >> 12);
    s.write("sticky", b"");
    std::fs::set_permissions(s.path("sticky"), std::fs::Permissions::from_mode(0o1644)).unwrap();
    assert_eq!(c.lookup(ROOT_ID, b"sticky").unwrap().attr.mode, 0o101644);
    assert_eq!(
        c.mknod(ROOT_ID, b"odd", 0o170000, 0).err(),
        Some(Errno::EINVAL)
    );
    assert_eq!(
        c.mknod(ROOT_ID, b"null", S_IFCHR, 1).err(),
        Some(Errno::EEXIST)
    );
}

#[test]
fn extended_attributes_round_trip_under_their_linux_names() {
    let s = Scratch::new("xattr");
    s.write("f", b"data");
    std::fs::create_dir(s.path("d")).unwrap();
    symlink("f", s.path("l")).unwrap();
    let c = s.client();
    let f = c.lookup(ROOT_ID, b"f").unwrap().nodeid;
    let size = |v: Vec<u8>| u32::from_le_bytes(v[..4].try_into().unwrap());
    let attrs: [(&[u8], &[u8]); 5] = [
        (b"user.comment", b"hello"),
        (b"security.selinux", b"system_u:object_r:etc_t:s0\0"),
        (b"trusted.overlay.opaque", b"y"),
        (b"system.posix_acl_access", &[2, 0, 0, 0, 1, 0, 6, 0]),
        (b"user.empty", b""),
    ];
    for (name, value) in attrs {
        c.setxattr(f, name, value, 0).unwrap();
        assert_eq!(size(c.getxattr(f, name, 0).unwrap()), value.len() as u32);
        assert_eq!(c.getxattr(f, name, 4096).unwrap(), value);
    }
    let mut listed = c.listxattr(f).unwrap();
    listed.sort();
    let mut want: Vec<Vec<u8>> = attrs.iter().map(|(n, _)| n.to_vec()).collect();
    want.sort();
    assert_eq!(listed, want);
    let mac = std::process::Command::new("/usr/bin/xattr")
        .arg("-p")
        .arg("user.comment")
        .arg(s.path("f"))
        .output()
        .unwrap();
    assert_eq!(String::from_utf8_lossy(&mac.stdout).trim(), "hello");
    assert_eq!(c.getxattr(f, b"user.comment", 2).err(), Some(Errno::ERANGE));
    assert_eq!(c.getxattr(f, b"user.none", 64).err(), Some(Errno::ENODATA));
    assert_eq!(
        c.setxattr(f, b"user.comment", b"x", 1).err(),
        Some(Errno::EEXIST)
    );
    assert_eq!(
        c.setxattr(f, b"user.none", b"x", 2).err(),
        Some(Errno::ENODATA)
    );
    c.setxattr(f, b"user.comment", b"replaced", 2).unwrap();
    assert_eq!(c.getxattr(f, b"user.comment", 64).unwrap(), b"replaced");
    c.removexattr(f, b"user.comment").unwrap();
    assert_eq!(
        c.removexattr(f, b"user.comment").err(),
        Some(Errno::ENODATA)
    );
    let large: Vec<u8> = (0..60_000u32).map(|i| (i % 251) as u8).collect();
    c.setxattr(f, b"user.large", &large, 0).unwrap();
    assert_eq!(c.getxattr(f, b"user.large", 65536).unwrap(), large);

    let set = |name: &str, value: &str| {
        assert!(std::process::Command::new("/usr/bin/xattr")
            .args(["-w", name, value])
            .arg(s.path("f"))
            .status()
            .unwrap()
            .success());
    };
    set("com.apple.metadata:kMDItemComment", "finder");
    assert!(!c
        .listxattr(f)
        .unwrap()
        .iter()
        .any(|n| n.starts_with(b"com.apple.")));
    let hidden = b"com.apple.metadata:kMDItemComment";
    assert_eq!(c.getxattr(f, hidden, 64).err(), Some(Errno::ENODATA));
    assert_eq!(
        c.setxattr(f, hidden, b"x", 0).err(),
        Some(Errno::EOPNOTSUPP)
    );
    assert_eq!(
        c.setxattr(f, NODE_KEY, b"x", 0).err(),
        Some(Errno::EOPNOTSUPP)
    );
    assert_eq!(c.removexattr(f, hidden).err(), Some(Errno::ENODATA));
    let long = [b"user.".to_vec(), vec![b'n'; 123]].concat();
    assert_eq!(c.setxattr(f, &long, b"x", 0).err(), Some(Errno::ERANGE));
    c.setxattr(f, &long[..127], b"x", 0).unwrap();

    let d = c.lookup(ROOT_ID, b"d").unwrap().nodeid;
    c.setxattr(d, b"user.dir", b"1", 0).unwrap();
    assert_eq!(c.listxattr(d).unwrap(), [b"user.dir".to_vec()]);
    let l = c.lookup(ROOT_ID, b"l").unwrap().nodeid;
    c.setxattr(l, b"user.onlink", b"1", 0).unwrap();
    assert_eq!(c.getxattr(f, b"user.onlink", 8).err(), Some(Errno::ENODATA));

    let cap = b"security.capability";
    let fh = c.open(f, O_WRONLY).unwrap();
    for change in 0..4 {
        c.setxattr(f, cap, &[1, 0, 0, 2, 0, 0, 0, 0], 0).unwrap();
        match change {
            0 => drop(c.write(f, fh, 0, b"x").unwrap()),
            1 => drop(c.write(f, fh, 1, b"y").unwrap()),
            2 => drop(c.truncate(f, None, 1).unwrap()),
            _ => drop(
                c.setattr(f, crate::abi::fattr::UID, 0, 0, 0, (0, 0), (0, 0))
                    .unwrap(),
            ),
        }
        assert_eq!(
            c.getxattr(f, cap, 64).err(),
            Some(Errno::ENODATA),
            "{change}"
        );
    }
    c.release(f, fh).unwrap();
}

#[test]
fn space_holes_and_sync() {
    let s = Scratch::new("space");
    let c = s.client_with(|cfg| cfg.sync = Sync::Full);
    let (e, fh) = c.create(ROOT_ID, b"f", 0o644, O_RDWR).unwrap();
    let (n, block) = (e.nodeid, 4096u64);
    c.fallocate(n, fh, 0, 0, 10 * block).unwrap();
    assert_eq!(c.getattr(n).unwrap().size, 10 * block);
    c.fallocate(n, fh, 1, 0, 20 * block).unwrap();
    assert_eq!(c.getattr(n).unwrap().size, 10 * block);
    c.write(n, fh, 0, &vec![0xaa; 10 * block as usize]).unwrap();
    c.fallocate(n, fh, 3, block + 100, 3 * block).unwrap();
    let back = std::fs::read(s.path("f")).unwrap();
    assert_eq!(back.len(), 10 * block as usize);
    let (from, to) = (block as usize + 100, 4 * block as usize + 100);
    assert!(back[..from].iter().all(|b| *b == 0xaa));
    assert!(back[from..to].iter().all(|b| *b == 0));
    assert!(back[to..].iter().all(|b| *b == 0xaa));
    assert_eq!(c.lseek(n, fh, 0, 3).unwrap(), 0, "data at the start");
    let hole = c.lseek(n, fh, 0, 4).unwrap();
    assert!(hole == 2 * block || hole == 10 * block, "hole at {hole}");
    assert_eq!(c.lseek(n, fh, 0, 1).err(), Some(Errno::EINVAL));
    assert_eq!(c.lseek(n, fh, 100 * block, 3).err(), Some(Errno(6)));
    assert_eq!(c.lseek(n, fh, u64::MAX - 5, 4).err(), Some(Errno(6)));
    c.chmod(n, 0o6755).unwrap();
    c.fallocate(n, fh, 1, 0, block).unwrap();
    assert_eq!(c.getattr(n).unwrap().mode, 0o100755);
    c.fallocate(n, fh, 3, 100 * block, block).unwrap();
    for mode in [0x10, 0x08, 0x20, 2] {
        assert_eq!(
            c.fallocate(n, fh, mode, 0, block).err(),
            Some(Errno::EOPNOTSUPP)
        );
    }
    assert_eq!(c.fallocate(n, fh, 0, 0, 0).err(), Some(Errno::EINVAL));
    assert_eq!(
        c.fallocate(n, fh, 0, u64::MAX, 1).err(),
        Some(Errno::EINVAL)
    );
    let dfh = c.opendir(ROOT_ID).unwrap();
    for sync in [Sync::Fsync, Sync::Barrier, Sync::Full] {
        let c2 = s.client_with(|cfg| cfg.sync = sync);
        let f = c2.lookup(ROOT_ID, b"f").unwrap().nodeid;
        let h = c2.open(f, O_WRONLY).unwrap();
        c2.write(f, h, 0, b"z").unwrap();
        c2.fsync(f, h, false).unwrap();
        let d = c2.opendir(ROOT_ID).unwrap();
        c2.fsync(ROOT_ID, d, true).unwrap();
        c2.call(crate::abi::op::SYNCFS, ROOT_ID, &[0; 8]).unwrap();
    }
    c.fsync(ROOT_ID, dfh, true).unwrap();
    assert_eq!(c.fsync(n, 9999, false).err(), Some(Errno::EBADF));
}

#[test]
fn a_read_only_share_refuses_every_change() {
    let s = Scratch::new("readonly");
    s.write("f", b"data");
    std::fs::create_dir(s.path("d")).unwrap();
    let c = s.client_with(|cfg| cfg.read_only = true);
    let f = c.lookup(ROOT_ID, b"f").unwrap().nodeid;
    let ro = Some(Errno::EROFS);
    assert_eq!(c.create(ROOT_ID, b"n", 0o644, O_WRONLY).err(), ro);
    assert_eq!(c.create(ROOT_ID, b"f", 0o644, 0).err(), ro);
    assert_eq!(c.mkdir(ROOT_ID, b"n", 0o755).err(), ro);
    assert_eq!(c.mknod(ROOT_ID, b"n", S_IFIFO, 0).err(), ro);
    assert_eq!(c.symlink(ROOT_ID, b"n", b"f").err(), ro);
    assert_eq!(c.link(f, ROOT_ID, b"n").err(), ro);
    assert_eq!(c.unlink(ROOT_ID, b"f").err(), ro);
    assert_eq!(c.rmdir(ROOT_ID, b"d").err(), ro);
    assert_eq!(c.rename(ROOT_ID, b"f", ROOT_ID, b"g", 0).err(), ro);
    assert_eq!(c.chmod(f, 0o600).err(), ro);
    assert_eq!(c.truncate(f, None, 0).err(), ro);
    assert_eq!(c.setxattr(f, b"user.a", b"b", 0).err(), ro);
    assert_eq!(c.removexattr(f, b"user.a").err(), ro);
    for flags in [O_WRONLY, O_RDWR, O_TRUNC] {
        assert_eq!(c.open(f, flags).err(), ro);
    }
    assert_eq!(read_all(&c, f), b"data");
    let fh = c.open(f, 0).unwrap();
    assert_eq!(c.fallocate(f, fh, 0, 0, 4096).err(), ro);
    assert_eq!(c.write(f, fh, 0, b"x").err(), Some(Errno::EBADF));
    assert_eq!(std::fs::read(s.path("f")).unwrap(), b"data");
    assert_eq!(std::fs::read_dir(&s.0).unwrap().count(), 2);
}

#[test]
fn the_deny_list_holds_for_every_change() {
    let s = Scratch::new("denywrite");
    s.write("data/disk.img", b"the VM's disk");
    s.write("run/sock", b"");
    s.write("mine", b"mine");
    std::fs::create_dir(s.path("d")).unwrap();
    let c = s.client_with(|cfg| cfg.deny = vec![s.path("data"), s.path("run")]);
    let mine = c.lookup(ROOT_ID, b"mine").unwrap().nodeid;
    let d = c.lookup(ROOT_ID, b"d").unwrap().nodeid;
    assert_eq!(c.rmdir(ROOT_ID, b"data").err(), Some(Errno::ENOENT));
    assert_eq!(c.unlink(ROOT_ID, b"data").err(), Some(Errno::ENOENT));
    assert_eq!(
        c.rename(ROOT_ID, b"data", d, b"taken", 0).err(),
        Some(Errno::ENOENT)
    );
    assert_eq!(
        c.rename(ROOT_ID, b"d", ROOT_ID, b"run", 0).err(),
        Some(Errno::EACCES)
    );
    assert_eq!(
        c.rename(ROOT_ID, b"d", ROOT_ID, b"run", 1).err(),
        Some(Errno::EEXIST)
    );
    assert_eq!(
        c.rename(ROOT_ID, b"d", ROOT_ID, b"run", 2).err(),
        Some(Errno::EACCES)
    );
    assert_eq!(
        c.rename(ROOT_ID, b"data", ROOT_ID, b"run", 2).err(),
        Some(Errno::ENOENT)
    );
    assert_eq!(c.mkdir(ROOT_ID, b"data", 0o755).err(), Some(Errno::EEXIST));
    assert!(c.create(ROOT_ID, b"data", 0o644, O_WRONLY).is_err());
    assert_eq!(c.link(mine, ROOT_ID, b"run").err(), Some(Errno::EEXIST));
    std::fs::hard_link(s.path("data/disk.img"), s.path("d/alias")).unwrap();
    let c = s.client_with(|cfg| cfg.deny = vec![s.path("data/disk.img"), s.path("run")]);
    let d = c.lookup(ROOT_ID, b"d").unwrap().nodeid;
    assert_eq!(
        c.create(d, b"alias", 0o644, O_WRONLY | O_TRUNC).err(),
        Some(Errno::EACCES)
    );
    assert_eq!(c.unlink(d, b"alias").err(), Some(Errno::ENOENT));
    assert_eq!(
        c.rename(d, b"alias", ROOT_ID, b"out", 0).err(),
        Some(Errno::ENOENT)
    );
    assert_eq!(
        std::fs::read(s.path("data/disk.img")).unwrap(),
        b"the VM's disk"
    );
    assert!(s.path("d/alias").exists() && s.path("run/sock").exists());

    let root = Client::new(Server::new(Box::new(
        Passthrough::new(&Config::new("/")).unwrap(),
    )));
    root.init();
    let dev = root.lookup(ROOT_ID, b"dev").unwrap().nodeid;
    let refused = Some(Errno::EPERM);
    assert_eq!(
        root.create(dev, b"fev-test", 0o644, O_WRONLY).err(),
        refused
    );
    assert_eq!(root.mkdir(dev, b"fev-test", 0o755).err(), refused);
    assert_eq!(root.unlink(dev, b"null").err(), refused);
    assert_eq!(root.symlink(dev, b"fev-test", b"x").err(), refused);
    assert_eq!(root.rename(dev, b"null", dev, b"zero", 2).err(), refused);
    assert_eq!(root.rmdir(ROOT_ID, b".vol").err(), Some(Errno::ENOENT));
    assert!(root.mkdir(ROOT_ID, b".vol", 0o755).is_err());
    assert!(root
        .rename(ROOT_ID, b".vol", ROOT_ID, b"fev-vol-test", 0)
        .is_err());
}

#[test]
fn scheme_two_follows_the_guest_s_own_renames_and_removals() {
    let s = Scratch::new("tree2");
    s.write("a/b/file", b"deep");
    s.write("a/other", b"other");
    let c = tree_client(&s);
    let a = c.lookup(ROOT_ID, b"a").unwrap().nodeid;
    let b = c.lookup(a, b"b").unwrap().nodeid;
    let file = c.lookup(b, b"file").unwrap().nodeid;
    let other = c.lookup(a, b"other").unwrap().nodeid;
    c.rename(ROOT_ID, b"a", ROOT_ID, b"z", 0).unwrap();
    assert_eq!(read_all(&c, file), b"deep");
    assert_eq!(c.lookup(ROOT_ID, b"z").unwrap().nodeid, a);
    assert_eq!(c.lookup(ROOT_ID, b"a").err(), Some(Errno::ENOENT));
    c.rename(b, b"file", a, b"file", 0).unwrap();
    assert_eq!(read_all(&c, file), b"deep");
    assert_eq!(c.lookup(a, b"file").unwrap().nodeid, file);
    c.rename(a, b"file", a, b"other", 0).unwrap();
    assert_eq!(c.lookup(a, b"other").unwrap().nodeid, file);
    assert_eq!(c.getattr(other).err(), Some(Errno::ESTALE));
    assert_eq!(std::fs::read(s.path("z/other")).unwrap(), b"deep");
    let (x, xh) = c.create(a, b"x", 0o644, O_WRONLY).unwrap();
    c.write(x.nodeid, xh, 0, b"was x").unwrap();
    c.release(x.nodeid, xh).unwrap();
    c.rename(a, b"x", a, b"other", 2).unwrap();
    assert_eq!(read_all(&c, x.nodeid), b"was x");
    assert_eq!(std::fs::read(s.path("z/other")).unwrap(), b"was x");
    assert_eq!(read_all(&c, file), b"deep");
    assert_eq!(c.lookup(a, b"x").unwrap().nodeid, file);
    assert_eq!(c.chmod(file, 0o600).unwrap().mode, 0o100600);
    assert_eq!(c.truncate(file, None, 2).unwrap().size, 2);
    let d = c.mkdir(b, b"made", 0o755).unwrap().nodeid;
    let l = c.symlink(d, b"l", b"target").unwrap().nodeid;
    assert_eq!(c.readlink(l).unwrap(), b"target");
    c.unlink(d, b"l").unwrap();
    assert_eq!(c.getattr(l).err(), Some(Errno::ESTALE));
    c.rmdir(b, b"made").unwrap();
    assert_eq!(c.getattr(d).err(), Some(Errno::ESTALE));
    let fh = c.open(file, O_RDWR).unwrap();
    c.unlink(a, b"x").unwrap();
    assert_eq!(c.read(file, fh, 0, 8).unwrap(), b"de");
    c.release(file, fh).unwrap();
    assert_eq!(c.link(x.nodeid, a, b"hard").err(), Some(Errno::EPERM));
    assert_eq!(
        c.setxattr(x.nodeid, b"user.a", b"b", 0).err(),
        Some(Errno::EOPNOTSUPP)
    );
    assert_eq!(c.listxattr(x.nodeid).unwrap(), Vec::<Vec<u8>>::new());
    for (id, n) in [
        (a, 2),
        (b, 1),
        (file, 3),
        (other, 1),
        (x.nodeid, 2),
        (d, 1),
        (l, 1),
    ] {
        c.forget(id, n + 4);
    }
    assert_eq!(c.getattr(a).err(), Some(Errno::ESTALE));
}

#[test]
fn scheme_two_changes_nothing_through_a_link_put_in_its_path() {
    let s = Scratch::new("swapwrite");
    s.write("a/b/x", b"plain");
    s.write("data/b/x", b"precious");
    s.write("data/b/keep", b"keep");
    let c = s.client_with(|cfg| {
        cfg.no_volfs = true;
        cfg.deny = vec![s.path("data")];
    });
    let a = c.lookup(ROOT_ID, b"a").unwrap().nodeid;
    let b = c.lookup(a, b"b").unwrap().nodeid;
    let x = c.lookup(b, b"x").unwrap().nodeid;
    std::fs::rename(s.path("a"), s.path("gone")).unwrap();
    symlink("data", s.path("a")).unwrap();

    let stale = Some(Errno::ESTALE);
    assert_eq!(c.create(b, b"new", 0o644, O_WRONLY).err(), stale);
    assert_eq!(c.mkdir(b, b"new", 0o755).err(), stale);
    assert_eq!(c.mknod(b, b"new", S_IFIFO | 0o644, 0).err(), stale);
    assert_eq!(c.symlink(b, b"new", b"t").err(), stale);
    assert_eq!(c.unlink(b, b"x").err(), stale);
    assert_eq!(c.unlink(b, b"keep").err(), stale);
    assert_eq!(c.rename(b, b"x", ROOT_ID, b"stolen", 0).err(), stale);
    assert_eq!(c.rename(ROOT_ID, b"gone", b, b"x", 0).err(), stale);
    assert_eq!(c.chmod(x, 0o777).err(), stale);
    assert_eq!(c.truncate(x, None, 0).err(), stale);
    assert_eq!(c.open(x, O_WRONLY | O_TRUNC).err(), stale);
    assert_eq!(std::fs::read(s.path("data/b/x")).unwrap(), b"precious");
    assert_eq!(std::fs::read(s.path("data/b/keep")).unwrap(), b"keep");
    assert_eq!(std::fs::read_dir(s.path("data/b")).unwrap().count(), 2);
    assert!(!s.path("stolen").exists());
    assert_eq!(
        std::fs::metadata(s.path("data/b/x")).unwrap().mode() & 0o777,
        0o644
    );
}

#[test]
fn another_spelling_reaches_the_same_file_for_a_change_too() {
    let s = Scratch::new("casewrite");
    if case_sensitive(&s.0) {
        return;
    }
    s.write("Makefile", b"all:");
    let c = s.client();
    let exact = c.lookup(ROOT_ID, b"Makefile").unwrap().nodeid;
    let (e, fh) = c.create(ROOT_ID, b"MAKEFILE", 0o644, O_WRONLY).unwrap();
    assert_eq!((e.nodeid, e.entry_valid), (exact, Duration::ZERO));
    c.release(exact, fh).unwrap();
    assert_eq!(
        c.create(ROOT_ID, b"makefile", 0o644, O_WRONLY | O_EXCL)
            .err(),
        Some(Errno::EEXIST)
    );
    assert_eq!(
        c.mkdir(ROOT_ID, b"makeFILE", 0o755).err(),
        Some(Errno::EEXIST)
    );
    c.unlink(ROOT_ID, b"makefile").unwrap();
    assert!(!s.path("Makefile").exists());
    s.write("Readme", b"hi");
    let t = tree_client(&s);
    let readme = t.lookup(ROOT_ID, b"Readme").unwrap().nodeid;
    t.rename(ROOT_ID, b"README", ROOT_ID, b"Notes", 0).unwrap();
    assert_eq!(read_all(&t, readme), b"hi", "the node went with the rename");
    assert_eq!(t.lookup(ROOT_ID, b"notes").unwrap().nodeid, readme);
}

fn churn(dir: &Path, no_volfs: bool, rounds: usize, seed: u64) -> String {
    use std::collections::HashMap;
    let _ = std::fs::remove_dir_all(dir.join("churn"));
    std::fs::create_dir_all(dir.join("churn/d0")).unwrap();
    std::fs::create_dir_all(dir.join("churn/d1")).unwrap();
    let root = std::fs::canonicalize(dir.join("churn")).unwrap();
    let mut cfg = Config::new(&root);
    cfg.no_volfs = no_volfs;
    let c = Client::new(Server::new(Box::new(Passthrough::new(&cfg).unwrap())));
    c.init();
    let dirs = [
        c.lookup(ROOT_ID, b"d0").unwrap().nodeid,
        c.lookup(ROOT_ID, b"d1").unwrap().nodeid,
    ];
    let by_identity = {
        let probe = root.join("d0/.probe");
        std::fs::write(&probe, b"").unwrap();
        let st = std::fs::metadata(&probe).unwrap();
        let found =
            !no_volfs && std::fs::metadata(format!("/.vol/{}/{}", st.dev(), st.ino())).is_ok();
        std::fs::remove_file(&probe).unwrap();
        found
    };
    let mut state = seed | 1;
    let mut next = move |n: u64| {
        state ^= state << 13;
        state ^= state >> 7;
        state ^= state << 17;
        state % n
    };
    let path = |d: u64, n: u64| root.join(format!("d{d}/n{n}"));
    let name = |n: u64| format!("n{n}").into_bytes();
    let mut objects = 0u64;
    let mut held: Vec<(u64, Vec<u8>)> = Vec::new();
    let (mut guest_ops, mut mac_ops, mut stale, mut checks, mut links) = (0, 0, 0, 0, 0);
    for round in 0..rounds {
        let (d, n, d2, n2) = (next(2), next(8), next(2), next(8));
        let mut fresh = || {
            objects += 1;
            format!("object {objects}\n").into_bytes()
        };
        match next(12) {
            0 | 1 => {
                if let Ok((e, fh)) = c.create(dirs[d as usize], &name(n), 0o644, O_WRONLY | O_EXCL)
                {
                    c.write(e.nodeid, fh, 0, &fresh()).unwrap();
                    c.release(e.nodeid, fh).unwrap();
                    c.forget(e.nodeid, 1);
                }
                guest_ops += 1;
            }
            2 => {
                if let Ok(mut f) = std::fs::OpenOptions::new()
                    .write(true)
                    .create_new(true)
                    .open(path(d, n))
                {
                    std::io::Write::write_all(&mut f, &fresh()).unwrap();
                }
                mac_ops += 1;
            }
            3 | 4 => {
                let flags = [0, 0, 1, 2][next(4) as usize];
                let _ = c.rename(
                    dirs[d as usize],
                    &name(n),
                    dirs[d2 as usize],
                    &name(n2),
                    flags,
                );
                guest_ops += 1;
            }
            5 => {
                let _ = std::fs::rename(path(d, n), path(d2, n2));
                mac_ops += 1;
            }
            6 => {
                let _ = c.unlink(dirs[d as usize], &name(n));
                guest_ops += 1;
            }
            7 => {
                let _ = std::fs::remove_file(path(d, n));
                mac_ops += 1;
            }
            8 => {
                if next(2) == 0 {
                    if let Ok(e) = c.lookup(dirs[d as usize], &name(n)) {
                        if c.link(e.nodeid, dirs[d2 as usize], &name(n2)).is_ok() {
                            c.forget(e.nodeid, 1);
                            links += 1;
                        }
                        c.forget(e.nodeid, 1);
                    }
                    guest_ops += 1;
                } else {
                    if std::fs::hard_link(path(d, n), path(d2, n2)).is_ok() {
                        links += 1;
                    }
                    mac_ops += 1;
                }
            }
            9 => {
                let tmp = root.join(format!("d{d}/.tmp"));
                std::fs::write(&tmp, fresh()).unwrap();
                let _ = std::fs::rename(&tmp, path(d, n));
                mac_ops += 1;
            }
            10 => {
                let dn = dirs[d as usize];
                if let Ok((e, fh)) = c.create(dn, b".gtmp", 0o644, O_WRONLY) {
                    c.truncate(e.nodeid, Some(fh), 0).unwrap();
                    c.write(e.nodeid, fh, 0, &fresh()).unwrap();
                    c.release(e.nodeid, fh).unwrap();
                    c.rename(dn, b".gtmp", dn, &name(n), 0).unwrap();
                    c.forget(e.nodeid, 1);
                }
                guest_ops += 1;
            }
            _ => {
                for _ in 0..next(6) {
                    if held.is_empty() {
                        break;
                    }
                    let (node, _) = held.swap_remove(next(held.len() as u64) as usize);
                    c.forget(node, 1);
                }
            }
        }

        let mut inos: HashMap<u64, Vec<u8>> = HashMap::new();
        for d in 0..2u64 {
            for n in 0..8u64 {
                let truth = std::fs::read(path(d, n)).ok();
                let got = c.lookup(dirs[d as usize], &name(n));
                checks += 1;
                match (&truth, got) {
                    (None, Err(_)) => {}
                    (Some(want), Ok(e)) => {
                        let read = read_all(&c, e.nodeid);
                        assert_eq!(
                            String::from_utf8_lossy(&read),
                            String::from_utf8_lossy(want),
                            "round {round} seed {seed}: d{d}/n{n} read through the guest is another file"
                        );
                        if let Some(other) = inos.insert(e.attr.ino, read.clone()) {
                            assert_eq!(
                                other, read,
                                "round {round} seed {seed}: inode number {} shown for two files",
                                e.attr.ino
                            );
                        }
                        if held.len() < 64 && next(3) == 0 {
                            held.push((e.nodeid, read));
                        } else {
                            c.forget(e.nodeid, 1);
                        }
                    }
                    (truth, got) => panic!(
                        "round {round} seed {seed}: d{d}/n{n} is {} on the Mac and {:?} to the guest",
                        if truth.is_some() { "there" } else { "absent" },
                        got.map(|e| e.nodeid)
                    ),
                }
            }
        }
        for (node, object) in &held {
            checks += 1;
            let Ok(fh) = c.open(*node, 0) else {
                stale += 1;
                continue;
            };
            let read = c.read(*node, fh, 0, 4096).unwrap();
            c.release(*node, fh).unwrap();
            if by_identity {
                assert_eq!(
                    String::from_utf8_lossy(&read),
                    String::from_utf8_lossy(object),
                    "round {round} seed {seed}: node {node} now reads another object"
                );
            }
        }
    }
    for (node, _) in held.drain(..) {
        c.forget(node, 1);
    }
    assert_eq!(fds_settled(&root, 0), 0, "descriptors left on the share");
    let _ = std::fs::remove_dir_all(&root);
    format!(
        "scheme {}: {rounds} rounds, {guest_ops} guest and {mac_ops} Mac changes, {links} hard links, \
         {objects} objects, {checks} reads checked, {stale} stale nodes refused",
        if by_identity { 1 } else { 2 }
    )
}

#[test]
fn identity_holds_under_churn_from_both_sides() {
    let rounds = std::env::var("FEV_CHURN_ROUNDS")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(300);
    match std::env::var_os("FEV_CHURN_DIR") {
        Some(dir) => {
            for seed in [0x9e37_79b9, 0x2545_f491, 7] {
                println!("churn {}", churn(Path::new(&dir), false, rounds, seed));
            }
        }
        None => {
            let s = Scratch::new("churn");
            for seed in [0x9e37_79b9, 0x2545_f491] {
                println!("churn {}", churn(&s.0, false, rounds, seed));
                println!("churn {}", churn(&s.0, true, rounds, seed));
            }
        }
    }
}

#[test]
fn scheme_two_requests_by_node_do_not_cross_a_rename_on_another_queue() {
    let s = Scratch::new("treerace");
    s.write("x", b"the real one");
    let c = std::sync::Arc::new(tree_client(&s));
    let x = c.lookup(ROOT_ID, b"x").unwrap().nodeid;
    let stop = std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
    let cutter = {
        let (c, stop) = (c.clone(), stop.clone());
        std::thread::spawn(move || {
            let mut cuts = 0u64;
            while !stop.load(Ordering::Relaxed) {
                if c.truncate(x, None, 1000 + (cuts % 7)).is_ok() {
                    cuts += 1;
                }
            }
            cuts
        })
    };
    let mut wrong = 0;
    let start = std::time::Instant::now();
    let mut rounds = 0;
    while start.elapsed() < Duration::from_millis(1500) {
        c.rename(ROOT_ID, b"x", ROOT_ID, b"moved", 0).unwrap();
        let (decoy, fh) = c.create(ROOT_ID, b"x", 0o644, O_WRONLY | O_EXCL).unwrap();
        c.write(decoy.nodeid, fh, 0, b"decoy").unwrap();
        c.release(decoy.nodeid, fh).unwrap();
        let wait = std::time::Instant::now();
        while wait.elapsed() < Duration::from_micros(300) {
            std::hint::spin_loop();
        }
        if std::fs::metadata(s.path("x")).unwrap().len() != 5 {
            wrong += 1;
        }
        c.unlink(ROOT_ID, b"x").unwrap();
        c.forget(decoy.nodeid, 1);
        c.rename(ROOT_ID, b"moved", ROOT_ID, b"x", 0).unwrap();
        rounds += 1;
    }
    stop.store(true, Ordering::Relaxed);
    let cuts = cutter.join().unwrap();
    assert!(rounds > 50 && cuts > 50, "{rounds} rounds, {cuts} cuts");
    assert!(rounds > 300, "{rounds} rounds, {cuts} cuts");
    assert_eq!(wrong, 0, "of {rounds} rounds, with {cuts} cuts by node");
}

#[test]
fn nothing_goes_through_a_link_swapped_in_above_a_node() {
    let s = Scratch::new("swaplink");
    s.write("data/c/keep", b"the VM's");
    s.write("data/disk.img", b"the VM's disk");
    std::fs::create_dir_all(s.path("a/b/c")).unwrap();
    s.write("a/b/disk.img", b"guest");
    s.write("a/b/c/keep", b"guest");
    symlink("the VM's link", s.path("data/c/lnk")).unwrap();
    symlink("guest", s.path("a/b/c/lnk")).unwrap();
    let secret =
        std::ffi::CString::new(s.path("data/c/keep").as_os_str().as_encoded_bytes()).unwrap();
    let set = unsafe {
        libc::setxattr(
            secret.as_ptr(),
            c"user.note".as_ptr(),
            b"secret".as_ptr().cast(),
            6,
            0,
            0,
        )
    };
    assert_eq!(set, 0);
    let c = std::sync::Arc::new(s.client_with(|cfg| cfg.deny = vec![s.path("data")]));
    let a = c.lookup(ROOT_ID, b"a").unwrap().nodeid;
    let b = c.lookup(a, b"b").unwrap().nodeid;
    let cdir = c.lookup(b, b"c").unwrap().nodeid;
    let keep = c.lookup(cdir, b"keep").unwrap().nodeid;
    let lnk = c.lookup(cdir, b"lnk").unwrap().nodeid;
    let stop = std::sync::Arc::new(AtomicBool::new(false));
    let target = s.path("data");
    c.symlink(a, b"bl", target.as_os_str().as_encoded_bytes())
        .unwrap();
    let swapper = {
        let (c, stop) = (c.clone(), stop.clone());
        std::thread::spawn(move || {
            while !stop.load(Ordering::Relaxed) {
                let _ = c.rename(a, b"b", a, b"bl", 2);
            }
        })
    };
    let rounds: usize = std::env::var("FEV_SWAP_ROUNDS")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(2_000);
    let mut breach = Vec::new();
    for i in 0..rounds {
        let _ = c.mkdir(cdir, b"x", 0o755);
        if target.join("c/x").exists() {
            breach.push(format!("round {i}: mkdir landed in the denied directory"));
            let _ = std::fs::remove_dir(target.join("c/x"));
        }
        let _ = c.rmdir(cdir, b"x");
        if let Ok((e, fh)) = c.create(b, b"disk.img", 0o644, O_WRONLY | O_TRUNC) {
            let _ = c.release(e.nodeid, fh);
        }
        if c.readlink(lnk).is_ok_and(|t| t != b"guest") {
            breach.push(format!(
                "round {i}: a link in the denied directory was read"
            ));
        }
        if c.getxattr(keep, b"user.note", 64)
            .is_ok_and(|v| v == b"secret")
        {
            breach.push(format!(
                "round {i}: an attribute in the denied directory was read"
            ));
        }
        let _ = c.setxattr(keep, b"user.mark", b"guest", 0);
        let _ = c.symlink(cdir, b"s", b"t");
        let _ = c.unlink(cdir, b"s");
        let _ = c.mknod(cdir, b"n", S_IFIFO | 0o600, 0);
        let _ = c.unlink(cdir, b"n");
        if let Ok(e) = c.link(keep, a, b"out") {
            if read_all(&c, e.nodeid) == b"the VM's" {
                breach.push(format!(
                    "round {i}: a denied file was linked into the share"
                ));
            }
        }
        let _ = c.unlink(a, b"out");
        for made in ["c/s", "c/n"] {
            if std::fs::symlink_metadata(target.join(made)).is_ok() {
                breach.push(format!(
                    "round {i}: {made} was made in the denied directory"
                ));
                let _ = std::fs::remove_file(target.join(made));
            }
        }
        let _ = c.truncate(keep, None, 0);
        let _ = c.chmod(keep, 0o600);
        let _ = c.setattr(
            keep,
            crate::abi::fattr::MTIME,
            0,
            0,
            0,
            (0, 0),
            (1_000_000_000, 0),
        );
        let _ = c.unlink(cdir, b"keep");
        if let Ok((e, fh)) = c.create(cdir, b"keep", 0o644, O_WRONLY) {
            let _ = c.release(e.nodeid, fh);
        }
        let changed = |rel: &str, want: &[u8]| loop {
            match std::fs::read(target.join(rel)) {
                Ok(got) => break got != want,
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => break true,
                Err(_) => std::thread::yield_now(),
            }
        };
        if changed("disk.img", b"the VM's disk") {
            breach.push(format!("round {i}: the denied file was cut or removed"));
            let _ = std::fs::write(target.join("disk.img"), b"the VM's disk");
        }
        if changed("c/keep", b"the VM's") {
            breach.push(format!(
                "round {i}: a file in the denied directory was changed"
            ));
            let _ = std::fs::write(target.join("c/keep"), b"the VM's");
        }
        let meta = std::fs::metadata(target.join("c/keep"));
        if meta.is_ok_and(|m| m.mtime() == 1_000_000_000 || m.mode() & 0o777 == 0o600) {
            breach.push(format!("round {i}: a denied file's times or mode were set"));
            let _ = std::fs::remove_file(target.join("c/keep"));
            let _ = std::fs::write(target.join("c/keep"), b"the VM's");
        }
        let mut names = [0u8; 256];
        let n =
            unsafe { libc::listxattr(secret.as_ptr(), names.as_mut_ptr().cast(), names.len(), 0) };
        if n > 0 && names[..n as usize].windows(9).any(|w| w == b"user.mark") {
            breach.push(format!(
                "round {i}: an attribute was set in the denied directory"
            ));
            unsafe { libc::removexattr(secret.as_ptr(), c"user.mark".as_ptr(), 0) };
        }
        if breach.len() > 5 {
            break;
        }
    }
    stop.store(true, Ordering::Relaxed);
    swapper.join().unwrap();
    assert!(breach.is_empty(), "{breach:#?}");
}

#[test]
fn a_refusal_is_reported_by_the_folder_the_mac_protects() {
    let home = b"/Users/me";
    let f =
        |p: &str, dir: bool| String::from_utf8(protected_folder(p.as_bytes(), home, dir)).unwrap();
    assert_eq!(
        f("/Users/me/Documents/proj/src/a.c", false),
        "/Users/me/Documents"
    );
    assert_eq!(f("/Users/me/Documents", true), "/Users/me/Documents");
    assert_eq!(f("/Users/me/Desktop/x", true), "/Users/me/Desktop");
    assert_eq!(f("/Users/me/Downloads/a/b", false), "/Users/me/Downloads");
    assert_eq!(
        f(
            "/Users/me/Library/Mobile Documents/com~apple~CloudDocs/x",
            false
        ),
        "/Users/me/Library/Mobile Documents"
    );
    assert_eq!(
        f("/Users/me/Library/CloudStorage/Dropbox/work/x", false),
        "/Users/me/Library/CloudStorage/Dropbox"
    );
    assert_eq!(f("/Volumes/USB/photos/1.jpg", false), "/Volumes/USB");
    assert_eq!(f("/Volumes/USB", true), "/Volumes/USB");
    assert_eq!(f("/Users/me/Documents2/x", false), "/Users/me/Documents2");
    assert_eq!(
        f("/Users/other/Documents/x", false),
        "/Users/other/Documents"
    );
    assert_eq!(f("/Users/me/src/secret", true), "/Users/me/src/secret");
    assert_eq!(f("/Users/me/src/secret/key", false), "/Users/me/src/secret");
    assert_eq!(f("/top", false), "/");
    assert_eq!(f("/Volumes", true), "/Volumes");
    assert_eq!(
        String::from_utf8(protected_folder(b"/Users/me/Documents/x", b"", false)).unwrap(),
        "/Users/me/Documents"
    );
}

#[test]
fn each_refused_folder_is_told_once_and_only_so_many() {
    let s = Scratch::new("denied");
    let fs = Passthrough::new(&Config::new(&s.0)).unwrap();
    fs.report_denied(b"/Volumes/A/x", false);
    let told = Arc::new(Mutex::new(Vec::<Vec<u8>>::new()));
    let sink = told.clone();
    fs.on_denied(Arc::new(move |folder: &[u8]| {
        sink.lock().unwrap().push(folder.to_vec())
    }));
    fs.report_denied(b"/Volumes/A/x", false);
    fs.report_denied(b"/Volumes/A/y/z", false);
    fs.report_denied(b"/Volumes/A", true);
    fs.report_denied(b"/Volumes/B/x", false);
    assert_eq!(
        *told.lock().unwrap(),
        vec![b"/Volumes/A".to_vec(), b"/Volumes/B".to_vec()]
    );
    for i in 0..(MAX_DENIED * 3) {
        fs.report_denied(format!("/Volumes/V{i}/x").as_bytes(), false);
    }
    assert_eq!(told.lock().unwrap().len(), MAX_DENIED);
    fs.report_denied(b"/.vol/1/999999999999", false);
    assert_eq!(told.lock().unwrap().len(), MAX_DENIED);
}

struct Watched {
    s: Scratch,
    fs: Arc<Passthrough>,
    c: Client,
}

impl Watched {
    fn new(name: &str) -> Self {
        Self::with(name, |_| {})
    }

    fn with(name: &str, tune: impl FnOnce(&mut Config)) -> Self {
        let s = Scratch::new(name);
        for d in ["w/sub", "out"] {
            std::fs::create_dir_all(s.path(d)).unwrap();
        }
        s.write("w/file", b"watched");
        s.write("w/sub/deep", b"deep");
        s.write("out/file", b"not watched");
        let mut cfg = Config::new(&s.0);
        tune(&mut cfg);
        let fs = Arc::new(Passthrough::new(&cfg).unwrap());
        let c = Client::new(Server::shared(fs.clone()));
        c.init();
        Self { s, fs, c }
    }

    fn watch(&self, dirs: &[&str]) {
        let roots: Vec<Vec<u8>> = dirs
            .iter()
            .map(|d| self.s.path(d).into_os_string().into_encoded_bytes())
            .collect();
        self.fs.set_watch(&roots);
    }

    fn valid(&self, path: &str) -> (Duration, Duration) {
        let e = walk(&self.c, path).unwrap();
        (e.entry_valid, e.attr_valid)
    }
}

const SHORT: Duration = Duration::from_secs(1);

#[test]
fn names_and_attributes_are_cached_for_long_only_under_an_acknowledged_watch() {
    let t = Watched::new("watch");
    for path in ["w", "w/file", "w/sub/deep", "out/file"] {
        assert_eq!(t.valid(path), (SHORT, SHORT), "{path}");
    }
    t.watch(&["w"]);
    assert_eq!(t.fs.watch_roots(), 1);
    assert_eq!(t.valid("w"), (SHORT, LONG));
    assert_eq!(t.valid("w/file"), (LONG, LONG));
    assert_eq!(t.valid("w/sub"), (LONG, LONG));
    assert_eq!(t.valid("w/sub/deep"), (LONG, LONG));
    assert_eq!(t.valid("out"), (SHORT, SHORT));
    assert_eq!(t.valid("out/file"), (SHORT, SHORT));
    let (file, out) = (
        walk(&t.c, "w/file").unwrap(),
        walk(&t.c, "out/file").unwrap(),
    );
    assert_eq!(t.c.getattr_valid(file.nodeid, None).unwrap().1, LONG);
    assert_eq!(t.c.getattr_valid(out.nodeid, None).unwrap().1, SHORT);
    let w = walk(&t.c, "w").unwrap().nodeid;
    let listed = t.c.list(w, true).unwrap();
    let entry = listed.iter().find(|e| e.name == b"file").unwrap();
    let e = entry.entry.as_ref().unwrap();
    assert_eq!((e.entry_valid, e.attr_valid), (LONG, LONG));
    let (made, fh) = t.c.create(w, b"made", 0o644, 0o1).unwrap();
    assert_eq!((made.entry_valid, made.attr_valid), (LONG, LONG));
    t.c.release(made.nodeid, fh).unwrap();
    let dir = t.c.mkdir(w, b"madedir", 0o755).unwrap();
    assert_eq!((dir.entry_valid, dir.attr_valid), (LONG, LONG));

    t.watch(&[]);
    assert_eq!(t.fs.watch_roots(), 0);
    for path in ["w", "w/file", "w/sub/deep", "w/made", "out/file"] {
        assert_eq!(t.valid(path), (SHORT, SHORT), "{path}");
    }
    assert_eq!(t.c.getattr_valid(file.nodeid, None).unwrap().1, SHORT);
    t.watch(&["w/sub", "out"]);
    assert_eq!(t.valid("w/file"), (SHORT, SHORT));
    assert_eq!(t.valid("w/sub/deep"), (LONG, LONG));
    assert_eq!(t.valid("out/file"), (LONG, LONG));
}

#[test]
fn a_missing_name_is_cached_only_under_a_watch_and_forgotten_when_the_guest_learns_better() {
    let t = Watched::new("absent");
    let (w, out) = (
        walk(&t.c, "w").unwrap().nodeid,
        walk(&t.c, "out").unwrap().nodeid,
    );
    assert_eq!(t.c.lookup(w, b"nothing"), Err(Errno::ENOENT));
    t.watch(&["w"]);
    let miss = t.c.lookup(w, b"nothing").unwrap();
    assert_eq!((miss.nodeid, miss.entry_valid), (0, LONG));
    assert_eq!(t.c.lookup(out, b"nothing"), Err(Errno::ENOENT));
    assert_eq!(t.fs.nodes.lock().unwrap().absent_len, 1);
    t.c.lookup(w, b"nothing").unwrap();
    assert_eq!(t.fs.nodes.lock().unwrap().absent_len, 1);
    let (made, fh) = t.c.create(w, b"nothing", 0o644, 0o1).unwrap();
    t.c.release(made.nodeid, fh).unwrap();
    assert_eq!(t.fs.nodes.lock().unwrap().absent_len, 0);
    assert_eq!(t.c.lookup(w, b"nothing").unwrap().nodeid, made.nodeid);
    assert_eq!(t.c.lookup(w, b".."), Err(Errno::EINVAL));
    let sub = t.c.lookup(w, b"sub").unwrap().nodeid;
    for name in [&b"a"[..], b"b", b"c"] {
        assert_eq!(t.c.lookup(sub, name).unwrap().nodeid, 0);
    }
    assert_eq!(t.fs.nodes.lock().unwrap().absent_len, 3);
    t.c.forget(sub, 1);
    assert_eq!(t.fs.nodes.lock().unwrap().absent_len, 0);
    t.watch(&[]);
    assert_eq!(t.c.lookup(w, b"gone"), Err(Errno::ENOENT));
    t.watch(&["w"]);
    {
        let mut nodes = t.fs.nodes.lock().unwrap();
        nodes.absent_len = MAX_ABSENT;
    }
    assert_eq!(t.c.lookup(w, b"one-too-many"), Err(Errno::ENOENT));
    t.fs.nodes.lock().unwrap().absent_len = 0;
}

#[test]
fn cached_data_and_listings_are_kept_across_opens_only_under_a_watch() {
    let t = Watched::new("keep");
    let file = walk(&t.c, "w/file").unwrap().nodeid;
    let out = walk(&t.c, "out/file").unwrap().nodeid;
    let flags = |node, open| {
        let (fh, flags) = t.c.open_flags(node, open, false).unwrap();
        t.c.release(node, fh).unwrap();
        flags
    };
    let no_keep = |path: &str| {
        let e = walk(&t.c, path).unwrap();
        let again = t.c.getattr(e.nodeid).unwrap();
        assert_eq!(
            e.attr.flags & ATTR_NO_KEEP_CACHE,
            again.flags & ATTR_NO_KEEP_CACHE
        );
        e.attr.flags & ATTR_NO_KEEP_CACHE != 0
    };
    assert_eq!(flags(file, 0), 0);
    assert!(no_keep("w") && no_keep("w/file") && no_keep("w/sub"));
    t.watch(&["w"]);
    assert_eq!(flags(file, 0), FOPEN_KEEP_CACHE);
    assert_eq!(flags(file, 2), FOPEN_KEEP_CACHE);
    assert_eq!(flags(out, 0), 0);
    assert!(!no_keep("w") && !no_keep("w/file") && !no_keep("w/sub"));
    assert!(no_keep("out") && no_keep("out/file"));
    assert_eq!(flags(file, 0o1001), 0);
    let free = crate::abi::FOPEN_FEV_FREE;
    let dir = walk(&t.c, "w").unwrap().nodeid;
    assert_eq!(
        t.c.open_flags(dir, 0, true).unwrap(),
        (0, FOPEN_KEEP_CACHE | FOPEN_CACHE_DIR | free)
    );
    t.watch(&[]);
    assert_eq!(flags(file, 0), 0);
    assert!(no_keep("w") && no_keep("w/file"));
    assert_eq!(t.c.open_flags(dir, 0, true).unwrap(), (0, free));
    std::fs::hard_link(t.s.path("w/file"), t.s.path("w/again")).unwrap();
    t.watch(&["w"]);
    assert!(no_keep("w/file"));
}

#[test]
fn a_directory_is_opened_once_without_a_handle_and_read_by_node() {
    let s = Scratch::new("bynode");
    for i in 0..60 {
        s.write(&format!("d/f{i:02}"), b"x");
    }
    std::fs::create_dir(s.path("d/sub")).unwrap();
    let mut cfg = Config::new(&s.0);
    cfg.max_listed = Some(150);
    let fs = Arc::new(Passthrough::new(&cfg).unwrap());
    let c = Client::new(Server::shared(fs.clone()));
    c.init();
    let d = c.lookup(ROOT_ID, b"d").unwrap().nodeid;
    assert_eq!(
        c.open_flags(d, 0, true).unwrap(),
        (0, crate::abi::FOPEN_FEV_FREE)
    );
    assert_eq!(fs.open_handles(), 0);
    std::fs::create_dir(s.path("gone")).unwrap();
    let gone = c.lookup(ROOT_ID, b"gone").unwrap().nodeid;
    std::fs::remove_dir(s.path("gone")).unwrap();
    assert_eq!(c.opendir(gone).err(), Some(Errno::ESTALE));
    assert_eq!(
        c.readdir(gone, 0, 0, 4096, false).err(),
        Some(Errno::ENOENT)
    );
    let kept = || fs.listings.lock().unwrap().map.len();
    let want: Vec<String> = {
        let mut n: Vec<String> = (0..60).map(|i| format!("f{i:02}")).collect();
        n.push("sub".into());
        n
    };
    for (plus, size) in [
        (false, 1 << 16),
        (true, 1 << 16),
        (false, 512),
        (true, 1024),
    ] {
        let mut all = Vec::new();
        let mut off = 0;
        loop {
            let got = c.readdir(d, 0, off, size, plus).unwrap();
            let Some(last) = got.last() else { break };
            off = last.off;
            all.extend(got);
        }
        assert_eq!(names(&all), want, "plus {plus}, {size} bytes");
        if plus {
            let e = all.iter().find(|e| e.name == b"f07").unwrap();
            let looked = c.lookup(d, b"f07").unwrap();
            assert_eq!(e.entry.as_ref().unwrap().attr, looked.attr);
        }
        assert_eq!(kept(), 0);
    }
    let first = c.readdir(d, 0, 0, 1024, false).unwrap();
    std::fs::remove_file(s.path("d/f59")).unwrap();
    let second = c.readdir(d, 0, 0, 1 << 16, false).unwrap();
    assert_eq!(kept(), 2);
    assert_eq!(names(&second).len(), 60);
    let mut rest = first.clone();
    let mut off = first.last().unwrap().off;
    loop {
        let got = c.readdir(d, 0, off, 1024, false).unwrap();
        let Some(last) = got.last() else { break };
        off = last.off;
        rest.extend(got);
    }
    assert_eq!(names(&rest).len(), 61, "the first reader's own listing");
    assert_eq!(
        c.readdir(d, 0, second.last().unwrap().off, 4096, false)
            .unwrap()
            .len(),
        0
    );
    assert_eq!(kept(), 0);
    let a = c.readdir(d, 0, 0, 512, false).unwrap();
    let b = c.readdir(d, 0, 0, 512, false).unwrap();
    let third = c.readdir(d, 0, 0, 512, false).unwrap();
    assert_eq!(kept(), 2, "the oldest made room");
    assert!(fs.listed.load(Ordering::Relaxed) <= 150);
    let mut seen = a.len();
    let mut off = a.last().unwrap().off;
    loop {
        let got = c.readdir(d, 0, off, 1 << 16, false).unwrap();
        let Some(last) = got.last() else { break };
        off = last.off;
        seen += got.len();
    }
    assert_eq!(seen, 62);
    let _ = (b, third);
    fs.listings.lock().unwrap().map.clear();
    let mut cfg = Config::new(&s.0);
    cfg.max_listed = Some(10);
    let small = Client::new(Server::new(Box::new(Passthrough::new(&cfg).unwrap())));
    small.init();
    let d2 = small.lookup(ROOT_ID, b"d").unwrap().nodeid;
    assert_eq!(
        small.readdir(d2, 0, 0, 4096, false).err(),
        Some(Errno::ENOMEM)
    );
    let f = c.lookup(d, b"f00").unwrap().nodeid;
    assert_eq!(c.readdir(f, 0, 0, 4096, false).err(), Some(Errno::ENOTDIR));
    assert_eq!(
        c.readdir(987_654, 0, 0, 4096, false).err(),
        Some(Errno::ESTALE)
    );
    c.fsync(d, 0, true).unwrap();
    c.readdir(d, 0, 0, 512, false).unwrap();
    c.call(crate::abi::op::DESTROY, 0, &[]).unwrap();
    assert_eq!((kept(), fs.listed.load(Ordering::Relaxed)), (0, 0));
}

#[test]
fn a_file_with_a_second_name_is_never_trusted_for_long() {
    let t = Watched::new("links");
    std::fs::hard_link(t.s.path("w/file"), t.s.path("out/other-name")).unwrap();
    t.watch(&["w"]);
    assert_eq!(t.valid("w/file"), (LONG, SHORT));
    let file = walk(&t.c, "w/file").unwrap().nodeid;
    assert_eq!(t.c.getattr_valid(file, None).unwrap().1, SHORT);
    let (fh, flags) = t.c.open_flags(file, 0, false).unwrap();
    assert_eq!(flags, 0);
    t.c.release(file, fh).unwrap();
    assert_eq!(t.valid("out/other-name"), (SHORT, SHORT));
    std::fs::remove_file(t.s.path("out/other-name")).unwrap();
    assert_eq!(t.valid("w/file"), (LONG, LONG));
}

#[test]
fn what_leaves_a_watched_directory_is_no_longer_watched() {
    let t = Watched::new("leaves");
    t.watch(&["w"]);
    let (w, out) = (
        walk(&t.c, "w").unwrap().nodeid,
        walk(&t.c, "out").unwrap().nodeid,
    );
    let file = walk(&t.c, "w/file").unwrap().nodeid;
    let sub = walk(&t.c, "w/sub").unwrap().nodeid;
    let deep = walk(&t.c, "w/sub/deep").unwrap().nodeid;
    let valid = |node| t.c.getattr_valid(node, None).unwrap().1;
    assert_eq!((valid(file), valid(sub), valid(deep)), (LONG, LONG, LONG));
    t.c.rename(w, b"file", out, b"file2", 0).unwrap();
    assert_eq!(valid(file), SHORT);
    t.c.rename(w, b"sub", out, b"sub", 0).unwrap();
    assert_eq!((valid(sub), valid(deep)), (SHORT, SHORT));
    assert_eq!(t.valid("out/sub/deep"), (SHORT, SHORT));
    t.c.rename(out, b"sub", w, b"sub", 0).unwrap();
    assert_eq!((valid(sub), valid(deep)), (LONG, LONG));
    let fh = t.c.open(deep, 0).unwrap();
    t.c.unlink(sub, b"deep").unwrap();
    assert_eq!(valid(deep), SHORT);
    assert_eq!(t.c.getattr_valid(deep, Some(fh)).unwrap().1, SHORT);
    t.c.release(deep, fh).unwrap();
}

#[test]
fn nothing_on_a_volume_that_is_not_this_mac_s_own_is_watched() {
    let t = Watched::new("remote");
    t.watch(&["w"]);
    assert_eq!(t.valid("w/file"), (LONG, LONG));
    let dev = std::fs::metadata(t.s.path("w")).unwrap().dev() as i32;
    let file = walk(&t.c, "w/file").unwrap().nodeid;
    let w = walk(&t.c, "w").unwrap().nodeid;
    {
        let mut nodes = t.fs.nodes.lock().unwrap();
        nodes.remote.insert(dev);
        nodes.watch_epoch += 1;
    }
    assert_eq!(t.c.getattr_valid(file, None).unwrap().1, SHORT);
    assert_eq!(t.c.lookup(w, b"nothing"), Err(Errno::ENOENT));
    assert_eq!(t.valid("w/file"), (LONG, LONG));
    let st = lstat(t.s.path("w").as_os_str().as_encoded_bytes()).unwrap();
    assert!(t.fs.volume(&st, b"").local);
    let dev = fs_kind(b"/dev");
    assert!(dev.devfs && dev.local && !dev.boot);
    assert!(fs_kind(t.s.path("w").as_os_str().as_encoded_bytes()).boot);
}

#[test]
fn a_volume_without_path_from_id_keeps_one_second() {
    let t = Watched::with("names", |c| c.no_volfs = true);
    t.watch(&["w"]);
    assert_eq!(t.fs.watch_roots(), 0);
    assert_eq!(t.valid("w/file"), (SHORT, SHORT));
    let w = walk(&t.c, "w").unwrap().nodeid;
    assert_eq!(t.c.lookup(w, b"nothing"), Err(Errno::ENOENT));
}

#[test]
fn a_watch_root_that_moved_is_no_longer_watched() {
    let t = Watched::new("rootmoved");
    t.watch(&["w"]);
    let file = walk(&t.c, "w/file").unwrap().nodeid;
    assert_eq!(t.c.getattr_valid(file, None).unwrap().1, LONG);
    std::fs::rename(t.s.path("w"), t.s.path("w-moved")).unwrap();
    std::fs::create_dir(t.s.path("w")).unwrap();
    let lost = [Change {
        path: t.s.path("w").into_os_string().into_encoded_bytes(),
        flags: fse::ROOT_CHANGED,
        ino: 0,
    }];
    let records = t.fs.records(&lost);
    assert_eq!(records.len(), 1);
    assert_eq!(records[0].op, crate::fsevents::op::INVAL_ALL);
    assert_eq!(t.fs.watch_roots(), 0);
    assert_eq!(t.c.getattr_valid(file, None).unwrap().1, SHORT);
    assert_eq!(t.valid("w-moved/file"), (SHORT, SHORT));
}

#[test]
fn lost_events_under_a_watched_root_are_found_by_a_rescan() {
    use crate::fsevents::{flag, mask, op};
    let t = Watched::new("rescan");
    t.s.write("w/gone", b"");
    t.s.write("w/swap", b"a");
    t.s.write("w/stays", b"");
    t.watch(&["w"]);
    let w = walk(&t.c, "w").unwrap().nodeid;
    let sub = walk(&t.c, "w/sub").unwrap().nodeid;
    let file = walk(&t.c, "w/file").unwrap().nodeid;
    let deep = walk(&t.c, "w/sub/deep").unwrap().nodeid;
    let gone = walk(&t.c, "w/gone").unwrap().nodeid;
    let swap = walk(&t.c, "w/swap").unwrap().nodeid;
    t.c.opendir(w).unwrap();
    let listed = t.c.readdir(w, 0, 0, 32768, true).unwrap();
    assert!(names(&listed).contains(&"stays".to_string()));
    let _ = t.c.lookup(w, b"later");
    std::thread::sleep(notify::RESCAN_MARGIN + Duration::from_millis(200));
    t.fs.records_at(&[], std::time::SystemTime::now());
    t.s.write("w/later", b"new");
    t.s.write("w/file", b"rewritten");
    std::fs::remove_file(t.s.path("w/sub/deep")).unwrap();
    std::fs::remove_file(t.s.path("w/gone")).unwrap();
    t.s.write("w/swap.tmp", b"b");
    std::fs::rename(t.s.path("w/swap.tmp"), t.s.path("w/swap")).unwrap();
    std::fs::create_dir(t.s.path("w/newdir")).unwrap();
    t.s.write("w/newdir/x", b"");
    let lost = [Change {
        path: t.s.path("w").into_os_string().into_encoded_bytes(),
        flags: fse::MUST_SCAN_SUBDIRS | fse::USER_DROPPED,
        ino: 0,
    }];
    let records = t.fs.records(&lost);
    let find = |op: u8, name: &[u8]| {
        records
            .iter()
            .find(|e| e.op == op && e.name == name && e.flags & flag::QUIET == 0)
            .unwrap_or_else(|| {
                panic!(
                    "no {op} for {:?} in {records:?}",
                    String::from_utf8_lossy(name)
                )
            })
    };
    assert_eq!(find(op::CREATE, b"later").parent, w);
    let d = find(op::CREATE, b"newdir");
    assert_eq!((d.parent, d.flags & flag::ISDIR), (w, flag::ISDIR));
    assert!(!records.iter().any(|e| e.name == b"x"), "{records:?}");
    let e = find(op::CHANGE, b"file");
    assert_eq!(
        (e.parent, e.child, e.mask & mask::CONTENT),
        (w, file, mask::CONTENT)
    );
    let e = find(op::DELETE, b"deep");
    assert_eq!((e.parent, e.child), (sub, deep));
    let e = find(op::DELETE, b"gone");
    assert_eq!(
        (e.parent, e.child, e.flags & flag::LAST_LINK),
        (w, gone, flag::LAST_LINK)
    );
    assert_eq!(find(op::DELETE, b"swap").child, swap);
    assert_eq!(find(op::CREATE, b"swap").parent, w);
    assert!(
        !records
            .iter()
            .any(|e| e.name == b"stays" || e.name == b"sub"),
        "{records:?}"
    );
    assert_eq!(records.last().unwrap().op, op::INVAL_ALL);
    assert_eq!(records.iter().filter(|e| e.op == op::INVAL_ALL).count(), 1);
}

#[test]
fn a_rescan_does_not_remove_a_name_told_in_another_case() {
    use crate::fsevents::op;
    let t = Watched::new("rescan-case");
    if case_sensitive(&t.s.0) {
        eprintln!("the scratch volume is case-sensitive: nothing to test");
        return;
    }
    t.s.write("w/License", b"");
    t.watch(&["w"]);
    let w = walk(&t.c, "w").unwrap().nodeid;
    let license = t.c.lookup(w, b"license").unwrap().nodeid;
    assert_ne!(license, 0);
    std::thread::sleep(notify::RESCAN_MARGIN + Duration::from_millis(200));
    t.fs.records_at(&[], std::time::SystemTime::now());
    t.s.write("w/new", b"");
    let records = t.fs.records(&[Change {
        path: t.s.path("w").into_os_string().into_encoded_bytes(),
        flags: fse::MUST_SCAN_SUBDIRS | fse::USER_DROPPED,
        ino: 0,
    }]);
    assert!(
        records
            .iter()
            .any(|e| e.op == op::CREATE && e.name == b"new"),
        "{records:?}"
    );
    assert!(
        !records
            .iter()
            .any(|e| e.name.eq_ignore_ascii_case(b"license")),
        "{records:?}"
    );
}

#[test]
fn a_rescan_announces_only_what_is_new_since_the_batch_before() {
    use crate::fsevents::op;
    let t = Watched::new("rescan-window");
    t.s.write("w/old", b"");
    t.watch(&["w"]);
    let w = walk(&t.c, "w").unwrap().nodeid;
    t.c.opendir(w).unwrap();
    t.c.readdir(w, 0, 0, 32768, false).unwrap();
    std::thread::sleep(notify::RESCAN_MARGIN + Duration::from_millis(200));
    t.fs.records_at(&[], std::time::SystemTime::now());
    t.s.write("w/new", b"");
    let records = t.fs.records(&[Change {
        path: t.s.path("w").into_os_string().into_encoded_bytes(),
        flags: fse::MUST_SCAN_SUBDIRS | fse::USER_DROPPED,
        ino: 0,
    }]);
    assert!(
        records
            .iter()
            .any(|e| e.op == op::CREATE && e.name == b"new" && e.parent == w),
        "{records:?}"
    );
    assert!(
        !records
            .iter()
            .any(|e| e.name == b"old" || e.name == b"file" || e.name == b"sub"),
        "{records:?}"
    );
}

#[test]
fn a_rescan_past_its_bound_is_one_inval_all() {
    use crate::fsevents::op;
    let t = Watched::new("rescan-bound");
    for i in 0..300 {
        t.s.write(&format!("w/f{i}"), b"");
    }
    t.watch(&["w"]);
    let w = walk(&t.c, "w").unwrap().nodeid;
    t.c.opendir(w).unwrap();
    t.c.readdir(w, 0, 0, 65536, true).unwrap();
    *t.fs.rescan_max.lock().unwrap() = Some(100);
    t.fs.records_at(&[], std::time::SystemTime::now());
    t.s.write("w/new", b"");
    let records = t.fs.records(&[Change {
        path: t.s.path("w").into_os_string().into_encoded_bytes(),
        flags: fse::MUST_SCAN_SUBDIRS | fse::USER_DROPPED,
        ino: 0,
    }]);
    assert_eq!(records.len(), 1, "{records:?}");
    assert_eq!(records[0].op, op::INVAL_ALL);
}

#[test]
fn a_rescan_finds_a_told_file_changed_before_the_margin() {
    use crate::fsevents::{mask, op};
    let t = Watched::new("rescan-seen");
    t.watch(&["w"]);
    let w = walk(&t.c, "w").unwrap().nodeid;
    let file = walk(&t.c, "w/file").unwrap().nodeid;
    let path = t.s.path("w/file").into_os_string().into_encoded_bytes();
    t.s.write("w/file", b"first");
    let records = t.fs.records(&[Change {
        path: path.clone(),
        flags: fse::MODIFIED,
        ino: 0,
    }]);
    assert!(
        records
            .iter()
            .any(|e| e.op == op::CHANGE && e.name == b"file"),
        "{records:?}"
    );
    t.s.write("w/file", b"second, and longer");
    std::thread::sleep(notify::RESCAN_MARGIN + Duration::from_millis(300));
    t.fs.records_at(&[], std::time::SystemTime::now());
    let records = t.fs.records(&[Change {
        path: t.s.path("w").into_os_string().into_encoded_bytes(),
        flags: fse::MUST_SCAN_SUBDIRS | fse::USER_DROPPED,
        ino: 0,
    }]);
    let e = records
        .iter()
        .find(|e| e.op == op::CHANGE && e.name == b"file")
        .unwrap_or_else(|| panic!("no CHANGE for file in {records:?}"));
    assert_eq!(
        (e.parent, e.child, e.mask & mask::CONTENT),
        (w, file, mask::CONTENT)
    );
    assert_eq!(records.last().unwrap().op, op::INVAL_ALL);
}

#[test]
fn a_directory_read_by_node_can_be_resumed_from_any_offset_it_gave() {
    let s = Scratch::new("seekdir");
    for i in 0..500 {
        s.write(&format!("d/name-{i:04}"), b"");
    }
    let c = s.client();
    let d = c.lookup(ROOT_ID, b"d").unwrap().nodeid;
    assert_eq!(c.opendir(d).unwrap(), 0);
    for size in [4096u32, 32768] {
        let mut from = Vec::new();
        let mut seen = Vec::new();
        let mut pos = 0;
        for i in 0..500 {
            let plus = pos == 0;
            let got = c.readdir(d, 0, pos, size, plus).unwrap();
            let first = got
                .first()
                .unwrap_or_else(|| panic!("end of the directory at entry {i}, offset {pos:#x}"));
            from.push(pos);
            seen.push(first.name.clone());
            pos = first.off;
        }
        let mut names = seen.clone();
        names.sort();
        names.dedup();
        assert_eq!(names.len(), 500, "every entry once");
        for i in (0..500).rev().step_by(7) {
            let got = c.readdir(d, 0, from[i], size, false).unwrap();
            assert_eq!(got.first().map(|e| &e.name), Some(&seen[i]), "entry {i}");
        }
    }
}

#[test]
fn an_answer_from_before_a_change_the_feed_has_passed_is_not_kept_for_long() {
    use notify::{fse, Change};
    let t = Watched::new("raced");
    let old = t.s.path("w/old");
    std::fs::write(&old, b"before").unwrap();
    let status = std::process::Command::new("touch")
        .args(["-t", "202001010000"])
        .arg(&old)
        .status()
        .unwrap();
    assert!(status.success());
    t.watch(&["w"]);
    let w = walk(&t.c, "w").unwrap().nodeid;
    let bytes = |p: &std::path::Path| p.as_os_str().as_encoded_bytes().to_vec();

    let (fs, path) = (t.fs.clone(), old.clone());
    *t.fs.after_stat.lock().unwrap() = Some(Box::new(move || {
        std::fs::write(&path, b"after, and longer").unwrap();
        let ino = std::os::unix::fs::MetadataExt::ino(&std::fs::metadata(&path).unwrap());
        let records = fs.records(&[Change {
            path: bytes(&path),
            flags: fse::MODIFIED,
            ino,
        }]);
        assert!(records.iter().all(|e| e.child == 0), "{records:?}");
    }));
    let e = t.c.lookup(w, b"old").unwrap();
    assert_eq!(e.attr.size, 6, "the answer is from before the change");
    assert_eq!(
        (e.entry_valid, e.attr_valid),
        (SHORT, SHORT),
        "and is not to be trusted for long"
    );
    let e = t.c.lookup(w, b"old").unwrap();
    assert_eq!((e.attr.size, e.attr_valid), (17, LONG));

    let (fs, path) = (t.fs.clone(), t.s.path("w/readme"));
    *t.fs.after_stat.lock().unwrap() = Some(Box::new(move || {
        std::fs::write(&path, b"x").unwrap();
        let ino = std::os::unix::fs::MetadataExt::ino(&std::fs::metadata(&path).unwrap());
        fs.records(&[Change {
            path: bytes(&path),
            flags: fse::CREATED,
            ino,
        }]);
    }));
    assert_eq!(t.c.lookup(w, b"README"), Err(Errno::ENOENT), "no timeout");
    assert_eq!(t.fs.nodes.lock().unwrap().absent_len, 0);

    let sub = walk(&t.c, "w/sub").unwrap().nodeid;
    let before = t.fs.changes.load(Ordering::Relaxed);
    t.fs.records(&[]);
    assert_eq!(t.fs.changes.load(Ordering::Relaxed), before + 1);
    let listed = t.c.list(sub, true).unwrap();
    let deep = listed.iter().find(|e| e.name == b"deep").unwrap();
    assert_eq!(deep.entry.as_ref().unwrap().attr_valid, LONG);
}

#[test]
#[ignore]
fn small_file_costs() {
    use std::time::Instant;
    let s = Scratch::new("costs");
    let c = s.client();
    let n = 20_000u32;
    let dir = c.mkdir(ROOT_ID, b"small", 0o755).unwrap().nodeid;
    c.opendir(dir).unwrap();
    let data = [b'x'; 4096];
    let (mut tc, mut tw, mut tr) = (Duration::ZERO, Duration::ZERO, Duration::ZERO);
    let mut nodes = Vec::new();
    for i in 0..n {
        let name = format!("{i:05}");
        let a = Instant::now();
        let (e, fh) = c.create(dir, name.as_bytes(), 0o100644, 0o101).unwrap();
        let b = Instant::now();
        c.write(e.nodeid, fh, 0, &data).unwrap();
        let w = Instant::now();
        c.release(e.nodeid, fh).unwrap();
        tr += w.elapsed();
        tc += b - a;
        tw += w - b;
        nodes.push(e.nodeid);
    }
    let us = |d: Duration| d.as_secs_f64() * 1e6 / n as f64;
    println!(
        "  CREATE {:.1}  WRITE {:.1}  RELEASE {:.1} us a file",
        us(tc),
        us(tw),
        us(tr)
    );
    let a = Instant::now();
    for node in &nodes {
        assert_eq!(c.read(*node, 0, 0, 8192).unwrap().len(), 4096);
    }
    println!(
        "  READ by node, descriptor not kept: {:.1} us a file",
        us(a.elapsed())
    );
    let a = Instant::now();
    for node in &nodes[nodes.len() - 400..] {
        for _ in 0..50 {
            c.read(*node, 0, 0, 8192).unwrap();
        }
    }
    println!("  READ by node, descriptor kept: {:.1} us", us(a.elapsed()));
    let a = Instant::now();
    for node in &nodes {
        let fh = c.open(*node, 0).unwrap();
        c.read(*node, fh, 0, 8192).unwrap();
        c.release(*node, fh).unwrap();
    }
    println!("  OPEN, READ, RELEASE: {:.1} us a file", us(a.elapsed()));
    let a = Instant::now();
    let listed = c.list(dir, true).unwrap();
    println!(
        "  READDIRPLUS: {:.2} us an entry ({})",
        us(a.elapsed()),
        listed.len()
    );
    let a = Instant::now();
    for i in 0..n {
        c.unlink(dir, format!("{i:05}").as_bytes()).unwrap();
    }
    println!("  UNLINK: {:.1} us a file", us(a.elapsed()));
}

#[test]
fn nothing_is_trusted_for_long_while_the_feed_is_stuck_on_a_batch() {
    use notify::{fse, Change};
    let t = Watched::new("stuck");
    t.watch(&["w"]);
    let w = walk(&t.c, "w").unwrap().nodeid;
    assert_eq!(t.valid("w/file"), (LONG, LONG));
    let file = walk(&t.c, "w/file").unwrap().nodeid;

    let half = STUCK.as_nanos() as u64 / 2;
    t.fs.batch_began
        .store(since_start().saturating_sub(half).max(1), Ordering::Relaxed);
    assert_eq!(t.valid("w/file"), (LONG, LONG));
    let two = 2 * STUCK.as_nanos() as u64;
    std::thread::sleep(STUCK * 2);
    t.fs.batch_began
        .store(since_start().saturating_sub(two).max(1), Ordering::Relaxed);
    assert_eq!(t.valid("w/file"), (SHORT, SHORT));
    assert_eq!(t.c.getattr_valid(file, None).unwrap().1, SHORT);
    assert_eq!(t.c.lookup(w, b"nothing"), Err(Errno::ENOENT));
    for e in t.c.list(w, true).unwrap().iter().filter_map(|e| e.entry) {
        assert!(e.attr_valid <= SHORT && e.entry_valid <= SHORT, "{e:?}");
    }
    let fh = t.c.open_flags(file, 0, false).unwrap();
    assert_eq!(fh.1 & FOPEN_KEEP_CACHE, 0, "nor are its pages kept");
    t.c.release(file, fh.0).unwrap();
    t.fs.batch_began.store(0, Ordering::Relaxed);
    assert_eq!(t.valid("w/file"), (LONG, LONG));
    assert_eq!(t.c.lookup(w, b"nothing").unwrap().nodeid, 0);

    let path = t.s.path("w/file");
    let ino = std::fs::metadata(&path).unwrap().ino();
    let change = || Change {
        path: path.as_os_str().as_encoded_bytes().to_vec(),
        flags: fse::MODIFIED,
        ino,
    };
    std::fs::write(&path, b"changed once").unwrap();
    let quick = t.fs.records(&[change()]);
    assert!(
        quick.iter().all(|e| e.op != crate::fsevents::op::INVAL_ALL),
        "{quick:?}"
    );
    std::fs::write(&path, b"changed again, more").unwrap();
    *t.fs.batch_delay.lock().unwrap() = Some(STUCK + Duration::from_millis(100));
    let slow = t.fs.records(&[change()]);
    assert_eq!(
        slow.last().map(|e| e.op),
        Some(crate::fsevents::op::INVAL_ALL),
        "{slow:?}"
    );
    assert_eq!(t.fs.batch_began.load(Ordering::Relaxed), 0);
}

#[test]
fn nothing_is_trusted_for_long_once_the_watchdog_has_rung() {
    let t = Watched::new("overdue");
    t.watch(&["w"]);
    let w = walk(&t.c, "w").unwrap().nodeid;
    assert_eq!(t.valid("w/file"), (LONG, LONG));
    let file = walk(&t.c, "w/file").unwrap().nodeid;
    for began in [0, since_start()] {
        t.fs.batch_began.store(began, Ordering::Relaxed);
        t.fs.set_overdue(true);
        assert_eq!(t.valid("w/file"), (SHORT, SHORT), "began {began}");
        assert_eq!(t.c.getattr_valid(file, None).unwrap().1, SHORT);
        assert_eq!(t.c.lookup(w, b"nothing"), Err(Errno::ENOENT));
        let fh = t.c.open_flags(file, 0, false).unwrap();
        assert_eq!(fh.1 & FOPEN_KEEP_CACHE, 0, "nor are its pages kept");
        t.c.release(file, fh.0).unwrap();
        t.fs.set_overdue(false);
        t.fs.batch_began.store(0, Ordering::Relaxed);
        assert_eq!(t.valid("w/file"), (LONG, LONG));
        assert_eq!(t.c.lookup(w, b"nothing").unwrap().nodeid, 0);
    }
}

#[test]
fn a_file_is_read_without_an_open_only_where_the_server_said_so() {
    let s = Scratch::new("free");
    s.write("d/a", b"content of a");
    s.write("d/secret", b"mode 000");
    s.write("e/b", b"content of b");
    std::fs::set_permissions(s.path("d/secret"), PermissionsExt::from_mode(0o000)).unwrap();
    let c = s.client();
    let free = |e: &Entry| e.attr.flags & ATTR_OPEN_FREE != 0;

    let e = walk(&c, "e/b").unwrap();
    assert!(!free(&e));
    assert_eq!(c.read(e.nodeid, 0, 0, 64).unwrap(), b"content of b");
    let fh = c.open(e.nodeid, 0).unwrap();
    c.release(e.nodeid, fh).unwrap();
    assert!(free(&walk(&c, "e/b").unwrap()));

    let d = walk(&c, "d").unwrap().nodeid;
    let listed = c.list(d, true).unwrap();
    let entry = |name: &[u8]| {
        listed
            .iter()
            .find(|e| e.name == name)
            .unwrap()
            .entry
            .unwrap()
    };
    let (a, secret) = (entry(b"a"), entry(b"secret"));
    assert!(free(&a) && !free(&secret));
    assert!(listed
        .iter()
        .all(|e| e.name != b"d" || !free(&e.entry.unwrap())));
    assert_ne!(c.getattr(a.nodeid).unwrap().flags & ATTR_OPEN_FREE, 0);
    assert_eq!(c.getattr(secret.nodeid).unwrap().flags & ATTR_OPEN_FREE, 0);
    assert_eq!(c.chmod(a.nodeid, 0o200).unwrap().flags & ATTR_OPEN_FREE, 0);
    assert_ne!(c.chmod(a.nodeid, 0o644).unwrap().flags & ATTR_OPEN_FREE, 0);

    assert_eq!(c.read(a.nodeid, 0, 0, 64).unwrap(), b"content of a");
    assert_eq!(c.read(a.nodeid, 0, 8, 64).unwrap(), b"of a");
    assert_eq!(c.getattr_valid(a.nodeid, Some(0)).unwrap().0.size, 12);
    assert_eq!(
        c.lseek(a.nodeid, 0, 0, 4).unwrap(),
        12,
        "SEEK_HOLE at the end"
    );
    c.fsync(a.nodeid, 0, false).unwrap();
    assert_eq!(c.read(secret.nodeid, 0, 0, 64).err(), Some(Errno::EACCES));
    assert_eq!(c.read(d, 0, 0, 64).err(), Some(Errno::EISDIR));
    assert_eq!(c.write(a.nodeid, 0, 0, b"x").err(), Some(Errno::EBADF));

    std::fs::write(s.path("d/a"), b"rewritten on the Mac").unwrap();
    assert_eq!(c.read(a.nodeid, 0, 0, 64).unwrap(), b"rewritten on the Mac");
    s.write("d/a.new", b"the replacement");
    std::fs::rename(s.path("d/a.new"), s.path("d/a")).unwrap();
    let now = c.lookup(d, b"a").unwrap();
    assert_ne!(now.nodeid, a.nodeid);
    assert_eq!(c.read(now.nodeid, 0, 0, 64).unwrap(), b"the replacement");
    assert_eq!(c.read(a.nodeid, 0, 0, 64).unwrap(), b"rewritten on the Mac");
    assert_eq!(fds_under(&s.0), 2, "one for each node read");
    c.forget(a.nodeid, 1);
    assert_eq!(fds_under(&s.0), 1);
    assert_eq!(c.read(a.nodeid, 0, 0, 64).err(), Some(Errno::ESTALE));

    let old = {
        let c = Client::new(Server::new(Box::new(
            Passthrough::new(&Config::new(&s.0)).unwrap(),
        )));
        let offered = !(1u64 << 31) & !crate::abi::init::FEV_REQUESTS;
        c.init_with(crate::abi::MAJOR, crate::abi::MINOR, 128 << 10, offered)
            .unwrap();
        c
    };
    let d = walk(&old, "d").unwrap().nodeid;
    for e in old.list(d, true).unwrap().iter().filter_map(|e| e.entry) {
        assert_eq!(e.attr.flags & ATTR_OPEN_FREE, 0);
    }
    let names = s.client_with(|c| c.no_volfs = true);
    let d = walk(&names, "d").unwrap().nodeid;
    for e in names.list(d, true).unwrap().iter().filter_map(|e| e.entry) {
        assert_eq!(e.attr.flags & ATTR_OPEN_FREE, 0);
    }
}

#[test]
fn a_file_read_without_a_handle_outlives_its_name() {
    let s = Scratch::new("freegone");
    for name in ["d/pinned", "d/read", "d/over", "d/new", "d/idle"] {
        s.write(name, name.as_bytes());
    }
    let (fs, c) = s.shared();
    let d = walk(&c, "d").unwrap().nodeid;
    let listed = c.list(d, true).unwrap();
    let node = |name: &[u8]| {
        listed
            .iter()
            .find(|e| e.name == name)
            .unwrap()
            .entry
            .unwrap()
            .nodeid
    };

    let pinned = node(b"pinned");
    let fh = c.open(pinned, 0).unwrap();
    c.unlink(d, b"pinned").unwrap();
    c.release(pinned, fh).unwrap();
    assert_eq!(c.read(pinned, 0, 0, 64).unwrap(), b"d/pinned");
    assert_eq!(c.getattr(pinned).unwrap().nlink, 0);

    let read = node(b"read");
    assert_eq!(c.read(read, 0, 0, 64).unwrap(), b"d/read");
    c.unlink(d, b"read").unwrap();
    assert_eq!(c.read(read, 0, 2, 64).unwrap(), b"read");

    let over = node(b"over");
    assert_eq!(c.read(over, 0, 0, 64).unwrap(), b"d/over");
    c.rename(d, b"new", d, b"over", 0).unwrap();
    assert_eq!(c.read(over, 0, 0, 64).unwrap(), b"d/over");
    assert_eq!(c.read(node(b"new"), 0, 0, 64).unwrap(), b"d/new");

    assert_eq!(fs.orphans(), 3);
    for n in [pinned, read, over] {
        c.forget(n, 1);
        assert_eq!(c.read(n, 0, 0, 64).err(), Some(Errno::ESTALE));
    }
    assert_eq!(fs.orphans(), 0);

    let idle = node(b"idle");
    std::fs::remove_file(s.path("d/idle")).unwrap();
    assert_eq!(c.read(idle, 0, 0, 64).err(), Some(Errno::ESTALE));
    s.write("d/busy", b"d/busy");
    let busy = c.lookup(d, b"busy").unwrap().nodeid;
    assert_eq!(c.read(busy, 0, 0, 0).unwrap(), b"");
    std::fs::remove_file(s.path("d/busy")).unwrap();
    assert_eq!(c.read(busy, 0, 0, 64).unwrap(), b"d/busy");
    let gone = c.getattr_valid(busy, None).unwrap();
    assert_eq!((gone.0.size, gone.0.nlink, gone.1), (6, 0, SHORT));
    assert_eq!(c.getattr_valid(busy, Some(0)).unwrap().0.size, 6);
    assert_eq!(c.read(idle, 0, 0, 0).err(), Some(Errno::ESTALE));
}

#[test]
fn descriptors_kept_without_a_handle_are_bounded() {
    let s = Scratch::new("freebound");
    let n = free::MAX_FREE_FDS + 40;
    for i in 0..n {
        s.write(&format!("many/{i:04}"), b"x");
    }
    let (fs, c) = s.shared();
    let many = walk(&c, "many").unwrap().nodeid;
    let listed = c.list(many, true).unwrap();
    for e in listed.iter().filter_map(|e| e.entry) {
        if e.attr.mode & S_IFMT == S_IFREG {
            assert_eq!(c.read(e.nodeid, 0, 0, 8).unwrap(), b"x");
        }
    }
    assert_eq!(fs.free_fds_held(), free::MAX_FREE_FDS);
    assert_eq!(fds_under(&s.0), free::MAX_FREE_FDS);
    for i in 0..free::MAX_DIR_FDS + 20 {
        let dir = c
            .mkdir(ROOT_ID, format!("dir{i:03}").as_bytes(), 0o755)
            .unwrap();
        let (e, fh) = c.create(dir.nodeid, b"f", 0o644, O_WRONLY).unwrap();
        c.release(e.nodeid, fh).unwrap();
    }
    assert!(
        dirs_under(&s.0) <= free::MAX_DIR_FDS + DIR_SLOTS,
        "{}",
        dirs_under(&s.0)
    );
    c.call(crate::abi::op::DESTROY, 0, &[]).unwrap();
    assert_eq!(fds_settled(&s.0, 0), 0);
    assert_eq!(dirs_under(&s.0), 0);
}

#[test]
fn a_listing_read_in_bulk_says_what_a_lookup_says() {
    let t = Watched::new("bulk");
    let w = t.s.path("w");
    std::fs::write(w.join("empty"), b"").unwrap();
    std::fs::write(w.join("large"), vec![7u8; 300_000]).unwrap();
    std::fs::write(w.join("exec"), b"#!/bin/sh\n").unwrap();
    std::fs::set_permissions(w.join("exec"), PermissionsExt::from_mode(0o4755)).unwrap();
    std::fs::write(w.join("ro"), b"read only").unwrap();
    std::fs::set_permissions(w.join("ro"), PermissionsExt::from_mode(0o400)).unwrap();
    std::fs::hard_link(w.join("large"), w.join("large-too")).unwrap();
    symlink("file", w.join("link")).unwrap();
    symlink("nowhere/at/all", w.join("dangling")).unwrap();
    let fifo = std::ffi::CString::new(w.join("fifo").as_os_str().as_bytes()).unwrap();
    assert_eq!(unsafe { libc::mkfifo(fifo.as_ptr(), 0o644) }, 0);
    std::fs::write(w.join("attributed"), b"with an attribute").unwrap();
    let key = std::ffi::CString::new("user.fev.test").unwrap();
    let path = std::ffi::CString::new(w.join("attributed").as_os_str().as_bytes()).unwrap();
    assert_eq!(
        unsafe { libc::setxattr(path.as_ptr(), key.as_ptr(), b"v".as_ptr().cast(), 1, 0, 0) },
        0
    );
    let c = &t.c;
    let wn = walk(c, "w").unwrap().nodeid;
    c.mknod(wn, b"null", 0o020666, 0x103).unwrap();

    let check = |c: &Client, wn: u64| {
        let listed = c.list(wn, true).unwrap();
        let mut names: Vec<_> = listed.iter().map(|e| e.name.clone()).collect();
        names.sort();
        let mut want: Vec<Vec<u8>> = std::fs::read_dir(&w)
            .unwrap()
            .map(|e| e.unwrap().file_name().as_bytes().to_vec())
            .chain([b".".to_vec(), b"..".to_vec()])
            .collect();
        want.sort();
        assert_eq!(names, want);
        for e in &listed {
            if e.name == b"." || e.name == b".." {
                continue;
            }
            let (plus, looked) = (e.entry.unwrap(), c.lookup(wn, &e.name).unwrap());
            let name = String::from_utf8_lossy(&e.name).into_owned();
            assert_eq!(plus.nodeid, looked.nodeid, "{name}");
            assert_eq!(plus.attr, looked.attr, "{name}");
            assert_eq!(e.kind, looked.attr.mode >> 12, "{name}");
            assert_eq!(e.ino, looked.attr.ino, "{name}");
        }
        listed
    };
    for e in check(c, wn).iter().filter_map(|e| e.entry) {
        assert!(e.attr_valid <= SHORT && e.entry_valid <= SHORT);
        assert!(e.attr_valid > SHORT / 2, "a listing a moment old: {e:?}");
    }
    t.watch(&["w"]);
    let listed = check(c, wn);
    let of = |name: &[u8]| {
        listed
            .iter()
            .find(|e| e.name == name)
            .unwrap()
            .entry
            .unwrap()
    };
    assert_eq!(
        (of(b"empty").entry_valid, of(b"empty").attr_valid),
        (LONG, LONG)
    );
    assert!(
        of(b"large").attr_valid <= SHORT,
        "a second name: never for long"
    );

    let fh = c.opendir(wn).unwrap();
    let first = c.readdir(wn, fh, 0, 1024, true).unwrap();
    assert!(first
        .iter()
        .filter_map(|e| e.entry)
        .all(|e| e.attr_valid >= SHORT / 2));
    std::fs::write(w.join("ro.new"), b"read only, and longer").unwrap();
    std::fs::rename(w.join("ro.new"), w.join("zz-last")).unwrap();
    std::fs::write(w.join("file"), b"watched, and changed").unwrap();
    t.fs.records(&[Change {
        path: w.join("file").into_os_string().into_encoded_bytes(),
        flags: fse::MODIFIED,
        ino: 0,
    }]);
    let rest_of = |first: &[crate::client::DirEntry]| {
        let longest = crate::abi::ENTRY_OUT_LEN + crate::abi::dirent_len(255);
        let mut off = first.last().unwrap().off;
        let mut rest = Vec::new();
        loop {
            let batch = c.readdir(wn, fh, off, 1024, true).unwrap();
            let Some(last) = batch.last() else { break };
            off = last.off;
            let used: usize = batch
                .iter()
                .map(|e| crate::abi::ENTRY_OUT_LEN + crate::abi::dirent_len(e.name.len()))
                .sum();
            rest.extend(batch);
            if used + longest <= 1024 {
                break;
            }
        }
        rest
    };
    let rest = rest_of(&first);
    assert!(!rest.is_empty(), "the listing took more than one request");
    let mut files = 0;
    for d in &rest {
        let Some(e) = d.entry else { continue };
        if e.attr.mode & S_IFMT != S_IFREG {
            continue;
        }
        files += 1;
        let looked = c.lookup(wn, &d.name).unwrap();
        assert_eq!((e.nodeid, e.attr), (looked.nodeid, looked.attr), "{d:?}");
        if d.name == b"file" {
            assert_eq!(e.attr.size, 20, "changed after the listing was taken");
        }
        if e.attr.nlink == 1 {
            assert_eq!((e.entry_valid, e.attr_valid), (LONG, LONG), "{d:?}");
        }
    }
    assert!(files > 0);
    let fh = c.opendir(wn).unwrap();
    let first = c.readdir(wn, fh, 0, 1024, true).unwrap();
    std::fs::write(w.join("large"), b"small now").unwrap();
    std::thread::sleep(SHORT + Duration::from_millis(50));
    let rest = rest_of(&first);
    for name in [&b"large"[..], b"large-too"] {
        if let Some(e) = rest.iter().find(|e| e.name == name) {
            assert_eq!(e.entry.unwrap().attr.size, 9);
        }
    }
    assert!(
        rest.iter()
            .any(|e| e.name == b"large" || e.name == b"large-too"),
        "one of the two names is past the first request"
    );
}

#[test]
fn a_listing_from_before_a_batch_does_not_put_back_what_the_batch_took() {
    use notify::{fse, Change};
    let t = Watched::new("bulkraced");
    let w = t.s.path("w");
    for i in 0..40 {
        std::fs::write(w.join(format!("f{i:02}")), b"before").unwrap();
    }
    t.watch(&["w"]);
    let c = &t.c;
    let wn = walk(c, "w").unwrap().nodeid;
    let fh = c.opendir(wn).unwrap();
    let first = c.readdir(wn, fh, 0, 1024, true).unwrap();
    let name = (0..40)
        .map(|i| format!("f{i:02}"))
        .find(|n| !first.iter().any(|e| e.name == n.as_bytes()))
        .expect("the listing took more than one request");
    let old = c.lookup(wn, name.as_bytes()).unwrap();

    let tmp = t.s.path("out/save.tmp");
    std::fs::write(&tmp, b"after, and longer").unwrap();
    let ino = std::fs::metadata(&tmp).unwrap().ino();
    std::fs::rename(&tmp, w.join(&name)).unwrap();
    let path = w.join(&name).into_os_string().into_encoded_bytes();
    let records = t.fs.records(&[Change {
        path: path.clone(),
        flags: fse::RENAMED,
        ino,
    }]);
    assert!(
        records
            .iter()
            .any(|e| e.op == crate::fsevents::op::DELETE && e.child == old.nodeid),
        "the guest is told the old file left the name: {records:?}"
    );

    let mut off = first.last().unwrap().off;
    let mut found = None;
    loop {
        let batch = c.readdir(wn, fh, off, 1024, true).unwrap();
        let Some(last) = batch.last() else { break };
        off = last.off;
        found = found.or(batch.into_iter().find(|e| e.name == name.as_bytes()));
    }
    let e = found.expect("the file is listed").entry.unwrap();
    assert_ne!(e.nodeid, old.nodeid, "the old file's node, at its old name");
    assert_eq!((e.attr.ino, e.attr.size), (ino, 17), "what is there now");

    std::fs::write(w.join(&name), b"after, and edited in place").unwrap();
    let records = t.fs.records(&[Change {
        path,
        flags: fse::MODIFIED,
        ino,
    }]);
    assert!(
        records.iter().all(|e| e.op != crate::fsevents::op::DELETE),
        "{records:?}"
    );
    assert!(
        records
            .iter()
            .any(|e| e.op == crate::fsevents::op::CHANGE && e.child != 0 && e.child != old.nodeid),
        "{records:?}"
    );
}

#[test]
fn a_listing_goes_with_its_last_reply_for_a_guest_that_knows_it() {
    let s = Scratch::new("lastreply");
    for i in 0..10 {
        s.write(&format!("d/f{i}"), b"x");
    }
    let listings = |fs: &Passthrough| fs.listings.lock().unwrap().map.len();
    for knows in [true, false] {
        let fs = Arc::new(Passthrough::new(&Config::new(&s.0)).unwrap());
        let c = Client::new(Server::shared(fs.clone()));
        let mut offered = !(1u64 << 31);
        if !knows {
            offered &= !crate::abi::init::FEV_REQUESTS;
        }
        c.init_with(crate::abi::MAJOR, crate::abi::MINOR, 128 << 10, offered)
            .unwrap();
        let d = walk(&c, "d").unwrap().nodeid;
        let fh = c.opendir(d).unwrap();
        let all = c.readdir(d, fh, 0, 32 << 10, true).unwrap();
        assert_eq!(all.len(), 12);
        assert_eq!(listings(&fs), usize::from(!knows), "knows: {knows}");
        let end = c
            .readdir(d, fh, all.last().unwrap().off, 32 << 10, true)
            .unwrap();
        assert!(end.is_empty());
        assert_eq!(listings(&fs), 0);
        let fh = c.opendir(d).unwrap();
        let exact: usize = all
            .iter()
            .map(|e| crate::abi::ENTRY_OUT_LEN + crate::abi::dirent_len(e.name.len()))
            .sum();
        let all = c.readdir(d, fh, 0, exact as u32 + 100, true).unwrap();
        assert_eq!(all.len(), 12);
        assert_eq!(listings(&fs), 1);
        assert!(c
            .readdir(d, fh, all.last().unwrap().off, 4096, true)
            .unwrap()
            .is_empty());
        assert_eq!(listings(&fs), 0);
    }
}

#[test]
fn a_created_file_has_the_guest_s_mode_and_says_it_was_made() {
    let s = Scratch::new("created");
    let c = s.client();
    for (i, (mode, want)) in [
        (0o644, 0o644),
        (0o666, 0o644),
        (0o600, 0o600),
        (0o777, 0o755),
        (0o444, 0o444),
        (0o000, 0o000),
    ]
    .into_iter()
    .enumerate()
    {
        let name = format!("f{i}");
        let (e, fh) = c
            .create(ROOT_ID, name.as_bytes(), 0o100000 | mode, O_WRONLY)
            .unwrap();
        assert_eq!(e.attr.mode & 0o7777, want, "{mode:o}");
        assert_eq!(
            std::fs::metadata(s.path(&name))
                .unwrap()
                .permissions()
                .mode()
                & 0o7777,
            want
        );
        assert_ne!(
            e.attr.flags & crate::abi::ATTR_NO_SEC,
            0,
            "made by this request"
        );
        assert_eq!(c.write(e.nodeid, fh, 0, b"x").unwrap(), 1);
        c.release(e.nodeid, fh).unwrap();
    }
    s.write("there", b"already");
    let (e, fh) = c.create(ROOT_ID, b"there", 0o100644, O_WRONLY).unwrap();
    assert_eq!(e.attr.flags & crate::abi::ATTR_NO_SEC, 0);
    assert_eq!(e.attr.size, 7);
    c.release(e.nodeid, fh).unwrap();
    const O_EXCL: u32 = 0o200;
    assert_eq!(
        c.create(ROOT_ID, b"there", 0o100644, O_WRONLY | O_EXCL)
            .err(),
        Some(Errno(17))
    );
    std::fs::create_dir(s.path("dir")).unwrap();
    symlink("there", s.path("link")).unwrap();
    assert!(c.create(ROOT_ID, b"dir", 0o100644, O_WRONLY).is_err());
    assert!(c.create(ROOT_ID, b"link", 0o100644, O_WRONLY).is_err());
    assert_eq!(std::fs::read(s.path("there")).unwrap(), b"already");
}

fn complete(fs: &Passthrough, node: u64) -> bool {
    fs.nodes.lock().unwrap().by_id[&node].complete
}

#[test]
fn a_directory_the_guest_made_is_complete_only_where_a_miss_is_cached_for_long() {
    use crate::abi::ATTR_COMPLETE;
    let t = Watched::new("complete-grant");
    let (w, out) = (
        walk(&t.c, "w").unwrap().nodeid,
        walk(&t.c, "out").unwrap().nodeid,
    );
    let granted = |e: &Entry| e.attr.flags & ATTR_COMPLETE != 0;
    let d = t.c.mkdir(w, b"before", 0o755).unwrap();
    assert!(!granted(&d) && !complete(&t.fs, d.nodeid));
    t.watch(&["w"]);
    let d = t.c.mkdir(w, b"made", 0o755).unwrap();
    assert!(granted(&d) && complete(&t.fs, d.nodeid));
    let deep = t.c.mkdir(d.nodeid, b"deep", 0o755).unwrap();
    assert!(granted(&deep) && complete(&t.fs, deep.nodeid));
    let o = t.c.mkdir(out, b"made", 0o755).unwrap();
    assert!(!granted(&o) && !complete(&t.fs, o.nodeid));
    assert!(!granted(&t.c.lookup(w, b"made").unwrap()));
    assert!(!granted(&t.c.lookup(w, b"sub").unwrap()));
    let fs = t.fs.clone();
    *t.fs.after_stat.lock().unwrap() = Some(Box::new(move || {
        fs.records(&[]);
    }));
    let raced = t.c.mkdir(w, b"raced", 0o755).unwrap();
    assert!(!granted(&raced) && !complete(&t.fs, raced.nodeid));
    *t.fs.batch_delay.lock().unwrap() = Some(STUCK + Duration::from_millis(300));
    let fs = t.fs.clone();
    let feed = std::thread::spawn(move || fs.records(&[]));
    std::thread::sleep(STUCK + Duration::from_millis(100));
    let stuck = t.c.mkdir(w, b"stuck", 0o755).unwrap();
    assert!(!granted(&stuck) && !complete(&t.fs, stuck.nodeid));
    feed.join().unwrap();
    t.watch(&[]);
    let after = t.c.mkdir(w, b"after", 0o755).unwrap();
    assert!(!granted(&after) && !complete(&t.fs, after.nodeid));
    t.watch(&["w"]);
    let plain = Client::new(Server::shared(t.fs.clone()));
    plain
        .init_with(
            crate::abi::MAJOR,
            crate::abi::MINOR,
            128 << 10,
            !(1u64 << 31) & !crate::abi::init::FEV_COMPLETE,
        )
        .unwrap();
    let w = walk(&plain, "w").unwrap().nodeid;
    let p = plain.mkdir(w, b"plain", 0o755).unwrap();
    assert!(!granted(&p) && !complete(&t.fs, p.nodeid));
    let lone = Client::new(Server::shared(t.fs.clone()));
    lone.init_with(
        crate::abi::MAJOR,
        crate::abi::MINOR,
        128 << 10,
        !(1u64 << 31) & !crate::abi::init::FEV_REQUESTS,
    )
    .unwrap();
    let w = walk(&lone, "w").unwrap().nodeid;
    let l = lone.mkdir(w, b"lone", 0o755).unwrap();
    assert!(!granted(&l) && !complete(&t.fs, l.nodeid));
}

#[test]
fn what_the_mac_puts_into_a_complete_directory_is_announced_and_ends_it() {
    use crate::fsevents::op;
    let t = Watched::new("complete-revoke");
    t.fs.set_feeding(true);
    t.watch(&["w"]);
    let w = walk(&t.c, "w").unwrap().nodeid;
    let path = |rel: &str| t.s.path(rel).into_os_string().into_encoded_bytes();
    let ino = |rel: &str| std::fs::symlink_metadata(t.s.path(rel)).unwrap().ino();
    let change = |rel: &str, flags: u32| Change {
        path: path(rel),
        flags,
        ino: ino(rel),
    };
    let names_changed = |records: &[crate::fsevents::Event], dir: u64| {
        records.iter().any(|e| {
            (matches!(e.op, op::CREATE | op::DELETE) && e.parent == dir)
                || (e.op == op::RENAME && (e.parent == dir || e.parent2 == dir))
                || e.op == op::INVAL_ALL
        })
    };

    let d = t.c.mkdir(w, b"d", 0o755).unwrap().nodeid;
    assert!(complete(&t.fs, d));
    let (f, fh) = t.c.create(d, b"own", 0o644, 0o1).unwrap();
    t.c.release(f.nodeid, fh).unwrap();
    t.c.mkdir(d, b"owndir", 0o755).unwrap();
    t.c.link(f.nodeid, d, b"ownlink").unwrap();
    t.c.symlink(d, b"ownsym", b"own").unwrap();
    let (tmp, fh) = t.c.create(d, b"saved.tmp", 0o644, 0o1).unwrap();
    t.c.release(tmp.nodeid, fh).unwrap();
    t.c.rename(d, b"saved.tmp", d, b"saved", 0).unwrap();
    let (far, fh) = t.c.create(w, b"far", 0o644, 0o1).unwrap();
    t.c.release(far.nodeid, fh).unwrap();
    t.c.rename(w, b"far", d, b"near", 0).unwrap();
    let records = t.fs.records(&[
        change("w/d", fse::CREATED | fse::IS_DIR),
        change("w/d/own", fse::CREATED | fse::MODIFIED),
        change("w/d/saved", fse::CREATED | fse::RENAMED | fse::MODIFIED),
        change("w/d/near", fse::CREATED | fse::RENAMED),
        change("w/d/owndir", fse::CREATED | fse::IS_DIR),
        change("w/d/ownlink", fse::CREATED | fse::IS_HARDLINK),
        change("w/d/ownsym", fse::CREATED),
    ]);
    assert!(!names_changed(&records, d), "{records:?}");
    assert!(complete(&t.fs, d));

    t.s.write("w/d/mac", b"");
    let records = t.fs.records(&[change("w/d/mac", fse::CREATED)]);
    assert!(names_changed(&records, d), "{records:?}");
    assert!(!complete(&t.fs, d));

    let d2 = t.c.mkdir(w, b"d2", 0o755).unwrap().nodeid;
    let (g, fh) = t.c.create(w, b"elsewhere", 0o644, 0o1).unwrap();
    t.c.release(g.nodeid, fh).unwrap();
    t.fs.records(&[
        change("w/d2", fse::CREATED | fse::IS_DIR),
        change("w/elsewhere", fse::CREATED),
    ]);
    assert!(complete(&t.fs, d2));
    std::fs::rename(t.s.path("w/elsewhere"), t.s.path("w/d2/moved")).unwrap();
    let records = t.fs.records(&[change("w/d2/moved", fse::MODIFIED)]);
    assert!(names_changed(&records, d2), "{records:?}");
    assert!(!complete(&t.fs, d2));
    let records = t.fs.records(&[change("w/d2/moved", fse::MODIFIED)]);
    assert!(!names_changed(&records, d2), "{records:?}");

    let d3 = t.c.mkdir(w, b"d3", 0o755).unwrap().nodeid;
    let (h, fh) = t.c.create(d3, b"one", 0o644, 0o1).unwrap();
    t.c.release(h.nodeid, fh).unwrap();
    t.fs.records(&[
        change("w/d3", fse::CREATED | fse::IS_DIR),
        change("w/d3/one", fse::CREATED),
    ]);
    assert!(complete(&t.fs, d3));
    std::fs::hard_link(t.s.path("w/d3/one"), t.s.path("w/d3/two")).unwrap();
    let records =
        t.fs.records(&[change("w/d3/two", fse::META | fse::IS_HARDLINK)]);
    assert!(names_changed(&records, d3), "{records:?}");
    assert!(!complete(&t.fs, d3));

    let d4 = t.c.mkdir(w, b"d4", 0o755).unwrap().nodeid;
    t.fs.records(&[change("w/d4", fse::CREATED | fse::IS_DIR)]);
    t.s.write("w/d4/listed", b"");
    t.c.opendir(d4).unwrap();
    t.c.readdir(d4, 0, 0, 32768, true).unwrap();
    assert!(complete(&t.fs, d4));
    let records = t.fs.records(&[change("w/d4/listed", fse::META)]);
    assert!(names_changed(&records, d4), "{records:?}");
    assert!(!complete(&t.fs, d4));

    let d5 = t.c.mkdir(w, b"d5", 0o755).unwrap().nodeid;
    let (r, fh) = t.c.create(d5, b"readme", 0o644, 0o1).unwrap();
    t.c.release(r.nodeid, fh).unwrap();
    t.fs.records(&[
        change("w/d5", fse::CREATED | fse::IS_DIR),
        change("w/d5/readme", fse::CREATED),
    ]);
    assert!(complete(&t.fs, d5));
    t.s.write("w/d5/README", b"");
    let two = std::fs::read_dir(t.s.path("w/d5")).unwrap().count() == 2;
    let records =
        t.fs.records(&[change("w/d5/README", fse::CREATED | fse::MODIFIED)]);
    assert_eq!(names_changed(&records, d5), two, "{records:?}");
    assert_eq!(complete(&t.fs, d5), !two);
}

#[test]
fn a_request_is_quick_only_where_the_mac_has_let_the_share_in() {
    use crate::fs::Need::{File, Handle, Meta, Names};
    let s = Scratch::new("quick");
    s.write("old/file", b"x");
    s.write("old/sub/inner", b"x");
    s.write("other/x", b"x");
    let (fs, c) = s.shared();
    let old = c.lookup(ROOT_ID, b"old").unwrap().nodeid;
    let file = c.lookup(old, b"file").unwrap().nodeid;
    assert!(fs.quick(old, Meta) && fs.quick(file, Meta));
    assert!(!fs.quick(old, Names) && !fs.quick(old, File));
    assert!(!fs.quick(file, File) && !fs.quick(file, Handle(0)));
    assert!(!fs.quick(987_654, Meta));
    let dev = fs.nodes.lock().unwrap().by_id[&old].dev;
    let was = fs.volumes.lock().unwrap()[&dev];
    for change in [
        (|v: &mut Volume| v.boot = false) as fn(&mut Volume),
        |v: &mut Volume| v.volfs = false,
    ] {
        change(fs.volumes.lock().unwrap().get_mut(&dev).unwrap());
        assert!(!fs.quick(old, Meta) && !fs.quick(file, Meta));
        fs.volumes.lock().unwrap().insert(dev, was);
    }
    assert!(fs.quick(old, Meta));
    let fh = c.open(file, libc::O_RDWR as u32).unwrap();
    assert!(fs.quick(file, Handle(fh)));
    assert!(fs.quick(old, Names) && fs.quick(file, File));
    c.release(file, fh).unwrap();
    fs.nodes
        .lock()
        .unwrap()
        .by_id
        .get_mut(&file)
        .unwrap()
        .dataless = true;
    assert!(fs.quick(file, Meta) && fs.quick(file, Handle(7)));
    assert!(!fs.quick(file, File) && !fs.quick(file, Handle(0)));
    fs.nodes
        .lock()
        .unwrap()
        .by_id
        .get_mut(&file)
        .unwrap()
        .dataless = false;
    fs.nodes
        .lock()
        .unwrap()
        .by_id
        .get_mut(&old)
        .unwrap()
        .dataless = true;
    assert!(!fs.quick(old, Names) && !fs.quick(old, File));
    fs.nodes
        .lock()
        .unwrap()
        .by_id
        .get_mut(&old)
        .unwrap()
        .dataless = false;
    assert!(fs.quick(file, File) && fs.quick(old, Names));
    let sub = c.lookup(old, b"sub").unwrap().nodeid;
    assert!(!fs.quick(sub, Names) && !fs.quick(sub, File));
    let dh = c.opendir(sub).unwrap();
    assert!(fs.quick(sub, Names) && fs.quick(sub, File));
    c.releasedir(sub, dh).unwrap();
    let made = c.mkdir(old, b"made", 0o755).unwrap().nodeid;
    assert!(fs.quick(made, Names));
    let other = c.lookup(ROOT_ID, b"other").unwrap().nodeid;
    assert!(!fs.quick(other, Names));
    let made = c.mkdir(other, b"made", 0o755).unwrap().nodeid;
    assert!(fs.quick(other, Names) && fs.quick(made, Names));
}

#[test]
fn a_lookup_in_a_placeholder_or_beside_another_volume_is_not_quick() {
    use crate::fs::Need::{Lookup, Meta};
    let s = Scratch::new("quick-lookup");
    s.write("dir/file", b"x");
    let (fs, c) = s.shared();
    let dir = c.lookup(ROOT_ID, b"dir").unwrap().nodeid;
    assert!(fs.quick(dir, Lookup) && fs.quick(ROOT_ID, Lookup));
    fs.nodes
        .lock()
        .unwrap()
        .by_id
        .get_mut(&dir)
        .unwrap()
        .dataless = true;
    assert!(fs.quick(dir, Meta), "described without a fetch");
    assert!(!fs.quick(dir, Lookup), "a name in it is not");
    fs.nodes
        .lock()
        .unwrap()
        .by_id
        .get_mut(&dir)
        .unwrap()
        .dataless = false;
    assert!(fs.quick(dir, Lookup));

    let fs = Arc::new(Passthrough::new(&Config::new(std::path::Path::new("/"))).unwrap());
    let c = Client::new(Server::shared(fs.clone()));
    c.init();
    if !fs.quick(ROOT_ID, Meta) {
        return;
    }
    assert!(fs.quick(ROOT_ID, Lookup));
    c.lookup(ROOT_ID, b"usr").unwrap();
    assert!(fs.quick(ROOT_ID, Lookup));
    let dev = c.lookup(ROOT_ID, b"dev").unwrap().nodeid;
    assert!(!fs.quick(ROOT_ID, Lookup), "a lookup beside a mount point");
    assert!(fs.quick(ROOT_ID, Meta));
    assert!(!fs.quick(dev, Meta) && !fs.quick(dev, Lookup));
}

#[test]
fn a_directory_made_beside_protected_folders_and_a_volume_served_by_names_are_not_quick() {
    use crate::fs::Need::{Meta, Names};
    let s = Scratch::new("quick-class");
    s.write("home/x", b"x");
    let mut fs = Passthrough::new(&Config::new(&s.0)).unwrap();
    let st = lstat(s.path("home").as_os_str().as_encoded_bytes()).unwrap();
    fs.class_parents = vec![(st.st_dev, st.st_ino)];
    let fs = Arc::new(fs);
    let c = Client::new(Server::shared(fs.clone()));
    c.init();
    let home = c.lookup(ROOT_ID, b"home").unwrap().nodeid;
    let made = c.mkdir(home, b"Documents", 0o755).unwrap().nodeid;
    assert!(fs.quick(home, Names), "the Mac let the guest make it");
    assert!(!fs.quick(made, Names), "but what it made may be protected");

    let mut cfg = Config::new(&s.0);
    cfg.no_volfs = true;
    let fs = Arc::new(Passthrough::new(&cfg).unwrap());
    let c = Client::new(Server::shared(fs.clone()));
    c.init();
    let home = c.lookup(ROOT_ID, b"home").unwrap().nodeid;
    assert!(!fs.quick(home, Meta));
}

fn a_remade_place_stays_denied(dir: &Path, make: &dyn Fn(Vec<PathBuf>) -> Client) -> &'static str {
    let at = |rel: &str| dir.join(rel);
    let write = |rel: &str, data: &[u8]| {
        std::fs::create_dir_all(at(rel).parent().unwrap()).unwrap();
        std::fs::write(at(rel), data).unwrap();
    };
    write("state/data/disk.img", b"the VM's disk");
    write("state/run/sock", b"");
    write("state/mine", b"mine");
    std::fs::create_dir_all(at("d")).unwrap();
    let c = make(vec![at("state/data"), at("state/run")]);
    let state = walk(&c, "state").unwrap().nodeid;
    let d = walk(&c, "d").unwrap().nodeid;
    assert_eq!(walk(&c, "state/data").err(), Some(Errno::ENOENT));
    let folds = !case_sensitive(dir);

    std::fs::remove_dir_all(at("state/data")).unwrap();
    write("state/data/new", b"made again");
    assert_eq!(
        walk(&c, "state/data").err(),
        Some(Errno::ENOENT),
        "a remade place is hidden"
    );
    assert_eq!(walk(&c, "state/data/new").err(), Some(Errno::ENOENT));
    if folds {
        assert_eq!(
            walk(&c, "state/DATA").err(),
            Some(Errno::ENOENT),
            "in another case too"
        );
    }
    for plus in [false, true] {
        assert_eq!(
            names(&c.list(state, plus).unwrap()),
            ["mine"],
            "listed (plus {plus})"
        );
    }
    assert_eq!(c.rmdir(state, b"data").err(), Some(Errno::ENOENT));
    assert_eq!(
        c.rename(state, b"data", d, b"taken", 0).err(),
        Some(Errno::ENOENT)
    );
    assert_eq!(c.mkdir(state, b"data", 0o755).err(), Some(Errno::EEXIST));
    assert!(c.create(state, b"data", 0o644, O_WRONLY).is_err());
    assert_eq!(std::fs::read(at("state/data/new")).unwrap(), b"made again");

    std::fs::remove_dir_all(at("state/data")).unwrap();
    std::fs::create_dir(at("d/sub")).unwrap();
    let mut spellings: Vec<&[u8]> = vec![b"data"];
    if folds {
        spellings.push(b"DATA");
        spellings.push(b"Data");
    }
    for name in spellings {
        let n = String::from_utf8_lossy(name);
        assert_eq!(
            c.mkdir(state, name, 0o755).err(),
            Some(Errno::EACCES),
            "mkdir {n}"
        );
        assert_eq!(
            c.create(state, name, 0o644, O_WRONLY).err(),
            Some(Errno::EACCES),
            "create {n}"
        );
        assert_eq!(
            c.symlink(state, name, b"/").err(),
            Some(Errno::EACCES),
            "symlink {n}"
        );
        assert_eq!(
            c.rename(d, b"sub", state, name, 0).err(),
            Some(Errno::EACCES),
            "rename onto {n}"
        );
        assert_eq!(
            c.rename(d, b"sub", state, name, 1).err(),
            Some(Errno::EACCES),
            "rename to new {n}"
        );
    }
    assert!(!at("state/data").exists() && at("d/sub").is_dir());
    assert!(c.mkdir(state, b"data2", 0o755).is_ok());
    assert!(c.mkdir(state, b"dat", 0o755).is_ok());

    write("state/data/again", b"");
    assert_eq!(walk(&c, "state/data").err(), Some(Errno::ENOENT));
    std::fs::create_dir(at("d/other")).unwrap();
    if walk(&c, "d/other").is_ok() {
        "shown"
    } else {
        "hidden"
    }
}

#[test]
fn a_denied_directory_removed_and_made_again_stays_denied() {
    for (arm, no_volfs) in [("volfs", false), ("tree", true)] {
        let s = Scratch::new(&format!("remade-{arm}"));
        let other = a_remade_place_stays_denied(&s.0, &|deny| {
            s.client_with(|cfg| {
                cfg.deny = deny;
                cfg.no_volfs = no_volfs;
            })
        });
        assert_eq!(other, "shown", "{arm}");
        let mut cfg = Config::new(&s.0);
        cfg.deny = vec![s.path("state/data")];
        cfg.no_volfs = no_volfs;
        let fs = Passthrough::new(&cfg).unwrap();
        std::fs::remove_dir_all(s.path("state/data")).unwrap();
        s.write("state/data/w/x", b"");
        for under in ["state/data", "state/data/w"] {
            let path = s.path(under);
            let (status, _) = fs.watch_status(path.as_os_str().as_bytes());
            assert_eq!(status, WatchStatus::Outside, "{arm}: watch at {under}");
        }
    }
    if let Some(dir) = std::env::var_os("FEV_DENY_DIR") {
        let dir = std::fs::canonicalize(dir).unwrap();
        let other = a_remade_place_stays_denied(&dir, &|deny| {
            let mut cfg = Config::new(&dir);
            cfg.deny = deny;
            let c = Client::new(Server::new(Box::new(Passthrough::new(&cfg).unwrap())));
            c.init();
            c
        });
        println!(
            "FS-DENY {}: remade place hidden; a directory made elsewhere afterwards {other}",
            dir.display()
        );
    }
}
