package debugremote

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

type Kind string

const (
	KindRunning Kind = "running"
	KindStopped Kind = "stopped"
	KindImage   Kind = "image"
)

type Resolved struct {
	Kind    Kind
	ID      string
	Name    string
	Image   string
	Env     []string
	User    string
	WorkDir string
	Pid     int
	HostPID bool
	Limits  Limits
}

func (e *Engine) Resolve(ctx context.Context, target, requestKind string) (*Resolved, error) {
	tryContainer := requestKind == "" || requestKind == "container"
	tryImage := requestKind == "" || requestKind == "image"
	if tryContainer {
		if c, err := e.ContainerInspect(ctx, target); err == nil {
			r := &Resolved{ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), Image: c.Config.Image,
				Env: c.Config.Env, User: c.Config.User, WorkDir: c.Config.WorkingDir, Pid: c.State.Pid,
				HostPID: c.HostConfig.PidMode == "host", Limits: c.HostConfig.Limits}
			switch {
			case c.State.Paused:
				return nil, fmt.Errorf("container %q is paused; `docker unpause` it first", target)
			case c.State.Running:
				r.Kind = KindRunning
			default:
				r.Kind = KindStopped
			}
			return r, nil
		} else if !NotFound(err) && requestKind == "container" {
			return nil, err
		}
	}
	if tryImage {
		if im, err := e.ImageInspect(ctx, target); err == nil {
			name := target
			if len(im.RepoTags) > 0 && im.RepoTags[0] != "<none>:<none>" {
				name = im.RepoTags[0]
			}
			return &Resolved{Kind: KindImage, ID: im.ID, Name: name,
				Env: im.Config.Env, User: im.Config.User, WorkDir: im.Config.WorkingDir}, nil
		}
	}
	return nil, fmt.Errorf("no container or image %q here (docker pull %s first)", target, target)
}

func shellEnv(target []string, termEnv []string) []string {
	out := append([]string{}, target...)
	set := func(kv string) {
		k := kv[:strings.IndexByte(kv, '=')+1]
		for i, e := range out {
			if strings.HasPrefix(e, k) {
				out[i] = kv
				return
			}
		}
		out = append(out, kv)
	}
	profile := "/nix/profiles/default/bin"
	path := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, e := range out {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			path = v
		}
	}
	set("PATH=" + profile + ":" + path)
	set("LANG=C.UTF-8")
	set("ZDOTDIR=/nix/etc")
	set("FEV_PROFILE=/nix/profiles/default")
	set("TERMINFO_DIRS=/nix/etc/terminfo:/nix/profiles/default/share/terminfo:")
	set("SSL_CERT_FILE=/nix/etc/ssl/ca-bundle.crt")
	set("NIX_SSL_CERT_FILE=/nix/etc/ssl/ca-bundle.crt")
	set("EDITOR=vim")
	set("PAGER=less")
	for _, e := range termEnv {
		if strings.HasPrefix(e, "TERM=") || strings.HasPrefix(e, "COLORTERM=") || strings.HasPrefix(e, "LANG=") {
			set(e)
		}
	}
	return out
}

const storeShell = "/nix/profiles/default/bin/zsh"

type RunSpec struct {
	Resolved  *Resolved
	HelperRef string
	Store     string
	ReadOnly  bool
	Argv      []string
	Dir       string
	TermEnv   []string
	TTY       bool
	Stdin     bool
}

func (s *RunSpec) runningCreate(admin bool) CreateConfig {
	ep := []string{AgentEntrypoint}
	if admin {
		pid := 1
		if s.Resolved.HostPID {
			pid = s.Resolved.Pid
		}
		ep = append(ep, "-B", "-C", "-p", strconv.Itoa(pid))
	} else {
		ep = append(ep, "-A", "-C")
	}
	if s.Dir != "" {
		ep = append(ep, "-w", s.Dir)
	}
	if len(s.Argv) > 0 {
		ep = append(ep, "--")
		ep = append(ep, s.Argv...)
	}
	hc := HostConfig{
		AutoRemove:  true,
		PidMode:     "container:" + s.Resolved.ID,
		NetworkMode: "container:" + s.Resolved.ID,
		Binds:       []string{s.Store + ":/nix:ro"},
		CapAdd:      []string{"SYS_PTRACE"},
		Limits:      s.Resolved.Limits,
	}
	if admin {
		hc.CapAdd = []string{"SYS_ADMIN", "SYS_PTRACE"}
		hc.SecurityOpt = []string{"seccomp=unconfined"}
	}
	return CreateConfig{
		Image:      s.HelperRef,
		Entrypoint: ep,
		Tty:        s.TTY,
		OpenStdin:  s.Stdin,
		StdinOnce:  s.Stdin,
		Labels:     map[string]string{Label: LabelSession},
		HostConfig: hc,
	}
}

func (s *RunSpec) imageCreate(image string) CreateConfig {
	dir := s.Dir
	if dir == "" {
		dir = s.Resolved.WorkDir
	}
	ep := []string{storeShell}
	if len(s.Argv) > 0 {
		ep = s.Argv
	}
	return CreateConfig{
		Image:      image,
		Entrypoint: ep,
		Env:        shellEnv(s.Resolved.Env, s.TermEnv),
		WorkingDir: dir,
		Tty:        s.TTY,
		OpenStdin:  s.Stdin,
		StdinOnce:  s.Stdin,
		Labels:     map[string]string{Label: LabelSession},
		HostConfig: HostConfig{
			AutoRemove:  true,
			Binds:       []string{s.Store + ":/nix:ro"},
			CapAdd:      []string{"SYS_PTRACE"},
			CapDrop:     []string{"MKNOD", "SYS_ADMIN"},
			SecurityOpt: []string{"no-new-privileges"},
		},
	}
}

type sessionIO struct {
	In     io.Reader
	Out    io.Writer
	Err    io.Writer
	Resize <-chan [2]int
}

func (e *Engine) runInteractive(ctx context.Context, name string, cfg CreateConfig, sio sessionIO) (int, error) {
	id, err := e.ContainerCreate(ctx, name, cfg)
	if err != nil {
		return -1, err
	}
	conn, br, err := e.Attach(ctx, id, cfg.OpenStdin)
	if err != nil {
		e.ContainerRemove(ctx, id)
		return -1, err
	}
	if err := e.ContainerStart(ctx, id); err != nil {
		conn.Close()
		e.ContainerRemove(ctx, id)
		return -1, err
	}
	done := make(chan struct{})
	go func() {
		if cfg.OpenStdin && sio.In != nil {
			io.Copy(conn, sio.In)
			if cw, ok := conn.(interface{ CloseWrite() error }); ok {
				cw.CloseWrite()
			}
		}
	}()
	go func() {
		defer close(done)
		if cfg.Tty {
			io.Copy(outOr(sio.Out), br)
		} else {
			demux(sio.Out, sio.Err, br)
		}
	}()
	if sio.Resize != nil {
		go e.watchResize(ctx, id, sio.Resize)
	}
	<-done
	conn.Close()
	code, _ := e.ContainerWait(ctx, id)
	e.ContainerRemove(ctx, id)
	return code, nil
}

func outOr(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

func (e *Engine) watchResize(ctx context.Context, id string, ch <-chan [2]int) {
	for {
		select {
		case <-ctx.Done():
			return
		case sz, ok := <-ch:
			if !ok {
				return
			}
			q := url.Values{"h": {fmt.Sprint(sz[0])}, "w": {fmt.Sprint(sz[1])}}
			resp, err := e.do(ctx, "POST", "/containers/"+id+"/resize", q, nil, "")
			if err == nil {
				resp.Body.Close()
			}
		}
	}
}

func demux(stdout, stderr io.Writer, r io.Reader) {
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(hdr[4:])
		w := stdout
		if hdr[0] == 2 {
			w = stderr
		}
		if w == nil {
			w = io.Discard
		}
		if _, err := io.CopyN(w, r, int64(n)); err != nil {
			return
		}
	}
}
