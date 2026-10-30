use super::{Activation, VirtioDevice};

const DEVICE_ID: u32 = 4;
const QUEUE_SIZES: [u16; 1] = [16];
const ENTROPY_CHUNK: usize = 256;
const MAX_PER_REQUEST: u64 = 64 << 10;

#[derive(Default)]
pub struct Rng {
    active: Option<Activation>,
}

impl Rng {
    pub fn new() -> Self {
        Self::default()
    }
}

pub fn host_entropy(buf: &mut [u8]) {
    for chunk in buf.chunks_mut(ENTROPY_CHUNK) {
        let ret = unsafe { libc::getentropy(chunk.as_mut_ptr().cast(), chunk.len()) };
        assert_eq!(ret, 0, "getentropy failed");
    }
}

impl VirtioDevice for Rng {
    fn device_id(&self) -> u32 {
        DEVICE_ID
    }

    fn features(&self) -> u64 {
        0
    }

    fn queue_max_sizes(&self) -> &[u16] {
        &QUEUE_SIZES
    }

    fn activate(&mut self, activation: Activation) {
        self.active = Some(activation);
    }

    fn notify(&mut self, _queue: u32) {
        let Some(a) = self.active.as_mut() else {
            return;
        };
        let mem = &a.mem;
        let mut chunk = [0u8; ENTROPY_CHUNK];
        let result = a.queues[0].drain(mem, |chain| {
            let mut written = 0u64;
            'bufs: for b in &chain.writable {
                let mut done = 0u64;
                while done < b.len as u64 {
                    if written == MAX_PER_REQUEST {
                        break 'bufs;
                    }
                    let n = (b.len as u64 - done)
                        .min(ENTROPY_CHUNK as u64)
                        .min(MAX_PER_REQUEST - written) as usize;
                    host_entropy(&mut chunk[..n]);
                    if mem.write(b.addr.wrapping_add(done), &chunk[..n]).is_err() {
                        break 'bufs;
                    }
                    done += n as u64;
                    written += n as u64;
                }
            }
            written.min(u32::MAX as u64) as u32
        });
        match result {
            Ok(true) => a.irq.signal_used(),
            Ok(false) => {}
            Err(e) => a.irq.fail("virtio-rng", &e),
        }
    }

    fn reset(&mut self) {
        self.active = None;
    }
}

#[cfg(test)]
mod tests {
    use super::super::queue::testing::{memory, Driver};
    use super::super::testing::{interrupt, INTID};
    use super::*;
    use std::sync::Arc;

    const BASE: u64 = 0x4000_0000;

    #[test]
    fn fills_guest_buffers_with_entropy() {
        let mem = Arc::new(memory());
        let (mut drv, q) = Driver::new(&mem, BASE, 8);
        let (chip, irq) = interrupt();
        let mut dev = Rng::new();
        dev.activate(Activation {
            mem: mem.clone(),
            irq,
            queues: vec![q],
            features: 0,
        });

        let head = drv.add(
            &mem,
            &[(BASE + 0x8000, 600, true), (BASE + 0x9000, 40, true)],
        );
        dev.notify(0);
        assert_eq!(drv.take_used(&mem), Some((head, 640)));
        assert!(chip.level(INTID));

        let mut a = [0u8; 600];
        let mut b = [0u8; 40];
        mem.read(BASE + 0x8000, &mut a).unwrap();
        mem.read(BASE + 0x9000, &mut b).unwrap();
        assert!(a.iter().any(|x| *x != 0) && b.iter().any(|x| *x != 0));
        assert_ne!(&a[..40], &b[..]);
    }

    #[test]
    fn one_request_is_capped() {
        let mem = Arc::new(memory());
        let (mut drv, q) = Driver::new(&mem, BASE, 8);
        let (_chip, irq) = interrupt();
        let mut dev = Rng::new();
        dev.activate(Activation {
            mem: mem.clone(),
            irq,
            queues: vec![q],
            features: 0,
        });
        let head = drv.add(
            &mem,
            &[
                (BASE + 0x10000, 60 << 10, true),
                (BASE + 0x20000, 60 << 10, true),
            ],
        );
        dev.notify(0);
        assert_eq!(drv.take_used(&mem), Some((head, MAX_PER_REQUEST as u32)));
    }

    #[test]
    fn a_buffer_outside_ram_gets_nothing() {
        let mem = Arc::new(memory());
        let (mut drv, q) = Driver::new(&mem, BASE, 8);
        let (_chip, irq) = interrupt();
        let mut dev = Rng::new();
        dev.activate(Activation {
            mem: mem.clone(),
            irq,
            queues: vec![q],
            features: 0,
        });
        let head = drv.add(&mem, &[(0x1000, 64, true)]);
        dev.notify(0);
        assert_eq!(drv.take_used(&mem), Some((head, 0)));
    }
}
