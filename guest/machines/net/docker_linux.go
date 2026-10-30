package machnet

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"fev/guest/internal/logbuf"
	"fev/guest/internal/streams"
	"fev/guest/machines"
)

const DockerSocket = "docker.sock"

const refusalBound = 5 * time.Second

type dockerSocket struct {
	l      *net.UnixListener
	path   string
	user   string
	dial   func() (net.Conn, error)
	rootMu sync.Mutex
	root   string
	logf   func(int, string, ...any)
	once   sync.Once
}

func listenDocker(p machines.Plug, dial func() (net.Conn, error), logf func(int, string, ...any)) (*dockerSocket, error) {
	if p.Isolated {
		return nil, nil
	}
	path := filepath.Join(p.Run, DockerSocket)
	os.Remove(path)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		l.Close()
		return nil, err
	}
	d := &dockerSocket{l: l, path: path, user: p.User, root: p.Root, dial: dial, logf: logf}
	go d.serve()
	return d, nil
}

func (d *dockerSocket) renamed(name string) {
	d.rootMu.Lock()
	d.root = filepath.Join(filepath.Dir(d.root), name)
	d.rootMu.Unlock()
}

func (d *dockerSocket) close() {
	d.once.Do(func() {
		d.l.Close()
	})
}

func (d *dockerSocket) serve() {
	for {
		c, err := d.l.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go d.handle(c)
	}
}

func (d *dockerSocket) handle(c *net.UnixConn) {
	uid, err := peerUID(c)
	if err != nil {
		c.Close()
		return
	}
	if !d.admits(uid) {
		d.refuse(c, uid)
		return
	}
	engine, err := d.dial()
	if err != nil {
		c.Close()
		return
	}
	streams.Splice(c, engine)
}

func peerUID(c *net.UnixConn) (uint32, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if cerr != nil {
		return 0, cerr
	}
	return cred.Uid, nil
}

func (d *dockerSocket) admits(uid uint32) bool {
	if uid == 0 {
		return true
	}
	if d.user == "" {
		return false
	}
	d.rootMu.Lock()
	root := d.root
	d.rootMu.Unlock()
	want, ok := userUID(root, d.user)
	return ok && want == uid
}

func userUID(root, user string) (uint32, bool) {
	dir, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, false
	}
	defer unix.Close(dir)
	fd, err := unix.Openat2(dir, "etc/passwd", &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_IN_ROOT})
	if err != nil {
		return 0, false
	}
	f := os.NewFile(uintptr(fd), "passwd")
	defer f.Close()
	return uidIn(f, user)
}

func (d *dockerSocket) refuse(c net.Conn, uid uint32) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(refusalBound))
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadSlice('\n')
		if err != nil || len(bytes.TrimSpace(line)) == 0 {
			break
		}
	}
	c.Write([]byte(refusal(uid, d.user)))
	d.logf(logbuf.Info, "the engine's socket refused uid %d", uid)
}
