package standin

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"sort"
	"strings"
	"time"

	"fev/host/docker/fakeguest"
	api "fev/proto/agent"
	"fev/proto/jsonrpc"
)

type VolumeEngine interface {
	Volume(name string) (fakeguest.VolumeData, bool)
	MakeVolume(v fakeguest.VolumeData) error
}

const volumeArchiveRoot = "data/"

func (a *Agent) volumeCall(ctx context.Context, c *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
	if a.Volumes == nil {
		return nil, jsonrpc.Errorf(api.CodeNotImplemented, "the stand-in has no engine")
	}
	bad := func(err error) error { return jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err) }
	failed := func(err error) error { return jsonrpc.Errorf(api.CodeFailed, "%v", err) }
	noVolume := func(name string) error { return failed(fmt.Errorf("no such volume: %s", name)) }
	m := a.machines()
	switch method {
	case api.MethodVolumeClone:
		p, err := jsonrpc.Bind[api.VolumeCloneParams](params)
		if err != nil {
			return nil, err
		}
		for _, n := range []string{p.Name, p.NewName} {
			if err := api.CheckVolumeName(n); err != nil {
				return nil, bad(err)
			}
		}
		v, ok := a.Volumes.Volume(p.Name)
		if !ok {
			return nil, noVolume(p.Name)
		}
		v.Name = p.NewName
		if err := a.Volumes.MakeVolume(v); err != nil {
			return nil, failed(err)
		}
		return api.VolumeInfo{Name: v.Name, Labels: nilIfEmpty(v.Labels), Files: int64(1 + len(v.Files))}, nil
	case api.MethodVolumeExport:
		p, err := jsonrpc.Bind[api.VolumeNameParams](params)
		if err != nil {
			return nil, err
		}
		if err := api.CheckVolumeName(p.Name); err != nil {
			return nil, bad(err)
		}
		v, ok := a.Volumes.Volume(p.Name)
		if !ok {
			return nil, noVolume(p.Name)
		}
		archive := volumeArchive(v)
		attached := make(chan struct{})
		var written int64
		var werr error
		op := m.startOp(c, method, func(conn net.Conn) {
			defer close(attached)
			written, werr = io.Copy(conn, bytes.NewReader(archive))
			if werr == nil {
				if cw, ok := conn.(interface{ CloseWrite() error }); ok {
					werr = cw.CloseWrite()
				}
			}
		})
		jsonrpc.AfterReply(ctx, func() {
			go func() {
				defer m.endOp(op)
				if err := op.waitStream(attached); err != nil {
					op.fail(api.PhaseArchive, err)
					return
				}
				if werr != nil || op.ctx.Err() != nil {
					op.fail(api.PhaseArchive, fmt.Errorf("the archive's stream: %v", werr))
					return
				}
				result, _ := json.Marshal(api.VolumeExported{Bytes: written})
				op.progress(api.OperationProgress{State: api.OperationDone, Phase: api.PhaseArchive, DoneBytes: written, TotalBytes: written, Result: result})
			}()
		})
		return api.Operation{ID: op.id, Token: op.token}, nil
	default:
		p, err := jsonrpc.Bind[api.VolumeNameParams](params)
		if err != nil {
			return nil, err
		}
		if err := api.CheckVolumeName(p.Name); err != nil {
			return nil, bad(err)
		}
		if _, ok := a.Volumes.Volume(p.Name); ok {
			return nil, failed(fmt.Errorf("the Docker engine has a volume named %s", p.Name))
		}
		attached := make(chan struct{})
		var made fakeguest.VolumeData
		var rerr error
		op := m.startOp(c, method, func(conn net.Conn) {
			defer close(attached)
			made, rerr = readVolumeArchive(conn)
			io.Copy(io.Discard, conn)
		})
		jsonrpc.AfterReply(ctx, func() {
			go func() {
				defer m.endOp(op)
				if err := op.waitStream(attached); err != nil {
					op.fail(api.PhaseArchive, err)
					return
				}
				if rerr != nil || op.ctx.Err() != nil {
					op.fail(api.PhaseArchive, fmt.Errorf("the archive: %v", rerr))
					return
				}
				made.Name = p.Name
				if err := a.Volumes.MakeVolume(made); err != nil {
					op.fail(api.PhaseArchive, err)
					return
				}
				result, _ := json.Marshal(api.VolumeInfo{Name: p.Name, Labels: nilIfEmpty(made.Labels), Files: int64(1 + len(made.Files))})
				op.progress(api.OperationProgress{State: api.OperationDone, Phase: api.PhaseArchive, Result: result})
			}()
		})
		return api.Operation{ID: op.id, Token: op.token}, nil
	}
}

func nilIfEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

func volumeArchive(v fakeguest.VolumeData) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	at := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	config, _ := json.Marshal(api.VolumeArchiveConfig{Version: 1, Kind: api.VolumeArchiveKind, Name: v.Name, Driver: "local",
		Labels: nilIfEmpty(v.Labels), Exported: at.Format(time.RFC3339)})
	tw.WriteHeader(&tar.Header{Name: "_fev/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: at, Format: tar.FormatPAX})
	tw.WriteHeader(&tar.Header{Name: "_fev/v1/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: at, Format: tar.FormatPAX})
	tw.WriteHeader(&tar.Header{Name: archiveConfig, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(config)), ModTime: at, Format: tar.FormatPAX})
	tw.Write(config)
	tw.WriteHeader(&tar.Header{Name: volumeArchiveRoot, Typeflag: tar.TypeDir, Mode: 0o755, ModTime: at, Format: tar.FormatPAX})
	names := make([]string, 0, len(v.Files))
	for n := range v.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		body := v.Files[n]
		tw.WriteHeader(&tar.Header{Name: volumeArchiveRoot + n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)), ModTime: at, Format: tar.FormatPAX})
		io.WriteString(tw, body)
	}
	tw.Close()
	return buf.Bytes()
}

func readVolumeArchive(r io.Reader) (fakeguest.VolumeData, error) {
	tr := tar.NewReader(r)
	var v fakeguest.VolumeData
	var cfg *api.VolumeArchiveConfig
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return v, err
		}
		name := strings.TrimPrefix(path.Clean("/"+h.Name), "/")
		switch {
		case name == archiveConfig:
			cfg = &api.VolumeArchiveConfig{}
			if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(cfg); err != nil {
				return v, fmt.Errorf("%s: %v", archiveConfig, err)
			}
		case name == "_fev" || strings.HasPrefix(name, "_fev/"):
		case name == "data":
			if cfg == nil {
				return v, fmt.Errorf("the archive has no %s before its root", archiveConfig)
			}
		case strings.HasPrefix(name, volumeArchiveRoot):
			if cfg == nil {
				return v, fmt.Errorf("the archive has no %s before its root", archiveConfig)
			}
			if h.Typeflag == tar.TypeReg {
				b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
				if err != nil {
					return v, err
				}
				if v.Files == nil {
					v.Files = map[string]string{}
				}
				v.Files[strings.TrimPrefix(name, volumeArchiveRoot)] = string(b)
			}
		default:
			return v, fmt.Errorf("the archive holds %q, which is neither _fev/ nor data/", h.Name)
		}
	}
	if cfg == nil || cfg.Kind != api.VolumeArchiveKind {
		return v, errors.New("this is not a Docker volume's archive")
	}
	v.Labels = cfg.Labels
	return v, nil
}
