package images

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/ulikunitz/xz"
	"go.yaml.in/yaml/v3"

	"fev/proto/agent"
)

const (
	maxMeta   = 4 << 20
	maxMetaXZ = 1 << 20
)

type metadata struct {
	Architecture string              `yaml:"architecture"`
	Properties   map[string]string   `yaml:"properties"`
	Templates    map[string]template `yaml:"templates"`
}

type template struct {
	When       []string          `yaml:"when"`
	CreateOnly bool              `yaml:"create_only"`
	Template   string            `yaml:"template"`
	Properties map[string]string `yaml:"properties"`
	UID        string            `yaml:"uid"`
	GID        string            `yaml:"gid"`
	Mode       string            `yaml:"mode"`
}

func readTemplates(data []byte, ref agent.ImageRef) ([]agent.ImageTemplate, error) {
	zr, err := xz.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("incus.tar.xz: %w", err)
	}
	files := map[string][]byte{}
	links := map[string]bool{}
	tr := tar.NewReader(io.LimitReader(zr, maxMeta+1))
	total := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("incus.tar.xz: %w", err)
		}
		name := strings.TrimPrefix(path.Clean("/"+h.Name), "/")
		switch h.Typeflag {
		case tar.TypeReg:
			total += int(h.Size)
			if total > maxMeta {
				return nil, fmt.Errorf("incus.tar.xz: more than %d bytes", maxMeta)
			}
			b, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("incus.tar.xz: %s: %w", name, err)
			}
			files[name] = b
		case tar.TypeSymlink, tar.TypeLink:
			links[name] = true
		}
	}
	raw, ok := files["metadata.yaml"]
	if !ok {
		return nil, fmt.Errorf("incus.tar.xz has no metadata.yaml")
	}
	var meta metadata
	if err := yaml.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("incus.tar.xz: metadata.yaml: %w", err)
	}
	p := meta.Properties
	release := p["release"] == "" || p["release"] == ref.Release || p["release"] == metaReleases[ref.Distro][ref.Release]
	if p["serial"] != ref.Serial || p["variant"] != ref.Variant || !release {
		return nil, fmt.Errorf("incus.tar.xz describes the build %q %q %q, not the one fetched (%s %s, serial %s)", p["release"], p["variant"], p["serial"], ref.Release, ref.Variant, ref.Serial)
	}
	sameArch := func(a string) bool { return a == ref.Arch || a == KernelArch(ref.Arch) }
	if !sameArch(meta.Architecture) || (p["architecture"] != "" && !sameArch(p["architecture"])) {
		return nil, fmt.Errorf("incus.tar.xz describes a build for %q (%q), not for %s", meta.Architecture, p["architecture"], ref.Arch)
	}
	out := make([]agent.ImageTemplate, 0, len(meta.Templates))
	for target, t := range meta.Templates {
		if !path.IsAbs(target) || path.Clean(target) != target || target == "/" {
			return nil, fmt.Errorf("incus.tar.xz: a template's target %q is not a clean absolute path", target)
		}
		if t.Template == "" || strings.Contains(t.Template, "/") {
			return nil, fmt.Errorf("incus.tar.xz: the template %q of %s is not a file name in templates/", t.Template, target)
		}
		if t.UID != "" || t.GID != "" || t.Mode != "" {
			return nil, fmt.Errorf("incus.tar.xz: the template of %s sets an owner or a mode, which fev does not apply", target)
		}
		name := "templates/" + t.Template
		if links[name] {
			return nil, fmt.Errorf("incus.tar.xz: the template %s is a link", name)
		}
		text, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("incus.tar.xz: the template %s of %s is not there", name, target)
		}
		out = append(out, agent.ImageTemplate{Path: target, When: t.When, CreateOnly: t.CreateOnly, Template: string(text), Properties: nonEmpty(t.Properties)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func nonEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}
