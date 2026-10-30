package agent

import (
	"context"
	"encoding/json"
	"io"
	"time"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"

	"fev/guest/internal/logbuf"
)

const maxMachineGrace = maxEngineGrace

func machineGrace(ms int) (time.Duration, error) {
	d := time.Duration(ms) * time.Millisecond
	if ms < 0 || d > maxMachineGrace {
		return 0, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "grace_ms must be between 0 and %d", maxMachineGrace.Milliseconds())
	}
	if d == 0 {
		d = api.DefaultMachineGraceMS * time.Millisecond
	}
	return d, nil
}

func (s *Server) handleMachines(method string, params json.RawMessage, st *connState) (any, bool, error) {
	if method == api.MethodMachinePorts {
		mp, ok := s.Sys.(MachinePorts)
		if !ok {
			return nil, true, failed(ErrNotImplemented)
		}
		if _, err := jsonrpc.Bind[api.Empty](params); err != nil {
			return nil, true, err
		}
		return mp.MachinePorts(), true, nil
	}
	if method == api.MethodTrustSet {
		tr, ok := s.Sys.(Trust)
		if !ok {
			return nil, true, failed(ErrNotImplemented)
		}
		p, err := jsonrpc.Bind[api.TrustParams](params)
		if err != nil {
			return nil, true, err
		}
		ders, err := api.TrustDER(p.Roots)
		if err != nil {
			return nil, true, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
		}
		r, err := tr.TrustSet(ders)
		if err != nil {
			return nil, true, failed(err)
		}
		s.Log.Logf(logbuf.Info, "agent: the Mac's trusted roots: %d (%.12s), installed in each machine at its next start", r.Count, r.Digest)
		return r, true, nil
	}
	if method == api.MethodSSHAgentSet {
		sa, ok := s.Sys.(SSHAgent)
		if !ok {
			return nil, true, failed(ErrNotImplemented)
		}
		p, err := jsonrpc.Bind[api.SSHAgentParams](params)
		if err != nil {
			return nil, true, err
		}
		if err := api.CheckSSHAgentPath(p.Path); err != nil {
			return nil, true, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
		}
		r, err := sa.SSHAgentSet(p.Path)
		return r, true, failed(err)
	}
	switch method {
	case api.MethodMachineList, api.MethodMachineInfo, api.MethodMachineStart, api.MethodMachineStop, api.MethodMachineRestart:
	default:
		return nil, false, nil
	}
	ms, ok := s.Sys.(Machines)
	if !ok {
		return nil, true, failed(ErrNotImplemented)
	}
	name := func(n string) error {
		if err := api.CheckMachineName(n); err != nil {
			return jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
		}
		return nil
	}
	st.mu.Lock()
	client := st.client
	st.mu.Unlock()
	switch method {
	case api.MethodMachineList:
		l, err := ms.MachineList()
		return l, true, failed(err)
	case api.MethodMachineInfo:
		p, err := jsonrpc.Bind[api.MachineNameParams](params)
		if err != nil {
			return nil, true, err
		}
		if err := name(p.Name); err != nil {
			return nil, true, err
		}
		i, err := ms.MachineInfo(p.Name)
		return i, true, failed(err)
	case api.MethodMachineStart:
		p, err := jsonrpc.Bind[api.MachineStartParams](params)
		if err != nil {
			return nil, true, err
		}
		if err := name(p.Name); err != nil {
			return nil, true, err
		}
		s.Log.Logf(logbuf.Info, "agent: machine %s: start requested by %q", p.Name, client)
		i, err := ms.MachineStart(p.Name, p.Config)
		return i, true, failed(err)
	case api.MethodMachineStop:
		p, err := jsonrpc.Bind[api.MachineStopParams](params)
		if err != nil {
			return nil, true, err
		}
		if err := name(p.Name); err != nil {
			return nil, true, err
		}
		grace, err := machineGrace(p.GraceMS)
		if err != nil {
			return nil, true, err
		}
		s.Log.Logf(logbuf.Info, "agent: machine %s: stop requested by %q (grace %v, force %v)", p.Name, client, grace, p.Force)
		i, err := ms.MachineStop(p.Name, grace, p.Force)
		return i, true, failed(err)
	default:
		p, err := jsonrpc.Bind[api.MachineRestartParams](params)
		if err != nil {
			return nil, true, err
		}
		if err := name(p.Name); err != nil {
			return nil, true, err
		}
		grace, err := machineGrace(p.GraceMS)
		if err != nil {
			return nil, true, err
		}
		s.Log.Logf(logbuf.Info, "agent: machine %s: restart requested by %q (grace %v)", p.Name, client, grace)
		i, err := ms.MachineRestart(p.Name, grace, p.Config)
		return i, true, failed(err)
	}
}

type Trust interface {
	TrustSet(ders [][]byte) (api.Trusted, error)
}

type SSHAgent interface {
	SSHAgentSet(path string) (api.SSHAgentBound, error)
}

type Progress func(phase string, done, total int64)

type MachineMaker interface {
	MachineCreate(p api.MachineCreateParams) (Making, error)
	MachineImport(name string, cfg api.MachineConfig) (Making, error)
	MachineExport(ctx context.Context, name string, w io.Writer, progress Progress) (int64, error)
	MachineDelete(name string, grace time.Duration, force bool) error
	MachineClone(name, newName string) (*api.MachineInfo, error)
	MachineRename(name, newName string) (*api.MachineInfo, error)
}

type Making interface {
	NeedStream() bool
	Run(ctx context.Context, stream io.Reader, progress Progress) (any, error)
	Abandon()
}

func (s *Server) handleMachineOps(ctx context.Context, c *jsonrpc.Conn, method string, params json.RawMessage, st *connState) (any, bool, error) {
	switch method {
	case api.MethodOperationCancel:
		p, err := jsonrpc.Bind[api.OperationID](params)
		if err != nil {
			return nil, true, err
		}
		s.cancelOperation(p.ID)
		return api.Empty{}, true, nil
	case api.MethodMachineCreate, api.MethodMachineImport, api.MethodMachineExport,
		api.MethodMachineDelete, api.MethodMachineClone, api.MethodMachineRename:
	default:
		return nil, false, nil
	}
	mm, ok := s.Sys.(MachineMaker)
	if !ok {
		return nil, true, failed(ErrNotImplemented)
	}
	name := func(n string) error {
		if err := api.CheckMachineName(n); err != nil {
			return jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
		}
		return nil
	}
	st.mu.Lock()
	client := st.client
	st.mu.Unlock()
	switch method {
	case api.MethodMachineCreate:
		p, err := jsonrpc.Bind[api.MachineCreateParams](params)
		if err != nil {
			return nil, true, err
		}
		if err := name(p.Name); err != nil {
			return nil, true, err
		}
		if len(p.UserData) > api.MaxUserDataBytes {
			return nil, true, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "user_data is %d bytes; at most %d", len(p.UserData), api.MaxUserDataBytes)
		}
		mk, err := mm.MachineCreate(p)
		if err != nil {
			return nil, true, failed(err)
		}
		s.Log.Logf(logbuf.Info, "agent: machine %s: create from %s %s %s %s asked for by %q (image sent: %v)",
			p.Name, p.Image.Distro, p.Image.Release, p.Image.Variant, p.Image.Serial, client, mk.NeedStream())
		op, err := s.startMaking(ctx, c, method, mk)
		return op, true, failed(err)
	case api.MethodMachineImport:
		p, err := jsonrpc.Bind[api.MachineImportParams](params)
		if err != nil {
			return nil, true, err
		}
		if err := name(p.Name); err != nil {
			return nil, true, err
		}
		mk, err := mm.MachineImport(p.Name, p.Config)
		if err != nil {
			return nil, true, failed(err)
		}
		s.Log.Logf(logbuf.Info, "agent: machine %s: import asked for by %q", p.Name, client)
		op, err := s.startMaking(ctx, c, method, mk)
		return op, true, failed(err)
	case api.MethodMachineExport:
		p, err := jsonrpc.Bind[api.MachineNameParams](params)
		if err != nil {
			return nil, true, err
		}
		if err := name(p.Name); err != nil {
			return nil, true, err
		}
		if ms, ok := s.Sys.(Machines); ok {
			if _, err := ms.MachineInfo(p.Name); err != nil {
				return nil, true, failed(err)
			}
		}
		s.Log.Logf(logbuf.Info, "agent: machine %s: export asked for by %q", p.Name, client)
		op, err := s.startOperation(ctx, c, method, true, func(ctx context.Context, op *operation) (any, error) {
			conn, err := op.stream()
			if err != nil {
				return nil, err
			}
			n, err := mm.MachineExport(ctx, p.Name, conn, op.progress)
			if err != nil {
				return nil, err
			}
			op.progress(api.PhaseArchive, n, n)
			return api.MachineExported{Bytes: n}, nil
		})
		return op, true, failed(err)
	case api.MethodMachineDelete:
		p, err := jsonrpc.Bind[api.MachineStopParams](params)
		if err != nil {
			return nil, true, err
		}
		if err := name(p.Name); err != nil {
			return nil, true, err
		}
		grace, err := machineGrace(p.GraceMS)
		if err != nil {
			return nil, true, err
		}
		s.Log.Logf(logbuf.Info, "agent: machine %s: delete asked for by %q (grace %v, force %v)", p.Name, client, grace, p.Force)
		return api.Empty{}, true, failed(mm.MachineDelete(p.Name, grace, p.Force))
	case api.MethodMachineClone:
		p, err := jsonrpc.Bind[api.MachineCloneParams](params)
		if err != nil {
			return nil, true, err
		}
		if err := name(p.Name); err != nil {
			return nil, true, err
		}
		if err := name(p.NewName); err != nil {
			return nil, true, err
		}
		s.Log.Logf(logbuf.Info, "agent: machine %s: clone to %s asked for by %q", p.Name, p.NewName, client)
		i, err := mm.MachineClone(p.Name, p.NewName)
		return i, true, failed(err)
	default:
		p, err := jsonrpc.Bind[api.MachineRenameParams](params)
		if err != nil {
			return nil, true, err
		}
		if err := name(p.Name); err != nil {
			return nil, true, err
		}
		if err := name(p.NewName); err != nil {
			return nil, true, err
		}
		s.Log.Logf(logbuf.Info, "agent: machine %s: rename to %s asked for by %q", p.Name, p.NewName, client)
		i, err := mm.MachineRename(p.Name, p.NewName)
		return i, true, failed(err)
	}
}

func (s *Server) startMaking(ctx context.Context, c *jsonrpc.Conn, method string, mk Making) (api.Operation, error) {
	need := mk.NeedStream()
	op, err := s.startOperation(ctx, c, method, need, func(ctx context.Context, op *operation) (any, error) {
		var r io.Reader
		if need {
			conn, err := op.stream()
			if err != nil {
				mk.Abandon()
				return nil, err
			}
			r = &readToEnd{conn: conn, op: op}
		}
		return mk.Run(ctx, r, op.progress)
	})
	if err != nil {
		mk.Abandon()
	}
	return op, err
}

type readToEnd struct {
	conn io.Reader
	op   *operation
}

func (r *readToEnd) Read(b []byte) (int, error) {
	n, err := r.conn.Read(b)
	if err == io.EOF {
		r.op.release()
	}
	return n, err
}
