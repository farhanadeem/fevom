package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"

	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

func (m *Machines) engineGuest(ctx context.Context) (*agent.Client, error) {
	if _, err := m.guest(ctx, true); err != nil {
		return nil, err
	}
	st, err := m.host.WaitStatus(ctx, func(st api.Status) bool {
		return st.State == api.StateRunning || st.State == api.StatePaused || st.State == api.StateStopped
	})
	if err != nil {
		return nil, err
	}
	if st.State == api.StateStopped {
		return nil, jsonrpc.Errorf(api.CodeFailed, "the VM stopped before its engine was ready")
	}
	return m.guest(ctx, true)
}

func volumeName(name, what string) error {
	if err := agent.CheckVolumeName(name); err != nil {
		return jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%s: %v", what, err)
	}
	return nil
}

func (m *Machines) cloneVolume(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.VolumesCloneParams](params)
	if err != nil {
		return nil, err
	}
	if err := volumeName(p.Name, "the volume"); err != nil {
		return nil, err
	}
	if err := volumeName(p.NewName, "the new volume"); err != nil {
		return nil, err
	}
	client, err := m.engineGuest(ctx)
	if err != nil {
		return nil, err
	}
	v, err := client.VolumeClone(ctx, p.Name, p.NewName)
	if err != nil {
		return nil, guestError(err)
	}
	m.log.Info("volumes: cloned", "volume", p.Name, "new", v.Name, "files", v.Files)
	return api.Volume{Name: v.Name, Labels: v.Labels, Files: v.Files}, nil
}

func (m *Machines) exportVolume(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.VolumesExportParams](params)
	if err != nil {
		return nil, err
	}
	if err := volumeName(p.Name, "the volume"); err != nil {
		return nil, err
	}
	path, err := macPath(p.Path)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(filepath.Dir(path)); err != nil || !st.IsDir() {
		return nil, jsonrpc.Errorf(api.CodeFailed, "%s is not a folder on this Mac", filepath.Dir(path))
	}
	if _, err := os.Lstat(path); err == nil && !p.Replace {
		return nil, jsonrpc.Errorf(api.CodeFailed, "%s exists; it is replaced only when asked", path)
	}
	return m.ops.Start(ctx, api.MethodVolumesExport, p.Name, agent.PhaseArchive, func(op *Operation) {
		octx := op.Context()
		client, err := m.engineGuest(octx)
		if err != nil {
			op.Fail(err)
			return
		}
		m.mu.Lock()
		vm := m.vm
		m.mu.Unlock()
		tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".fev-*")
		if err != nil {
			op.Fail(err)
			return
		}
		kept := false
		defer func() {
			tmp.Close()
			if !kept {
				os.Remove(tmp.Name())
			}
		}()
		g := newGuestOp(op)
		gop, err := client.VolumeExport(octx, p.Name, g.progress)
		if err != nil {
			op.Fail(guestError(err))
			return
		}
		plain, err := m.receiveArchive(octx, vm, gop, tmp, op)
		if err != nil {
			ctx, cancel := context.WithTimeout(context.Background(), guestCancelBound)
			client.OperationCancel(ctx, gop.ID)
			cancel()
			op.Fail(fmt.Errorf("the archive: %w", err))
			return
		}
		end, err := g.wait(client, gop.ID)
		switch {
		case err != nil:
			op.Fail(err)
			return
		case end.State == agent.OperationCancelled:
			op.Cancelled()
			return
		case end.State != agent.OperationDone:
			op.Fail(errors.New(end.Error))
			return
		}
		var r agent.VolumeExported
		if json.Unmarshal(end.Result, &r) == nil && r.Bytes != plain {
			op.Fail(fmt.Errorf("the guest wrote %d bytes of the archive and %d arrived", r.Bytes, plain))
			return
		}
		if err := tmp.Sync(); err != nil {
			op.Fail(err)
			return
		}
		st, err := tmp.Stat()
		if err != nil {
			op.Fail(err)
			return
		}
		if p.Replace {
			err = os.Rename(tmp.Name(), path)
		} else {
			err = unix.RenameatxNp(unix.AT_FDCWD, tmp.Name(), unix.AT_FDCWD, path, unix.RENAME_EXCL)
		}
		if err != nil {
			op.Fail(fmt.Errorf("%s: %w", path, err))
			return
		}
		kept = true
		m.log.Info("volumes: exported", "volume", p.Name, "path", path, "bytes", st.Size(), "tar_bytes", plain)
		op.Progress(agent.PhaseArchive, plain, plain)
		op.Done(api.VolumeExported{Path: path, Bytes: st.Size()})
	}), nil
}

func (m *Machines) importVolume(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.VolumesImportParams](params)
	if err != nil {
		return nil, err
	}
	path, err := macPath(p.Path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, jsonrpc.Errorf(api.CodeFailed, "%v", err)
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, jsonrpc.Errorf(api.CodeFailed, "%s is not a file", path)
	}
	read := &countReader{r: f}
	zr, err := zstd.NewReader(read)
	if err != nil {
		f.Close()
		return nil, jsonrpc.Errorf(api.CodeFailed, "%s: %v", path, err)
	}
	fail := func(code int, format string, args ...any) (any, error) {
		zr.Close()
		f.Close()
		return nil, jsonrpc.Errorf(code, format, args...)
	}
	cfg, body, err := readArchiveConfig(zr, "a Docker volume's")
	if err != nil {
		return fail(api.CodeFailed, "%s: %v", path, err)
	}
	var was agent.VolumeArchiveConfig
	raw, _ := json.Marshal(cfg)
	json.Unmarshal(raw, &was)
	if was.Kind != agent.VolumeArchiveKind {
		return fail(api.CodeFailed, "%s: this is not a Docker volume's archive (a machine's is made with fev import)", path)
	}
	name := p.Name
	if name == "" {
		name = was.Name
	}
	if err := agent.CheckVolumeName(name); err != nil {
		return fail(jsonrpc.CodeInvalidParams, "the volume's name: %v", err)
	}
	return m.ops.Start(ctx, api.MethodVolumesImport, name, agent.PhaseArchive, func(op *Operation) {
		defer f.Close()
		defer zr.Close()
		octx := op.Context()
		client, err := m.engineGuest(octx)
		if err != nil {
			op.Fail(err)
			return
		}
		m.mu.Lock()
		vm := m.vm
		m.mu.Unlock()
		g := newGuestOp(op)
		total := st.Size()
		var sent atomic.Int64
		on := func(n int64) {
			sent.Store(n)
			op.Progress(agent.PhaseArchive, n, total)
		}
		read.on.Store(&on)
		gop, err := client.VolumeImport(octx, name, func(p agent.OperationProgress) {
			if p.State != agent.OperationRunning {
				g.progress(p)
			}
		})
		if err != nil {
			op.Fail(guestError(err))
			return
		}
		if err := sendArchive(octx, vm, gop, body); err != nil {
			ctx, cancel := context.WithTimeout(context.Background(), guestCancelBound)
			client.OperationCancel(ctx, gop.ID)
			cancel()
			op.Fail(fmt.Errorf("the archive: %w", err))
			return
		}
		end, err := g.wait(client, gop.ID)
		switch {
		case err != nil:
			op.Fail(err)
			return
		case end.State == agent.OperationCancelled:
			op.Cancelled()
			return
		case end.State != agent.OperationDone:
			op.Fail(errors.New(end.Error))
			return
		}
		var v agent.VolumeInfo
		if err := json.Unmarshal(end.Result, &v); err != nil || v.Name != name {
			op.Fail(fmt.Errorf("the guest's import ended with a result that is not this volume's: %s", clip(string(end.Result), 400)))
			return
		}
		m.log.Info("volumes: imported", "volume", name, "path", path, "files", v.Files)
		op.Progress(agent.PhaseArchive, sent.Load(), total)
		op.Done(api.Volume{Name: v.Name, Labels: v.Labels, Files: v.Files})
	}), nil
}
