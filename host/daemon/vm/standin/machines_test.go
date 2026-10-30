package standin

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"
	"fev/proto/netplan"
	"fev/proto/relay"
)

func hostConn(t *testing.T, a *Agent) (*jsonrpc.Conn, chan json.RawMessage) {
	t.Helper()
	notes := make(chan json.RawMessage, 256)
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close() })
	a.SetHost(a.ServeConn(server))
	c := jsonrpc.NewConn(client, jsonrpc.HandlerFunc(func(_ context.Context, _ *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
		b, _ := json.Marshal(map[string]any{"method": method, "params": params})
		notes <- b
		return nil, nil
	}))
	t.Cleanup(func() { c.Close() })
	if err := c.Call(bg, api.MethodHello, api.HelloParams{APIVersion: api.APIVersion}, nil); err != nil {
		t.Fatal(err)
	}
	return c, notes
}

func waitNote(t *testing.T, notes chan json.RawMessage, method string, match func(json.RawMessage) bool) json.RawMessage {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case b := <-notes:
			var n struct {
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			json.Unmarshal(b, &n)
			if n.Method == method && (match == nil || match(n.Params)) {
				return n.Params
			}
		case <-deadline:
			t.Fatalf("no %s arrived", method)
		}
	}
}

func opEnded(id uint64) func(json.RawMessage) bool {
	return func(p json.RawMessage) bool {
		var op api.OperationProgress
		json.Unmarshal(p, &op)
		return op.ID == id && op.State != api.OperationRunning
	}
}

func TestTheStandInServesTheMachineCallsAheadOfTheGuest(t *testing.T) {
	pending, err := api.PendingFixtures()
	if err != nil {
		t.Fatal(err)
	}
	all, err := api.Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	a := booted(t)
	c, _ := hostConn(t, a)
	for _, fx := range append(pending, all...) {
		if !servedAhead[fx.Method] || fx.Notification {
			continue
		}
		t.Run(fx.Method, func(t *testing.T) {
			params := fx.Params
			if fx.Method == api.MethodOperationCancel {
				a.RemoveMachine("dev")
				a.AddMachine("dev", standinConfig)
				var op api.Operation
				if err := c.Call(bg, api.MethodMachineExport, api.MachineNameParams{Name: "dev"}, &op); err != nil {
					t.Fatal(err)
				}
				params, _ = json.Marshal(api.OperationID{ID: op.ID})
			} else if fx.Method == api.MethodK8sImagePull {
				if err := a.SetK8s(K8sOptions{Wanted: true, Cached: true, Plan: netplan.Default()}); err != nil {
					t.Fatal(err)
				}
			} else {
				seedMachines(t, c, a, fx)
			}
			var result json.RawMessage
			if err := c.Call(bg, fx.Method, params, &result); err != nil {
				t.Fatal(err)
			}
			types := api.Schema[fx.Method]
			if p, ok := api.Pending[fx.Method]; ok {
				types = p
			}
			if err := api.Conforms(result, types.Result); err != nil {
				t.Fatalf("result %s does not fit: %v", result, err)
			}
		})
	}
}

func TestTheStandInMakesExportsAndImportsMachines(t *testing.T) {
	a := booted(t)
	c, notes := hostConn(t, a)
	user := api.MachineUser{Name: "ada", UID: 501}
	kept := api.MachineImage{ImageRef: api.ImageRef{Distro: "ubuntu", Release: "resolute", Arch: "arm64", Variant: api.VariantDefault, Serial: StandinSerial}}
	var op api.Operation
	if err := c.Call(bg, api.MethodMachineCreate, api.MachineCreateParams{Name: "dev", Image: kept, User: user, Config: standinConfig}, &op); err != nil {
		t.Fatal(err)
	}
	if op.Token != "" {
		t.Fatalf("a create of a kept build asked for a stream: %+v", op)
	}
	waitNote(t, notes, api.NotifyMachineState, func(p json.RawMessage) bool {
		var st api.MachineState
		json.Unmarshal(p, &st)
		return st.Name == "dev" && st.State == api.MachineRunning
	})
	var done api.OperationProgress
	json.Unmarshal(waitNote(t, notes, api.NotifyOperationProgress, opEnded(op.ID)), &done)
	var created api.MachineCreated
	json.Unmarshal(done.Result, &created)
	if done.State != api.OperationDone || created.Machine.State != api.MachineRunning || created.Machine.Address != standinConfig.Address {
		t.Fatalf("the create ended %+v: %+v", done, created)
	}

	fresh := api.MachineImage{ImageRef: api.ImageRef{Distro: "fedora", Release: "44", Arch: "arm64", Variant: api.VariantDefault, Serial: "20261015_00:00"}, Templates: []api.ImageTemplate{}}
	cfg := standinConfig
	cfg.MAC, cfg.Address = "02:42:c6:13:00:0b", "198.19.0.11"
	if err := c.Call(bg, api.MethodMachineCreate, api.MachineCreateParams{Name: "f44", Image: fresh, User: user, Config: cfg}, &op); err != nil {
		t.Fatal(err)
	}
	if len(op.Token) != relay.TokenLen {
		t.Fatalf("a create of a build the guest lacks gave no token: %+v", op)
	}
	var tarball bytes.Buffer
	tw := tar.NewWriter(&tarball)
	tw.WriteHeader(&tar.Header{Name: "etc/os-release", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5})
	tw.Write([]byte("ID=x\n"))
	tw.Close()
	host, guest := net.Pipe()
	go func(g net.Conn, token string) { a.AttachStream(token, g); g.Close() }(guest, op.Token)
	if err := relay.ReadReply(host); err != nil {
		t.Fatal(err)
	}
	host.Write(tarball.Bytes())
	host.Close()
	json.Unmarshal(waitNote(t, notes, api.NotifyOperationProgress, opEnded(op.ID)), &done)
	if done.State != api.OperationDone {
		t.Fatalf("the create from a stream ended %+v", done)
	}
	var list api.MachineList
	c.Call(bg, api.MethodMachineList, nil, &list)
	found := false
	for _, k := range list.Images {
		found = found || k.ImageRef == fresh.ImageRef
	}
	if !found || len(list.Machines) != 2 {
		t.Fatalf("after the create: %+v", list)
	}

	if err := c.Call(bg, api.MethodMachineExport, api.MachineNameParams{Name: "dev"}, &op); err != nil {
		t.Fatal(err)
	}
	host, guest = net.Pipe()
	go func(g net.Conn, token string) { a.AttachStream(token, g); g.Close() }(guest, op.Token)
	if err := relay.ReadReply(host); err != nil {
		t.Fatal(err)
	}
	archive, _ := io.ReadAll(host)
	host.Close()
	json.Unmarshal(waitNote(t, notes, api.NotifyOperationProgress, opEnded(op.ID)), &done)
	var exported api.MachineExported
	json.Unmarshal(done.Result, &exported)
	if done.State != api.OperationDone || exported.Bytes != int64(len(archive)) || !bytes.Contains(archive, []byte(archiveConfig)) {
		t.Fatalf("the export ended %+v, %d bytes read", done, len(archive))
	}
	cfg.MAC, cfg.Address = "02:42:c6:13:00:0c", "198.19.0.12"
	if err := c.Call(bg, api.MethodMachineImport, api.MachineImportParams{Name: "copy", Config: cfg}, &op); err != nil {
		t.Fatal(err)
	}
	host, guest = net.Pipe()
	go func(g net.Conn, token string) { a.AttachStream(token, g); g.Close() }(guest, op.Token)
	if err := relay.ReadReply(host); err != nil {
		t.Fatal(err)
	}
	host.Write(archive)
	host.Close()
	json.Unmarshal(waitNote(t, notes, api.NotifyOperationProgress, opEnded(op.ID)), &done)
	var imported api.MachineInfo
	json.Unmarshal(done.Result, &imported)
	if done.State != api.OperationDone || imported.Name != "copy" || imported.State != api.MachineStopped || imported.Image != kept.ImageRef {
		t.Fatalf("the import ended %+v: %+v", done, imported)
	}
}

func TestTheStandInRunsSessionsInMachines(t *testing.T) {
	a := booted(t)
	c, notes := hostConn(t, a)
	a.AddMachine("dev", standinConfig)
	var started api.ExecStarted
	p := api.ExecStartParams{Context: api.ExecMachine, Machine: "dev", Argv: []string{"cat"}}
	if err := c.Call(bg, api.MethodExecStart, p, &started); !strings.Contains(errString(err), "not running") {
		t.Fatalf("a session in a stopped machine: %v", err)
	}
	if err := c.Call(bg, api.MethodMachineStart, api.MachineStartParams{Name: "dev", Config: standinConfig}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(bg, api.MethodExecStart, p, &started); err != nil {
		t.Fatal(err)
	}
	host, guest := net.Pipe()
	go a.AttachExec(started.Token, guest)
	if err := relay.ReadReply(host); err != nil {
		t.Fatal(err)
	}
	go func() {
		relay.WriteFrame(host, relay.Stdin, []byte("hello\n"))
		relay.WriteFrame(host, relay.Stdin, nil)
	}()
	buf := make([]byte, relay.MaxFrame)
	var got []byte
	for {
		stream, n, err := relay.ReadFrame(host, buf)
		if err != nil {
			break
		}
		if stream == relay.Stdout {
			got = append(got, buf[:n]...)
		}
	}
	var exit api.ExecExit
	json.Unmarshal(waitNote(t, notes, api.NotifyExecExit, nil), &exit)
	if string(got) != "hello\n" || exit.ID != started.ID || exit.ExitCode != 0 {
		t.Fatalf("cat gave %q and %+v", got, exit)
	}
	if a.AttachExec(started.Token, guest) {
		t.Fatal("a token was taken twice")
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
