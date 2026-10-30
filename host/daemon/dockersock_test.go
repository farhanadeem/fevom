package daemon

import (
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	api "fev/proto/daemon"
	priv "fev/proto/privhelper"
)

type linkSpy struct {
	HelperClient
	asked bool
	state string
	mu    sync.Mutex
	links int
}

func (l *linkSpy) Call(ctx context.Context, method string, params, result any) (*os.File, error) {
	switch method {
	case priv.MethodHello:
		f, err := l.HelperClient.Call(ctx, method, params, result)
		if h, ok := result.(*priv.HelloResult); ok && err == nil {
			h.DockerSockLink, h.DockerSock = l.asked, l.state
		}
		return f, err
	case priv.MethodDockerSockLink:
		l.mu.Lock()
		l.links++
		l.mu.Unlock()
		if st, ok := result.(*priv.DockerSock); ok {
			*st = priv.DockerSock{State: priv.StateFev, Target: "/Users/x/.fev/run/docker.sock"}
		}
		return nil, nil
	}
	return l.HelperClient.Call(ctx, method, params, result)
}

func (l *linkSpy) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.links
}

func TestTheDaemonHasTheHelperMakeTheLinkTheUserAskedFor(t *testing.T) {
	for _, c := range []struct {
		asked bool
		state string
		want  int
	}{
		{true, priv.StateAbsent, 1},
		{true, priv.StateForeign, 1},
		{true, priv.StateFev, 0},
		{false, priv.StateAbsent, 0},
	} {
		f, n, h, _, _ := direct(t, "")
		spy := &linkSpy{HelperClient: n.Helper, asked: c.asked, state: c.state}
		n.Helper = spy
		if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
			t.Fatal(err)
		}
		f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOn && len(b.Routes) > 0 })
		deadline := time.Now().Add(2 * time.Second)
		for spy.count() < c.want && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := spy.count(); got != c.want {
			t.Errorf("asked %v, the link %s: dockersock.link called %d times, want %d", c.asked, c.state, got, c.want)
		}
		if slices.Contains(h.Calls(), priv.MethodDockerSockUnlink) {
			t.Errorf("the daemon removed the link: %v", h.Calls())
		}
		f.client().VMStop(bg, api.StopParams{TimeoutMS: 5000})
	}
}

func TestTheLinkIsMadeWhateverDirectAccessSays(t *testing.T) {
	f, n, h, _, _ := direct(t, "")
	spy := &linkSpy{HelperClient: n.Helper, asked: true, state: priv.StateAbsent}
	n.Helper = spy
	if _, err := f.client().ConfigSet(bg, map[string]any{api.KeyNetworkDirectAccess: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	waitLinks := func(want int, what string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for spy.count() < want && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		if got := spy.count(); got != want {
			t.Errorf("%s: dockersock.link called %d times, want %d; calls %v", what, got, want, h.Calls())
		}
	}
	waitLinks(1, "direct access off")
	if slices.Contains(h.Calls(), priv.MethodTunnelOpen) || slices.Contains(h.Calls(), priv.MethodRoutesSet) {
		t.Errorf("direct access off opened a tunnel: %v", h.Calls())
	}
	if b := f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOff }); b.Reason != api.DirectSetting {
		t.Errorf("direct access off: %+v", b)
	}
	f.client().VMStop(bg, api.StopParams{TimeoutMS: 5000})

	if _, err := f.client().ConfigSet(bg, map[string]any{api.KeyAdminEnabled: false}); err != nil {
		t.Fatal(err)
	}
	calls := len(h.Calls())
	f.client().VMStart(bg, api.StartParams{})
	f.network(func(b *api.NetworkStatus) bool { return b.Reason == api.DirectAdminOff })
	waitLinks(1, "admin off")
	if got := h.Calls()[calls:]; len(got) != 0 {
		t.Errorf("admin off asked the helper %v", got)
	}
	if _, err := f.client().ConfigSet(bg, map[string]any{api.KeyAdminEnabled: true}); err != nil {
		t.Fatal(err)
	}
	waitLinks(2, "admin on again")
	if slices.Contains(h.Calls(), priv.MethodTunnelOpen) {
		t.Errorf("a tunnel with direct access off: %v", h.Calls())
	}
	f.client().VMStop(bg, api.StopParams{TimeoutMS: 5000})
	if _, err := f.client().ConfigSet(bg, map[string]any{api.KeyNetworkDirectAccess: true}); err != nil {
		t.Fatal(err)
	}
	f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOn && len(b.Routes) > 0 })
	waitLinks(3, "direct access on")
	f.client().VMStart(bg, api.StartParams{})
	waitLinks(4, "a start with the tunnel kept")
	if got := strings.Count(strings.Join(h.Calls(), " "), priv.MethodTunnelOpen); got != 1 {
		t.Errorf("%d tunnels opened: %v", got, h.Calls())
	}
	if slices.Contains(h.Calls(), priv.MethodDockerSockUnlink) {
		t.Errorf("the daemon removed the link: %v", h.Calls())
	}
}
