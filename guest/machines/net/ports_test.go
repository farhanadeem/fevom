package machnet

import (
	"net/netip"
	"reflect"
	"testing"

	"fev/guest/internal/listen"
	api "fev/proto/agent"
)

func lst(proto, addr string, port uint16, netns, cookie uint64) listen.Listener {
	return listen.Listener{Proto: proto, Addr: netip.MustParseAddr(addr), Port: port, NetNS: netns, Cookie: cookie}
}

func TestAMachinesPortsFollowItsNamespaceAlone(t *testing.T) {
	p := newPorts()
	p.add("dev", netip.MustParseAddr("198.19.0.10"), 42, func(ns uint64) []listen.Listener {
		if ns != 42 {
			t.Errorf("seed asked for namespace %d", ns)
		}
		return []listen.Listener{lst("udp", "198.19.0.10", 68, 42, 1)}
	})
	p.add("f43", netip.MustParseAddr("198.19.0.11"), 43, nil)
	first := p.next()
	if len(first) != 2 || first[0].Name != "dev" || len(first[0].Ports) != 1 || first[1].Name != "f43" || len(first[1].Ports) != 0 {
		t.Fatalf("first: %+v", first)
	}
	if len(p.next()) != 0 {
		t.Fatal("told twice")
	}
	for _, ev := range []listen.Event{
		{Listener: lst("tcp", "0.0.0.0", 3000, 42, 2), Added: true},
		{Listener: lst("tcp", "0.0.0.0", 3000, 42, 3), Added: true},
		{Listener: lst("tcp", "::", 8080, 42, 4), Added: true},
		{Listener: lst("tcp", "0.0.0.0", 80, 7, 5), Added: true},
		{Listener: lst("tcp", "0.0.0.0", 81, 280, 6), Added: true},
		{Listener: lst("tcp", "::", 8080, 42, 4)},
		{Listener: lst("tcp", "127.0.0.1", 9, 42, 99)},
	} {
		p.event(ev)
	}
	got := p.next()
	want := []api.MachinePort{{Proto: "tcp", Address: "0.0.0.0", Port: 3000}, {Proto: "udp", Address: "198.19.0.10", Port: 68}}
	if len(got) != 1 || got[0].Name != "dev" || got[0].Address != "198.19.0.10" || !reflect.DeepEqual(got[0].Ports, want) {
		t.Fatalf("after the burst: %+v", got)
	}
	if got[0].Seq <= first[1].Seq {
		t.Errorf("seq %d not past %d", got[0].Seq, first[1].Seq)
	}
	p.event(listen.Event{Listener: lst("tcp", "0.0.0.0", 3000, 42, 2)})
	if got := p.next(); len(got) != 1 || len(got[0].Ports) != 2 {
		t.Fatalf("a twin closed: %+v", got)
	}
	l := p.list()
	if len(l.Machines) != 2 || l.Machines[0].Name != "dev" || l.Machines[1].Name != "f43" {
		t.Fatalf("list: %+v", l)
	}
	last := l.Machines[0].Seq
	p.remove("dev", 42)
	p.event(listen.Event{Listener: lst("tcp", "0.0.0.0", 3000, 42, 3)})
	got = p.next()
	if len(got) != 1 || got[0].Name != "dev" || len(got[0].Ports) != 0 || got[0].Seq <= last || got[0].Ports == nil {
		t.Fatalf("unplugged: %+v", got)
	}
	if l := p.list(); len(l.Machines) != 1 {
		t.Errorf("list after: %+v", l)
	}
	p.add("dev", netip.Addr{}, 44, nil)
	p.next()
	p.remove("dev", 42)
	if got := p.next(); len(got) != 0 {
		t.Errorf("stale remove told: %+v", got)
	}
}

func TestARenamedMachinesPortsFollowItsName(t *testing.T) {
	p := newPorts()
	p.add("alp", netip.MustParseAddr("198.19.0.12"), 50, nil)
	p.event(listen.Event{Listener: lst("tcp", "0.0.0.0", 8080, 50, 1), Added: true})
	before := p.next()
	if len(before) != 1 || before[0].Name != "alp" || len(before[0].Ports) != 1 {
		t.Fatalf("before: %+v", before)
	}
	p.rename("alp", "alp-r")
	got := p.next()
	if len(got) != 2 || got[0].Name != "alp" || len(got[0].Ports) != 0 || got[0].Seq <= before[0].Seq ||
		got[1].Name != "alp-r" || got[1].Address != "198.19.0.12" || len(got[1].Ports) != 1 || got[1].Seq <= got[0].Seq {
		t.Fatalf("renamed: %+v", got)
	}
	if l := p.list(); len(l.Machines) != 1 || l.Machines[0].Name != "alp-r" {
		t.Fatalf("list after the rename: %+v", l)
	}
	p.event(listen.Event{Listener: lst("tcp", "0.0.0.0", 9090, 50, 2), Added: true})
	if got := p.next(); len(got) != 1 || got[0].Name != "alp-r" || len(got[0].Ports) != 2 {
		t.Fatalf("an event after the rename: %+v", got)
	}
	p.rename("alp-r", "alp")
	p.next()
	p.remove("alp", 50)
	got = p.next()
	if len(got) != 1 || got[0].Name != "alp" || len(got[0].Ports) != 0 {
		t.Fatalf("unplugged: %+v", got)
	}
	if l := p.list(); len(l.Machines) != 0 {
		t.Fatalf("left after the stop: %+v", l)
	}
	p.add("u24", netip.MustParseAddr("198.19.0.13"), 51, nil)
	p.rename("u24", "u24-r")
	p.next()
	p.remove("u24", 51)
	if got := p.next(); len(got) != 1 || got[0].Name != "u24-r" || len(got[0].Ports) != 0 {
		t.Fatalf("an unplug under the old name: %+v", got)
	}
	if l := p.list(); len(l.Machines) != 0 {
		t.Fatalf("left: %+v", l)
	}
}
