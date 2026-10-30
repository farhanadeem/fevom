package machines

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/flosch/pongo2/v6"

	api "fev/proto/agent"
)

const (
	TriggerCreate = "create"
	TriggerCopy   = "copy"
)

var templateSet = sync.OnceValues(func() (*pongo2.TemplateSet, error) {
	set := pongo2.NewSet("fev-machine-templates", noFiles{})
	for _, tag := range []string{"extends", "import", "include", "ssi"} {
		if err := set.BanTag(tag); err != nil {
			return nil, err
		}
	}
	return set, nil
})

type noFiles struct{}

func (noFiles) Abs(base, name string) string { return name }
func (noFiles) Get(path string) (io.Reader, error) {
	return nil, fmt.Errorf("a machine's template reads no file (%s)", path)
}

func TemplatesFor(all []api.ImageTemplate, trigger string) []api.ImageTemplate {
	var out []api.ImageTemplate
	for _, t := range all {
		if slices.Contains(t.When, trigger) {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b api.ImageTemplate) int { return strings.Compare(a.Path, b.Path) })
	return out
}

type TemplateContext struct {
	Name   string
	Arch   string
	Config map[string]string
}

func RenderTemplate(t api.ImageTemplate, trigger string, ctx TemplateContext) (string, error) {
	set, err := templateSet()
	if err != nil {
		return "", err
	}
	tpl, err := set.FromString("{% autoescape off %}" + t.Template + "{% endautoescape %}")
	if err != nil {
		return "", fmt.Errorf("the template of %s: %w", t.Path, err)
	}
	config := ctx.Config
	if config == nil {
		config = map[string]string{}
	}
	meta := map[string]string{
		"name": ctx.Name, "type": "container", "architecture": kernelArch(ctx.Arch),
		"ephemeral":  "false",
		"privileged": "true",
	}
	properties := t.Properties
	if properties == nil {
		properties = map[string]string{}
	}
	configGet := func(key, def *pongo2.Value) *pongo2.Value {
		v, ok := config[key.String()]
		if !ok {
			return def
		}
		return pongo2.AsValue(strings.TrimRight(v, "\r\n"))
	}
	out, err := tpl.Execute(pongo2.Context{
		"trigger": trigger, "path": t.Path,
		"container": meta, "instance": meta,
		"config": config, "devices": map[string]map[string]string{},
		"properties": properties, "config_get": configGet,
	})
	if err != nil {
		return "", fmt.Errorf("the template of %s: %w", t.Path, err)
	}
	return out, nil
}

func kernelArch(arch string) string {
	if arch == "amd64" {
		return "x86_64"
	}
	return "aarch64"
}
