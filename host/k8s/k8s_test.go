package k8s

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

func TestFevdLinksNoKubernetesClient(t *testing.T) {
	deps := func(pkg string) []string {
		t.Helper()
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		var k8s []string
		for _, line := range strings.Fields(string(out)) {
			if strings.HasPrefix(line, "k8s.io/") || strings.HasPrefix(line, "sigs.k8s.io/") {
				k8s = append(k8s, line)
			}
		}
		return k8s
	}
	if got := deps("fev/host/cmd/fevd"); len(got) != 0 {
		t.Errorf("fevd links %d Kubernetes packages, among them %v", len(got), got[:min(5, len(got))])
	}
	found := false
	for _, p := range deps("fev/host/cmd/fev-k8s") {
		found = found || p == "k8s.io/client-go/kubernetes"
	}
	if !found {
		t.Error("fev-k8s does not link client-go")
	}
}

type echoBackend struct {
	mu      sync.Mutex
	started Config
	lan     bool
	deliver func(api.K8sChanged)
}

func (b *echoBackend) Start(ctx context.Context, cfg Config, reach func(Reach)) error {
	b.mu.Lock()
	b.started = cfg
	b.mu.Unlock()
	reach(Reach{Watching: true, Forwards: []Forward{{Proto: "tcp", Port: 6443}}})
	return nil
}
func (b *echoBackend) SetLAN(lan bool) { b.mu.Lock(); b.lan = lan; b.mu.Unlock() }
func (b *echoBackend) Watch(ctx context.Context, p api.K8sWatchParams, deliver func(api.K8sChanged)) ([]api.K8sObject, error) {
	deliver(api.K8sChanged{Type: api.K8sChangeAdded, Object: api.K8sObject{Kind: "Pod", Name: "early"}})
	b.mu.Lock()
	b.deliver = deliver
	b.mu.Unlock()
	return []api.K8sObject{{Kind: "Namespace", Name: "default"}}, nil
}
func (b *echoBackend) Get(ctx context.Context, ref api.K8sRef) (api.K8sManifest, error) {
	return api.K8sManifest{}, Errorf(api.CodeConflict, "conflict on %s", ref.Name)
}
func (b *echoBackend) Apply(ctx context.Context, p api.K8sApplyParams) (api.K8sApplyResult, error) {
	return api.K8sApplyResult{}, ErrNotBuilt("k8s.apply")
}
func (b *echoBackend) Update(ctx context.Context, p api.K8sUpdateParams) (api.K8sApplyResult, error) {
	return api.K8sApplyResult{}, nil
}
func (b *echoBackend) Scale(ctx context.Context, p api.K8sScaleParams) (api.K8sObject, error) {
	return api.K8sObject{}, nil
}
func (b *echoBackend) Restart(ctx context.Context, ref api.K8sRef) (api.K8sObject, error) {
	return api.K8sObject{}, nil
}
func (b *echoBackend) Delete(ctx context.Context, ref api.K8sRef) error { return nil }
func (b *echoBackend) Logs(ctx context.Context, p api.K8sLogsParams, text func(string)) error {
	text("one\n")
	text("two\n")
	if p.Follow {
		<-ctx.Done()
		return nil
	}
	return errors.New("the log failed")
}
func (b *echoBackend) ExecTarget(ctx context.Context, p api.K8sExecParams) (string, error) {
	return "c1", nil
}
func (b *echoBackend) Exec(ctx context.Context, p api.K8sExecParams, stream io.ReadWriter, resize <-chan [2]int) (int, error) {
	sz := <-resize
	sc := bufio.NewScanner(stream)
	for sc.Scan() {
		if sc.Text() == "bye" {
			return 7, nil
		}
		io.WriteString(stream, sc.Text()+" "+itoa(sz[0])+"\n")
	}
	return 0, nil
}
func (b *echoBackend) Close() {}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s
}

func serve(t *testing.T, b Backend) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "k8w.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "k8s.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go (&Server{B: b}).Serve(l)
	return path
}

func TestTheWireCarriesTheBackend(t *testing.T) {
	b := &echoBackend{}
	c, err := Dial(context.Background(), serve(t, b))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reached := make(chan Reach, 1)
	if err := c.Start(ctx, Config{Kubeconfig: "k", Relay: "/r", LAN: true}, func(r Reach) { reached <- r }); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-reached:
		if !r.Watching || len(r.Forwards) != 1 {
			t.Errorf("reach %+v", r)
		}
	case <-ctx.Done():
		t.Fatal("no cluster.reach")
	}
	if err := c.SetLAN(ctx, false); err != nil {
		t.Fatal(err)
	}
	changes := make(chan api.K8sChanged, 4)
	objs, stop, err := c.Watch(ctx, api.K8sWatchParams{}, func(ch api.K8sChanged) { changes <- ch })
	if err != nil || len(objs) != 1 {
		t.Fatalf("watch: %v %v", objs, err)
	}
	select {
	case ch := <-changes:
		if ch.Object.Name != "early" || ch.Subscription == 0 {
			t.Errorf("the held change: %+v", ch)
		}
	case <-ctx.Done():
		t.Fatal("the change made before the watch's reply was lost")
	}
	stop()
	_, err = c.Get(ctx, api.K8sRef{Kind: "Pod", Name: "x"})
	if !IsCode(err, api.CodeConflict) {
		t.Errorf("get: %v, want CodeConflict", err)
	}
	_, err = c.Apply(ctx, api.K8sApplyParams{})
	var je *jsonrpc.Error
	if !errors.As(err, &je) || je.Code != api.CodeNotImplemented || !strings.Contains(je.Message, "not built") {
		t.Errorf("an unbuilt call: %v", err)
	}
	var text strings.Builder
	ended := make(chan error, 1)
	if _, err := c.Logs(ctx, api.K8sLogsParams{Namespace: "default", Pod: "p"}, func(s string) { text.WriteString(s) }, func(err error) { ended <- err }); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		if err == nil || text.String() != "one\ntwo\n" {
			t.Errorf("the log: %q, ended %v", text.String(), err)
		}
	case <-ctx.Done():
		t.Fatal("no log end")
	}
	e, err := c.Exec(ctx, api.K8sExecParams{Namespace: "default", Pod: "p", TTY: true, Rows: 40, Cols: 120})
	if err != nil || e.Container != "c1" {
		t.Fatalf("exec: %+v %v", e, err)
	}
	r := bufio.NewReader(e.Stream)
	io.WriteString(e.Stream, "hello\n")
	if line, _ := r.ReadString('\n'); line != "hello 40\n" {
		t.Errorf("the echo said %q", line)
	}
	io.WriteString(e.Stream, "bye\n")
	if x := e.Wait(); x.ExitCode != 7 || x.Error != "" {
		t.Errorf("the exit: %+v", x)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started.Kubeconfig != "k" || b.started.Relay != "/r" || b.lan {
		t.Errorf("the backend was given %+v, lan %v", b.started, b.lan)
	}
}
