package agent

import (
	"context"
	"encoding/json"
)

const (
	MethodK8sStart     = "k8s.start"
	MethodK8sStop      = "k8s.stop"
	MethodK8sReset     = "k8s.reset"
	MethodK8sStatus    = "k8s.status"
	MethodK8sConfigure = "k8s.configure"
	MethodK8sImagePull = "k8s.image.pull"
	NotifyK8sState     = "k8s.state"
)

const K8sCmdlineParam = "fev.k8s"

const (
	K8sOff      = "off"
	K8sStarting = "starting"
	K8sRunning  = "running"
	K8sStopping = "stopping"
	K8sError    = "error"
)

const (
	K8sPhaseChecking  = "checking"
	K8sPhaseFetching  = "fetching"
	K8sPhaseVerifying = "verifying"
	K8sPhaseImporting = "importing"
	K8sPhaseStarting  = "starting"
	K8sPhaseStopping  = "stopping"
	K8sPhaseDeleting  = "deleting"
)

type K8sStartParams struct {
	ArchiveURL string    `json:"archive_url,omitempty"`
	Config     K8sConfig `json:"config"`
}

type K8sConfig struct {
	Kubelet       json.RawMessage `json:"kubelet,omitempty"`
	MetricsServer bool            `json:"metrics_server,omitempty"`
}

type K8sResetParams struct {
	All     bool `json:"all,omitempty"`
	KeepCRI bool `json:"keep_cri,omitempty"`
}

type K8sResetResult struct {
	FreedBytes  int64 `json:"freed_bytes"`
	ArchiveKept bool  `json:"archive_kept"`
}

type K8sStatus struct {
	Wanted     bool       `json:"wanted"`
	State      string     `json:"state"`
	PID        int        `json:"pid,omitempty"`
	CRI        bool       `json:"cri"`
	Subvolume  bool       `json:"subvolume"`
	Bytes      int64      `json:"bytes"`
	Version    string     `json:"version,omitempty"`
	Archive    K8sArchive `json:"archive"`
	Error      string     `json:"error,omitempty"`
	ImportedMS int64      `json:"imported_ms,omitempty"`
	Kubeconfig string     `json:"kubeconfig,omitempty"`
}

type K8sArchive struct {
	Cached bool   `json:"cached"`
	Bytes  int64  `json:"bytes,omitempty"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
}

type K8sImagePullParams struct {
	Ref string `json:"ref"`
}

type K8sImagePulled struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

func (c *Client) K8sStart(ctx context.Context, p K8sStartParams, progress func(OperationProgress)) (*Operation, error) {
	return c.startOperation(ctx, MethodK8sStart, p, progress)
}

func (c *Client) K8sStop(ctx context.Context) (*K8sStatus, error) {
	var r K8sStatus
	return &r, c.conn.Call(ctx, MethodK8sStop, Empty{}, &r)
}

func (c *Client) K8sReset(ctx context.Context, p K8sResetParams, progress func(OperationProgress)) (*Operation, error) {
	return c.startOperation(ctx, MethodK8sReset, p, progress)
}

func (c *Client) K8sStatus(ctx context.Context) (*K8sStatus, error) {
	var r K8sStatus
	return &r, c.conn.Call(ctx, MethodK8sStatus, Empty{}, &r)
}

func (c *Client) K8sConfigure(ctx context.Context, cfg K8sConfig) (*K8sStatus, error) {
	var r K8sStatus
	return &r, c.conn.Call(ctx, MethodK8sConfigure, cfg, &r)
}

func (c *Client) K8sImagePull(ctx context.Context, ref string) (*K8sImagePulled, error) {
	var r K8sImagePulled
	return &r, c.conn.Call(ctx, MethodK8sImagePull, K8sImagePullParams{Ref: ref}, &r)
}

func (c *Client) OnK8sState(deliver func(K8sStatus)) {
	c.mu.Lock()
	c.k8sState = deliver
	c.mu.Unlock()
}
