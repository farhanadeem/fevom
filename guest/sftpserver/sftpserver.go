package sftpserver

import (
	"fmt"
	"io"
	"os"

	"github.com/pkg/sftp"
)

type stdio struct {
	io.Reader
	io.Writer
}

func (stdio) Close() error {
	os.Stdout.Close()
	return os.Stdin.Close()
}

func Serve(rwc io.ReadWriteCloser, dir string) error {
	s, err := sftp.NewServer(rwc, sftp.WithServerWorkingDirectory(dir))
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Serve(); err != nil && err != io.EOF {
		return err
	}
	return nil
}

func Main(name string) int {
	dir, err := os.Getwd()
	if err == nil {
		err = Serve(stdio{os.Stdin, os.Stdout}, dir)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		return 1
	}
	return 0
}
