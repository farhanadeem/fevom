package privhelper_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fev/host/privhelper"
	"fev/host/privhelper/fakesys"
	api "fev/proto/privhelper"
)

type heldProbe struct {
	*fakesys.System
	begun   chan string
	release chan struct{}
}

func (h *heldProbe) Probe(ctx context.Context, names []string, kinds []privhelper.ProbeKind, d time.Duration) (map[string][]privhelper.Answer, error) {
	h.begun <- names[0]
	select {
	case <-h.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return h.System.Probe(ctx, names, kinds, d)
}

func serveOn(t *testing.T, root, home string, sys privhelper.System) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bv.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	srv, err := privhelper.New(privhelper.Config{
		Record: api.InstallRecord{UID: os.Getuid(), Home: home, Base: "198.19.0.0/16", ResolverPort: 15353, ResolverForm: api.ResolverFormFile},
		Root:   root, Sys: sys, Logf: t.Logf,
		PeerOf: func(*net.UnixConn) (privhelper.Peer, error) { return privhelper.Peer{UID: os.Getuid()}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "p.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.AcceptUnix()
			if err != nil {
				return
			}
			go srv.ServeConn(c)
		}
	}()
	return sock
}

func setNames(sock string, names ...string) (api.LocalNames, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var res api.LocalNames
	_, err := api.Call(ctx, sock, api.MethodLocalNamesSet, api.LocalNamesParams{Names: names}, &res)
	return res, err
}

func tooSoon(err error) bool {
	var e *api.Error
	return errors.As(err, &e) && e.Code == api.CodeTooSoon
}

func TestTwoHelpersAtOnceCannotStepAroundTheRateLimit(t *testing.T) {
	dir := t.TempDir()
	root, home := filepath.Join(dir, "root"), filepath.Join(dir, "home")
	for _, d := range []string{filepath.Join(root, "etc"), home} {
		os.MkdirAll(d, 0o755)
	}
	sys := &heldProbe{System: fakesys.New(), begun: make(chan string, 2), release: make(chan struct{})}
	a, b := serveOn(t, root, home, sys), serveOn(t, root, home, sys)

	type out struct {
		res api.LocalNames
		err error
	}
	results := make(chan out, 2)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err := setNames(a, "first.local")
		results <- out{res, err}
	}()
	<-sys.begun
	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err := setNames(b, "second.local")
		results <- out{res, err}
	}()
	select {
	case n := <-sys.begun:
		t.Errorf("a second helper probed %s while the first was probing", n)
	case <-time.After(300 * time.Millisecond):
	}
	close(sys.release)
	wg.Wait()
	close(results)
	took, refused := 0, 0
	for o := range results {
		switch {
		case o.err == nil && len(o.res.Names) == 1 && o.res.Names[0].State == api.NamePresent:
			took++
		case tooSoon(o.err):
			refused++
		default:
			t.Errorf("a call: %+v %v", o.res, o.err)
		}
	}
	if took != 1 || refused != 1 {
		t.Errorf("%d calls added names and %d were too soon; want 1 and 1", took, refused)
	}
	des, _ := os.ReadDir(filepath.Join(root, "etc/resolver"))
	if len(des) != 1 {
		t.Errorf("entries: %v", des)
	}
}

func TestAnEmptySetDoesNotResetTheRateLimit(t *testing.T) {
	r := newRig(t, nil)
	r.mu.Lock()
	r.now = time.Now()
	r.mu.Unlock()
	if _, err := setNames(r.sock, "a.local"); err != nil {
		t.Fatal(err)
	}
	r.advance(100 * time.Millisecond)
	if _, err := setNames(r.sock); err != nil {
		t.Fatalf("removing: %v", err)
	}
	r.advance(100 * time.Millisecond)
	if _, err := setNames(r.sock, "b.local"); !tooSoon(err) {
		t.Errorf("adding just after an empty set: %v, want too soon", err)
	}
	r.advance(time.Second)
	if res, err := setNames(r.sock, "b.local"); err != nil || len(res.Names) != 1 || res.Names[0].State != api.NamePresent {
		t.Errorf("a second later: %+v %v", res, err)
	}
}

func TestTheNamesLockIsTheHelpersAlone(t *testing.T) {
	r := newRig(t, nil)
	lock := filepath.Join(r.root, privhelper.LockPath)
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lock, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := setNames(r.sock, "a.local"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(lock)
	if err != nil {
		t.Fatal(err)
	}
	if m := fi.Mode().Perm(); m != 0o600 {
		t.Errorf("the names' lock is %v, want -rw-------", fi.Mode())
	}
}
