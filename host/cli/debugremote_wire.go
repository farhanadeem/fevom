package cli

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"fev/host/bundle"
	"fev/host/daemon/config"
	"fev/host/debugremote"
	"fev/proto/debugstore"
	"fev/proto/version"

	api "fev/proto/daemon"
)

func init() {
	newDebugRemote = func(ctx context.Context, a *app, name string) (debugRemote, error) {
		l, err := a.layout()
		if err != nil {
			return nil, err
		}
		docker := filepath.Join(toolsDir(l), "docker")
		if _, err := os.Stat(docker); err != nil {
			return nil, failf(ExitFailed, "a remote context needs the docker CLI (%s); run \"fev setup\"", docker)
		}
		agents := os.Getenv("FEV_DEBUG_AGENTS")
		if agents == "" {
			agents = bundle.DebugAgent(bundle.Self())
		}
		if agents == "" {
			return nil, failf(ExitFailed, "this build has no remote debug helper (Resources/guest/debug-agent)")
		}
		cache := ""
		if st, err := config.Load(l.ConfigFile(), config.Keys(config.ThisMac()), nil); err == nil {
			if v, err := st.Get(api.KeyDebugCacheURL); err == nil {
				if s, ok := v[api.KeyDebugCacheURL].Value.(string); ok && s != debugstore.CacheURL {
					cache = s
				}
			}
		}
		return &remote{a: a, opts: debugremote.Options{
			Ctx:       debugremote.Context{DockerCLI: docker, Config: userDockerConfig(), Name: name},
			AgentDir:  agents,
			BuildID:   version.Build,
			NoticeDir: filepath.Join(l.Home, "config"),
			CacheURL:  cache,
			ForceArch: os.Getenv("FEV_DEBUG_ARCH"),
		}}, nil
	}
}

func userDockerConfig() string {
	if d := os.Getenv("DOCKER_CONFIG"); d != "" {
		return d
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".docker")
	}
	return ""
}

type remote struct {
	a    *app
	opts debugremote.Options
}

func (r *remote) Debug(ctx context.Context, p remoteDebug) (int, error) {
	o := r.opts
	o.Target, o.ReadOnly, o.Fallback, o.Dir, o.Argv = p.Target, p.ReadOnly, p.Fallback, p.Dir, p.Command
	o.In, o.Out, o.Err = p.In, p.Out, p.Err
	o.TTY = terminal(r.a.Stdin) && terminal(r.a.Stdout)
	o.Stdin = o.TTY || !terminal(r.a.Stdin)
	o.TermEnv = sessionEnv(os.Getenv, o.TTY)
	if p.Reset {
		if _, err := o.Reset(ctx); err != nil {
			return 0, err
		}
	}
	if o.TTY {
		in, ok := fileOf(r.a.Stdin)
		if ok {
			restore, err := rawMode(in)
			if err == nil {
				defer restore()
			}
			sizes := make(chan [2]int, 4)
			if rows, cols := termSize(in); rows > 0 {
				sizes <- [2]int{rows, cols}
			}
			sigs := make(chan os.Signal, 4)
			signal.Notify(sigs, syscall.SIGWINCH)
			defer signal.Stop(sigs)
			done := make(chan struct{})
			defer close(done)
			go func() {
				for {
					select {
					case <-sigs:
						if rows, cols := termSize(in); rows > 0 {
							select {
							case sizes <- [2]int{rows, cols}:
							default:
							}
						}
					case <-done:
						return
					}
				}
			}()
			o.Resize = sizes
		}
	}
	code, err := o.Run(ctx)
	if err != nil {
		var ee *exit
		if errors.As(err, &ee) {
			return 0, err
		}
		return 0, failf(ExitFailed, "%v", err)
	}
	return code, nil
}

func (r *remote) Reset(ctx context.Context) (int64, error) {
	o := r.opts
	res, err := o.Reset(ctx)
	if err != nil {
		return 0, failf(ExitFailed, "%v", err)
	}
	if !res.StoreRemoved && res.HelperImages == 0 {
		return 0, nil
	}
	return -1, nil
}

func (r *remote) Close() error { return nil }
