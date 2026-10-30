package agent

import (
	"encoding/json"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"
)

type K8sCRI interface {
	K8sCRI(on bool) (api.K8sStatus, error)
}

func (s *Server) handleK8sCRI(method string, params json.RawMessage) (any, bool, error) {
	if method != api.MethodK8sCRI {
		return nil, false, nil
	}
	k, ok := s.Sys.(K8sCRI)
	if !ok {
		return nil, true, failed(ErrNotImplemented)
	}
	p, err := jsonrpc.Bind[api.K8sCRIParams](params)
	if err != nil {
		return nil, true, err
	}
	st, err := k.K8sCRI(p.On)
	if err != nil {
		return nil, true, failed(err)
	}
	return st, true, nil
}
