package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"fev/host/daemon/vm"
	"fev/host/storage"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

type StorageCalls struct {
	Machines *Machines
	Image    string
	Share    *vm.Share

	host Host
}

func (s *StorageCalls) Name() string    { return "storage" }
func (s *StorageCalls) Marks() []string { return nil }

func (s *StorageCalls) Start(h Host) error {
	s.host = h
	h.Handle(api.MethodStorageUsage, s.usage)
	h.Handle(api.MethodStorageSalvage, s.salvage)
	h.Handle(api.MethodStorageCheck, s.check)
	h.Handle(api.MethodMemoryReclaim, s.reclaim)
	return nil
}

func (s *StorageCalls) StateChanged(api.Status)        {}
func (s *StorageCalls) ConfigChanged(api.ConfigChange) {}
func (s *StorageCalls) Close()                         {}

func (s *StorageCalls) usage(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.UsageParams](params)
	if err != nil {
		return nil, err
	}
	client, err := s.Machines.guest(ctx, false)
	if errors.Is(err, errVMDown) {
		return nil, jsonrpc.Errorf(api.CodeFailed, "the VM is not running: what the data disk holds is the guest's to say")
	}
	if err != nil {
		return nil, err
	}
	u, err := client.StorageUsage(ctx, agent.UsageParams{Engine: p.Engine})
	if err != nil {
		return nil, guestError(err)
	}
	r := api.StorageUsage{UsedBytes: u.UsedBytes, ImagesBytes: u.ImagesBytes, EngineBytes: u.EngineBytes, DebugBytes: u.DebugBytes, Machines: []api.MachineUsage{}}
	for _, m := range u.Machines {
		r.Machines = append(r.Machines, api.MachineUsage{Name: m.Name, DiskBytes: m.DiskBytes})
	}
	if k := u.Kubernetes; k != nil {
		r.Kubernetes = &api.KubernetesUsage{Bytes: k.Bytes, ClusterBytes: k.ClusterBytes, CacheBytes: k.CacheBytes,
			ImagesBytes: k.ImagesBytes, ImagesCounted: k.ImagesCounted}
	}
	if u.Engine != nil {
		r.Engine = &api.EngineUsage{ImagesBytes: u.Engine.ImagesBytes, ContainersBytes: u.Engine.ContainersBytes,
			VolumesBytes: u.Engine.VolumesBytes, BuildCacheBytes: u.Engine.BuildCacheBytes}
	}
	if s.Image != "" {
		if n, err := storage.Allocated(s.Image); err == nil {
			r.ImageFileBytes = n
		}
	}
	return r, nil
}

func (s *StorageCalls) shareDest(dest string) (string, error) {
	if dest == "" || !filepath.IsAbs(dest) {
		return "", jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "dest %q is not an absolute Mac path", dest)
	}
	dest = filepath.Clean(dest)
	if s.Share == nil || s.Share.Root == "" {
		return "", jsonrpc.Errorf(api.CodeFailed, "salvage writes to the Mac through the file share, and this daemon shares nothing with the VM")
	}
	on, running := s.host.Config().InEffect(api.KeyFilesShare)
	if !running {
		on = s.host.Config().Bool(api.KeyFilesShare)
	}
	if on != true {
		return "", jsonrpc.Errorf(api.CodeFailed, "salvage writes to the Mac through the file share, which is off (files.share): turn it on, restart, and salvage again")
	}
	root := filepath.Clean(s.Share.Root)
	rel, err := filepath.Rel(root, dest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", jsonrpc.Errorf(api.CodeFailed, "%s is not under %s, the folder the VM is given of the Mac", dest, root)
	}
	for _, hidden := range s.Share.Deny {
		if h := filepath.Clean(hidden); dest == h || strings.HasPrefix(dest, h+"/") {
			return "", jsonrpc.Errorf(api.CodeFailed, "%s is in %s, which the VM is not given: choose another folder", dest, h)
		}
	}
	return filepath.Join("/", rel), nil
}

func (s *StorageCalls) salvage(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.SalvageParams](params)
	if err != nil {
		return nil, err
	}
	dest, err := s.shareDest(p.Dest)
	if err != nil {
		return nil, err
	}
	m := s.Machines
	return m.ops.Start(ctx, api.MethodStorageSalvage, filepath.Clean(p.Dest), "", func(op *Operation) {
		octx := op.Context()
		client, err := m.guest(octx, true)
		if err != nil {
			op.Fail(err)
			return
		}
		g := newGuestOp(op)
		gop, err := client.StorageSalvage(octx, agent.SalvageParams{Dest: dest, Volumes: p.Volumes, Machines: p.Machines, Docker: p.Docker}, g.progress)
		if err != nil {
			op.Fail(guestError(err))
			return
		}
		end, err := g.wait(client, gop.ID)
		switch {
		case err != nil:
			op.Fail(err)
			return
		case end.State == agent.OperationCancelled:
			op.Cancelled()
			return
		case end.State != agent.OperationDone:
			op.Fail(errors.New(end.Error))
			return
		}
		var r agent.Salvaged
		if err := json.Unmarshal(end.Result, &r); err != nil {
			op.Fail(err)
			return
		}
		s.host.Log().Info("data: salvaged", "dest", p.Dest, "method", r.Method, "files", r.Files, "bytes", r.Bytes, "not_read", r.Lost)
		op.Done(api.Salvaged{Method: r.Method, Files: r.Files, Bytes: r.Bytes, Errors: r.Errors, Lost: r.Lost})
	}), nil
}

func (s *StorageCalls) reclaim(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.ReclaimParams](params)
	if err != nil {
		return nil, err
	}
	if p.FloorMiB < 0 {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "floor_mib %d is negative", p.FloorMiB)
	}
	client, err := s.Machines.guest(ctx, false)
	if errors.Is(err, errVMDown) {
		return nil, jsonrpc.Errorf(api.CodeFailed, "the VM is not running: there is no memory of the guest's to give back")
	}
	if err != nil {
		return nil, err
	}
	r, err := client.MemoryReclaim(ctx, agent.ReclaimParams{FloorMiB: p.FloorMiB})
	if err != nil {
		return nil, guestError(err)
	}
	s.host.Log().Info("memory: reclaim asked for", "reclaimed_bytes", r.ReclaimedBytes, "skipped", r.Skipped, "floor_mib", r.FloorMiB)
	return api.Reclaimed{FileBeforeBytes: r.FileBeforeBytes, FileAfterBytes: r.FileAfterBytes, ReclaimedBytes: r.ReclaimedBytes,
		FloorMiB: r.FloorMiB, Compacted: r.Compacted, Skipped: r.Skipped, DurationMS: r.DurationMS}, nil
}

func (s *StorageCalls) check(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.StorageCheckParams](params)
	if err != nil {
		return nil, err
	}
	m := s.Machines
	if _, err := m.guest(ctx, false); errors.Is(err, errVMDown) {
		return nil, jsonrpc.Errorf(api.CodeFailed, "the VM is not running: the data disk is checked as the VM has it mounted; start fev first")
	} else if err != nil {
		return nil, err
	}
	return m.ops.Start(ctx, api.MethodStorageCheck, "data", "", func(op *Operation) {
		octx := op.Context()
		client, err := m.guest(octx, false)
		if err != nil {
			op.Fail(err)
			return
		}
		g := newGuestOp(op)
		start := client.StorageCheck
		if p.Follow {
			start = client.StorageCheckFollow
		}
		gop, err := start(octx, g.progress)
		if err != nil {
			op.Fail(guestError(err))
			return
		}
		end, err := g.wait(client, gop.ID)
		switch {
		case err != nil:
			op.Fail(err)
			return
		case end.State == agent.OperationCancelled:
			op.Cancelled()
			return
		case end.State != agent.OperationDone:
			op.Fail(errors.New(end.Error))
			return
		}
		var r agent.StorageCheck
		if err := json.Unmarshal(end.Result, &r); err != nil {
			op.Fail(err)
			return
		}
		if r.State == agent.OperationFailed || r.State == agent.OperationCancelled {
			why := r.Error
			if why == "" {
				why = "the check " + r.State
			}
			op.Fail(errors.New(why))
			return
		}
		s.host.Log().Info("data: checked", "auto", r.Auto, "read_only", r.ReadOnly, "scrubbed_bytes", r.ScrubbedBytes,
			"corrected", r.Corrected, "uncorrectable", r.Uncorrectable)
		op.Done(api.StorageChecked{Auto: r.Auto, ReadOnly: r.ReadOnly, ScrubbedBytes: r.ScrubbedBytes, Corrected: r.Corrected, Uncorrectable: r.Uncorrectable})
	}), nil
}
