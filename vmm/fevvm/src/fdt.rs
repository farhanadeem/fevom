use vm_fdt::{Error, FdtWriter};

use crate::layout::SPI_BASE;

const PHANDLE_GIC: u32 = 1;
const PHANDLE_CLOCK: u32 = 2;

const GIC_SPI: u32 = 0;
const GIC_PPI: u32 = 1;
const IRQ_TYPE_LEVEL_HIGH: u32 = 4;

const TIMER_PPIS: [u32; 4] = [13, 14, 11, 10];
pub const VTIMER_INTID: u32 = 16 + TIMER_PPIS[2];

#[derive(Debug, Clone, Copy)]
pub struct Region {
    pub base: u64,
    pub size: u64,
}

#[derive(Debug, Clone, Copy)]
pub struct IrqDevice {
    pub region: Region,
    pub intid: u32,
}

pub struct FdtConfig<'a> {
    pub ram: Region,
    pub cpus: &'a [u64],
    pub cmdline: &'a str,
    pub initrd: Option<(u64, u64)>,
    pub rng_seed: &'a [u8],
    pub gic_dist: Region,
    pub gic_redist: Region,
    pub uart: IrqDevice,
    pub rtc: Region,
    pub virtio: &'a [IrqDevice],
}

fn reg(r: Region) -> [u64; 2] {
    [r.base, r.size]
}

fn spi(intid: u32) -> [u32; 3] {
    [GIC_SPI, intid - SPI_BASE, IRQ_TYPE_LEVEL_HIGH]
}

pub fn build(cfg: &FdtConfig) -> Result<Vec<u8>, Error> {
    let mut fdt = FdtWriter::new()?;

    let root = fdt.begin_node("")?;
    fdt.property_string("compatible", "fev,virt")?;
    fdt.property_u32("#address-cells", 2)?;
    fdt.property_u32("#size-cells", 2)?;
    fdt.property_u32("interrupt-parent", PHANDLE_GIC)?;

    let chosen = fdt.begin_node("chosen")?;
    fdt.property_string("bootargs", cfg.cmdline)?;
    fdt.property_string("stdout-path", &format!("/pl011@{:x}", cfg.uart.region.base))?;
    if let Some((start, end)) = cfg.initrd {
        fdt.property_u64("linux,initrd-start", start)?;
        fdt.property_u64("linux,initrd-end", end)?;
    }
    if !cfg.rng_seed.is_empty() {
        fdt.property("rng-seed", cfg.rng_seed)?;
    }
    fdt.end_node(chosen)?;

    let memory = fdt.begin_node(&format!("memory@{:x}", cfg.ram.base))?;
    fdt.property_string("device_type", "memory")?;
    fdt.property_array_u64("reg", &reg(cfg.ram))?;
    fdt.end_node(memory)?;

    let cpus = fdt.begin_node("cpus")?;
    fdt.property_u32("#address-cells", 1)?;
    fdt.property_u32("#size-cells", 0)?;
    for &mpidr in cfg.cpus {
        let cpu = fdt.begin_node(&format!("cpu@{mpidr:x}"))?;
        fdt.property_string("device_type", "cpu")?;
        fdt.property_string("compatible", "arm,armv8")?;
        fdt.property_string("enable-method", "psci")?;
        fdt.property_u32("reg", mpidr as u32)?;
        fdt.end_node(cpu)?;
    }
    fdt.end_node(cpus)?;

    let psci = fdt.begin_node("psci")?;
    fdt.property_string_list(
        "compatible",
        vec!["arm,psci-1.0".into(), "arm,psci-0.2".into()],
    )?;
    fdt.property_string("method", "hvc")?;
    fdt.end_node(psci)?;

    let gic = fdt.begin_node(&format!("intc@{:x}", cfg.gic_dist.base))?;
    fdt.property_string("compatible", "arm,gic-v3")?;
    fdt.property_u32("#interrupt-cells", 3)?;
    fdt.property_null("interrupt-controller")?;
    fdt.property_array_u64(
        "reg",
        &[
            cfg.gic_dist.base,
            cfg.gic_dist.size,
            cfg.gic_redist.base,
            cfg.gic_redist.size,
        ],
    )?;
    fdt.property_phandle(PHANDLE_GIC)?;
    fdt.end_node(gic)?;

    let timer = fdt.begin_node("timer")?;
    fdt.property_string("compatible", "arm,armv8-timer")?;
    let mut cells = Vec::new();
    for ppi in TIMER_PPIS {
        cells.extend([GIC_PPI, ppi, IRQ_TYPE_LEVEL_HIGH]);
    }
    fdt.property_array_u32("interrupts", &cells)?;
    fdt.property_null("always-on")?;
    fdt.end_node(timer)?;

    let clock = fdt.begin_node("apb-pclk")?;
    fdt.property_string("compatible", "fixed-clock")?;
    fdt.property_u32("#clock-cells", 0)?;
    fdt.property_u32("clock-frequency", 24_000_000)?;
    fdt.property_string("clock-output-names", "clk24mhz")?;
    fdt.property_phandle(PHANDLE_CLOCK)?;
    fdt.end_node(clock)?;

    let uart = fdt.begin_node(&format!("pl011@{:x}", cfg.uart.region.base))?;
    fdt.property_string_list(
        "compatible",
        vec!["arm,pl011".into(), "arm,primecell".into()],
    )?;
    fdt.property_array_u64("reg", &reg(cfg.uart.region))?;
    fdt.property_array_u32("interrupts", &spi(cfg.uart.intid))?;
    fdt.property_array_u32("clocks", &[PHANDLE_CLOCK, PHANDLE_CLOCK])?;
    fdt.property_string_list("clock-names", vec!["uartclk".into(), "apb_pclk".into()])?;
    fdt.end_node(uart)?;

    let rtc = fdt.begin_node(&format!("pl031@{:x}", cfg.rtc.base))?;
    fdt.property_string_list(
        "compatible",
        vec!["arm,pl031".into(), "arm,primecell".into()],
    )?;
    fdt.property_array_u64("reg", &reg(cfg.rtc))?;
    fdt.property_u32("clocks", PHANDLE_CLOCK)?;
    fdt.property_string("clock-names", "apb_pclk")?;
    fdt.end_node(rtc)?;

    for dev in cfg.virtio {
        let node = fdt.begin_node(&format!("virtio_mmio@{:x}", dev.region.base))?;
        fdt.property_string("compatible", "virtio,mmio")?;
        fdt.property_array_u64("reg", &reg(dev.region))?;
        fdt.property_array_u32("interrupts", &spi(dev.intid))?;
        fdt.property_null("dma-coherent")?;
        fdt.end_node(node)?;
    }

    fdt.end_node(root)?;
    fdt.finish()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sample() -> Vec<u8> {
        build(&FdtConfig {
            ram: Region {
                base: 0x4000_0000,
                size: 1 << 30,
            },
            cpus: &[0, 1],
            cmdline: "console=hvc0",
            initrd: Some((0x4200_0000, 0x4210_0000)),
            rng_seed: &[7; 32],
            gic_dist: Region {
                base: 0x0800_0000,
                size: 0x1_0000,
            },
            gic_redist: Region {
                base: 0x080a_0000,
                size: 0x4_0000,
            },
            uart: IrqDevice {
                region: Region {
                    base: 0x0c00_0000,
                    size: 0x1000,
                },
                intid: 32,
            },
            rtc: Region {
                base: 0x0c01_0000,
                size: 0x1000,
            },
            virtio: &[IrqDevice {
                region: Region {
                    base: 0x0c10_0000,
                    size: 0x200,
                },
                intid: 48,
            }],
        })
        .unwrap()
    }

    fn contains(haystack: &[u8], needle: &[u8]) -> bool {
        haystack.windows(needle.len()).any(|w| w == needle)
    }

    #[test]
    fn has_a_valid_header_and_the_nodes_the_kernel_needs() {
        let blob = sample();
        assert_eq!(&blob[0..4], &0xd00d_feedu32.to_be_bytes());
        let total = u32::from_be_bytes(blob[4..8].try_into().unwrap()) as usize;
        assert_eq!(total, blob.len());
        for name in [
            &b"chosen\0"[..],
            b"memory@40000000\0",
            b"cpu@0\0",
            b"cpu@1\0",
            b"psci\0",
            b"intc@8000000\0",
            b"timer\0",
            b"pl011@c000000\0",
            b"pl031@c010000\0",
            b"virtio_mmio@c100000\0",
            b"console=hvc0\0",
            b"arm,gic-v3\0",
        ] {
            assert!(
                contains(&blob, name),
                "missing {:?}",
                String::from_utf8_lossy(name)
            );
        }
    }

    #[test]
    fn numbers_spis_relative_to_32() {
        let blob = sample();
        let cells: Vec<u8> = [0u32, 16, 4].iter().flat_map(|c| c.to_be_bytes()).collect();
        assert!(contains(&blob, &cells));
    }

    #[test]
    fn is_deterministic() {
        assert_eq!(sample(), sample());
    }
}
