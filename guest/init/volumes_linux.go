package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/agent"
	"fev/guest/internal/logbuf"
	"fev/guest/machines"
	"fev/guest/storage"
)

var _ agent.Volumes = (*system)(nil)

const volumeStaging = engineData + "/.fev-volumes"

const engineDataPath = "/var/lib/docker/"

const volumeCallLimit = 30 * time.Second

var volumeSweep sync.Once

type engineVolume struct {
	Name       string
	Driver     string
	Mountpoint string
	Labels     map[string]string
	Options    map[string]string
}

func (s *system) volumeCall(ctx context.Context, method, p string, body, into any) error {
	if s.engine.sup.Status().State != api.EngineReady {
		return errors.New("the Docker engine is not running")
	}
	ctx, cancel := context.WithTimeout(ctx, volumeCallLimit)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+p, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.engine.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s", agent.ErrNoVolume, strings.TrimPrefix(p, "/volumes/"))
	case resp.StatusCode >= 300:
		var m struct{ Message string }
		json.Unmarshal(b, &m)
		return fmt.Errorf("the Docker engine answered %s %s with %d: %s", method, p, resp.StatusCode, m.Message)
	}
	if into != nil {
		return json.Unmarshal(b, into)
	}
	return nil
}

func (s *system) inspectVolume(ctx context.Context, name string) (*engineVolume, error) {
	var v engineVolume
	if err := s.volumeCall(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), nil, &v); err != nil {
		return nil, err
	}
	if v.Name != name {
		return nil, fmt.Errorf("the Docker engine described volume %s as %q", name, v.Name)
	}
	return &v, nil
}

func volumeFolder(v *engineVolume) (string, error) {
	if v.Driver != "local" {
		return "", fmt.Errorf("the volume %s is of the driver %q: only the local driver's volumes are copied", v.Name, v.Driver)
	}
	if len(v.Options) > 0 {
		return "", fmt.Errorf("the volume %s was made with options (a bind, tmpfs or NFS mount by another name): its files are not the engine's to copy", v.Name)
	}
	mp := path.Clean(v.Mountpoint)
	rest, ok := strings.CutPrefix(mp, engineDataPath)
	if !ok || mp != v.Mountpoint || path.Base(mp) != "_data" || path.Base(path.Dir(mp)) != v.Name {
		return "", fmt.Errorf("the volume %s is at %q, not in the engine's data", v.Name, v.Mountpoint)
	}
	return filepath.Join(engineData, rest), nil
}

func (s *system) copyable(ctx context.Context, name string) (*engineVolume, string, error) {
	v, err := s.inspectVolume(ctx, name)
	if err != nil {
		return nil, "", err
	}
	dir, err := volumeFolder(v)
	return v, dir, err
}

func (s *system) stage() (string, error) {
	volumeSweep.Do(func() {
		if err := os.RemoveAll(volumeStaging); err != nil {
			s.log.Logf(logbuf.Warning, "volumes: an earlier boot's copies could not be cleared: %v", err)
		}
	})
	if err := os.Mkdir(volumeStaging, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	var b [8]byte
	rand.Read(b[:])
	return filepath.Join(volumeStaging, hex.EncodeToString(b[:])), nil
}

func (s *system) newVolume(ctx context.Context, name string, labels map[string]string, staged string) error {
	if _, err := s.inspectVolume(ctx, name); err == nil {
		return fmt.Errorf("the Docker engine has a volume named %s", name)
	} else if !errors.Is(err, agent.ErrNoVolume) {
		return err
	}
	var made engineVolume
	err := s.volumeCall(ctx, http.MethodPost, "/volumes/create", map[string]any{"Name": name, "Driver": "local", "Labels": labels}, &made)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		rctx, cancel := context.WithTimeout(context.Background(), volumeCallLimit)
		defer cancel()
		if rerr := s.volumeCall(rctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), nil, nil); rerr != nil {
			s.log.Logf(logbuf.Warning, "volumes: %s: the volume made for a copy that failed could not be removed: %v", name, rerr)
		}
		return err
	}
	dir, err := volumeFolder(&engineVolume{Name: name, Driver: made.Driver, Mountpoint: made.Mountpoint})
	if err != nil {
		return fail(err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) > 0 {
		return fmt.Errorf("the volume %s was made by somebody else meanwhile; it is left as it is", name)
	}
	if err := unix.Renameat2(unix.AT_FDCWD, staged, unix.AT_FDCWD, dir, unix.RENAME_EXCHANGE); err != nil {
		return fail(fmt.Errorf("put the files in the volume: %w", err))
	}
	return nil
}

func (s *system) VolumeClone(ctx context.Context, name, newName string) (*api.VolumeInfo, error) {
	v, dir, err := s.copyable(ctx, name)
	if err != nil {
		return nil, err
	}
	staged, err := s.stage()
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staged)
	n, err := storage.CloneTree(dir, staged)
	if err != nil {
		return nil, fmt.Errorf("copy %s: %w", name, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.newVolume(ctx, newName, v.Labels, staged); err != nil {
		return nil, err
	}
	s.log.Logf(logbuf.Info, "volumes: %s: cloned from %s, %d entries", newName, name, n)
	return &api.VolumeInfo{Name: newName, Labels: v.Labels, Files: n}, nil
}

func (s *system) VolumeExportable(ctx context.Context, name string) error {
	_, _, err := s.copyable(ctx, name)
	return err
}

func (s *system) VolumeExport(ctx context.Context, name string, w io.Writer, progress agent.Progress) (int64, error) {
	v, dir, err := s.copyable(ctx, name)
	if err != nil {
		return 0, err
	}
	config, _ := json.Marshal(api.VolumeArchiveConfig{
		Version: 1, Kind: api.VolumeArchiveKind, Name: name, Driver: "local", Labels: v.Labels,
		Exported: time.Now().UTC().Format(time.RFC3339),
	})
	progress(api.PhaseArchive, 0, 0)
	cw := &volumeCount{ctx: ctx, w: w, progress: progress}
	n, err := storage.ExportVolume(dir, config, cw, nil)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		s.log.Logf(logbuf.Info, "volumes: %s: exported, %d bytes", name, n)
	}
	return n, err
}

func (s *system) VolumeImportable(ctx context.Context, name string) error {
	_, err := s.inspectVolume(ctx, name)
	switch {
	case err == nil:
		return fmt.Errorf("the Docker engine has a volume named %s", name)
	case errors.Is(err, agent.ErrNoVolume):
		return nil
	}
	return err
}

func (s *system) VolumeImport(ctx context.Context, name string, r io.Reader, progress agent.Progress) (*api.VolumeInfo, error) {
	staged, err := s.stage()
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staged)
	if err := os.Mkdir(staged, 0o700); err != nil {
		return nil, err
	}
	progress(api.PhaseArchive, 0, 0)
	cr := &volumeCount{ctx: ctx, r: r, progress: progress}
	raw, warnings, n, err := storage.ImportVolume(staged, cr, machines.Unpack)
	if err != nil {
		return nil, err
	}
	var cfg api.VolumeArchiveConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("the archive's %s: %w", storage.ArchiveConfig, err)
	}
	if cfg.Kind != api.VolumeArchiveKind {
		return nil, errors.New("this is not a Docker volume's archive")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.newVolume(ctx, name, cfg.Labels, staged); err != nil {
		return nil, err
	}
	for i, w := range warnings {
		if i == 5 {
			break
		}
		s.log.Logf(logbuf.Warning, "volumes: %s: %s", name, w)
	}
	s.log.Logf(logbuf.Info, "volumes: %s: imported (exported as %s), %d entries", name, cfg.Name, n)
	return &api.VolumeInfo{Name: name, Labels: cfg.Labels, Files: n}, nil
}

type volumeCount struct {
	ctx      context.Context
	w        io.Writer
	r        io.Reader
	n        int64
	progress agent.Progress
}

func (c *volumeCount) Write(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.w.Write(b)
	c.n += int64(n)
	c.progress(api.PhaseArchive, c.n, 0)
	return n, err
}

func (c *volumeCount) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(b)
	c.n += int64(n)
	c.progress(api.PhaseArchive, c.n, 0)
	return n, err
}
