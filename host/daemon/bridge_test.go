package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fev/proto/agent"
	"fev/proto/bridge"
	"fev/proto/jsonrpc"
	"fev/proto/relay"
	"fev/proto/version"
	"fev/proto/vmmctl"
)

type fakeBridgeGuest struct {
	t      *testing.T
	dir    string
	client *agent.Client
	server *jsonrpc.Conn

	mu      sync.Mutex
	streams map[string]chan net.Conn
	refused chan agent.StreamRefuseParams
}

func (g *fakeBridgeGuest) VsockPath(port uint32) string {
	return filepath.Join(g.dir, fmt.Sprintf("vsock_%d.sock", port))
}
func (g *fakeBridgeGuest) Agent() *agent.Client    { return g.client }
func (g *fakeBridgeGuest) BootID() string          { return "boot" }
func (g *fakeBridgeGuest) Control() *vmmctl.Client { return nil }

type bridgeRig struct {
	*fakeBridgeGuest
	b        *bridgeServer
	notified chan [2]string
}

func newBridgeRig(t *testing.T, agentSock string) *bridgeRig {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bv.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	g := &fakeBridgeGuest{t: t, dir: dir, streams: map[string]chan net.Conn{}, refused: make(chan agent.StreamRefuseParams, 8)}

	l, err := net.Listen("unix", g.VsockPath(relay.Port))
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
			req, err := relay.ReadRequest(c)
			g.mu.Lock()
			ch := g.streams[req.Target]
			delete(g.streams, req.Target)
			g.mu.Unlock()
			if err != nil || req.Kind != relay.Stream || ch == nil {
				relay.WriteError(c, relay.CodeDenied, "no such stream request")
				c.Close()
				continue
			}
			relay.WriteOK(c)
			ch <- c
		}
	}()

	cs, ss := net.Pipe()
	g.server = jsonrpc.NewConn(ss, jsonrpc.HandlerFunc(func(_ context.Context, _ *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
		switch method {
		case agent.MethodHello:
			return agent.HelloResult{APIVersion: agent.APIVersion, AgentVersion: version.Build}, nil
		case agent.MethodStreamRefuse:
			p, err := jsonrpc.Bind[agent.StreamRefuseParams](params)
			if err != nil {
				return nil, err
			}
			g.refused <- p
			return struct{}{}, nil
		}
		return nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "%s", method)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g.client, _, err = agent.NewClient(ctx, cs, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.server.Close() })

	r := &bridgeRig{fakeBridgeGuest: g, notified: make(chan [2]string, 4)}
	r.b = &bridgeServer{
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		agentSock: func() string { return agentSock },
		share:     func(m string) (bool, bool) { return m != "noshare", m != "ghost" },
		notify: func(title, message string) error {
			r.notified <- [2]string{title, message}
			return nil
		},
		slots: make(chan struct{}, agent.MaxStreams),
		home:  os.Getenv("HOME"), user: "me",
	}
	g.client.OnMachineStreamRequest(r.b.requests(g))
	return r
}

func (r *bridgeRig) ask(service, machine string, uid *int) (net.Conn, *agent.StreamRefuseParams) {
	r.t.Helper()
	var raw [relay.TokenLen / 2]byte
	rand.Read(raw[:])
	token := hex.EncodeToString(raw[:])
	ch := make(chan net.Conn, 1)
	r.mu.Lock()
	r.streams[token] = ch
	r.mu.Unlock()
	if err := r.server.Notify(agent.NotifyStreamRequest, agent.StreamRequest{Token: token, Service: service, Machine: machine, UID: uid}); err != nil {
		r.t.Fatal(err)
	}
	select {
	case c := <-ch:
		r.t.Cleanup(func() { c.Close() })
		return c, nil
	case p := <-r.refused:
		return nil, &p
	case <-time.After(10 * time.Second):
		r.t.Fatal("neither a stream nor a refusal")
	}
	return nil, nil
}

func uid(n int) *int { return &n }

type helper struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

func (r *bridgeRig) helper(machine string, h *bridge.Header) (*helper, error) {
	r.t.Helper()
	c, refused := r.ask(agent.StreamMac, machine, uid(501))
	if refused != nil {
		r.t.Fatalf("refused: %s", refused.Reason)
	}
	if err := bridge.WriteHeader(c, h); err != nil {
		r.t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(30 * time.Second))
	hp := &helper{t: r.t, c: c, r: bufio.NewReader(c)}
	return hp, bridge.ReadReply(hp.r)
}

func (h *helper) send(stream byte, p []byte) {
	h.t.Helper()
	if err := bridge.WriteFrame(h.c, stream, p); err != nil {
		h.t.Fatal(err)
	}
}

func (h *helper) control(c bridge.Control) {
	b, _ := json.Marshal(c)
	h.send(bridge.FrameControl, b)
}

func (h *helper) until(want string) (stdout, stderr string) {
	h.t.Helper()
	var out, errs bytes.Buffer
	buf := make([]byte, bridge.MaxFrame)
	for !strings.Contains(out.String()+errs.String(), want) {
		s, n, err := bridge.ReadFrame(h.r, buf)
		if err != nil {
			h.t.Fatalf("waiting for %q: %v; stdout %q stderr %q", want, err, out.String(), errs.String())
		}
		switch s {
		case bridge.FrameStdout:
			out.Write(buf[:n])
		case bridge.FrameStderr:
			errs.Write(buf[:n])
		case bridge.FrameExit:
			h.t.Fatalf("ended (%s) before %q; stdout %q stderr %q", buf[:n], want, out.String(), errs.String())
		}
	}
	return out.String(), errs.String()
}

func (h *helper) end() (stdout, stderr []byte, exit bridge.Exit) {
	h.t.Helper()
	var out, errs bytes.Buffer
	buf := make([]byte, bridge.MaxFrame)
	for {
		s, n, err := bridge.ReadFrame(h.r, buf)
		if err != nil {
			h.t.Fatalf("no exit: %v; stdout %q stderr %q", err, out.String(), errs.String())
		}
		switch s {
		case bridge.FrameStdout:
			out.Write(buf[:n])
		case bridge.FrameStderr:
			errs.Write(buf[:n])
		case bridge.FrameExit:
			if err := json.Unmarshal(buf[:n], &exit); err != nil {
				h.t.Fatal(err)
			}
			return out.Bytes(), errs.Bytes(), exit
		}
	}
}

func TestABridgedCommandRunsOnTheMac(t *testing.T) {
	r := newBridgeRig(t, "")
	h, err := r.helper("dev", &bridge.Header{Verb: bridge.VerbRun, Dir: "/mnt/mac/private/tmp",
		Argv: []string{"sh", "-c", `uname -s; pwd; echo "$FEV_MACHINE $HOME $V"; echo oops >&2; exit 3`}, Env: []string{"V=passed"}})
	if err != nil {
		t.Fatal(err)
	}
	h.send(bridge.FrameStdin, nil)
	out, errs, exit := h.end()
	want := fmt.Sprintf("Darwin\n/private/tmp\ndev %s passed\n", os.Getenv("HOME"))
	if string(out) != want || string(errs) != "oops\n" || exit.Status() != 3 {
		t.Errorf("stdout %q (want %q), stderr %q, exit %+v", out, want, errs, exit)
	}
}

func TestTheWorkingDirectoryAndArgumentsAreTranslated(t *testing.T) {
	r := newBridgeRig(t, "")
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.b.home = home
	tmp, _ := filepath.EvalSymlinks(os.TempDir())
	run := func(machine, dir string, argv ...string) (string, string) {
		t.Helper()
		h, err := r.helper(machine, &bridge.Header{Verb: bridge.VerbRun, Dir: dir, Argv: argv})
		if err != nil {
			t.Fatal(err)
		}
		h.send(bridge.FrameStdin, nil)
		out, errs, exit := h.end()
		if exit.Status() != 0 {
			t.Fatalf("%s from %s: %+v %q", machine, dir, exit, errs)
		}
		return string(out), string(errs)
	}
	pwdArg := []string{"sh", "-c", `echo "$(pwd -P) $1"`, "sh"}
	if out, _ := run("dev", "/mnt/mac/private/tmp", append(pwdArg, "/mnt/mac/private/tmp")...); out != "/private/tmp /private/tmp\n" {
		t.Errorf("from /mnt/mac/private/tmp: %q", out)
	}
	for _, c := range []struct{ machine, dir, arg, arged, said string }{
		{"dev", "/home/me/src", "/mnt/mac/private/tmp", "/private/tmp", ""},
		{"dev", "/", "x", "x", ""},
		{"noshare", "/mnt/mac/private/tmp", "/mnt/mac/x", "/mnt/mac/x", ""},
		{"dev", "/mnt/mac/no/such/dir", "y", "y", "mac: /no/such/dir is not a directory on the Mac; the command runs in an empty one\n"},
	} {
		out, errs := run(c.machine, c.dir, append(pwdArg, c.arg)...)
		dir, arg, _ := strings.Cut(strings.TrimSuffix(out, "\n"), " ")
		if !strings.HasPrefix(dir, filepath.Join(tmp, "fev-mac-")) || arg != c.arged || errs != c.said {
			t.Errorf("%s from %s: ran in %q with %q, said %q", c.machine, c.dir, dir, arg, errs)
			continue
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s from %s: the empty directory %s was left: %v", c.machine, c.dir, dir, err)
		}
	}
	out, _ := run("dev", "/home/me", "sh", "-c", "rm -rf x && touch made && pwd -P")
	dir := strings.TrimSuffix(out, "\n")
	if st, err := os.Stat(filepath.Join(home, "x")); err != nil || !st.IsDir() {
		t.Errorf("mac rm -rf x from the machine's home reached the Mac's: %v", err)
	}
	if dir == home || !strings.HasPrefix(dir, filepath.Join(tmp, "fev-mac-")) {
		t.Errorf("ran in %q", dir)
	} else if _, err := os.Stat(filepath.Join(dir, "made")); err != nil {
		t.Errorf("what the command made was removed: %v", err)
	} else {
		if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("the directory: %v %v", st.Mode().Perm(), err)
		}
		os.RemoveAll(dir)
	}
}

func TestStandardInputReachesTheMacByteForByte(t *testing.T) {
	r := newBridgeRig(t, "")
	h, err := r.helper("dev", &bridge.Header{Verb: bridge.VerbRun, Argv: []string{"cat"}})
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 3<<20+17)
	rand.Read(data)
	go func() {
		for p := data; len(p) > 0; {
			n := min(len(p), 100_000)
			bridge.WriteFrame(h.c, bridge.FrameStdin, p[:n])
			p = p[n:]
		}
		bridge.WriteFrame(h.c, bridge.FrameStdin, nil)
	}()
	out, _, exit := h.end()
	if !bytes.Equal(out, data) || exit.Status() != 0 {
		t.Errorf("%d bytes back of %d, equal %v, exit %+v", len(out), len(data), bytes.Equal(out, data), exit)
	}
}

func TestAProgramTheMacLacksIs127(t *testing.T) {
	r := newBridgeRig(t, "")
	h, err := r.helper("dev", &bridge.Header{Verb: bridge.VerbRun, Argv: []string{"no-such-program-anywhere"}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, exit := h.end()
	if exit.Code != 127 || !strings.Contains(exit.Error, "not found") {
		t.Errorf("exit %+v", exit)
	}
}

func TestASignalReachesTheCommand(t *testing.T) {
	r := newBridgeRig(t, "")
	h, err := r.helper("dev", &bridge.Header{Verb: bridge.VerbRun, Argv: []string{"sh", "-c", `trap 'echo caught; exit 7' INT; echo ready; while :; do sleep 0.05; done`}})
	if err != nil {
		t.Fatal(err)
	}
	h.until("ready")
	h.control(bridge.Control{Signal: "INT"})
	out, _, exit := h.end()
	if !strings.Contains(string(out), "caught") || exit.Status() != 7 {
		t.Errorf("stdout %q, exit %+v", out, exit)
	}
}

func TestATerminalIsSizedResizedAndInterrupted(t *testing.T) {
	r := newBridgeRig(t, "")
	h, err := r.helper("dev", &bridge.Header{Verb: bridge.VerbRun, Stdin: true, Stdout: true, Stderr: true, Rows: 30, Cols: 100, Env: []string{"TERM=xterm"},
		Argv: []string{"sh", "-c", `[ -t 0 ] && [ -t 1 ] && echo terminal; stty size; read x; stty size; trap 'echo caught; exit 5' INT; echo ready; while :; do sleep 0.05; done`}})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := h.until("30 100")
	h.control(bridge.Control{Rows: 50, Cols: 132})
	h.send(bridge.FrameStdin, []byte("go\r"))
	out, _ := h.until("ready")
	if !strings.Contains(first, "terminal") || !strings.Contains(out, "50 132") {
		t.Errorf("output %q, then %q", first, out)
	}
	h.send(bridge.FrameStdin, []byte{3})
	rest, _, exit := h.end()
	if !strings.Contains(string(rest), "caught") || exit.Status() != 5 {
		t.Errorf("after ^C: %q, exit %+v", rest, exit)
	}
}

func TestTerminalsAreDecidedPerStream(t *testing.T) {
	r := newBridgeRig(t, "")
	h, err := r.helper("dev", &bridge.Header{Verb: bridge.VerbRun, Stdin: true, Rows: 24, Cols: 80,
		Argv: []string{"sh", "-c", `[ -t 0 ] && echo in; [ -t 1 ] || echo out-piped; [ -t 2 ] || echo err-piped >&2`}})
	if err != nil {
		t.Fatal(err)
	}
	out, errs, exit := h.end()
	if string(out) != "in\nout-piped\n" || string(errs) != "err-piped\n" || exit.Status() != 0 {
		t.Errorf("stdout %q stderr %q exit %+v", out, errs, exit)
	}
}

func TestAHelperThatGoesAwayHangsUpTheCommand(t *testing.T) {
	r := newBridgeRig(t, "")
	mark := filepath.Join(r.dir, "hup")
	h, err := r.helper("dev", &bridge.Header{Verb: bridge.VerbRun, Argv: []string{"sh", "-c", `trap 'touch "$1"; exit 0' HUP; echo ready; while :; do sleep 0.05; done`, "sh", mark}})
	if err != nil {
		t.Fatal(err)
	}
	h.until("ready")
	h.c.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(mark); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the command had no hangup")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestInfoAndNotify(t *testing.T) {
	r := newBridgeRig(t, "")
	h, err := r.helper("noshare", &bridge.Header{Verb: bridge.VerbInfo})
	if err != nil {
		t.Fatal(err)
	}
	var i bridge.Info
	if err := json.NewDecoder(h.r).Decode(&i); err != nil {
		t.Fatal(err)
	}
	if i.Home != os.Getenv("HOME") || i.User != "me" || i.Share {
		t.Errorf("info %+v", i)
	}
	if _, err := r.helper("dev", &bridge.Header{Verb: bridge.VerbNotify, Message: "built"}); err != nil {
		t.Fatal(err)
	}
	if got := <-r.notified; got != [2]string{"dev", "built"} {
		t.Errorf("notified %v", got)
	}
	if _, err := r.helper("dev", &bridge.Header{Verb: bridge.VerbLink, Names: []string{"x"}}); err == nil {
		t.Error("the daemon took a link, which is init's")
	}
	if _, err := r.helper("ghost", &bridge.Header{Verb: bridge.VerbInfo}); err == nil || !strings.Contains(err.Error(), "no machine ghost") {
		t.Errorf("a machine the daemon does not know: %v", err)
	}
}

func TestARequestWithoutAUIDIsRefused(t *testing.T) {
	r := newBridgeRig(t, "")
	if _, refused := r.ask(agent.StreamMac, "dev", nil); refused == nil || !strings.Contains(refused.Reason, "uid") {
		t.Errorf("refusal %+v", refused)
	}
	if _, refused := r.ask("printer", "dev", uid(0)); refused == nil || !strings.Contains(refused.Reason, "no service") {
		t.Errorf("refusal %+v", refused)
	}
}

func TestAMachinesAgentReachesTheMacsAgent(t *testing.T) {
	none := newBridgeRig(t, "")
	if _, refused := none.ask(agent.StreamSSHAgent, "dev", uid(501)); refused == nil || refused.Reason != "SSH_AUTH_SOCK is not set" {
		t.Errorf("refusal %+v", refused)
	}

	dir, err := os.MkdirTemp("/tmp", "bv.")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "agent.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b, _ := io.ReadAll(c)
				c.Write(append([]byte("signed:"), b...))
			}()
		}
	}()
	r := newBridgeRig(t, sock)
	c, refused := r.ask(agent.StreamSSHAgent, "dev", uid(0))
	if refused != nil {
		t.Fatalf("refused: %s", refused.Reason)
	}
	c.Write([]byte("sign this"))
	c.(*net.UnixConn).CloseWrite()
	got, _ := io.ReadAll(c)
	if string(got) != "signed:sign this" {
		t.Errorf("the agent answered %q", got)
	}
}

func TestAMacProgramRunByPathIsTheFileAtThatPath(t *testing.T) {
	r := newBridgeRig(t, "")
	exec := func(machine, p string, argv ...string) (string, bridge.Exit) {
		t.Helper()
		h, err := r.helper(machine, &bridge.Header{Verb: bridge.VerbExec, Path: p, Dir: "/mnt/mac/private/tmp", Argv: argv})
		if err != nil {
			t.Fatal(err)
		}
		h.send(bridge.FrameStdin, nil)
		out, _, exit := h.end()
		return string(out), exit
	}
	if out, exit := exec("dev", "/mnt/mac/bin/sh", "my-sh", "-c", `echo "$0 $(pwd -P)"; exit 4`); out != "my-sh /private/tmp\n" || exit.Status() != 4 {
		t.Errorf("/mnt/mac/bin/sh: %q %+v", out, exit)
	}
	if out, exit := exec("dev", "/mnt/mac/usr/bin/true", "false"); out != "" || exit.Status() != 0 || exit.Error != "" {
		t.Errorf("true run as false: %q %+v", out, exit)
	}
	if _, exit := exec("dev", "/home/me/a.out", "a.out"); exit.Code != 126 || !strings.Contains(exit.Error, "the Mac cannot see") {
		t.Errorf("a machine's own file: %+v", exit)
	}
	if _, exit := exec("dev", "/mnt/mac/no/such/program", "program"); exit.Code != 127 || !strings.Contains(exit.Error, "/no/such/program") {
		t.Errorf("a path the Mac lacks: %+v", exit)
	}
	if _, exit := exec("noshare", "/mnt/mac/bin/sh", "sh", "-c", "exit 0"); exit.Code != 126 {
		t.Errorf("no share: %+v", exit)
	}
	view := t.TempDir()
	os.MkdirAll(filepath.Join(view, "home/me"), 0o755)
	if err := os.WriteFile(filepath.Join(view, "home/me/a.out"), []byte("#!/bin/sh\necho \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.b.view = func(string) string { return view }
	if out, exit := exec("dev", "/home/me/a.out", "a.out", "from", "the view"); out != "from the view\n" || exit.Status() != 0 {
		t.Errorf("from the view: %q %+v", out, exit)
	}
}
