package daemon

import (
	"context"
	"strings"
	"sync"
	"testing"

	api "fev/proto/daemon"
)

type fakeKeeper struct {
	mu      sync.Mutex
	st      api.HTTPSStatus
	actions []string
}

func (k *fakeKeeper) State() api.HTTPSStatus {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.st
}

func (k *fakeKeeper) Trust(_ context.Context, action string) (api.HTTPSStatus, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.actions = append(k.actions, action)
	switch action {
	case api.TrustInstall:
		k.st.Trust, k.st.Root, k.st.Key = api.TrustTrusted, strings.Repeat("ab", 32), api.KeySigner
	case api.TrustRemove:
		k.st.Trust = api.TrustAbsent
	}
	return k.st, nil
}

type fakeDomains struct {
	mu  sync.Mutex
	st  api.DomainsStatus
	fns []func()
}

func (d *fakeDomains) Status() api.DomainsStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.st
}

func (d *fakeDomains) Subscribe(fn func()) func() {
	d.mu.Lock()
	d.fns = append(d.fns, fn)
	d.mu.Unlock()
	return func() {}
}

func TestTheHTTPSAndDomainsBlocksAreTheirKeepers(t *testing.T) {
	f := newFixture(t)
	k := &fakeKeeper{st: api.HTTPSStatus{Trust: api.TrustAbsent, Key: api.KeyNone}}
	d := &fakeDomains{st: api.DomainsStatus{Resolver: "absent", Port: 15353}}
	f.opts.Modules = []Module{&HTTPS{Keeper: k}, &Domains{Source: d}}
	f.start()
	c := f.client()
	st, _ := c.Status(bg)
	if st.HTTPS == nil || st.HTTPS.Trust != api.TrustAbsent || !st.HTTPS.Enabled || st.Domains == nil || st.Domains.Port != 15353 {
		t.Fatalf("status: %+v %+v", st.HTTPS, st.Domains)
	}
	got, err := c.HTTPSTrust(bg, api.TrustInstall)
	if err != nil || got.Trust != api.TrustTrusted || !got.Enabled {
		t.Fatalf("https.trust: %+v %v", got, err)
	}
	if st, _ := c.Status(bg); st.HTTPS.Trust != api.TrustTrusted || st.HTTPS.Root != got.Root {
		t.Errorf("after: %+v", st.HTTPS)
	}
	if _, err := c.HTTPSTrust(bg, "trust-everything"); code(err) != -32602 {
		t.Errorf("an unknown action: %v", err)
	}
	if _, err := c.ConfigSet(bg, map[string]any{api.KeyNetworkHTTPS: false}); err != nil {
		t.Fatal(err)
	}
	f.until("https off", func(st api.Status) bool {
		return st.HTTPS != nil && !st.HTTPS.Enabled && st.HTTPS.Trust == api.TrustTrusted
	})
	d.mu.Lock()
	d.st.Names = 3
	fns := d.fns
	d.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
	f.until("names", func(st api.Status) bool { return st.Domains != nil && st.Domains.Names == 3 })
}
