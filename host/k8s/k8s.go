package k8s

import (
	"context"
	"errors"
	"io"

	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

type Config struct {
	Kubeconfig     string        `json:"kubeconfig"`
	Relay          string        `json:"relay"`
	Ranges         api.K8sRanges `json:"ranges"`
	LAN            bool          `json:"lan,omitempty"`
	APIAddress     string        `json:"api_address,omitempty"`
	KubeconfigPath string        `json:"kubeconfig_path,omitempty"`
}

type Reach struct {
	Watching  bool             `json:"watching"`
	Error     string           `json:"error,omitempty"`
	Node      string           `json:"node,omitempty"`
	Version   string           `json:"version,omitempty"`
	Ready     bool             `json:"ready"`
	Targets   []Target         `json:"targets,omitempty"`
	Forwards  []Forward        `json:"forwards,omitempty"`
	Services  []api.K8sService `json:"services,omitempty"`
	Ingresses []api.K8sIngress `json:"ingresses,omitempty"`
	Pulls     []string         `json:"pulls,omitempty"`
}

type Target struct {
	ID         string   `json:"id"`
	Address    string   `json:"address"`
	Names      []string `json:"names"`
	HTTPPort   int      `json:"http_port,omitempty"`
	HTTPSPort  int      `json:"https_port,omitempty"`
	Redirect80 bool     `json:"redirect80,omitempty"`
	Own443     bool     `json:"own443,omitempty"`
	Pass443    bool     `json:"pass443,omitempty"`
	Hosts      []string `json:"hosts,omitempty"`
}

type Forward struct {
	Proto string `json:"proto"`
	Port  int    `json:"port"`
	LAN   bool   `json:"lan,omitempty"`
}

type Backend interface {
	Start(ctx context.Context, cfg Config, reach func(Reach)) error
	SetLAN(lan bool)
	Watch(ctx context.Context, p api.K8sWatchParams, deliver func(api.K8sChanged)) ([]api.K8sObject, error)
	Get(ctx context.Context, ref api.K8sRef) (api.K8sManifest, error)
	Apply(ctx context.Context, p api.K8sApplyParams) (api.K8sApplyResult, error)
	Update(ctx context.Context, p api.K8sUpdateParams) (api.K8sApplyResult, error)
	Scale(ctx context.Context, p api.K8sScaleParams) (api.K8sObject, error)
	Restart(ctx context.Context, ref api.K8sRef) (api.K8sObject, error)
	Delete(ctx context.Context, ref api.K8sRef) error
	Logs(ctx context.Context, p api.K8sLogsParams, text func(string)) error
	ExecTarget(ctx context.Context, p api.K8sExecParams) (container string, err error)
	Exec(ctx context.Context, p api.K8sExecParams, stream io.ReadWriter, resize <-chan [2]int) (int, error)
	Close()
}

func Errorf(code int, format string, args ...any) error {
	return jsonrpc.Errorf(code, format, args...)
}

func ErrNotBuilt(what string) error {
	return jsonrpc.Errorf(api.CodeNotImplemented, "%s: not built in this build", what)
}

func IsCode(err error, code int) bool {
	var e *jsonrpc.Error
	return errors.As(err, &e) && e.Code == code
}
