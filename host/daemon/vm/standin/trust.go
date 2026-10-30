package standin

import (
	"encoding/json"
	"sort"
	"sync"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"
)

type trustState struct {
	mu         sync.Mutex
	set        *api.Trusted
	calls      int
	agentPath  string
	agentCalls int
}

func (a *Agent) trustSet(params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.TrustParams](params)
	if err != nil {
		return nil, err
	}
	ders, err := api.TrustDER(p.Roots)
	if err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	r := api.Trusted{Count: api.TrustCount(ders), Digest: api.TrustDigest(ders)}
	a.trust.mu.Lock()
	a.trust.set, a.trust.calls = &r, a.trust.calls+1
	a.trust.mu.Unlock()
	a.Logf(api.LogInit, 6, "agent: the Mac's trusted roots: %d (%.12s), installed in each machine at its next start", r.Count, r.Digest)
	return r, nil
}

func (a *Agent) Trusted() (*api.Trusted, int) {
	a.trust.mu.Lock()
	defer a.trust.mu.Unlock()
	if a.trust.set == nil {
		return nil, a.trust.calls
	}
	t := *a.trust.set
	return &t, a.trust.calls
}

func (a *Agent) sshAgentSet(params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.SSHAgentParams](params)
	if err != nil {
		return nil, err
	}
	if err := api.CheckSSHAgentPath(p.Path); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	r := api.SSHAgentBound{Machines: []string{}}
	a.mu.Lock()
	r.Engine = p.Path != "" && a.engine.State == api.EngineReady
	a.mu.Unlock()
	if p.Path != "" {
		m := a.machines()
		m.mu.Lock()
		for name, fm := range m.byName {
			if fm.Info.State == api.MachineRunning {
				r.Machines = append(r.Machines, name)
			}
		}
		m.mu.Unlock()
		sort.Strings(r.Machines)
	}
	a.trust.mu.Lock()
	a.trust.agentPath, a.trust.agentCalls = p.Path, a.trust.agentCalls+1
	a.trust.mu.Unlock()
	a.Logf(api.LogInit, 6, "agent: the Mac's SSH agent at %q in the engine and the machines", p.Path)
	return r, nil
}

func (a *Agent) SSHAgentPath() (string, int) {
	a.trust.mu.Lock()
	defer a.trust.mu.Unlock()
	return a.trust.agentPath, a.trust.agentCalls
}
