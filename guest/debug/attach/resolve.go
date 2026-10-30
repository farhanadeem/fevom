package attach

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const apiTimeout = 10 * time.Second

type Resolved struct {
	Kind           string
	ID             string
	Name           string
	Image          string
	Pid            int
	Paused         bool
	Env            []string
	Cwd            string
	Caps           uint64
	HasCaps        bool
	Graph          GraphDriver
	DiffIDs        []string
	Seccomp        string
	SeccompProfile []byte
}

type inspectContainer struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Running bool `json:"Running"`
		Paused  bool `json:"Paused"`
		Pid     int  `json:"Pid"`
	} `json:"State"`
	Config struct {
		Env        []string `json:"Env"`
		WorkingDir string   `json:"WorkingDir"`
		Image      string   `json:"Image"`
	} `json:"Config"`
	Image      string `json:"Image"`
	HostConfig struct {
		CapAdd      []string `json:"CapAdd"`
		CapDrop     []string `json:"CapDrop"`
		Privileged  bool     `json:"Privileged"`
		SecurityOpt []string `json:"SecurityOpt"`
	} `json:"HostConfig"`
	GraphDriver GraphDriver `json:"GraphDriver"`
}

type inspectImage struct {
	ID     string `json:"Id"`
	Config struct {
		Env        []string `json:"Env"`
		WorkingDir string   `json:"WorkingDir"`
	} `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	RootFS   struct {
		Type   string   `json:"Type"`
		Layers []string `json:"Layers"`
	} `json:"RootFS"`
}

func Resolve(ctx context.Context, dial func(ctx context.Context) (net.Conn, error), target, kind string) (*Resolved, error) {
	cli := &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		DisableKeepAlives: true,
	}}
	tryContainer := kind == "" || kind == DebugRequestContainer
	tryImage := kind == "" || kind == DebugRequestImage

	if tryContainer {
		c, err := inspectCtr(ctx, cli, target)
		if err == nil {
			return fromContainer(c), nil
		}
		if !isNotFound(err) {
			return nil, err
		}
	}
	if tryImage {
		im, err := inspectImg(ctx, cli, target)
		if err == nil {
			return fromImage(im, target), nil
		}
		if !isNotFound(err) {
			return nil, err
		}
	}
	return nil, &NotHereError{Target: target, Kind: kind}
}

type NotHereError struct {
	Target string
	Kind   string
}

func (e *NotHereError) Error() string {
	what := "container or image"
	switch e.Kind {
	case DebugRequestContainer:
		what = "container"
	case DebugRequestImage:
		what = "image"
	}
	return fmt.Sprintf("no %s %q here (docker pull %s first)", what, e.Target, e.Target)
}

func fromContainer(c *inspectContainer) *Resolved {
	r := &Resolved{
		ID:    c.ID,
		Name:  strings.TrimPrefix(c.Name, "/"),
		Image: c.Config.Image,
		Env:   c.Config.Env,
		Cwd:   c.Config.WorkingDir,
		Graph: c.GraphDriver,
	}
	if r.Image == "" {
		r.Image = c.Image
	}
	if c.State.Running || c.State.Paused {
		r.Kind = DebugKindRunning
		r.Pid = c.State.Pid
		r.Paused = c.State.Paused
	} else {
		r.Kind = DebugKindStopped
	}
	caps, ok := capsFromInspect(c.HostConfig.CapAdd, c.HostConfig.CapDrop)
	if c.HostConfig.Privileged {
		caps = allCapsMask()
	}
	r.Caps, r.HasCaps = caps, ok
	r.Seccomp, r.SeccompProfile = seccompOption(c.HostConfig.SecurityOpt, c.HostConfig.Privileged)
	return r
}

func seccompOption(opts []string, privileged bool) (string, []byte) {
	if privileged {
		return "unconfined", nil
	}
	for _, o := range opts {
		v, ok := strings.CutPrefix(o, "seccomp=")
		if !ok {
			v, ok = strings.CutPrefix(o, "seccomp:")
		}
		if !ok {
			continue
		}
		switch strings.TrimSpace(v) {
		case "unconfined":
			return "unconfined", nil
		case "", "builtin", "default":
			return "", nil
		}
		return "custom", []byte(v)
	}
	return "", nil
}

func SeccompNote(r *Resolved) string {
	switch r.Seccomp {
	case "custom":
		return "this session is held to the container's own seccomp profile, compiled to the same filter the container ran under"
	case "unconfined":
		return "the container ran with no seccomp filter (seccomp=unconfined); this session runs unconfined too"
	}
	return ""
}

func PtraceNote(r *Resolved) string {
	if r.Kind == DebugKindRunning && r.HasCaps && r.Caps&(1<<CapSysPtrace) != 0 {
		return r.Name + " holds CAP_SYS_PTRACE, so it can trace this debug shell; fev accepts this because the container already has ptrace over its own processes"
	}
	return ""
}

func fromImage(im *inspectImage, target string) *Resolved {
	name := target
	if len(im.RepoTags) > 0 && im.RepoTags[0] != "<none>:<none>" {
		name = im.RepoTags[0]
	}
	return &Resolved{
		Kind:    DebugKindImage,
		ID:      im.ID,
		Name:    name,
		Env:     im.Config.Env,
		Cwd:     im.Config.WorkingDir,
		DiffIDs: im.RootFS.Layers,
	}
}

func inspectCtr(ctx context.Context, cli *http.Client, id string) (*inspectContainer, error) {
	var c inspectContainer
	if err := getJSON(ctx, cli, "/containers/"+id+"/json", &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func inspectImg(ctx context.Context, cli *http.Client, ref string) (*inspectImage, error) {
	var im inspectImage
	if err := getJSON(ctx, cli, "/images/"+ref+"/json", &im); err != nil {
		return nil, err
	}
	return &im, nil
}

type httpStatusError struct {
	code int
	body string
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("engine %d: %s", e.code, e.body) }

func isNotFound(err error) bool {
	if e, ok := err.(*httpStatusError); ok {
		return e.code == http.StatusNotFound
	}
	return false
}

func getJSON(ctx context.Context, cli *http.Client, path string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://engine"+path, nil)
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &httpStatusError{code: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
