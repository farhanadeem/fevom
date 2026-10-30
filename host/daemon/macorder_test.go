package daemon

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"fev/proto/agent"
	api "fev/proto/daemon"
)

func TestMachinesStartAgainOnlyAfterTheGuestHasTheMacsAddresses(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var sets [][]string
	g := &GuestNet{
		interfaces: func() ([]MacAddress, error) {
			return []MacAddress{{netip.MustParseAddr("127.0.0.1"), "lo0"}, {netip.MustParseAddr("192.0.2.9"), "en0"}}, nil
		},
		macSet: func(ctx context.Context, _ *agent.Client, a []string) error {
			mu.Lock()
			sets = append(sets, a)
			n := len(sets)
			mu.Unlock()
			if n == 2 {
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		},
	}
	f := machinesDaemon(t, func(m *Machines) {
		m.BeforeResume = func(boot string) { g.WaitMacTold(boot, MacToldWait) }
	}, func(fx *fixture) { fx.opts.Modules = append(fx.opts.Modules, g) })
	f.create("dev", "ubuntu")
	if _, err := f.c.VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	f.stopped()
	if _, err := f.c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if l, _ := f.c.Machines(bg); names(l)["dev"] == api.MachineRunning {
			t.Fatal("dev was started again before the guest had the Mac's addresses")
		}
		time.Sleep(50 * time.Millisecond)
	}
	close(release)
	f.machineIs("dev", api.MachineRunning)
	mu.Lock()
	defer mu.Unlock()
	if len(sets) < 2 || len(sets[1]) != 1 || sets[1][0] != "192.0.2.9" {
		t.Errorf("the sends: %q, want the Mac's address as it is now, not loopback's", sets)
	}
}

func TestAFailedSendOfTheMacsAddressesIsRetriedOnce(t *testing.T) {
	f := newFixture(t)
	f.start()
	calls := make(chan time.Time, 16)
	var mu sync.Mutex
	fail := 1
	g := &GuestNet{
		macRetry:   300 * time.Millisecond,
		interfaces: func() ([]MacAddress, error) { return []MacAddress{{netip.MustParseAddr("192.0.2.9"), "en0"}}, nil },
		macSet: func(context.Context, *agent.Client, []string) error {
			calls <- time.Now()
			mu.Lock()
			defer mu.Unlock()
			if fail > 0 {
				fail--
				return errors.New("the machines' network is not up")
			}
			return nil
		},
	}
	if err := g.Start(moduleHost{f.d}); err != nil {
		t.Fatal(err)
	}
	vm := newMacVM(t, "b1")
	g.host = agentHost{Host: moduleHost{f.d}, vm: vm}
	next := func(within time.Duration) time.Time {
		t.Helper()
		select {
		case at := <-calls:
			return at
		case <-time.After(within):
			t.Fatalf("no send within %v", within)
			return time.Time{}
		}
	}
	none := func(why string, d time.Duration) {
		t.Helper()
		select {
		case <-calls:
			t.Fatalf("a send %s", why)
		case <-time.After(d):
		}
	}
	told := g.MacTold("b1")
	g.StateChanged(api.Status{State: api.StateRunning, BootID: "b1"})
	first := next(2 * time.Second)
	select {
	case <-told:
		t.Fatal("the machines were let go after a failed send, before its retry")
	case <-time.After(100 * time.Millisecond):
	}
	second := next(2 * time.Second)
	if d := second.Sub(first); d < 250*time.Millisecond {
		t.Errorf("the retry came %v after the failure, want the timer's", d)
	}
	select {
	case <-told:
	case <-time.After(2 * time.Second):
		t.Fatal("the machines are still held after the retry landed")
	}
	none("after the retry landed", 700*time.Millisecond)

	mu.Lock()
	fail = 2
	mu.Unlock()
	g.MacChanged()
	next(2 * time.Second)
	next(2 * time.Second)
	none("after the retry failed too", 900*time.Millisecond)
	g.MacChanged()
	next(2 * time.Second)
	none("after a change sent and landed", 700*time.Millisecond)

	mu.Lock()
	fail = 2
	mu.Unlock()
	g.StateChanged(api.Status{State: api.StateStopped})
	vm2 := newMacVM(t, "b2")
	g.host = agentHost{Host: moduleHost{f.d}, vm: vm2}
	told2 := g.MacTold("b2")
	g.StateChanged(api.Status{State: api.StateStarting, Phase: api.PhaseEngine, BootID: "b2"})
	next(2 * time.Second)
	next(2 * time.Second)
	select {
	case <-told2:
	case <-time.After(2 * time.Second):
		t.Fatal("the machines are held after the send and its retry both failed")
	}
}
