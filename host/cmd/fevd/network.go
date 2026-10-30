package main

import (
	"log/slog"
	"net/netip"
	"os"
	"sync"

	"fev/host/daemon"
	"fev/host/macnet"
	"fev/host/netstack"
	api "fev/proto/daemon"
	priv "fev/proto/privhelper"
)

type macNetwork struct{ w *macnet.Watcher }

func startMacNetwork(log *slog.Logger) daemon.MacNetwork {
	w, err := macnet.Start(log)
	if err != nil || w == nil {
		log.Warn("the Mac's network is not followed: direct access is not checked against its routes after the start", "err", err)
		if w == nil {
			return nil
		}
	}
	return &macNetwork{w: w}
}

func (m *macNetwork) current() macnet.Snapshot {
	return m.w.Current().WithoutHeld(macnet.HeldIn(priv.RecordPath, 0))
}

func installedRecorded6() string { return recorded6(priv.RecordPath, 0, os.Getuid()) }

func installedRecorded4() string {
	rec, ok := priv.ReadRecord(priv.RecordPath, 0)
	if !ok || rec.UID != os.Getuid() {
		return ""
	}
	return rec.Base
}

func recorded6(path string, owner, uid int) string {
	rec, ok := priv.ReadRecord(path, owner)
	if !ok || rec.UID != uid {
		return ""
	}
	return rec.Base6
}

func (m *macNetwork) Check(ours []netip.Prefix, tunnel string) ([]netip.Prefix, []api.RouteConflict, []string) {
	r := macnet.Check(ours, tunnel, m.current())
	conflicts := make([]api.RouteConflict, len(r.Conflicts))
	warnings := make([]string, len(r.Conflicts))
	for i, c := range r.Conflicts {
		conflicts[i] = api.RouteConflict{Prefix: c.Prefix.String(), Route: c.With.String(), Interface: c.Interface, Withdrawn: c.Withdrawn}
		warnings[i] = c.Warning()
	}
	return r.Keep, conflicts, warnings
}

func (m *macNetwork) Taken(tunnel string) []netip.Prefix { return macnet.Taken(m.current(), tunnel) }

func (m *macNetwork) Subscribe(fn func()) func() {
	var mu sync.Mutex
	last := m.w.Current()
	return m.w.Subscribe(func(s macnet.Snapshot) {
		mu.Lock()
		skip := s.SameNetwork(last) && dnsOnly(s.Cause)
		last = s
		mu.Unlock()
		if !skip {
			fn()
		}
	})
}

func dnsOnly(cause []string) bool {
	for _, k := range cause {
		if k != "State:/Network/Global/DNS" {
			return false
		}
	}
	return len(cause) > 0
}

func (m *macNetwork) Refresh(reason string) { m.w.Refresh(reason) }

func (m *macNetwork) NAT64() []netip.Prefix { return m.w.Current().NAT64 }

func (m *macNetwork) Close() error {
	m.w.Close()
	return nil
}

type macHolder struct {
	mu sync.Mutex
	w  *macnet.Watcher
}

func (h *macHolder) start(log *slog.Logger) daemon.MacNetwork {
	m := startMacNetwork(log)
	if mn, ok := m.(*macNetwork); ok {
		h.mu.Lock()
		h.w = mn.w
		h.mu.Unlock()
	}
	return m
}

func (h *macHolder) watcher() *macnet.Watcher {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.w
}

func (h *macHolder) GlobalIPv6() bool {
	w := h.watcher()
	return w != nil && w.Current().GlobalIPv6
}

func (h *macHolder) Addresses() netstack.MacAddresses {
	w := h.watcher()
	if w == nil {
		return anyAddress{}
	}
	return w.Current()
}

func (h *macHolder) LANs() []netip.Prefix {
	w := h.watcher()
	if w == nil {
		return nil
	}
	nets := w.Current().Networks
	out := make([]netip.Prefix, 0, len(nets))
	for _, n := range nets {
		out = append(out, n.Prefix.Masked())
	}
	return out
}

func (h *macHolder) Own() ([]daemon.MacAddress, bool) {
	w := h.watcher()
	if w == nil {
		return nil, false
	}
	snap := w.Current()
	out := make([]daemon.MacAddress, len(snap.Addresses))
	for i, a := range snap.Addresses {
		out[i] = daemon.MacAddress{Addr: a.Addr, Interface: a.Interface}
	}
	return out, true
}

type anyAddress struct{}

func (anyAddress) HasAddress(netip.Addr) bool { return true }
