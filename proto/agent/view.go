package agent

import (
	"context"
	"fmt"
)

const (
	MethodViewStart   = "view.start"
	MethodViewStop    = "view.stop"
	MethodViewStatus  = "view.status"
	MethodViewObjects = "view.objects"

	NotifyViewChanged = "view.changed"
	NotifyViewRemoved = "view.removed"
)

const (
	ViewStopped  = "stopped"
	ViewStarting = "starting"
	ViewReady    = "ready"
	ViewFailed   = "failed"
)

type ViewStartParams struct {
	UID      uint32   `json:"uid"`
	GID      uint32   `json:"gid"`
	User     string   `json:"user,omitempty"`
	Group    string   `json:"group,omitempty"`
	Threads  int      `json:"threads,omitempty"`
	Isolated []string `json:"isolated,omitempty"`
}

type ViewObjects struct {
	Machines   int `json:"machines"`
	Volumes    int `json:"volumes"`
	Containers int `json:"containers"`
	Images     int `json:"images"`
}

type ViewStatus struct {
	State     string       `json:"state"`
	Error     string       `json:"error,omitempty"`
	Since     string       `json:"since"`
	Objects   *ViewObjects `json:"objects,omitempty"`
	VsockPort uint32       `json:"vsock_port,omitempty"`
}

const ViewVsockPort = 2049

const (
	ViewMachine   = "machine"
	ViewVolume    = "volume"
	ViewContainer = "container"
	ViewImage     = "image"
	ViewBind      = "bind"
)

type ViewObject struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	FSID     uint32 `json:"fsid"`
	UID      uint32 `json:"uid"`
	GID      uint32 `json:"gid"`
	ReadOnly bool   `json:"read_only,omitempty"`
	Mounted  bool   `json:"mounted"`
}

type ViewObjectList struct {
	Objects []ViewObject `json:"objects"`
}

type ViewRemoved struct {
	Paths []string `json:"paths"`
}

func CheckViewStatus(s ViewStatus) error {
	switch s.State {
	case ViewStopped, ViewStarting, ViewReady, ViewFailed:
	default:
		return fmt.Errorf("view: unknown state %q", s.State)
	}
	if (s.State == ViewFailed) != (s.Error != "") {
		return fmt.Errorf("view: an error with state %s: %q", s.State, s.Error)
	}
	if (s.State == ViewReady) != (s.Objects != nil) {
		return fmt.Errorf("view: objects with state %s", s.State)
	}
	if s.Since == "" {
		return fmt.Errorf("view: no since")
	}
	return nil
}

func (c *Client) ViewStart(ctx context.Context, p ViewStartParams) (*ViewStatus, error) {
	var r ViewStatus
	return &r, c.conn.Call(ctx, MethodViewStart, p, &r)
}

func (c *Client) ViewStop(ctx context.Context) (*ViewStatus, error) {
	var r ViewStatus
	return &r, c.conn.Call(ctx, MethodViewStop, Empty{}, &r)
}

func (c *Client) ViewStatus(ctx context.Context) (*ViewStatus, error) {
	var r ViewStatus
	return &r, c.conn.Call(ctx, MethodViewStatus, Empty{}, &r)
}

func (c *Client) ViewObjects(ctx context.Context) (*ViewObjectList, error) {
	var r ViewObjectList
	return &r, c.conn.Call(ctx, MethodViewObjects, Empty{}, &r)
}

func (c *Client) OnViewRemoved(deliver func(ViewRemoved)) {
	c.mu.Lock()
	c.viewRemoved = deliver
	c.mu.Unlock()
}

func (c *Client) OnViewChanged(deliver func(ViewStatus)) {
	c.mu.Lock()
	c.views = deliver
	c.mu.Unlock()
}
