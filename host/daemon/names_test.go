package daemon

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"fev/host/docker/engine"
	"fev/host/trust"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/netplan"
	priv "fev/proto/privhelper"
)

type sentSets struct {
	mu   sync.Mutex
	sets []agent.DomainsParams
	hold chan struct{}
	fail bool
}

func (s *sentSets) send(_ context.Context, _ *agent.Client, p agent.DomainsParams) error {
	s.mu.Lock()
	s.sets = append(s.sets, p)
	hold, fail := s.hold, s.fail
	s.hold = nil
	s.mu.Unlock()
	if hold != nil {
		<-hold
	}
	if fail {
		return errors.New("refused")
	}
	return nil
}

func (s *sentSets) all() []agent.DomainsParams {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sets)
}

func (s *sentSets) wait(t *testing.T, n int) []agent.DomainsParams {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := s.all()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d domains.set calls, want %d", len(got), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func container(id, name string, n byte, labels map[string]string) *engine.Container {
	return &engine.Container{ID: id, Name: name, Running: true, Labels: labels, NetworkMode: "bridge",
		Networks: []engine.Network{{Name: "bridge", Address: netip.AddrFrom4([4]byte{198, 19, 2, n})}}}
}

func TestDomainsSetIsCoalescedAndItsSeqIsEachBoots(t *testing.T) {
	f := newFixture(t)
	sent := &sentSets{hold: make(chan struct{})}
	release := sent.hold
	n := &Names{AddressFile: filepath.Join(f.layout.Home, "names.json"), domainsSet: sent.send}
	f.opts.Modules = []Module{n, &Domains{Source: n}}
	f.start()
	f.client().VMStart(bg, api.StartParams{})
	first := sent.wait(t, 1)[0]
	if first.Seq != 1 || len(first.Targets) != 0 || first.Index != "198.19.4.1" {
		t.Errorf("the boot's first set: %+v", first)
	}
	feed := n.Feed()
	for i := byte(1); i <= 5; i++ {
		feed.Container("c"+string('0'+rune(i)), container("c"+string('0'+rune(i)), "web"+string('0'+rune(i)), 10+i, nil))
	}
	time.Sleep(50 * time.Millisecond)
	if got := len(sent.all()); got != 1 {
		t.Errorf("%d calls while the first was out, want 1", got)
	}
	close(release)
	got := sent.wait(t, 2)
	time.Sleep(100 * time.Millisecond)
	got = sent.all()
	if len(got) != 2 || got[1].Seq != 2 || len(got[1].Targets) != 5 {
		t.Fatalf("after the burst: %d calls, the last %+v", len(got), got[len(got)-1])
	}
	for _, tg := range got[1].Targets {
		if a, _ := netip.ParseAddr(tg.Address); !netip.MustParsePrefix("198.19.4.0/22").Contains(a) || tg.Target == "" || tg.Kind != agent.TargetContainer {
			t.Errorf("target %+v", tg)
		}
	}
	feed.Container("c1", nil)
	eventually(t, "the set without c1", func() bool {
		got := sent.all()
		last := got[len(got)-1]
		return last.Seq >= 3 && last.Seq == uint64(len(got)) && len(last.Targets) == 4
	})
	f.client().VMStop(bg, api.StopParams{})
	f.stopped()
	before := len(sent.all())
	f.client().VMStart(bg, api.StartParams{})
	got = sent.wait(t, before+1)
	if got[before].Seq != 1 {
		t.Errorf("the next boot's first set has Seq %d", got[before].Seq)
	}
}

func TestDomainsSetNotAgainAtAnEngineReady(t *testing.T) {
	f := newFixture(t)
	sent := &sentSets{}
	n := &Names{AddressFile: filepath.Join(f.layout.Home, "names.json"), domainsSet: sent.send}
	f.opts.Modules = []Module{n, &Domains{Source: n}}
	f.start()
	f.client().VMStart(bg, api.StartParams{})
	f.until("the engine ready", func(st api.Status) bool { return st.State == api.StateRunning && st.Engine == api.EngineReady })
	sent.wait(t, 1)
	restart := func() {
		t.Helper()
		vm, ok := f.d.sup.VM()
		if !ok {
			t.Fatal("no VM")
		}
		if _, err := vm.Agent().EngineRestart(bg, agent.EngineStopParams{}); err != nil {
			t.Fatal(err)
		}
		f.until("the engine on its way", func(st api.Status) bool { return st.Engine != api.EngineReady })
		f.until("the engine ready again", func(st api.Status) bool { return st.Engine == api.EngineReady })
		time.Sleep(100 * time.Millisecond)
	}
	restart()
	if got := sent.all(); len(got) != 1 {
		t.Fatalf("%d domains.set calls after the boot and an engine restart, want 1: %+v", len(got), got)
	}
	sent.mu.Lock()
	sent.fail = true
	sent.mu.Unlock()
	n.sendDomains()
	sent.wait(t, 2)
	time.Sleep(50 * time.Millisecond)
	sent.mu.Lock()
	sent.fail = false
	sent.mu.Unlock()
	restart()
	got := sent.all()
	if len(got) != 3 || got[2].Seq != 3 {
		t.Fatalf("after a failed call and an engine restart: %d calls, want 3 (the last Seq 3): %+v", len(got), got)
	}
}

func TestNamesFollowTheHelpersHelloAndLocalNames(t *testing.T) {
	f, h := namesDirect(t, "answered=nas.local")
	names := f.opts.Modules[1].(*Names)
	f.client().VMStart(bg, api.StartParams{})
	f.running()
	st := f.domains(func(d *api.DomainsStatus) bool { return d.Port == h.Port })
	if st.Resolver != priv.StateFev {
		t.Errorf("the resolver's state: %+v", st)
	}
	feed := names.Feed()
	feed.Container("a1", container("a1", "web", 7, nil))
	feed.Container("a2", container("a2", "shop", 8, map[string]string{agent.LabelDomains: "shop-custom.local,*.files-custom.local"}))
	feed.Container("a3", container("a3", "files", 9, map[string]string{agent.LabelDomains: "nas.local"}))
	feed.Container("a4", container("a4", "many", 10, map[string]string{agent.LabelDomains: "n1.fev.local,n2.fev.local"}))
	ip := lookup(t, h.Port, "web.fev.local")
	if ip != "198.19.4.2" && !strings.HasPrefix(ip, "198.19.4.") {
		t.Errorf("web.fev.local is %q", ip)
	}
	eventually(t, "custom names sent", func() bool {
		return slices.Equal(h.Names(), []string{"files-custom.local", "shop-custom.local"})
	})
	d := f.domains(func(d *api.DomainsStatus) bool { return len(d.Local) == 3 })
	for _, l := range d.Local {
		want := priv.NamePresent
		if l.Name == "nas.local" {
			want = priv.NameRefused
		}
		if l.State != want {
			t.Errorf("%s: %+v", l.Name, l)
		}
	}
	f.awaitWarning(true, "the refused name's warning", "nas.local", "files", "192.0.2.7")
	feed.Container("a2", nil)
	eventually(t, "a name gone with its container", func() bool { return len(h.Names()) == 0 })
	feed.Container("a2", container("a2", "shop", 8, map[string]string{agent.LabelDomains: "shop-custom.local"}))
	eventually(t, "back", func() bool { return len(h.Names()) == 1 })
	feed.Container("a3", nil)
	f.awaitWarning(false, "the refused name's warning gone with its container", "nas.local")
	f.d.Close()
	if got := h.Names(); len(got) != 0 {
		t.Errorf("entries left at a clean exit: %v", got)
	}
}

func TestCustomNamesWaitWithoutAHelper(t *testing.T) {
	f := newFixture(t)
	n := &Names{AddressFile: filepath.Join(f.layout.Home, "names.json")}
	f.opts.Modules = []Module{n, &Domains{Source: n}}
	f.start()
	n.Feed().Container("a2", container("a2", "shop", 8, map[string]string{agent.LabelDomains: "shop-custom.local"}))
	d := f.domains(func(d *api.DomainsStatus) bool { return len(d.Local) == 1 })
	if d.Local[0].State != api.LocalWaiting || d.Port != 0 || d.Resolver != priv.StateAbsent {
		t.Errorf("without a helper: %+v", d)
	}
}

type fileCA struct{ ca *trust.CA }

func (c fileCA) CA() (*trust.CA, error) { return c.ca, nil }
func (c fileCA) RootPEM() string        { return c.ca.RootPEM() }

func issue(t *testing.T, n *Names, requester string, names ...string) string {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: names}, k)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(agent.TLSIssueRequest{CSR: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), Requester: requester})
	a, b := net.Pipe()
	defer b.Close()
	go n.issuer().Serve(a)
	go b.Write(append(req, '\n'))
	b.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(b).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var out agent.TLSIssued
	if err := json.Unmarshal(line, &out); err != nil {
		t.Fatal(err)
	}
	if out.Error == "" && out.Certificate == "" {
		t.Fatalf("neither a leaf nor an error: %s", line)
	}
	return out.Error
}

func TestALabelNameIsSignedOnlyWhileTheMacHasIt(t *testing.T) {
	f, h := namesDirect(t, "answered=nas.local")
	names := f.opts.Modules[1].(*Names)
	ca, err := trust.OpenFileCA(t.TempDir(), trust.RootOptions{Constrain: true})
	if err != nil {
		t.Fatal(err)
	}
	names.CA = fileCA{ca}
	f.client().VMStart(bg, api.StartParams{})
	f.running()
	feed := names.Feed()
	feed.Container("a1", container("a1", "web", 7, nil))
	feed.Container("a2", container("a2", "shop", 8, map[string]string{agent.LabelDomains: "shop-custom.local,*.files-custom.local"}))
	feed.Container("a3", container("a3", "files", 9, map[string]string{agent.LabelDomains: "nas.local"}))
	f.domains(func(d *api.DomainsStatus) bool {
		got := map[string]string{}
		for _, l := range d.Local {
			got[l.Name] = l.State
		}
		return got["shop-custom.local"] == priv.NamePresent && got["files-custom.local"] == priv.NamePresent && got["nas.local"] == priv.NameRefused
	})
	if got := h.Names(); !slices.Contains(got, "shop-custom.local") || slices.Contains(got, "nas.local") {
		t.Fatalf("the helper holds %v", got)
	}
	if e := issue(t, names, "a3", "nas.local"); e == "" || !strings.Contains(e, "not on this Mac") {
		t.Errorf("nas.local, which a device on the LAN answers, for its container: %q", e)
	}
	if e := issue(t, names, "a3", "files.fev.local"); e != "" {
		t.Errorf("the same container's fev.local name: %q", e)
	}
	if e := issue(t, names, "a2", "shop-custom.local", "api.files-custom.local", "files-custom.local", "shop.fev.local"); e != "" {
		t.Errorf("names held on the Mac: %q", e)
	}
	if e := issue(t, names, "a1", "web.fev.local"); e != "" {
		t.Errorf("a fev.local name: %q", e)
	}
	if e := issue(t, names, "", "nas.local"); e != "" {
		t.Errorf("the web front's own leaf: %q", e)
	}
	if _, err := f.client().ConfigSet(bg, map[string]any{api.KeyAdminEnabled: false}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the helper let go", func() bool {
		names.mu.Lock()
		defer names.mu.Unlock()
		return !names.helper
	})
	if e := issue(t, names, "a2", "shop-custom.local"); e == "" {
		t.Error("a label name signed with no helper")
	}
	if e := issue(t, names, "a2", "shop.fev.local"); e != "" {
		t.Errorf("a fev.local name with no helper: %q", e)
	}

	f2, _ := namesDirect(t, "refuse=localnames.set")
	names2 := f2.opts.Modules[1].(*Names)
	names2.CA = fileCA{ca}
	f2.client().VMStart(bg, api.StartParams{})
	f2.running()
	names2.Feed().Container("a2", container("a2", "shop", 8, map[string]string{agent.LabelDomains: "shop-custom.local"}))
	f2.domains(func(d *api.DomainsStatus) bool { return len(d.Local) == 1 && d.Local[0].State == api.LocalWaiting })
	if e := issue(t, names2, "a2", "shop-custom.local"); !strings.Contains(e, "ask again") {
		t.Errorf("a name waiting for the helper: %q", e)
	}
	if e := issue(t, names, "a3", "nas.local"); strings.Contains(e, "ask again") {
		t.Errorf("a refused name is told to ask again: %q", e)
	}
}

func lookup(t *testing.T, port int, name string) string {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	c := &dns.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, _, err := c.Exchange(m, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil && len(r.Answer) > 0 {
			if a, ok := r.Answer[0].(*dns.A); ok {
				return a.A.String()
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s on 127.0.0.1:%d: %v %v", name, port, r, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func namesDirect(t *testing.T, words string) (*fixture, *StandInHelper) {
	t.Helper()
	f := newFixture(t)
	h, err := NewStandInHelper(words)
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(f.layout.RunDir(), "ph.sock")
	os.MkdirAll(filepath.Dir(sock), 0o700)
	if err := h.Listen(sock); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	nw := &Network{Helper: SocketHelper{Path: sock}, Tunnel: &fakeSplice{}}
	n := &Names{AddressFile: filepath.Join(f.layout.Home, "names.json"), Network: nw, domainsSet: (&sentSets{}).send}
	f.opts.Modules = []Module{nw, n, &Domains{Source: n}}
	f.start()
	return f, h
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *fixture) domains(ok func(*api.DomainsStatus) bool) *api.DomainsStatus {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := f.d.hub.Status()
		if st.Domains != nil && ok(st.Domains) {
			return st.Domains
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("the domains block: %+v", st.Domains)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEngineNetworksInsideTheBaseAreRouted(t *testing.T) {
	f, h := namesDirect(t, "")
	names := f.opts.Modules[1].(*Names)
	f.client().VMStart(bg, api.StartParams{})
	f.running()
	eventually(t, "the plan's routes", func() bool { return len(h.Routes()) == 5 })
	on := func(id string, prefix string, host byte) *engine.Container {
		p := netip.MustParsePrefix(prefix)
		a := p.Addr().As4()
		a[3] = host
		return &engine.Container{ID: id, Name: id, Running: true, NetworkMode: "custom",
			Networks: []engine.Network{{Name: "custom", Address: netip.AddrFrom4(a), Prefix: p}}}
	}
	feed := names.Feed()
	feed.Container("u1", on("u1", "198.19.10.0/24", 2))
	feed.Container("p1", on("p1", "198.19.65.0/24", 2))
	feed.Container("o1", on("o1", "10.123.0.0/24", 2))
	feed.Container("t1", on("t1", "198.19.3.0/24", 9))
	feed.Container("l1", on("l1", "198.19.1.0/25", 9))
	eventually(t, "the user network routed", func() bool { return slices.Contains(h.Routes(), "198.19.10.0/24") })
	time.Sleep(300 * time.Millisecond)
	if r := h.Routes(); len(r) != 6 || slices.Contains(r, "198.19.65.0/24") || slices.Contains(r, "10.123.0.0/24") {
		t.Errorf("routes %v", r)
	}
	feed.Container("u1", nil)
	eventually(t, "the user network's route gone", func() bool { return !slices.Contains(h.Routes(), "198.19.10.0/24") })
}

func TestAMachineShadowedByAContainerIsLoggedOncePerChange(t *testing.T) {
	f := newFixture(t)
	n := &Names{AddressFile: filepath.Join(f.layout.Home, "names.json")}
	f.opts.Modules = []Module{n, &Domains{Source: n}}
	f.start()
	feed := n.Feed()
	n.Machine(agent.MachineState{MachineInfo: agent.MachineInfo{Name: "dev", State: agent.MachineRunning, Address: "198.19.0.12"}})
	feed.Container("c1", container("c1", "dev", 7, nil))
	count := func() int {
		return strings.Count(f.logFile("fevd.log"), "a container holds a machine's name")
	}
	deadline := time.Now().Add(5 * time.Second)
	for count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if count() != 1 {
		t.Fatalf("%d lines for the shadowed machine, want 1:\n%s", count(), f.logFile("fevd.log"))
	}
	if a := n.Registry().Lookup("dev.fev.local"); !a.Found || a.Target != "container dev" {
		t.Errorf("dev.fev.local is %+v, want the container's", a)
	}
	feed.Container("c2", container("c2", "web", 8, nil))
	feed.Container("c3", container("c3", "api", 9, nil))
	time.Sleep(50 * time.Millisecond)
	if count() != 1 {
		t.Errorf("%d lines after other changes, want 1", count())
	}
	feed.Container("c1", nil)
	deadline = time.Now().Add(5 * time.Second)
	for !strings.Contains(f.logFile("fevd.log"), "no machine's name is held by a container") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(f.logFile("fevd.log"), "no machine's name is held by a container") {
		t.Error("the end of the shadow is not said")
	}
	if a := n.Registry().Lookup("dev.fev.local"); a.Target != "machine dev" {
		t.Errorf("dev.fev.local is %+v after the container went, want the machine's", a)
	}
}

func aaaaOf(t *testing.T, port int, name string) []string {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeAAAA)
	r, _, err := (&dns.Client{Timeout: 2 * time.Second}).Exchange(m, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("%s AAAA: %v", name, err)
	}
	var out []string
	for _, rr := range r.Answer {
		if a, ok := rr.(*dns.AAAA); ok {
			out = append(out, a.AAAA.String())
		}
	}
	return out
}

func TestTheMacIsAnsweredAAAAOnceTheTunnelCarriesIPv6(t *testing.T) {
	f := newFixture(t)
	h, err := NewStandInHelper("")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(f.layout.RunDir(), "ph.sock")
	os.MkdirAll(filepath.Dir(sock), 0o700)
	if err := h.Listen(sock); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	nw := &Network{Helper: SocketHelper{Path: sock}, Tunnel: &fakeSplice{}}
	n := &Names{AddressFile: filepath.Join(f.layout.Home, "names.json"), Network: nw, domainsSet: (&sentSets{}).send}
	plan := &Plan{Recorded6: h.Recorded6, Fresh: func() bool { return true }}
	f.opts.Modules = []Module{plan, nw, n, &Domains{Source: n}}
	f.start()
	f.client().VMStart(bg, api.StartParams{})
	f.running()
	f.network(func(b *api.NetworkStatus) bool { return b.Address6 != "" })
	f.domains(func(d *api.DomainsStatus) bool { return d.Port == h.Port })
	n.Feed().Container("a1", container("a1", "web", 7, nil))
	web := lookup(t, h.Port, "web.fev.local")
	p6, _ := netplan.New6(StandInBase6)
	want := p6.Name6(netip.MustParseAddr(web)).String()
	eventually(t, "web's AAAA", func() bool { return slices.Equal(aaaaOf(t, h.Port, "web.fev.local"), []string{want}) })
	if got := aaaaOf(t, h.Port, "fev.local"); !slices.Equal(got, []string{p6.Name6(netplan.Default().Index()).String()}) {
		t.Errorf("fev.local's AAAA: %v", got)
	}
	if _, err := f.client().ConfigSet(bg, map[string]any{api.KeyNetworkDirectAccess: false}); err != nil {
		t.Fatal(err)
	}
	f.network(func(b *api.NetworkStatus) bool { return b.DirectAccess == api.DirectOff })
	if got := aaaaOf(t, h.Port, "web.fev.local"); len(got) != 0 {
		t.Errorf("an AAAA with the tunnel closed: %v", got)
	}
}
