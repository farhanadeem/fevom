#![no_main]
libfuzzer_sys::fuzz_target!(|data: &[u8]| fileshare::fuzzing::fuzz_decoder(data));
