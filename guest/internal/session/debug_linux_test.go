package session

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"fev/guest/debug/attach"
	api "fev/proto/agent"
)

func targetGoneDial(t *testing.T, body func() (int, string)) func(ctx context.Context) (net.Conn, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json") {
			code, js := body()
			if code == http.StatusOK {
				w.Write([]byte(js))
				return
			}
			http.Error(w, `{"message":"No such container"}`, code)
			return
		}
		http.Error(w, `{"message":"No such object"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

const runningCtr = `{"Id":"c1","Name":"/c1","State":{"Running":true,"Pid":7},"Config":{"Image":"alpine"}}`
const exitedCtr = `{"Id":"c1","Name":"/c1","State":{"Running":false,"Pid":0},"Config":{"Image":"alpine"}}`

func TestTargetGoneWhenTheTargetIsRemoved(t *testing.T) {
	old := targetGoneWindow
	targetGoneWindow = 500 * time.Millisecond
	defer func() { targetGoneWindow = old }()

	d := &Debug{Dial: targetGoneDial(t, func() (int, string) { return http.StatusNotFound, "" })}
	x := &api.ExecExit{ExitCode: 128 + int(unix.SIGKILL)}
	d.targetGone(&attach.Resolved{ID: "c1", Kind: attach.DebugKindRunning})(x)
	if x.Reason != api.ExecReasonTargetGone {
		t.Fatalf("a removed target gave reason %q, want %q", x.Reason, api.ExecReasonTargetGone)
	}
}

func TestTargetGoneWhenTheTargetStopsRunning(t *testing.T) {
	old := targetGoneWindow
	targetGoneWindow = 500 * time.Millisecond
	defer func() { targetGoneWindow = old }()

	d := &Debug{Dial: targetGoneDial(t, func() (int, string) { return http.StatusOK, exitedCtr })}
	x := &api.ExecExit{Signal: int(unix.SIGKILL), ExitCode: -1}
	d.targetGone(&attach.Resolved{ID: "c1", Kind: attach.DebugKindRunning})(x)
	if x.Reason != api.ExecReasonTargetGone {
		t.Fatalf("a stopped target gave reason %q, want %q", x.Reason, api.ExecReasonTargetGone)
	}
}

func TestTargetGoneResolvesAfterTheEngineCatchesUp(t *testing.T) {
	old := targetGoneWindow
	targetGoneWindow = 3 * time.Second
	defer func() { targetGoneWindow = old }()

	var n int32
	d := &Debug{Dial: targetGoneDial(t, func() (int, string) {
		if atomic.AddInt32(&n, 1) <= 3 {
			return http.StatusOK, runningCtr
		}
		return http.StatusNotFound, ""
	})}
	x := &api.ExecExit{ExitCode: 128 + int(unix.SIGKILL)}
	d.targetGone(&attach.Resolved{ID: "c1", Kind: attach.DebugKindRunning})(x)
	if x.Reason != api.ExecReasonTargetGone {
		t.Fatalf("the poll did not catch the target's departure: reason %q", x.Reason)
	}
	if atomic.LoadInt32(&n) < 4 {
		t.Fatalf("only %d probes; the poll should have retried past the race", n)
	}
}

func TestTargetGoneLeftUnsetWhileTheTargetRuns(t *testing.T) {
	old := targetGoneWindow
	targetGoneWindow = 400 * time.Millisecond
	defer func() { targetGoneWindow = old }()

	d := &Debug{Dial: targetGoneDial(t, func() (int, string) { return http.StatusOK, runningCtr })}
	x := &api.ExecExit{Signal: int(unix.SIGKILL), ExitCode: -1}
	d.targetGone(&attach.Resolved{ID: "c1", Kind: attach.DebugKindRunning})(x)
	if x.Reason != "" {
		t.Fatalf("a shell killed under a running target set reason %q, want none", x.Reason)
	}
}

func TestTargetGoneIgnoresANormalExit(t *testing.T) {
	old := targetGoneWindow
	targetGoneWindow = 400 * time.Millisecond
	defer func() { targetGoneWindow = old }()

	var called int32
	d := &Debug{Dial: func(ctx context.Context) (net.Conn, error) {
		atomic.AddInt32(&called, 1)
		return nil, context.Canceled
	}}
	x := &api.ExecExit{ExitCode: 0}
	d.targetGone(&attach.Resolved{ID: "c1", Kind: attach.DebugKindRunning})(x)
	if x.Reason != "" {
		t.Fatalf("a normal exit set reason %q", x.Reason)
	}
	if atomic.LoadInt32(&called) != 0 {
		t.Fatalf("a normal exit probed the engine %d times", called)
	}
}
