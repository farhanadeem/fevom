pub mod pl011;
pub mod pl031;

fn primecell_id(offset: u64, periph: [u8; 4]) -> Option<u64> {
    const CELL: [u8; 4] = [0x0d, 0xf0, 0x05, 0xb1];
    if !(0xfe0..0x1000).contains(&offset) || !offset.is_multiple_of(4) {
        return None;
    }
    let i = ((offset - 0xfe0) / 4) as usize;
    Some(if i < 4 { periph[i] } else { CELL[i - 4] } as u64)
}
