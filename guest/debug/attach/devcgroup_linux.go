//go:build linux

package attach

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const progLoadTries = 100

func DenyDeviceCgroup(parent, name string) (dir string, procs *os.File, err error) {
	dir = filepath.Join(parent, name)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", nil, fmt.Errorf("make the debug cgroup parent %s: %w", parent, err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
		return "", nil, fmt.Errorf("make the deny-all device cgroup %s: %w", dir, err)
	}
	cleanup := func() { os.Remove(dir) }
	fd, err := loadDenyDeviceProgram()
	if err != nil {
		cleanup()
		return "", nil, err
	}
	defer unix.Close(fd)
	cg, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("open the device cgroup %s: %w", dir, err)
	}
	defer unix.Close(cg)
	at := make([]byte, 128)
	binary.LittleEndian.PutUint32(at[0:], uint32(cg))
	binary.LittleEndian.PutUint32(at[4:], uint32(fd))
	binary.LittleEndian.PutUint32(at[8:], 6)
	binary.LittleEndian.PutUint32(at[12:], 0)
	if _, _, errno := unix.Syscall(unix.SYS_BPF, 8, uintptr(unsafe.Pointer(&at[0])), uintptr(len(at))); errno != 0 {
		cleanup()
		return "", nil, fmt.Errorf("attach the deny-all device program: %v", errno)
	}
	procs, err = os.OpenFile(filepath.Join(dir, "cgroup.procs"), os.O_WRONLY, 0)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("open %s/cgroup.procs: %w", dir, err)
	}
	return dir, procs, nil
}

func loadDenyDeviceProgram() (int, error) {
	p := denyDeviceProgram()
	license := []byte("GPL\x00")
	logBuf := make([]byte, 16384)
	var name [16]byte
	copy(name[:], "fev_dbgdeny")
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
	if restore, err := blockAllSignals(); err == nil {
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
		return -1, fmt.Errorf("load the deny-all device program: %v", errno)
	}
	return int(fd), nil
}

func blockAllSignals() (restore func(), err error) {
	var all, old unix.Sigset_t
	for i := range all.Val {
		all.Val[i] = ^uint64(0)
	}
	if err := unix.PthreadSigmask(unix.SIG_BLOCK, &all, &old); err != nil {
		return nil, err
	}
	return func() { unix.PthreadSigmask(unix.SIG_SETMASK, &old, nil) }, nil
}
