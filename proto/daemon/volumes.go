package daemon

import "context"

const (
	MethodVolumesClone  = "volumes.clone"
	MethodVolumesExport = "volumes.export"
	MethodVolumesImport = "volumes.import"
)

type VolumesCloneParams struct {
	Name    string `json:"name"`
	NewName string `json:"new_name"`
}

type Volume struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Files  int64             `json:"files"`
}

type VolumesExportParams struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Replace bool   `json:"replace,omitempty"`
}

type VolumeExported struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

type VolumesImportParams struct {
	Path string `json:"path"`
	Name string `json:"name,omitempty"`
}

func (c *Client) CloneVolume(ctx context.Context, name, newName string) (*Volume, error) {
	var r Volume
	return &r, c.conn.Call(ctx, MethodVolumesClone, VolumesCloneParams{Name: name, NewName: newName}, &r)
}

func (c *Client) ExportVolume(ctx context.Context, p VolumesExportParams) (*Operation, error) {
	var r Operation
	return &r, c.conn.Call(ctx, MethodVolumesExport, p, &r)
}

func (c *Client) ImportVolume(ctx context.Context, p VolumesImportParams) (*Operation, error) {
	var r Operation
	return &r, c.conn.Call(ctx, MethodVolumesImport, p, &r)
}
