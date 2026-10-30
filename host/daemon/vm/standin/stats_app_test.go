package standin

import (
	"testing"
	"time"

	api "fev/proto/agent"
)

func TestTheSampleFollowsTheEnginesContainersAndTheMachines(t *testing.T) {
	a := booted(t)
	a.Containers = func() []StatsName {
		return []StatsName{{ID: "bb", Name: "db"}, {ID: "aa", Name: "web"}}
	}
	a.AddMachine("zed", api.MachineConfig{User: "u"})
	a.AddMachine("alp", api.MachineConfig{User: "u"})
	a.AddMachine("off", api.MachineConfig{User: "u"})
	m := a.machines()
	m.mu.Lock()
	m.byName["zed"].Info.State = api.MachineRunning
	m.byName["alp"].Info.State = api.MachineRunning
	m.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.mu.Lock()
		ready := a.engine.State == api.EngineReady
		a.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the engine was not ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	s1 := a.sample(1)
	time.Sleep(20 * time.Millisecond)
	s2 := a.sample(1)
	var got []string
	for _, g := range s2.Groups {
		got = append(got, g.Kind+":"+g.Name+":"+g.ID)
	}
	want := []string{"system:guest:", "service:engine:", "container:web:aa", "container:db:bb", "machine:alp:", "machine:zed:"}
	if len(got) != len(want) {
		t.Fatalf("groups %v, want %v", got, want)
	}
	for i := range want {
		g := s2.Groups[i]
		if g.Kind == api.StatsMachine {
			if g.Kind+":"+g.Name+":" != want[i] || g.ID == "" {
				t.Errorf("group %d: %v, want %v with the machine's uuid", i, got[i], want[i])
			}
			continue
		}
		if got[i] != want[i] {
			t.Errorf("group %d: %v, want %v", i, got[i], want[i])
		}
	}
	for i := range s2.Groups {
		if s2.Groups[i].CPUUsec <= s1.Groups[i].CPUUsec {
			t.Errorf("%s's CPU time did not grow: %d then %d", s2.Groups[i].Name, s1.Groups[i].CPUUsec, s2.Groups[i].CPUUsec)
		}
	}
	if s2.Groups[2].CPUUsec == s2.Groups[3].CPUUsec {
		t.Errorf("two containers at one rate: %d", s2.Groups[2].CPUUsec)
	}
}
