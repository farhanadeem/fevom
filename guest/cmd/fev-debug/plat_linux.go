package main

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func isatty(fd uintptr) bool {
	_, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
	return err == nil
}

func execCommand(cmd string, args []string) error {
	path, err := exec.LookPath(cmd)
	if err != nil {
		return err
	}
	argv := append([]string{cmd}, args...)
	return syscall.Exec(path, argv, syscall_Environ())
}

func syscall_Environ() []string { return syscall.Environ() }
