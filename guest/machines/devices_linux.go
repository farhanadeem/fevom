package machines

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

type devNode struct {
	Path  string `json:"path"`
	Major uint32 `json:"major"`
	Minor uint32 `json:"minor"`
	Mode  uint32 `json:"mode"`
}

var machineDevices = []string{
	"null", "zero", "full", "random", "urandom", "tty",
	"net/tun", "fuse", "kmsg", "loop-control", "btrfs-control", "mapper/control", "vsock",
}

func devNodes(dev string) (nodes []devNode, missing []string) {
	for _, p := range machineDevices {
		var st unix.Stat_t
		if err := unix.Stat(filepath.Join(dev, p), &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFCHR {
			missing = append(missing, p)
			continue
		}
		nodes = append(nodes, devNode{Path: p, Major: unix.Major(uint64(st.Rdev)), Minor: unix.Minor(uint64(st.Rdev)), Mode: st.Mode & 0o7777})
	}
	return nodes, missing
}

func diskMajors(sysBlock string) ([]uint32, error) {
	entries, err := os.ReadDir(sysBlock)
	if err != nil {
		return nil, err
	}
	seen := map[uint32]bool{}
	var majors []uint32
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "vd") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(sysBlock, e.Name(), "dev"))
		if err != nil {
			continue
		}
		maj, _, _ := strings.Cut(strings.TrimSpace(string(b)), ":")
		m, err := strconv.ParseUint(maj, 10, 32)
		if err == nil && !seen[uint32(m)] {
			seen[uint32(m)] = true
			majors = append(majors, uint32(m))
		}
	}
	if len(majors) == 0 {
		return nil, errors.New("the VM has no vd* disk")
	}
	return majors, nil
}

const procDevices = "/proc/devices"

func blockMajor(path, driver string) (uint32, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	return parseBlockMajor(string(b), driver)
}

func slicesContains(s []uint32, v uint32) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func insn(code uint8, dst, src uint8, off int16, imm int32) []byte {
	b := make([]byte, 8)
	b[0] = code
	b[1] = src<<4 | dst
	binary.LittleEndian.PutUint16(b[2:], uint16(off))
	binary.LittleEndian.PutUint32(b[4:], uint32(imm))
	return b
}

func deviceProgram(majors []uint32) []byte {
	var p []byte
	p = append(p, insn(0x61, 2, 1, 0, 0)...)
	p = append(p, insn(0x54, 2, 0, 0, 0xffff)...)
	p = append(p, insn(0x61, 3, 1, 4, 0)...)
	p = append(p, insn(0xb7, 0, 0, 0, 1)...)
	p = append(p, insn(0x55, 2, 0, int16(len(majors)+2), 1)...)
	for i, m := range majors {
		p = append(p, insn(0x15, 3, 0, int16(len(majors)-i), int32(m))...)
	}
	p = append(p, insn(0x05, 0, 0, 1, 0)...)
	p = append(p, insn(0xb7, 0, 0, 0, 0)...)
	p = append(p, insn(0x95, 0, 0, 0, 0)...)
	return p
}

func attachDeviceProgram(group string, majors []uint32) error {
	fd, err := loadDeviceProgram(majors)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	cg, err := unix.Open(group, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(cg)
	at := make([]byte, 128)
	binary.LittleEndian.PutUint32(at[0:], uint32(cg))
	binary.LittleEndian.PutUint32(at[4:], uint32(fd))
	binary.LittleEndian.PutUint32(at[8:], 6)
	binary.LittleEndian.PutUint32(at[12:], 2)
	if _, _, errno := unix.Syscall(unix.SYS_BPF, 8, uintptr(unsafe.Pointer(&at[0])), uintptr(len(at))); errno != 0 {
		return fmt.Errorf("attach the device program: %v", errno)
	}
	return nil
}

var progLoadTries = 100

func blockSignals() (restore func(), err error) {
	var all, old unix.Sigset_t
	for i := range all.Val {
		all.Val[i] = ^uint64(0)
	}
	if err := unix.PthreadSigmask(unix.SIG_BLOCK, &all, &old); err != nil {
		return nil, err
	}
	return func() { unix.PthreadSigmask(unix.SIG_SETMASK, &old, nil) }, nil
}

func loadDeviceProgram(majors []uint32) (int, error) {
	p := deviceProgram(majors)
	license := []byte("GPL\x00")
	logBuf := make([]byte, 16384)
	var name [16]byte
	copy(name[:], "fev_machdev")
	type progLoad struct {
		ProgType    uint32
		InsnCnt     uint32
		Insns       uint64
		License     uint64
		LogLevel    uint32
		LogSize     uint32
		LogBuf      uint64
		KernVersion uint32
		ProgFlags   uint32
		ProgName    [16]byte
	}
	attr := progLoad{
		ProgType: 15,
		InsnCnt:  uint32(len(p) / 8),
		Insns:    uint64(uintptr(unsafe.Pointer(&p[0]))),
		License:  uint64(uintptr(unsafe.Pointer(&license[0]))),
		LogLevel: 1, LogSize: uint32(len(logBuf)),
		LogBuf:   uint64(uintptr(unsafe.Pointer(&logBuf[0]))),
		ProgName: name,
	}
	buf := make([]byte, 128)
	copy(buf, unsafe.Slice((*byte)(unsafe.Pointer(&attr)), unsafe.Sizeof(attr)))
	var fd uintptr
	var errno unix.Errno
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if restore, err := blockSignals(); err == nil {
		defer restore()
	}
	for try := 0; try < progLoadTries; try++ {
		fd, _, errno = unix.Syscall(unix.SYS_BPF, 5, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if errno != unix.EAGAIN {
			break
		}
	}
	runtime.KeepAlive(p)
	runtime.KeepAlive(license)
	runtime.KeepAlive(logBuf)
	if errno != 0 {
		return -1, fmt.Errorf("load the device program: %v: %s", errno, strings.TrimRight(string(logBuf), "\x00"))
	}
	return int(fd), nil
}
