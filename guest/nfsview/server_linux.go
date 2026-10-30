package nfsview

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const NFSDDir = "/proc/fs/nfsd"

const MacNamesParam = "/sys/module/nfsd/parameters/fev_refuse_mac_names"

var OwnerChangeParam = "/sys/module/nfsd/parameters/fev_refuse_owner_change"

var ErrNoOwnerChangeParam = errors.New("this kernel has no " + OwnerChangeParam + " (patch 9010): the view is not started, as a Mac chown, chgrp or cp -p would change owners in the guest")

type Server struct {
	Dir            string
	Threads        int
	Port           int
	LeaseSeconds   int
	RefuseMacNames bool
	VsockPort      uint32

	vsock    bool
	vsockErr error
}

func (s *Server) Vsock() (bool, error) { return s.vsock, s.vsockErr }

const vsockBuffer = 2 << 20

func (s *Server) addVsock() error {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("nfsview: a vsock socket: %w", err)
	}
	defer unix.Close(fd)
	for _, o := range []int{unix.SO_VM_SOCKETS_BUFFER_MAX_SIZE, unix.SO_VM_SOCKETS_BUFFER_SIZE} {
		if err := unix.SetsockoptUint64(fd, unix.AF_VSOCK, o, vsockBuffer); err != nil {
			return fmt.Errorf("nfsview: the vsock listener's buffer: %w", err)
		}
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: s.VsockPort}); err != nil {
		return fmt.Errorf("nfsview: vsock port %d: %w", s.VsockPort, err)
	}
	if err := unix.Listen(fd, 64); err != nil {
		return fmt.Errorf("nfsview: listen on vsock port %d: %w", s.VsockPort, err)
	}
	return s.write("portlist", strconv.Itoa(fd))
}

func (s *Server) dir() string {
	if s.Dir == "" {
		return NFSDDir
	}
	return s.Dir
}

func (s *Server) write(name, value string) error {
	p := filepath.Join(s.dir(), name)
	if err := os.WriteFile(p, []byte(value+"\n"), 0); err != nil {
		return fmt.Errorf("nfsview: %s <- %q: %w", p, value, err)
	}
	return nil
}

func (s *Server) Mount() error {
	var st unix.Statfs_t
	if unix.Statfs(s.dir(), &st) == nil && st.Type == 0x6e667364 {
		return nil
	}
	if err := unix.Mount("nfsd", s.dir(), "nfsd", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("nfsview: mount nfsd at %s: %w", s.dir(), err)
	}
	return nil
}

func (s *Server) Running() bool {
	b, err := os.ReadFile(filepath.Join(s.dir(), "threads"))
	return err == nil && strings.TrimSpace(string(b)) != "0"
}

func (s *Server) Start() error {
	if s.Running() {
		return errors.New("nfsview: nfsd is running")
	}
	if err := os.WriteFile(OwnerChangeParam, []byte("Y\n"), 0); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNoOwnerChangeParam
		}
		return fmt.Errorf("nfsview: %s: %w", OwnerChangeParam, err)
	}
	if s.RefuseMacNames {
		if err := os.WriteFile(MacNamesParam, []byte("Y\n"), 0); err != nil {
			return fmt.Errorf("nfsview: %s: %w (a kernel without patch 9004?)", MacNamesParam, err)
		}
	}
	if s.LeaseSeconds > 0 {
		if err := s.write("nfsv4leasetime", strconv.Itoa(s.LeaseSeconds)); err != nil {
			return err
		}
	}
	threads := s.Threads
	if threads <= 0 {
		threads = 4
	}
	port := s.Port
	if port == 0 {
		port = 2049
	}
	for _, w := range [][2]string{
		{"versions", "-3"},
		{"portlist", "tcp " + strconv.Itoa(port)},
	} {
		if err := s.write(w[0], w[1]); err != nil {
			return err
		}
	}
	s.vsock, s.vsockErr = false, nil
	if s.VsockPort != 0 {
		if s.vsockErr = s.addVsock(); s.vsockErr == nil {
			s.vsock = true
		}
	}
	return s.write("threads", strconv.Itoa(threads))
}

func (s *Server) Stop() error {
	if !s.Running() {
		return nil
	}
	return s.write("threads", "0")
}
