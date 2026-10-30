package agent

import (
	"encoding/json"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"
)

type View interface {
	ViewStart(api.ViewStartParams) api.ViewStatus
	ViewStop() (api.ViewStatus, error)
	ViewStatus() api.ViewStatus
	ViewObjects() api.ViewObjectList
}

const maxViewThreads = 64

func (s *Server) handleView(method string, params json.RawMessage) (any, bool, error) {
	switch method {
	case api.MethodViewStart, api.MethodViewStop, api.MethodViewStatus, api.MethodViewObjects:
	default:
		return nil, false, nil
	}
	v, ok := s.Sys.(View)
	if !ok {
		return nil, true, failed(ErrNotImplemented)
	}
	switch method {
	case api.MethodViewStart:
		p, err := jsonrpc.Bind[api.ViewStartParams](params)
		if err != nil {
			return nil, true, err
		}
		if p.Threads < 0 || p.Threads > maxViewThreads {
			return nil, true, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "threads must be between 0 (the default, 4) and %d", maxViewThreads)
		}
		return v.ViewStart(p), true, nil
	case api.MethodViewStop:
		st, err := v.ViewStop()
		if err != nil {
			return nil, true, failed(err)
		}
		return st, true, nil
	case api.MethodViewObjects:
		return v.ViewObjects(), true, nil
	default:
		return v.ViewStatus(), true, nil
	}
}
