package attach

import "strings"

const (
	CapSysAdmin  = 21
	CapSysPtrace = 19
	CapMknod     = 27
)

const DockerDefaultCaps uint64 = 0x00000000a80425fb

func RunningCaps(targetBounding uint64) uint64 {
	return targetBounding | (1 << CapSysPtrace)
}

func StoppedImageCaps(inspectBounding uint64, hasInspect bool) uint64 {
	fixed := (DockerDefaultCaps &^ (1 << CapSysAdmin) &^ (1 << CapMknod)) | (1 << CapSysPtrace)
	if hasInspect {
		fixed = (fixed & (inspectBounding | (1 << CapSysPtrace)))
	}
	return fixed
}

func DropList(keep uint64) []int {
	var drop []int
	for cap := 0; cap < 64; cap++ {
		if keep&(1<<uint(cap)) == 0 {
			drop = append(drop, cap)
		}
	}
	return drop
}

var capBit = map[string]uint{
	"CHOWN": 0, "DAC_OVERRIDE": 1, "DAC_READ_SEARCH": 2, "FOWNER": 3, "FSETID": 4,
	"KILL": 5, "SETGID": 6, "SETUID": 7, "SETPCAP": 8, "LINUX_IMMUTABLE": 9,
	"NET_BIND_SERVICE": 10, "NET_BROADCAST": 11, "NET_ADMIN": 12, "NET_RAW": 13,
	"IPC_LOCK": 14, "IPC_OWNER": 15, "SYS_MODULE": 16, "SYS_RAWIO": 17,
	"SYS_CHROOT": 18, "SYS_PTRACE": CapSysPtrace, "SYS_PACCT": 20,
	"SYS_ADMIN": CapSysAdmin, "SYS_BOOT": 22, "SYS_NICE": 23, "SYS_RESOURCE": 24,
	"SYS_TIME": 25, "SYS_TTY_CONFIG": 26, "MKNOD": CapMknod, "LEASE": 28,
	"AUDIT_WRITE": 29, "AUDIT_CONTROL": 30, "SETFCAP": 31, "MAC_OVERRIDE": 32,
	"MAC_ADMIN": 33, "SYSLOG": 34, "WAKE_ALARM": 35, "BLOCK_SUSPEND": 36,
	"AUDIT_READ": 37, "PERFMON": 38, "BPF": 39, "CHECKPOINT_RESTORE": 40,
}

func allCapsMask() uint64 {
	var m uint64
	for _, b := range capBit {
		m |= 1 << b
	}
	return m
}

func capsFromInspect(add, drop []string) (uint64, bool) {
	if len(add) == 0 && len(drop) == 0 {
		return DockerDefaultCaps, true
	}
	caps := DockerDefaultCaps
	apply := func(names []string, set bool) {
		for _, n := range names {
			n = strings.ToUpper(strings.TrimPrefix(strings.ToUpper(n), "CAP_"))
			if n == "ALL" {
				if set {
					caps = allCapsMask()
				} else {
					caps = 0
				}
				continue
			}
			if b, okName := capBit[n]; okName {
				if set {
					caps |= 1 << b
				} else {
					caps &^= 1 << b
				}
			}
		}
	}
	apply(drop, false)
	apply(add, true)
	return caps, true
}
