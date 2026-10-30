package agent

import (
	"context"
	"errors"
	"fmt"

	"fev/proto/jsonrpc"
)

var ErrDebugToolsUnavailable = errors.New("the debug tools could not be prepared")

func IsDebugToolsUnavailable(err error) bool {
	var e *jsonrpc.Error
	return errors.As(err, &e) && e.Code == CodeDebugToolsUnavailable
}

const (
	MethodDebugStatus = "debug.status"
	MethodDebugReset  = "debug.reset"
)

const (
	DebugKindRunning = "running"
	DebugKindStopped = "stopped"
	DebugKindImage   = "image"
)

const (
	DebugRequestContainer = "container"
	DebugRequestImage     = "image"
)

const (
	ExecReasonToolsUnavailable = "debug.tools_unavailable"
	ExecReasonTargetGone       = "debug.target_gone"
)

type DebugTarget struct {
	Target   string `json:"target"`
	Kind     string `json:"kind,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
	CacheURL string `json:"cache_url,omitempty"`
	IndexURL string `json:"index_url,omitempty"`
}

type DebugStarted struct {
	Kind     string   `json:"kind"`
	TargetID string   `json:"target_id"`
	Name     string   `json:"name"`
	Image    string   `json:"image,omitempty"`
	ReadOnly bool     `json:"read_only"`
	Notes    []string `json:"notes,omitempty"`
}

type DebugStatus struct {
	Exists       bool           `json:"exists"`
	Bytes        int64          `json:"bytes"`
	Paths        int            `json:"paths"`
	Packages     []DebugPackage `json:"packages,omitempty"`
	DefaultSet   string         `json:"default_set,omitempty"`
	DefaultWhole bool           `json:"default_whole"`
	IndexDate    string         `json:"index_date,omitempty"`
	Sessions     int            `json:"sessions"`
}

type DebugPackage struct {
	Name      string   `json:"name"`
	Attr      string   `json:"attr,omitempty"`
	StorePath string   `json:"store_path"`
	Commands  []string `json:"commands,omitempty"`
	Default   bool     `json:"default,omitempty"`
}

type DebugResetParams struct {
	Force bool `json:"force,omitempty"`
}

type DebugReset struct {
	FreedBytes int64 `json:"freed_bytes"`
}

func CheckDebugStart(p ExecStartParams) error {
	if (p.Context == ExecDebug) != (p.Debug != nil) {
		return fmt.Errorf("debug: context %q with debug %v", p.Context, p.Debug != nil)
	}
	if p.Debug != nil && p.Debug.Target == "" {
		return fmt.Errorf("debug: no target")
	}
	if p.Debug != nil {
		switch p.Debug.Kind {
		case "", DebugRequestContainer, DebugRequestImage:
		default:
			return fmt.Errorf("debug: unknown kind %q", p.Debug.Kind)
		}
	}
	return nil
}

func CheckDebugStatus(s DebugStatus) error {
	if !s.Exists {
		if s.Bytes != 0 || s.Paths != 0 || len(s.Packages) != 0 || s.DefaultWhole || s.IndexDate != "" {
			return fmt.Errorf("debug: a store that does not exist reports contents")
		}
	}
	if s.Sessions < 0 {
		return fmt.Errorf("debug: sessions %d", s.Sessions)
	}
	return nil
}

func (c *Client) DebugStatus(ctx context.Context) (*DebugStatus, error) {
	var r DebugStatus
	return &r, c.conn.Call(ctx, MethodDebugStatus, Empty{}, &r)
}

func (c *Client) DebugReset(ctx context.Context, force bool) (*DebugReset, error) {
	var r DebugReset
	return &r, c.conn.Call(ctx, MethodDebugReset, DebugResetParams{Force: force}, &r)
}
