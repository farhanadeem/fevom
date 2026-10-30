package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"fev/host/daemon"
	"fev/host/daemon/boottrace"
	"fev/host/daemon/config"
	"fev/host/daemon/home"
	"fev/host/docker/engine"
	"fev/host/docker/fakeguest"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/vmmctl"
)

func bindings(specs ...string) []engine.Binding {
	var out []engine.Binding
	for _, spec := range specs {
		f := strings.Fields(spec)
		var port uint16
		fmt.Sscan(f[2], &port)
		b := engine.Binding{Proto: f[0], HostPort: port, ContainerPort: 80}
		if f[1] != "-" {
			b.HostIP = netip.MustParseAddr(f[1])
		}
		out = append(out, b)
	}
	return out
}

func TestWhereAPublishedPortLeadsInTheGuest(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"-p 8080:80, as Docker reports it", []string{"tcp 0.0.0.0 8080", "tcp :: 8080"}, "tcp/8080->127.0.0.1:8080"},
		{"no address given", []string{"tcp - 8080"}, "tcp/8080->127.0.0.1:8080"},
		{"the IPv4 wildcard alone", []string{"tcp 0.0.0.0 8080"}, "tcp/8080->127.0.0.1:8080"},
		{"-p 127.0.0.1:5432:5432", []string{"tcp 127.0.0.1 5432"}, "tcp/5432->127.0.0.1:5432"},
		{"-p [::1]:8082:80, IPv6 only", []string{"tcp ::1 8082"}, "tcp/8082->[::1]:8082"},
		{"-p [::]:8082:80, IPv6 only", []string{"tcp :: 8082"}, "tcp/8082->[::1]:8082"},
		{"both loopbacks", []string{"tcp ::1 8080", "tcp 127.0.0.1 8080"}, "tcp/8080->127.0.0.1:8080"},
		{"a specific address", []string{"tcp 10.1.2.3 8080"}, "tcp/8080->10.1.2.3:8080"},
		{"a specific IPv6 address", []string{"tcp fd00::5 8080"}, "tcp/8080->[fd00::5]:8080"},
		{"a specific address and the IPv4 wildcard", []string{"tcp 10.1.2.3 8080", "tcp 0.0.0.0 8080"}, "tcp/8080->127.0.0.1:8080"},
		{"a specific address and IPv6 loopback", []string{"tcp 10.1.2.3 8080", "tcp ::1 8080"}, "tcp/8080->[::1]:8080"},
		{"two specific addresses: always the same one", []string{"tcp fd00::5 8080", "tcp 10.9.9.9 8080", "tcp 10.1.2.3 8080"}, "tcp/8080->10.1.2.3:8080"},
		{"another loopback address is a specific one", []string{"tcp 127.0.0.2 8080"}, "tcp/8080->127.0.0.2:8080"},
		{"IPv4 written as IPv6", []string{"tcp ::ffff:127.0.0.1 8080"}, "tcp/8080->127.0.0.1:8080"},
		{"a low port", []string{"tcp 0.0.0.0 80", "tcp :: 80"}, "tcp/80->127.0.0.1:80"},
		{"TCP and UDP on one number are two forwards", []string{"udp 0.0.0.0 53", "tcp 0.0.0.0 53"}, "tcp/53->127.0.0.1:53 udp/53->127.0.0.1:53"},
		{"several ports, in order", []string{"tcp - 9000", "tcp - 80", "udp ::1 7000"}, "tcp/80->127.0.0.1:80 udp/7000->[::1]:7000 tcp/9000->127.0.0.1:9000"},
		{"what is not TCP or UDP, and port 0", []string{"sctp 0.0.0.0 9", "tcp 0.0.0.0 0"}, ""},
		{"nothing published", nil, ""},
	}
	for _, tc := range cases {
		var got []string
		for _, w := range Wants(bindings(tc.in...), netip.Addr{}) {
			got = append(got, fmt.Sprintf("%s/%d->%s", w.Proto, w.Port, w.Guest))
		}
		if strings.Join(got, " ") != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, strings.Join(got, " "), tc.want)
		}
	}
}

func TestTheCompatibilityLinkIsOnlyLookedAt(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "home", "run", "docker.sock")
	link := filepath.Join(dir, "var-run", "docker.sock")
	os.MkdirAll(filepath.Dir(link), 0o755)
	check := func(wantState, wantTarget string) {
		t.Helper()
		if state, target := compatStatus(link, socket); state != wantState || target != wantTarget {
			t.Fatalf("got %s %q, want %s %q", state, target, wantState, wantTarget)
		}
	}
	check(api.CompatAbsent, "")
	os.Symlink(socket, link)
	check(api.CompatLinked, "")
	os.Remove(link)
	os.Symlink("../home/run/../run/docker.sock", link)
	check(api.CompatLinked, "")
	os.Remove(link)
	os.Symlink("/Users/someone/.other/run/docker.sock", link)
	check(api.CompatElsewhere, "/Users/someone/.other/run/docker.sock")
	os.Remove(link)
	os.WriteFile(link, nil, 0o600)
	check(api.CompatElsewhere, "")
	if info, err := os.Lstat(link); err != nil || !info.Mode().IsRegular() {
		t.Fatal("the path was changed")
	}
}

type fakeHost struct {
	t      *testing.T
	layout home.Layout
	cfg    *config.Store
	engine *fakeguest.Engine
	relay  *fakeguest.Relay

	mu             sync.Mutex
	status         api.Status
	vm             bool
	bootID         string
	marks          []string
	methods        map[string]daemon.MethodFunc
	forwards       []string
	events         []api.DockerEvent
	changed        chan struct{}
	agent          *agent.Client
	starts, checks int
	rouses, nudges int
	warnings       map[string]string
	startErr       error
}

func (h *fakeHost) PublishFiles(api.FilesStatus) {}
func (h *fakeHost) WorkEnded()                   {}

func (h *fakeHost) Layout() home.Layout   { return h.layout }
func (h *fakeHost) Log() *slog.Logger     { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func (h *fakeHost) Config() *config.Store { return h.cfg }
func (h *fakeHost) Fake() bool            { return true }
func (h *fakeHost) Status() api.Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}
func (h *fakeHost) VM() (daemon.VM, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return fakeVM{h.layout.VMDir(), h.bootID, h.agent}, h.vm
}
func (h *fakeHost) Mark(name string) {
	h.mu.Lock()
	h.marks = append(h.marks, name)
	h.mu.Unlock()
}
func (h *fakeHost) Handle(method string, fn daemon.MethodFunc) { h.methods[method] = fn }
func (h *fakeHost) StopVM(context.Context) error               { return nil }
func (h *fakeHost) StartVM() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.starts++
	return h.startErr
}
func (h *fakeHost) StartOnDemand() error { return h.StartVM() }

func (h *fakeHost) StartAsked() error { return h.StartVM() }
func (h *fakeHost) WaitStatus(ctx context.Context, until func(api.Status) bool) (api.Status, error) {
	return api.Status{}, errors.New("the fake host has no status to wait for")
}
func (h *fakeHost) PublishMachine(api.MachinesChanged) {}
func (h *fakeHost) PublishOperation(api.Operation)     {}
func (h *fakeHost) Warn(key, text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.warnings == nil {
		h.warnings = map[string]string{}
	}
	if text == "" {
		delete(h.warnings, key)
	} else {
		h.warnings[key] = text
	}
}

func (h *fakeHost) warning(key string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.warnings[key]
}
func (h *fakeHost) Rouse(asked bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if asked {
		h.rouses++
	} else {
		h.nudges++
	}
	return false
}
func (h *fakeHost) CheckAlive() {
	h.mu.Lock()
	h.checks++
	h.mu.Unlock()
}
func (h *fakeHost) PublishDocker(e api.DockerEvent) {
	h.mu.Lock()
	h.events = append(h.events, e)
	h.mu.Unlock()
}
func (h *fakeHost) PublishForward(c api.ForwardsChanged) {
	note := c.Forward.ID + " " + c.Forward.State
	if c.Removed {
		note = c.Forward.ID + " removed"
	}
	h.mu.Lock()
	h.forwards = append(h.forwards, note)
	h.mu.Unlock()
	select {
	case h.changed <- struct{}{}:
	default:
	}
}

type fakeVM struct {
	dir, boot string
	agent     *agent.Client
}

func (v fakeVM) VsockPath(port uint32) string {
	return filepath.Join(v.dir, fmt.Sprintf("vsock_%d.sock", port))
}
func (v fakeVM) Agent() *agent.Client    { return v.agent }
func (v fakeVM) BootID() string          { return v.boot }
func (v fakeVM) Control() *vmmctl.Client { return nil }

type world struct {
	*fakeHost
	m *Module
}

func newWorld(t *testing.T, opts Options, values ...map[string]any) *world {
	t.Helper()
	return newWorldWith(t, opts, nil, values...)
}

func newWorldWith(t *testing.T, opts Options, initial func(*api.Status), values ...map[string]any) *world {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bm.")
	if err != nil {
		t.Fatal(err)
	}
	layout := home.Layout{Home: dir, DataDir: filepath.Join(dir, "data")}
	if err := layout.Create(); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(layout.ConfigFile(), config.Keys(config.ThisMac()), func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range values {
		if _, err := cfg.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	h := &fakeHost{t: t, layout: layout, cfg: cfg, engine: fakeguest.NewEngine(), methods: map[string]daemon.MethodFunc{}, changed: make(chan struct{}, 1)}
	h.relay = &fakeguest.Relay{Engine: h.engine}
	h.status = api.Status{State: api.StateStopped, Engine: api.EngineStopped}
	if initial != nil {
		initial(&h.status)
	}
	el, err := net.Listen("unix", fakeVM{dir: layout.VMDir()}.VsockPath(portEngine))
	if err != nil {
		t.Fatal(err)
	}
	rl, err := net.Listen("unix", fakeVM{dir: layout.VMDir()}.VsockPath(portRelay))
	if err != nil {
		t.Fatal(err)
	}
	go h.engine.Serve(el)
	go h.relay.Serve(rl)
	if opts.CompatLink == "" {
		opts.CompatLink = filepath.Join(dir, "compat", "docker.sock")
	}
	w := &world{fakeHost: h, m: New(opts)}
	if err := w.m.Start(h); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		w.m.Close()
		rl.Close()
		h.engine.Close()
		os.RemoveAll(dir)
	})
	return w
}

func (w *world) to(state, engineState, errText string) {
	w.mu.Lock()
	if state == api.StateStarting && w.status.State == api.StateStopped {
		w.bootID = fmt.Sprintf("boot%d", w.status.Seq)
	}
	w.status.Seq++
	w.status.State, w.status.Engine, w.status.Error = state, engineState, errText
	if state != api.StateStopped && state != api.StateStopping {
		w.status.StopRequested = false
	}
	w.vm = state != api.StateStopped
	st := w.status
	w.mu.Unlock()
	w.m.StateChanged(st)
	w.settled()
}

func (w *world) settled() {
	w.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		w.m.state.Lock()
		n := w.m.settles
		w.m.state.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			w.t.Fatal("the engine's configuration was never settled")
		}
		time.Sleep(200 * time.Microsecond)
	}
}

func (w *world) toWith(state, engineState string, change func(*api.Status)) {
	w.mu.Lock()
	change(&w.status)
	w.mu.Unlock()
	w.to(state, engineState, "")
}

func (w *world) startCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.starts
}

func (w *world) checkCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.checks
}

func (w *world) boot() {
	w.to(api.StateStarting, api.EngineStopped, "")
	w.to(api.StateStarting, api.EngineStarting, "")
	w.to(api.StateRunning, api.EngineReady, "")
}

func (w *world) client() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", w.m.Socket())
		},
	}}
}

type answer struct {
	status int
	body   string
	err    error
}

func (w *world) get(path string) answer {
	resp, err := w.client().Get("http://docker" + path)
	if err != nil {
		return answer{err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return answer{resp.StatusCode, strings.TrimSpace(string(body)), err}
}

func (w *world) later(path string) chan answer {
	ch := make(chan answer, 1)
	go func() { ch <- w.get(path) }()
	return ch
}

func TestWhoMayReachAPublishedPortOnTheMac(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		lan  bool
	}{
		{"-p 8080:80, as Docker reports it", []string{"tcp 0.0.0.0 8080", "tcp :: 8080"}, true},
		{"no address given", []string{"tcp - 8080"}, true},
		{"the IPv4 wildcard alone", []string{"udp 0.0.0.0 8080"}, true},
		{"the IPv6 wildcard alone", []string{"tcp :: 8080"}, true},
		{"the IPv4 wildcard written as IPv6", []string{"tcp ::ffff:0.0.0.0 8080"}, true},
		{"a low port", []string{"tcp 0.0.0.0 80"}, true},
		{"-p 127.0.0.1:5432:5432", []string{"tcp 127.0.0.1 5432"}, false},
		{"-p [::1]:8082:80", []string{"udp ::1 8082"}, false},
		{"both loopbacks", []string{"tcp ::1 8080", "tcp 127.0.0.1 8080"}, false},
		{"a specific address: an address of the guest, which is nothing on the Mac", []string{"tcp 10.1.2.3 8080"}, false},
		{"a specific IPv6 address", []string{"tcp fd00::5 8080"}, false},
		{"another loopback address", []string{"tcp 127.0.0.2 8080"}, false},
		{"loopback and a wildcard: the widest wins", []string{"tcp 127.0.0.1 8080", "tcp :: 8080"}, true},
		{"a specific address and a wildcard: the widest wins", []string{"tcp 0.0.0.0 8080", "tcp 10.1.2.3 8080"}, true},
		{"a specific address and loopback", []string{"tcp 10.1.2.3 8080", "tcp ::1 8080"}, false},
	}
	for _, tc := range cases {
		wants := Wants(bindings(tc.in...), netip.Addr{})
		if len(wants) != 1 || wants[0].LAN != tc.lan {
			t.Errorf("%s: got %+v, want LAN %v", tc.name, wants, tc.lan)
		}
	}
	wants := Wants(bindings("tcp 0.0.0.0 53", "udp 127.0.0.1 53", "tcp 127.0.0.1 54"), netip.Addr{})
	if len(wants) != 3 || !wants[0].LAN || wants[1].LAN || wants[2].LAN {
		t.Errorf("%+v", wants)
	}
}

func lanAddr() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			return n.IP.String()
		}
	}
	return ""
}

func fetchAt(host string, port uint16) (string, error) {
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, fmt.Sprint(port)) + "/")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(body)), err
}

func (w *world) macs(port uint16, want string) {
	w.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		var got string
		for _, f := range call[api.Forwards](w, api.MethodForwardsList).Forwards {
			if strings.HasSuffix(f.ID, fmt.Sprintf("/%d", port)) && f.State == api.ForwardActive {
				got = fmt.Sprint(f.Mac)
			}
		}
		if got == want {
			return
		}
		select {
		case <-w.changed:
		case <-deadline:
			w.t.Fatalf("port %d listens on %s, want %s", port, got, want)
		}
	}
}

func TestAWildcardPublishIsDialledAtTheGuestsUplinkAddress(t *testing.T) {
	uplink := netip.MustParseAddr("198.18.0.2")
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"-p 8080:80, as Docker reports it", []string{"tcp 0.0.0.0 8080", "tcp :: 8080"}, "tcp/8080->198.18.0.2:8080"},
		{"no address given", []string{"udp - 53"}, "udp/53->198.18.0.2:53"},
		{"the IPv4 wildcard and loopback", []string{"tcp 127.0.0.1 8080", "tcp 0.0.0.0 8080"}, "tcp/8080->198.18.0.2:8080"},
		{"the IPv4 wildcard and a specific address", []string{"tcp 10.1.2.3 8080", "tcp 0.0.0.0 8080"}, "tcp/8080->198.18.0.2:8080"},
		{"-p 127.0.0.1:5432:5432 answers on loopback alone", []string{"tcp 127.0.0.1 5432"}, "tcp/5432->127.0.0.1:5432"},
		{"IPv6 only: the uplink has no IPv6 address", []string{"tcp :: 8082"}, "tcp/8082->[::1]:8082"},
		{"loopback and the IPv6 wildcard", []string{"tcp 127.0.0.1 8080", "tcp :: 8080"}, "tcp/8080->127.0.0.1:8080"},
		{"a specific address", []string{"tcp 10.1.2.3 8080"}, "tcp/8080->10.1.2.3:8080"},
	}
	for _, tc := range cases {
		var got []string
		for _, w := range Wants(bindings(tc.in...), uplink) {
			got = append(got, fmt.Sprintf("%s/%d->%s", w.Proto, w.Port, w.Guest))
		}
		if strings.Join(got, " ") != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, strings.Join(got, " "), tc.want)
		}
	}

	guest := []agent.Interface{
		{Name: "lo", Up: true, Addresses: []string{"127.0.0.1/8", "::1/128"}},
		{Name: "tunl0", Up: false, Addresses: []string{}},
		{Name: "eth0", Up: true, Addresses: []string{"fe80::1/64", "198.18.0.2/24"}},
		{Name: "docker0", Up: true, Addresses: []string{"172.17.0.1/16"}},
	}
	if got := uplinkOf(guest); got != uplink {
		t.Errorf("uplink of a guest: %s", got)
	}
	for name, ifs := range map[string][]agent.Interface{
		"nothing":               nil,
		"loopback alone":        guest[:1],
		"an uplink not up":      {{Name: "eth0", Addresses: []string{"198.18.0.2/24"}}},
		"no IPv4 address":       {{Name: "eth0", Up: true, Addresses: []string{"fe80::1/64", "169.254.3.4/16"}}},
		"what is no address":    {{Name: "eth0", Up: true, Addresses: []string{"nonsense"}}},
		"no address at all":     {{Name: "eth0", Up: true, Addresses: []string{"0.0.0.0/0"}}},
		"a multicast address":   {{Name: "eth0", Up: true, Addresses: []string{"224.0.0.1/4"}}},
		"the broadcast address": {{Name: "eth0", Up: true, Addresses: []string{"255.255.255.255/32"}}},
	} {
		if got := uplinkOf(ifs); got.IsValid() {
			t.Errorf("%s: uplink %s", name, got)
		}
	}

	w := newWorld(t, Options{Autostart: true})
	asked := 0
	w.m.uplinkCall = func(context.Context, daemon.VM) netip.Addr {
		asked++
		return netip.MustParseAddr(fakeguest.Uplink)
	}
	w.boot()
	wide, narrow := freePort(t), freePort(t)
	w.engine.Run(fakeguest.Spec{Name: "wide", Ports: []fakeguest.Port{{HostPort: wide, Proto: "tcp", ContainerPort: 80}}})
	w.engine.Run(fakeguest.Spec{Name: "narrow", Ports: []fakeguest.Port{{HostIP: "127.0.0.1", HostPort: narrow, Proto: "tcp", ContainerPort: 80}}})
	w.macs(wide, fmt.Sprintf("[0.0.0.0:%d [::]:%d]", wide, wide))
	w.macs(narrow, fmt.Sprintf("[127.0.0.1:%d [::1]:%d]", narrow, narrow))
	for _, host := range []string{"127.0.0.1", "::1"} {
		if body, err := fetchAt(host, wide); err != nil || body != "hello from wide:80/tcp" {
			t.Fatalf("%s: %q, %v", host, body, err)
		}
	}
	if body, err := fetchAt("127.0.0.1", narrow); err != nil || body != "hello from narrow:80/tcp" {
		t.Fatalf("%q, %v", body, err)
	}
	if up, lo := w.relay.Opened(fmt.Sprintf("tcp %s:%d", fakeguest.Uplink, wide)), w.relay.Opened(fmt.Sprintf("tcp 127.0.0.1:%d", wide)); up != 2 || lo != 0 {
		t.Fatalf("the wildcard publish was dialled %d times at the uplink address and %d times at loopback", up, lo)
	}
	if lo := w.relay.Opened(fmt.Sprintf("tcp 127.0.0.1:%d", narrow)); lo != 1 {
		t.Fatalf("the loopback publish was dialled %d times at loopback", lo)
	}
	if asked != 1 {
		t.Fatalf("the guest was asked for its uplink address %d times", asked)
	}
}

func TestAPublishedPortFollowsItsAddressAndTheSetting(t *testing.T) {
	w := newWorld(t, Options{Autostart: true})
	w.boot()
	wide, narrow := freePort(t), freePort(t)
	w.engine.Run(fakeguest.Spec{Name: "wide", Ports: []fakeguest.Port{{HostPort: wide, Proto: "tcp", ContainerPort: 80}}})
	w.engine.Run(fakeguest.Spec{Name: "narrow", Ports: []fakeguest.Port{{HostIP: "127.0.0.1", HostPort: narrow, Proto: "tcp", ContainerPort: 80}}})
	wildcards := fmt.Sprintf("[0.0.0.0:%d [::]:%d]", wide, wide)
	w.macs(wide, wildcards)
	w.macs(narrow, fmt.Sprintf("[127.0.0.1:%d [::1]:%d]", narrow, narrow))
	for _, host := range []string{"127.0.0.1", "::1"} {
		if body, err := fetchAt(host, wide); err != nil || body != "hello from wide:80/tcp" {
			t.Fatalf("%s: %q, %v", host, body, err)
		}
		if body, err := fetchAt(host, narrow); err != nil || body != "hello from narrow:80/tcp" {
			t.Fatalf("%s: %q, %v", host, body, err)
		}
	}
	lan := lanAddr()
	onLAN := func(name string, port uint16, served bool) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			if lan == "" {
				t.Skip("this Mac has no address but loopback; the LAN address is not exercised")
			}
			body, err := fetchAt(lan, port)
			if served && (err != nil || !strings.HasPrefix(body, "hello from ")) {
				t.Fatalf("%s:%d: %q, %v", lan, port, body, err)
			}
			if !served && err == nil {
				t.Fatalf("%s:%d served %q", lan, port, body)
			}
		})
	}
	onLAN("a wildcard publish answers at the LAN address", wide, true)
	onLAN("a loopback publish does not", narrow, false)

	w.mu.Lock()
	w.forwards = nil
	w.mu.Unlock()
	w.m.ConfigChanged(api.ConfigChange{Key: api.KeyNetworkPublishLAN, Value: false})
	w.macs(wide, fmt.Sprintf("[127.0.0.1:%d [::1]:%d]", wide, wide))
	if body, err := fetchAt("127.0.0.1", wide); err != nil || body != "hello from wide:80/tcp" {
		t.Fatalf("with the setting off: %q, %v", body, err)
	}
	onLAN("with the setting off a wildcard publish does not", wide, false)
	w.mu.Lock()
	notes := strings.Join(w.forwards, ", ")
	w.mu.Unlock()
	if want := fmt.Sprintf("/tcp/%d active", wide); !strings.HasSuffix(notes, want) || strings.Contains(notes, ", ") {
		t.Fatalf("forwards.changed after the setting was turned off: %q, want one notice ending %q", notes, want)
	}
	w.m.ConfigChanged(api.ConfigChange{Key: api.KeyNetworkPublishLAN, Value: true})
	w.macs(wide, wildcards)
	onLAN("with the setting on again it does", wide, true)
}

func TestWithTheSettingOffFromTheStartEveryForwardIsLoopbackOnly(t *testing.T) {
	w := newWorld(t, Options{Autostart: true}, map[string]any{api.KeyNetworkPublishLAN: false})
	w.boot()
	port := freePort(t)
	w.engine.Run(fakeguest.Spec{Name: "web", Ports: []fakeguest.Port{{HostPort: port, Proto: "tcp", ContainerPort: 80}}})
	w.macs(port, fmt.Sprintf("[127.0.0.1:%d [::1]:%d]", port, port))
	if body, err := fetchAt("::1", port); err != nil || body != "hello from web:80/tcp" {
		t.Fatalf("%q, %v", body, err)
	}
	t.Run("not at the LAN address", func(t *testing.T) {
		lan := lanAddr()
		if lan == "" {
			t.Skip("this Mac has no address but loopback; the LAN address is not exercised")
		}
		if body, err := fetchAt(lan, port); err == nil {
			t.Fatalf("%s:%d served %q", lan, port, body)
		}
	})
}

func (w *world) held(n int) {
	w.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if held, _, _ := w.m.proxy.Counts(); held == n {
			return
		} else if time.Now().After(deadline) {
			w.t.Fatalf("%d connections are held, want %d", held, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func call[T any](w *world, method string) T {
	w.t.Helper()
	result, err := w.methods[method](context.Background(), nil)
	if err != nil {
		w.t.Fatal(err)
	}
	data, _ := json.Marshal(result)
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		w.t.Fatal(err)
	}
	return out
}

func (w *world) waitForwards(want ...string) []api.Forward {
	w.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		list := call[api.Forwards](w, api.MethodForwardsList).Forwards
		var got []string
		for _, f := range list {
			got = append(got, fmt.Sprintf("%s/%s %s", f.Proto, f.Mac[0][strings.LastIndex(f.Mac[0], ":")+1:], f.State))
		}
		sort.Strings(got)
		if reflect.DeepEqual(got, want) || (len(got) == 0 && len(want) == 0) {
			return list
		}
		select {
		case <-w.changed:
		case <-deadline:
			w.t.Fatalf("forwards are %v, want %v", got, want)
		}
	}
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return uint16(l.Addr().(*net.TCPAddr).Port)
}

func fetch(port uint16) (string, error) {
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(body)), err
}

func TestFromAColdStartToAPublishedPort(t *testing.T) {
	w := newWorld(t, Options{Autostart: true})
	if info, err := os.Stat(w.m.Socket()); err != nil || info.Mode().Perm() != 0o600 || filepath.Base(w.m.Socket()) != "docker.sock" {
		t.Fatalf("socket: %v, %v", info, err)
	}
	early := w.later("/_ping")
	w.held(1)
	w.to(api.StateStarting, api.EngineStopped, "")
	during := w.later("/info")
	w.held(2)
	w.to(api.StateStarting, api.EngineStarting, "")
	if fmt.Sprint(w.marks) != "["+MarkSockBound+"]" || w.engine.Stats().Connections != 0 {
		t.Fatalf("before the engine was ready: marks %v, %d engine connections", w.marks, w.engine.Stats().Connections)
	}
	w.to(api.StateRunning, api.EngineReady, "")
	if a := <-early; a.err != nil || a.status != 200 || a.body != "OK" {
		t.Fatalf("the request made before the boot: %+v", a)
	}
	if a := <-during; a.err != nil || a.status != 200 || !strings.Contains(a.body, "fev-fake") {
		t.Fatalf("the request made during the boot: %+v", a)
	}
	if fmt.Sprint(w.marks) != "["+MarkSockBound+" "+boottrace.DockerReleased+"]" {
		t.Fatalf("marks %v", w.marks)
	}
	w.to(api.StateRunning, api.EngineReady, "")
	if len(w.marks) != 2 {
		t.Fatalf("released again: %v", w.marks)
	}

	port := freePort(t)
	id, err := w.engine.Run(fakeguest.Spec{Name: "web", Ports: []fakeguest.Port{{HostPort: port, Proto: "tcp", ContainerPort: 80}}})
	if err != nil {
		t.Fatal(err)
	}
	list := w.waitForwards(fmt.Sprintf("tcp/%d active", port))
	if f := list[0]; f.Source != api.ForwardDocker || f.Owner != id || f.Guest != fmt.Sprintf("127.0.0.1:%d", port) {
		t.Fatalf("%+v", f)
	}
	if body, err := fetch(port); err != nil || body != "hello from web:80/tcp" {
		t.Fatalf("the published port: %q, %v", body, err)
	}
	w.engine.Die(id)
	w.waitForwards()
	if _, err := fetch(port); err == nil {
		t.Fatal("the port still answers after the container died")
	}
	w.mu.Lock()
	notes := strings.Join(w.forwards, ", ")
	w.mu.Unlock()
	if want := list[0].ID + " active, " + list[0].ID + " removed"; notes != want {
		t.Fatalf("forwards.changed: %q, want %q", notes, want)
	}
}

func TestAConflictIsReportedAndTheContainerLeftRunning(t *testing.T) {
	w := newWorld(t, Options{Autostart: true})
	w.boot()
	port := freePort(t)
	other, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	id, _ := w.engine.Run(fakeguest.Spec{Name: "web", Ports: []fakeguest.Port{{HostPort: port, Proto: "tcp", ContainerPort: 80}}})
	list := w.waitForwards(fmt.Sprintf("tcp/%d conflict", port))
	if !strings.Contains(list[0].Error, fmt.Sprintf("port %d/tcp on the Mac is in use by another program", port)) {
		t.Fatalf("%+v", list[0])
	}
	w.mu.Lock()
	notes := strings.Join(w.forwards, ", ")
	w.mu.Unlock()
	if notes != list[0].ID+" conflict" {
		t.Fatalf("forwards.changed: %q", notes)
	}
	if a := w.get("/containers/" + id + "/json"); a.status != 200 || !strings.Contains(a.body, `"Running":true`) {
		t.Fatalf("the container: %+v", a)
	}
	other.Close()
	w.engine.Die(id)
	w.engine.Start(id)
	w.waitForwards(fmt.Sprintf("tcp/%d active", port))
}

func TestWhenTheEngineFailsHeldCommandsAreToldAndALaterStartServes(t *testing.T) {
	w := newWorld(t, Options{Autostart: true})
	w.to(api.StateStarting, api.EngineStarting, "")
	pending := w.later("/_ping")
	w.held(1)
	w.to(api.StateStarting, api.EngineFailed, "the Docker engine failed: exit status 1")
	want := `{"message":"fev: the Docker engine failed: exit status 1"}`
	if a := <-pending; a.err != nil || a.status != 503 || a.body != want {
		t.Fatalf("the held request: %+v", a)
	}
	if a := w.get("/_ping"); a.status != 503 || a.body != want {
		t.Fatalf("a later request: %+v", a)
	}
	w.to(api.StateStarting, api.EngineStarting, "")
	pending = w.later("/_ping")
	w.held(1)
	w.to(api.StateRunning, api.EngineReady, "")
	if a := <-pending; a.err != nil || a.status != 200 {
		t.Fatalf("after the engine came up: %+v", a)
	}
}

func TestAStopThatWasAskedForIsAnAnswerAndAStopOnTheWayToAStartIsAWait(t *testing.T) {
	w := newWorldWith(t, Options{}, func(st *api.Status) { st.StopRequested = true })
	if a := w.get("/_ping"); a.err != nil || a.status != 503 || a.body != `{"message":"fev: the VM is stopped"}` {
		t.Fatalf("%+v", a)
	}
	if w.startCount() != 0 {
		t.Fatalf("a command started the VM %d times after a stop that was asked for", w.startCount())
	}
	w.to(api.StateStarting, api.EngineStopped, "")
	pending := w.later("/_ping")
	w.held(1)
	w.to(api.StateRunning, api.EngineReady, "")
	if a := <-pending; a.status != 200 {
		t.Fatalf("%+v", a)
	}
	w.to(api.StateStopping, api.EngineReady, "")
	pending = w.later("/_ping")
	w.held(1)
	w.to(api.StateStopped, api.EngineStopped, "the VMM exited with status 1")
	if a := <-pending; a.status != 503 || a.body != `{"message":"fev: the VM is stopped: the VMM exited with status 1"}` {
		t.Fatalf("%+v", a)
	}
	if w.startCount() != 0 {
		t.Fatalf("a command started a VM that had failed for good (%d starts)", w.startCount())
	}
}

func TestAnEngineRestartTakesTheForwardsAwayAndBringsThemBack(t *testing.T) {
	w := newWorld(t, Options{Autostart: true})
	w.boot()
	port := freePort(t)
	w.engine.Run(fakeguest.Spec{Name: "web", Ports: []fakeguest.Port{{HostPort: port, Proto: "tcp", ContainerPort: 80}}})
	w.waitForwards(fmt.Sprintf("tcp/%d active", port))
	resp, err := w.client().Get("http://docker/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	w.to(api.StateRunning, api.EngineStopping, "")
	if list := call[api.Forwards](w, api.MethodForwardsList).Forwards; len(list) != 0 {
		t.Fatalf("forwards while the engine is away: %+v", list)
	}
	if _, err := fetch(port); err == nil {
		t.Fatal("the port answers while the engine is away")
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Log("the client's stream ended cleanly")
	}
	pending := w.later("/_ping")
	w.held(1)
	w.to(api.StateRunning, api.EngineStarting, "")
	w.to(api.StateRunning, api.EngineReady, "")
	if a := <-pending; a.status != 200 {
		t.Fatalf("%+v", a)
	}
	w.waitForwards(fmt.Sprintf("tcp/%d active", port))
	if body, err := fetch(port); err != nil || body != "hello from web:80/tcp" {
		t.Fatalf("%q, %v", body, err)
	}
	if len(w.marks) != 3 {
		t.Fatalf("marks %v", w.marks)
	}
}

func TestTheFeedDeliversEveryKindOfEventInOrder(t *testing.T) {
	w := newWorld(t, Options{Autostart: true})
	var mu sync.Mutex
	var got []string
	seen := make(chan struct{}, 16)
	cancel := w.m.Subscribe(func(e engine.Event) {
		mu.Lock()
		got = append(got, e.Type+" "+e.Action+" "+e.ID)
		mu.Unlock()
		seen <- struct{}{}
	})
	w.boot()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(strings.Join(w.engine.Stats().Paths, " "), "GET /containers/json") {
		if time.Now().After(deadline) {
			t.Fatal("the feed never listed the containers")
		}
		time.Sleep(time.Millisecond)
	}
	w.engine.Emit("volume", "create", "pgdata", nil)
	w.engine.Emit("image", "pull", "nginx:latest", map[string]string{"name": "nginx"})
	w.engine.Emit("network", "create", "n1", map[string]string{"name": "appnet"})
	w.engine.Emit("container", "exec_start: sh", "c1", map[string]string{"name": "web"})
	for range 4 {
		select {
		case <-seen:
		case <-time.After(5 * time.Second):
			t.Fatalf("got only %v", got)
		}
	}
	mu.Lock()
	if want := "volume create pgdata, image pull nginx:latest, network create n1, container exec_start c1"; strings.Join(got, ", ") != want {
		t.Fatalf("the feed gave %q, want %q", strings.Join(got, ", "), want)
	}
	mu.Unlock()
	cancel()
	w.engine.Emit("volume", "destroy", "pgdata", nil)
	done := make(chan struct{}, 1)
	w.m.Subscribe(func(e engine.Event) {
		if e.Action == "prune" {
			done <- struct{}{}
		}
	})
	w.engine.Emit("volume", "prune", "x", nil)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("an event arrived after the subscription was cancelled: %v", got)
	}
}

func TestARemovalIsFollowedByOneTrimAndNothingElseByAny(t *testing.T) {
	old := TrimDelay
	TrimDelay = 150 * time.Millisecond
	defer func() { TrimDelay = old }()
	w := newWorld(t, Options{Autostart: true})
	trims := make(chan time.Time, 16)
	w.m.trimCall = func(context.Context, daemon.VM) (*agent.TrimResult, error) {
		trims <- time.Now()
		return &agent.TrimResult{}, nil
	}
	seen := make(chan engine.Event, 64)
	w.m.Subscribe(func(e engine.Event) { seen <- e })
	w.boot()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(strings.Join(w.engine.Stats().Paths, " "), "GET /containers/json") {
		if time.Now().After(deadline) {
			t.Fatal("the feed never listed the containers")
		}
		time.Sleep(time.Millisecond)
	}
	emit := func(kind, action, id string) {
		w.engine.Emit(kind, action, id, nil)
		select {
		case <-seen:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s %s never arrived", kind, action)
		}
	}
	for _, e := range [][2]string{{"image", "pull"}, {"container", "start"}, {"container", "die"}, {"volume", "create"}, {"network", "destroy"}} {
		emit(e[0], e[1], "x")
	}
	select {
	case <-trims:
		t.Fatal("a trim with nothing removed")
	case <-time.After(3 * TrimDelay):
	}
	var last time.Time
	for _, e := range [][2]string{{"image", "untag"}, {"image", "delete"}, {"image", "delete"}, {"volume", "destroy"}, {"container", "destroy"}, {"builder", "prune"}} {
		emit(e[0], e[1], "x")
		last = time.Now()
		time.Sleep(TrimDelay / 5)
	}
	select {
	case at := <-trims:
		if at.Sub(last) < TrimDelay*4/5-TrimDelay/5 {
			t.Errorf("the trim came %v after the last removal, before the delay of %v", at.Sub(last), TrimDelay)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no trim after the removals")
	}
	select {
	case <-trims:
		t.Fatal("a second trim for one burst")
	case <-time.After(3 * TrimDelay):
	}
	emit("image", "delete", "y")
	w.to(api.StateStopping, api.EngineStopping, "")
	select {
	case <-trims:
		t.Fatal("a trim after the engine had gone")
	case <-time.After(3 * TrimDelay):
	}
}

func TestEngineEventsArePublishedToTheDaemonsSubscribers(t *testing.T) {
	w := newWorld(t, Options{Autostart: true})
	seen := make(chan engine.Event, 16)
	w.m.Subscribe(func(e engine.Event) { seen <- e })
	w.boot()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(strings.Join(w.engine.Stats().Paths, " "), "GET /containers/json") {
		if time.Now().After(deadline) {
			t.Fatal("the feed never listed the containers")
		}
		time.Sleep(time.Millisecond)
	}
	w.engine.Emit("container", "health_status: healthy", "c1", map[string]string{"name": "web", "image": "nginx"})
	w.engine.Emit("daemon", "reload", "d", nil)
	w.engine.Emit("network", "connect", "n1", nil)
	w.engine.Emit("volume", "mount", "v1", nil)
	w.engine.Emit("image", "tag", "i1", nil)
	for range 5 {
		select {
		case <-seen:
		case <-time.After(5 * time.Second):
			t.Fatal("the feed stalled")
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var got []string
	for _, e := range w.events {
		got = append(got, e.Type+" "+e.Action+" "+e.ID)
		if _, err := time.Parse(time.RFC3339Nano, e.Time); err != nil {
			t.Errorf("event time %q: %v", e.Time, err)
		}
	}
	if want := "container health_status c1, network connect n1, volume mount v1, image tag i1"; strings.Join(got, ", ") != want {
		t.Fatalf("published %q, want %q", strings.Join(got, ", "), want)
	}
	if e := w.events[0]; e.Detail != "healthy" || e.Name != "web" || e.Attributes["image"] != "nginx" {
		t.Errorf("the first event lost its detail, name or attributes: %+v", e)
	}
}

func TestDockerStatusReportsTheContextAndTheCompatibilityLink(t *testing.T) {
	dockerConfig := filepath.Join(t.TempDir(), "docker")
	w := newWorld(t, Options{Autostart: true, DockerConfig: dockerConfig})
	st := call[api.DockerStatus](w, api.MethodDockerStatus)
	want := api.DockerStatus{
		Engine: api.EngineStopped, Socket: w.m.Socket(),
		Context:      api.DockerContext{Name: "fev", Exists: true, Selected: true, Current: "fev"},
		CompatSocket: api.CompatAbsent,
		ConfigFile:   filepath.Join(w.layout.Home, "config", "docker.json"), ConfigState: api.EngineConfigNone,
	}
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("got  %+v\nwant %+v", st, want)
	}
	data, _ := os.ReadFile(filepath.Join(dockerConfig, "config.json"))
	if !strings.Contains(string(data), `"currentContext": "fev"`) {
		t.Fatalf("config.json: %s", data)
	}
	if _, err := os.Stat(filepath.Join(w.layout.Home, "config", "docker-context.json")); err != nil {
		t.Fatal(err)
	}

	w.m.ConfigChanged(api.ConfigChange{Key: api.KeyDockerSetContext, Value: false})
	st = call[api.DockerStatus](w, api.MethodDockerStatus)
	if st.Context != (api.DockerContext{Name: "fev", Exists: true, Current: "default"}) {
		t.Fatalf("with set_context off: %+v", st.Context)
	}
	w.m.ConfigChanged(api.ConfigChange{Key: api.KeyDockerSetContext, Value: true})
	if st = call[api.DockerStatus](w, api.MethodDockerStatus); !st.Context.Selected {
		t.Fatalf("with set_context on again: %+v", st.Context)
	}
	w.m.ConfigChanged(api.ConfigChange{Key: api.KeyCPU, Value: false})
	if st = call[api.DockerStatus](w, api.MethodDockerStatus); !st.Context.Selected {
		t.Fatalf("after another key changed: %+v", st.Context)
	}

	w.boot()
	os.MkdirAll(filepath.Dir(w.m.opts.CompatLink), 0o755)
	os.Symlink("/Users/someone/.other/run/docker.sock", w.m.opts.CompatLink)
	st = call[api.DockerStatus](w, api.MethodDockerStatus)
	if st.Engine != api.EngineReady || st.CompatSocket != api.CompatElsewhere || st.CompatTarget != "/Users/someone/.other/run/docker.sock" {
		t.Fatalf("%+v", st)
	}
	os.Remove(w.m.opts.CompatLink)
	os.Symlink(w.m.Socket(), w.m.opts.CompatLink)
	if st = call[api.DockerStatus](w, api.MethodDockerStatus); st.CompatSocket != api.CompatLinked || st.CompatTarget != "" {
		t.Fatalf("%+v", st)
	}
	w.m.Close()
	if data, _ := os.ReadFile(filepath.Join(dockerConfig, "config.json")); !strings.Contains(string(data), `"currentContext": "fev"`) {
		t.Fatalf("after Close, config.json: %s", data)
	}
}

func TestWithNoConfigurationDirectoryGivenNoneIsTouched(t *testing.T) {
	scratchHome := t.TempDir()
	t.Setenv("HOME", scratchHome)
	t.Setenv("DOCKER_CONFIG", filepath.Join(scratchHome, "elsewhere"))
	w := newWorld(t, Options{Autostart: true})
	w.m.ConfigChanged(api.ConfigChange{Key: api.KeyDockerSetContext, Value: true})
	w.m.ConfigChanged(api.ConfigChange{Key: api.KeyDockerSetContext, Value: false})
	st := call[api.DockerStatus](w, api.MethodDockerStatus)
	if st.Context != (api.DockerContext{Name: "fev"}) {
		t.Fatalf("%+v", st.Context)
	}
	if entries, _ := os.ReadDir(scratchHome); len(entries) != 0 {
		t.Fatalf("files were written under the home directory: %v", entries)
	}
}

func TestAnIPv6OnlyPortIsReachedOnTheGuestsIPv6Loopback(t *testing.T) {
	w := newWorld(t, Options{Autostart: true})
	w.boot()
	port := freePort(t)
	a, _ := w.engine.Run(fakeguest.Spec{Name: "six", Ports: []fakeguest.Port{{HostIP: "::1", HostPort: port, Proto: "tcp", ContainerPort: 80}}})
	list := w.waitForwards(fmt.Sprintf("tcp/%d active", port))
	if list[0].Guest != fmt.Sprintf("[::1]:%d", port) || list[0].Owner != a {
		t.Fatalf("%+v", list[0])
	}
	if body, err := fetch(port); err != nil || body != "hello from six:80/tcp" {
		t.Fatalf("%q, %v", body, err)
	}
	b, _ := w.engine.Run(fakeguest.Spec{Name: "four", Ports: []fakeguest.Port{{HostIP: "127.0.0.1", HostPort: port, Proto: "tcp", ContainerPort: 80}}})
	list = w.waitForwards(fmt.Sprintf("tcp/%d active", port), fmt.Sprintf("tcp/%d conflict", port))
	for _, f := range list {
		if f.Owner == b && f.State != api.ForwardConflict {
			t.Fatalf("the second container's forward: %+v", f)
		}
	}
	if body, err := fetch(port); err != nil || body != "hello from six:80/tcp" {
		t.Fatalf("%q, %v", body, err)
	}
	if n := w.relay.Opened(fmt.Sprintf("tcp 127.0.0.1:%d", port)); n != 0 {
		t.Fatalf("%d connections were sent to the guest's IPv4 loopback", n)
	}
}

func TestWithTheSettingOffAUsersOwnSelectionStays(t *testing.T) {
	dockerConfig := filepath.Join(t.TempDir(), "docker")
	w := newWorld(t, Options{Autostart: true, DockerConfig: dockerConfig})
	w.m.ConfigChanged(api.ConfigChange{Key: api.KeyDockerSetContext, Value: false})
	if err := os.WriteFile(filepath.Join(dockerConfig, "config.json"), []byte(`{"currentContext":"fev"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w.m.ConfigChanged(api.ConfigChange{Key: api.KeyDockerSetContext, Value: false})
	if st := call[api.DockerStatus](w, api.MethodDockerStatus); !st.Context.Selected {
		t.Fatalf("the user's selection was undone: %+v", st.Context)
	}
}
