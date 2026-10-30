package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

type TrustKeeper interface {
	State() api.HTTPSStatus
	Trust(ctx context.Context, action string) (api.HTTPSStatus, error)
}

type HTTPS struct {
	Keeper TrustKeeper

	host Host
	mu   sync.Mutex
}

func (m *HTTPS) Name() string            { return "https" }
func (m *HTTPS) Marks() []string         { return nil }
func (m *HTTPS) StateChanged(api.Status) {}
func (m *HTTPS) Close()                  {}

func (m *HTTPS) Start(h Host) error {
	m.host = h
	h.Handle(api.MethodHTTPSTrust, m.trust)
	m.publish(m.Keeper.State())
	return nil
}

func (m *HTTPS) ConfigChanged(ch api.ConfigChange) {
	if ch.Key == api.KeyNetworkHTTPS {
		m.publish(m.Keeper.State())
	}
}

func (m *HTTPS) Refresh() {
	if m.host != nil {
		m.publish(m.Keeper.State())
	}
}

func (m *HTTPS) publish(st api.HTTPSStatus) {
	st.Enabled = m.host.Config().Bool(api.KeyNetworkHTTPS)
	block := st
	updateStatus(m.host, func(s *api.Status) { s.HTTPS = &block })
}

func (m *HTTPS) trust(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.HTTPSTrustParams](params)
	if err != nil {
		return nil, err
	}
	switch p.Action {
	case api.TrustInstall, api.TrustRemove, api.TrustRotate:
	default:
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "action is install, remove or rotate, not %q", clip(p.Action, 20))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st, err := m.Keeper.Trust(ctx, p.Action)
	if err != nil {
		return nil, jsonrpc.Errorf(api.CodeFailed, "%s", clip(err.Error(), 320))
	}
	m.host.Log().Info("https: the Mac's trust in fev's root", "action", p.Action, "trust", st.Trust, "root", st.Root)
	m.publish(st)
	st.Enabled = m.host.Config().Bool(api.KeyNetworkHTTPS)
	return st, nil
}

type DomainsSource interface {
	Status() api.DomainsStatus
	Subscribe(fn func()) (cancel func())
}

type Domains struct {
	Source DomainsSource

	host   Host
	cancel func()
}

func (m *Domains) Name() string                   { return "domains" }
func (m *Domains) Marks() []string                { return nil }
func (m *Domains) StateChanged(api.Status)        {}
func (m *Domains) ConfigChanged(api.ConfigChange) {}

func (m *Domains) Start(h Host) error {
	m.host = h
	m.cancel = m.Source.Subscribe(m.publish)
	m.publish()
	return nil
}

func (m *Domains) Close() {
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *Domains) publish() {
	block := m.Source.Status()
	updateStatus(m.host, func(s *api.Status) { s.Domains = &block })
}

type NoCA struct{}

var errNoCA = errors.New("this daemon has no CA: a daemon with a home of its own uses the login keychain's CA only as the user's own daemon, and a file CA with --ca-dir")

func (NoCA) State() api.HTTPSStatus {
	return api.HTTPSStatus{Trust: api.TrustAbsent, Key: api.KeyNone, Error: errNoCA.Error()}
}

func (NoCA) Trust(context.Context, string) (api.HTTPSStatus, error) {
	return NoCA{}.State(), errNoCA
}
