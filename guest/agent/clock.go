package agent

import (
	"math"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"

	"fev/guest/internal/logbuf"
)

const minTimeUnixNS = 1577836800 * 1e9

func (s *Server) timeSet(st *connState, p api.TimeSetParams) (any, error) {
	if p.UnixNS <= minTimeUnixNS {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "unix_ns must be a time after 2020, in nanoseconds")
	}
	if p.ThresholdMS < 0 {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "threshold_ms must not be negative")
	}
	threshold := p.ThresholdMS
	if threshold == 0 {
		threshold = api.DefaultTimeThresholdMS
	}
	s.clock.Lock()
	defer s.clock.Unlock()
	now, err := s.Sys.WallClock()
	if err != nil {
		return nil, failed(err)
	}
	offsetMS := math.Round(float64(now-p.UnixNS)/1e3) / 1e3
	r := api.TimeSet{OffsetMS: offsetMS, ThresholdMS: threshold}
	if math.Abs(offsetMS) <= float64(threshold) {
		return r, nil
	}
	if err := s.Sys.SetWallClock(p.UnixNS); err != nil {
		s.Log.Logf(logbuf.Error, "agent: the wall clock is %.3f s from the Mac's and could not be set: %v", offsetMS/1e3, err)
		return nil, failed(err)
	}
	r.Stepped = true
	was := "behind"
	if offsetMS > 0 {
		was = "ahead of"
	}
	s.Log.Logf(logbuf.Info, "agent: wall clock stepped by %+.3f s for %q (it was %s the Mac's; threshold %d ms)",
		-offsetMS/1e3, st.client, was, threshold)
	return r, nil
}
