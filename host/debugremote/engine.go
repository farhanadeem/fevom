package debugremote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type Engine struct {
	t      *Transport
	client *http.Client
}

func NewEngine(t *Transport) *Engine { return &Engine{t: t, client: t.Client()} }

const apiBase = "/v1.44"

func (e *Engine) url(p string, q url.Values) string {
	u := "http://docker" + apiBase + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

func (e *Engine) do(ctx context.Context, method, p string, q url.Values, body io.Reader, ctype string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, e.url(p, q), body)
	if err != nil {
		return nil, err
	}
	req.Host = "docker"
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return nil, &apiError{status: resp.StatusCode, msg: engineMessage(msg)}
	}
	return resp, nil
}

func (e *Engine) getJSON(ctx context.Context, p string, q url.Values, v any) error {
	resp, err := e.do(ctx, http.MethodGet, p, q, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

func (e *Engine) postJSON(ctx context.Context, p string, q url.Values, body, v any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	resp, err := e.do(ctx, http.MethodPost, p, q, r, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if v == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func NotFound(err error) bool {
	var ae *apiError
	return asAPIError(err, &ae) && ae.status == http.StatusNotFound
}

func asAPIError(err error, out **apiError) bool {
	for err != nil {
		if ae, ok := err.(*apiError); ok {
			*out = ae
			return true
		}
		type unwrap interface{ Unwrap() error }
		u, ok := err.(unwrap)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func engineMessage(b []byte) string {
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &m) == nil && m.Message != "" {
		return m.Message
	}
	return strings.TrimSpace(string(b))
}

type Info struct {
	Architecture string `json:"Architecture"`
	OSType       string `json:"OSType"`
	ServerVer    string `json:"ServerVersion"`
}

func (i Info) GoArch() string {
	switch i.Architecture {
	case "aarch64", "arm64":
		return "arm64"
	case "x86_64", "amd64":
		return "amd64"
	default:
		return ""
	}
}

func (e *Engine) Info(ctx context.Context) (Info, error) {
	var i Info
	return i, e.getJSON(ctx, "/info", nil, &i)
}

type ContainerJSON struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status  string `json:"Status"`
		Running bool   `json:"Running"`
		Paused  bool   `json:"Paused"`
		Pid     int    `json:"Pid"`
	} `json:"State"`
	Config struct {
		Image      string   `json:"Image"`
		Env        []string `json:"Env"`
		WorkingDir string   `json:"WorkingDir"`
		User       string   `json:"User"`
	} `json:"Config"`
	HostConfig struct {
		CapAdd  []string `json:"CapAdd"`
		CapDrop []string `json:"CapDrop"`
		PidMode string   `json:"PidMode"`
		Limits
	} `json:"HostConfig"`
}

type Limits struct {
	Memory     int64  `json:"Memory,omitempty"`
	MemorySwap int64  `json:"MemorySwap,omitempty"`
	PidsLimit  *int64 `json:"PidsLimit,omitempty"`
	NanoCpus   int64  `json:"NanoCpus,omitempty"`
	CpuQuota   int64  `json:"CpuQuota,omitempty"`
	CpuPeriod  int64  `json:"CpuPeriod,omitempty"`
	CpuShares  int64  `json:"CpuShares,omitempty"`
	CpusetCpus string `json:"CpusetCpus,omitempty"`
}

func (e *Engine) ContainerInspect(ctx context.Context, ref string) (*ContainerJSON, error) {
	var c ContainerJSON
	if err := e.getJSON(ctx, "/containers/"+ref+"/json", nil, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

type ImageJSON struct {
	ID       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
	Config   struct {
		Env        []string `json:"Env"`
		WorkingDir string   `json:"WorkingDir"`
		User       string   `json:"User"`
	} `json:"Config"`
}

func (e *Engine) ImageInspect(ctx context.Context, ref string) (*ImageJSON, error) {
	var im ImageJSON
	if err := e.getJSON(ctx, "/images/"+ref+"/json", nil, &im); err != nil {
		return nil, err
	}
	return &im, nil
}

type imageSummary struct {
	ID       string            `json:"Id"`
	RepoTags []string          `json:"RepoTags"`
	Labels   map[string]string `json:"Labels"`
}

func (e *Engine) HelperImages(ctx context.Context) ([]imageSummary, error) {
	q := url.Values{}
	q.Set("filters", `{"label":["`+Label+"="+LabelHelper+`"]}`)
	var out []imageSummary
	return out, e.getJSON(ctx, "/images/json", q, &out)
}

func (e *Engine) ImageExists(ctx context.Context, ref string) bool {
	_, err := e.ImageInspect(ctx, ref)
	return err == nil
}

func (e *Engine) ImageLoad(ctx context.Context, tar io.Reader) error {
	resp, err := e.do(ctx, http.MethodPost, "/images/load", url.Values{"quiet": {"1"}}, tar, "application/x-tar")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	for _, line := range bytes.Split(body, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(line, &m) == nil && m.Error != "" {
			return fmt.Errorf("image load: %s", m.Error)
		}
	}
	return nil
}

func (e *Engine) ImageRemove(ctx context.Context, ref string) error {
	q := url.Values{"force": {"1"}}
	resp, err := e.do(ctx, http.MethodDelete, "/images/"+ref, q, nil, "")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

type ContainerSummary struct {
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	Image  string   `json:"Image"`
	State  string   `json:"State"`
	Status string   `json:"Status"`
}

func (e *Engine) ContainerList(ctx context.Context, all bool) ([]ContainerSummary, error) {
	q := url.Values{}
	if all {
		q.Set("all", "1")
	}
	var out []ContainerSummary
	if err := e.getJSON(ctx, "/containers/json", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type ImageSummary struct {
	ID       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
	Size     int64    `json:"Size"`
	Created  int64    `json:"Created"`
}

func (e *Engine) ImageList(ctx context.Context) ([]ImageSummary, error) {
	var out []ImageSummary
	if err := e.getJSON(ctx, "/images/json", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type CreateConfig struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Entrypoint []string          `json:"Entrypoint,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	WorkingDir string            `json:"WorkingDir,omitempty"`
	Tty        bool              `json:"Tty"`
	OpenStdin  bool              `json:"OpenStdin"`
	StdinOnce  bool              `json:"StdinOnce"`
	Labels     map[string]string `json:"Labels,omitempty"`
	HostConfig HostConfig        `json:"HostConfig"`
}

type HostConfig struct {
	AutoRemove   bool     `json:"AutoRemove"`
	PidMode      string   `json:"PidMode,omitempty"`
	NetworkMode  string   `json:"NetworkMode,omitempty"`
	IpcMode      string   `json:"IpcMode,omitempty"`
	CapAdd       []string `json:"CapAdd,omitempty"`
	CapDrop      []string `json:"CapDrop,omitempty"`
	SecurityOpt  []string `json:"SecurityOpt,omitempty"`
	Binds        []string `json:"Binds,omitempty"`
	ReadonlyPath []string `json:"-"`
	Limits
}

func (e *Engine) ContainerCreate(ctx context.Context, name string, cfg CreateConfig) (string, error) {
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	var r struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if err := e.postJSON(ctx, "/containers/create", q, cfg, &r); err != nil {
		return "", err
	}
	return r.ID, nil
}

func (e *Engine) ContainerStart(ctx context.Context, id string) error {
	return e.postJSON(ctx, "/containers/"+id+"/start", nil, nil, nil)
}

func (e *Engine) ContainerRemove(ctx context.Context, id string) error {
	q := url.Values{"force": {"1"}, "v": {"1"}}
	resp, err := e.do(ctx, http.MethodDelete, "/containers/"+id, q, nil, "")
	if err != nil {
		if NotFound(err) {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}

func (e *Engine) ContainerWait(ctx context.Context, id string) (int, error) {
	resp, err := e.do(ctx, http.MethodPost, "/containers/"+id+"/wait", nil, nil, "")
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	var r struct {
		StatusCode int `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return -1, err
	}
	if r.Error != nil && r.Error.Message != "" {
		return r.StatusCode, fmt.Errorf("%s", r.Error.Message)
	}
	return r.StatusCode, nil
}

func (e *Engine) ContainerCommit(ctx context.Context, id, repo, tag string) (string, error) {
	q := url.Values{"container": {id}, "repo": {repo}, "tag": {tag}}
	ref := repo + ":" + tag
	cfg := map[string]any{"Labels": map[string]string{Label: LabelTemp}}
	var r struct {
		ID string `json:"Id"`
	}
	if err := e.postJSON(ctx, "/commit", q, cfg, &r); err != nil {
		return "", err
	}
	return ref, nil
}

type volumeJSON struct {
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels"`
}

func (e *Engine) StoreVolumes(ctx context.Context) ([]string, error) {
	q := url.Values{}
	q.Set("filters", `{"label":["`+Label+"="+LabelStore+`"]}`)
	var r struct {
		Volumes []volumeJSON `json:"Volumes"`
	}
	if err := e.getJSON(ctx, "/volumes", q, &r); err != nil {
		return nil, err
	}
	var out []string
	for _, v := range r.Volumes {
		if v.Labels[Label] == LabelStore {
			out = append(out, v.Name)
		}
	}
	return out, nil
}

func (e *Engine) VolumeExists(ctx context.Context, name string) bool {
	var v volumeJSON
	return e.getJSON(ctx, "/volumes/"+name, nil, &v) == nil
}

func (e *Engine) VolumeCreate(ctx context.Context, name string) (created bool, err error) {
	if e.VolumeExists(ctx, name) {
		return false, nil
	}
	body := map[string]any{"Name": name, "Labels": map[string]string{Label: LabelStore}}
	if err := e.postJSON(ctx, "/volumes/create", nil, body, nil); err != nil {
		return false, err
	}
	return true, nil
}

func (e *Engine) VolumeRemove(ctx context.Context, name string) error {
	q := url.Values{"force": {"1"}}
	resp, err := e.do(ctx, http.MethodDelete, "/volumes/"+name, q, nil, "")
	if err != nil {
		if NotFound(err) {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}

func (e *Engine) Attach(ctx context.Context, id string, stdin bool) (net.Conn, *bufio.Reader, error) {
	conn, err := e.t.dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	q := url.Values{"stream": {"1"}, "stdout": {"1"}, "stderr": {"1"}}
	if stdin {
		q.Set("stdin", "1")
	}
	req := fmt.Sprintf("POST %s/containers/%s/attach?%s HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n",
		apiBase, id, q.Encode())
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		conn.Close()
		return nil, nil, &apiError{status: resp.StatusCode, msg: engineMessage(msg)}
	}
	return conn, br, nil
}
