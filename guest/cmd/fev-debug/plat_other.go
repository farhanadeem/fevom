//go:build !linux

package main

import (
	"fmt"
	"os"
	"os/exec"
)

func isatty(fd uintptr) bool {
	fi, err := os.Stdin.Stat()
	_ = fd
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func execCommand(cmd string, args []string) error {
	c := exec.Command(cmd, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s: %w", cmd, err)
	}
	return nil
}
