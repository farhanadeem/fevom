package webproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"

	"fev/guest/internal/webproxy/webctl"
	api "fev/proto/agent"
	"fev/proto/netplan"
)

type (
	Control = webctl.Control
	Report  = webctl.Report
)

const (
	ControlFD    = webctl.ControlFD
	ListenFDsEnv = webctl.ListenFDsEnv
)

func Listeners(fds string) (l80, l443 net.Listener, err error) {
	if fds == "" {
		if l80, err = net.Listen("tcp4", fmt.Sprintf(":%d", netplan.WebHTTPPort)); err != nil {
			return nil, nil, fmt.Errorf("listen on %d: %w", netplan.WebHTTPPort, err)
		}
		if l443, err = net.Listen("tcp4", fmt.Sprintf(":%d", netplan.WebHTTPSPort)); err != nil {
			l80.Close()
			return nil, nil, fmt.Errorf("listen on %d: %w", netplan.WebHTTPSPort, err)
		}
		return l80, l443, nil
	}
	a, b, ok := strings.Cut(fds, ",")
	n80, err1 := strconv.Atoi(a)
	n443, err2 := strconv.Atoi(b)
	if !ok || err1 != nil || err2 != nil || n80 < 3 || n443 < 3 || n80 == n443 {
		return nil, nil, fmt.Errorf("%s=%q is not two descriptors", ListenFDsEnv, fds)
	}
	ls := make([]net.Listener, 2)
	for i, n := range []int{n80, n443} {
		f := os.NewFile(uintptr(n), fmt.Sprintf("web-listener-%d", n))
		l, err := net.FileListener(f)
		f.Close()
		if err != nil {
			for _, x := range ls[:i] {
				x.Close()
			}
			return nil, nil, fmt.Errorf("descriptor %d: %w", n, err)
		}
		ls[i] = l
	}
	return ls[0], ls[1], nil
}

func ListenerPairs(fds string) ([][2]net.Listener, error) {
	parts := strings.Split(fds, ",")
	if fds == "" || len(parts)%2 != 0 || len(parts) > 4 {
		return nil, fmt.Errorf("%s=%q is not two or four descriptors", ListenFDsEnv, fds)
	}
	var out [][2]net.Listener
	closeAll := func() {
		for _, pr := range out {
			pr[0].Close()
			pr[1].Close()
		}
	}
	for i := 0; i < len(parts); i += 2 {
		l80, l443, err := Listeners(parts[i] + "," + parts[i+1])
		if err != nil {
			closeAll()
			return nil, err
		}
		out = append(out, [2]net.Listener{l80, l443})
	}
	return out, nil
}

type Namespaces struct {
	mu       sync.Mutex
	root     string
	machines map[string]string
}

func (n *Namespaces) Path(t api.DomainTarget) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	switch t.Kind {
	case api.TargetMachine:
		if p, ok := n.machines[t.ID]; ok {
			return p, nil
		}
		return "", fmt.Errorf("machine %s's network namespace is not known", t.ID)
	default:
		if t.NetNS == "" || n.root == "" {
			return "", fmt.Errorf("container %s's network namespace is not known", t.Name)
		}
		return n.root + t.NetNS, nil
	}
}

func (n *Namespaces) Apply(c Control) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if c.NetNSRoot != "" {
		n.root = c.NetNSRoot
	}
	if c.Machines != nil {
		n.machines = c.Machines
	}
}

func controlLines(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	return sc
}

func apply(line []byte, p *Proxy, ns *Namespaces) error {
	var c Control
	if err := json.Unmarshal(line, &c); err != nil {
		return fmt.Errorf("a control line: %w", err)
	}
	ns.Apply(c)
	if c.Names6 != "" {
		if n, err := netip.ParsePrefix(c.Names6); err == nil {
			p.SetNames6(n)
		}
	}
	if c.Domains != nil {
		p.Set(*c.Domains)
	}
	return nil
}

func Follow(r io.Reader, p *Proxy, ns *Namespaces) error { return follow(controlLines(r), p, ns) }

func follow(sc *bufio.Scanner, p *Proxy, ns *Namespaces) error {
	for sc.Scan() {
		if err := apply(sc.Bytes(), p, ns); err != nil {
			return err
		}
	}
	return sc.Err()
}

func Serve(ctrl io.Reader, rep *Reporter, l80, l443 net.Listener, p *Proxy, ns *Namespaces, more ...net.Listener) error {
	sc := controlLines(ctrl)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return fmt.Errorf("init's first control line: %w", err)
		}
		return errors.New("the control socket ended before init's first line")
	}
	if err := apply(sc.Bytes(), p, ns); err != nil {
		return err
	}
	done := make(chan error, 3+len(more))
	go func() { done <- fmt.Errorf("the listener on %s: %w", l80.Addr(), p.ServeHTTP80(l80)) }()
	go func() { done <- fmt.Errorf("the listener on %s: %w", l443.Addr(), p.ServeHTTPS(l443)) }()
	for i, l := range more {
		l, secure := l, i%2 == 1
		go func() { done <- fmt.Errorf("the listener on %s: %w", l.Addr(), p.accept(l, secure)) }()
	}
	rep.Send(Report{Ready: true})
	go func() { done <- follow(sc, p, ns) }()
	return <-done
}

type Reporter struct {
	mu sync.Mutex
	W  io.Writer
}

func (r *Reporter) Send(rep Report) {
	b, _ := json.Marshal(rep)
	r.mu.Lock()
	r.W.Write(append(b, '\n'))
	r.mu.Unlock()
}

type SocketIssuer struct {
	Path string
}

func (s SocketIssuer) Issue(ctx context.Context, req api.TLSIssueRequest) (api.TLSIssued, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", s.Path)
	if err != nil {
		return api.TLSIssued{}, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return api.TLSIssued{}, err
	}
	line, err := bufio.NewReader(io.LimitReader(c, 1<<20)).ReadBytes('\n')
	if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
		return api.TLSIssued{}, fmt.Errorf("the daemon's answer: %w", err)
	}
	var out api.TLSIssued
	if err := json.Unmarshal(line, &out); err != nil {
		return api.TLSIssued{}, err
	}
	return out, nil
}

func addrPort(a netip.AddrPort) string { return a.String() }
