package daemon

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"fev/proto/agent"
	api "fev/proto/daemon"
)

type Emulation struct {
	host Host
	log  *slog.Logger

	mu                sync.Mutex
	given             bool
	boot              string
	refreshing, dirty bool
}

const emulationCallBound = 10 * time.Second

func (e *Emulation) Name() string    { return "emulation" }
func (e *Emulation) Marks() []string { return nil }

func (e *Emulation) Start(h Host) error {
	e.host, e.log = h, h.Log()
	return nil
}

func (e *Emulation) BootWords() []string {
	if e == nil || e.host == nil {
		return nil
	}
	on := e.host.Config().Bool(api.KeyEmulationX86)
	e.mu.Lock()
	e.given = on
	e.mu.Unlock()
	var words []string
	engine, started := e.host.Config().InEffect(api.KeyEmulationEngine)
	if !started {
		if v, err := e.host.Config().Get(api.KeyEmulationEngine); err == nil {
			engine = v[api.KeyEmulationEngine].Value
		}
	}
	if engine == agent.EmulationEngineRosetta || engine == agent.EmulationEngineAuto {
		words = append(words, agent.EmulationEngineParam+"="+engine.(string))
	}
	if !on {
		words = append(words, agent.EmulationCmdlineParam+"=0")
	}
	return words
}

func (e *Emulation) StateChanged(st api.Status) {
	if st.State != api.StateRunning || st.BootID == "" {
		return
	}
	e.mu.Lock()
	first := st.BootID != e.boot
	if first {
		e.boot = st.BootID
	}
	given := e.given
	e.mu.Unlock()
	if !first {
		return
	}
	e.host.Config().SetInEffect(api.KeyEmulationX86, given)
	go e.refresh(st.BootID)
}

func (e *Emulation) refresh(boot string) {
	vm, ok := e.host.VM()
	if !ok || vm.BootID() != boot || vm.Agent() == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), emulationCallBound)
	defer cancel()
	g, err := vm.Agent().EmulationStatus(ctx)
	if err != nil {
		e.log.Warn("emulation: the guest did not say what its x86 translator is", "boot", boot, "err", err)
		return
	}
	e.show(boot, g)
}

const statusAskBound = 2 * time.Second

func (e *Emulation) StatusAsked(ctx context.Context) {
	e.mu.Lock()
	boot := e.boot
	e.mu.Unlock()
	vm, ok := e.host.VM()
	if !ok || boot == "" || vm.BootID() != boot || vm.Agent() == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, statusAskBound)
	defer cancel()
	g, err := vm.Agent().EmulationStatus(cctx)
	if err != nil {
		e.log.Debug("emulation: the guest's table was not read for the status", "boot", boot, "err", err)
		return
	}
	e.show(boot, g)
}

func (e *Emulation) BeforeEngineRestart(ctx context.Context, vm VM) error {
	a := vm.Agent()
	if a == nil {
		return errors.New("the guest's agent is not connected")
	}
	on := e.host.Config().Bool(api.KeyEmulationX86)
	cctx, cancel := context.WithTimeout(ctx, emulationCallBound)
	defer cancel()
	g, err := a.EmulationSet(cctx, on)
	if err != nil {
		e.log.Error("emulation: emulation.x86 was not applied at the engine's restart", "x86", on, "err", err)
		return err
	}
	e.host.Config().SetInEffect(api.KeyEmulationX86, on)
	e.log.Info("emulation: emulation.x86 applied at the engine's restart", "x86", on, "registered", g.Registered)
	e.show(vm.BootID(), g)
	return nil
}

func (e *Emulation) Event(event any) {
	d, ok := event.(api.DockerEvent)
	if !ok || d.Type != "container" || e.host == nil {
		return
	}
	switch d.Action {
	case "start", "die", "destroy", "stop", "rename":
	default:
		return
	}
	st := e.host.Status()
	if st.Emulation == nil || st.Emulation.Mode != "auto" && st.Emulation.Translator != agent.EmulationRosetta {
		return
	}
	e.mu.Lock()
	e.dirty = true
	if e.refreshing {
		e.mu.Unlock()
		return
	}
	e.refreshing = true
	e.mu.Unlock()
	go func() {
		for {
			e.mu.Lock()
			if !e.dirty {
				e.refreshing = false
				e.mu.Unlock()
				return
			}
			e.dirty = false
			boot := e.boot
			e.mu.Unlock()
			e.refresh(boot)
		}
	}()
}

func (e *Emulation) ConfigChanged(ch api.ConfigChange) {
	if ch.Key != api.KeyEmulationX86 && ch.Key != api.KeyEmulationEngine {
		return
	}
	pending := ch.Pending
	updateStatus(e.host, func(st *api.Status) {
		if st.Emulation == nil {
			return
		}
		b := *st.Emulation
		if ch.Key == api.KeyEmulationX86 {
			b.Pending = pending
		} else {
			b.EnginePending = pending
		}
		if b.Pending != st.Emulation.Pending || b.EnginePending != st.Emulation.EnginePending {
			st.Emulation = &b
		}
	})
}

func (e *Emulation) Close() {}

func (e *Emulation) show(boot string, g *agent.EmulationStatus) {
	b := emulationBlock(g)
	if v, err := e.host.Config().Get(api.KeyEmulationX86); err == nil {
		b.Pending = v[api.KeyEmulationX86].Pending
	}
	if v, err := e.host.Config().Get(api.KeyEmulationEngine); err == nil {
		b.EnginePending = v[api.KeyEmulationEngine].Pending
	}
	updateStatus(e.host, func(st *api.Status) {
		if st.BootID == boot {
			st.Emulation = b
		}
	})
}

func emulationBlock(g *agent.EmulationStatus) *api.EmulationStatus {
	b := &api.EmulationStatus{Translator: g.Translator, Version: g.Version, HardwareOrdering: g.MemoryModel, Restored: g.Restored}
	b.Mode, b.Reason = g.Mode, g.Reason
	for _, w := range g.Workloads {
		b.Workloads = append(b.Workloads, api.EmulationWorkload{ID: w.ID, Kind: w.Kind, Name: w.Name, Engine: w.Engine, Reason: w.Reason})
	}
	switch {
	case g.X86 && g.Registered && g.TableEnabled && g.Error == "":
		b.State = api.EmulationOn
	case g.X86:
		b.State, b.Error = api.EmulationUnavailable, g.Error
		if b.Error == "" {
			b.Error = "fev's x86 entries are not in force in the guest's binfmt_misc table"
		}
	default:
		b.State = api.EmulationOff
	}
	return b
}
