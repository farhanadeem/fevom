use std::sync::{Arc, OnceLock};

use crate::bus::MmioDevice;
use crate::console::ConsoleOut;
use crate::exit::{MmioAccess, SysRegAccess};
use crate::irq::{IrqChip, IrqLine};
use crate::memory::GuestMemory;
use crate::psci::Psci;
use crate::virtio::balloon::{releasable, Balloon, Releaser};
use crate::virtio::blk::{self, Block, Disk};
use crate::virtio::console::Console;
use crate::virtio::fs;
use crate::virtio::net::Net;
use crate::virtio::queue::{Queue, MAX_INDIRECT};
use crate::virtio::rng::Rng;
use crate::virtio::vsock::Vsock;
use crate::virtio::{Activation, Interrupt, MmioTransport, VirtioDevice};

pub const BASE: u64 = 0x4000_0000;
pub const MEM_LEN: u64 = 256 << 10;
pub const HEADER: usize = 16;
pub const DESC_OFF: u64 = 0x1000;
pub const AVAIL_OFF: u64 = 0x1800;
pub const USED_OFF: u64 = 0x1c00;
pub const QUEUE_STRIDE: u64 = 0x1000;

struct NullChip;

impl IrqChip for NullChip {
    fn set_spi(&self, _intid: u32, _level: bool) {}
}

fn interrupt() -> Arc<Interrupt> {
    Arc::new(Interrupt::new(IrqLine::new(Arc::new(NullChip), 48)))
}

fn guest(data: &[u8]) -> (Arc<GuestMemory>, Vec<Queue>) {
    let mut header = [0u8; HEADER];
    let n = data.len().min(HEADER);
    header[..n].copy_from_slice(&data[..n]);
    let image = &data[n..];

    let mem = GuestMemory::new(BASE, MEM_LEN).expect("guest memory");
    let len = image.len().min(MEM_LEN as usize);
    mem.write(BASE, &image[..len]).unwrap();

    let queues = (0..2u64)
        .map(|i| {
            let h = &header[i as usize * 8..][..8];
            let size_log = (3 ^ (h[1] & 0xf)) as u32;
            let mut q = Queue::new(1 << size_log.min(15));
            if h[0] & 0x10 != 0 {
                q.size = u16::from_le_bytes([h[1], h[7]]);
            }
            q.ready = h[0] & 1 == 0;
            q.event_idx = h[0] & 2 != 0;
            q.indirect = h[0] & 4 != 0;
            let place = |default: u64, lo: u8, hi: u8| {
                let off = default + i * QUEUE_STRIDE;
                let delta = u16::from_le_bytes([lo, hi]) as u64;
                if h[0] & 8 != 0 {
                    u64::MAX - delta
                } else {
                    BASE + (off ^ (delta << 2))
                }
            };
            q.desc_table = place(DESC_OFF, h[2], h[3]);
            q.avail_ring = place(AVAIL_OFF, h[4], h[5]);
            q.used_ring = place(USED_OFF, h[6], 0);
            q
        })
        .collect();
    (Arc::new(mem), queues)
}

pub fn fuzz_queue(data: &[u8]) {
    let (mem, mut queues) = guest(data);
    let q = &mut queues[0];
    for _ in 0..(q.size as u32 * 2 + 4) {
        match q.pop(&mem) {
            Ok(Some(chain)) => {
                let limit = (q.size as usize).max(MAX_INDIRECT as usize);
                assert!(chain.readable.len() + chain.writable.len() <= limit);
                assert!(!chain.readable.is_empty() || !chain.writable.is_empty());
                let mut scratch = [0u8; 64];
                let _ = chain.read_exact(&mem, &mut scratch);
                let _ = chain.write(&mem, &scratch);
                if q.push_used(&mem, chain.head, chain.writable_len() as u32)
                    .is_err()
                {
                    break;
                }
                let _ = q.needs_interrupt(&mem);
            }
            Ok(None) => {
                if !matches!(q.enable_notifications(&mem), Ok(true)) {
                    break;
                }
            }
            Err(_) => break,
        }
    }
}

fn disk() -> &'static Disk {
    static DISK: OnceLock<Disk> = OnceLock::new();
    DISK.get_or_init(|| {
        let path = std::env::temp_dir().join(format!("fev-fuzz-{}.img.raw", std::process::id()));
        std::fs::File::create(&path)
            .and_then(|f| f.set_len(1 << 20))
            .expect("create fuzz disk");
        let disk = Disk::open(&path, false).expect("open fuzz disk");
        let _ = std::fs::remove_file(&path);
        disk
    })
}

pub fn fuzz_blk(data: &[u8]) {
    let (mem, mut queues) = guest(data);
    let q = &mut queues[0];
    for _ in 0..8 {
        let Ok(Some(chain)) = q.pop(&mem) else { break };
        let written = blk::handle(disk(), &mem, &chain);
        assert!(written as u64 <= chain.writable_len());
        if q.push_used(&mem, chain.head, written).is_err() {
            break;
        }
    }
}

pub fn fuzz_fs(data: &[u8]) {
    static SERVER: OnceLock<fileshare::Server> = OnceLock::new();
    let server = SERVER.get_or_init(fileshare::fuzzing::fake_server);
    let (mem, mut queues) = guest(data);
    for q in &mut queues {
        for _ in 0..8 {
            let Ok(Some(chain)) = q.pop(&mem) else { break };
            let (written, _, _) = fs::handle(server, &mem, &chain);
            assert!(written as u64 <= chain.writable_len());
            if q.push_used(&mem, chain.head, written).is_err() {
                break;
            }
        }
    }
    if server.negotiated().is_none() {
        let mut init = [0u8; 40 + 64];
        init[0] = 104;
        init[4] = 26;
        init[40] = 7;
        init[44] = 45;
        init[52..60].copy_from_slice(&[0xff; 8]);
        let reply = fileshare::iov::OwnedIov::new(80, 80);
        server.handle(&fileshare::Request {
            head: &init,
            payload: fileshare::iov::IoVecs::empty(),
            reply: reply.iov(),
        });
    }
}

fn activate(
    dev: &mut dyn VirtioDevice,
    mem: &Arc<GuestMemory>,
    queues: Vec<Queue>,
) -> Arc<Interrupt> {
    let irq = interrupt();
    dev.activate(Activation {
        mem: mem.clone(),
        irq: irq.clone(),
        queues,
        features: 0,
    });
    irq
}

pub fn fuzz_console(data: &[u8]) {
    let (mem, queues) = guest(data);
    let out = Arc::new(ConsoleOut::new(Box::new(std::io::sink()), None));
    let (mut dev, input) = Console::new(out);
    input.push(&data[data.len().min(HEADER)..data.len().min(HEADER + 64)]);
    activate(&mut dev, &mem, queues);
    dev.notify(1);
    dev.notify(0);
    input.push(b"more input\n");
    dev.notify(0);
    dev.reset();
}

pub fn fuzz_rng(data: &[u8]) {
    let (mem, mut queues) = guest(data);
    queues.truncate(1);
    let mut dev = Rng::new();
    activate(&mut dev, &mem, queues);
    dev.notify(0);
    dev.reset();
}

pub fn fuzz_balloon(data: &[u8]) {
    struct Check;
    impl Releaser for Check {
        fn release(&self, addr: u64, len: u64) -> u64 {
            const PAGE: u64 = 16384;
            match releasable(addr, len, BASE, MEM_LEN, PAGE) {
                Some((start, size)) => {
                    assert!(start >= BASE && start + size <= BASE + MEM_LEN);
                    assert!(start % PAGE == 0 && size % PAGE == 0 && size > 0);
                    assert!(start >= addr && start + size <= addr + len);
                    size
                }
                None => 0,
            }
        }
    }
    let (mem, mut queues) = guest(data);
    let reporting = queues.remove(0);
    let other = queues.remove(0);
    let (mut dev, _stats) = Balloon::new(Arc::new(Check));
    let queues = vec![
        other,
        Queue::new(16),
        Queue::new(16),
        Queue::new(16),
        reporting,
    ];
    activate(&mut dev, &mem, queues);
    if data.first().is_some_and(|b| b & 0x80 != 0) {
        dev.write_config(12, &[data[0], 0, 0, 0]);
    }
    dev.notify(4);
    dev.notify(0);
    dev.reset();
}

pub fn fuzz_net(data: &[u8]) {
    use crate::virtio::netlink::Peer;
    let Ok((ours, theirs)) = std::os::unix::net::UnixStream::pair() else {
        return;
    };
    let (mem, queues) = guest(data);
    let Ok(mut dev) = Net::new(ours) else { return };
    let Ok(mut peer) = Peer::accept(theirs) else {
        return;
    };
    let body = &data[data.len().min(HEADER)..];
    let mut rest = &body[..body.len().min(4096)];
    while let [a, b, tail @ ..] = rest {
        let len = (u16::from_le_bytes([*a, *b]) as usize % 3000).min(tail.len());
        let _ = peer.to_guest.put(&tail[..len]);
        rest = &tail[len..];
    }
    activate(&mut dev, &mem, queues);
    dev.notify(1);
    dev.notify(0);
    dev.deliver_now();
    while peer.try_recv().is_some() {}
    for (i, chunk) in body.chunks(8).take(8).enumerate() {
        peer.scribble((i / 4) * 1024 + (i % 4) * 128, chunk);
    }
    dev.notify(1);
    dev.deliver_now();
    dev.reset();
}

pub fn fuzz_vsock(data: &[u8]) {
    use std::io::Write;
    static DIR: OnceLock<std::path::PathBuf> = OnceLock::new();
    let dir = DIR.get_or_init(|| {
        let dir = std::path::PathBuf::from(format!("/tmp/bvfz-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("fuzz vsock dir");
        let listener =
            std::os::unix::net::UnixListener::bind(dir.join("host_0.sock")).expect("bind");
        std::mem::forget(listener);
        extern "C" fn cleanup() {
            if let Some(dir) = DIR.get() {
                let _ = std::fs::remove_dir_all(dir);
            }
        }
        unsafe { libc::atexit(cleanup) };
        dir
    });
    let (mem, mut queues) = guest(data);
    queues.push(Queue::new(16));
    let Ok(mut dev) = Vsock::new(dir, &[1]) else {
        return;
    };
    let host =
        std::os::unix::net::UnixStream::connect(crate::virtio::vsock::guest_port_path(dir, 1));
    if let Ok(mut h) = host.as_ref() {
        let _ = h.write_all(b"from the host");
    }
    activate(&mut dev, &mem, queues);
    dev.notify(1);
    dev.notify(0);
    dev.notify(1);
    dev.reset();
}

pub fn fuzz_transport(data: &[u8]) {
    let (mem, _) = guest(data);
    let shared = Arc::new(Disk::open_shared(disk()).expect("share fuzz disk"));
    let window = shared.len();
    let transport = MmioTransport::new(
        Box::new(Block::new(shared, 2).with_window(crate::layout::DISK_WINDOW_BASE, window)),
        mem,
        IrqLine::new(Arc::new(NullChip), 48),
    );
    for op in data[data.len().min(HEADER)..]
        .as_chunks::<5>()
        .0
        .iter()
        .take(256)
    {
        let offset = (op[0] as u64 & 0x7f) * 4;
        let size = [1u8, 2, 4, 8][(op[0] >> 7) as usize + (op[1] as usize & 2)];
        let value = u32::from_le_bytes([op[1], op[2], op[3], op[4]]) as u64;
        if op[1] & 1 == 0 {
            transport.write(offset, size, value);
        } else {
            let _ = transport.read(offset, size);
        }
    }
    transport.write(0x070, 4, 0);
}

pub fn fuzz_control(data: &[u8]) {
    crate::control::fuzz(data);
}

pub fn fuzz_exit(data: &[u8]) {
    let mut words = data
        .as_chunks::<8>()
        .0
        .iter()
        .map(|c| u64::from_le_bytes(*c));
    let mut psci = Psci::new(vec![0, 1, 2, 0x100]);
    while let (Some(a), Some(b), Some(c), Some(d)) =
        (words.next(), words.next(), words.next(), words.next())
    {
        if let Some(access) = MmioAccess::decode(a) {
            assert!(matches!(access.size, 1 | 2 | 4 | 8));
            assert!(access.reg <= 31);
            let _ = access.load_value(b);
            let _ = access.store_value(c);
        }
        let sys = SysRegAccess::decode(a);
        assert!(sys.reg <= 31);
        let _ = sys.is_known_inert();
        let caller = (d % 4) as usize;
        let _ = psci.call(caller, [a, b, c, d]);
        let _ = psci.started((b % 4) as usize);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    struct Prng(u64);

    impl Prng {
        fn next(&mut self) -> u64 {
            self.0 ^= self.0 >> 12;
            self.0 ^= self.0 << 25;
            self.0 ^= self.0 >> 27;
            self.0.wrapping_mul(0x2545_f491_4f6c_dd1d)
        }
        fn below(&mut self, n: u64) -> u64 {
            self.next() % n
        }
        fn chance(&mut self, one_in: u64) -> bool {
            self.below(one_in) == 0
        }
    }

    fn structured(rng: &mut Prng) -> Vec<u8> {
        let mut data = vec![0u8; HEADER + 0x4000];
        for b in data.iter_mut().take(HEADER) {
            if rng.chance(12) {
                *b = rng.next() as u8;
            }
        }
        let size = 8u64;
        let put = |data: &mut Vec<u8>, off: u64, bytes: &[u8]| {
            let at = HEADER + off as usize;
            if at + bytes.len() <= data.len() {
                data[at..at + bytes.len()].copy_from_slice(bytes);
            }
        };
        let addr = |rng: &mut Prng| match rng.below(10) {
            0 => rng.next(),
            1 => BASE + MEM_LEN - rng.below(64),
            _ => BASE + 0x2000 + rng.below(0x2000),
        };
        for queue in 0..2 {
            let shift = queue * QUEUE_STRIDE;
            for i in 0..size {
                let flags = match rng.below(12) {
                    0 => rng.next() as u16,
                    1 => 4,
                    2..=5 => 1,
                    6..=8 => 2,
                    9 => 3,
                    _ => 0,
                };
                let len = match rng.below(8) {
                    0 => rng.next() as u32,
                    1 => 16 * rng.below(6) as u32,
                    2 => 512,
                    _ => rng.below(64) as u32,
                };
                let next = if rng.chance(8) {
                    rng.next() as u16
                } else {
                    ((i + 1) % size) as u16
                };
                let d = DESC_OFF + shift + i * 16;
                put(&mut data, d, &addr(rng).to_le_bytes());
                put(&mut data, d + 8, &len.to_le_bytes());
                put(&mut data, d + 12, &flags.to_le_bytes());
                put(&mut data, d + 14, &next.to_le_bytes());
            }
            let avail_idx = if rng.chance(10) {
                rng.next() as u16
            } else {
                rng.below(size + 1) as u16
            };
            put(&mut data, AVAIL_OFF + shift + 2, &avail_idx.to_le_bytes());
            for i in 0..size {
                let head = if rng.chance(10) {
                    rng.next() as u16
                } else {
                    rng.below(size) as u16
                };
                put(
                    &mut data,
                    AVAIL_OFF + shift + 4 + i * 2,
                    &head.to_le_bytes(),
                );
            }
        }
        for off in (0x2000..0x4000u64).step_by(0x200) {
            match rng.below(4) {
                0 => {
                    let kind = [0u32, 1, 4, 8, 11, 13, 99][rng.below(7) as usize];
                    put(&mut data, off, &kind.to_le_bytes());
                    let sector = if rng.chance(6) {
                        rng.next()
                    } else {
                        rng.below(2100)
                    };
                    put(&mut data, off + 8, &sector.to_le_bytes());
                }
                1 => {
                    for i in 0..4 {
                        put(&mut data, off + i * 16, &addr(rng).to_le_bytes());
                        put(
                            &mut data,
                            off + i * 16 + 8,
                            &(rng.below(600) as u32).to_le_bytes(),
                        );
                        put(
                            &mut data,
                            off + i * 16 + 12,
                            &(rng.below(4) as u16).to_le_bytes(),
                        );
                        put(
                            &mut data,
                            off + i * 16 + 14,
                            &((i + 1) as u16).to_le_bytes(),
                        );
                    }
                }
                2 => {
                    for i in 0..0x40 {
                        put(&mut data, off + i * 8, &rng.next().to_le_bytes());
                    }
                }
                _ => {}
            }
        }
        data
    }

    fn smoke(harness: fn(&[u8]), seed: u64, rounds: u32) {
        let mut rng = Prng(seed);
        for round in 0..rounds {
            let input = structured(&mut rng);
            let result = std::panic::catch_unwind(|| harness(&input));
            assert!(
                result.is_ok(),
                "harness panicked on round {round} of seed {seed:#x}"
            );
        }
        harness(&[]);
        harness(&[0xff; 7]);
        harness(&[0; HEADER]);
        harness(&vec![0xff; HEADER + 0x4000]);
    }

    #[test]
    fn queue_survives_hostile_input() {
        smoke(fuzz_queue, 0x9e37_79b9_7f4a_7c15, 3000);
    }

    #[test]
    fn fs_survives_hostile_input() {
        let mut rng = Prng(0xf5f5_0000_1234_5678);
        for _ in 0..2000 {
            let mut input = structured(&mut rng);
            for slot in 0..128usize {
                let at = HEADER + 0x2000 + slot * 64;
                let opcode = rng.below(54) as u32;
                input[at..at + 4].copy_from_slice(&(40 + rng.below(64) as u32).to_le_bytes());
                input[at + 4..at + 8].copy_from_slice(&opcode.to_le_bytes());
                input[at + 16..at + 24].copy_from_slice(&rng.below(48).to_le_bytes());
            }
            for i in 0..8usize {
                if !rng.chance(4) {
                    let d = HEADER + DESC_OFF as usize + i * 16;
                    let addr = BASE + 0x2000 + 64 * rng.below(128);
                    input[d..d + 8].copy_from_slice(&addr.to_le_bytes());
                }
            }
            fuzz_fs(&input);
        }
        fuzz_fs(&[]);
        fuzz_fs(&[0; HEADER]);
        fuzz_fs(&vec![0xff; HEADER + 0x4000]);
    }

    #[test]
    fn blk_survives_hostile_input() {
        smoke(fuzz_blk, 0x1234_5678_9abc_def1, 3000);
    }

    #[test]
    fn console_survives_hostile_input() {
        smoke(fuzz_console, 0x0dd_ba11_cafe_f00d, 500);
    }

    #[test]
    fn balloon_survives_hostile_input() {
        smoke(fuzz_balloon, 0xba11_00ba_1100_ba11, 3000);
    }

    #[test]
    fn net_survives_hostile_input() {
        let mut rng = Prng(0x0e70_0e70_0e70_0e70);
        for _ in 0..400 {
            let mut input = structured(&mut rng);
            let mut at = HEADER;
            while at + 4 < HEADER + 4096 {
                let len = match rng.below(6) {
                    0 => rng.next() as u32,
                    1 => rng.below(12) as u32,
                    _ => 12 + rng.below(1500) as u32,
                };
                input[at..at + 4].copy_from_slice(&len.to_le_bytes());
                at += 4 + (len as usize).min(1600);
            }
            fuzz_net(&input);
        }
        fuzz_net(&[]);
    }

    #[test]
    fn vsock_survives_hostile_input() {
        let mut rng = Prng(0x7001_7001_7001_7001);
        for _ in 0..400 {
            let mut input = structured(&mut rng);
            for off in (0x2000..0x4000usize).step_by(0x200) {
                if rng.chance(3) {
                    continue;
                }
                let at = HEADER + off;
                let mut h = [0u8; 44];
                h[0] = if rng.chance(8) { rng.next() as u8 } else { 3 };
                h[8] = if rng.chance(8) { rng.next() as u8 } else { 2 };
                h[16] = rng.below(3) as u8;
                h[20] = rng.below(3) as u8;
                if rng.chance(4) {
                    h[20..24].copy_from_slice(&(1u32 << 30).to_le_bytes());
                }
                let len = if rng.chance(6) {
                    rng.next() as u32
                } else {
                    rng.below(64) as u32
                };
                h[24..28].copy_from_slice(&len.to_le_bytes());
                h[28] = if rng.chance(10) { rng.next() as u8 } else { 1 };
                h[30] = rng.below(9) as u8;
                h[32] = rng.below(4) as u8;
                h[36..40].copy_from_slice(&(rng.next() as u32 >> rng.below(32)).to_le_bytes());
                h[40..44].copy_from_slice(&(rng.next() as u32 >> rng.below(32)).to_le_bytes());
                input[at..at + 44].copy_from_slice(&h);
            }
            fuzz_vsock(&input);
        }
        fuzz_vsock(&[]);
    }

    #[test]
    fn rng_survives_hostile_input() {
        smoke(fuzz_rng, 0xfeed_face_dead_beef, 1500);
    }

    #[test]
    fn transport_survives_hostile_register_sequences() {
        let mut rng = Prng(0x5eed_5eed_5eed_5eed);
        for _ in 0..300 {
            let mut input = structured(&mut rng);
            let script: [(u8, u32); 12] = [
                (0x70 / 4, 1),
                (0x70 / 4, 3),
                (0x24 / 4, 1),
                (0x20 / 4, 1),
                (0x70 / 4, 0xb),
                (0x30 / 4, 0),
                (0x80 / 4, (BASE + DESC_OFF) as u32),
                (0x90 / 4, (BASE + AVAIL_OFF) as u32),
                (0xa0 / 4, (BASE + USED_OFF) as u32),
                (0x44 / 4, 1),
                (0x70 / 4, 0xf),
                (0x50 / 4, 0),
            ];
            let mut ops = Vec::new();
            for (reg, value) in script {
                let value = if rng.chance(10) {
                    rng.next() as u32
                } else {
                    value
                };
                ops.push(reg);
                ops.extend((value & !3 | 2).to_le_bytes());
            }
            for _ in 0..rng.below(40) {
                ops.push(rng.next() as u8);
                ops.extend((rng.next() as u32).to_le_bytes());
            }
            input.truncate(HEADER);
            input.extend(ops);
            fuzz_transport(&input);
        }
        fuzz_transport(&[]);
    }

    #[test]
    fn control_survives_hostile_lines() {
        let path = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../proto/vmmctl/fixtures/wire/session.jsonl");
        let text = std::fs::read_to_string(path).unwrap();
        let mut rng = Prng(0x6374_726c);
        for line in text.lines() {
            let line = &line.as_bytes()[2..];
            fuzz_control(line);
            for _ in 0..200 {
                let mut bytes = line.to_vec();
                match rng.next() % 4 {
                    0 => bytes.truncate(rng.below(bytes.len() as u64 + 1) as usize),
                    1 => {
                        let at = rng.below(bytes.len().max(1) as u64) as usize;
                        if let Some(b) = bytes.get_mut(at) {
                            *b = rng.next() as u8;
                        }
                    }
                    2 => {
                        let at = rng.below(bytes.len().max(1) as u64) as usize;
                        let piece = bytes[at..].to_vec();
                        bytes.extend_from_slice(&piece);
                    }
                    _ => {
                        let at = rng.below(bytes.len().max(1) as u64) as usize;
                        bytes.insert(at, b"\"\\{}[]:,\n0-e"[rng.below(12) as usize]);
                    }
                }
                fuzz_control(&bytes);
            }
        }
        for _ in 0..2000 {
            let len = rng.below(200) as usize;
            let bytes: Vec<u8> = (0..len).map(|_| rng.next() as u8).collect();
            fuzz_control(&bytes);
        }
        for id in [
            "0",
            "-1",
            "1e3",
            "18446744073709551616",
            "\"\\n\"",
            "\"\\u0000\"",
            "null",
        ] {
            fuzz_control(
                format!("{{\"jsonrpc\":\"2.0\",\"id\":{id},\"method\":\"stats\"}}").as_bytes(),
            );
            fuzz_control(
                format!("{{\"jsonrpc\":\"2.0\",\"id\":{id},\"method\":\"nope\"}}").as_bytes(),
            );
        }
    }

    #[test]
    fn exit_decoding_and_psci_survive_any_register_values() {
        let mut rng = Prng(0xabad_1dea_abad_1dea);
        for _ in 0..2000 {
            let words: Vec<u8> = (0..32)
                .flat_map(|_| {
                    let w = match rng.below(4) {
                        0 => 0x8400_0000 | rng.below(16),
                        1 => 0xc400_0000 | rng.below(16),
                        2 => (0x24 << 26) | (rng.next() & 0x3ff_ffff),
                        _ => rng.next(),
                    };
                    w.to_le_bytes()
                })
                .collect();
            fuzz_exit(&words);
        }
    }
}
