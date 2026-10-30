package standin

import (
	"encoding/json"
	"sync"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"
)

type domainsState struct {
	mu  sync.Mutex
	set *api.DomainsParams
}

func (a *Agent) domainsSet(params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.DomainsParams](params)
	if err != nil {
		return nil, err
	}
	if err := api.CheckDomains(p); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	a.domains.mu.Lock()
	defer a.domains.mu.Unlock()
	if a.domains.set != nil && p.Seq < a.domains.set.Seq {
		return api.DomainsApplied{Seq: a.domains.set.Seq, Targets: len(a.domains.set.Targets)}, nil
	}
	a.domains.set = &p
	return api.DomainsApplied{Seq: p.Seq, Targets: len(p.Targets)}, nil
}

func (a *Agent) Domains() *api.DomainsParams {
	a.domains.mu.Lock()
	defer a.domains.mu.Unlock()
	return a.domains.set
}
