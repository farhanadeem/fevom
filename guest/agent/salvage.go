package agent

import (
	"context"
	"errors"
	"time"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"

	"fev/guest/internal/logbuf"
)

type Salvager interface {
	StorageSalvage(ctx context.Context, p api.SalvageParams, progress func(phase string, done, total int64)) (api.Salvaged, error)
}

type Usager interface {
	StorageUsage(engine bool) (*api.StorageUsage, error)
}

func (s *Server) storageSalvage(ctx context.Context, c *jsonrpc.Conn, st *connState, p api.SalvageParams) (any, error) {
	sv, ok := s.Sys.(Salvager)
	if !ok {
		return nil, failed(ErrNotImplemented)
	}
	if p.Dest == "" || p.Dest[0] != '/' {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "storage.salvage: dest %q is not a Mac path", p.Dest)
	}
	if !s.salvaging.CompareAndSwap(false, true) {
		return nil, failed(errors.New("a salvage is running already"))
	}
	st.mu.Lock()
	client := st.client
	st.mu.Unlock()
	op, err := s.startOperation(ctx, c, api.MethodStorageSalvage, false, func(ctx context.Context, op *operation) (any, error) {
		defer s.salvaging.Store(false)
		began := time.Now()
		r, err := sv.StorageSalvage(ctx, p, op.progress)
		if err != nil {
			return nil, err
		}
		s.Log.Logf(logbuf.Info, "agent: salvage %d to %s done in %v by %s: %d files, %d bytes, %d not read",
			op.id, p.Dest, time.Since(began).Round(time.Millisecond), r.Method, r.Files, r.Bytes, r.Lost)
		return r, nil
	})
	if err != nil {
		s.salvaging.Store(false)
		return nil, failed(err)
	}
	s.Log.Logf(logbuf.Info, "agent: salvage %d to %s asked for by %q (volumes %v, machines %v, docker %v)",
		op.ID, p.Dest, client, p.Volumes, p.Machines, p.Docker)
	return op, nil
}

func (s *Server) storageUsage(p api.UsageParams) (any, error) {
	u, ok := s.Sys.(Usager)
	if !ok {
		return nil, failed(ErrNotImplemented)
	}
	r, err := u.StorageUsage(p.Engine)
	return r, failed(err)
}
