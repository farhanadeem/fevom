package isolate

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

func Filter() []unix.SockFilter {
	const (
		ld   = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeq  = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		ret  = unix.BPF_RET | unix.BPF_K
		deny = unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)
	)
	return []unix.SockFilter{
		{Code: ld, K: 4},
		{Code: jeq, Jt: 1, Jf: 0, K: unix.AUDIT_ARCH_AARCH64},
		{Code: ret, K: deny},
		{Code: ld, K: 0},
		{Code: jeq, Jt: 3, Jf: 0, K: unix.SYS_IO_URING_SETUP},
		{Code: jeq, Jt: 0, Jf: 3, K: unix.SYS_SOCKET},
		{Code: ld, K: 16},
		{Code: jeq, Jt: 0, Jf: 1, K: unix.AF_VSOCK},
		{Code: ret, K: deny},
		{Code: ret, K: unix.SECCOMP_RET_ALLOW},
	}
}

func Install() error {
	f := Filter()
	prog := unix.SockFprog{Len: uint16(len(f)), Filter: &f[0]}
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&prog)))
	if errno != 0 {
		return fmt.Errorf("the isolated machine's seccomp filter: %w", errno)
	}
	return nil
}
