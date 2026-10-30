package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

const k8sFenceBound = 60 * time.Second

type FenceFunc func(ctx context.Context, bound time.Duration, waiting func(left []string)) (left []string, release func(), err error)

func notServed(err error) bool {
	var e *jsonrpc.Error
	return errors.As(err, &e) && (e.Code == agent.CodeNotImplemented || e.Code == jsonrpc.CodeMethodNotFound)
}

func transfers(left []string) string {
	count := map[string]int{}
	var order []string
	for _, k := range left {
		if count[k] == 0 {
			order = append(order, k)
		}
		count[k]++
	}
	var parts []string
	for _, k := range order {
		if n := count[k]; n > 1 {
			_, noun, _ := strings.Cut(k, " ")
			if s, ok := strings.CutSuffix(noun, "y"); ok {
				noun = s + "ie"
			}
			parts = append(parts, fmt.Sprintf("%d %ss", n, noun))
		} else {
			parts = append(parts, k)
		}
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

func (k *K8s) criRestart(op *Operation, client *agent.Client, on bool) (served bool, err error) {
	octx := op.Context()
	st, err := client.K8sStatus(octx)
	if err == nil && st.CRI == on {
		return true, nil
	}
	plugins := "with its CRI plugins"
	if !on {
		plugins = "without its CRI plugins"
	}
	fence := k.Fence
	if fence == nil {
		fence = func(context.Context, time.Duration, func([]string)) ([]string, func(), error) {
			return nil, func() {}, nil
		}
	}
	op.ProgressNote(api.K8sPhaseFencing, "")
	began := time.Now()
	left, release, err := fence(octx, k8sFenceBound, func(left []string) {
		op.ProgressNote(api.K8sPhaseFencing, "waiting for "+transfers(left)+" to end before containerd restarts "+plugins)
	})
	if err != nil {
		return true, fmt.Errorf("the Docker socket's fence: %w", err)
	}
	defer release()
	if len(left) > 0 {
		k.log.Warn("k8s: containerd restarts with Docker requests still in flight after the fence's minute; they may fail", "left", transfers(left))
		op.ProgressNote(api.K8sPhaseFencing, transfers(left)+" did not end within a minute: containerd restarts "+plugins+", and it may fail")
	}
	held := time.Now()
	_, err = client.K8sCRI(octx, on)
	if notServed(err) {
		k.log.Info("k8s: the guest has no k8s.cri: containerd's restart is the guest's own, unfenced")
		return false, nil
	}
	if err != nil {
		return true, guestError(err)
	}
	k.log.Info("k8s: containerd restarted inside the fence", "cri", on, "waited_ms", held.Sub(began).Milliseconds(), "held_ms", time.Since(held).Milliseconds())
	return true, nil
}
