package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"

	"fev/guest/internal/logbuf"
)

var ErrNoVolume = errors.New("no such volume")

type Volumes interface {
	VolumeClone(ctx context.Context, name, newName string) (*api.VolumeInfo, error)
	VolumeExportable(ctx context.Context, name string) error
	VolumeExport(ctx context.Context, name string, w io.Writer, progress Progress) (int64, error)
	VolumeImportable(ctx context.Context, name string) error
	VolumeImport(ctx context.Context, name string, r io.Reader, progress Progress) (*api.VolumeInfo, error)
}

func (s *Server) handleVolumes(ctx context.Context, c *jsonrpc.Conn, method string, params json.RawMessage, st *connState) (any, bool, error) {
	switch method {
	case api.MethodVolumeClone, api.MethodVolumeExport, api.MethodVolumeImport:
	default:
		return nil, false, nil
	}
	vs, ok := s.Sys.(Volumes)
	if !ok {
		return nil, true, failed(ErrNotImplemented)
	}
	name := func(n string) error {
		if err := api.CheckVolumeName(n); err != nil {
			return jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
		}
		return nil
	}
	st.mu.Lock()
	client := st.client
	st.mu.Unlock()
	switch method {
	case api.MethodVolumeClone:
		p, err := jsonrpc.Bind[api.VolumeCloneParams](params)
		if err != nil {
			return nil, true, err
		}
		if err := name(p.Name); err != nil {
			return nil, true, err
		}
		if err := name(p.NewName); err != nil {
			return nil, true, err
		}
		s.Log.Logf(logbuf.Info, "agent: volume %s: clone to %s asked for by %q", p.Name, p.NewName, client)
		i, err := vs.VolumeClone(ctx, p.Name, p.NewName)
		return i, true, failed(err)
	case api.MethodVolumeExport:
		p, err := jsonrpc.Bind[api.VolumeNameParams](params)
		if err != nil {
			return nil, true, err
		}
		if err := name(p.Name); err != nil {
			return nil, true, err
		}
		if err := vs.VolumeExportable(ctx, p.Name); err != nil {
			return nil, true, failed(err)
		}
		s.Log.Logf(logbuf.Info, "agent: volume %s: export asked for by %q", p.Name, client)
		op, err := s.startOperation(ctx, c, method, true, func(ctx context.Context, op *operation) (any, error) {
			conn, err := op.stream()
			if err != nil {
				return nil, err
			}
			n, err := vs.VolumeExport(ctx, p.Name, conn, op.progress)
			if err != nil {
				return nil, err
			}
			op.progress(api.PhaseArchive, n, n)
			return api.VolumeExported{Bytes: n}, nil
		})
		return op, true, failed(err)
	default:
		p, err := jsonrpc.Bind[api.VolumeNameParams](params)
		if err != nil {
			return nil, true, err
		}
		if err := name(p.Name); err != nil {
			return nil, true, err
		}
		if err := vs.VolumeImportable(ctx, p.Name); err != nil {
			return nil, true, failed(err)
		}
		s.Log.Logf(logbuf.Info, "agent: volume %s: import asked for by %q", p.Name, client)
		op, err := s.startOperation(ctx, c, method, true, func(ctx context.Context, op *operation) (any, error) {
			conn, err := op.stream()
			if err != nil {
				return nil, err
			}
			return vs.VolumeImport(ctx, p.Name, &readToEnd{conn: conn, op: op}, op.progress)
		})
		return op, true, failed(err)
	}
}
