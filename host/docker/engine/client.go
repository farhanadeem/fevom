package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Dialer func(ctx context.Context) (net.Conn, error)

const (
	maxBody       = 16 << 20
	maxEventLine  = 1 << 20
	maxPorts      = 4 * 65536
	maxContainers = 16384
)

type Client struct {
	http *http.Client
}

func NewClient(dial Dialer) *Client {
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext:        func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		MaxIdleConns:       2,
		IdleConnTimeout:    0,
		DisableCompression: true,
	}}}
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("the Docker engine answered %d: %s", e.Status, e.Message)
}

func NotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		var body struct {
			Message string `json:"message"`
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(data, &body) != nil || body.Message == "" {
			body.Message = strings.TrimSpace(string(data))
		}
		return nil, &StatusError{Status: resp.StatusCode, Message: body.Message}
	}
	return resp, nil
}

func (c *Client) get(ctx context.Context, path string, into any) error {
	resp, err := c.do(ctx, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(data) > maxBody {
		return fmt.Errorf("the Docker engine's answer to %s is larger than %d bytes", path, maxBody)
	}
	return json.Unmarshal(data, into)
}

func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.do(ctx, "/_ping")
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
	return resp.Body.Close()
}

type Binding struct {
	Proto         string
	HostIP        netip.Addr
	HostPort      uint16
	ContainerPort uint16
}

type Container struct {
	ID          string
	Name        string
	Image       string
	Running     bool
	Bindings    []Binding
	Binds       []string
	SCTP        []string
	Labels      map[string]string
	NetworkMode string
	Networks    []Network
	SandboxKey  string
	Exposed     []string
}

type Network struct {
	Name     string
	Address  netip.Addr
	Prefix   netip.Prefix
	Address6 netip.Addr
}

const (
	maxLabels     = 256
	maxLabelBytes = 4096
	maxNetworks   = 64
	maxExposed    = 256
)

const maxBinds = 1024

func ValidID(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for i, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case i > 0 && (c == '_' || c == '.' || c == '-'):
		default:
			return false
		}
	}
	return true
}

func (c *Client) Running(ctx context.Context) ([]string, error) {
	var list []struct {
		ID string `json:"Id"`
	}
	if err := c.get(ctx, "/containers/json", &list); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list))
	for _, item := range list {
		if ValidID(item.ID) && len(ids) < maxContainers {
			ids = append(ids, item.ID)
		}
	}
	return ids, nil
}

type inspected struct {
	ID    string `json:"Id"`
	Name  string
	State struct {
		Running bool
	}
	Config struct {
		Image        string
		Labels       map[string]string
		ExposedPorts map[string]struct{}
	}
	HostConfig struct {
		NetworkMode string
	}
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string
		}
		SandboxKey string
		Networks   map[string]struct {
			IPAddress         string
			IPPrefixLen       int
			GlobalIPv6Address string
		}
	}
	Mounts []struct {
		Type   string
		Source string
		Name   string
		Driver string
	}
}

const maxVolumeLooks = 64

func (c *Client) boundDevice(ctx context.Context, name string) string {
	var doc struct {
		Driver  string
		Options map[string]string
	}
	if err := c.get(ctx, "/volumes/"+url.PathEscape(name), &doc); err != nil {
		return ""
	}
	device := doc.Options["device"]
	if doc.Driver != "local" || !strings.HasPrefix(device, "/") || len(device) > 4096 {
		return ""
	}
	for _, opt := range strings.Split(doc.Options["o"], ",") {
		if opt == "bind" || opt == "rbind" {
			return device
		}
	}
	return ""
}

func (c *Client) Inspect(ctx context.Context, id string) (Container, error) {
	if !ValidID(id) {
		return Container{}, fmt.Errorf("%q is not a container id", id)
	}
	var doc inspected
	if err := c.get(ctx, "/containers/"+url.PathEscape(id)+"/json", &doc); err != nil {
		return Container{}, err
	}
	if !ValidID(doc.ID) {
		return Container{}, fmt.Errorf("the Docker engine described container %q with the id %q", id, doc.ID)
	}
	out := Container{ID: doc.ID, Name: strings.TrimPrefix(doc.Name, "/"), Image: doc.Config.Image, Running: doc.State.Running}
	if !out.Running {
		return out, nil
	}
	registryFields(&out, &doc)
	for _, m := range doc.Mounts {
		if m.Type == "bind" && strings.HasPrefix(m.Source, "/") && len(m.Source) <= 4096 && len(out.Binds) < maxBinds {
			out.Binds = append(out.Binds, m.Source)
		}
	}
	looks := 0
	for _, m := range doc.Mounts {
		if m.Type != "volume" || m.Name == "" || m.Driver != "local" || looks >= maxVolumeLooks || len(out.Binds) >= maxBinds {
			continue
		}
		looks++
		if device := c.boundDevice(ctx, m.Name); device != "" {
			out.Binds = append(out.Binds, device)
		}
	}
	for key, bindings := range doc.NetworkSettings.Ports {
		port, proto, _ := strings.Cut(key, "/")
		containerPort, err := strconv.ParseUint(port, 10, 16)
		if err != nil || containerPort == 0 || (proto != "tcp" && proto != "udp") {
			if proto == "sctp" && len(bindings) > 0 && len(out.SCTP) < 16 {
				out.SCTP = append(out.SCTP, key)
			}
			continue
		}
		for _, b := range bindings {
			hostPort, err := strconv.ParseUint(b.HostPort, 10, 16)
			if err != nil || hostPort == 0 {
				continue
			}
			binding := Binding{Proto: proto, HostPort: uint16(hostPort), ContainerPort: uint16(containerPort)}
			if b.HostIP != "" {
				addr, err := netip.ParseAddr(b.HostIP)
				if err != nil || addr.Zone() != "" {
					continue
				}
				binding.HostIP = addr.Unmap()
			}
			if len(out.Bindings) < maxPorts {
				out.Bindings = append(out.Bindings, binding)
			}
		}
	}
	return out, nil
}

type Event struct {
	Type       string            `json:"type"`
	Action     string            `json:"action"`
	Detail     string            `json:"detail,omitempty"`
	ID         string            `json:"id"`
	Name       string            `json:"name,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
	Time       time.Time         `json:"time"`
}

const (
	TypeContainer = "container"
	TypeNetwork   = "network"
	TypeVolume    = "volume"
	TypeImage     = "image"
)

type wireEvent struct {
	Type   string
	Action string
	Actor  struct {
		ID         string
		Attributes map[string]string
	}
	Time     int64 `json:"time"`
	TimeNano int64 `json:"timeNano"`
}

func Normalise(line []byte) (Event, error) {
	var w wireEvent
	if err := json.Unmarshal(line, &w); err != nil {
		return Event{}, fmt.Errorf("an event that is not JSON: %w", err)
	}
	if w.Type == "" || w.Action == "" {
		return Event{}, errors.New("an event with no type or no action")
	}
	e := Event{Type: w.Type, Action: w.Action, ID: w.Actor.ID, Attributes: w.Actor.Attributes}
	if action, detail, ok := strings.Cut(w.Action, ": "); ok {
		e.Action, e.Detail = action, detail
	}
	e.Name = w.Actor.Attributes["name"]
	if e.Name == e.ID {
		e.Name = ""
	}
	switch {
	case w.TimeNano > 0:
		e.Time = time.Unix(0, w.TimeNano)
	case w.Time > 0:
		e.Time = time.Unix(w.Time, 0)
	}
	return e, nil
}

type Stream struct {
	body io.ReadCloser
	r    *bufio.Reader
}

func (c *Client) Events(ctx context.Context) (*Stream, error) {
	resp, err := c.do(ctx, "/events")
	if err != nil {
		return nil, err
	}
	return &Stream{body: resp.Body, r: bufio.NewReaderSize(resp.Body, 64<<10)}, nil
}

func (s *Stream) Next() (Event, error) {
	for {
		line, err := s.line()
		if err != nil {
			return Event{}, err
		}
		if len(line) == 0 {
			continue
		}
		if e, err := Normalise(line); err == nil {
			return e, nil
		}
	}
}

func (s *Stream) line() ([]byte, error) {
	var line []byte
	for {
		part, err := s.r.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > maxEventLine {
			return nil, fmt.Errorf("an event longer than %d bytes", maxEventLine)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		return line[:len(line)-1], nil
	}
}

func (s *Stream) Close() { s.body.Close() }

func registryFields(out *Container, doc *inspected) {
	for k, v := range doc.Config.Labels {
		if len(out.Labels) >= maxLabels || len(k)+len(v) > maxLabelBytes {
			continue
		}
		if out.Labels == nil {
			out.Labels = map[string]string{}
		}
		out.Labels[k] = v
	}
	if len(doc.HostConfig.NetworkMode) <= 300 {
		out.NetworkMode = doc.HostConfig.NetworkMode
	}
	if k := doc.NetworkSettings.SandboxKey; strings.HasPrefix(k, "/") && len(k) <= 4096 && !strings.Contains(k, "/../") {
		out.SandboxKey = k
	}
	for name, n := range doc.NetworkSettings.Networks {
		ip, err := netip.ParseAddr(n.IPAddress)
		if err != nil || !ip.Is4() || len(name) > 255 || len(out.Networks) >= maxNetworks {
			continue
		}
		var prefix netip.Prefix
		if n.IPPrefixLen > 0 && n.IPPrefixLen <= 32 {
			prefix = netip.PrefixFrom(ip, n.IPPrefixLen).Masked()
		}
		nw := Network{Name: name, Address: ip, Prefix: prefix}
		if ip6, err := netip.ParseAddr(n.GlobalIPv6Address); err == nil && ip6.Is6() && !ip6.Is4In6() && ip6.Zone() == "" {
			nw.Address6 = ip6
		}
		out.Networks = append(out.Networks, nw)
	}
	sort.Slice(out.Networks, func(i, j int) bool { return out.Networks[i].Name < out.Networks[j].Name })
	for key := range doc.Config.ExposedPorts {
		port, proto, _ := strings.Cut(key, "/")
		if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 || (proto != "tcp" && proto != "udp") || len(out.Exposed) >= maxExposed {
			continue
		}
		out.Exposed = append(out.Exposed, key)
	}
	sort.Strings(out.Exposed)
}
