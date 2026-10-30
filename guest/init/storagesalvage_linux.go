package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/internal/logbuf"
	"fev/guest/nfsview"
	"fev/guest/storage"
)

const fuseMagic = 0x65735546

func (s *system) StorageSalvage(ctx context.Context, p api.SalvageParams, progress func(string, int64, int64)) (api.Salvaged, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(shareMount, &st); err != nil || st.Type != fuseMagic {
		return api.Salvaged{}, errors.New("salvage writes to the Mac through the file share, and this VM has none")
	}
	req := storage.SalvageRequest{
		Dest:    filepath.Join(shareMount, filepath.Clean("/"+p.Dest)),
		Volumes: p.Volumes, Machines: p.Machines, Docker: p.Docker,
		Progress: progress,
	}
	select {
	case <-s.volDone:
	case <-s.ready:
	}
	s.mu.Lock()
	vol, device, tools := s.vol, s.dataDevice, s.dataTools
	s.mu.Unlock()
	if vol != nil {
		return vol.Salvage(ctx, req)
	}
	if device == "" {
		return api.Salvaged{}, errors.New(api.NoDataFilesystem + ": no data disk is attached")
	}
	return storage.Restore(ctx, storage.RestoreConfig{
		Device: device, ToolDir: tools,
		Start: func(path string, args []string, out *os.File) (func() error, func(), error) {
			null, err := os.Open(os.DevNull)
			if err != nil {
				return nil, nil, err
			}
			defer null.Close()
			child, err := s.reaper.Start(path, append([]string{path}, args...), &os.ProcAttr{
				Files: []*os.File{null, out, out}, Env: []string{"PATH=/system/bin"},
			})
			if err != nil {
				return nil, nil, err
			}
			wait := func() error {
				status := child.Wait()
				switch {
				case status.Signaled():
					return fmt.Errorf("%s: killed by signal %d", path, status.Signal())
				case status.ExitStatus() != 0:
					return fmt.Errorf("%s: exit status %d", path, status.ExitStatus())
				}
				return nil
			}
			kill := func() { s.reaper.Signal(child, unix.SIGKILL, false) }
			return wait, kill, nil
		},
		Logf: func(format string, args ...any) { s.log.Logf(logbuf.Info, format, args...) },
	}, req)
}

func (s *system) StorageUsage(engine bool) (*api.StorageUsage, error) {
	vol, err := s.volume()
	if err != nil {
		return nil, err
	}
	u, err := vol.Usage()
	if err != nil {
		return nil, err
	}
	if engine {
		if e, err := s.engineUsage(); err == nil {
			u.Engine = e
		} else {
			s.log.Logf(logbuf.Debug, "storage: usage: the engine's own account is not given: %v", err)
		}
		if u.Kubernetes != nil && s.engine.sup.Status().State == api.EngineReady {
			ctr := &nfsview.Containerd{Dial: dialUnix(engineContainerd), Namespace: "k8s.io", Snapshotter: "overlayfs"}
			if n, _, err := ctr.SnapshotsUsage(context.Background()); err == nil {
				u.Kubernetes.ImagesBytes, u.Kubernetes.ImagesCounted = n, true
				u.Kubernetes.Bytes += n
			} else {
				s.log.Logf(logbuf.Debug, "storage: usage: the cluster's images are not counted: %v", err)
			}
		}
	}
	return u, nil
}

const engineDFLimit = 2 * time.Minute

func (s *system) engineUsage() (*api.EngineUsage, error) {
	if s.engine.sup.Status().State != api.EngineReady {
		return nil, errors.New("the engine is not ready")
	}
	client := &http.Client{Timeout: engineDFLimit, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", engineSocket)
		},
	}}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://docker/system/df")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /system/df: %s", resp.Status)
	}
	var df struct {
		LayersSize int64
		Containers []struct{ SizeRw int64 }
		Volumes    []struct{ UsageData *struct{ Size int64 } }
		BuildCache []struct{ Size int64 }
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&df); err != nil {
		return nil, fmt.Errorf("GET /system/df: %w", err)
	}
	e := &api.EngineUsage{ImagesBytes: df.LayersSize}
	for _, c := range df.Containers {
		e.ContainersBytes += c.SizeRw
	}
	for _, v := range df.Volumes {
		if v.UsageData != nil && v.UsageData.Size > 0 {
			e.VolumesBytes += v.UsageData.Size
		}
	}
	for _, b := range df.BuildCache {
		e.BuildCacheBytes += b.Size
	}
	return e, nil
}
