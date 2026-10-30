package debugremote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
)

type Options struct {
	Ctx Context

	Target   string
	Kind     string
	ReadOnly bool
	Argv     []string
	Dir      string
	TermEnv  []string
	TTY      bool
	Stdin    bool

	AgentDir  string
	ForceArch string
	BuildID   string

	NoticeDir string

	CacheURL string

	Fallback bool

	In     io.Reader
	Out    io.Writer
	Err    io.Writer
	Resize <-chan [2]int

	dial func(ctx context.Context) (net.Conn, error)
}

func (o *Options) transport() *Transport {
	return &Transport{Ctx: o.Ctx, DialFunc: o.dial}
}

func (o *Options) errf(format string, a ...any) {
	if o.Err != nil {
		fmt.Fprintf(o.Err, "fev debug: "+format+"\n", a...)
	}
}

func (o *Options) Run(ctx context.Context) (int, error) {
	t := o.transport()
	if err := t.probe(ctx); err != nil {
		if o.Fallback {
			o.errf("cannot reach the engine (%v); falling back to docker exec", err)
			return o.fallbackExec(ctx)
		}
		return 2, fmt.Errorf("reaching context %q: %w", o.Ctx.Name, err)
	}
	eng := NewEngine(t)

	info, err := eng.Info(ctx)
	if err != nil {
		return 2, fmt.Errorf("engine info: %w", err)
	}
	goarch := info.GoArch()
	if o.ForceArch != "" {
		if o.ForceArch != "arm64" && o.ForceArch != "amd64" {
			return 2, fmt.Errorf("FEV_DEBUG_ARCH=%q: arm64 or amd64", o.ForceArch)
		}
		goarch = o.ForceArch
	}
	if goarch == "" {
		return 2, fmt.Errorf("engine architecture %q is not supported", info.Architecture)
	}

	res, err := eng.Resolve(ctx, o.Target, o.Kind)
	if err != nil {
		return 1, err
	}

	o.notice(info)

	af, err := FromDir(filepath.Join(o.AgentDir, goarch))
	if err != nil {
		return o.setupFail(ctx, err)
	}
	if want := lockArch(goarch); !strings.Contains(firstLines(af.Files["defaultset.lock"], 3), "("+want+")") {
		return 2, fmt.Errorf("the debug tools for this %s engine are not in this build (its default set is not %s's)", info.Architecture, want)
	}
	if _, _, err := eng.EnsureHelper(ctx, af, goarch, o.BuildID); err != nil {
		return o.setupFail(ctx, fmt.Errorf("loading the debug helper: %w", err))
	}
	store := StoreVolumeFor(af.Stamp)
	created, err := eng.VolumeCreate(ctx, store)
	if err != nil {
		return o.setupFail(ctx, fmt.Errorf("the store volume: %w", err))
	}

	sio := sessionIO{In: o.In, Out: o.Out, Err: o.Err, Resize: o.Resize}
	ref := HelperRepo + ":" + o.BuildID
	spec := &RunSpec{Resolved: res, HelperRef: ref, Store: store, ReadOnly: o.ReadOnly,
		Argv: o.Argv, Dir: o.Dir, TermEnv: o.TermEnv, TTY: o.TTY, Stdin: o.Stdin}

	if created {
		if err := eng.fillStore(ctx, ref, store, o.CacheURL); err != nil {
			eng.VolumeRemove(ctx, store)
			return o.setupFail(ctx, fmt.Errorf("fetching the debug tools: %w", err))
		}
		if old, err := eng.StoreVolumes(ctx); err == nil {
			for _, v := range old {
				if v != store {
					eng.VolumeRemove(ctx, v)
				}
			}
		}
	}

	switch res.Kind {
	case KindRunning:
		return o.runRunning(ctx, eng, spec, af)
	case KindImage:
		return eng.runInteractive(ctx, "", spec.imageCreate(res.ID), sio)
	case KindStopped:
		return o.runStopped(ctx, eng, spec)
	}
	return 2, fmt.Errorf("unknown target kind %q", res.Kind)
}

func (o *Options) runRunning(ctx context.Context, eng *Engine, spec *RunSpec, af *AgentFiles) (int, error) {
	sio := sessionIO{In: o.In, Out: o.Out, Err: o.Err, Resize: o.Resize}
	code, err := eng.runInteractive(ctx, "", spec.runningCreate(true), sio)
	if err != nil && refusedAdmin(err) {
		o.errf("the engine refuses CAP_SYS_ADMIN or an unconfined helper; using the tools-at-/ layout, whose shell holds the helper's default seccomp filter, not the target's own")
		return eng.runInteractive(ctx, "", spec.runningCreate(false), sio)
	}
	if err != nil {
		return o.setupFail(ctx, err)
	}
	return code, nil
}

func (o *Options) runStopped(ctx context.Context, eng *Engine, spec *RunSpec) (int, error) {
	o.errf("a stopped container is read-only on a remote engine: edits are not kept (run it on your own fev to keep them)")
	tag := "fev-debug-commit-" + shortID(spec.Resolved.ID)
	tmpRef, err := eng.ContainerCommit(ctx, spec.Resolved.ID, HelperRepo, tag)
	if err != nil {
		return o.setupFail(ctx, fmt.Errorf("committing the stopped container: %w", err))
	}
	defer eng.ImageRemove(ctx, tmpRef)
	sio := sessionIO{In: o.In, Out: o.Out, Err: o.Err, Resize: o.Resize}
	return eng.runInteractive(ctx, "", spec.imageCreate(tmpRef), sio)
}

func (e *Engine) fillStore(ctx context.Context, helperRef, store, cacheURL string) error {
	cfg := CreateConfig{
		Image:      helperRef,
		Entrypoint: []string{AgentEntrypoint, "fetch"},
		Labels:     map[string]string{Label: LabelSession},
		HostConfig: HostConfig{AutoRemove: true, Binds: []string{store + ":/nix"}},
	}
	if cacheURL != "" {
		cfg.Env = []string{"FEV_DEBUG_CACHE=" + cacheURL}
	}
	id, err := e.ContainerCreate(ctx, "", cfg)
	if err != nil {
		return err
	}
	if err := e.ContainerStart(ctx, id); err != nil {
		e.ContainerRemove(ctx, id)
		return err
	}
	code, err := e.ContainerWait(ctx, id)
	e.ContainerRemove(ctx, id)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("the store fetch exited %d", code)
	}
	return nil
}

func (o *Options) setupFail(ctx context.Context, cause error) (int, error) {
	if o.Fallback {
		o.errf("%v; falling back to docker exec", cause)
		return o.fallbackExec(ctx)
	}
	return 2, cause
}

func refusedAdmin(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "sys_admin") || strings.Contains(s, "capability") || strings.Contains(s, "not permitted") ||
		strings.Contains(s, "operation not permitted") || strings.Contains(s, "seccomp")
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

var errNoFallbackTarget = errors.New("no fallback target")

func lockArch(goarch string) string {
	if goarch == "amd64" {
		return "x86_64-linux"
	}
	return "aarch64-linux"
}

func firstLines(b []byte, n int) string {
	s := string(b)
	for i, k := 0, 0; i < len(s); i++ {
		if s[i] == '\n' {
			if k++; k == n {
				return s[:i]
			}
		}
	}
	return s
}
