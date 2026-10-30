pub const GIC_DIST_BASE: u64 = 0x0800_0000;
pub const GIC_REDIST_BASE: u64 = 0x080a_0000;

pub const UART_BASE: u64 = 0x0c00_0000;
pub const UART_SIZE: u64 = 0x1000;
pub const UART_EARLY_BASE: u64 = 0x0900_0000;
pub const RTC_BASE: u64 = 0x0c01_0000;
pub const RTC_SIZE: u64 = 0x1000;

pub const VIRTIO_BASE: u64 = 0x0c10_0000;
pub const VIRTIO_SLOT_SIZE: u64 = 0x1000;
pub const VIRTIO_REG_SIZE: u64 = 0x200;
pub const VIRTIO_MAX_SLOTS: u32 = 32;

pub const RAM_BASE: u64 = 0x4000_0000;

pub const DEFAULT_IPA_BITS: u32 = 36;
pub const WINDOW_IPA_BITS: u32 = 40;
pub const FS_WINDOW_BASE: u64 = 128 << 30;
pub const DISK_WINDOW_BASE: u64 = 256 << 30;
pub const DISK_WINDOW_MAX: u64 = 256 << 30;
pub const FS_WINDOW_RESERVE: u64 = 4 << 30;
pub const WINDOW_ALIGN: u64 = 1 << 30;

pub fn disk_window(ipa_bits: u32, ram_end: u64, image_len: u64) -> Option<(u64, u64)> {
    let (base, room) = if ipa_bits >= WINDOW_IPA_BITS {
        (DISK_WINDOW_BASE, DISK_WINDOW_MAX)
    } else {
        let top = 1u64.checked_shl(ipa_bits)?.checked_sub(FS_WINDOW_RESERVE)?;
        let base = ram_end.checked_next_multiple_of(WINDOW_ALIGN)?;
        (base, top.checked_sub(base)?)
    };
    if ram_end > base {
        return None;
    }
    let len = if image_len < WINDOW_ALIGN {
        image_len & !(HOST_PAGE - 1)
    } else {
        image_len.min(room) & !(WINDOW_ALIGN - 1)
    };
    (len != 0 && len <= room).then_some((base, len))
}

const _: () = assert!(1u64 << DEFAULT_IPA_BITS <= FS_WINDOW_BASE);
const _: () = assert!(FS_WINDOW_BASE + FS_WINDOW_RESERVE <= DISK_WINDOW_BASE);
const _: () = assert!(DISK_WINDOW_BASE + DISK_WINDOW_MAX <= 1 << WINDOW_IPA_BITS);

pub const HOST_PAGE: u64 = 16 << 10;

pub const SPI_BASE: u32 = 32;
pub const UART_INTID: u32 = SPI_BASE;
pub const VIRTIO_INTID_BASE: u32 = SPI_BASE + 16;

pub const fn virtio_slot_base(slot: u32) -> u64 {
    VIRTIO_BASE + slot as u64 * VIRTIO_SLOT_SIZE
}

pub const fn virtio_slot_intid(slot: u32) -> u32 {
    VIRTIO_INTID_BASE + slot
}

#[cfg(test)]
mod tests {
    use super::*;

    const GIB: u64 = 1 << 30;

    #[test]
    fn disk_window_at_forty_bits_is_fixed_and_capped() {
        assert_eq!(
            disk_window(40, RAM_BASE + 8 * GIB, 1024 * GIB),
            Some((256 * GIB, 256 * GIB))
        );
        assert_eq!(
            disk_window(40, RAM_BASE + GIB, 3 * GIB + 5),
            Some((256 * GIB, 3 * GIB))
        );
        assert_eq!(
            disk_window(40, RAM_BASE + GIB, (64 << 20) + 4096),
            Some((256 * GIB, 64 << 20))
        );
        assert_eq!(disk_window(40, RAM_BASE + GIB, 4096), None);
        assert_eq!(disk_window(40, RAM_BASE + GIB, 0), None);
        assert_eq!(
            disk_window(48, RAM_BASE + GIB, 1024 * GIB),
            Some((256 * GIB, 256 * GIB))
        );
    }

    #[test]
    fn disk_window_in_a_narrow_space_is_what_fits_above_ram() {
        assert_eq!(
            disk_window(36, RAM_BASE + 8 * GIB, 1024 * GIB),
            Some((9 * GIB, 51 * GIB))
        );
        assert_eq!(
            disk_window(36, RAM_BASE + (512 << 20), 1024 * GIB),
            Some((2 * GIB, 58 * GIB))
        );
        assert_eq!(
            disk_window(36, RAM_BASE + GIB, 256 << 20),
            Some((2 * GIB, 256 << 20))
        );
        assert_eq!(disk_window(36, 60 * GIB, 1024 * GIB), None);
        assert_eq!(disk_window(36, 61 * GIB, 1024 * GIB), None);
        assert_eq!(disk_window(32, RAM_BASE + 8 * GIB, 1024 * GIB), None);
        assert_eq!(
            disk_window(64, RAM_BASE + GIB, 1024 * GIB).map(|w| w.0),
            Some(256 * GIB)
        );
    }
}
