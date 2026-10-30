package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fev/host/debugremote"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/version"
)

const remoteListBound = 20 * time.Second

func (m *Machines) remoteConfigured() bool { return m.DebugDockerConfig != "" }

func (m *Machines) dockerEnv() []string {
	env := os.Environ()
	if m.DebugDockerConfig != "" {
		env = append(env, "DOCKER_CONFIG="+m.DebugDockerConfig)
	}
	return env
}

func (m *Machines) ownEngine(host string) bool {
	if m.DebugOwnDockerHost == "" {
		return false
	}
	p, ok := strings.CutPrefix(host, "unix://")
	if !ok {
		return false
	}
	own := m.DebugOwnDockerHost
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if r, err := filepath.EvalSymlinks(own); err == nil {
		own = r
	}
	return p == own
}

func (m *Machines) remoteContext(name string) (debugremote.Context, error) {
	if !m.remoteConfigured() {
		return debugremote.Context{}, jsonrpc.Errorf(api.CodeFailed, "this daemon has no access to Docker contexts")
	}
	info, ok := debugremote.Lookup(m.DebugDockerConfig, m.DebugDefaultHost, name)
	if !ok {
		return debugremote.Context{}, jsonrpc.Errorf(api.CodeFailed, "no Docker context %q", name)
	}
	if m.ownEngine(info.Host) {
		return debugremote.Context{}, jsonrpc.Errorf(api.CodeFailed, "context %q is fev's own engine; debug its containers in the workspace, not through a remote context", name)
	}
	return debugremote.Context{
		DockerCLI: m.DebugDockerCLI, Config: m.DebugDockerConfig, Name: info.Name, Host: info.Host,
	}, nil
}

func (m *Machines) remoteEngine(name string) (*debugremote.Engine, error) {
	c, err := m.remoteContext(name)
	if err != nil {
		return nil, err
	}
	return debugremote.NewEngine(&debugremote.Transport{Ctx: c, Env: m.dockerEnv()}), nil
}

func (m *Machines) contextsList(_ context.Context, _ json.RawMessage) (any, error) {
	out := api.RemoteContexts{Contexts: []api.RemoteContext{}}
	if !m.remoteConfigured() {
		return out, nil
	}
	list, err := debugremote.Contexts(m.DebugDockerConfig, m.DebugDefaultHost)
	if err != nil {
		return nil, jsonrpc.Errorf(api.CodeFailed, "reading the Docker contexts: %v", oneLine(err.Error()))
	}
	for _, c := range list {
		if m.ownEngine(c.Host) {
			continue
		}
		out.Contexts = append(out.Contexts, api.RemoteContext{
			Name: c.Name, Host: c.Host, Current: c.Current, Description: c.Description,
		})
	}
	return out, nil
}

func (m *Machines) contextContainers(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.ContextRef](params)
	if err != nil {
		return nil, err
	}
	eng, err := m.remoteEngine(p.Context)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, remoteListBound)
	defer cancel()
	cs, err := eng.ContainerList(cctx, true)
	if err != nil {
		return nil, jsonrpc.Errorf(api.CodeFailed, "reaching context %q: %v", p.Context, oneLine(err.Error()))
	}
	out := api.RemoteContainers{Containers: []api.RemoteContainer{}}
	for _, c := range cs {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out.Containers = append(out.Containers, api.RemoteContainer{
			ID: c.ID, Name: name, Image: c.Image, State: c.State, Status: c.Status,
		})
	}
	return out, nil
}

func (m *Machines) contextImages(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.ContextRef](params)
	if err != nil {
		return nil, err
	}
	eng, err := m.remoteEngine(p.Context)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, remoteListBound)
	defer cancel()
	ims, err := eng.ImageList(cctx)
	if err != nil {
		return nil, jsonrpc.Errorf(api.CodeFailed, "reaching context %q: %v", p.Context, oneLine(err.Error()))
	}
	out := api.RemoteImages{Images: []api.RemoteImage{}}
	for _, im := range ims {
		var tags []string
		for _, t := range im.RepoTags {
			if t != "<none>:<none>" {
				tags = append(tags, t)
			}
		}
		out.Images = append(out.Images, api.RemoteImage{
			ID: im.ID, RepoTags: tags, Size: im.Size, Created: im.Created,
		})
	}
	return out, nil
}

func (t *sessionTable) startRemoteDebug(ctx context.Context, caller Caller, p api.DebugStartParams) (any, error) {
	m := t.m
	rc, err := m.remoteContext(p.Context)
	if err != nil {
		return nil, err
	}
	if m.DebugAgentDir == "" {
		return nil, jsonrpc.Errorf(api.CodeFailed, "this build has no remote debug helper; a remote Debug Shell is not available")
	}
	daemonEnd, appEnd, err := socketPair()
	if err != nil {
		return nil, jsonrpc.Errorf(api.CodeFailed, "the session's stream: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	resize := make(chan [2]int, 4)
	t.mu.Lock()
	t.nextID++
	s := &session{id: t.nextID, caller: caller, over: make(chan struct{}), debug: true,
		remote: &remoteRun{appEnd: appEnd, resize: resize, cancel: cancel}}
	t.byID[s.id] = s
	t.mu.Unlock()
	token := newSessionToken()
	t.armRemote(s, token)
	if p.Rows > 0 && p.Cols > 0 {
		resize <- [2]int{p.Rows, p.Cols}
	}
	opts := debugremote.Options{
		Ctx:      rc,
		Target:   p.Target,
		Kind:     p.Kind,
		ReadOnly: p.ReadOnly,
		Argv:     p.Argv,
		Dir:      p.Dir,
		TermEnv:  p.Env,
		TTY:      p.TTY,
		Stdin:    true,
		AgentDir: m.DebugAgentDir,
		BuildID:  version.Build,
		CacheURL: m.debugCacheURL(),
		In:       daemonEnd,
		Out:      daemonEnd,
		Err:      daemonEnd,
		Resize:   resize,
	}
	go func() {
		code, rerr := opts.Run(runCtx)
		daemonEnd.Close()
		cancel()
		t.forget(s)
		if rerr != nil {
			s.exit(api.SessionExit{ExitCode: -1, Error: oneLine(rerr.Error())})
			return
		}
		s.exit(api.SessionExit{ExitCode: code, StreamEnded: true})
	}()
	return api.DebugStarted{
		ID: s.id, Token: token, Kind: "remote", Name: p.Target, ReadOnly: p.ReadOnly,
		Notes: []string{"on " + p.Context + " (its own engine); setup messages follow in the shell"},
	}, nil
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}
