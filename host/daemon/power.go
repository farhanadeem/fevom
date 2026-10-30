package daemon

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"sync"
	"time"

	"fev/host/daemon/boottrace"
	"fev/host/power"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

type Holder interface {
	Held() int
}

func (d *Daemon) powerLoop(src power.Source) {
	defer close(d.powerDone)
	for {
		select {
		case <-d.powerStop:
			return
		case e, ok := <-src.Events():
			if !ok {
				return
			}
			var outcome any
			switch e.Kind {
			case power.WillSleep:
				outcome = d.willSleep(e)
			case power.HasPoweredOn:
				outcome, _ = d.wake("")
			case power.WillNotSleep:
				outcome, _ = d.wake(api.WakeNotSleep)
			case power.WillPowerOn:
				outcome = d.willPowerOn()
			default:
				e.Ack()
			}
			if e.Handled != nil {
				e.Handled(outcome)
			}
		}
	}
}

func (d *Daemon) willSleep(e power.Event) api.PowerSleep {
	var rec api.PowerSleep
	pause := d.cfg.Bool(api.KeyPowerPauseInSleep)
	if pause && !d.shuttingDown.Load() {
		rec = d.sup.Pause()
	}
	rec.Time = stamp(time.Now())
	d.log.Info("sleep: the Mac is going to sleep", "paused", rec.Paused, "changed", rec.Changed,
		"pause_ms", math.Round(rec.PauseMS*10)/10, "timed_out", rec.TimedOut, "pause_in_sleep", pause)
	d.hub.Publish(rec)
	e.Ack()
	return rec
}

var UnfinishedAfter = 5 * time.Second

var wakeLines = map[string]string{
	"":                 "wake: the Mac has powered on",
	api.WakeNotSleep:   "wake: the Mac called the sleep off",
	api.WakeUnfinished: "wake: the Mac said it would power on and never said it had powered on; a call needed the guest",
	api.WakeCommand:    "wake: a command asked for the VM while it was paused for the Mac's sleep",
}

func (d *Daemon) held() int {
	held := 0
	for _, m := range d.opts.Modules {
		if h, ok := m.(Holder); ok {
			held += h.Held()
		}
	}
	return held
}

func (d *Daemon) wake(cause string) (api.PowerWake, bool) {
	began := time.Now()
	rec, ended := d.sup.Wake(cause, d.held)
	rec.Time = stamp(time.Now())
	if !ended && cause != "" && cause != api.WakeNotSleep {
		return rec, false
	}
	if cause == "" || cause == api.WakeNotSleep {
		for _, m := range d.opts.Modules {
			if w, ok := m.(Waker); ok {
				go w.Woke()
			}
		}
	}
	d.log.Info(wakeLines[cause], "resumed", rec.Resumed, "paused_ms", math.Round(rec.PausedMS),
		"clock_stepped", rec.ClockStepped, "clock_offset_ms", math.Round(rec.ClockOffsetMS), "released", rec.Released,
		"took_ms", math.Round(float64(time.Since(began).Microseconds())/100)/10)
	d.hub.Publish(rec)
	return rec, ended
}

func (d *Daemon) willPowerOn() api.PowerWake {
	paused, inHand := d.sup.WakeBegun()
	d.log.Debug("power: the Mac will power on", "paused", paused, "pause_in_hand", inHand)
	if (paused && d.held() > 0) || inHand {
		time.AfterFunc(UnfinishedAfter, func() { d.wake(api.WakeUnfinished) })
	}
	return api.PowerWake{Time: stamp(time.Now())}
}

func (d *Daemon) rouse(asked bool) bool {
	cause := api.WakeCommand
	if !asked {
		if d.hub.Status().State != api.StatePaused {
			return false
		}
		cause = api.WakeUnfinished
	}
	_, ended := d.wake(cause)
	return ended
}

type caffeine struct {
	assert func(reason string, display bool) (release func(), err error)

	mu              sync.Mutex
	holders         map[*client]bool
	system, display func()
}

func (c *caffeine) hold(cl *client, p api.CaffeinateParams) (api.Caffeinated, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	reason := "fev"
	if p.Reason != "" {
		reason = "fev: " + clip(p.Reason, 120)
	}
	if c.system == nil {
		release, err := c.assert(reason, false)
		if err != nil {
			return api.Caffeinated{}, jsonrpc.Errorf(api.CodeFailed, "the Mac did not take the assertion: %v", err)
		}
		c.system = release
	}
	wantDisplay := p.Display || c.holders[cl]
	if wantDisplay && c.display == nil {
		release, err := c.assert(reason, true)
		if err != nil {
			if len(c.holders) == 0 {
				c.system()
				c.system = nil
			}
			return api.Caffeinated{}, jsonrpc.Errorf(api.CodeFailed, "the Mac did not take the display assertion: %v", err)
		}
		c.display = release
	}
	_, had := c.holders[cl]
	c.holders[cl] = wantDisplay
	if !had {
		cl.onGone(func() { c.release(cl) })
	}
	return api.Caffeinated{Holders: len(c.holders), Display: c.display != nil}, nil
}

func (c *caffeine) release(cl *client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.holders, cl)
	anyDisplay := false
	for _, d := range c.holders {
		anyDisplay = anyDisplay || d
	}
	if !anyDisplay && c.display != nil {
		c.display()
		c.display = nil
	}
	if len(c.holders) == 0 && c.system != nil {
		c.system()
		c.system = nil
	}
}

func (c *caffeine) releaseAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.holders = map[*client]bool{}
	for _, release := range []func(){c.display, c.system} {
		if release != nil {
			release()
		}
	}
	c.display, c.system = nil, nil
}

func (d *Daemon) caffeinateHold(_ context.Context, cl *client, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.CaffeinateParams](params)
	if err != nil {
		return nil, err
	}
	r, err := d.caffeine.hold(cl, p)
	if err != nil {
		return nil, err
	}
	d.log.Info("caffeinate: a client holds the Mac awake", "client", cl.name, "holders", r.Holders, "display", r.Display)
	return r, nil
}

func (d *Daemon) writeStopMark() {
	if err := os.WriteFile(api.StopMarkPath(d.layout.Home), nil, 0o600); err != nil {
		d.log.Error("cannot leave the mark that a stop was asked for", "err", err)
	}
}

func (d *Daemon) pauseMark(paused bool) {
	path := api.PauseMarkPath(d.layout.Home)
	if !paused {
		os.Remove(path)
		return
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		d.log.Warn("cannot leave the mark that the VM is paused; a docker command will wait without being told why", "err", err)
	}
}

func (d *Daemon) stopStands() bool {
	_, err := os.Stat(api.StopMarkPath(d.layout.Home))
	return err == nil
}

func (d *Daemon) startAsked() error {
	d.askMu.Lock()
	defer d.askMu.Unlock()
	if err := d.sup.Start(); err != nil {
		return err
	}
	d.askedNS = boottrace.Now()
	d.clearStopMark()
	return nil
}

func (d *Daemon) restartAsked() error {
	d.askMu.Lock()
	defer d.askMu.Unlock()
	if err := d.sup.Restart(); err != nil {
		return err
	}
	d.askedNS = boottrace.Now()
	d.clearStopMark()
	return nil
}

func (d *Daemon) startAskedSince(began uint64) bool { return d.askedNS >= began }

func (d *Daemon) clearStopMark() {
	if err := os.Remove(api.StopMarkPath(d.layout.Home)); err == nil {
		d.log.Info("the mark that a stop was asked for is removed")
	}
}
