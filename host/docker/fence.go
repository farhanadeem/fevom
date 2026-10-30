package docker

import (
	"context"
	"errors"
	"time"
)

func (m *Module) Fence(ctx context.Context, bound time.Duration, waiting func(left []string)) ([]string, func(), error) {
	if m.proxy == nil {
		return nil, func() {}, errors.New("the Docker socket is not open")
	}
	f, release, err := m.proxy.Fence(ctx, bound, waiting)
	if err != nil {
		return nil, func() {}, err
	}
	if f.Waited > 0 {
		m.log.Info("docker socket: fenced for containerd's restart", "waited_ms", f.Waited.Milliseconds(), "left", len(f.Left))
	}
	return f.Left, release, nil
}

func (m *Module) Freed() { m.removed() }
