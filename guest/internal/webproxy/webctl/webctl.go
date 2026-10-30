package webctl

import (
	api "fev/proto/agent"
)

const (
	Path      = "/system/bin/fev-webproxy"
	StageName = "fev-webproxy"
	ControlFD = 3
)

const ListenFDsEnv = "FEV_WEBPROXY_LISTEN_FDS"

type Control struct {
	Domains   *api.DomainsParams `json:"domains,omitempty"`
	NetNSRoot string             `json:"netns_root,omitempty"`
	Machines  map[string]string  `json:"machines,omitempty"`
	Names6    string             `json:"names6,omitempty"`
}

type Report struct {
	Ready      bool   `json:"ready,omitempty"`
	Unredirect string `json:"unredirect,omitempty"`
	Error      string `json:"error,omitempty"`
}
