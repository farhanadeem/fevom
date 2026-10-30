package webctl

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type Held struct {
	f80, f443 *os.File
	more      []*os.File
}

func Hold(addr netip.Addr, port80, port443 int) (*Held, error) {
	if !addr.Is4() {
		return nil, fmt.Errorf("%s is not an IPv4 address", addr)
	}
	f80, err := listen4(addr, port80)
	if err != nil {
		return nil, err
	}
	f443, err := listen4(addr, port443)
	if err != nil {
		f80.Close()
		return nil, err
	}
	return &Held{f80: f80, f443: f443}, nil
}

func (h *Held) Also(addr netip.Addr, port80, port443 int) error {
	f80, err := listen(addr, port80)
	if err != nil {
		return err
	}
	f443, err := listen(addr, port443)
	if err != nil {
		f80.Close()
		return err
	}
	h.more = append(h.more, f80, f443)
	return nil
}

const backlog = 4096

func listen4(addr netip.Addr, port int) (*os.File, error) { return listen(addr, port) }

func listen(addr netip.Addr, port int) (*os.File, error) {
	family := unix.AF_INET
	if addr.Is6() {
		family = unix.AF_INET6
	}
	syscall.ForkLock.RLock()
	fd, err := unix.Socket(family, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, err
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		unix.Close(fd)
		return nil, err
	}
	var sa unix.Sockaddr
	if addr.Is6() {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 1); err != nil {
			unix.Close(fd)
			return nil, err
		}
		sa = &unix.SockaddrInet6{Port: port, Addr: addr.As16()}
	} else {
		sa = &unix.SockaddrInet4{Port: port, Addr: addr.As4()}
	}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("bind %s: %w", netip.AddrPortFrom(addr, uint16(port)), err)
	}
	if err := unix.Listen(fd, backlog); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("listen on %d: %w", port, err)
	}
	return os.NewFile(uintptr(fd), fmt.Sprintf("web-listener-%d", port)), nil
}

func (h *Held) Files() []*os.File { return append([]*os.File{h.f80, h.f443}, h.more...) }

func (h *Held) FDs(first int) string {
	var out []string
	for i := range h.Files() {
		out = append(out, strconv.Itoa(first+i))
	}
	return strings.Join(out, ",")
}

func (h *Held) Addrs() (a80, a443 netip.AddrPort, err error) {
	var out [2]netip.AddrPort
	for i, f := range h.Files()[:2] {
		sa, err := unix.Getsockname(int(f.Fd()))
		if err != nil {
			return a80, a443, err
		}
		in, ok := sa.(*unix.SockaddrInet4)
		if !ok {
			return a80, a443, errors.New("not an IPv4 socket")
		}
		out[i] = netip.AddrPortFrom(netip.AddrFrom4(in.Addr), uint16(in.Port))
	}
	return out[0], out[1], nil
}

func (h *Held) Wait() error {
	var fds []unix.PollFd
	for _, f := range h.Files() {
		fds = append(fds, unix.PollFd{Fd: int32(f.Fd()), Events: unix.POLLIN})
	}
	for {
		_, err := unix.Poll(fds, -1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		for _, p := range fds {
			if p.Revents&(unix.POLLNVAL|unix.POLLERR) != 0 {
				return fmt.Errorf("a web listener failed (revents %#x)", p.Revents)
			}
		}
		return nil
	}
}

func (h *Held) Close() {
	for _, f := range h.Files() {
		f.Close()
	}
}
