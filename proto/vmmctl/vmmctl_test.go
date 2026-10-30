package vmmctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"fev/proto/contract"
	"fev/proto/jsonrpc"
)

var update = flag.Bool("update", false, "rewrite fixtures/wire/session.jsonl from the table in this file")

func TestFixturesMatchTheSchema(t *testing.T) {
	fixtures, err := Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range contract.Verify(fixtures, Schema) {
		t.Error(err)
	}
}

func fixture(t *testing.T, name string) Fixture {
	t.Helper()
	data, err := fixtureFiles.ReadFile("fixtures/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var fx Fixture
	if err := contract.StrictDecode(data, &fx); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return fx
}

func TestTheFixturesObeyTheRulesOfTheInterface(t *testing.T) {
	fixtures, err := Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, fx := range fixtures {
		switch fx.Method {
		case MethodPause:
			r, _ := jsonrpc.Bind[PauseResult](fx.Result)
			seen[fmt.Sprintf("pause changed=%v", r.Changed)] = true
			if r.State != StatePaused || r.VCPUs <= 0 || r.PausedAtNS == 0 {
				t.Errorf("pause ends as %+v", r)
			}
			if r.Changed == (r.TookNS == 0) {
				t.Errorf("pause: changed is %v and it took %d ns", r.Changed, r.TookNS)
			}
		case MethodResume:
			r, _ := jsonrpc.Bind[ResumeResult](fx.Result)
			seen[fmt.Sprintf("resume changed=%v", r.Changed)] = true
			if r.State != StateRunning || r.VCPUs <= 0 {
				t.Errorf("resume ends as %+v", r)
			}
			if r.Changed == (r.TookNS == 0) || r.Changed == (r.OffsetNS == 0) {
				t.Errorf("resume: changed is %v, it took %d ns and applied %d ns", r.Changed, r.TookNS, r.OffsetNS)
			}
			if r.TotalOffsetNS < r.OffsetNS {
				t.Errorf("resume: the total offset %d is less than this call's %d", r.TotalOffsetNS, r.OffsetNS)
			}
		case MethodStats:
			r, _ := jsonrpc.Bind[Stats](fx.Result)
			seen["stats "+r.State] = true
			seen[fmt.Sprintf("stats windows=%v", r.Windows != nil)] = true
			if r.APIVersion != APIVersion || r.Build == "" || r.AtNS == 0 {
				t.Errorf("stats does not say what it is: %+v", r)
			}
			switch r.State {
			case StateRunning, StateStopping:
				if r.PausedAtNS != 0 {
					t.Errorf("stats: %s since a pause at %d", r.State, r.PausedAtNS)
				}
			case StatePaused:
				if r.PausedAtNS == 0 || r.PausedAtNS > r.AtNS || r.Pauses == 0 {
					t.Errorf("stats: paused at %d, taken at %d, after %d pauses", r.PausedAtNS, r.AtNS, r.Pauses)
				}
			default:
				t.Errorf("stats: state %q", r.State)
			}
			if held := r.Memory.ResidentBytes + r.Memory.CompressedBytes; held > r.Memory.GuestBytes {
				t.Errorf("stats: %d bytes of guest RAM held out of %d", held, r.Memory.GuestBytes)
			}
			if r.Windows == nil {
				continue
			}
			for name, w := range map[string]*Window{"data_disk": r.Windows.DataDisk, "file_share": r.Windows.FileShare} {
				if w == nil {
					continue
				}
				seen["window "+w.State] = true
				switch w.State {
				case WindowOn:
					if w.SizeBytes == 0 || w.GuestAddr == 0 || w.Reason != "" {
						t.Errorf("stats: %s is on as %+v", name, w)
					}
				case WindowOff, WindowFailed:
					if w.SizeBytes != 0 || w.GuestAddr != 0 || w.Reason == "" {
						t.Errorf("stats: %s is %s as %+v", name, w.State, w)
					}
				default:
					t.Errorf("stats: %s is in state %q", name, w.State)
				}
				if w.Live > w.Mappings || w.Unmappings > w.Mappings {
					t.Errorf("stats: %s has %d live and %d removed of %d mappings", name, w.Live, w.Unmappings, w.Mappings)
				}
			}
		}
	}
	for _, want := range []string{
		"pause changed=true", "pause changed=false", "resume changed=true", "resume changed=false",
		"stats running", "stats paused", "stats stopping", "stats windows=false", "stats windows=true",
		"window on", "window off", "window failed",
	} {
		if !seen[want] {
			t.Errorf("no fixture shows: %s", want)
		}
	}
}

type step struct {
	call, fixture string
	code          int
	message       string
	note          string
	params        string
}

var session = []step{
	{call: MethodStats, fixture: "stats"},
	{call: MethodPause, fixture: "pause"},
	{call: MethodPause, fixture: "pause.unchanged"},
	{call: MethodStats, fixture: "stats.paused"},
	{call: MethodResume, fixture: "resume"},
	{call: MethodResume, fixture: "resume.unchanged"},
	{call: MethodBudget, fixture: "budget", params: "budget"},
	{note: NotifySpace, fixture: "space"},
	{note: NotifySpace, fixture: "space.held"},
	{call: MethodBudget, fixture: "budget.held", params: "budget.held"},
	{note: NotifyReset, fixture: "reset"},
	{call: "hotplug", code: jsonrpc.CodeMethodNotFound, message: `method "hotplug" not found`},
	{call: MethodStop, fixture: "stop"},
	{call: MethodPause, code: CodeStopping, message: "the machine is stopping"},
	{call: MethodStats, fixture: "stats.window-failed"},
	{call: MethodKill, fixture: "kill"},
}

func compact(t *testing.T, data []byte) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, data); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func transcript(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	id := 0
	for _, s := range session {
		if s.note != "" {
			fmt.Fprintf(&out, "< {\"jsonrpc\":\"2.0\",\"method\":%q,\"params\":%s}\n", s.note, compact(t, fixture(t, s.fixture).Params))
			continue
		}
		id++
		if s.params != "" {
			fmt.Fprintf(&out, "> {\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":%q,\"params\":%s}\n", id, s.call, compact(t, fixture(t, s.params).Params))
		} else {
			fmt.Fprintf(&out, "> {\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":%q}\n", id, s.call)
		}
		if s.code != 0 {
			message, _ := json.Marshal(s.message)
			fmt.Fprintf(&out, "< {\"jsonrpc\":\"2.0\",\"id\":%d,\"error\":{\"code\":%d,\"message\":%s}}\n", id, s.code, message)
			continue
		}
		fmt.Fprintf(&out, "< {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":%s}\n", id, compact(t, fixture(t, s.fixture).Result))
	}
	return out.Bytes()
}

func TestTheWireSessionFileIsCurrent(t *testing.T) {
	want := transcript(t)
	if *update {
		if err := os.WriteFile("fixtures/wire/session.jsonl", want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !bytes.Equal(WireSession(), want) {
		t.Errorf("fixtures/wire/session.jsonl is stale: run go test ./vmmctl -update\nfile:\n%s\nwant:\n%s", WireSession(), want)
	}
}

type tap struct {
	io.ReadWriteCloser
	mu       sync.Mutex
	in, out  bytes.Buffer
	notified chan struct{}
}

func (p *tap) Read(b []byte) (int, error) {
	n, err := p.ReadWriteCloser.Read(b)
	p.mu.Lock()
	p.in.Write(b[:n])
	p.mu.Unlock()
	return n, err
}

func (p *tap) Write(b []byte) (int, error) {
	p.mu.Lock()
	p.out.Write(b)
	p.mu.Unlock()
	return p.ReadWriteCloser.Write(b)
}

func lines(b []byte) []string {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func sameJSON(a, b string) bool {
	var x, y any
	return json.Unmarshal([]byte(a), &x) == nil && json.Unmarshal([]byte(b), &y) == nil && reflect.DeepEqual(x, y)
}

func fakeVMM(t *testing.T, rw io.ReadWriteCloser) {
	var mu sync.Mutex
	next := 0
	jsonrpc.NewConn(rw, jsonrpc.HandlerFunc(func(ctx context.Context, c *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		for next < len(session) && session[next].note != "" {
			next++
		}
		if next == len(session) || session[next].call != method {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInternalError, "the session does not call %s here", method)
		}
		s := session[next]
		next++
		var zero any = Empty{}
		if method == MethodBudget {
			zero = Baseline{}
		}
		if len(params) != 0 || method == MethodBudget {
			if err := Conforms(params, zero); err != nil {
				return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%s: %v", method, err)
			}
		}
		var notes []step
		for i := next; i < len(session) && session[i].note != ""; i++ {
			notes = append(notes, session[i])
		}
		if len(notes) != 0 {
			jsonrpc.AfterReply(ctx, func() {
				for _, note := range notes {
					c.Notify(note.note, fixture(t, note.fixture).Params)
				}
			})
		}
		if s.code != 0 {
			return nil, jsonrpc.Errorf(s.code, "%s", s.message)
		}
		if method == MethodKill {
			jsonrpc.AfterReply(ctx, func() { c.Close() })
		}
		return fixture(t, s.fixture).Result, nil
	}))
}

func TestTheTypedClientSpeaksTheWireSession(t *testing.T) {
	a, b := net.Pipe()
	wire := &tap{ReadWriteCloser: a}
	fakeVMM(t, b)
	c := NewClient(wire)
	resets := make(chan Reset, 1)
	c.OnReset(func(r Reset) { resets <- r })
	spaces := make(chan Space, 4)
	c.OnSpace(func(sp Space) { spaces <- sp })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i, s := range session {
		var got any
		var err error
		switch {
		case s.note == NotifySpace:
			select {
			case sp := <-spaces:
				same(t, s.note, sp, fixture(t, s.fixture).Params)
			case <-ctx.Done():
				t.Fatal("the space notification never arrived")
			}
			continue
		case s.note != "":
			select {
			case r := <-resets:
				got = r
				same(t, s.note, got, fixture(t, s.fixture).Params)
			case <-ctx.Done():
				t.Fatal("the reset notification never arrived")
			}
			continue
		case s.call == MethodPause:
			got, err = c.Pause(ctx)
		case s.call == MethodResume:
			got, err = c.Resume(ctx)
		case s.call == MethodStats:
			got, err = c.Stats(ctx)
		case s.call == MethodBudget:
			b, bindErr := jsonrpc.Bind[Baseline](fixture(t, s.params).Params)
			if bindErr != nil {
				t.Fatal(bindErr)
			}
			got, err = c.Budget(ctx, b)
		case s.call == MethodStop:
			got, err = Empty{}, c.Stop(ctx)
		case s.call == MethodKill:
			got, err = Empty{}, c.Kill(ctx)
		default:
			err = c.conn.Call(ctx, s.call, nil, nil)
		}
		if s.code != 0 {
			var e *jsonrpc.Error
			if !errors.As(err, &e) || e.Code != s.code || e.Message != s.message {
				t.Errorf("step %d, %s: error %v, want code %d %q", i, s.call, err, s.code, s.message)
			}
			if IsStopping(err) != (s.code == CodeStopping) {
				t.Errorf("step %d, %s: IsStopping(%v) is %v", i, s.call, err, IsStopping(err))
			}
			continue
		}
		if err != nil {
			t.Fatalf("step %d, %s: %v", i, s.call, err)
		}
		same(t, s.call, got, fixture(t, s.fixture).Result)
	}
	select {
	case <-c.Done():
	case <-ctx.Done():
		t.Fatal("the connection did not end after kill")
	}

	var down, up []string
	for _, line := range lines(WireSession()) {
		switch {
		case strings.HasPrefix(line, "> "):
			down = append(down, line[2:])
		case strings.HasPrefix(line, "< "):
			up = append(up, line[2:])
		default:
			t.Fatalf("a line of the transcript names no side: %q", line)
		}
	}
	wire.mu.Lock()
	sent, received := lines(wire.out.Bytes()), lines(wire.in.Bytes())
	wire.mu.Unlock()
	for name, pair := range map[string][2][]string{"the daemon": {sent, down}, "the VMM": {received, up}} {
		got, want := pair[0], pair[1]
		if len(got) != len(want) {
			t.Errorf("%s sent %d messages, the transcript has %d:\n%s", name, len(got), len(want), strings.Join(got, "\n"))
			continue
		}
		for i := range got {
			if !sameJSON(got[i], want[i]) {
				t.Errorf("message %d from %s:\n got %s\nwant %s", i+1, name, got[i], want[i])
			}
		}
	}
}

func same(t *testing.T, what string, got any, want json.RawMessage) {
	t.Helper()
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(string(data), string(want)) {
		t.Errorf("%s: client returned %s, the fixture says %s", what, data, want)
	}
}

func TestAVMMThatDoesNotAnswerAndOneThatDiesUnderKill(t *testing.T) {
	a, b := net.Pipe()
	release := make(chan struct{})
	jsonrpc.NewConn(b, jsonrpc.HandlerFunc(func(ctx context.Context, c *jsonrpc.Conn, method string, _ json.RawMessage) (any, error) {
		switch method {
		case MethodStats:
			select {
			case <-release:
			case <-c.Done():
			}
			return fixture(t, "stats.paused").Result, nil
		case MethodKill:
			c.Close()
			<-c.Done()
		}
		return Empty{}, nil
	}))
	c := NewClient(a)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Stats(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stats from a VMM that does not answer: %v", err)
	}
	close(release)
	long, cancelLong := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelLong()
	st, err := c.Stats(long)
	if err != nil || st.State != StatePaused {
		t.Fatalf("stats once it answers: %+v %v", st, err)
	}
	if err := c.Kill(long); err != nil {
		t.Errorf("kill of a VMM that died before replying: %v", err)
	}
	select {
	case <-c.Done():
	case <-long.Done():
		t.Error("the connection did not end")
	}
	if _, err := c.Pause(long); !errors.Is(err, jsonrpc.ErrClosed) {
		t.Errorf("pause after the VMM has gone: %v", err)
	}
}
