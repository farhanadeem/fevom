package nfsview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

const RPCDir = "/proc/thread-self/net/rpc"

func EnterView(netns, mntns string) error {
	runtime.LockOSThread()
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return fmt.Errorf("nfsview: unshare(CLONE_FS): %w", err)
	}
	for _, ns := range []struct {
		path string
		kind int
	}{{netns, unix.CLONE_NEWNET}, {mntns, unix.CLONE_NEWNS}} {
		if ns.path == "" {
			continue
		}
		fd, err := unix.Open(ns.path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("nfsview: %s: %w", ns.path, err)
		}
		err = unix.Setns(fd, ns.kind)
		unix.Close(fd)
		if err != nil {
			return fmt.Errorf("nfsview: setns %s: %w", ns.path, err)
		}
	}
	return nil
}

func ServeChannels(ctx context.Context, a *Answerer, logf func(string, ...any)) error {
	runtime.LockOSThread()
	if logf == nil {
		logf = func(string, ...any) {}
	}
	type ch struct {
		name string
		fd   int
	}
	var chans []ch
	defer func() {
		for _, c := range chans {
			unix.Close(c.fd)
		}
	}()
	for _, name := range Channels {
		fd, err := unix.Open(filepath.Join(RPCDir, name, "channel"), unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			return fmt.Errorf("nfsview: %s: %w", name, err)
		}
		chans = append(chans, ch{name, fd})
	}
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	defer w.Close()
	stop := context.AfterFunc(ctx, func() { w.Write([]byte{0}) })
	defer stop()
	fds := make([]unix.PollFd, len(chans)+1)
	for i, c := range chans {
		fds[i] = unix.PollFd{Fd: int32(c.fd), Events: unix.POLLIN}
	}
	fds[len(chans)] = unix.PollFd{Fd: int32(r.Fd()), Events: unix.POLLIN}
	buf := make([]byte, 64<<10)
	for {
		if _, err := unix.Poll(fds, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("nfsview: poll: %w", err)
		}
		if fds[len(chans)].Revents != 0 {
			return ctx.Err()
		}
		for i, c := range chans {
			if fds[i].Revents&unix.POLLIN == 0 {
				continue
			}
			n, err := unix.Read(c.fd, buf)
			if err != nil {
				if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
					continue
				}
				return fmt.Errorf("nfsview: %s: %w", c.name, err)
			}
			answers, bad := a.Handle(c.name, buf[:n])
			for _, b := range bad {
				logf("%s: %s", c.name, b)
			}
			for _, ans := range answers {
				if _, err := unix.Write(c.fd, ans); err != nil {
					logf("%s: the kernel refused %q: %v", c.name, ans, err)
				}
			}
		}
	}
}

func FlushAll(now time.Time) error {
	s := []byte(strconv.FormatInt(now.Unix(), 10) + "\n")
	for _, c := range Channels {
		if err := os.WriteFile(filepath.Join(RPCDir, c, "flush"), s, 0); err != nil {
			return fmt.Errorf("nfsview: flush %s: %w", c, err)
		}
	}
	return nil
}

func FlushExports(now time.Time) error {
	s := []byte(strconv.FormatInt(now.Unix(), 10) + "\n")
	for _, c := range []string{ChannelFH, ChannelExport} {
		if err := os.WriteFile(filepath.Join(RPCDir, c, "flush"), s, 0); err != nil {
			return fmt.Errorf("nfsview: flush %s: %w", c, err)
		}
	}
	return nil
}
