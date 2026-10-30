package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"

	"fev/host/netstack"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/version"
	"fev/proto/vmmctl"
)

type fakeMacNet struct {
	mu    sync.Mutex
	own   []MacAddress
	known bool
}

func (m *fakeMacNet) GlobalIPv6() bool                 { return false }
func (m *fakeMacNet) Addresses() netstack.MacAddresses { return nil }
func (m *fakeMacNet) Own() ([]MacAddress, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]MacAddress(nil), m.own...), m.known
}
func (m *fakeMacNet) set(known bool, own ...MacAddress) {
	m.mu.Lock()
	m.own, m.known = own, known
	m.mu.Unlock()
}

type macVM struct {
	client *agent.Client
	boot   string
	sets   chan []string
}

func (v *macVM) VsockPath(uint32) string { return "/nonexistent" }
func (v *macVM) Agent() *agent.Client    { return v.client }
func (v *macVM) BootID() string          { return v.boot }
func (v *macVM) Control() *vmmctl.Client { return nil }

func newMacVM(t *testing.T, boot string) *macVM {
	t.Helper()
	v := &macVM{boot: boot, sets: make(chan []string, 8)}
	cs, ss := net.Pipe()
	server := jsonrpc.NewConn(ss, jsonrpc.HandlerFunc(func(_ context.Context, _ *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
		switch method {
		case agent.MethodHello:
			return agent.HelloResult{APIVersion: agent.APIVersion, AgentVersion: version.Build}, nil
		case agent.MethodMacAddressesSet:
			p, err := jsonrpc.Bind[agent.MacAddresses](params)
			if err != nil {
				return nil, err
			}
			if err := agent.CheckMacAddresses(p); err != nil {
				return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
			}
			v.sets <- p.Addresses
			return nil, nil
		}
		return nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "%s", method)
	}))
	t.Cleanup(func() { server.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	if v.client, _, err = agent.NewClient(ctx, cs, "test"); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTheGuestIsToldTheMacsAddresses(t *testing.T) {
	f := newFixture(t)
	f.start()
	mac := &fakeMacNet{}
	g := &GuestNet{Mac: mac}
	if err := g.Start(moduleHost{f.d}); err != nil {
		t.Fatal(err)
	}
	vm := newMacVM(t, "b1")
	g.host = agentHost{Host: moduleHost{f.d}, vm: vm}
	next := func() []string {
		t.Helper()
		select {
		case got := <-vm.sets:
			return got
		case <-time.After(5 * time.Second):
			t.Fatal("no mac_addresses.set within 5 s")
			return nil
		}
	}
	none := func(why string) {
		t.Helper()
		select {
		case got := <-vm.sets:
			t.Fatalf("%s: mac_addresses.set %q", why, got)
		case <-time.After(200 * time.Millisecond):
		}
	}

	g.StateChanged(api.Status{State: api.StateStarting, Phase: api.PhaseInit, BootID: "b1"})
	g.MacChanged()
	none("before init is ready")

	g.StateChanged(api.Status{State: api.StateRunning, BootID: "b1"})
	none("with the Mac's network unknown")

	g.mu.Lock()
	g.iface = "utun7"
	g.mu.Unlock()
	mac.set(true,
		MacAddress{netip.MustParseAddr("127.0.0.1"), "lo0"},
		MacAddress{netip.MustParseAddr("::1"), "lo0"},
		MacAddress{netip.MustParseAddr("fe80::1%lo0"), "lo0"},
		MacAddress{netip.MustParseAddr("192.168.1.20"), "en0"},
		MacAddress{netip.MustParseAddr("fe80::aa:bb%en0"), "en0"},
		MacAddress{netip.MustParseAddr("2001:db8::20"), "en0"},
		MacAddress{netip.MustParseAddr("100.64.0.5"), "utun3"},
		MacAddress{netip.MustParseAddr("198.19.3.1"), "utun7"},
	)
	g.MacChanged()
	want := []string{"100.64.0.5", "192.168.1.20", "2001:db8::20", "fe80::aa:bb%en0"}
	if got := next(); !reflect.DeepEqual(got, want) {
		t.Errorf("after the Mac's network became known: %q, want %q", got, want)
	}

	mac.set(true, MacAddress{netip.MustParseAddr("10.0.0.9"), "en0"})
	g.MacChanged()
	if got := next(); !reflect.DeepEqual(got, []string{"10.0.0.9"}) {
		t.Errorf("after a change: %q", got)
	}

	g.StateChanged(api.Status{State: api.StateStopped})
	g.MacChanged()
	none("while stopped")
	vm2 := newMacVM(t, "b2")
	g.host = agentHost{Host: moduleHost{f.d}, vm: vm2}
	g.StateChanged(api.Status{State: api.StateStarting, Phase: api.PhaseEngine, BootID: "b2"})
	select {
	case got := <-vm2.sets:
		if !reflect.DeepEqual(got, []string{"10.0.0.9"}) {
			t.Errorf("at the next boot: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no mac_addresses.set at the next boot")
	}
}
