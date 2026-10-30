package agent

import "context"

const (
	MethodK8sCRI = "k8s.cri"
)

type K8sCRIParams struct {
	On bool `json:"on"`
}

type K8sStopParams struct {
	KeepCRI bool `json:"keep_cri,omitempty"`
}

func (c *Client) K8sCRI(ctx context.Context, on bool) (*K8sStatus, error) {
	var r K8sStatus
	return &r, c.conn.Call(ctx, MethodK8sCRI, K8sCRIParams{On: on}, &r)
}

func (c *Client) K8sStopKeepingCRI(ctx context.Context) (*K8sStatus, error) {
	var r K8sStatus
	return &r, c.conn.Call(ctx, MethodK8sStop, K8sStopParams{KeepCRI: true}, &r)
}
