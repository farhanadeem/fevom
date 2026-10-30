package nfsview

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"

	"golang.org/x/sys/unix"
)

type Namespaces struct {
	net, mnt *os.File
}

func NewNamespaces() (*Namespaces, error) {
	type result struct {
		ns  *Namespaces
		err error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		ns, err := makeNamespaces()
		ch <- result{ns, err}
	}()
	r := <-ch
	return r.ns, r.err
}

func makeNamespaces() (*Namespaces, error) {
	if err := unix.Unshare(unix.CLONE_FS | unix.CLONE_NEWNS | unix.CLONE_NEWNET); err != nil {
		return nil, fmt.Errorf("nfsview: unshare the view's namespaces: %w", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
		return nil, fmt.Errorf("nfsview: the view's mounts as slaves: %w", err)
	}
	if err := loopbackUp(); err != nil {
		return nil, err
	}
	netns, err := os.Open("/proc/thread-self/ns/net")
	if err != nil {
		return nil, fmt.Errorf("nfsview: %w", err)
	}
	mntns, err := os.Open("/proc/thread-self/ns/mnt")
	if err != nil {
		netns.Close()
		return nil, fmt.Errorf("nfsview: %w", err)
	}
	return &Namespaces{net: netns, mnt: mntns}, nil
}

func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("nfsview: a socket for lo: %w", err)
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("nfsview: lo's flags: %w", err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("nfsview: lo up: %w", err)
	}
	return nil
}

func (n *Namespaces) NetPath() string { return "/proc/self/fd/" + strconv.Itoa(int(n.net.Fd())) }
func (n *Namespaces) MntPath() string { return "/proc/self/fd/" + strconv.Itoa(int(n.mnt.Fd())) }

func (n *Namespaces) Close() error {
	err := n.net.Close()
	if e := n.mnt.Close(); err == nil {
		err = e
	}
	return err
}

func (n *Namespaces) Do(f func() error) error {
	ch := make(chan error, 1)
	go func() {
		if err := EnterView(n.NetPath(), n.MntPath()); err != nil {
			ch <- err
			return
		}
		ch <- f()
	}()
	return <-ch
}

func (n *Namespaces) Go(f func() error, done func(error)) {
	go func() {
		err := EnterView(n.NetPath(), n.MntPath())
		if err == nil {
			err = f()
		}
		if done != nil {
			done(err)
		}
	}()
}

func (n *Namespaces) Dial(ctx context.Context, address string) (net.Conn, error) {
	var c net.Conn
	err := n.Do(func() error {
		var d net.Dialer
		var err error
		c, err = d.DialContext(ctx, "tcp", address)
		return err
	})
	return c, err
}
