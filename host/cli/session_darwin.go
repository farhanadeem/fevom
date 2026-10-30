//go:build darwin

package cli

import (
	"syscall"
	"unsafe"
)

func GraphicAccess() bool {
	var info [48]byte
	const sysGetauditAddr = 357
	const hasGraphicAccess = 0x0010
	if _, _, e := syscall.Syscall(sysGetauditAddr, uintptr(unsafe.Pointer(&info[0])), uintptr(len(info)), 0); e != 0 {
		return true
	}
	return *(*uint64)(unsafe.Pointer(&info[40]))&hasGraphicAccess != 0
}
