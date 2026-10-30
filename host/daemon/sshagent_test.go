package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"fev/proto/agent"
	"fev/proto/bridge"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/version"
	"fev/proto/vmmctl"
)

type agentVM struct {
	client *agent.Client
	set    chan string
}

func (v *agentVM) VsockPath(uint32) string { return "/nonexistent" }
func (v *agentVM) Agent() *agent.Client    { return v.client }
func (v *agentVM) BootID() string          { return "boot" }
func (v *agentVM) Control() *vmmctl.Client { return nil }
func newAgentVM(t *testing.T) *agentVM {
	t.Helper()
	v := &agentVM{set: make(chan string, 8)}
	cs, ss := net.Pipe()
	server := jsonrpc.NewConn(ss, jsonrpc.HandlerFunc(func(_ context.Context, _ *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
		switch method {
		case agent.MethodHello:
			return agent.HelloResult{APIVersion: agent.APIVersion, AgentVersion: version.Build}, nil
		case agent.MethodSSHAgentSet:
			p, err := jsonrpc.Bind[agent.SSHAgentParams](params)
			if err != nil {
				return nil, err
			}
			v.set <- p.Path
			return agent.SSHAgentBound{Engine: true, Machines: []string{}}, nil
		}
		return nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "%s", method)
	}))
	t.Cleanup(func() { server.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	if v.client, _, err = agent.NewClient(ctx, cs, "test"); err != nil {
		t.Fatal(err)
	}
	return v
}

type agentHost struct {
	Host
	vm VM
}

func (h agentHost) VM() (VM, bool) { return h.vm, true }

func listenAgent(t *testing.T, path, name string) {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b, _ := io.ReadAll(c)
				c.Write(append([]byte(name+":"), b...))
			}()
		}
	}()
}

func TestTheSSHAgentFollowsTheClientsAgent(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "bv.")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	a1, a2 := filepath.Join(dir, "a1.sock"), filepath.Join(dir, "a2.sock")
	listenAgent(t, a1, "one")
	listenAgent(t, a2, "two")
	plain := filepath.Join(dir, "plain")
	os.WriteFile(plain, nil, 0o600)

	f := newFixture(t)
	sa := &SSHAgent{Initial: a1, GuestPath: func(p string) string { return "/guest" + p }}
	f.opts.Modules = []Module{sa}
	f.start()
	v := newAgentVM(t)
	sa.host = agentHost{Host: moduleHost{f.d}, vm: v}

	if got, want := sa.BootWords(), []string{bridge.AgentPathWord("/guest" + a1)}; !reflect.DeepEqual(got, want) {
		t.Errorf("the boot's words: %q, want %q", got, want)
	}
	r := newBridgeRig(t, "")
	r.b.agentSock = sa.Path
	sign := func() string {
		t.Helper()
		c, refused := r.ask(agent.StreamSSHAgent, "dev", uid(501))
		if refused != nil {
			t.Fatalf("refused: %s", refused.Reason)
		}
		c.Write([]byte("x"))
		c.(*net.UnixConn).CloseWrite()
		b, _ := io.ReadAll(c)
		return string(b)
	}
	if got := sign(); got != "one:x" {
		t.Fatalf("before any hello: %q", got)
	}

	hello := func(sock string) {
		t.Helper()
		conn := f.conn()
		c, _, err := api.NewClient(bg, conn, "test", api.WithSSHAuthSock(sock))
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	for _, p := range []string{"", plain, filepath.Join(dir, "none.sock"), "relative.sock"} {
		hello(p)
		if sa.Path() != a1 {
			t.Fatalf("a hello naming %q moved the agent to %q", p, sa.Path())
		}
	}
	select {
	case p := <-v.set:
		t.Fatalf("the guest was told %q for nothing", p)
	case <-time.After(100 * time.Millisecond):
	}

	hello(a2)
	if sa.Path() != a2 {
		t.Fatalf("after a hello naming %s: %q", a2, sa.Path())
	}
	select {
	case p := <-v.set:
		if p != "/guest"+a2 {
			t.Errorf("the guest was told %q", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the running guest was not told the new path")
	}
	if got := sign(); got != "two:x" {
		t.Errorf("after the move, the machines' agent is %q", got)
	}
	sa.StateChanged(api.Status{State: api.StateRunning})
	select {
	case p := <-v.set:
		t.Fatalf("told %q again", p)
	case <-time.After(200 * time.Millisecond):
	}
	if got := strings.Join(sa.BootWords(), " "); got != bridge.AgentPathWord("/guest"+a2) {
		t.Errorf("the next boot's words: %q", got)
	}

	pinned := &SSHAgent{Initial: a1, GuestPath: sa.GuestPath, Pinned: true}
	pinned.Start(agentHost{Host: moduleHost{f.d}, vm: v})
	if w := pinned.BootWords(); w != nil {
		t.Errorf("pinned: words %q", w)
	}
	pinned.Offer(a2)
	select {
	case p := <-v.set:
		t.Fatalf("pinned: the guest was told %q", p)
	case <-time.After(200 * time.Millisecond):
	}
}
