package daemon

import "context"

const TypeList = "list"

const ApplyMachineRestart = "machine_restart"

const (
	MethodConfigAdd    = "config.add"
	MethodConfigRemove = "config.remove"
	MethodHTTPSTrust   = "https.trust"
)

type ConfigListParams struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}

const (
	TrustInstall = "install"
	TrustRemove  = "remove"
	TrustRotate  = "rotate"
)

type HTTPSTrustParams struct {
	Action string `json:"action"`
}

const (
	KeyNetworkSubnet           = "network.subnet"
	KeyNetworkSubnet6          = "network.subnet6"
	KeyNetworkDirectAccess     = "network.direct_access"
	KeyNetworkHTTPS            = "network.https"
	KeyNetworkProxy            = "network.proxy"
	KeyNetworkProxyExclude     = "network.proxy_exclude"
	KeyNetworkProxyTransparent = "network.proxy.transparent"
	KeyAdminEnabled            = "admin.enabled"
)

const (
	MachineKeyIsolated        = "isolated"
	MachineKeyEmulation       = "emulation"
	MachineKeyIsolateNetwork  = "isolate_network"
	MachineKeyMounts          = "mounts"
	MachineKeyForwardSSHAgent = "forward_ssh_agent"
	MachineKeyHTTPPort        = "http_port"
	MachineKeyHTTPSPort       = "https_port"
)

type NetworkStatus struct {
	Subnet       string          `json:"subnet"`
	Subnet6      string          `json:"subnet6,omitempty"`
	DirectAccess string          `json:"direct_access"`
	Reason       string          `json:"reason,omitempty"`
	Interface    string          `json:"interface,omitempty"`
	Routes       []string        `json:"routes,omitempty"`
	Address6     string          `json:"address6,omitempty"`
	NAT64        []string        `json:"nat64,omitempty"`
	Via          string          `json:"via,omitempty"`
	NoHelper     string          `json:"no_helper,omitempty"`
	Conflicts    []RouteConflict `json:"conflicts,omitempty"`
	Helper       *HelperStatus   `json:"helper,omitempty"`
}

const (
	DirectOn       = "on"
	DirectOff      = "off"
	DirectDegraded = "degraded"
)

const ViaVmnet = "vmnet"

const (
	DirectSetting    = "setting"
	DirectAdminOff   = "admin_off"
	DirectNoHelper   = "no_helper"
	DirectOutdated   = "helper_outdated"
	DirectOtherUser  = "other_user"
	DirectHelperOff  = "helper_off"
	DirectFailed     = "failed"
	DirectHelperHeld = "helper_held"
)

type RouteConflict struct {
	Prefix    string `json:"prefix"`
	Route     string `json:"route"`
	Interface string `json:"interface"`
	Withdrawn bool   `json:"withdrawn"`
}

type HelperStatus struct {
	ProtocolVersion int    `json:"protocol_version"`
	SecurityLevel   int    `json:"security_level"`
	Build           string `json:"build,omitempty"`
	Outdated        bool   `json:"outdated,omitempty"`
}

type DomainsStatus struct {
	Resolver   string            `json:"resolver"`
	Port       int               `json:"port"`
	Names      int               `json:"names"`
	Local      []LocalNameStatus `json:"local,omitempty"`
	Collisions []string          `json:"collisions,omitempty"`
	MDNS       string            `json:"mdns,omitempty"`
}

type LocalNameStatus struct {
	Name       string `json:"name"`
	Container  string `json:"container"`
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	AnsweredBy string `json:"answered_by,omitempty"`
}

const (
	LocalWaiting = "waiting"
	LocalMDNS    = "mdns"
)

type HTTPSStatus struct {
	Enabled  bool   `json:"enabled"`
	Trust    string `json:"trust"`
	Root     string `json:"root,omitempty"`
	Key      string `json:"key"`
	Identity string `json:"identity,omitempty"`
	Error    string `json:"error,omitempty"`
}

const (
	TrustTrusted   = "trusted"
	TrustUntrusted = "untrusted"
	TrustAbsent    = "absent"
	TrustDeclined  = "declined"
)

const (
	KeyIdentity = "identity"
	KeySigner   = "signer"
	KeyFile     = "file"
	KeyNone     = "none"
	KeyUnusable = "unusable"
)

type DataStatus struct {
	Budget *DataBudget `json:"budget,omitempty"`
}

type DataBudget struct {
	ReferencedBytes int64  `json:"referenced_bytes"`
	BudgetBytes     int64  `json:"budget_bytes"`
	LimitBytes      int64  `json:"limit_bytes"`
	Limited         bool   `json:"limited"`
	At              string `json:"at"`
}

func (c *Client) ConfigAdd(ctx context.Context, key string, values ...string) ([]ConfigChange, error) {
	var r ConfigChanges
	return r.Changed, c.conn.Call(ctx, MethodConfigAdd, ConfigListParams{Key: key, Values: nonNil(values)}, &r)
}

func (c *Client) ConfigRemove(ctx context.Context, key string, values ...string) ([]ConfigChange, error) {
	var r ConfigChanges
	return r.Changed, c.conn.Call(ctx, MethodConfigRemove, ConfigListParams{Key: key, Values: nonNil(values)}, &r)
}

func (c *Client) HTTPSTrust(ctx context.Context, action string) (*HTTPSStatus, error) {
	var r HTTPSStatus
	return &r, c.conn.Call(ctx, MethodHTTPSTrust, HTTPSTrustParams{Action: action}, &r)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
