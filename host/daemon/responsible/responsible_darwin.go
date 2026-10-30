package responsible

/*
#include <dlfcn.h>
#include <libproc.h>
#include <sys/types.h>

static pid_t fev_responsible(pid_t pid) {
	static pid_t (*f)(pid_t);
	static int looked;
	if (!looked) {
		f = (pid_t (*)(pid_t))dlsym(RTLD_DEFAULT, "responsibility_get_pid_responsible_for_pid");
		looked = 1;
	}
	return f ? f(pid) : -1;
}
*/
import "C"

import (
	"os"
	"path/filepath"
	"unsafe"
)

func Of(pid int) Who {
	r := int(C.fev_responsible(C.pid_t(pid)))
	if r <= 0 {
		return Who{}
	}
	buf := make([]byte, C.PROC_PIDPATHINFO_MAXSIZE)
	n := C.proc_pidpath(C.int(r), unsafe.Pointer(&buf[0]), C.uint32_t(len(buf)))
	if n <= 0 {
		return Who{PID: r}
	}
	self := ""
	if pid == os.Getpid() {
		if exe, err := os.Executable(); err == nil {
			if real, err := filepath.EvalSymlinks(exe); err == nil {
				self = real
			}
		}
	}
	return Describe(r, string(buf[:n]), self)
}

func Self() Who { return Of(os.Getpid()) }
