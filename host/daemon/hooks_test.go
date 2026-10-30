package daemon

import (
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	api "fev/proto/daemon"
)

type hooked struct {
	stub
	hmu   sync.Mutex
	calls []string
	h     Host
}

func (m *hooked) Start(h Host) error { m.h = h; return nil }

func (m *hooked) note(s string) {
	m.hmu.Lock()
	m.calls = append(m.calls, s)
	m.hmu.Unlock()
}

func (m *hooked) BeforeStop(boot string) {
	vm, ok := m.h.VM()
	m.note("before-stop " + boot + " " + m.h.Status().State + " " + map[bool]string{true: "agent", false: "no-agent"}[ok && vm.Agent() != nil])
}
func (m *hooked) VMEnded(boot string) { m.note("ended " + boot) }
func (m *hooked) Event(e any) {
	if d, ok := e.(api.DockerEvent); ok {
		m.note("docker " + d.Type + " " + d.Action)
	}
}

func (m *hooked) seen() []string {
	m.hmu.Lock()
	defer m.hmu.Unlock()
	return append([]string{}, m.calls...)
}

func (m *hooked) wait(t *testing.T, prefix string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range m.seen() {
			if strings.HasPrefix(c, prefix) {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %q among %v", prefix, m.seen())
}

func TestTheStopsAndTheEndsHooksAreCalledInOrder(t *testing.T) {
	m := &hooked{}
	f := newFixture(t)
	f.opts.Modules = []Module{m}
	f.start()
	c := f.client()
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	boot := f.running().BootID
	if _, err := c.VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	f.stopped()
	m.wait(t, "ended "+boot)
	if got := strings.Join(m.seen(), "; "); got != "before-stop "+boot+" stopping agent; ended "+boot {
		t.Fatalf("a stop's hooks: %s", got)
	}

	m.hmu.Lock()
	m.calls = nil
	m.hmu.Unlock()
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	boot = f.running().BootID
	info, _ := f.layout.ReadInfo()
	syscall.Kill(info.VMMPID, syscall.SIGKILL)
	m.wait(t, "ended "+boot)
	if got := m.seen(); len(got) != 1 {
		t.Fatalf("a crash's hooks: %v", got)
	}

	h := moduleHost{f.d}
	h.PublishDocker(api.DockerEvent{Type: "container", Action: "die", ID: "a"})
	h.PublishDocker(api.DockerEvent{Type: "volume", Action: "destroy", ID: "v"})
	m.wait(t, "docker volume destroy")
	var events []string
	for _, c := range m.seen() {
		if strings.HasPrefix(c, "docker ") {
			events = append(events, c)
		}
	}
	if strings.Join(events, ", ") != "docker container die, docker volume destroy" {
		t.Fatalf("events: %v", events)
	}
}
