package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"fev/host/daemon/config"
	api "fev/proto/daemon"
	"fev/proto/relay"
)

const noServer = "https://127.0.0.1:1"

type machinesFixture struct {
	*fixture
	m *Machines
	c *api.Client

	mu       sync.Mutex
	changes  []api.MachinesChanged
	ops      map[string]api.Operation
	opEnded  chan api.Operation
	machines chan api.MachinesChanged
}

func machinesDaemon(t *testing.T, opts ...any) *machinesFixture {
	t.Helper()
	var script [][]string
	var before []func(*fixture)
	m := &Machines{ImageServer: noServer, MacUser: "ada", UID: 501}
	for _, o := range opts {
		switch o := o.(type) {
		case []string:
			script = append(script, o)
		case func(*Machines):
			o(m)
		case func(*fixture):
			before = append(before, o)
		}
	}
	f := newFixture(t, script...)
	f.opts.Modules = []Module{m}
	for _, fn := range before {
		fn(f)
	}
	f.start()
	mf := &machinesFixture{fixture: f, m: m, ops: map[string]api.Operation{}, opEnded: make(chan api.Operation, 64), machines: make(chan api.MachinesChanged, 256)}
	mf.c = f.client()
	if _, _, err := mf.c.Subscribe(bg, api.Events{
		Machine: func(mc api.Machine, removed bool) {
			ch := api.MachinesChanged{Machine: mc, Removed: removed}
			mf.mu.Lock()
			mf.changes = append(mf.changes, ch)
			mf.mu.Unlock()
			select {
			case mf.machines <- ch:
			default:
			}
		},
		Operation: func(op api.Operation) {
			mf.mu.Lock()
			mf.ops[op.ID] = op
			mf.mu.Unlock()
			if op.State != api.OperationRunning {
				mf.opEnded <- op
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	return mf
}

func (f *machinesFixture) ended(id string) api.Operation {
	f.t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case op := <-f.opEnded:
			if op.ID == id {
				return op
			}
		case <-deadline:
			f.t.Fatalf("operation %s did not end", id)
		}
	}
}

func (f *machinesFixture) machineIs(name, state string) {
	f.t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		if l, err := f.c.Machines(bg); err == nil {
			for _, m := range l.Machines {
				if m.Name == name && m.State == state {
					return
				}
			}
		}
		select {
		case <-f.machines:
		case <-deadline:
			l, _ := f.c.Machines(bg)
			f.t.Fatalf("%s did not become %s: %+v", name, state, l)
		}
	}
}

func (f *machinesFixture) create(name, distro string) api.Machine {
	f.t.Helper()
	op, err := f.c.CreateMachine(bg, api.MachinesCreateParams{Name: name, Distro: distro})
	if err != nil {
		f.t.Fatal(err)
	}
	if op.State != api.OperationRunning || op.Method != api.MethodMachinesCreate || op.Target != name {
		f.t.Fatalf("the create began %+v", op)
	}
	end := f.ended(op.ID)
	if end.State != api.OperationDone {
		f.t.Fatalf("the create of %s ended %+v", name, end)
	}
	var r api.MachineCreated
	json.Unmarshal(end.Result, &r)
	return r.Machine
}

func names(l *api.Machines) map[string]string {
	out := map[string]string{}
	for _, m := range l.Machines {
		out[m.Name] = m.State
	}
	return out
}

func TestMachinesAreMadeAndTheRunningOnesStartAgain(t *testing.T) {
	f := machinesDaemon(t)
	l, err := f.c.Machines(bg)
	if err != nil || len(l.Machines) != 0 || l.Default != "" {
		t.Fatalf("an empty list: %+v %v", l, err)
	}
	if st := f.d.hub.Status(); st.State != api.StateStopped {
		t.Fatalf("machines.list started the VM: %s", st.State)
	}
	if _, err := f.c.Machine(bg, "nobody"); !api.IsNoMachine(err) {
		t.Errorf("info of a machine nobody has: %v", err)
	}
	dev := f.create("dev", "ubuntu")
	if dev.State != api.MachineRunning || !dev.Default || dev.User != "ada" || dev.Address != "198.19.0.2" || dev.Image.Release != "resolute" {
		t.Fatalf("the first machine: %+v", dev)
	}
	f44 := f.create("f44", "fedora")
	alp := f.create("alp", "alpine")
	if f44.Default || f44.Address != "198.19.0.3" || alp.Address != "198.19.0.4" {
		t.Fatalf("the second and third: %+v %+v", f44, alp)
	}
	values, err := f.c.ConfigGet(bg, "machine.dev.username", "machine.alp.username")
	if err != nil || values["machine.dev.username"].Value != "ada" {
		t.Fatalf("the machines' keys: %+v %v", values, err)
	}
	if _, err := f.c.StopMachine(bg, api.MachinesStopParams{Name: "alp"}); err != nil {
		t.Fatal(err)
	}
	f.machineIs("alp", api.MachineStopped)

	if _, err := f.c.VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	f.stopped()
	l, _ = f.c.Machines(bg)
	if got := names(l); got["dev"] != api.MachineStopped || got["f44"] != api.MachineStopped || got["alp"] != api.MachineStopped || l.Default != "dev" {
		t.Fatalf("with the VM stopped: %+v", l)
	}
	if _, err := f.c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	f.machineIs("dev", api.MachineRunning)
	f.machineIs("f44", api.MachineRunning)
	time.Sleep(200 * time.Millisecond)
	if l, _ = f.c.Machines(bg); names(l)["alp"] != api.MachineStopped {
		t.Fatalf("a machine stopped by request was started again: %+v", l)
	}
	for _, m := range l.Machines {
		if want := map[string]string{"dev": "198.19.0.2", "f44": "198.19.0.3"}[m.Name]; want != "" && m.Address != want {
			t.Errorf("after the VM's restart %s has %q, not %s", m.Name, m.Address, want)
		}
	}
	data, _ := os.ReadFile(f.layout.MachinesFile())
	var rec machinesFile
	json.Unmarshal(data, &rec)
	ran := map[string]bool{}
	for _, e := range rec.Machines {
		ran[e.Name] = e.Running
	}
	if rec.Default != "dev" || !ran["dev"] || !ran["f44"] || ran["alp"] {
		t.Fatalf("the record: %s", data)
	}
}

func TestRenameCloneDeleteAndResetKeepTheRecordAndKeysInStep(t *testing.T) {
	f := machinesDaemon(t)
	f.create("dev", "ubuntu")
	if _, err := f.c.ConfigSet(bg, map[string]any{"machine.dev.username": "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.ConfigSet(bg, map[string]any{"machine.dev.username": "Not Valid"}); err == nil {
		t.Error("a user name no distribution takes was accepted")
	}
	web, err := f.c.RenameMachine(bg, "dev", "web")
	if err != nil {
		t.Fatal(err)
	}
	l, _ := f.c.Machines(bg)
	if web.Name != "web" || l.Default != "web" || web.User != "alice" || web.Address != "198.19.0.2" {
		t.Fatalf("after the rename: %+v, %+v", web, l)
	}
	if _, err := f.c.ConfigGet(bg, "machine.dev.username"); err == nil {
		t.Error("the old name's key is still there")
	}
	copy, err := f.c.CloneMachine(bg, "web", "copy")
	if err != nil {
		t.Fatal(err)
	}
	if copy.State != api.MachineStopped || copy.Address != "198.19.0.3" || copy.User != "alice" || copy.Default {
		t.Fatalf("the clone: %+v", copy)
	}
	if _, err := f.c.CloneMachine(bg, "web", "copy"); err == nil {
		t.Error("a clone onto a name in use was made")
	}
	if _, err := f.c.CreateMachine(bg, api.MachinesCreateParams{Name: "default", Distro: "ubuntu"}); err == nil {
		t.Error("a machine named default was begun")
	}
	op, err := f.c.CreateMachine(bg, api.MachinesCreateParams{Name: "x86", Distro: "ubuntu", Arch: "x86_64"})
	if err != nil {
		t.Fatalf("an amd64 machine: %v", err)
	}
	if end := f.ended(op.ID); end.State != api.OperationDone {
		t.Fatalf("an amd64 machine's create ended %+v", end)
	}
	if x, err := f.c.Machine(bg, "x86"); err != nil || x.Image.Arch != "amd64" || x.Image.Distro != "ubuntu" || x.Image.Release != "resolute" {
		t.Errorf("the amd64 machine: %+v %v", x, err)
	}
	if rec, err := os.ReadFile(f.m.path); err != nil || !strings.Contains(string(rec), `"arch": "amd64"`) {
		t.Errorf("the record has no amd64 machine: %v\n%s", err, rec)
	}
	for _, arch := range []string{"riscv64", "i386"} {
		if _, err := f.c.CreateMachine(bg, api.MachinesCreateParams{Name: "y", Distro: "ubuntu", Arch: arch}); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("no architecture %q", arch)) {
			t.Errorf("a %s machine: %v", arch, err)
		}
	}
	if _, err := f.c.Machine(bg, "y"); err == nil {
		t.Error("a machine of a refused architecture is listed")
	}
	if err := f.c.DeleteMachine(bg, api.MachinesStopParams{Name: "x86"}); err != nil {
		t.Fatal(err)
	}
	if err := f.c.DeleteMachine(bg, api.MachinesStopParams{Name: "web"}); err != nil {
		t.Fatal(err)
	}
	l, _ = f.c.Machines(bg)
	if l.Default != "" || len(l.Machines) != 1 || l.Machines[0].Name != "copy" {
		t.Fatalf("after deleting the default: %+v", l)
	}
	if _, err := f.c.SetDefaultMachine(bg, "copy"); err != nil {
		t.Fatal(err)
	}
	f.c.ConfigSet(bg, map[string]any{api.KeyDataAllowBackup: true})
	f.m.Forget()
	l, _ = f.c.Machines(bg)
	values, _ := f.c.ConfigList(bg)
	if len(l.Machines) != 0 || l.Default != "" || values["machine.copy.username"].Value != nil || values[api.KeyDataAllowBackup].Value != true {
		t.Fatalf("after a reset: %+v, %+v", l, values)
	}
	f.mu.Lock()
	removed := 0
	for _, ch := range f.changes {
		if ch.Removed {
			removed++
		}
	}
	f.mu.Unlock()
	if removed < 3 {
		t.Errorf("%d removals were told (a rename's old name, a delete, a reset)", removed)
	}
}

func TestAMachinesKeyIsPendingOnlyWhileItRunsWithAnother(t *testing.T) {
	f := machinesDaemon(t)
	f.create("sandbox", "alpine")
	f.create("other", "alpine")
	key, mounts := "machine.sandbox.http_port", "machine.sandbox.mounts"
	pending := func(name string) bool {
		t.Helper()
		values, err := f.c.ConfigGet(bg, name)
		if err != nil {
			t.Fatal(err)
		}
		return values[name].Pending
	}
	changed, err := f.c.ConfigSet(bg, map[string]any{key: 8080})
	if err != nil || len(changed) != 1 || !changed[0].Pending || changed[0].Apply != api.ApplyMachineRestart {
		t.Fatalf("a running machine's web port changed: %+v %v", changed, err)
	}
	if !pending(key) || pending("machine.other.http_port") || pending("machine.sandbox.username") {
		t.Errorf("pending: %s %v, other's %v, the user %v", key, pending(key), pending("machine.other.http_port"), pending("machine.sandbox.username"))
	}
	changed, err = f.c.ConfigAdd(bg, mounts, "/Users/ada/src/project")
	if err != nil || len(changed) != 1 || !changed[0].Pending {
		t.Fatalf("config.add on a running machine's list key: %+v %v", changed, err)
	}
	if st, _ := f.c.Status(bg); len(st.PendingRestart) != 0 {
		t.Errorf("a machine's keys wait for a VM restart: %v", st.PendingRestart)
	}
	if changed, err = f.c.ConfigRemove(bg, mounts, "/Users/ada/src/project"); err != nil || len(changed) != 1 || changed[0].Pending {
		t.Errorf("config.remove back to what the machine runs with: %+v %v", changed, err)
	}
	if _, err := f.c.RenameMachine(bg, "sandbox", "box"); err != nil {
		t.Fatal(err)
	}
	key = "machine.box.http_port"
	if !pending(key) {
		t.Errorf("%s is not pending after the rename", key)
	}
	if _, err := f.c.RestartMachine(bg, api.MachinesStopParams{Name: "box"}); err != nil {
		t.Fatal(err)
	}
	f.machineIs("box", api.MachineRunning)
	if pending(key) {
		t.Errorf("%s still pending after the machine's restart", key)
	}
	if _, err := f.c.StopMachine(bg, api.MachinesStopParams{Name: "box"}); err != nil {
		t.Fatal(err)
	}
	f.machineIs("box", api.MachineStopped)
	if changed, err = f.c.ConfigSet(bg, map[string]any{key: 9090}); err != nil || len(changed) != 1 || changed[0].Pending {
		t.Errorf("a stopped machine's web port changed: %+v %v", changed, err)
	}
	if _, err := f.c.StartMachine(bg, "box"); err != nil {
		t.Fatal(err)
	}
	f.machineIs("box", api.MachineRunning)
	if pending(key) {
		t.Errorf("%s pending after a start that read it", key)
	}
	f.c.ConfigSet(bg, map[string]any{key: 7070})
	if !pending(key) {
		t.Errorf("%s not pending while box runs with 9090", key)
	}
	if _, err := f.c.VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	f.stopped()
	if pending(key) {
		t.Errorf("%s pending with the VM stopped", key)
	}
}

func TestNamesThatDifferOnlyInCaseAreOneName(t *testing.T) {
	f := machinesDaemon(t)
	for _, name := range []string{"Dev.1", "1abc", "x-y"} {
		if got := f.create(name, "ubuntu"); got.Name != name {
			t.Fatalf("made as %q, not %q", got.Name, name)
		}
	}
	if _, err := f.c.ConfigSet(bg, map[string]any{"machine.Dev.1.username": "alice"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dev.1", "DEV.1", "1ABC", "X-Y", "Docker", "DEFAULT", "a_b"} {
		if _, err := f.c.CreateMachine(bg, api.MachinesCreateParams{Name: name, Distro: "ubuntu"}); err == nil {
			t.Errorf("a create of %s was begun", name)
		}
		if _, err := f.c.CloneMachine(bg, "x-y", name); err == nil {
			t.Errorf("a clone onto %s was made", name)
		}
		if _, err := f.c.RenameMachine(bg, "1abc", name); err == nil && name != "1ABC" {
			t.Errorf("1abc was renamed %s", name)
		} else if err == nil {
			if _, err := f.c.RenameMachine(bg, "1ABC", "1abc"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := f.c.CloneMachine(bg, "x-y", "dev.1"); err == nil || !strings.Contains(err.Error(), "Dev.1 exists") || !strings.Contains(err.Error(), "only in case") {
		t.Errorf("the refusal names the other machine: %v", err)
	}
	m, err := f.c.RenameMachine(bg, "Dev.1", "DEV.1")
	if err != nil {
		t.Fatal(err)
	}
	l, _ := f.c.Machines(bg)
	if m.Name != "DEV.1" || m.User != "alice" || l.Default != "DEV.1" || names(l)["Dev.1"] != "" {
		t.Fatalf("after a rename to its own name in capitals: %+v %+v", m, l)
	}
	if values, err := f.c.ConfigGet(bg, "machine.DEV.1.username"); err != nil || values["machine.DEV.1.username"].Value != "alice" {
		t.Fatalf("its key: %+v %v", values, err)
	}
	if _, err := f.c.Machine(bg, "dev.1"); !api.IsNoMachine(err) {
		t.Errorf("a machine is named as it is called, not in another case: %v", err)
	}
}

func TestExportImportAndCancel(t *testing.T) {
	f := machinesDaemon(t, []string{"create-ms=3000"})
	op, err := f.c.CreateMachine(bg, api.MachinesCreateParams{Name: "slow", Distro: "debian"})
	if err != nil {
		t.Fatal(err)
	}
	f.machineIs("slow", api.MachineCreating)
	cancelled, err := f.c.OperationCancel(bg, op.ID)
	if err != nil || cancelled.State != api.OperationCancelled {
		t.Fatalf("the cancel: %+v %v", cancelled, err)
	}
	if got, _ := f.c.OperationGet(bg, op.ID); got.State != api.OperationCancelled || got.Ended == "" {
		t.Fatalf("operation.get after it: %+v", got)
	}
	if l, _ := f.c.Machines(bg); len(l.Machines) != 0 {
		t.Fatalf("a cancelled create left %+v", l)
	}

	f.create("dev", "ubuntu")
	dir := t.TempDir()
	path := filepath.Join(dir, "dev.tar.zst")
	exp, err := f.c.ExportMachine(bg, api.MachinesExportParams{Name: "dev", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	end := f.ended(exp.ID)
	var r api.MachineExported
	json.Unmarshal(end.Result, &r)
	st, err := os.Stat(path)
	if end.State != api.OperationDone || err != nil || r.Path != path || r.Bytes != st.Size() || end.DoneBytes == 0 {
		t.Fatalf("the export ended %+v: %+v %v", end, r, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the export left %d files", len(entries))
	}
	zf, _ := os.Open(path)
	zr, err := zstd.NewReader(zf)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	zr.Close()
	zf.Close()
	if !bytes.Contains(plain, []byte(archiveConfig)) || int64(len(plain)) != end.DoneBytes {
		t.Fatalf("the archive holds %d plain bytes, the operation counted %d", len(plain), end.DoneBytes)
	}
	if _, err := f.c.ExportMachine(bg, api.MachinesExportParams{Name: "dev", Path: path}); err == nil {
		t.Error("an export over an existing file was begun without replace")
	}
	exp, err = f.c.ExportMachine(bg, api.MachinesExportParams{Name: "dev", Path: path, Replace: true})
	if err != nil || f.ended(exp.ID).State != api.OperationDone {
		t.Fatalf("an export with replace: %v", err)
	}

	imp, err := f.c.ImportMachine(bg, api.MachinesImportParams{Path: path, Name: "copy"})
	if err != nil {
		t.Fatal(err)
	}
	end = f.ended(imp.ID)
	var m api.Machine
	json.Unmarshal(end.Result, &m)
	if end.State != api.OperationDone || m.Name != "copy" || m.State != api.MachineStopped || m.Address != "198.19.0.3" || end.TotalBytes != st.Size() {
		t.Fatalf("the import ended %+v: %+v", end, m)
	}
	if _, err := f.c.ImportMachine(bg, api.MachinesImportParams{Path: path}); err == nil || !strings.Contains(err.Error(), "dev exists") {
		t.Errorf("an import under a name in use: %v", err)
	}
	if _, err := f.c.ImportMachine(bg, api.MachinesImportParams{Path: "relative.tar.zst"}); err == nil {
		t.Error("a relative path was taken")
	}
}

func readSession(conn net.Conn) (stdout, stderr []byte, err error) {
	buf := make([]byte, relay.MaxFrame)
	for {
		stream, n, err := relay.ReadFrame(conn, buf)
		if err == io.EOF {
			return stdout, stderr, nil
		}
		if err != nil {
			return stdout, stderr, err
		}
		switch stream {
		case relay.Stdout:
			stdout = append(stdout, buf[:n]...)
		case relay.Stderr:
			stderr = append(stderr, buf[:n]...)
		}
	}
}

func (f *machinesFixture) run(c *api.Client, p api.SessionStartParams) (string, api.SessionExit, error) {
	exits := make(chan api.SessionExit, 1)
	started, err := c.SessionStart(bg, p, func(e api.SessionExit) { exits <- e })
	if err != nil {
		return "", api.SessionExit{}, err
	}
	stream, err := api.AttachSession(bg, f.layout.Home, started.Token)
	if err != nil {
		return "", api.SessionExit{}, err
	}
	defer stream.Close()
	if p.Login || (len(p.Argv) > 0 && p.Argv[0] == "cat") {
		relay.WriteFrame(stream, relay.Stdin, []byte("typed\n"))
		relay.WriteFrame(stream, relay.Stdin, nil)
	}
	out, _, err := readSession(stream)
	if err != nil {
		return "", api.SessionExit{}, err
	}
	select {
	case e := <-exits:
		return string(out), e, nil
	case <-time.After(10 * time.Second):
		return string(out), api.SessionExit{}, fmt.Errorf("no session.exit")
	}
}

func TestASessionStartsItsMachineAndItsStreamIsPassed(t *testing.T) {
	f := machinesDaemon(t)
	if _, err := f.c.SessionStart(bg, api.SessionStartParams{Argv: []string{"true"}}, nil); !api.IsNoMachine(err) {
		t.Fatalf("a session with no default machine: %v", err)
	}
	f.create("dev", "ubuntu")
	if _, err := f.c.VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	f.stopped()
	c := f.client()
	out, exit, err := f.run(c, api.SessionStartParams{Argv: []string{"echo", "hello", "there"}})
	if err != nil || out != "hello there\n" || exit.ExitCode != 0 || !exit.StreamEnded {
		t.Fatalf("echo: %q %+v %v", out, exit, err)
	}
	out, exit, err = f.run(c, api.SessionStartParams{Argv: []string{"whoami"}})
	if out != "ada\n" || err != nil {
		t.Fatalf("whoami: %q %v", out, err)
	}
	out, exit, err = f.run(c, api.SessionStartParams{Machine: "dev", User: "root", Argv: []string{"cat"}})
	if out != "typed\n" || exit.ExitCode != 0 || err != nil {
		t.Fatalf("cat: %q %+v %v", out, exit, err)
	}
	_, exit, _ = f.run(c, api.SessionStartParams{Argv: []string{"nosuch"}})
	if exit.ExitCode != 127 {
		t.Fatalf("a command not found: %+v", exit)
	}
	_, exit, _ = f.run(c, api.SessionStartParams{Argv: []string{"exit", "200"}})
	if exit.ExitCode != 200 {
		t.Fatalf("exit 200: %+v", exit)
	}
	exits := make(chan api.SessionExit, 1)
	started, err := c.SessionStart(bg, api.SessionStartParams{Argv: []string{"sleep", "30000"}}, func(e api.SessionExit) { exits <- e })
	if err != nil {
		t.Fatal(err)
	}
	stream, err := api.AttachSession(bg, f.layout.Home, started.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.AttachSession(bg, f.layout.Home, started.Token); err == nil {
		t.Error("a token was attached twice")
	}
	if err := c.SessionSignal(bg, started.ID, 2); err != nil {
		t.Fatal(err)
	}
	readSession(stream)
	stream.Close()
	if e := <-exits; e.ExitCode != -1 || e.Signal != 2 || !e.StreamEnded {
		t.Fatalf("after SIGINT: %+v", e)
	}
	if err := f.c.SessionSignal(bg, started.ID, 9); err == nil {
		t.Error("a session was signalled from another connection")
	}
}

func TestAStreamNotTakenEndsItsSessionWithAnError(t *testing.T) {
	f := machinesDaemon(t, func(m *Machines) { m.holdFor = 300 * time.Millisecond })
	f.create("dev", "ubuntu")
	exits := make(chan api.SessionExit, 1)
	started, err := f.c.SessionStart(bg, api.SessionStartParams{Argv: []string{"sleep", "30000"}}, func(e api.SessionExit) { exits <- e })
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", api.SessionSocketPath(f.layout.Home))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	relay.WriteRequest(conn, relay.Request{Kind: relay.Exec, Target: started.Token})
	select {
	case e := <-exits:
		if e.ExitCode != -1 || e.Error != ErrNotTaken || e.StreamEnded {
			t.Fatalf("the exit: %+v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no exit after the bound")
	}
}

const churnArg = "fev-test-churn"

func churn() {
	for {
		fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		if err == nil {
			syscall.Close(fds[0])
			syscall.Close(fds[1])
		}
	}
}

func TestNoSessionsOutputIsLostUnderUnixSocketChurn(t *testing.T) {
	n := 300
	if testing.Short() {
		n = 50
	}
	f := machinesDaemon(t)
	f.create("dev", "ubuntu")
	for range 2 {
		cmd := exec.Command(os.Args[0], churnArg)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	}
	c := f.client()
	want := ""
	for i := 1; i <= 200; i++ {
		want += fmt.Sprintf("%d\n", i)
	}
	lost := 0
	for i := 0; i < n; i++ {
		out, exit, err := f.run(c, api.SessionStartParams{Argv: []string{"seq", "200"}})
		if err != nil || exit.ExitCode != 0 {
			t.Fatalf("session %d: %+v %v", i, exit, err)
		}
		if out != want {
			lost++
		}
	}
	if lost != 0 {
		t.Fatalf("%d of %d sessions lost their output under churn", lost, n)
	}
}

func TestAnAmd64MachineNeedsX86EmulationOn(t *testing.T) {
	f := machinesDaemon(t)
	op, err := f.c.CreateMachine(bg, api.MachinesCreateParams{Name: "x86", Distro: "ubuntu", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if end := f.ended(op.ID); end.State != api.OperationDone {
		t.Fatalf("the amd64 create ended %+v", end)
	}
	if _, err := f.c.StopMachine(bg, api.MachinesStopParams{Name: "x86"}); err != nil {
		t.Fatal(err)
	}
	f.m.cfg.SetInEffect(api.KeyEmulationX86, false)
	if _, err := f.c.CreateMachine(bg, api.MachinesCreateParams{Name: "y86", Distro: "ubuntu", Arch: "amd64"}); err == nil || !strings.Contains(err.Error(), api.KeyEmulationX86) {
		t.Errorf("an amd64 create with emulation off: %v", err)
	}
	if _, err := f.c.StartMachine(bg, "x86"); err == nil || !strings.Contains(err.Error(), "x86 emulation, which is off") {
		t.Errorf("an amd64 start with emulation off: %v", err)
	}
	f.create("arm", "ubuntu")
	f.m.cfg.SetInEffect(api.KeyEmulationX86, true)
	if x, err := f.c.StartMachine(bg, "x86"); err != nil || x.State != api.MachineRunning {
		t.Errorf("the amd64 start with emulation on: %+v %v", x, err)
	}
}

func TestACreatesEmulationHoldsFromItsFirstBoot(t *testing.T) {
	f := machinesDaemon(t)
	op, err := f.c.CreateMachine(bg, api.MachinesCreateParams{Name: "x86", Distro: "ubuntu", Arch: "amd64", Emulation: "fex"})
	if err != nil {
		t.Fatal(err)
	}
	end := f.ended(op.ID)
	if end.State != api.OperationDone {
		t.Fatalf("the create ended %+v", end)
	}
	var made api.MachineCreated
	if err := json.Unmarshal(end.Result, &made); err != nil || made.Machine.Emulation != "fex" {
		t.Fatalf("the create's first boot ran without its emulation: %+v %v", made.Machine, err)
	}
	if v, err := f.m.cfg.Get(config.MachinePrefix + "x86." + api.MachineKeyEmulation); err != nil || v[config.MachinePrefix+"x86."+api.MachineKeyEmulation].Value != "fex" {
		t.Errorf("the machine's key: %+v %v", v, err)
	}
	if _, err := f.c.CreateMachine(bg, api.MachinesCreateParams{Name: "arm", Distro: "ubuntu", Emulation: "rosetta"}); err == nil || !strings.Contains(err.Error(), "runs natively") {
		t.Errorf("an arm64 create with an x86 engine: %v", err)
	}
}
