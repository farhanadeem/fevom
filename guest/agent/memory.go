package agent

import (
	"errors"
	"fmt"
	"math"
	"time"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"

	"fev/guest/internal/logbuf"
)

const maxReclaimFloorMiB = 1 << 24

const ReclaimPressurePercent = 10.0

func (s *Server) memoryReclaim(st *connState, p api.ReclaimParams) (any, error) {
	if p.FloorMiB < 0 || p.FloorMiB > maxReclaimFloorMiB {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "floor_mib must be between 0 and %d", maxReclaimFloorMiB)
	}
	floor := p.FloorMiB
	if floor == 0 {
		floor = api.DefaultReclaimFloorMiB
	}
	floorBytes := uint64(floor) << 20

	s.reclaim.Lock()
	defer s.reclaim.Unlock()
	start := time.Now()
	idle, own, live, err := s.Sys.IdleCache()
	if err != nil {
		return nil, failed(err)
	}
	own = min(own, idle)
	nobodys := idle - own
	keep := min(own, floorBytes)
	r := api.Reclaimed{FileBeforeBytes: idle, FileAfterBytes: idle, FloorMiB: floor}
	done := func() api.Reclaimed {
		r.DurationMS = math.Round(float64(time.Since(start).Microseconds())/100) / 10
		return r
	}
	atFloor := nobodys < nobodysLeastBytes && own <= floorBytes+api.ReclaimSlackMiB<<20
	if atFloor && !p.Running {
		r.Skipped = api.ReclaimSkippedFloor
		s.compactIfScattered(&r, p)
		return done(), nil
	}
	if pressure, err := s.Sys.MemoryPressure(); err == nil && pressure >= ReclaimPressurePercent {
		r.Skipped = api.ReclaimSkippedPressure
		s.Log.Logf(logbuf.Info, "agent: memory reclaim for %q skipped: memory pressure %.1f%% over the last 10 s", st.client, pressure)
		return done(), nil
	}
	if err := s.Sys.StorageSync(); err != nil && !errors.Is(err, ErrNotImplemented) {
		s.Log.Logf(logbuf.Warning, "agent: the data disk's sync before a memory reclaim failed: %v", err)
	}
	if atFloor {
		r.Skipped = api.ReclaimSkippedFloor
	} else {
		if err := s.Sys.ReclaimIdle(idle - keep); err != nil {
			s.Log.Logf(logbuf.Warning, "agent: memory reclaim for %q failed: %v", st.client, err)
			return nil, failed(err)
		}
		if left, _, _, err := s.Sys.IdleCache(); err == nil {
			r.FileAfterBytes = min(left, idle)
		}
		r.ReclaimedBytes = r.FileBeforeBytes - r.FileAfterBytes
	}
	if p.Running {
		s.trimRunning(&r)
	}
	if !p.NoCompact && r.ReclaimedBytes+r.TrimmedBytes >= trimCompactBytes {
		if err := s.compact(); err != nil {
			s.Log.Logf(logbuf.Warning, "agent: compaction after a reclaim failed: %v", err)
		} else {
			r.Compacted = true
		}
	} else {
		s.compactIfScattered(&r, p)
	}
	done()
	if r.ReclaimedBytes > 0 || r.TrimmedBytes > 0 {
		trim := ""
		if p.Running {
			trim = fmt.Sprintf("; trimmed %d MiB of idle running containers' cache (%d running, %d MiB left that a later trim could take)",
				r.TrimmedBytes>>20, r.Running, r.ColdBytes>>20)
		}
		s.Log.Logf(logbuf.Info, "agent: reclaimed %d MiB of cache for %q in %.0f ms (%d MiB left, %d MiB held by running containers; floor %d MiB)%s",
			r.ReclaimedBytes>>20, st.client, r.DurationMS, r.FileAfterBytes>>20, live>>20, floor, trim)
	}
	return r, nil
}

const (
	trimCompactBytes = 16 << 20
	scatteredBytes   = 32 << 20
)

const nobodysLeastBytes = 8 << 20

func (s *Server) compactIfScattered(r *api.Reclaimed, p api.ReclaimParams) {
	if p.NoCompact {
		return
	}
	if n, err := s.Sys.Unreportable(); err != nil || n < scatteredBytes {
		return
	}
	if err := s.compact(); err != nil {
		s.Log.Logf(logbuf.Warning, "agent: compaction of scattered free memory failed: %v", err)
		return
	}
	r.Compacted = true
}

const macPageOrder = 2

var reportPassWait = 3 * time.Second

func (s *Server) compact() error {
	was, err := s.Sys.SetReportingOrder(macPageOrder)
	lowered := err == nil
	switch {
	case lowered:
		if !s.lowered {
			s.lowered, s.restoreOrder = true, was
		}
		s.lowerings++
	case !errors.Is(err, ErrNotImplemented):
		s.Log.Logf(logbuf.Warning, "agent: free-page reporting's order not lowered for a compaction: %v", err)
	}
	err = s.Sys.CompactMemory()
	if lowered {
		n := s.lowerings
		time.AfterFunc(reportPassWait, func() {
			s.reclaim.Lock()
			defer s.reclaim.Unlock()
			if !s.lowered || n != s.lowerings {
				return
			}
			s.lowered = false
			if _, err := s.Sys.SetReportingOrder(s.restoreOrder); err != nil {
				s.Log.Logf(logbuf.Warning, "agent: free-page reporting's order not put back to %d: %v", s.restoreOrder, err)
			}
		})
	}
	return err
}

type cpuSeen struct {
	usec, refaults uint64
	at             time.Time
	tookActive     bool
}

const rereadPages = 256

func (s *Server) trimRunning(r *api.Reclaimed) {
	runs, err := s.Sys.RunningCache()
	if err != nil {
		if !errors.Is(err, ErrNotImplemented) {
			s.Log.Logf(logbuf.Warning, "agent: running containers' cache could not be read: %v", err)
		}
		return
	}
	now := time.Now()
	seen := make(map[string]cpuSeen, len(runs))
	rereads := make(map[string]bool, len(runs))
	floor := uint64(api.TrimFloorMiB) << 20
	r.Running = len(runs)
	var was uint64
	for _, run := range runs {
		cache := run.InactiveBytes + run.ActiveBytes + run.SlabBytes
		was += cache
		next := cpuSeen{usec: run.CPUUsec, refaults: run.Refaults, at: now}
		before, ok := s.seen[run.Dir]
		rereads[run.Dir] = s.rereads[run.Dir]
		if ok && before.tookActive && run.Refaults-min(run.Refaults, before.refaults) > rereadPages {
			rereads[run.Dir] = true
			s.Log.Logf(logbuf.Info, "agent: %s read back what a trim took of its active cache; only its inactive cache is taken from now on", run.Dir)
		}
		idle := false
		if ok && run.CPUUsec >= before.usec {
			used := float64(run.CPUUsec - before.usec)
			span := float64(now.Sub(before.at).Microseconds())
			idle = span > 0 && used < span*api.TrimIdleCPUPercent/100
		}
		ask := run.InactiveBytes
		if !rereads[run.Dir] {
			ask += run.ActiveBytes/2 + run.SlabBytes/2
		}
		ask -= min(ask, floor)
		if idle && ask > api.ReclaimSlackMiB<<20/4 {
			if err := s.Sys.TrimRunning(run.Dir, ask); err != nil {
				s.Log.Logf(logbuf.Warning, "agent: trim of %s failed: %v", run.Dir, err)
			} else {
				next.tookActive = !rereads[run.Dir] && run.ActiveBytes > 0
			}
		}
		seen[run.Dir] = next
	}
	after := runs
	if again, err := s.Sys.RunningCache(); err == nil {
		after = again
	}
	var now2 uint64
	for _, run := range after {
		cache := run.InactiveBytes + run.ActiveBytes + run.SlabBytes
		now2 += cache
		if !rereads[run.Dir] {
			r.ColdBytes += cache - min(cache, floor)
		} else {
			r.ColdBytes += run.InactiveBytes - min(run.InactiveBytes, floor)
		}
	}
	r.TrimmedBytes = was - min(now2, was)
	s.seen, s.rereads = seen, rereads
}
