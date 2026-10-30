package agent

import (
	"encoding/json"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"
)

type Domains interface {
	DomainsSet(api.DomainsParams) (api.DomainsApplied, error)
}

type EnginePorts interface {
	EnginePorts() api.EnginePorts
}

type MacAddresses interface {
	MacAddressesSet(addresses []string) error
}

func (s *Server) handleDomains(method string, params json.RawMessage) (any, bool, error) {
	switch method {
	case api.MethodEnginePorts:
		e, ok := s.Sys.(EnginePorts)
		if !ok {
			return nil, true, failed(ErrNotImplemented)
		}
		return e.EnginePorts(), true, nil
	case api.MethodMacAddressesSet:
		m, ok := s.Sys.(MacAddresses)
		if !ok {
			return nil, true, failed(ErrNotImplemented)
		}
		p, err := jsonrpc.Bind[api.MacAddresses](params)
		if err != nil {
			return nil, true, err
		}
		if err := api.CheckMacAddresses(p); err != nil {
			return nil, true, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
		}
		if err := m.MacAddressesSet(p.Addresses); err != nil {
			return nil, true, failed(err)
		}
		return api.Empty{}, true, nil
	case api.MethodDomainsSet:
	default:
		return nil, false, nil
	}
	d, ok := s.Sys.(Domains)
	if !ok {
		return nil, true, failed(ErrNotImplemented)
	}
	p, err := jsonrpc.Bind[api.DomainsParams](params)
	if err != nil {
		return nil, true, err
	}
	if err := api.CheckDomains(p); err != nil {
		return nil, true, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	r, err := d.DomainsSet(p)
	if err != nil {
		return nil, true, failed(err)
	}
	return r, true, nil
}
