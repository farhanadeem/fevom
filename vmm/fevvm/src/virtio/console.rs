use std::collections::VecDeque;
use std::sync::{Arc, Mutex};

use super::queue::Queue;
use super::{Activation, Interrupt, VirtioDevice};
use crate::console::ConsoleOut;
use crate::memory::GuestMemory;

const DEVICE_ID: u32 = 3;
const QUEUE_SIZES: [u16; 2] = [64, 64];
const RX: u32 = 0;
const TX: u32 = 1;
const MAX_PENDING_INPUT: usize = 64 << 10;

struct Active {
    mem: Arc<GuestMemory>,
    irq: Arc<Interrupt>,
    rx: Queue,
    tx: Queue,
}

#[derive(Default)]
struct State {
    active: Option<Active>,
    pending: VecDeque<u8>,
}

impl State {
    fn flush_input(&mut self) {
        let Some(a) = self.active.as_mut() else {
            return;
        };
        let result = (|| {
            loop {
                while !self.pending.is_empty() {
                    let Some(chain) = a.rx.pop(&a.mem)? else {
                        break;
                    };
                    let n = chain.write(&a.mem, self.pending.make_contiguous());
                    self.pending.drain(..n);
                    a.rx.push_used(&a.mem, chain.head, n as u32)?;
                }
                if self.pending.is_empty() || !a.rx.enable_notifications(&a.mem)? {
                    break;
                }
            }
            a.rx.needs_interrupt(&a.mem)
        })();
        match result {
            Ok(true) => a.irq.signal_used(),
            Ok(false) => {}
            Err(e) => a.irq.fail("virtio-console", &e),
        }
    }
}

pub struct Console {
    out: Arc<ConsoleOut>,
    state: Arc<Mutex<State>>,
}

#[derive(Clone)]
pub struct ConsoleInput {
    state: Arc<Mutex<State>>,
}

impl ConsoleInput {
    pub fn push(&self, data: &[u8]) {
        let mut state = self.state.lock().unwrap();
        let room = MAX_PENDING_INPUT.saturating_sub(state.pending.len());
        state.pending.extend(&data[..data.len().min(room)]);
        state.flush_input();
    }
}

impl Console {
    pub fn new(out: Arc<ConsoleOut>) -> (Self, ConsoleInput) {
        let state = Arc::new(Mutex::new(State::default()));
        (
            Self {
                out,
                state: state.clone(),
            },
            ConsoleInput { state },
        )
    }

    fn transmit(&mut self) {
        let mut state = self.state.lock().unwrap();
        let Some(a) = state.active.as_mut() else {
            return;
        };
        let out = &self.out;
        let mem = &a.mem;
        let mut chunk = [0u8; 4096];
        let result = a.tx.drain(mem, |chain| {
            for b in &chain.readable {
                let mut done = 0u64;
                while done < b.len as u64 {
                    let n = (b.len as u64 - done).min(chunk.len() as u64) as usize;
                    if mem
                        .read(b.addr.wrapping_add(done), &mut chunk[..n])
                        .is_err()
                    {
                        break;
                    }
                    out.write(&chunk[..n]);
                    done += n as u64;
                }
            }
            0
        });
        match result {
            Ok(true) => a.irq.signal_used(),
            Ok(false) => {}
            Err(e) => a.irq.fail("virtio-console", &e),
        }
    }
}

impl VirtioDevice for Console {
    fn device_id(&self) -> u32 {
        DEVICE_ID
    }

    fn features(&self) -> u64 {
        0
    }

    fn queue_max_sizes(&self) -> &[u16] {
        &QUEUE_SIZES
    }

    fn activate(&mut self, mut activation: Activation) {
        let tx = activation.queues.pop().unwrap();
        let rx = activation.queues.pop().unwrap();
        let mut state = self.state.lock().unwrap();
        state.active = Some(Active {
            mem: activation.mem,
            irq: activation.irq,
            rx,
            tx,
        });
    }

    fn notify(&mut self, queue: u32) {
        match queue {
            RX => self.state.lock().unwrap().flush_input(),
            TX => self.transmit(),
            _ => {}
        }
    }

    fn reset(&mut self) {
        self.state.lock().unwrap().active = None;
    }
}

#[cfg(test)]
mod tests {
    use super::super::queue::testing::{memory, Driver};
    use super::super::testing::{interrupt, INTID};
    use super::*;
    use crate::console::testing::capture;

    const BASE: u64 = 0x4000_0000;

    struct Fixture {
        mem: Arc<GuestMemory>,
        rx: Driver,
        tx: Driver,
        dev: Console,
        input: ConsoleInput,
        cap: crate::console::testing::Capture,
        chip: Arc<crate::irq::testing::RecordingChip>,
    }

    fn fixture() -> Fixture {
        let mem = Arc::new(memory());
        let (rx, rxq) = Driver::new(&mem, BASE, 8);
        let (tx, txq) = Driver::new(&mem, BASE + 0x1000, 8);
        let (cap, out) = capture();
        let (chip, irq) = interrupt();
        let (mut dev, input) = Console::new(out);
        dev.activate(Activation {
            mem: mem.clone(),
            irq,
            queues: vec![rxq, txq],
            features: 0,
        });
        Fixture {
            mem,
            rx,
            tx,
            dev,
            input,
            cap,
            chip,
        }
    }

    #[test]
    fn guest_output_reaches_the_host() {
        let mut f = fixture();
        f.mem.write(BASE + 0x8000, b"hello ").unwrap();
        f.mem.write(BASE + 0x9000, b"world\n").unwrap();
        let head = f.tx.add(
            &f.mem,
            &[(BASE + 0x8000, 6, false), (BASE + 0x9000, 6, false)],
        );
        f.dev.notify(TX);
        assert_eq!(f.cap.text(), "hello world\n");
        assert_eq!(f.tx.take_used(&f.mem), Some((head, 0)));
        assert!(f.chip.level(INTID));
    }

    #[test]
    fn host_input_waits_for_a_guest_buffer() {
        let mut f = fixture();
        f.input.push(b"ls\n");
        assert_eq!(f.rx.take_used(&f.mem), None);
        assert!(!f.chip.level(INTID));

        let head = f.rx.add(&f.mem, &[(BASE + 0x8000, 2, true)]);
        f.dev.notify(RX);
        assert_eq!(f.rx.take_used(&f.mem), Some((head, 2)));
        assert!(f.chip.level(INTID));

        let head = f.rx.add(&f.mem, &[(BASE + 0x9000, 16, true)]);
        f.dev.notify(RX);
        assert_eq!(f.rx.take_used(&f.mem), Some((head, 1)));
        let mut got = [0u8; 3];
        f.mem.read(BASE + 0x8000, &mut got[..2]).unwrap();
        f.mem.read(BASE + 0x9000, &mut got[2..]).unwrap();
        assert_eq!(&got, b"ls\n");
    }

    #[test]
    fn input_is_delivered_at_once_when_a_buffer_is_posted() {
        let mut f = fixture();
        let head = f.rx.add(&f.mem, &[(BASE + 0x8000, 64, true)]);
        f.input.push(b"echo hi\n");
        assert_eq!(f.rx.take_used(&f.mem), Some((head, 8)));
    }

    #[test]
    fn input_before_activation_is_kept_and_bounded() {
        let (_cap, out) = capture();
        let (mut dev, input) = Console::new(out);
        input.push(&vec![b'x'; MAX_PENDING_INPUT + 100]);
        assert_eq!(dev.state.lock().unwrap().pending.len(), MAX_PENDING_INPUT);
        dev.reset();
    }

    #[test]
    fn a_malformed_queue_stops_the_device_without_panicking() {
        let mut f = fixture();
        f.tx.publish(&f.mem, 99);
        f.dev.notify(TX);
        assert!(f
            .dev
            .state
            .lock()
            .unwrap()
            .active
            .as_ref()
            .unwrap()
            .irq
            .failed());
    }
}
