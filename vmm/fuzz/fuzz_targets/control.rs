#![no_main]
libfuzzer_sys::fuzz_target!(|data: &[u8]| fevvm::fuzzing::fuzz_control(data));
