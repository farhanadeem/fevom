package agent

import (
	"context"
	"encoding/json"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"
)

type K8s interface {
	K8sStart(ctx context.Context, p api.K8sStartParams, progress func(phase string, done, total int64)) (api.K8sStatus, error)
	K8sStop(p api.K8sStopParams) (api.K8sStatus, error)
	K8sReset(ctx context.Context, p api.K8sResetParams, progress func(phase string, done, total int64)) (api.K8sResetResult, error)
	K8sStatus() api.K8sStatus
	K8sConfigure(cfg api.K8sConfig) (api.K8sStatus, error)
	K8sImagePull(ctx context.Context, ref string) (api.K8sImagePulled, error)
}

func (s *Server) handleK8s(ctx context.Context, c *jsonrpc.Conn, method string, params json.RawMessage) (any, bool, error) {
	switch method {
	case api.MethodK8sStart, api.MethodK8sStop, api.MethodK8sReset, api.MethodK8sStatus, api.MethodK8sConfigure, api.MethodK8sImagePull:
	default:
		return nil, false, nil
	}
	k, ok := s.Sys.(K8s)
	if !ok {
		return nil, true, failed(ErrNotImplemented)
	}
	switch method {
	case api.MethodK8sStatus:
		return k.K8sStatus(), true, nil
	case api.MethodK8sStart:
		p, err := jsonrpc.Bind[api.K8sStartParams](params)
		if err != nil {
			return nil, true, err
		}
		op, err := s.startOperation(ctx, c, method, false, func(ctx context.Context, op *operation) (any, error) {
			return k.K8sStart(ctx, p, op.progress)
		})
		return op, true, err
	case api.MethodK8sStop:
		p, err := jsonrpc.Bind[api.K8sStopParams](params)
		if err != nil {
			return nil, true, err
		}
		st, err := k.K8sStop(p)
		if err != nil {
			return nil, true, failed(err)
		}
		return st, true, nil
	case api.MethodK8sReset:
		p, err := jsonrpc.Bind[api.K8sResetParams](params)
		if err != nil {
			return nil, true, err
		}
		op, err := s.startOperation(ctx, c, method, false, func(ctx context.Context, op *operation) (any, error) {
			return k.K8sReset(ctx, p, op.progress)
		})
		return op, true, err
	case api.MethodK8sConfigure:
		p, err := jsonrpc.Bind[api.K8sConfig](params)
		if err != nil {
			return nil, true, err
		}
		st, err := k.K8sConfigure(p)
		if err != nil {
			return nil, true, failed(err)
		}
		return st, true, nil
	case api.MethodK8sImagePull:
		p, err := jsonrpc.Bind[api.K8sImagePullParams](params)
		if err != nil {
			return nil, true, err
		}
		if p.Ref == "" {
			return nil, true, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "k8s.image.pull: no ref")
		}
		r, err := k.K8sImagePull(ctx, p.Ref)
		if err != nil {
			return nil, true, failed(err)
		}
		return r, true, nil
	}
	return nil, true, failed(ErrNotImplemented)
}
