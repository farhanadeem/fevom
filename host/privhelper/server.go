package privhelper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"time"

	"fev/proto/netplan"
	api "fev/proto/privhelper"
)

type Peer struct {
	UID  int
	PID  int
	Path string
}

type Config struct {
	Record   api.InstallRecord
	AllowUID int
	Root     string
	Sys      System
	Store    Store
	PeerOf   func(*net.UnixConn) (Peer, error)
	Logf     func(format string, a ...any)
	Now      func() time.Time
	Deadline time.Duration
}

const DefaultDeadline = 10 * time.Second

type Server struct {
	rec      api.InstallRecord
	allowUID int
	root     string
	sys      System
	store    Store
	peerOf   func(*net.UnixConn) (Peer, error)
	logf     func(string, ...any)
	now      func() time.Time
	deadline time.Duration
	bases    api.Bases
	plan     netplan.Plan
	plan6    *netplan.Plan6
}

func New(c Config) (*Server, error) {
	if c.Sys == nil {
		return nil, errors.New("no system")
	}
	s := &Server{rec: c.Record, allowUID: c.AllowUID, root: c.Root, sys: c.Sys, store: c.Store, peerOf: c.PeerOf, logf: c.Logf, now: c.Now, deadline: c.Deadline}
	if s.store == nil {
		s.store = storeFor(c.Root)
	}
	var err error
	if s.plan, s.plan6, s.bases, err = checkRecord(c.Record); err != nil {
		return nil, err
	}
	if s.allowUID == 0 {
		s.allowUID = c.Record.UID
	}
	if s.peerOf == nil {
		s.peerOf = PeerOf
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.deadline == 0 {
		s.deadline = DefaultDeadline
	}
	return s, nil
}

func checkRecord(r api.InstallRecord) (netplan.Plan, *netplan.Plan6, api.Bases, error) {
	if r.UID <= 0 {
		return netplan.Plan{}, nil, nil, fmt.Errorf("the install record's uid %d is not a user's", r.UID)
	}
	if !filepath.IsAbs(r.Home) || filepath.Clean(r.Home) != r.Home {
		return netplan.Plan{}, nil, nil, fmt.Errorf("the install record's home %q is not a clean absolute path", r.Home)
	}
	if r.ResolverPort < 1024 || r.ResolverPort > 65535 {
		return netplan.Plan{}, nil, nil, fmt.Errorf("the install record's resolver port %d is not a high port", r.ResolverPort)
	}
	switch r.ResolverForm {
	case "", api.ResolverFormFile, api.ResolverFormStore:
	case api.ResolverFormTunnel:
		return netplan.Plan{}, nil, nil, fmt.Errorf("the resolver entries' form %q is not built (an entry bound to the tunnel outlived it); %q and %q are", r.ResolverForm, api.ResolverFormStore, api.ResolverFormFile)
	default:
		return netplan.Plan{}, nil, nil, fmt.Errorf("the install record's resolver form %q is none of %q, %q and %q", r.ResolverForm, api.ResolverFormFile, api.ResolverFormStore, api.ResolverFormTunnel)
	}
	plan, err := netplan.Parse(r.Base)
	if err != nil {
		return netplan.Plan{}, nil, nil, fmt.Errorf("the install record's base: %w", err)
	}
	var plan6 *netplan.Plan6
	if r.Base6 != "" {
		p6, err := netplan.Parse6(r.Base6)
		if err != nil {
			return netplan.Plan{}, nil, nil, fmt.Errorf("the install record's IPv6 base: %w", err)
		}
		plan6 = &p6
	}
	bases, err := api.BasesOf(r)
	if err != nil {
		return netplan.Plan{}, nil, nil, err
	}
	return plan, plan6, bases, nil
}

func (s *Server) path(p string) string {
	if s.root == "" {
		return p
	}
	return filepath.Join(s.root, p)
}

func (s *Server) ServeConn(uc *net.UnixConn) {
	defer uc.Close()
	uc.SetDeadline(time.Now().Add(s.deadline))
	peer, err := s.peerOf(uc)
	if err != nil {
		s.logf("refused a connection whose peer cannot be read: %v", err)
		return
	}
	if peer.UID != 0 && peer.UID != s.allowUID {
		s.logf("refused uid %d (pid %d, %s): only uid %d and root may call", peer.UID, peer.PID, peer.Path, s.allowUID)
		return
	}
	req, err := api.ReadRequest(uc)
	if err != nil {
		s.logf("uid %d (pid %d, %s): %v", peer.UID, peer.PID, peer.Path, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.deadline)
	defer cancel()
	start := time.Now()
	result, fd, err := s.Call(ctx, req.Method, req.Params)
	outcome := "ok"
	if err != nil {
		outcome = err.Error()
	}
	s.logf("uid %d (pid %d, %s): %s %s: %s (%s)", peer.UID, peer.PID, peer.Path, req.Method, bytes.TrimSpace(req.Params), outcome, time.Since(start).Round(time.Millisecond))
	if werr := api.WriteReply(uc, req, result, err, fd); werr != nil {
		s.logf("writing the reply to %s: %v", req.Method, werr)
	}
	if fd >= 0 {
		s.sys.CloseFD(fd)
	}
}

func (s *Server) Call(ctx context.Context, method string, params json.RawMessage) (any, int, error) {
	switch method {
	case api.MethodHello:
		if err := decode(params, &api.Empty{}); err != nil {
			return nil, -1, err
		}
		r, err := s.hello()
		return r, -1, err
	case api.MethodTunnelOpen:
		var p api.TunnelParams
		if err := decode(params, &p); err != nil {
			return nil, -1, err
		}
		r, fd, err := s.tunnelOpen(p)
		return r, fd, err
	case api.MethodRoutesSet:
		var p api.RoutesParams
		if err := decode(params, &p); err != nil {
			return nil, -1, err
		}
		r, err := s.routesSet(p)
		return r, -1, err
	case api.MethodPortBind:
		var p api.PortParams
		if err := decode(params, &p); err != nil {
			return nil, -1, err
		}
		r, fd, err := s.portBind(p)
		return r, fd, err
	case api.MethodDockerSockLink:
		if err := decode(params, &api.Empty{}); err != nil {
			return nil, -1, err
		}
		if !s.rec.DockerSockLink {
			return nil, -1, api.Refusal{Arg: "docker_sock", Value: api.DockerSockPath, Reason: "the admin setup was not asked for the link: sudo fev setup --admin --docker-sock"}
		}
		r, err := LinkDockerSock(s.path(api.DockerSockPath), dockerSockTarget(s.rec.Home), s.rec.DockerSockConsent)
		return r, -1, err
	case api.MethodDockerSockUnlink:
		if err := decode(params, &api.Empty{}); err != nil {
			return nil, -1, err
		}
		if !s.rec.DockerSockLink {
			return nil, -1, api.Refusal{Arg: "docker_sock", Value: api.DockerSockPath, Reason: "the admin setup was not asked for the link: sudo fev setup --admin --docker-sock"}
		}
		r, err := UnlinkDockerSock(s.path(api.DockerSockPath), dockerSockTarget(s.rec.Home))
		return r, -1, err
	case api.MethodLocalNamesSet:
		var p api.LocalNamesParams
		if err := decode(params, &p); err != nil {
			return nil, -1, err
		}
		r, err := s.localNamesSet(ctx, p)
		return r, -1, err
	}
	if _, ok := api.Pending[method]; ok {
		return nil, -1, &api.Error{Code: api.CodeNotImplemented, Message: method + " is not implemented in this helper"}
	}
	return nil, -1, &api.Error{Code: -32601, Message: "no verb " + method}
}

func decode(params json.RawMessage, v any) error {
	params = bytes.TrimSpace(params)
	if len(params) == 0 || bytes.Equal(params, []byte("null")) {
		params = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(params))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return api.Refusal{Arg: "params", Reason: err.Error()}
	}
	if dec.More() {
		return api.Refusal{Arg: "params", Reason: "more than one value"}
	}
	return nil
}
