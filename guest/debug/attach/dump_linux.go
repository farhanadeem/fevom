//go:build linux

package attach

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func DumpTargetStack(pid int) ([][]Insn, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const (
		ptraceSeize            = 0x4206
		ptraceInterrupt        = 0x4207
		ptraceSeccompGetFilter = 0x420c
		ptraceDetach           = 17
	)
	status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return nil, fmt.Errorf("the target's status: %w", err)
	}
	mode, filters, err := seccompStatus(status)
	if err != nil {
		return nil, err
	}
	if mode == 0 && filters == 0 {
		return nil, nil
	}
	for try := 0; ; try++ {
		_, _, errno := unix.Syscall6(unix.SYS_PTRACE, ptraceSeize, uintptr(pid), 0, 0, 0, 0)
		if errno == 0 {
			break
		}
		if errno != unix.EPERM || try >= 150 || (try >= 5 && !traced(pid)) {
			return nil, fmt.Errorf("ptrace seize %d: %w", pid, errno)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer unix.Syscall6(unix.SYS_PTRACE, ptraceDetach, uintptr(pid), 0, 0, 0, 0)
	if _, _, errno := unix.Syscall6(unix.SYS_PTRACE, ptraceInterrupt, uintptr(pid), 0, 0, 0, 0); errno != 0 {
		return nil, fmt.Errorf("ptrace interrupt %d: %w", pid, errno)
	}
	var ws unix.WaitStatus
	for {
		_, err := unix.Wait4(pid, &ws, unix.WALL, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("wait on the seized target %d: %w", pid, err)
		}
		break
	}
	if !ws.Stopped() {
		return nil, fmt.Errorf("the seized target %d did not stop (%v)", pid, ws)
	}
	get := func(idx int) ([]Insn, error) {
		n, _, errno := unix.Syscall6(unix.SYS_PTRACE, ptraceSeccompGetFilter, uintptr(pid), uintptr(idx), 0, 0, 0)
		switch {
		case errno == unix.EINVAL && idx == 0:
			return nil, errNoFilter
		case errno == unix.ENOENT:
			return nil, errNoIndex
		case errno != 0:
			return nil, errno
		case int(n) <= 0 || int(n) > 4096:
			return nil, fmt.Errorf("a filter of %d instructions", int(n))
		}
		buf := make([]Insn, int(n))
		m, _, errno := unix.Syscall6(unix.SYS_PTRACE, ptraceSeccompGetFilter, uintptr(pid), uintptr(idx), uintptr(unsafe.Pointer(&buf[0])), 0, 0)
		runtime.KeepAlive(buf)
		if errno != 0 {
			return nil, errno
		}
		if int(m) != len(buf) {
			return nil, errors.New("the filter changed size while it was read")
		}
		return buf, nil
	}
	return readStack(get, mode, filters)
}

func traced(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "TracerPid:"); ok {
			return strings.TrimSpace(v) != "0"
		}
	}
	return false
}
