package nfsview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const PipeFS = "/run/fev/view/rpc_pipefs"

func MountPipeFS(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var st, parent unix.Stat_t
	if unix.Stat(dir, &st) == nil && unix.Stat(filepath.Dir(dir), &parent) == nil && st.Dev != parent.Dev {
		return nil
	}
	if err := unix.Mount("rpc_pipefs", dir, "rpc_pipefs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("nfsview: mount rpc_pipefs at %s: %w", dir, err)
	}
	return nil
}

func ServeCLD(ctx context.Context, pipefs string, logf func(string, ...any)) error {
	return ServeCLDReady(ctx, pipefs, nil, logf)
}

func ServeCLDReady(ctx context.Context, pipefs string, ready func(), logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	dir := filepath.Join(pipefs, "nfsd")
	pipe := filepath.Join(dir, "cld")
	ino, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return fmt.Errorf("nfsview: inotify: %w", err)
	}
	defer unix.Close(ino)
	if _, err := unix.InotifyAddWatch(ino, dir, unix.IN_CREATE|unix.IN_DELETE); err != nil {
		return fmt.Errorf("nfsview: watch %s: %w", dir, err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	defer w.Close()
	stop := context.AfterFunc(ctx, func() { w.Write([]byte{0}) })
	defer stop()

	fd := -1
	closePipe := func() {
		if fd >= 0 {
			unix.Close(fd)
			fd = -1
		}
	}
	defer closePipe()
	reopen := func() {
		closePipe()
		f, err := unix.Open(pipe, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			if !errors.Is(err, unix.ENOENT) {
				logf("cld: open %s: %v", pipe, err)
			}
			return
		}
		fd = f
	}
	reopen()
	if ready != nil {
		ready()
	}
	buf := make([]byte, 4096)
	ev := make([]byte, 4096)
	for {
		fds := []unix.PollFd{{Fd: int32(ino), Events: unix.POLLIN}, {Fd: int32(r.Fd()), Events: unix.POLLIN}}
		if fd >= 0 {
			fds = append(fds, unix.PollFd{Fd: int32(fd), Events: unix.POLLIN})
		}
		if _, err := unix.Poll(fds, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("nfsview: poll: %w", err)
		}
		if fds[1].Revents != 0 {
			return ctx.Err()
		}
		if fds[0].Revents != 0 {
			for {
				if _, err := unix.Read(ino, ev); err != nil {
					break
				}
			}
			reopen()
			continue
		}
		if len(fds) < 3 || fds[2].Revents == 0 {
			continue
		}
		if fds[2].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			closePipe()
			continue
		}
		n, err := unix.Read(fd, buf)
		if err != nil {
			if !errors.Is(err, unix.EAGAIN) {
				closePipe()
			}
			continue
		}
		if n == 0 {
			continue
		}
		out, cmd, err := AnswerCLD(buf[:n])
		if err != nil {
			logf("cld: %v", err)
			continue
		}
		if _, err := unix.Write(fd, out); err != nil {
			logf("cld: answer to %s: %v", CLDCommandName(cmd), err)
		}
	}
}
