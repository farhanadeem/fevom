package fw

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"fev/guest/internal/reaper"
)

type Tool struct {
	Root   string
	Reaper *reaper.Reaper
}

const bound = 20 * time.Second

var applyMu sync.Mutex

func (t Tool) Apply(build func(Tables) string) error {
	applyMu.Lock()
	defer applyMu.Unlock()
	now, err := t.Run(nil, "/usr/sbin/iptables-save")
	if err != nil {
		return fmt.Errorf("read the rules: %w", err)
	}
	if out, err := t.Run([]byte(build(ParseSave(string(now)))), "/usr/sbin/iptables-restore", "--noflush"); err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

func (t Tool) Apply6(build func(Tables) string) error {
	applyMu.Lock()
	defer applyMu.Unlock()
	now, err := t.Run(nil, "/usr/sbin/ip6tables-save")
	if err != nil {
		return fmt.Errorf("read the IPv6 rules: %w", err)
	}
	if out, err := t.Run([]byte(build(ParseSave(string(now)))), "/usr/sbin/ip6tables-restore", "--noflush"); err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

func (t Tool) Run(stdin []byte, path string, args ...string) ([]byte, error) {
	if t.Root == "" || t.Reaper == nil {
		return nil, fmt.Errorf("no firewall tool")
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer null.Close()
	in := null
	var inW *os.File
	if stdin != nil {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		defer r.Close()
		in, inW = r, w
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		if inW != nil {
			inW.Close()
		}
		return nil, err
	}
	defer outR.Close()
	argv0 := path[strings.LastIndexByte(path, '/')+1:]
	child, err := t.Reaper.Start(path, append([]string{argv0}, args...), &os.ProcAttr{
		Dir:   "/",
		Env:   []string{"PATH=/usr/sbin:/usr/bin"},
		Files: []*os.File{in, outW, outW},
		Sys:   &syscall.SysProcAttr{Chroot: t.Root},
	})
	outW.Close()
	if err != nil {
		if inW != nil {
			inW.Close()
		}
		return nil, err
	}
	if inW != nil {
		go func() {
			inW.Write(stdin)
			inW.Close()
		}()
	}
	var killed atomic.Bool
	timer := time.AfterFunc(bound, func() {
		killed.Store(t.Reaper.Signal(child, unix.SIGKILL, false))
	})
	var out bytes.Buffer
	out.ReadFrom(outR)
	ws := child.Wait()
	timer.Stop()
	switch {
	case killed.Load():
		return out.Bytes(), fmt.Errorf("%s: killed after %v", path, bound)
	case ws.Signaled():
		return out.Bytes(), fmt.Errorf("%s: signal %d", path, ws.Signal())
	case ws.ExitStatus() != 0:
		return out.Bytes(), fmt.Errorf("%s: exit %d", path, ws.ExitStatus())
	}
	return out.Bytes(), nil
}

func (t Tool) Nft(script string) error {
	if out, err := t.Run([]byte(script), "/usr/sbin/nft", "-f", "-"); err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
