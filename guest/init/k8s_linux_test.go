package main

import (
	"bufio"
	"context"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "fev/proto/agent"
	"fev/proto/netplan"

	"fev/guest/internal/logbuf"
)

func TestTheNeverPullNamesTheImageAsContainerdDoes(t *testing.T) {
	for in, want := range map[string]string{
		"web":                                   "docker.io/library/web:latest",
		"web:dev":                               "docker.io/library/web:dev",
		"me/app:1":                              "docker.io/me/app:1",
		"docker.io/library/web:dev":             "docker.io/library/web:dev",
		"ghcr.io/me/app:1":                      "ghcr.io/me/app:1",
		"localhost:5000/app":                    "localhost:5000/app:latest",
		"localhost/app:2":                       "localhost/app:2",
		"registry.example.com:443/a/b":          "registry.example.com:443/a/b:latest",
		"web@sha256:" + strings.Repeat("a", 64): "docker.io/library/web@sha256:" + strings.Repeat("a", 64),
	} {
		if got := normalizeRef(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestTheArchivesMacPathIsReadThroughTheShare(t *testing.T) {
	s := &system{}
	for in, want := range map[string]string{
		"/Users/tester/k3s/airgap.tar.zst": "/mnt/mac/Users/tester/k3s/airgap.tar.zst",
		"/tmp/x/a.tar.zst":                 "/mnt/mac/private/tmp/x/a.tar.zst",
		"/Volumes/USB/a":                   "/mnt/mac/Volumes/USB/a",
		"/Usersx/a":                        "/mnt/mac/Usersx/a",
	} {
		if got := s.macPath(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestTheSupervisorsClusterWordsAreFollowed(t *testing.T) {
	done := make(chan struct{})
	close(done)
	k := &k8sGuest{s: &system{log: logbuf.New(16, func() float64 { return 0 }, nil), ready: done, volDone: done}, changed: make(chan struct{}), sup: supK8s{state: api.K8sOff}, reg: &k8sRegistry{}}
	k.reg.s = k.s
	br := bufio.NewReader(strings.NewReader("k8s off 0 false\ndockerd 7\nk8s importing 0 true\nk8s running 42 true imported_ms=3100\n" +
		"k8s-sysctl kernel/panic 10 0\nk8s-drained 3 2 1450\nk8s error 0 true error=the airgap archive's import failed\nexit 0\n"))
	var r supervisorReport
	states := make(chan string, 8)
	go func() {
		for i := 0; i < 4; i++ {
			s, _ := k.wait(context.Background(), time.Second, "importing", api.K8sRunning, api.K8sError)
			states <- s.state
		}
	}()
	drained := make(chan string, 1)
	k.drained = drained
	for {
		ok, err := readReport(br, &r, k.reported)
		if !ok {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if r.dockerd == 7 && k.sup.state == api.K8sRunning {
			if k.sup.pid != 42 || !k.sup.cri || k.sup.importedMS != 3100 {
				t.Errorf("running: %+v", k.sup)
			}
		}
	}
	if r.dockerd != 7 || !r.exited {
		t.Errorf("dockerd's words: %+v", r)
	}
	if k.sup.state != api.K8sError || k.sup.err != "the airgap archive's import failed" {
		t.Errorf("the last word: %+v", k.sup)
	}
	if st := k.status(false); st.State != api.K8sError || st.Error == "" || st.Kubeconfig != "" {
		t.Errorf("status %+v", st)
	}
	select {
	case d := <-drained:
		if d != "3 2 1450" {
			t.Errorf("drained %q", d)
		}
	default:
		t.Error("the drain's end was not passed on")
	}
	if st := <-states; st != "importing" && st != api.K8sRunning && st != api.K8sError {
		t.Errorf("waited state %q", st)
	}
}

func TestTheRelayTakesTheClustersAddressesOnlyWhileItRuns(t *testing.T) {
	k := &k8sGuest{s: &system{plan: netplan.Default()}, changed: make(chan struct{}), sup: supK8s{state: api.K8sOff}}
	dns := netplan.Default().KubeDNS()
	pod := netip.MustParseAddr("198.19.16.7")
	if k.relayTarget(dns) || k.relayTarget(pod) {
		t.Error("taken while off")
	}
	k.sup.state = api.K8sRunning
	if !k.relayTarget(dns) || !k.relayTarget(pod) {
		t.Error("refused while running")
	}
	if k.relayTarget(netip.MustParseAddr("198.19.8.1")) || k.relayTarget(netip.MustParseAddr("10.0.0.1")) {
		t.Error("an address outside the ranges taken")
	}
}

func TestTheSupervisorsClusterState(t *testing.T) {
	st := &engineStage{}
	k := &st.k8s
	cases := []struct {
		set  func()
		want string
	}{
		{func() { k.want = "off" }, "off"},
		{func() { k.want = "on" }, "starting"},
		{func() { k.importer = 9 }, "importing"},
		{func() { k.importer, k.k3s = 0, 10 }, "starting"},
		{func() { k.k3sUp = true }, "running"},
		{func() { k.k3sStopping = true }, "stopping"},
		{func() {
			k.k3s, k.k3sStopping, k.k3sUp, k.want, k.dirty, k.draining = 0, false, false, "off", true, true
		}, "stopping"},
		{func() { k.draining, k.cleaner = false, 11 }, "stopping"},
		{func() { k.cleaner, k.dirty, st.ctrCRI = 0, false, true }, "stopping"},
		{func() { st.ctrCRI, st.ctr, st.ctrReady = false, 5, true }, "off"},
		{func() { k.want, k.importFailed = "on", true }, "error"},
	}
	for i, c := range cases {
		c.set()
		if got := st.k8sState(); got != c.want {
			t.Errorf("step %d: %q, want %q (%+v)", i, got, c.want, *k)
		}
	}
	*k = k8sStage{want: "off", dirty: true}
	if k.mayRestartContainerd() {
		t.Error("a containerd restart before the cleaner")
	}
	k.dirty = false
	if !k.mayRestartContainerd() {
		t.Error("no restart after the cleaner")
	}
	k.k3s = 3
	if k.mayRestartContainerd() {
		t.Error("a restart under k3s")
	}
}

func TestTheClustersScriptsAreShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for name, script := range map[string]string{"cleaner": k8sCleanScript, "purger": k8sPurgeScript} {
		if out, err := exec.Command(sh, "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%s: %v: %s", name, err, out)
		}
	}
}

func TestThePurgerRunsToItsEnd(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	if _, err := os.Stat(k3sUnpacked); err == nil {
		t.Skip("a k3s payload is here: the purger would act on it")
	}
	out, err := exec.Command(sh, "-c", k8sPurgeScript).CombinedOutput()
	if err != nil || strings.Contains(string(out), "not set") {
		t.Errorf("the purger: %v: %s", err, out)
	}
}

func TestAK3sCrashLoopIsAnError(t *testing.T) {
	st := &engineStage{events: make(chan string, 16)}
	k := &st.k8s
	k.want = "on"
	for i := 1; i <= 5; i++ {
		k.k3s, k.k3sHeld = 100+i, false
		st.k8sReaped(100+i, 0x100)
		if i < 5 && (k.importFailed || !k.k3sHeld) {
			t.Fatalf("end %d: failed %v held %v", i, k.importFailed, k.k3sHeld)
		}
	}
	if !k.importFailed || st.k8sState() != "error" || !strings.Contains(k.failure, "k3s ended 5 times in 1m0s") {
		t.Fatalf("after five ends: %v %q %q", k.importFailed, st.k8sState(), k.failure)
	}
	k.importFailed, k.failure, k.ends = false, "", nil
	if st.k8sState() != "starting" {
		t.Errorf("after a new start: %q", st.k8sState())
	}
}

func TestAResetSeesThePurgeItWasToldOf(t *testing.T) {
	done := make(chan struct{})
	close(done)
	k := &k8sGuest{s: &system{log: logbuf.New(16, func() float64 { return 0 }, nil), ready: done, volDone: done}, changed: make(chan struct{}), sup: supK8s{state: api.K8sStopping}, reg: &k8sRegistry{}}
	k.reg.s = k.s
	from := k.purges
	k.reported(reportK8s, "purged 0 true")
	k.reported(reportK8s, "stopping 0 true")
	k.reported(reportK8s, "off 0 false")
	if err := k.waitPurged(from, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := k.waitPurged(k.purges, 50*time.Millisecond); err == nil {
		t.Error("a purge that was not told was seen")
	}
}

func TestTheWantsThatKeepTheCRI(t *testing.T) {
	for want, cri := range map[string]bool{"on": true, "cri": true, "off-cri": true, "reset-cri": true, "off": false, "reset": false} {
		st := &engineStage{}
		st.k8s.want = want
		if got := st.wantCRI(); got != cri {
			t.Errorf("want %q: containerd's CRI %v, want %v", want, got, cri)
		}
		st.stopping = true
		if st.wantCRI() {
			t.Errorf("want %q: the CRI kept through the engine's own stop", want)
		}
	}
	for want, state := range map[string]string{"cri": "off", "off-cri": "off", "reset-cri": "stopping", "off": "stopping"} {
		st := &engineStage{ctrCRI: true, ctr: 5, ctrReady: true}
		st.k8s.want = want
		if got := st.k8sState(); got != state {
			t.Errorf("want %q with the CRI on: %q, want %q", want, got, state)
		}
	}
	st := &engineStage{ctrCRI: true, ctr: 5, ctrReady: true}
	st.k8s.want, st.k8s.purger = "reset-cri", 77
	st.k8sReaped(77, 0)
	if st.k8s.want != "cri" {
		t.Errorf("a reset keeping the CRI ended in %q", st.k8s.want)
	}
	st.k8s.want, st.k8s.purger = "reset", 78
	st.k8sReaped(78, 0)
	if st.k8s.want != "off" {
		t.Errorf("a reset ended in %q", st.k8s.want)
	}
}

func TestTheClaimsAreBoundInsideTheK3sPart(t *testing.T) {
	k := &k8sGuest{}
	k3s, storage := -1, -1
	for i, b := range k.binds() {
		switch b.target {
		case "/var/lib/rancher/k3s":
			k3s = i
		case "/var/lib/rancher/k3s/storage":
			storage = i
			if b.source != k8sData+"/storage" || b.ro {
				t.Errorf("the claims' bind %+v", b)
			}
		}
	}
	if k3s < 0 || storage < k3s {
		t.Fatalf("the k3s part at %d, the claims at %d", k3s, storage)
	}
	u := k8sUnbindTargets()
	at := map[string]int{}
	for i, t := range u {
		at[t] = i
	}
	if _, ok := at["/var/lib/rancher/k3s/storage"]; !ok || at["/var/lib/rancher/k3s/storage"] > at["/var/lib/rancher/k3s"] {
		t.Errorf("unbound in the order %v", u)
	}
	payload := false
	for _, b := range k.binds() {
		if b.target == k3sUnpacked {
			payload = b.source == k8sCache+"/k3s/data" && b.ro
		}
	}
	if !payload {
		t.Errorf("no read-only bind of the cache's payload at %s: %+v", k3sUnpacked, k.binds())
	}
	if _, ok := at[k3sUnpacked]; !ok {
		t.Errorf("the payload not taken away at a stop: %v", u)
	}
}

func TestTheCRIToggleEndsOnceContainerdServes(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "report")
	if err != nil {
		t.Fatal(err)
	}
	st := &engineStage{report: f, ctr: 5, ctrCRI: true}
	st.k8s.want = "cri"
	st.k8s.tell(st)
	st.ctrReady = true
	st.k8s.tell(st)
	data, _ := os.ReadFile(f.Name())
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || strings.Contains(lines[0], "serving") || !strings.HasSuffix(lines[1], " serving") {
		t.Fatalf("the supervisor said %q", lines)
	}
	done := make(chan struct{})
	close(done)
	k := &k8sGuest{s: &system{log: logbuf.New(16, func() float64 { return 0 }, nil), ready: done, volDone: done}, changed: make(chan struct{}), sup: supK8s{state: api.K8sOff}, reg: &k8sRegistry{}}
	k.reg.s = k.s
	mark := k.mark()
	k.reported(reportK8s, strings.TrimPrefix(lines[0], reportK8s+" "))
	if err := k.waitCRI(mark, true, 200*time.Millisecond); err == nil {
		t.Fatal("the toggle ended at containerd's spawn, before it served")
	}
	k.reported(reportK8s, strings.TrimPrefix(lines[1], reportK8s+" "))
	if err := k.waitCRI(mark, true, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestAStopWaitsForContainerdToServeAgain(t *testing.T) {
	done := make(chan struct{})
	close(done)
	k := &k8sGuest{s: &system{log: logbuf.New(16, func() float64 { return 0 }, nil), ready: done, volDone: done}, changed: make(chan struct{}), sup: supK8s{state: api.K8sOff}, reg: &k8sRegistry{}}
	k.reg.s = k.s
	k.reported(reportK8s, "off 0 false")
	if err := k.waitServing(100 * time.Millisecond); err == nil {
		t.Fatal("a containerd just spawned was taken as serving")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		k.reported(reportK8s, "off 0 false serving")
	}()
	if err := k.waitServing(2 * time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestTheClustersIdleCostsAreHeld(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "manifests")
	if err := writeK8sManifests(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gateway-api-crd.yaml.skip", "fev-local-storage.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("the manifests lack %s: %v", name, err)
		}
	}
}
