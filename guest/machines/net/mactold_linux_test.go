package machnet

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"fev/guest/machines"
)

func TestAnIsolatedNetworkWaitsForTheMacsAddresses(t *testing.T) {
	n := New(Options{})
	n.macToldBound = 200 * time.Millisecond
	_, err := n.Plug(machines.Plug{Name: "iso", IsolateNetwork: true})
	if err == nil || !strings.Contains(err.Error(), "mac_addresses.set") {
		t.Fatalf("an isolate_network plug before the Mac's addresses: %v, want the refusal", err)
	}

	n.macToldBound = 5 * time.Second
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- n.waitMacTold() }()
	select {
	case err := <-done:
		t.Fatalf("the wait ended before any mac_addresses.set: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := n.SetMacAddresses([]netip.Addr{netip.MustParseAddr("192.0.2.5")}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the wait after the set: %v", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("the wait took %v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the wait did not end with the set")
	}
	if err := n.waitMacTold(); err != nil {
		t.Error(err)
	}
	m := New(Options{})
	m.macToldBound = 200 * time.Millisecond
	if err := m.SetMacAddresses(nil); err != nil {
		t.Fatal(err)
	}
	if err := m.waitMacTold(); err != nil {
		t.Errorf("after an empty set: %v", err)
	}
}
