use std::mem::MaybeUninit;
use std::sync::atomic::{AtomicBool, Ordering};

static RAW: AtomicBool = AtomicBool::new(false);
static mut SAVED: MaybeUninit<libc::termios> = MaybeUninit::uninit();

pub fn is_raw() -> bool {
    RAW.load(Ordering::Relaxed)
}

pub fn enter_raw() -> bool {
    unsafe {
        if libc::isatty(0) != 1 {
            return false;
        }
        let saved = &raw mut SAVED;
        if libc::tcgetattr(0, (*saved).as_mut_ptr()) != 0 {
            return false;
        }
        let mut raw = (*saved).assume_init();
        libc::cfmakeraw(&mut raw);
        if libc::tcsetattr(0, libc::TCSANOW, &raw) != 0 {
            return false;
        }
        RAW.store(true, Ordering::SeqCst);

        for sig in [libc::SIGTERM, libc::SIGHUP, libc::SIGINT, libc::SIGQUIT] {
            libc::signal(sig, on_fatal_signal as *const () as libc::sighandler_t);
        }
        let hook = std::panic::take_hook();
        std::panic::set_hook(Box::new(move |info| {
            restore();
            hook(info);
        }));
    }
    true
}

pub fn restore() {
    if RAW.swap(false, Ordering::SeqCst) {
        unsafe {
            let saved = &raw const SAVED;
            libc::tcsetattr(0, libc::TCSANOW, (*saved).as_ptr());
        }
    }
}

extern "C" fn on_fatal_signal(sig: libc::c_int) {
    restore();
    unsafe { libc::_exit(128 + sig) };
}
