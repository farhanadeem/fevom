package images

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fev/host/docker/vsock"
	"fev/proto/agent"
	"fev/proto/jsonrpc"
	"fev/proto/relay"
)

type fakeGuest struct {
	t      *testing.T
	kept   []agent.ImageInfo
	relay  string
	client *agent.Client

	mu      sync.Mutex
	created *agent.MachineCreateParams
	tars    [][]byte
	refuse  string
	cancels []uint64
	done    chan struct{}
}

const token = "0a1b2c3d4e5f60718293a4b5c6d7e8f9"

func newFakeGuest(t *testing.T, kept []agent.ImageInfo) *fakeGuest {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bv.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	g := &fakeGuest{t: t, kept: kept, relay: filepath.Join(dir, "vsock_1026.sock"), done: make(chan struct{}, 4)}
	l, err := net.Listen("unix", g.relay)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	host, guest := net.Pipe()
	t.Cleanup(func() { host.Close(); guest.Close() })
	var conn *jsonrpc.Conn
	conn = jsonrpc.NewConn(guest, jsonrpc.HandlerFunc(func(ctx context.Context, c *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
		switch method {
		case agent.MethodHello:
			return agent.HelloResult{APIVersion: agent.APIVersion, AgentVersion: "dev", Kernel: "7.2.9"}, nil
		case agent.MethodMachineList:
			return agent.MachineList{Machines: []agent.MachineInfo{}, Images: g.kept}, nil
		case agent.MethodMachineCreate:
			p, err := jsonrpc.Bind[agent.MachineCreateParams](params)
			if err != nil {
				return nil, err
			}
			g.mu.Lock()
			g.created = &p
			g.mu.Unlock()
			op := agent.Operation{ID: 3}
			if p.Image.Templates != nil {
				op.Token = token
			} else {
				jsonrpc.AfterReply(ctx, func() { g.finish(c, 0) })
			}
			return op, nil
		case agent.MethodOperationCancel:
			p, _ := jsonrpc.Bind[agent.OperationID](params)
			g.mu.Lock()
			g.cancels = append(g.cancels, p.ID)
			g.mu.Unlock()
			return agent.Empty{}, nil
		}
		return nil, jsonrpc.Errorf(agent.CodeNotImplemented, "%s", method)
	}))
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go g.stream(c, conn)
		}
	}()
	client, _, err := agent.NewClient(t.Context(), host, "test")
	if err != nil {
		t.Fatal(err)
	}
	g.client = client
	return g
}

func (g *fakeGuest) stream(c net.Conn, conn *jsonrpc.Conn) {
	defer c.Close()
	req, err := relay.ReadRequest(c)
	if err != nil || req.Kind != relay.Stream || req.Target != token {
		relay.WriteError(c, relay.CodeDenied, "no such stream")
		return
	}
	g.mu.Lock()
	refuse := g.refuse
	g.mu.Unlock()
	if refuse != "" {
		relay.WriteError(c, relay.CodeRefused, refuse)
		return
	}
	relay.WriteOK(c)
	data, err := io.ReadAll(c)
	if err != nil {
		g.t.Errorf("the guest's read of the image: %v", err)
	}
	g.mu.Lock()
	g.tars = append(g.tars, data)
	g.mu.Unlock()
	g.finish(conn, int64(len(data)))
}

func (g *fakeGuest) finish(c *jsonrpc.Conn, n int64) {
	c.Notify(agent.NotifyOperationProgress, agent.OperationProgress{ID: 3, Method: agent.MethodMachineCreate, State: agent.OperationRunning, Phase: agent.PhaseImage, DoneBytes: n})
	result, _ := json.Marshal(agent.MachineCreated{Machine: agent.MachineInfo{Name: "u", State: agent.MachineRunning}})
	c.Notify(agent.NotifyOperationProgress, agent.OperationProgress{ID: 3, Method: agent.MethodMachineCreate, State: agent.OperationDone, Phase: agent.PhaseBoot, Result: result})
}

func (g *fakeGuest) open(ctx context.Context, tok string) (Stream, error) {
	return vsock.Relay{Path: g.relay}.Open(ctx, relay.Request{Kind: relay.Stream, Target: tok})
}

type progress struct {
	mu   sync.Mutex
	all  []agent.OperationProgress
	done chan struct{}
}

func newProgress() *progress { return &progress{done: make(chan struct{})} }

func (p *progress) note(op agent.OperationProgress) {
	p.mu.Lock()
	p.all = append(p.all, op)
	p.mu.Unlock()
	if op.State != agent.OperationRunning {
		close(p.done)
	}
}

func (p *progress) wait(t *testing.T) []agent.OperationProgress {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the operation never ended")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.all
}

func TestACreateSendsTheCheckedImageToTheGuestOnTheOperationsStream(t *testing.T) {
	key := newSigner(t)
	srv := newServer(t)
	b := makeBuild(t, key, ref("fedora", "44", "default", "20261013_20:33"), fixture(t, "rootfs-blocks.tar.xz"), now.Add(-14*time.Hour))
	srv.publish(b)
	srv.index(t, key, b)
	src := srv.source(t, key, now)
	g := newFakeGuest(t, nil)

	p := newProgress()
	params := agent.MachineCreateParams{Name: "f", User: agent.MachineUser{Name: "ada", UID: 501}, Config: agent.MachineConfig{User: "ada", MAC: "52:54:00:12:34:56", Address: "198.19.249.10"}}
	op, prep, sent, err := src.Create(t.Context(), g.client, g.open, "fedora", "", "", params, nil, p.note)
	if err != nil {
		t.Fatal(err)
	}
	all := p.wait(t)
	if op.Token != token || prep.Kept || sent.TarBytes != int64(len(rootfsTar())) || !sent.Parallel {
		t.Fatalf("op %+v, kept %v, sent %+v", op, prep.Kept, sent)
	}
	g.mu.Lock()
	created, tars := g.created, g.tars
	g.mu.Unlock()
	if len(tars) != 1 || sha256.Sum256(tars[0]) != sha256.Sum256(rootfsTar()) {
		t.Fatalf("the guest read %d streams; the first is not the tar", len(tars))
	}
	if created.Image.ImageRef != b.ref || len(created.Image.Templates) != 2 || created.Name != "f" || created.User.UID != 501 || created.Config.Address != "198.19.249.10" {
		t.Errorf("machine.create carried %+v", created)
	}
	if last := all[len(all)-1]; last.State != agent.OperationDone || len(last.Result) == 0 {
		t.Errorf("the operation's end: %+v", last)
	}
	if left := tempFiles(t, src); len(left) != 0 || prep.Build.rootfs != nil {
		t.Errorf("the build's file outlived the send: %q", left)
	}

	g.kept = []agent.ImageInfo{{ImageRef: b.ref, DiskBytes: 600 << 20}}
	before := srv.got(b.path() + "rootfs.tar.xz")
	p = newProgress()
	op, prep, sent, err = src.Create(t.Context(), g.client, g.open, "fedora", "44", "", params, nil, p.note)
	if err != nil {
		t.Fatal(err)
	}
	p.wait(t)
	g.mu.Lock()
	created, tars = g.created, g.tars
	g.mu.Unlock()
	if op.Token != "" || !prep.Kept || sent.TarBytes != 0 || len(tars) != 1 || created.Image.Templates != nil || created.Image.Serial != b.ref.Serial {
		t.Errorf("a create from the guest's build: op %+v, kept %v, sent %+v, streams %d, image %+v", op, prep.Kept, sent, len(tars), created.Image)
	}
	if after := srv.got(b.path() + "rootfs.tar.xz"); after != before {
		t.Errorf("the rootfs was fetched again for a build the guest keeps")
	}
}

func TestACreateWhoseStreamFailsIsCancelledAndUserDataNeedsTheCloudVariant(t *testing.T) {
	key := newSigner(t)
	srv := newServer(t)
	b := makeBuild(t, key, ref("ubuntu", "resolute", "cloud", "20261013_07:42"), fixture(t, "rootfs-stream.tar.xz"), now.Add(-time.Hour))
	arch := makeBuild(t, key, ref("archlinux", "current", "default", "20261014_04:18"), fixture(t, "rootfs-stream.tar.xz"), now.Add(-time.Hour))
	srv.publish(b)
	srv.publish(arch)
	srv.index(t, key, b, arch)
	src := srv.source(t, key, now)
	g := newFakeGuest(t, nil)
	g.mu.Lock()
	g.refuse = "the machine is being deleted"
	g.mu.Unlock()
	params := agent.MachineCreateParams{Name: "u", UserData: "#cloud-config\n{}"}
	_, _, _, err := src.Create(t.Context(), g.client, g.open, "ubuntu", "", "", params, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "the machine is being deleted") {
		t.Fatalf("a refused stream: %v", err)
	}
	g.mu.Lock()
	cancels, created := g.cancels, g.created
	g.mu.Unlock()
	if len(cancels) != 1 || cancels[0] != 3 || created.Image.Variant != agent.VariantCloud {
		t.Errorf("cancels %v, variant %q", cancels, created.Image.Variant)
	}
	if left := tempFiles(t, src); len(left) != 0 {
		t.Errorf("left behind: %q", left)
	}
	if _, _, _, err := src.Create(t.Context(), g.client, g.open, "arch", "", "", params, nil, nil); err == nil || !strings.Contains(err.Error(), "no archlinux current image with cloud-init") {
		t.Errorf("Arch with user data: %v", err)
	}
}

func TestAnAmd64MachineIsMadeFromTheServersAmd64Build(t *testing.T) {
	key := newSigner(t)
	srv := newServer(t)
	armRef := ref("alpine", "3.24", "default", "20261013_13:01")
	amdRef := armRef
	amdRef.Arch = AMD64
	arm := makeBuild(t, key, armRef, fixture(t, "rootfs-stream.tar.xz"), now.Add(-time.Hour))
	amd := makeBuild(t, key, amdRef, fixture(t, "rootfs-blocks.tar.xz"), now.Add(-time.Hour))
	srv.publish(arm)
	srv.publish(amd)
	srv.index(t, key, arm, amd)
	src := srv.source(t, key, now)
	g := newFakeGuest(t, []agent.ImageInfo{{ImageRef: armRef}})
	params := agent.MachineCreateParams{Name: "a", User: agent.MachineUser{Name: "ada", UID: 501}}
	p := newProgress()
	op, prep, sent, err := src.Create(t.Context(), g.client, g.open, "alpine", "", "x86_64", params, nil, p.note)
	if err != nil {
		t.Fatal(err)
	}
	p.wait(t)
	g.mu.Lock()
	created, tars := g.created, g.tars
	g.mu.Unlock()
	if op.Token != token || prep.Kept || created.Image.ImageRef != amdRef || len(tars) != 1 || sent.TarBytes != int64(len(rootfsTar())) {
		t.Fatalf("an amd64 create beside a kept arm64 build: op %+v, kept %v, image %+v, %d streams", op, prep.Kept, created.Image.ImageRef, len(tars))
	}
	if n := srv.got(arm.path() + "rootfs.tar.xz"); n != 0 {
		t.Errorf("the arm64 rootfs was fetched %d times for an amd64 machine", n)
	}
	g.kept = append(g.kept, agent.ImageInfo{ImageRef: amdRef})
	p = newProgress()
	op, prep, _, err = src.Create(t.Context(), g.client, g.open, "alpine", "3.24", "amd64", params, nil, p.note)
	if err != nil {
		t.Fatal(err)
	}
	p.wait(t)
	g.mu.Lock()
	created = g.created
	g.mu.Unlock()
	if op.Token != "" || !prep.Kept || created.Image.ImageRef != amdRef {
		t.Errorf("from the kept amd64 build: op %+v, kept %v, image %+v", op, prep.Kept, created.Image.ImageRef)
	}
	if _, _, _, err := src.Create(t.Context(), g.client, g.open, "fedora", "", "amd64", params, nil, nil); err == nil || !strings.Contains(err.Error(), "no fedora image for amd64") {
		t.Errorf("a distribution the server has no amd64 build of: %v", err)
	}
	if _, _, _, err := src.Create(t.Context(), g.client, g.open, "alpine", "", "ppc64le", params, nil, nil); err == nil || !strings.Contains(err.Error(), `no architecture "ppc64le"`) {
		t.Errorf("ppc64le: %v", err)
	}
}

func TestWithNoServerOnlyAKeptBuildIsUsed(t *testing.T) {
	src, err := NewSource(NoServer, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	kept := []agent.ImageInfo{{ImageRef: agent.ImageRef{Distro: "ubuntu", Release: "resolute", Arch: Arch, Variant: agent.VariantDefault, Serial: "20261014_00:00"}}}
	p, err := src.Prepare(t.Context(), kept, "ubuntu", "", "", agent.VariantDefault, nil)
	if err != nil || !p.Kept || p.Build != nil || !errors.Is(p.Offline, ErrNoServer) {
		t.Fatalf("a kept build with no server: %+v %v", p, err)
	}
	if _, err := src.Prepare(t.Context(), kept, "fedora", "", "", agent.VariantDefault, nil); !errors.Is(err, ErrNoServer) {
		t.Fatalf("a build the guest lacks with no server: %v", err)
	}
}
