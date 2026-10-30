package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"fev/proto/netplan"
	priv "fev/proto/privhelper"
	"fev/proto/version"
)

type StandInHelper struct {
	Version, Level, UID, Port int
	Base                      netip.Prefix
	Base6                     netip.Prefix
	NAT64                     []netip.Prefix
	PreNAT64                  bool
	Refuse6                   bool
	Refuse                    map[string]bool
	Answered                  map[string]bool

	ln net.Listener

	mu      sync.Mutex
	calls   []string
	routes  []string
	names   []string
	peers   []*os.File
	bound   []string
	utun    net.Listener
	utunDir string
}

var StandInBase6 = netip.MustParsePrefix("fd5b:6e2a:91c0::/48")

func (h *StandInHelper) NAT64Prefixes() []netip.Prefix { return slices.Clone(h.NAT64) }

func (h *StandInHelper) Recorded6() string {
	if !h.Base6.IsValid() {
		return ""
	}
	return h.Base6.String()
}

func NewStandInHelper(words string) (*StandInHelper, error) {
	h := &StandInHelper{Version: priv.ProtocolVersion, Level: priv.SecurityLevel, UID: os.Getuid(), Base: netplan.DefaultBase,
		Base6: StandInBase6, Refuse: map[string]bool{}, Answered: map[string]bool{}}
	for _, w := range strings.Fields(words) {
		k, v, _ := strings.Cut(w, "=")
		var err error
		switch k {
		case "version":
			h.Version, err = strconv.Atoi(v)
		case "level":
			h.Level, err = strconv.Atoi(v)
		case "uid":
			h.UID, err = strconv.Atoi(v)
		case "base":
			h.Base, err = netip.ParsePrefix(v)
		case "base6":
			if v == "none" {
				h.Base6 = netip.Prefix{}
			} else if h.Base6, err = netip.ParsePrefix(v); err == nil {
				_, err = netplan.New6(h.Base6)
			}
		case "prenat64":
			h.PreNAT64 = true
		case "refuse6":
			h.Refuse6 = true
		case "nat64":
			var p netip.Prefix
			if p, err = netip.ParsePrefix(v); err == nil {
				if err = netplan.CheckNAT64(p); err == nil {
					h.NAT64 = append(h.NAT64, p)
				}
			}
		case "port":
			h.Port, err = strconv.Atoi(v)
		case "answered":
			h.Answered[v] = true
		case "refuse":
			h.Refuse[v] = true
		default:
			err = errors.New("unknown")
		}
		if err != nil {
			return nil, fmt.Errorf("FEV_FAKE_HELPER: %q: %v", w, err)
		}
	}
	if h.Port == 0 {
		l, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		h.Port = l.LocalAddr().(*net.UDPAddr).Port
		l.Close()
	}
	return h, nil
}

func (h *StandInHelper) Listen(path string) error {
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	h.ln = ln
	if err := h.serveUtun(path + ".utun"); err != nil {
		ln.Close()
		return err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go h.serve(c.(*net.UnixConn))
		}
	}()
	return nil
}

func (h *StandInHelper) Close() {
	if h.ln != nil {
		h.ln.Close()
	}
	if h.utun != nil {
		h.utun.Close()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range h.peers {
		f.Close()
	}
	h.peers = nil
}

func (h *StandInHelper) Calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.calls)
}

func (h *StandInHelper) Routes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.routes)
}

func (h *StandInHelper) Names() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.names)
}

func (h *StandInHelper) TunnelClosed() bool {
	f := h.lastPeer()
	if f == nil {
		return true
	}
	raw, err := f.SyscallConn()
	if err != nil {
		return true
	}
	closed := false
	raw.Control(func(fd uintptr) {
		syscall.SetNonblock(int(fd), true)
		_, err := syscall.Write(int(fd), []byte{0, 0, 0, 0})
		closed = errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
			errors.Is(err, syscall.ENOTCONN) || errors.Is(err, syscall.EDESTADDRREQ)
	})
	return closed
}

func (h *StandInHelper) lastPeer() *os.File {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := len(h.peers); n > 0 {
		return h.peers[n-1]
	}
	return nil
}

func (h *StandInHelper) serveUtun(path string) error {
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	h.utun = l
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			uc := c.(*net.UnixConn)
			if f := h.lastPeer(); f != nil {
				uc.WriteMsgUnix([]byte("utun99\n"), syscall.UnixRights(int(f.Fd())), nil)
			} else {
				uc.Write([]byte("none\n"))
			}
			uc.Close()
		}
	}()
	return nil
}

func (h *StandInHelper) serve(uc *net.UnixConn) {
	defer uc.Close()
	req, err := priv.ReadRequest(uc)
	if err != nil {
		return
	}
	h.mu.Lock()
	h.calls = append(h.calls, req.Method)
	h.mu.Unlock()
	if h.Refuse[req.Method] {
		priv.WriteReply(uc, req, nil, errors.New("the stand-in refuses "+req.Method), -1)
		return
	}
	plan, _ := netplan.New(h.Base)
	switch req.Method {
	case priv.MethodHello:
		priv.WriteReply(uc, req, priv.HelloResult{ProtocolVersion: h.Version, SecurityLevel: h.Level, Build: version.Build, UID: h.UID,
			Base: h.Base.String(), Base6: h.Recorded6(), ResolverPort: h.Port, Resolver: priv.StateFev, DockerSock: priv.StateAbsent, LocalNames: h.Names(),
			RoutesNAT64: !h.PreNAT64}, nil, -1)
	case priv.MethodTunnelOpen:
		var p priv.TunnelParams
		if err := json.Unmarshal(req.Params, &p); err != nil || p.MTU < priv.MinMTU || p.MTU > priv.MaxMTU {
			priv.WriteReply(uc, req, nil, priv.Refusal{Arg: "mtu", Value: strconv.Itoa(p.MTU), Reason: "outside 1280 to 65535"}, -1)
			return
		}
		mac6 := ""
		if p.IPv6 {
			p6, err := netplan.New6(h.Base6)
			if err != nil || h.Refuse6 {
				priv.WriteReply(uc, req, nil, priv.Refusal{Arg: "ipv6", Value: "true", Reason: "the install recorded no IPv6 base; run `sudo fev setup --admin` again"}, -1)
				return
			}
			mac6 = p6.TunnelMac6().String()
		}
		syscall.ForkLock.RLock()
		fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
		if err == nil {
			syscall.CloseOnExec(fds[0])
			syscall.CloseOnExec(fds[1])
			for _, fd := range fds {
				for _, opt := range []int{syscall.SO_RCVBUF, syscall.SO_SNDBUF} {
					syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, opt, 512<<10)
				}
			}
		}
		syscall.ForkLock.RUnlock()
		if err != nil {
			priv.WriteReply(uc, req, nil, err, -1)
			return
		}
		h.mu.Lock()
		h.peers = append(h.peers, os.NewFile(uintptr(fds[1]), "utun99 (stand-in)"))
		h.routes = nil
		h.mu.Unlock()
		priv.WriteReply(uc, req, priv.Tunnel{Interface: "utun99", Mac: plan.TunnelMac().String(), Peer: plan.TunnelPeer().String(), Mac6: mac6, MTU: p.MTU}, nil, fds[0])
		syscall.Close(fds[0])
	case priv.MethodRoutesSet:
		var p priv.RoutesParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			priv.WriteReply(uc, req, nil, err, -1)
			return
		}
		if p.Interface != "utun99" {
			priv.WriteReply(uc, req, nil, priv.Refusal{Arg: "interface", Value: p.Interface, Reason: "does not carry the tunnel's address"}, -1)
			return
		}
		bases := priv.Bases{h.Base}
		if h.Base6.IsValid() {
			bases = append(bases, h.Base6)
		}
		nat64 := h.NAT64
		if h.PreNAT64 {
			nat64 = nil
		}
		if _, err := priv.CheckRoutesNAT64(bases, nat64, p.Prefixes); err != nil {
			priv.WriteReply(uc, req, nil, err, -1)
			return
		}
		h.mu.Lock()
		old := h.routes
		h.routes = slices.Clone(p.Prefixes)
		h.mu.Unlock()
		var added, removed []string
		for _, r := range p.Prefixes {
			if !slices.Contains(old, r) {
				added = append(added, r)
			}
		}
		for _, r := range old {
			if !slices.Contains(p.Prefixes, r) {
				removed = append(removed, r)
			}
		}
		priv.WriteReply(uc, req, priv.Routes{Interface: p.Interface, Prefixes: p.Prefixes, Added: added, Removed: removed}, nil, -1)
	case priv.MethodLocalNamesSet:
		var p priv.LocalNamesParams
		if err := json.Unmarshal(req.Params, &p); err != nil || len(p.Names) > priv.MaxLocalNames {
			priv.WriteReply(uc, req, nil, priv.Refusal{Arg: "names", Value: strconv.Itoa(len(p.Names)), Reason: "not a list of at most 32 names"}, -1)
			return
		}
		var out priv.LocalNames
		var now []string
		for _, raw := range p.Names {
			name, err := priv.CanonicalLocalName(raw, "standin-mac.local")
			switch {
			case err != nil:
				out.Names = append(out.Names, priv.LocalName{Name: raw, State: priv.NameRefused, Reason: err.Error()})
			case h.Answered[name]:
				out.Names = append(out.Names, priv.LocalName{Name: name, State: priv.NameRefused, Reason: "answered on the network", AnsweredBy: "192.0.2.7"})
			default:
				out.Names = append(out.Names, priv.LocalName{Name: name, State: priv.NamePresent})
				now = append(now, name)
			}
		}
		h.mu.Lock()
		for _, old := range h.names {
			if !slices.Contains(now, old) {
				out.Names = append(out.Names, priv.LocalName{Name: old, State: priv.NameRemoved})
			}
		}
		h.names = now
		h.mu.Unlock()
		priv.WriteReply(uc, req, out, nil, -1)
	case priv.MethodPortBind:
		var p priv.PortParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			priv.WriteReply(uc, req, nil, err, -1)
			return
		}
		if err := priv.CheckPort(p); err != nil {
			priv.WriteReply(uc, req, nil, err, -1)
			return
		}
		h.mu.Lock()
		h.bound = append(h.bound, fmt.Sprintf("%s%d:%d", p.Proto, p.Family, p.Port))
		h.mu.Unlock()
		f, addr, err := standInBind(p)
		if err != nil {
			priv.WriteReply(uc, req, nil, err, -1)
			return
		}
		defer f.Close()
		priv.WriteReply(uc, req, priv.Bound{Address: addr}, nil, int(f.Fd()))
	default:
		priv.WriteReply(uc, req, nil, &priv.Error{Code: priv.CodeNotImplemented, Message: req.Method + " is not served by the stand-in"}, -1)
	}
}

func (h *StandInHelper) Bound() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.bound)
}

func standInBind(p priv.PortParams) (*os.File, string, error) {
	host := "127.0.0.1"
	if p.Family == 6 {
		host = "::1"
	}
	addr := net.JoinHostPort(host, "0")
	if p.Proto == "tcp" {
		l, err := net.Listen("tcp"+strconv.Itoa(p.Family), addr)
		if err != nil {
			return nil, "", err
		}
		defer l.Close()
		f, err := l.(*net.TCPListener).File()
		return f, l.Addr().String(), err
	}
	pc, err := net.ListenPacket("udp"+strconv.Itoa(p.Family), addr)
	if err != nil {
		return nil, "", err
	}
	defer pc.Close()
	f, err := pc.(*net.UDPConn).File()
	return f, pc.LocalAddr().String(), err
}
