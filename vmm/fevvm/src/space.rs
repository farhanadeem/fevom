use std::os::fd::AsRawFd;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;
use std::time::Duration;

use crate::vmmctl;

pub trait Volume: Send + Sync {
    fn read(&self) -> Option<(u64, u64)>;
}

pub struct ImageVolume(pub std::fs::File);

impl Volume for ImageVolume {
    fn read(&self) -> Option<(u64, u64)> {
        let fd = self.0.as_raw_fd();
        let mut sfs: libc::statfs = unsafe { std::mem::zeroed() };
        let mut st: libc::stat = unsafe { std::mem::zeroed() };
        if unsafe { libc::fstatfs(fd, &mut sfs) } != 0 || unsafe { libc::fstat(fd, &mut st) } != 0 {
            return None;
        }
        Some((
            (sfs.f_bavail as u64).saturating_mul(sfs.f_bsize as u64),
            (st.st_blocks.max(0) as u64).saturating_mul(512),
        ))
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Params {
    pub every: u64,
    pub threshold: u64,
    pub hold: bool,
    pub backoff_first: Duration,
    pub backoff_max: Duration,
}

pub const EVERY_ENV: &str = "FEV_VMM_SPACE_EVERY_MIB";
pub const THRESHOLD_ENV: &str = "FEV_VMM_SPACE_THRESHOLD_MIB";
pub const HOLD_ENV: &str = "FEV_VMM_HOLD";
pub const BACKOFF_ENV: &str = "FEV_VMM_HOLD_BACKOFF_MS";

impl Default for Params {
    fn default() -> Self {
        Params {
            every: 64 << 20,
            threshold: 256 << 20,
            hold: true,
            backoff_first: Duration::from_millis(100),
            backoff_max: Duration::from_secs(2),
        }
    }
}

impl Params {
    pub fn from_env() -> Self {
        let num = |name: &str| std::env::var(name).ok().and_then(|v| v.parse::<u64>().ok());
        let mut p = Params::default();
        if let Some(mib) = num(EVERY_ENV) {
            p.every = mib << 20;
        }
        if let Some(mib) = num(THRESHOLD_ENV) {
            p.threshold = mib << 20;
        }
        if std::env::var_os(HOLD_ENV).is_some_and(|v| v == "off") {
            p.hold = false;
        }
        if let Some(ms) = num(BACKOFF_ENV).filter(|ms| *ms != 0) {
            p.backoff_max = Duration::from_millis(ms);
            p.backoff_first = p.backoff_first.min(p.backoff_max);
        }
        p
    }
}

struct State {
    base: (u64, u64),
    told: bool,
    margin: u64,
}

type Report = Box<dyn Fn(vmmctl::Space) + Send + Sync>;
type Waker = Box<dyn Fn() + Send + Sync>;

pub struct Space {
    pub params: Params,
    volume: Box<dyn Volume>,
    state: Mutex<State>,
    report: Mutex<Option<Report>>,
    wakers: Mutex<Vec<Waker>>,
    checks: AtomicU64,
    reports: AtomicU64,
    under: AtomicU64,
    shrunk: AtomicU64,
    discarded: AtomicU64,
    check_ns: AtomicU64,
    held: AtomicU64,
    held_ever: AtomicU64,
    retries: AtomicU64,
    since: Mutex<Option<std::time::Instant>>,
    #[cfg(test)]
    refuse: std::sync::atomic::AtomicBool,
}

impl Space {
    pub fn new(params: Params, volume: Box<dyn Volume>) -> Space {
        let base = volume.read().unwrap_or((0, 0));
        Space {
            params,
            volume,
            state: Mutex::new(State {
                base,
                told: false,
                margin: 0,
            }),
            report: Mutex::new(None),
            wakers: Mutex::new(Vec::new()),
            checks: AtomicU64::new(0),
            reports: AtomicU64::new(0),
            under: AtomicU64::new(0),
            shrunk: AtomicU64::new(0),
            discarded: AtomicU64::new(0),
            check_ns: AtomicU64::new(0),
            held: AtomicU64::new(0),
            held_ever: AtomicU64::new(0),
            retries: AtomicU64::new(0),
            since: Mutex::new(None),
            #[cfg(test)]
            refuse: Default::default(),
        }
    }

    #[cfg(test)]
    pub fn refusing(&self) -> bool {
        self.refuse.load(Ordering::Relaxed)
    }

    #[cfg(test)]
    pub fn refuse(&self, on: bool) {
        self.refuse.store(on, Ordering::Relaxed);
    }

    pub fn off() -> Space {
        struct Nothing;
        impl Volume for Nothing {
            fn read(&self) -> Option<(u64, u64)> {
                None
            }
        }
        Space::new(
            Params {
                every: 0,
                hold: false,
                ..Params::default()
            },
            Box::new(Nothing),
        )
    }

    pub fn on_report(&self, report: impl Fn(vmmctl::Space) + Send + Sync + 'static) {
        *self.report.lock().unwrap() = Some(Box::new(report));
    }

    #[inline]
    pub fn crossed(&self, before: u64, len: u64) -> bool {
        let every = self.params.every;
        every != 0 && (before + len) / every != before / every
    }

    fn reading(&self, available: u64, image: u64) -> vmmctl::Space {
        vmmctl::Space {
            available_bytes: available,
            image_bytes: image,
            held_requests: self.held(),
            at_ns: crate::trace::now_ns(),
        }
    }

    fn send(&self, space: vmmctl::Space) {
        if let Some(report) = self.report.lock().unwrap().as_ref() {
            report(space);
        }
    }

    pub fn check(&self) -> Option<i64> {
        let start = std::time::Instant::now();
        let (available, image) = self.volume.read()?;
        self.check_ns
            .fetch_add(start.elapsed().as_nanos() as u64, Ordering::Relaxed);
        self.checks.fetch_add(1, Ordering::Relaxed);
        let ((base_available, base_image), margin) = {
            let st = self.state.lock().unwrap();
            (st.base, st.margin)
        };
        let moved =
            (base_available as i128 - available as i128) - (image as i128 - base_image as i128);
        let moved = moved.clamp(i64::MIN as i128, i64::MAX as i128) as i64;
        let under = available < margin && image > base_image;
        let shrunk = base_image.saturating_sub(image) > self.params.threshold;
        if moved.unsigned_abs() > self.params.threshold || under || shrunk {
            self.reports.fetch_add(1, Ordering::Relaxed);
            if under {
                self.under.fetch_add(1, Ordering::Relaxed);
            }
            if shrunk {
                self.shrunk.fetch_add(1, Ordering::Relaxed);
            }
            self.send(self.reading(available, image));
        }
        Some(moved)
    }

    pub fn discarded(&self, len: u64) {
        let before = self.discarded.fetch_add(len, Ordering::Relaxed);
        if self.crossed(before, len) {
            self.check();
        }
    }

    pub fn baseline(&self, available: u64, image: u64, margin: u64) -> u64 {
        {
            let mut st = self.state.lock().unwrap();
            st.base = (available, image);
            st.told = true;
            st.margin = margin;
        }
        let held = self.held();
        self.room();
        held
    }

    pub fn base(&self) -> (u64, u64, bool) {
        let st = self.state.lock().unwrap();
        (st.base.0, st.base.1, st.told)
    }

    pub fn held(&self) -> u64 {
        self.held.load(Ordering::Relaxed)
    }

    pub fn on_room(&self, wake: impl Fn() + Send + Sync + 'static) {
        self.wakers.lock().unwrap().push(Box::new(wake));
    }

    pub fn forget_wakers(&self) {
        self.wakers.lock().unwrap().clear();
    }

    pub fn room(&self) {
        if self.held() == 0 {
            return;
        }
        for wake in self.wakers.lock().unwrap().iter() {
            wake();
        }
    }

    pub fn keep(&self) {
        self.held_ever.fetch_add(1, Ordering::Relaxed);
        if self.held.fetch_add(1, Ordering::Relaxed) == 0 {
            *self.since.lock().unwrap() = Some(std::time::Instant::now());
            let (available, image) = self.volume.read().unwrap_or((0, 0));
            crate::vmm_log!(
                "space: the Mac has no room for a write to the data image ({} MiB free on its volume); held, not failed",
                available >> 20
            );
            self.send(self.reading(available, image));
        }
    }

    pub fn retried(&self) {
        self.retries.fetch_add(1, Ordering::Relaxed);
    }

    pub fn let_go(&self, n: u64, answered: bool) {
        if n == 0 {
            return;
        }
        if self.held.fetch_sub(n, Ordering::Relaxed) == n {
            let after = self
                .since
                .lock()
                .unwrap()
                .take()
                .map_or(0, |t| t.elapsed().as_millis());
            let (available, image) = self.volume.read().unwrap_or((0, 0));
            if answered {
                crate::vmm_log!(
                    "space: every held write is done after {after} ms ({} MiB free)",
                    available >> 20
                );
            } else {
                crate::vmm_log!(
                    "space: {n} held request(s) dropped unanswered at a reset after {after} ms"
                );
            }
            self.send(self.reading(available, image));
        }
    }

    pub fn counters(&self) -> [(&'static str, u64); 5] {
        [
            ("space_checks", self.checks.load(Ordering::Relaxed)),
            ("space_reports", self.reports.load(Ordering::Relaxed)),
            ("space_check_ns", self.check_ns.load(Ordering::Relaxed)),
            ("held", self.held_ever.load(Ordering::Relaxed)),
            ("held_retries", self.retries.load(Ordering::Relaxed)),
        ]
    }

    pub fn summary(&self) -> String {
        let c = self.counters();
        let (available, image, told) = self.base();
        format!(
            "space: {} checks ({} ns each), {} reports; {} held in all, {} now, {} retries; measuring from {} MiB free and {} MiB of image ({}); {} of the reports for a volume under the margin, {} for an image that shrank",
            c[0].1,
            c[2].1.checked_div(c[0].1).unwrap_or(0),
            c[1].1,
            c[3].1,
            self.held(),
            c[4].1,
            available >> 20,
            image >> 20,
            if told { "the daemon's" } else { "read at open" },
            self.under.load(Ordering::Relaxed),
            self.shrunk.load(Ordering::Relaxed)
        )
    }
}

pub fn no_space(errno: i32) -> bool {
    errno == libc::ENOSPC || errno == libc::EDQUOT
}

#[cfg(test)]
pub mod tests {
    use super::*;
    use std::sync::Arc;

    #[derive(Default)]
    pub struct FakeVolume {
        pub figures: Mutex<Option<(u64, u64)>>,
        pub reads: AtomicU64,
    }

    impl FakeVolume {
        pub fn set(&self, available: u64, image: u64) {
            *self.figures.lock().unwrap() = Some((available, image));
        }
    }

    impl Volume for Arc<FakeVolume> {
        fn read(&self) -> Option<(u64, u64)> {
            self.reads.fetch_add(1, Ordering::Relaxed);
            *self.figures.lock().unwrap()
        }
    }

    const MIB: u64 = 1 << 20;

    pub fn fake(
        params: Params,
        available: u64,
        image: u64,
    ) -> (Arc<Space>, Arc<FakeVolume>, Arc<Mutex<Vec<vmmctl::Space>>>) {
        let volume = Arc::new(FakeVolume::default());
        volume.set(available, image);
        let space = Arc::new(Space::new(params, Box::new(volume.clone())));
        let seen = Arc::new(Mutex::new(Vec::new()));
        let sink = seen.clone();
        space.on_report(move |s| sink.lock().unwrap().push(s));
        (space, volume, seen)
    }

    #[test]
    fn one_writer_takes_each_crossing_and_none_between() {
        let (space, volume, _) = fake(Params::default(), 10_000 * MIB, 100 * MIB);
        assert_eq!(volume.reads.load(Ordering::Relaxed), 1);
        let every = space.params.every;
        let mut total = 0u64;
        let mut crossings = 0;
        while total < 200 * MIB {
            if space.crossed(total, 4096) {
                crossings += 1;
                assert_eq!((total + 4096) % every, 0);
            }
            total += 4096;
        }
        assert_eq!(crossings, 3);
        assert!(space.crossed(0, 3 * every));
        let (off, _, _) = fake(
            Params {
                every: 0,
                ..Params::default()
            },
            MIB,
            MIB,
        );
        assert!(!off.crossed(0, 1 << 40));
        assert!(!Space::off().crossed(0, 1 << 40));
    }

    #[test]
    fn a_volume_under_the_margin_is_reported_when_the_image_grew_to_get_there() {
        let (space, volume, seen) = fake(Params::default(), 10_000 * MIB, 100 * MIB);
        let margin = 5120 * MIB;
        volume.set(1024 * MIB, 100 * MIB + 8976 * MIB);
        assert_eq!(space.check(), Some(0));
        assert!(seen.lock().unwrap().is_empty());
        volume.set(10_000 * MIB, 100 * MIB);
        space.baseline(10_000 * MIB, 100 * MIB, margin);
        volume.set(10_000 * MIB - 4096 * MIB, 100 * MIB + 4096 * MIB);
        assert_eq!(space.check(), Some(0));
        assert!(seen.lock().unwrap().is_empty());
        volume.set(margin - MIB, 100 * MIB + 10_000 * MIB - margin + MIB);
        assert_eq!(space.check(), Some(0));
        assert_eq!(seen.lock().unwrap().len(), 1);
        assert_eq!(seen.lock().unwrap()[0].available_bytes, margin - MIB);
        let image = 100 * MIB + 10_000 * MIB - margin + MIB;
        space.baseline(margin - MIB, image, margin);
        for _ in 0..5 {
            assert_eq!(space.check(), Some(0));
        }
        assert_eq!(seen.lock().unwrap().len(), 1);
        volume.set(margin - 65 * MIB, image + 64 * MIB);
        assert_eq!(space.check(), Some(0));
        assert_eq!(seen.lock().unwrap().len(), 2);
        space.baseline(margin - 65 * MIB, image + 64 * MIB, margin);
        volume.set(margin - 65 * MIB - 100 * MIB, image + 64 * MIB);
        assert_eq!(space.check(), Some((100 * MIB) as i64));
        assert_eq!(seen.lock().unwrap().len(), 2);
        assert!(space
            .summary()
            .contains("2 of the reports for a volume under the margin"));
    }

    #[test]
    fn an_image_that_shrank_is_reported_as_its_discards_land() {
        let params = Params::default();
        let (space, volume, seen) = fake(params, 255 * MIB, 1871 * MIB);
        space.baseline(255 * MIB, 1871 * MIB, 256 * MIB);
        let reads = volume.reads.load(Ordering::Relaxed);
        for _ in 0..63 {
            space.discarded(MIB);
        }
        assert_eq!(volume.reads.load(Ordering::Relaxed), reads);
        volume.set(255 * MIB + 64 * MIB, 1871 * MIB - 64 * MIB);
        space.discarded(MIB);
        assert_eq!(volume.reads.load(Ordering::Relaxed), reads + 1);
        assert!(seen.lock().unwrap().is_empty());
        volume.set(255 * MIB + 257 * MIB, 1871 * MIB - 257 * MIB);
        assert_eq!(space.check(), Some(0));
        assert_eq!(seen.lock().unwrap().len(), 1);
        assert_eq!(seen.lock().unwrap()[0].available_bytes, 512 * MIB);
        space.baseline(512 * MIB, 1614 * MIB, 256 * MIB);
        volume.set(700 * MIB, 1426 * MIB);
        assert_eq!(space.check(), Some(0));
        assert_eq!(seen.lock().unwrap().len(), 1);
        volume.set(769 * MIB, 1357 * MIB);
        assert_eq!(space.check(), Some(0));
        assert_eq!(seen.lock().unwrap().len(), 2);
        assert!(space.summary().contains("2 for an image that shrank"));
        space.baseline(769 * MIB, 1357 * MIB, 256 * MIB);
        volume.set(469 * MIB, 1657 * MIB);
        assert_eq!(space.check(), Some(0));
        assert_eq!(seen.lock().unwrap().len(), 2);
    }

    #[test]
    fn only_what_the_image_does_not_explain_is_reported() {
        let (space, volume, seen) = fake(Params::default(), 10_000 * MIB, 100 * MIB);
        volume.set(10_000 * MIB - 3072 * MIB, 100 * MIB + 3072 * MIB);
        assert_eq!(space.check(), Some(0));
        volume.set(10_000 * MIB - 1024 * MIB, 100 * MIB + 1024 * MIB);
        assert_eq!(space.check(), Some(0));
        volume.set(
            10_000 * MIB - 1024 * MIB - 256 * MIB,
            100 * MIB + 1024 * MIB,
        );
        assert_eq!(space.check(), Some((256 * MIB) as i64));
        assert!(seen.lock().unwrap().is_empty());
        volume.set(
            10_000 * MIB - 1024 * MIB - 256 * MIB - 1,
            100 * MIB + 1024 * MIB,
        );
        assert_eq!(space.check(), Some((256 * MIB) as i64 + 1));
        {
            let seen = seen.lock().unwrap();
            assert_eq!(seen.len(), 1);
            assert_eq!(
                seen[0].available_bytes,
                10_000 * MIB - 1024 * MIB - 256 * MIB - 1
            );
            assert_eq!(seen[0].image_bytes, 100 * MIB + 1024 * MIB);
            assert_eq!(seen[0].held_requests, 0);
        }
        space.check();
        assert_eq!(seen.lock().unwrap().len(), 2);
        assert_eq!(
            space.baseline(
                10_000 * MIB - 1024 * MIB - 256 * MIB - 1,
                100 * MIB + 1024 * MIB,
                0
            ),
            0
        );
        assert_eq!(space.check(), Some(0));
        assert_eq!(seen.lock().unwrap().len(), 2);
        volume.set(10_000 * MIB, 100 * MIB + 1024 * MIB);
        assert!(space.check().unwrap() < -((256 * MIB) as i64));
        assert_eq!(seen.lock().unwrap().len(), 3);
        assert_eq!(space.counters()[0], ("space_checks", 7));
        assert_eq!(space.counters()[1], ("space_reports", 3));
    }

    #[test]
    fn the_baseline_is_the_last_budget_whoever_caused_it() {
        let (space, volume, seen) = fake(Params::default(), 8192 * MIB, 0);
        volume.set(7168 * MIB, 0);
        space.check();
        assert_eq!(seen.lock().unwrap().len(), 1);
        space.baseline(7168 * MIB, 0, 0);
        volume.set(8192 * MIB, 0);
        space.baseline(8192 * MIB, 0, 0);
        volume.set(7168 * MIB, 0);
        assert_eq!(space.check(), Some((1024 * MIB) as i64));
        assert_eq!(seen.lock().unwrap().len(), 2);
        assert_eq!(space.base(), (8192 * MIB, 0, true));
    }

    #[test]
    fn a_volume_that_cannot_be_read_is_no_report() {
        let (space, volume, seen) = fake(Params::default(), 8192 * MIB, 0);
        *volume.figures.lock().unwrap() = None;
        assert_eq!(space.check(), None);
        assert!(seen.lock().unwrap().is_empty());
    }

    #[test]
    fn the_hold_account_tells_the_daemon_at_its_two_ends() {
        let (space, volume, seen) = fake(Params::default(), 32 * MIB, 4096 * MIB);
        let woken = Arc::new(AtomicU64::new(0));
        let w = woken.clone();
        space.on_room(move || {
            w.fetch_add(1, Ordering::Relaxed);
        });
        assert_eq!(space.baseline(32 * MIB, 4096 * MIB, 0), 0);
        space.room();
        assert_eq!(
            woken.load(Ordering::Relaxed),
            0,
            "nothing held, nobody woken"
        );
        space.keep();
        space.keep();
        space.keep();
        assert_eq!(space.held(), 3);
        {
            let seen = seen.lock().unwrap();
            assert_eq!(seen.len(), 1);
            assert_eq!(
                (seen[0].held_requests, seen[0].available_bytes),
                (1, 32 * MIB)
            );
        }
        assert_eq!(space.baseline(32 * MIB, 4096 * MIB, 0), 3);
        assert_eq!(woken.load(Ordering::Relaxed), 1);
        volume.set(900 * MIB, 4096 * MIB);
        space.let_go(2, true);
        assert_eq!(seen.lock().unwrap().len(), 1);
        space.let_go(1, true);
        {
            let seen = seen.lock().unwrap();
            assert_eq!(seen.len(), 2);
            assert_eq!(
                (seen[1].held_requests, seen[1].available_bytes),
                (0, 900 * MIB)
            );
        }
        space.forget_wakers();
        space.keep();
        space.room();
        assert_eq!(woken.load(Ordering::Relaxed), 1);
        space.let_go(1, false);
        assert_eq!(seen.lock().unwrap().len(), 4);
        assert_eq!(space.counters()[3], ("held", 4));
    }

    #[test]
    fn the_image_volume_reads_a_real_file() {
        let path = std::env::temp_dir().join(format!("fev-space-{}.img", std::process::id()));
        let file = std::fs::File::create(&path).unwrap();
        file.set_len(1 << 30).unwrap();
        use std::os::unix::fs::FileExt;
        file.write_all_at(&[7u8; 1 << 16], 0).unwrap();
        file.sync_all().unwrap();
        let (available, image) = ImageVolume(file).read().unwrap();
        assert!(available > 0);
        assert!((1 << 16..1 << 24).contains(&image), "{image}");
        std::fs::remove_file(path).unwrap();
    }

    #[test]
    fn the_defaults_are_the_measured_ones() {
        let p = Params::default();
        assert_eq!((p.every, p.threshold, p.hold), (64 * MIB, 256 * MIB, true));
        assert_eq!(
            (p.backoff_first, p.backoff_max),
            (Duration::from_millis(100), Duration::from_secs(2))
        );
    }
}
