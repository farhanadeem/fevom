package debugremote

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const AgentEntrypoint = "/fev-debug-agent"

type AgentFiles struct {
	Files      map[string][]byte
	Executable []string
	Stamp      string
}

func FromDir(dir string) (*AgentFiles, error) {
	af := &AgentFiles{Files: map[string][]byte{}}
	want := map[string]string{
		"fev-debug-agent": "fev-debug-agent",
		"fev-debug-stage": "fev-debug-stage",
		"fev-debug":       "fev-debug",
		"defaultset.lock": "defaultset.lock",
	}
	exe := map[string]bool{"fev-debug-agent": true, "fev-debug-stage": true, "fev-debug": true}
	for src, inImage := range want {
		b, err := os.ReadFile(filepath.Join(dir, src))
		if err != nil {
			return nil, fmt.Errorf("debug agent files: %w", err)
		}
		af.Files[inImage] = b
		if exe[src] {
			af.Executable = append(af.Executable, inImage)
		}
	}
	sort.Strings(af.Executable)
	stamp, err := os.ReadFile(filepath.Join(dir, "stamp"))
	if err != nil {
		return nil, fmt.Errorf("debug agent files: %w", err)
	}
	af.Stamp = strings.TrimSpace(string(stamp))
	return af, nil
}

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func (af *AgentFiles) buildLayer() ([]byte, string) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dir := func(name string, mode int64) {
		tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: mode, ModTime: epoch})
	}
	for _, d := range []string{"etc", "etc/ssl", "etc/ssl/certs", "root", "proc", "sys", "dev", "nix"} {
		dir(d, 0o755)
	}
	dir("tmp", 0o1777)
	exe := map[string]bool{}
	for _, p := range af.Executable {
		exe[p] = true
	}
	var names []string
	for n := range af.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		body := af.Files[name]
		mode := int64(0o644)
		if exe[name] {
			mode = 0o755
		}
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(body)), ModTime: epoch})
		tw.Write(body)
	}
	tw.Close()
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), "sha256:" + hex.EncodeToString(sum[:])
}

func (af *AgentFiles) SaveTar(goarch, repo, tag string) ([]byte, error) {
	dockerArch := map[string]string{"arm64": "arm64", "amd64": "amd64"}[goarch]
	if dockerArch == "" {
		return nil, fmt.Errorf("debug helper: unknown architecture %q", goarch)
	}
	layer, diffID := af.buildLayer()

	config := map[string]any{
		"architecture": dockerArch,
		"os":           "linux",
		"config": map[string]any{
			"Entrypoint": []string{AgentEntrypoint},
			"Env":        []string{"PATH=/nix/profiles/default/bin:/bin"},
			"Labels":     map[string]string{Label: LabelHelper},
			"WorkingDir": "/",
		},
		"rootfs":  map[string]any{"type": "layers", "diff_ids": []string{diffID}},
		"created": epoch.Format(time.RFC3339),
		"history": []any{map[string]any{"created": epoch.Format(time.RFC3339), "created_by": "fev debug helper"}},
	}
	cfgBytes, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	cfgSum := sha256.Sum256(cfgBytes)
	cfgName := hex.EncodeToString(cfgSum[:]) + ".json"
	layerName := hex.EncodeToString([]byte(diffID[7 : 7+8]))
	layerPath := layerName + "/layer.tar"

	manifest := []map[string]any{{
		"Config":   cfgName,
		"RepoTags": []string{repo + ":" + tag},
		"Layers":   []string{layerPath},
	}}
	manBytes, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	write := func(name string, body []byte, mode int64) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(body)), ModTime: epoch}); err != nil {
			return err
		}
		_, err := tw.Write(body)
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: layerName + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: epoch}); err != nil {
		return nil, err
	}
	if err := write(layerPath, layer, 0o644); err != nil {
		return nil, err
	}
	if err := write(cfgName, cfgBytes, 0o644); err != nil {
		return nil, err
	}
	if err := write("manifest.json", manBytes, 0o644); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (e *Engine) EnsureHelper(ctx context.Context, af *AgentFiles, goarch, tag string) (loaded bool, ref string, err error) {
	ref = HelperRepo + ":" + tag
	if e.ImageExists(ctx, ref) {
		return false, ref, nil
	}
	tarBytes, err := af.SaveTar(goarch, HelperRepo, tag)
	if err != nil {
		return false, ref, err
	}
	if err := e.ImageLoad(ctx, bytes.NewReader(tarBytes)); err != nil {
		return false, ref, err
	}
	if olds, err := e.HelperImages(ctx); err == nil {
		for _, im := range olds {
			for _, t := range im.RepoTags {
				if t != ref && isHelperTag(t) {
					e.ImageRemove(ctx, t)
				}
			}
		}
	}
	return true, ref, nil
}

func isHelperTag(t string) bool {
	return len(t) > len(HelperRepo)+1 && t[:len(HelperRepo)+1] == HelperRepo+":"
}

var _ = io.Discard
