package nfsview

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const ControlSocket = "@fev/view/ctl"

const (
	CallAttach  = "attach"
	CallStart   = "start"
	CallRelease = "release"
)

type Control struct {
	Objects  func() *Objects
	EngineNS func() (path string, done func(), ok bool)
	Name     func(ctx context.Context, id string) string
	Bundle   func(id string) (pid int, dests []string, err error)
	Logf     func(string, ...any)
}

const controlWait = 5 * time.Second

func (c *Control) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func (c *Control) Serve(l *net.UnixListener) error {
	for {
		conn, err := l.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go c.handle(conn)
	}
}

func ValidID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !(id[i] >= '0' && id[i] <= '9' || id[i] >= 'a' && id[i] <= 'f') {
			return false
		}
	}
	return true
}

func (c *Control) handle(conn *net.UnixConn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(controlWait))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return
	}
	words := strings.Fields(line)
	answer := func(s string) { fmt.Fprintln(conn, s) }
	if len(words) != 2 || !ValidID(words[1]) {
		answer("err a call is VERB ID")
		return
	}
	verb, id := words[0], words[1]
	ns, done, ok := c.EngineNS()
	if !ok {
		answer("ok no engine")
		return
	}
	defer done()
	if err := c.peer(conn, ns); err != nil {
		c.logf("control: %s %.12s refused: %v", verb, id, err)
		answer("err peer")
		return
	}
	o := c.Objects()
	if o == nil {
		answer("ok no view")
		return
	}
	t := time.Now()
	switch verb {
	case CallAttach:
		name := c.Name(context.Background(), id)
		if name == "" {
			answer("ok not a container")
			return
		}
		err = o.Attach(ns, id, name)
	case CallStart:
		var pid int
		var dests []string
		pid, dests, err = c.Bundle(id)
		if err == nil {
			err = o.Binds(ns, id, pid, dests)
		}
	case CallRelease:
		o.Release(id)
	default:
		answer("err no call " + strconv.Quote(verb))
		return
	}
	if err != nil {
		c.logf("control: %s %.12s: %v", verb, id, err)
		answer("err " + strings.ReplaceAll(err.Error(), "\n", "; "))
		return
	}
	answer(fmt.Sprintf("ok %.2fms", float64(time.Since(t).Microseconds())/1000))
}

func (c *Control) peer(conn *net.UnixConn, enginePath string) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	pidfd := -1
	var serr error
	raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if serr != nil {
			return
		}
		pidfd, serr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PEERPIDFD)
	})
	if serr != nil {
		return serr
	}
	defer unix.Close(pidfd)
	if cred.Uid != 0 {
		return fmt.Errorf("uid %d", cred.Uid)
	}
	pid, err := pidfdPid(pidfd)
	if err != nil {
		return err
	}
	var theirs, engine unix.Stat_t
	if err := unix.Stat("/proc/"+strconv.Itoa(pid)+"/ns/mnt", &theirs); err != nil {
		return err
	}
	if err := unix.Stat(enginePath, &engine); err != nil {
		return err
	}
	if err := unix.PidfdSendSignal(pidfd, 0, nil, 0); err != nil {
		return fmt.Errorf("pid %d gone before its namespace was trusted", pid)
	}
	if theirs.Ino != engine.Ino || theirs.Dev != engine.Dev {
		return fmt.Errorf("pid %d is not in the engine's mount namespace", pid)
	}
	return nil
}

func pidfdPid(pidfd int) (int, error) {
	b, err := os.ReadFile("/proc/self/fdinfo/" + strconv.Itoa(pidfd))
	if err != nil {
		return 0, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "Pid:"); ok {
			pid, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || pid <= 0 {
				return 0, fmt.Errorf("the peer's pidfd names no process")
			}
			return pid, nil
		}
	}
	return 0, errors.New("the peer's pidfd has no pid")
}

func ListenControl() (*net.UnixListener, error) {
	return net.ListenUnix("unix", &net.UnixAddr{Name: ControlSocket, Net: "unix"})
}
