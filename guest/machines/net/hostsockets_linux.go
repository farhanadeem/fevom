package machnet

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"fev/guest/internal/logbuf"
	"fev/guest/machines"
	api "fev/proto/agent"
	"fev/proto/bridge"
)

const (
	MacSocket   = "mac.sock"
	AgentSocket = "ssh-auth.sock"
)

type hostSocket struct {
	l       *net.UnixListener
	service string
	n       *Network
	user    string
	mu      sync.Mutex
	name    string
	root    string
	once    sync.Once
	idBase  uint32
}

func (n *Network) listenHost(p machines.Plug, file, service string) (*hostSocket, error) {
	path := filepath.Join(p.Run, file)
	os.Remove(path)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		l.Close()
		return nil, err
	}
	h := &hostSocket{l: l, service: service, n: n, user: p.User, name: p.Name, root: p.Root, idBase: p.IDBase}
	go h.serve()
	return h, nil
}

func (n *Network) hostSockets(p machines.Plug) (mac, agent *hostSocket) {
	if p.Run == "" || n.o.Requests == nil {
		return nil, nil
	}
	var err error
	if p.Isolated {
		if p.Agent {
			if agent, err = n.listenHost(p, AgentSocket, api.StreamSSHAgent); err != nil {
				n.logf(logbuf.Warning, "%s: no SSH agent socket: %v", p.Name, err)
			}
		}
		return nil, agent
	}
	if mac, err = n.listenHost(p, MacSocket, api.StreamMac); err != nil {
		n.logf(logbuf.Warning, "%s: no command bridge: %v", p.Name, err)
	}
	if agent, err = n.listenHost(p, AgentSocket, api.StreamSSHAgent); err != nil {
		n.logf(logbuf.Warning, "%s: no SSH agent socket: %v", p.Name, err)
	}
	if err := ensureLocalLinks(p.Root); err != nil {
		n.logf(logbuf.Warning, "%s: mac and macctl are not linked from /usr/local/bin: %v", p.Name, err)
	}
	return mac, agent
}

func (h *hostSocket) close() {
	if h == nil {
		return
	}
	h.once.Do(func() { h.l.Close() })
}

func (h *hostSocket) renamed(name string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.name = name
	h.root = filepath.Join(filepath.Dir(h.root), name)
	h.mu.Unlock()
}

func (h *hostSocket) current() (name, root string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.name, h.root
}

func (h *hostSocket) serve() {
	for {
		c, err := h.l.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go h.handle(c)
	}
}

func (h *hostSocket) admits(uid uint32) bool {
	if uid == 0 {
		return true
	}
	if h.user == "" {
		return false
	}
	_, root := h.current()
	want, ok := userUID(root, h.user)
	return ok && want == uid
}

func (h *hostSocket) refusal(uid uint32) string {
	who := "root"
	if h.user != "" {
		who = "root and " + h.user
	}
	what := "the Mac's commands"
	if h.service == api.StreamSSHAgent {
		what = "the Mac's SSH agent"
	}
	return fmt.Sprintf("uid %d may not use %s from this machine; its socket serves %s", uid, what, who)
}

func (h *hostSocket) handle(c *net.UnixConn) {
	uid, err := peerUID(c)
	if err != nil {
		c.Close()
		return
	}
	if h.idBase != 0 {
		if uid < h.idBase || uid-h.idBase >= 65536 {
			c.Close()
			return
		}
		uid -= h.idBase
	}
	h.handleAs(c, uid)
}

func (h *hostSocket) handleAs(c *net.UnixConn, uid uint32) {
	name, _ := h.current()
	if h.service == api.StreamSSHAgent {
		if !h.admits(uid) {
			c.Close()
			h.n.logf(logbuf.Info, "%s: %s", name, h.refusal(uid))
			return
		}
		u := int(uid)
		h.n.o.Requests.Open(c, api.StreamRequest{Service: api.StreamSSHAgent, Machine: name, UID: &u}, nil, nil)
		return
	}
	h.serveBridge(c, uid)
}

func (h *hostSocket) serveBridge(c *net.UnixConn, uid uint32) {
	c.SetDeadline(time.Now().Add(refusalBound))
	r := bufio.NewReader(c)
	hdr, line, err := bridge.ReadHeader(r)
	if err == nil {
		err = hdr.Check()
	}
	name, _ := h.current()
	switch {
	case err != nil:
		bridge.WriteError(c, err.Error())
		c.Close()
		return
	case !h.admits(uid):
		bridge.WriteError(c, h.refusal(uid))
		c.Close()
		h.n.logf(logbuf.Info, "%s: %s", name, h.refusal(uid))
		return
	case hdr.Verb == bridge.VerbLink || hdr.Verb == bridge.VerbUnlink:
		err := h.n.link(hdr, uid)
		if err != nil {
			bridge.WriteError(c, err.Error())
		} else {
			bridge.WriteOK(c)
			h.n.logf(logbuf.Info, "%s: %s %v", name, hdr.Verb, hdr.Names)
		}
		c.Close()
		return
	}
	c.SetDeadline(time.Time{})
	head := line
	if n := r.Buffered(); n > 0 {
		more, _ := r.Peek(n)
		head = append(append([]byte{}, line...), more...)
	}
	u := int(uid)
	h.n.o.Requests.Open(c, api.StreamRequest{Service: api.StreamMac, Machine: name, UID: &u}, head, func(conn net.Conn, why string) {
		conn.SetWriteDeadline(time.Now().Add(refusalBound))
		bridge.WriteError(conn, "the Mac did not take the command: "+why)
	})
}

func (n *Network) link(hdr *bridge.Header, uid uint32) error {
	if uid != 0 {
		return fmt.Errorf("mac %s needs root: try sudo", hdr.Verb)
	}
	dir := n.o.Links
	if dir == "" {
		return errors.New("this VM keeps no linked commands (it has no data disk)")
	}
	for _, name := range hdr.Names {
		p := filepath.Join(dir, name)
		if hdr.Verb == bridge.VerbLink {
			if err := LinkCommand(dir, name); err != nil {
				return err
			}
			continue
		}
		if bridge.Builtin(name) {
			return fmt.Errorf("%s is one of fev's own and stays linked", name)
		}
		if target, err := os.Readlink(p); err != nil || target != bridge.HelperPath {
			return fmt.Errorf("%s is not linked", name)
		}
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	return nil
}

func LinkCommand(dir, name string) error {
	p := filepath.Join(dir, name)
	err := os.Symlink(bridge.HelperPath, p)
	if errors.Is(err, os.ErrExist) {
		if target, rerr := os.Readlink(p); rerr == nil && target == bridge.HelperPath {
			return nil
		}
		return fmt.Errorf("%s: something other than a link to the helper has that name", name)
	}
	return err
}

func EnsureLinks(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, set := range [][]string{bridge.Own, bridge.Defaults} {
		for _, name := range set {
			p := filepath.Join(dir, name)
			if target, err := os.Readlink(p); err == nil && target == bridge.HelperPath {
				continue
			}
			os.RemoveAll(p)
			if err := os.Symlink(bridge.HelperPath, p); err != nil {
				return err
			}
		}
	}
	return nil
}

func ensureLocalLinks(root string) error {
	dir, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	bin, err := unix.Openat2(dir, "usr/local/bin", &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_IN_ROOT})
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(bin)
	for _, name := range bridge.Own {
		err := unix.Symlinkat(filepath.Join(bridge.BinDir, name), bin, name)
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
	}
	return nil
}
