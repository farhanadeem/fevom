package spawn

/*
#include <errno.h>
#include <signal.h>
#include <spawn.h>
#include <stdlib.h>

static int fev_spawn(pid_t *pid, const char *path, char *const argv[], char *const envp[], const char *dir, int in, int out, int err) {
	posix_spawn_file_actions_t fa;
	posix_spawnattr_t at;
	int r = posix_spawn_file_actions_init(&fa);
	if (r != 0) return r;
	r = posix_spawnattr_init(&at);
	if (r != 0) { posix_spawn_file_actions_destroy(&fa); return r; }
	sigset_t none, all;
	sigemptyset(&none);
	sigfillset(&all);
	if ((r = posix_spawn_file_actions_adddup2(&fa, in, 0)) == 0 &&
	    (r = posix_spawn_file_actions_adddup2(&fa, out, 1)) == 0 &&
	    (r = posix_spawn_file_actions_adddup2(&fa, err, 2)) == 0 &&
	    (r = posix_spawn_file_actions_addchdir(&fa, dir)) == 0 &&
	    (r = posix_spawnattr_setflags(&at, POSIX_SPAWN_SETPGROUP | POSIX_SPAWN_SETSIGMASK | POSIX_SPAWN_SETSIGDEF | POSIX_SPAWN_CLOEXEC_DEFAULT)) == 0 &&
	    (r = posix_spawnattr_setpgroup(&at, 0)) == 0 &&
	    (r = posix_spawnattr_setsigmask(&at, &none)) == 0 &&
	    (r = posix_spawnattr_setsigdefault(&at, &all)) == 0) {
		r = posix_spawn(pid, path, &fa, &at, argv, envp);
	}
	posix_spawnattr_destroy(&at);
	posix_spawn_file_actions_destroy(&fa);
	return r;
}
*/
import "C"

import (
	"os"
	"syscall"
	"unsafe"
)

func cstrings(ss []string) (**C.char, func()) {
	arr := (*[1 << 28]*C.char)(C.malloc(C.size_t(len(ss)+1) * C.size_t(unsafe.Sizeof((*C.char)(nil)))))[: len(ss)+1 : len(ss)+1]
	for i, s := range ss {
		arr[i] = C.CString(s)
	}
	arr[len(ss)] = nil
	return &arr[0], func() {
		for _, p := range arr[:len(ss)] {
			C.free(unsafe.Pointer(p))
		}
		C.free(unsafe.Pointer(&arr[0]))
	}
}

func Start(path string, argv, env []string, dir string, files [3]*os.File) (*os.Process, error) {
	cpath, cdir := C.CString(path), C.CString(dir)
	defer C.free(unsafe.Pointer(cpath))
	defer C.free(unsafe.Pointer(cdir))
	cargv, freeArgv := cstrings(argv)
	defer freeArgv()
	cenv, freeEnv := cstrings(env)
	defer freeEnv()
	var pid C.pid_t
	r := C.fev_spawn(&pid, cpath, cargv, cenv, cdir, C.int(files[0].Fd()), C.int(files[1].Fd()), C.int(files[2].Fd()))
	if r != 0 {
		return nil, &os.PathError{Op: "posix_spawn", Path: path, Err: syscall.Errno(r)}
	}
	return os.FindProcess(int(pid))
}
