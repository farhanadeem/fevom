package attach

import (
	"fmt"
	"strings"
)

type GraphDriver struct {
	Name string            `json:"Name"`
	Data map[string]string `json:"Data"`
}

func OverlayLowers(gd GraphDriver) ([]string, error) {
	if gd.Name != "" && gd.Name != "overlay2" && gd.Name != "overlay" {
		return nil, fmt.Errorf("debug: a stopped container on the %q storage driver is not supported", gd.Name)
	}
	upper := gd.Data["UpperDir"]
	lower := gd.Data["LowerDir"]
	if upper == "" && lower == "" {
		return nil, fmt.Errorf("debug: the container's inspect carries no overlay layer paths")
	}
	var lowers []string
	if upper != "" {
		lowers = append(lowers, upper)
	}
	for _, l := range strings.Split(lower, ":") {
		if l = strings.TrimSpace(l); l != "" {
			lowers = append(lowers, l)
		}
	}
	return lowers, nil
}

func ContainerUpper(gd GraphDriver) string {
	if gd.Data == nil {
		return ""
	}
	return gd.Data["UpperDir"]
}

func GraphFromMount(typ, source string, options []string) (GraphDriver, error) {
	switch typ {
	case "overlay", "overlay2":
		gd := GraphDriver{Name: "overlay2", Data: map[string]string{}}
		for _, o := range options {
			if v, ok := strings.CutPrefix(o, "lowerdir="); ok {
				gd.Data["LowerDir"] = v
			}
			if v, ok := strings.CutPrefix(o, "upperdir="); ok {
				gd.Data["UpperDir"] = v
			}
			if v, ok := strings.CutPrefix(o, "workdir="); ok {
				gd.Data["WorkDir"] = v
			}
		}
		if gd.Data["LowerDir"] == "" && gd.Data["UpperDir"] == "" {
			return GraphDriver{}, fmt.Errorf("debug: the snapshot's overlay mount names no layers")
		}
		return gd, nil
	case "bind", "rbind":
		if source == "" {
			return GraphDriver{}, fmt.Errorf("debug: the snapshot's bind mount names no source")
		}
		return GraphDriver{Name: "overlay2", Data: map[string]string{"LowerDir": source}}, nil
	default:
		return GraphDriver{}, fmt.Errorf("debug: the snapshot is a %q mount, which debug does not support", typ)
	}
}
