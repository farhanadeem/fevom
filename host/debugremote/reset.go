package debugremote

import (
	"context"
	"fmt"
)

type ResetResult struct {
	StoreRemoved bool
	HelperImages int
	TempImages   int
}

func (o *Options) Reset(ctx context.Context) (ResetResult, error) {
	var r ResetResult
	t := o.transport()
	if err := t.probe(ctx); err != nil {
		return r, fmt.Errorf("reaching context %q: %w", o.Ctx.Name, err)
	}
	eng := NewEngine(t)

	imgs, err := eng.HelperImages(ctx)
	if err == nil {
		for _, im := range imgs {
			for _, tag := range im.RepoTags {
				if isHelperTag(tag) {
					if eng.ImageRemove(ctx, tag) == nil {
						r.HelperImages++
					}
				}
			}
		}
	}

	vols, _ := eng.StoreVolumes(ctx)
	if eng.VolumeExists(ctx, StoreVolume) {
		vols = append(vols, StoreVolume)
	}
	seen := map[string]bool{}
	for _, v := range vols {
		if seen[v] {
			continue
		}
		seen[v] = true
		if err := eng.VolumeRemove(ctx, v); err != nil {
			return r, fmt.Errorf("removing the store volume %s: %w", v, err)
		}
		r.StoreRemoved = true
	}

	name := o.Ctx.Name
	if name == "" {
		name = "default"
	}
	o.forgetContext(name)
	return r, nil
}
