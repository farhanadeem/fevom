use std::fmt;
use std::fs::File;
use std::io::{self, Read};
use std::path::Path;

use crate::memory::GuestMemory;

const IMAGE_HEADER_LEN: usize = 64;
const IMAGE_MAGIC: u32 = 0x644d_5241;
const ALIGN_2M: u64 = 2 << 20;
pub const FDT_MAX_SIZE: u64 = 2 << 20;

#[derive(Debug)]
pub enum BootError {
    Io(String, io::Error),
    NotAnImage,
    TooBig {
        what: &'static str,
        need: u64,
        ram: u64,
    },
}

impl fmt::Display for BootError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Io(path, e) => write!(f, "{path}: {e}"),
            Self::NotAnImage => write!(
                f,
                "kernel is not an uncompressed arm64 Image (bad magic); pass kernel/out/Image"
            ),
            Self::TooBig { what, need, ram } => write!(
                f,
                "{what} does not fit: needs {} MiB of guest RAM, have {} MiB",
                need.div_ceil(1 << 20),
                ram >> 20
            ),
        }
    }
}

impl std::error::Error for BootError {}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ImageHeader {
    pub text_offset: u64,
    pub image_size: u64,
}

impl ImageHeader {
    pub fn parse(header: &[u8]) -> Option<Self> {
        if header.len() < IMAGE_HEADER_LEN {
            return None;
        }
        let u64_at = |o: usize| u64::from_le_bytes(header[o..o + 8].try_into().unwrap());
        let magic = u32::from_le_bytes(header[56..60].try_into().unwrap());
        if magic != IMAGE_MAGIC {
            return None;
        }
        let image_size = u64_at(16);
        (image_size != 0).then_some(Self {
            text_offset: u64_at(8),
            image_size,
        })
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Loaded {
    pub entry: u64,
    pub initrd: Option<(u64, u64)>,
    pub fdt_addr: u64,
}

pub fn place(
    ram_base: u64,
    ram_len: u64,
    header: ImageHeader,
    kernel_file_len: u64,
    initrd_len: Option<u64>,
) -> Result<Loaded, BootError> {
    let too_big = |what, end: u64| BootError::TooBig {
        what,
        need: end.saturating_sub(ram_base),
        ram: ram_len,
    };
    let ram_end = ram_base + ram_len;
    let entry = ram_base
        .checked_add(header.text_offset)
        .ok_or(BootError::NotAnImage)?;
    let kernel_end = entry
        .checked_add(header.image_size.max(kernel_file_len))
        .ok_or(BootError::NotAnImage)?;
    if kernel_end > ram_end {
        return Err(too_big("kernel", kernel_end));
    }

    let mut next = kernel_end.next_multiple_of(ALIGN_2M);
    let initrd = match initrd_len {
        Some(len) => {
            let end = next.saturating_add(len);
            if end > ram_end {
                return Err(too_big("initrd", end));
            }
            let range = (next, end);
            next = end.next_multiple_of(ALIGN_2M);
            Some(range)
        }
        None => None,
    };

    let fdt_end = next.saturating_add(FDT_MAX_SIZE);
    if fdt_end > ram_end {
        return Err(too_big("device tree", fdt_end));
    }
    Ok(Loaded {
        entry,
        initrd,
        fdt_addr: next,
    })
}

fn open(path: &Path) -> Result<(File, u64), BootError> {
    let err = |e| BootError::Io(path.display().to_string(), e);
    let file = File::open(path).map_err(err)?;
    let len = file.metadata().map_err(err)?.len();
    Ok((file, len))
}

fn read_into(
    mem: &GuestMemory,
    addr: u64,
    file: &File,
    offset: u64,
    len: u64,
    path: &Path,
) -> Result<(), BootError> {
    let err = |e| BootError::Io(path.display().to_string(), e);
    match mem.read_from_file(file, offset, addr, len) {
        Ok(true) => Ok(()),
        Ok(false) => Err(err(io::Error::other("short read"))),
        Err(e) => Err(err(io::Error::other(e))),
    }
}

pub fn load(mem: &GuestMemory, kernel: &Path, initrd: Option<&Path>) -> Result<Loaded, BootError> {
    let (mut kfile, klen) = open(kernel)?;
    let mut header = [0u8; IMAGE_HEADER_LEN];
    kfile
        .read_exact(&mut header)
        .map_err(|_| BootError::NotAnImage)?;
    let parsed = ImageHeader::parse(&header).ok_or(BootError::NotAnImage)?;

    let initrd_file = initrd.map(open).transpose()?;
    let loaded = place(
        mem.base(),
        mem.len(),
        parsed,
        klen,
        initrd_file.as_ref().map(|(_, len)| *len),
    )?;

    read_into(mem, loaded.entry, &kfile, 0, klen, kernel)?;
    if let (Some((file, len)), Some((start, _))) = (initrd_file, loaded.initrd) {
        read_into(mem, start, &file, 0, len, initrd.unwrap())?;
    }
    Ok(loaded)
}

pub const FLAT_LOAD_OFFSET: u64 = 0x8_0000;

pub fn load_flat(
    mem: &GuestMemory,
    path: &Path,
    initrd: Option<&Path>,
) -> Result<Loaded, BootError> {
    let (file, len) = open(path)?;
    let entry = mem.base() + FLAT_LOAD_OFFSET;
    let fdt_addr = (mem.end().saturating_sub(FDT_MAX_SIZE)) / ALIGN_2M * ALIGN_2M;
    let initrd_file = initrd.map(open).transpose()?;
    let initrd_len = initrd_file.as_ref().map_or(0, |(_, len)| *len);
    let top = fdt_addr.saturating_sub(initrd_len) / ALIGN_2M * ALIGN_2M;
    if entry + len.saturating_mul(2) > top || top < mem.base() {
        return Err(BootError::TooBig {
            what: "flat binary",
            need: FLAT_LOAD_OFFSET + len.saturating_mul(2) + initrd_len + 2 * FDT_MAX_SIZE,
            ram: mem.len(),
        });
    }
    read_into(mem, entry, &file, 0, len, path)?;
    let mut placed = None;
    if let Some((file, len)) = initrd_file {
        read_into(mem, top, &file, 0, len, initrd.unwrap())?;
        placed = Some((top, top + len));
    }
    Ok(Loaded {
        entry,
        initrd: placed,
        fdt_addr,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn header(text_offset: u64, image_size: u64) -> Vec<u8> {
        let mut h = vec![0u8; 64];
        h[8..16].copy_from_slice(&text_offset.to_le_bytes());
        h[16..24].copy_from_slice(&image_size.to_le_bytes());
        h[56..60].copy_from_slice(b"ARM\x64");
        h
    }

    #[test]
    fn parses_image_header() {
        let h = ImageHeader::parse(&header(0, 0x150_0000)).unwrap();
        assert_eq!(h.text_offset, 0);
        assert_eq!(h.image_size, 0x150_0000);
        assert!(ImageHeader::parse(&[0u8; 64]).is_none());
        assert!(ImageHeader::parse(&header(0, 0)).is_none());
        assert!(ImageHeader::parse(&header(0, 1)[..63]).is_none());
    }

    #[test]
    fn places_kernel_initrd_and_fdt_without_overlap() {
        let h = ImageHeader {
            text_offset: 0,
            image_size: 0x150_0000,
        };
        let l = place(0x4000_0000, 256 << 20, h, 0x130_0000, Some(0x12_3456)).unwrap();
        assert_eq!(l.entry, 0x4000_0000);
        assert_eq!(l.initrd, Some((0x4160_0000, 0x4172_3456)));
        assert_eq!(l.fdt_addr, 0x4180_0000);

        let l = place(0x4000_0000, 256 << 20, h, 0x130_0000, None).unwrap();
        assert_eq!(l.initrd, None);
        assert_eq!(l.fdt_addr, 0x4160_0000);
    }

    #[test]
    fn loads_a_flat_binary_below_the_device_tree() {
        let path = std::env::temp_dir().join(format!("fev-flat-{}.bin", std::process::id()));
        std::fs::write(&path, b"\x1f\x20\x03\xd5payload").unwrap();
        let mem = GuestMemory::new(0x4000_0000, 16 << 20).unwrap();
        let l = load_flat(&mem, &path, None).unwrap();
        assert_eq!(
            (l.entry, l.fdt_addr, l.initrd),
            (0x4008_0000, 0x40e0_0000, None)
        );
        let l = load_flat(&mem, &path, Some(&path)).unwrap();
        assert_eq!(l.initrd, Some((0x40c0_0000, 0x40c0_0000 + 11)));
        let mut got = [0u8; 11];
        mem.read(0x4008_0000, &mut got).unwrap();
        assert_eq!(&got[4..], b"payload");

        let small = GuestMemory::new(0x4000_0000, 2 << 20).unwrap();
        assert!(matches!(
            load_flat(&small, &path, None),
            Err(BootError::TooBig {
                what: "flat binary",
                ..
            })
        ));
        std::fs::remove_file(path).unwrap();
    }

    #[test]
    fn refuses_what_does_not_fit() {
        let h = ImageHeader {
            text_offset: 0,
            image_size: 32 << 20,
        };
        assert!(matches!(
            place(0x4000_0000, 16 << 20, h, 1, None),
            Err(BootError::TooBig { what: "kernel", .. })
        ));
        assert!(matches!(
            place(0x4000_0000, 64 << 20, h, 1, Some(40 << 20)),
            Err(BootError::TooBig { what: "initrd", .. })
        ));
        assert!(matches!(
            place(0x4000_0000, 32 << 20, h, 1, None),
            Err(BootError::TooBig {
                what: "device tree",
                ..
            })
        ));
        let huge = ImageHeader {
            text_offset: u64::MAX,
            image_size: 1,
        };
        assert!(place(0x4000_0000, 64 << 20, huge, 1, None).is_err());
    }
}
