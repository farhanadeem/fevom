package standin

import (
	"context"
	"encoding/json"
	"sort"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"
)

const standinEngineBytes = 3 << 30

func (a *Agent) storageUsage(p api.UsageParams) (any, error) {
	if a.NoData != "" {
		return nil, jsonrpc.Errorf(api.CodeFailed, "%s: %s", api.NoDataFilesystem, a.NoData)
	}
	m := a.machines()
	m.mu.Lock()
	u := api.StorageUsage{Machines: []api.MachineUsage{}, EngineBytes: standinEngineBytes}
	for _, fm := range m.byName {
		u.Machines = append(u.Machines, api.MachineUsage{Name: fm.Info.Name, DiskBytes: fm.Info.DiskBytes})
		u.UsedBytes += fm.Info.DiskBytes
	}
	for _, k := range m.kept {
		u.ImagesBytes += k.DiskBytes
	}
	m.mu.Unlock()
	sort.Slice(u.Machines, func(i, j int) bool { return u.Machines[i].Name < u.Machines[j].Name })
	u.UsedBytes += u.ImagesBytes + u.EngineBytes + standinUsed
	a.mu.Lock()
	if a.debug.made {
		u.DebugBytes = standinDebugBytes
		u.UsedBytes += standinDebugBytes
	}
	a.mu.Unlock()
	a.mu.Lock()
	ready := a.engine.State == api.EngineReady
	a.mu.Unlock()
	if p.Engine && ready {
		u.Engine = &api.EngineUsage{ImagesBytes: 2 << 30, ContainersBytes: 100 << 20, VolumesBytes: 800 << 20, BuildCacheBytes: 124 << 20}
	}
	a.k8s.mu.Lock()
	cluster, cache := a.k8s.saved.Subvolume, a.k8s.saved.Cached
	a.k8s.mu.Unlock()
	if cluster || cache {
		k := &api.KubernetesUsage{}
		if cluster {
			k.ClusterBytes = standinK8sBytes
		}
		if cache {
			k.CacheBytes = standinCacheBytes
		}
		u.UsedBytes += k.ClusterBytes + k.CacheBytes
		if p.Engine && ready && cluster {
			k.ImagesBytes, k.ImagesCounted = standinK8sImagesBytes, true
		}
		k.Bytes = k.ClusterBytes + k.CacheBytes + k.ImagesBytes
		u.Kubernetes = k
	}
	return u, nil
}

func (a *Agent) storageSalvage(ctx context.Context, c *jsonrpc.Conn, p api.SalvageParams) (any, error) {
	if p.Dest == "" {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "dest is required")
	}
	for _, name := range p.Machines {
		if err := api.CheckMachineName(name); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
		}
	}
	m := a.machines()
	op := m.startOp(c, api.MethodStorageSalvage, nil)
	method, phase := api.SalvageCopy, api.PhaseCopy
	if a.NoData != "" {
		method, phase = api.SalvageRestore, api.PhaseRestore
	}
	files := int64(len(p.Volumes) + len(p.Machines))
	jsonrpc.AfterReply(ctx, func() {
		go func() {
			defer m.endOp(op)
			op.progress(api.OperationProgress{Phase: phase})
			result, _ := json.Marshal(api.Salvaged{Method: method, Files: files})
			op.progress(api.OperationProgress{State: api.OperationDone, Phase: phase, Result: result})
		}()
	})
	return api.Operation{ID: op.id}, nil
}
