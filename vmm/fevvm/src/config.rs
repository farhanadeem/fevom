use std::os::fd::RawFd;
use std::path::PathBuf;

pub const USAGE: &str = "\
usage: fevvm --kernel IMAGE [options]

  --kernel PATH        uncompressed arm64 kernel Image
  --flat PATH          instead of a kernel: a flat bare-metal binary, loaded
                       0x80000 into RAM and entered at its first byte, on a
                       machine with no virtio devices (console on the UART)
  --initrd PATH        initial ramdisk (cpio, optionally gzip)
  --cmdline STRING     kernel command line (default: console=hvc0)
  --cpus N             number of vCPUs (default: 2)
  --mem MIB            guest memory ceiling in MiB (default: 1024)
  --disk PATH[,ro|,swap]  raw disk image; repeat for /dev/vda, /dev/vdb, ...
                       swap: a disk the guest swaps on, outside the data
                       disk's free-space check and its held writes
  --disk-sync MODE     what a guest flush does on the host: fsync (to the
                       filesystem; lost on power failure), barrier (ordered
                       on the drive), full (on the drive), or paced (the
                       default: barrier, and the first flush a second or
                       more after the last full flush is full)
  --rosetta            attach the installed Rosetta for Linux as a read-only share
  --rosetta-optional   attach Rosetta if usable; otherwise boot with FEX available
  --share SPEC         share a Mac directory with the guest over virtio-fs;
                       repeatable. SPEC is comma-separated:
                         tag=NAME    the name the guest mounts (required)
                         path=DIR    the Mac directory (required)
                         ro          refuse writes
                         sync=MODE   what a guest fsync does: fsync, barrier or
                                     full; follows --disk-sync when not given,
                                     with paced read as barrier
                         deny=PATH   hide this path from the guest; repeatable
                       A path cannot contain a comma. Files the Mac user owns
                       belong, in the guest, to whoever asks.
  --net PATH           unix socket of the netstack; enables the network device
  --vsock-dir DIR      directory for vsock unix sockets; enables the vsock device.
                       A guest connecting to host port Q reaches DIR/host_Q.sock,
                       where a host process may listen; no flag is needed
  --vsock-port P       expose guest port P at DIR/vsock_P.sock; repeatable
  --console stdio|none where the guest console goes (default: stdio)
  --stamp              prefix console lines with milliseconds since start
  --no-reboot          exit when the guest asks to reset, instead of restarting
  --no-window          do not map the first writable disk's image into the
                       guest for its driver to read from: every read is a
                       request to the VMM
  --verbose            log VMM events to stderr
  --trace-fd N         write boot marks to this inherited descriptor, one JSON
                       object per line, timed in ns on CLOCK_UPTIME_RAW; the
                       first line carries the build identifier
  --lifeline-fd N      stop the VM cleanly, as if the guest had powered off,
                       when this inherited descriptor reaches end of file
  --control-fd N       serve pause, resume, stats, stop and kill on this
                       inherited socket, as JSON-RPC, one object a line
                       (proto/vmmctl); a guest reset is told there
  --version            print the build identifier
  --help               show this text

With the console on a terminal, Ctrl-A x stops the VM.
SIGUSR1 (or Ctrl-T) prints what the VM's memory costs the host.
FEV_VMM_IRQ_STEER=off in the environment leaves a device's interrupt on the
CPU the guest routes it to; otherwise it follows the CPU that sent a request.
Exit status: 0 guest powered off or VM stopped, 1 VMM or guest fault, 2 bad usage.";

pub const DEFAULT_CMDLINE: &str = "console=hvc0";
const MIN_MEM_MIB: u64 = 64;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DiskConfig {
    pub path: PathBuf,
    pub read_only: bool,
    pub swap: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ConsoleMode {
    Stdio,
    None,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum SyncMode {
    Fsync,
    Barrier,
    Full,
    #[default]
    Paced,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ShareConfig {
    pub tag: String,
    pub path: PathBuf,
    pub read_only: bool,
    pub sync: Option<SyncMode>,
    pub deny: Vec<PathBuf>,
}

fn sync_mode(flag: &str, v: &str) -> Result<SyncMode, ParseError> {
    match v {
        "fsync" => Ok(SyncMode::Fsync),
        "barrier" => Ok(SyncMode::Barrier),
        "full" => Ok(SyncMode::Full),
        "paced" => Ok(SyncMode::Paced),
        other => bad(format!("{flag}: unknown mode '{other}'")),
    }
}

impl ShareConfig {
    fn parse(spec: &str) -> Result<Self, ParseError> {
        let (mut tag, mut path) = (None, None);
        let mut share = ShareConfig {
            tag: String::new(),
            path: PathBuf::new(),
            read_only: false,
            sync: None,
            deny: Vec::new(),
        };
        for part in spec.split(',') {
            match part.split_once('=') {
                Some(("tag", v)) if tag.is_none() => tag = Some(v),
                Some(("path", v)) if path.is_none() => path = Some(v),
                Some(("tag" | "path", _)) => {
                    return bad(format!("--share {spec}: tag and path are given once each"))
                }
                Some(("sync", v)) => {
                    share.sync = match sync_mode("--share sync", v)? {
                        SyncMode::Paced => {
                            return bad(format!(
                                "--share {spec}: sync takes fsync, barrier or full"
                            ))
                        }
                        mode => Some(mode),
                    }
                }
                Some(("deny", v)) if !v.is_empty() => share.deny.push(v.into()),
                None if part == "ro" => share.read_only = true,
                _ => return bad(format!("--share {spec}: unknown option '{part}'")),
            }
        }
        match tag {
            Some(t)
                if !t.is_empty() && t.len() <= 36 && t.bytes().all(|b| b.is_ascii_graphic()) =>
            {
                share.tag = t.into()
            }
            _ => {
                return bad(format!(
                    "--share {spec}: tag= must be 1 to 36 printable ASCII characters"
                ))
            }
        }
        match path {
            Some(p) if p.starts_with('/') => share.path = p.into(),
            _ => return bad(format!("--share {spec}: path= must be an absolute path")),
        }
        if share.deny.iter().any(|d| !d.is_absolute()) {
            return bad(format!("--share {spec}: deny= must be an absolute path"));
        }
        Ok(share)
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Config {
    pub kernel: PathBuf,
    pub flat: Option<PathBuf>,
    pub initrd: Option<PathBuf>,
    pub cmdline: String,
    pub cpus: u32,
    pub mem_mib: u64,
    pub disks: Vec<DiskConfig>,
    pub disk_sync: SyncMode,
    pub shares: Vec<ShareConfig>,
    pub rosetta: bool,
    pub rosetta_optional: bool,
    pub net: Option<PathBuf>,
    pub vsock_dir: Option<PathBuf>,
    pub vsock_ports: Vec<u32>,
    pub console: ConsoleMode,
    pub stamp: bool,
    pub no_reboot: bool,
    pub no_window: bool,
    pub verbose: bool,
    pub trace_fd: Option<RawFd>,
    pub lifeline_fd: Option<RawFd>,
    pub control_fd: Option<RawFd>,
}

#[derive(Debug, PartialEq, Eq)]
pub enum ParseError {
    Help,
    Version,
    Bad(String),
}

fn bad<T>(msg: impl Into<String>) -> Result<T, ParseError> {
    Err(ParseError::Bad(msg.into()))
}

impl Config {
    pub fn parse<I: IntoIterator<Item = String>>(args: I) -> Result<Self, ParseError> {
        let mut kernel = None;
        let mut cfg = Config {
            kernel: PathBuf::new(),
            flat: None,
            initrd: None,
            cmdline: DEFAULT_CMDLINE.into(),
            cpus: 2,
            mem_mib: 1024,
            disks: Vec::new(),
            disk_sync: SyncMode::default(),
            shares: Vec::new(),
            rosetta: false,
            rosetta_optional: false,
            net: None,
            vsock_dir: None,
            vsock_ports: Vec::new(),
            console: ConsoleMode::Stdio,
            stamp: false,
            no_reboot: false,
            no_window: false,
            verbose: false,
            trace_fd: None,
            lifeline_fd: None,
            control_fd: None,
        };

        let mut args = args.into_iter();
        while let Some(arg) = args.next() {
            let mut value = |name: &str| {
                args.next()
                    .ok_or_else(|| ParseError::Bad(format!("{name} needs a value")))
            };
            match arg.as_str() {
                "--help" | "-h" => return Err(ParseError::Help),
                "--version" => return Err(ParseError::Version),
                "--kernel" => kernel = Some(PathBuf::from(value("--kernel")?)),
                "--flat" => cfg.flat = Some(PathBuf::from(value("--flat")?)),
                "--initrd" => cfg.initrd = Some(PathBuf::from(value("--initrd")?)),
                "--cmdline" => cfg.cmdline = value("--cmdline")?,
                "--cpus" => {
                    let v = value("--cpus")?;
                    cfg.cpus = match v.parse() {
                        Ok(n) if n >= 1 => n,
                        _ => return bad(format!("--cpus: '{v}' is not a positive number")),
                    };
                }
                "--mem" => {
                    let v = value("--mem")?;
                    cfg.mem_mib = match v.parse() {
                        Ok(n) if n >= MIN_MEM_MIB => n,
                        _ => {
                            return bad(format!(
                                "--mem: '{v}' is not a number of MiB of at least {MIN_MEM_MIB}"
                            ))
                        }
                    };
                }
                "--disk" => {
                    let v = value("--disk")?;
                    let (path, read_only, swap) = if let Some(p) = v.strip_suffix(",ro") {
                        (p, true, false)
                    } else if let Some(p) = v.strip_suffix(",swap") {
                        (p, false, true)
                    } else {
                        (v.as_str(), false, false)
                    };
                    if path.is_empty() {
                        return bad("--disk needs a path");
                    }
                    cfg.disks.push(DiskConfig {
                        path: path.into(),
                        read_only,
                        swap,
                    });
                }
                "--disk-sync" => cfg.disk_sync = sync_mode("--disk-sync", &value("--disk-sync")?)?,
                "--rosetta" => cfg.rosetta = true,
                "--rosetta-optional" => {
                    cfg.rosetta = true;
                    cfg.rosetta_optional = true;
                }
                "--share" => {
                    let share = ShareConfig::parse(&value("--share")?)?;
                    if share.tag == fileshare::rosetta::TAG {
                        return bad("--share: tag 'rosetta' is reserved; use --rosetta");
                    }
                    if cfg.shares.iter().any(|s| s.tag == share.tag) {
                        return bad(format!("--share: tag '{}' is given twice", share.tag));
                    }
                    cfg.shares.push(share);
                }
                "--trace-fd" => {
                    cfg.trace_fd = Some(inherited_fd("--trace-fd", &value("--trace-fd")?)?)
                }
                "--lifeline-fd" => {
                    cfg.lifeline_fd = Some(inherited_fd("--lifeline-fd", &value("--lifeline-fd")?)?)
                }
                "--control-fd" => {
                    cfg.control_fd = Some(inherited_fd("--control-fd", &value("--control-fd")?)?)
                }
                "--net" => cfg.net = Some(PathBuf::from(value("--net")?)),
                "--vsock-dir" => cfg.vsock_dir = Some(PathBuf::from(value("--vsock-dir")?)),
                "--vsock-port" => {
                    let v = value("--vsock-port")?;
                    match v.parse() {
                        Ok(p) if cfg.vsock_ports.contains(&p) => {
                            return bad(format!("--vsock-port {p} is given twice"))
                        }
                        Ok(p) => cfg.vsock_ports.push(p),
                        Err(_) => return bad(format!("--vsock-port: '{v}' is not a port number")),
                    }
                }
                "--console" => {
                    cfg.console = match value("--console")?.as_str() {
                        "stdio" => ConsoleMode::Stdio,
                        "none" => ConsoleMode::None,
                        other => return bad(format!("--console: unknown mode '{other}'")),
                    };
                }
                "--stamp" => cfg.stamp = true,
                "--no-reboot" => cfg.no_reboot = true,
                "--no-window" => cfg.no_window = true,
                "--verbose" => cfg.verbose = true,
                other => return bad(format!("unknown argument '{other}'")),
            }
        }

        cfg.kernel = match (kernel, &cfg.flat) {
            (Some(_), Some(_)) => return bad("--kernel and --flat are alternatives"),
            (Some(k), None) => k,
            (None, Some(_))
                if !cfg.disks.is_empty()
                    || cfg.net.is_some()
                    || cfg.vsock_dir.is_some()
                    || !cfg.shares.is_empty()
                    || cfg.rosetta =>
            {
                return bad(
                    "--flat runs on a bare machine: no --disk, --net, --vsock-dir, or --share",
                )
            }
            (None, Some(_)) => PathBuf::new(),
            (None, None) => return bad("--kernel is required"),
        };
        if cfg.vsock_dir.is_none() && !cfg.vsock_ports.is_empty() {
            return bad("--vsock-port needs --vsock-dir");
        }
        let fds = [cfg.trace_fd, cfg.lifeline_fd, cfg.control_fd];
        for (i, fd) in fds.iter().enumerate() {
            if fd.is_some() && fds[..i].contains(fd) {
                return bad(
                    "--trace-fd, --lifeline-fd and --control-fd must be different descriptors",
                );
            }
        }
        if cfg.cmdline.contains('\0') {
            return bad("--cmdline must not contain NUL");
        }
        Ok(cfg)
    }
}

fn inherited_fd(name: &str, v: &str) -> Result<RawFd, ParseError> {
    match v.parse::<RawFd>() {
        Ok(fd) if fd > 2 => Ok(fd),
        _ => bad(format!("{name}: '{v}' is not a descriptor number above 2")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parse(args: &[&str]) -> Result<Config, ParseError> {
        Config::parse(args.iter().map(|s| s.to_string()))
    }

    #[test]
    fn applies_defaults() {
        let c = parse(&["--kernel", "Image"]).unwrap();
        assert_eq!(c.cpus, 2);
        assert_eq!(c.mem_mib, 1024);
        assert_eq!(c.cmdline, "console=hvc0");
        assert_eq!(c.console, ConsoleMode::Stdio);
        assert!(c.disks.is_empty() && c.initrd.is_none());
        assert_eq!(c.disk_sync, SyncMode::Paced);
        assert_eq!((c.trace_fd, c.lifeline_fd), (None, None));
        assert_eq!(c.control_fd, None);
    }

    #[test]
    fn rosetta_is_an_explicit_dedicated_device() {
        assert!(!parse(&["--kernel", "k"]).unwrap().rosetta);
        assert!(parse(&["--kernel", "k", "--rosetta"]).unwrap().rosetta);
        let optional = parse(&["--kernel", "k", "--rosetta-optional"]).unwrap();
        assert!(optional.rosetta && optional.rosetta_optional);
        assert!(
            !parse(&["--kernel", "k", "--rosetta"])
                .unwrap()
                .rosetta_optional
        );
        assert!(parse(&["--flat", "f", "--rosetta"]).is_err());
        assert!(parse(&["--flat", "f", "--rosetta-optional"]).is_err());
        assert!(parse(&["--kernel", "k", "--share", "tag=rosetta,path=/tmp,ro"]).is_err());
    }

    #[test]
    fn reads_a_share() {
        let c = parse(&[
            "--kernel",
            "k",
            "--share",
            "tag=mac,path=/",
            "--share",
            "path=/tmp/x y,tag=scratch,ro,sync=full,deny=/tmp/x y/data,deny=/a",
        ])
        .unwrap();
        assert_eq!(
            c.shares[0],
            ShareConfig {
                tag: "mac".into(),
                path: "/".into(),
                read_only: false,
                sync: None,
                deny: vec![],
            }
        );
        assert_eq!(
            c.shares[1],
            ShareConfig {
                tag: "scratch".into(),
                path: "/tmp/x y".into(),
                read_only: true,
                sync: Some(SyncMode::Full),
                deny: vec!["/tmp/x y/data".into(), "/a".into()],
            }
        );
        assert!(parse(&["--kernel", "k"]).unwrap().shares.is_empty());

        let long = format!("tag={},path=/", "t".repeat(37));
        for spec in [
            "",
            "tag=mac",
            "path=/",
            "tag=,path=/",
            "tag=mac,path=relative",
            "tag=mac,path=/,rw",
            "tag=mac,path=/,sync=never",
            "tag=mac,path=/,deny=relative",
            "tag=mac,path=/,deny=",
            "tag=mac,path=/,owner=real",
            "tag=mac,tag=again,path=/",
            "tag=mac,path=/,path=/x",
            "tag=has space,path=/",
            long.as_str(),
        ] {
            assert!(
                matches!(
                    parse(&["--kernel", "k", "--share", spec]),
                    Err(ParseError::Bad(_))
                ),
                "'{spec}' should be refused"
            );
        }
        let twice = [
            "--kernel",
            "k",
            "--share",
            "tag=a,path=/",
            "--share",
            "tag=a,path=/tmp",
        ];
        assert!(matches!(parse(&twice), Err(ParseError::Bad(m)) if m.contains("twice")));
        assert!(parse(&["--flat", "f", "--share", "tag=a,path=/"]).is_err());
        assert!(parse(&["--kernel", "k", "--share"]).is_err());
    }

    #[test]
    fn reads_every_flag() {
        let c = parse(&[
            "--kernel",
            "k",
            "--initrd",
            "i",
            "--cmdline",
            "a b",
            "--cpus",
            "10",
            "--mem",
            "4096",
            "--disk",
            "root.img",
            "--disk",
            "data.img,ro",
            "--disk",
            "swap.img,swap",
            "--console",
            "none",
            "--stamp",
            "--no-reboot",
            "--verbose",
            "--net",
            "/tmp/n.sock",
            "--vsock-dir",
            "/tmp/v",
            "--vsock-port",
            "1024",
            "--vsock-port",
            "22",
            "--disk-sync",
            "full",
            "--trace-fd",
            "3",
            "--lifeline-fd",
            "4",
            "--control-fd",
            "6",
        ])
        .unwrap();
        assert_eq!(c.disk_sync, SyncMode::Full);
        assert_eq!((c.trace_fd, c.lifeline_fd), (Some(3), Some(4)));
        assert_eq!(c.control_fd, Some(6));
        assert_eq!((c.cpus, c.mem_mib), (10, 4096));
        assert_eq!(c.cmdline, "a b");
        assert_eq!(c.initrd, Some("i".into()));
        assert_eq!(
            c.disks,
            vec![
                DiskConfig {
                    path: "root.img".into(),
                    read_only: false,
                    swap: false
                },
                DiskConfig {
                    path: "data.img".into(),
                    read_only: true,
                    swap: false
                },
                DiskConfig {
                    path: "swap.img".into(),
                    read_only: false,
                    swap: true
                },
            ]
        );
        assert_eq!(c.console, ConsoleMode::None);
        assert!(c.stamp && c.no_reboot && c.verbose);
        assert_eq!(c.net, Some("/tmp/n.sock".into()));
        assert_eq!(c.vsock_dir, Some("/tmp/v".into()));
        assert_eq!(c.vsock_ports, vec![1024, 22]);
    }

    #[test]
    fn a_flat_binary_stands_in_for_the_kernel() {
        let c = parse(&["--flat", "test.flat", "--cmdline", "ipi"]).unwrap();
        assert_eq!(c.flat, Some("test.flat".into()));
        assert_eq!(c.cmdline, "ipi");
    }

    #[test]
    fn reads_each_disk_sync_mode() {
        for (name, mode) in [
            ("fsync", SyncMode::Fsync),
            ("barrier", SyncMode::Barrier),
            ("full", SyncMode::Full),
            ("paced", SyncMode::Paced),
        ] {
            let c = parse(&["--kernel", "k", "--disk-sync", name]).unwrap();
            assert_eq!(c.disk_sync, mode);
        }
    }

    #[test]
    fn version_is_asked_for_like_help() {
        assert_eq!(parse(&["--version"]), Err(ParseError::Version));
        assert_eq!(
            parse(&["--kernel", "k", "--version"]),
            Err(ParseError::Version)
        );
    }

    #[test]
    fn rejects_bad_input_with_a_reason() {
        assert_eq!(parse(&["--help"]), Err(ParseError::Help));
        for args in [
            &["--cpus", "2"][..],
            &["--kernel"],
            &["--kernel", "k", "--cpus", "0"],
            &["--kernel", "k", "--cpus", "many"],
            &["--kernel", "k", "--mem", "16"],
            &["--kernel", "k", "--disk", ",ro"],
            &["--kernel", "k", "--disk", ",swap"],
            &["--kernel", "k", "--console", "tcp"],
            &["--kernel", "k", "--frobnicate"],
            &["--kernel", "k", "--flat", "f"],
            &["--flat", "f", "--disk", "d"],
            &["--flat", "f", "--net", "n"],
            &["--kernel", "k", "--vsock-port", "5"],
            &["--kernel", "k", "--disk-sync", "never"],
            &["--kernel", "k", "--disk-sync"],
            &["--kernel", "k", "--share", "tag=t,path=/tmp,sync=paced"],
            &["--kernel", "k", "--trace-fd", "1"],
            &["--kernel", "k", "--trace-fd", "-3"],
            &["--kernel", "k", "--trace-fd", "pipe"],
            &["--kernel", "k", "--lifeline-fd", "0"],
            &["--kernel", "k", "--trace-fd", "5", "--lifeline-fd", "5"],
            &["--kernel", "k", "--control-fd", "2"],
            &["--kernel", "k", "--control-fd"],
            &["--kernel", "k", "--trace-fd", "5", "--control-fd", "5"],
            &["--kernel", "k", "--lifeline-fd", "4", "--control-fd", "4"],
            &[
                "--kernel",
                "k",
                "--vsock-dir",
                "/tmp/v",
                "--vsock-port",
                "ssh",
            ],
        ] {
            assert!(matches!(parse(args), Err(ParseError::Bad(_))), "{args:?}");
        }
    }
}
