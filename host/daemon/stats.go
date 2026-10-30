package daemon

import (
	"context"
	"encoding/json"
	"time"

	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/vmmctl"
)

func (s *Supervisor) account() (state string, b *boot, ctl *vmmctl.Client, guest *agent.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state = s.hub.Status().State
	if b = s.cur; b == nil || b.reason != "" {
		return state, nil, nil, nil
	}
	ctl = b.ctl
	if state == api.StateRunning || state == api.StatePaused {
		guest = b.agent
	}
	return state, b, ctl, guest
}

func (d *Daemon) vmmStats(b *boot, ctl *vmmctl.Client) *api.VMMStats {
	if b == nil || ctl == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(b.ctx, d.sup.timing.AliveBound)
	st, err := ctl.Stats(ctx)
	cancel()
	if err != nil {
		return nil
	}
	out := &api.VMMStats{
		GuestBytes: st.Memory.GuestBytes, FootprintBytes: st.Memory.FootprintBytes,
		HeldBytes: st.Memory.ResidentBytes + st.Memory.CompressedBytes, ReleasedBytes: st.Memory.ReleasedBytes,
	}
	var ns uint64
	for _, v := range st.VCPUs {
		ns += v.CPUNS
	}
	out.CPUMS = float64(ns/1000) / 1000
	if w := st.Windows; w != nil {
		window := func(w *vmmctl.Window) *api.Window {
			if w == nil {
				return nil
			}
			return &api.Window{State: w.State, Reason: w.Reason, SizeBytes: w.SizeBytes, Reads: w.Reads, Mappings: w.Mappings, Live: w.Live, Faults: w.Faults}
		}
		out.Windows = &api.Windows{DataDisk: window(w.DataDisk), FileShare: window(w.FileShare)}
	}
	return out
}

func statusWindows(w *vmmctl.Windows) *api.Windows {
	if w == nil || (w.DataDisk == nil && w.FileShare == nil) {
		return nil
	}
	window := func(w *vmmctl.Window) *api.Window {
		if w == nil {
			return nil
		}
		return &api.Window{State: clip(w.State, 16), Reason: clip(w.Reason, 200), SizeBytes: w.SizeBytes}
	}
	return &api.Windows{DataDisk: window(w.DataDisk), FileShare: window(w.FileShare)}
}

func guestStats(s *agent.StatsSample) *api.GuestStats {
	if s == nil {
		return nil
	}
	out := &api.GuestStats{MemTotalKB: s.MemTotalKB, MemAvailableKB: s.MemAvailableKB, CachedKB: s.CachedKB, Groups: []api.StatsGroup{}}
	for i, g := range s.Groups {
		if i == maxStatsGroups {
			break
		}
		out.Groups = append(out.Groups, api.StatsGroup{
			Kind: clip(g.Kind, 16), Name: clip(g.Name, 128), ID: clip(g.ID, 64), CPUUsec: g.CPUUsec,
			MemoryBytes: g.MemoryBytes, MemoryFileBytes: g.MemoryFileBytes, DiskReadBytes: g.DiskReadBytes, DiskWriteBytes: g.DiskWriteBytes,
			NetRxBytes: g.NetRxBytes, NetTxBytes: g.NetTxBytes, PIDs: g.PIDs,
		})
	}
	return out
}

const maxStatsGroups = 4096

func (d *Daemon) statsSubscribe(ctx context.Context, c *jsonrpc.Conn, cl *client, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.StatsSubscribeParams](params)
	if err != nil {
		return nil, err
	}
	if p.IntervalMS < 0 || (p.IntervalMS != 0 && p.IntervalMS < api.MinStatsIntervalMS) {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "interval_ms must be at least %d", api.MinStatsIntervalMS)
	}
	if p.IntervalMS > maxStatsIntervalMS {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "interval_ms must be at most %d", maxStatsIntervalMS)
	}
	if p.IntervalMS == 0 {
		p.IntervalMS = api.DefaultStatsIntervalMS
	}
	id := d.nextSub.Add(1)
	stop := make(chan struct{})
	cl.add(id, func() { close(stop) })
	d.pumps.Add(1)
	jsonrpc.AfterReply(ctx, func() {
		go func() {
			defer d.pumps.Done()
			d.pumpStats(c, id, p.IntervalMS, stop)
		}()
	})
	d.log.Debug("stats: a client subscribed", "client", cl.name, "interval_ms", p.IntervalMS)
	return api.Subscription{Subscription: id}, nil
}

const maxStatsIntervalMS = 3600 * 1000

type guestFeed struct {
	from *agent.Client
	stop func()
	err  error
}

func (d *Daemon) pumpStats(c *jsonrpc.Conn, id uint64, intervalMS int, stop <-chan struct{}) {
	interval := time.Duration(intervalMS) * time.Millisecond
	samples := make(chan agent.StatsSample, 1)
	var asked, from *agent.Client
	var unsub func()
	var lastGuest time.Time
	fed := make(chan guestFeed, 1)
	pending := false
	defer func() {
		if unsub != nil {
			go unsub()
		}
		if pending {
			go func() {
				if f := <-fed; f.stop != nil {
					f.stop()
				}
			}()
		}
	}()
	follow := func(guest *agent.Client) {
		if guest == asked || pending {
			return
		}
		if unsub != nil {
			go unsub()
			unsub = nil
		}
		asked, from = guest, nil
		if guest == nil {
			return
		}
		pending = true
		lastGuest = time.Now()
		go func() {
			stop, err := guest.SubscribeStats(context.Background(), intervalMS, func(s agent.StatsSample) {
				for {
					select {
					case samples <- s:
						return
					default:
					}
					select {
					case <-samples:
					default:
					}
				}
			})
			fed <- guestFeed{from: guest, stop: stop, err: err}
		}()
	}
	emit := func(state string, b *boot, ctl *vmmctl.Client, guest *agent.StatsSample) bool {
		sample := api.StatsSample{Subscription: id, Time: stamp(time.Now()), State: state, VMM: d.vmmStats(b, ctl)}
		if state == api.StateRunning {
			sample.Guest = guestStats(guest)
		}
		return c.Notify(api.NotifyStatsSample, sample) == nil
	}

	tick := time.NewTicker(interval)
	defer tick.Stop()
	state, b, ctl, guest := d.sup.account()
	follow(guest)
	if (guest == nil || state != api.StateRunning) && !emit(state, b, ctl, nil) {
		return
	}
	for {
		select {
		case <-stop:
			return
		case <-c.Done():
			return
		case f := <-fed:
			pending = false
			switch {
			case f.err != nil:
				select {
				case <-f.from.Done():
				default:
					d.log.Warn("stats: the guest did not start its stream; samples carry the VMM's account alone", "err", f.err)
				}
			case f.from != asked:
				go f.stop()
			default:
				from, unsub = f.from, f.stop
			}
		case s := <-samples:
			state, b, ctl, guest := d.sup.account()
			if guest != from || guest == nil {
				follow(guest)
				continue
			}
			lastGuest = time.Now()
			if !emit(state, b, ctl, &s) {
				return
			}
		case <-tick.C:
			state, b, ctl, guest := d.sup.account()
			follow(guest)
			if guest != nil && time.Since(lastGuest) < interval+interval/2 {
				continue
			}
			if !emit(state, b, ctl, nil) {
				return
			}
		}
	}
}
