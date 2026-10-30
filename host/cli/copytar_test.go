package cli

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/relay"
)

func isolatedFake(t *testing.T, run sessionRun) *fake {
	f := newFake(t)
	f.on(api.MethodMachinesList, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return api.Machines{Default: "iso", Machines: []api.Machine{{Name: "iso", State: api.MachineRunning, Isolated: true}, {Name: "dev", State: api.MachineRunning}}}, nil
	})
	fakeSessions(f, run)
	return f
}

func TestPushAndPullReachAnIsolatedMachineAsTar(t *testing.T) {
	noTerminalEnv(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "proj", "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "proj", "sub", "a.txt"), []byte("alpha"), 0o640)
	os.Symlink("sub/a.txt", filepath.Join(dir, "proj", "link"))
	os.WriteFile(filepath.Join(dir, "one"), []byte("1"), 0o600)

	var mu sync.Mutex
	var lines []string
	var got map[string]string
	f := isolatedFake(t, func(p api.SessionStartParams, stream net.Conn, _ <-chan int) api.SessionExit {
		mu.Lock()
		lines = append(lines, fmt.Sprintf("%s: %s %q login=%v shell=%v", p.Machine, p.Copy, p.Argv, p.Login, p.Shell))
		mu.Unlock()
		var in bytes.Buffer
		buf := make([]byte, relay.MaxFrame)
		for {
			kind, n, err := relay.ReadFrame(stream, buf)
			if err != nil || kind == relay.Stdin && n == 0 {
				break
			}
			in.Write(buf[:n])
		}
		m := map[string]string{}
		tr := tar.NewReader(&in)
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(tr)
			m[h.Name] = string(h.Typeflag) + " " + h.Linkname + string(b)
		}
		mu.Lock()
		got = m
		mu.Unlock()
		return api.SessionExit{}
	})
	f.run("push", filepath.Join(dir, "proj"), filepath.Join(dir, "one"), "dst").want(t, 0)
	mu.Lock()
	wantLine := `: in ["dst" "proj" "one"] login=false shell=false`
	if len(lines) != 1 || lines[0] != wantLine {
		t.Errorf("push's command:\n%q\nwant\n%q", lines, wantLine)
	}
	var names []string
	for n := range got {
		names = append(names, n+"="+got[n])
	}
	sort.Strings(names)
	want := []string{"one=0 1", "proj/=5 ", "proj/link=2 sub/a.txt", "proj/sub/=5 ", "proj/sub/a.txt=0 alpha"}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Errorf("push sent %q, want %q", names, want)
	}
	mu.Unlock()

	f.run("push", "-m", "dev", filepath.Join(dir, "one")).want(t, 0)
	mu.Lock()
	if last := lines[len(lines)-1]; last != fmt.Sprintf("dev:  [%q] login=true shell=true", "cp -R -- "+linuxPath(filepath.Join(dir, "one"))+` "$HOME"/`) {
		t.Errorf("a trusted machine's push: %q", last)
	}
	mu.Unlock()

	home, _ := os.UserHomeDir()
	for _, c := range []struct{ arg, mac, want string }{
		{"", "", "."}, {"~", "", "."}, {"~/a/b", "", "./a/b"}, {"rel/x", "", "rel/x"}, {"/tmp/", "", "/tmp/"},
		{home, home, "."}, {home + "/src", home, "./src"}, {"/etc/hostname", home, "/etc/hostname"},
	} {
		if got := copyPath(c.arg, c.mac); got != c.want {
			t.Errorf("copyPath(%q, %q) = %q, want %q", c.arg, c.mac, got, c.want)
		}
	}
}

func sends(add func(tw *tar.Writer)) sessionRun {
	return func(p api.SessionStartParams, stream net.Conn, _ <-chan int) api.SessionExit {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		add(tw)
		tw.Close()
		for data := b.Bytes(); len(data) > 0; {
			n := min(len(data), relay.MaxFrame)
			relay.WriteFrame(stream, relay.Stdout, data[:n])
			data = data[n:]
		}
		return api.SessionExit{}
	}
}

func entry(tw *tar.Writer, name string, typ byte, body, link string) {
	tw.WriteHeader(&tar.Header{Name: name, Typeflag: typ, Mode: 0o644, Size: int64(len(body)), Linkname: link})
	if body != "" {
		tw.Write([]byte(body))
	}
}

func TestPullFromAnIsolatedMachineStaysInItsDestination(t *testing.T) {
	noTerminalEnv(t)
	dest := t.TempDir()
	os.WriteFile(filepath.Join(dest, "f"), []byte("old"), 0o644)
	sent := sends(func(tw *tar.Writer) {
		entry(tw, "./", tar.TypeDir, "", "")
		entry(tw, "./f", tar.TypeReg, "new", "")
		entry(tw, "./d/", tar.TypeDir, "", "")
		entry(tw, "./d/a", tar.TypeReg, "A", "")
		entry(tw, "./d/l", tar.TypeSymlink, "", "a")
	})
	var asked api.SessionStartParams
	f := isolatedFake(t, func(p api.SessionStartParams, stream net.Conn, x <-chan int) api.SessionExit {
		asked = p
		return sent(p, stream, x)
	})
	f.run("pull", "~/f", "~/d", dest).want(t, 0)
	if asked.Copy != agent.CopyOut || strings.Join(asked.Argv, " ") != "./f ./d" || asked.Login || asked.Shell {
		t.Errorf("pull asked the machine for %+v", asked)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "f")); string(b) != "new" {
		t.Errorf("f is %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "d", "a")); string(b) != "A" {
		t.Errorf("d/a is %q", b)
	}
	if l, _ := os.Readlink(filepath.Join(dest, "d", "l")); l != "a" {
		t.Errorf("d/l is a link to %q", l)
	}
	if left, _ := filepath.Glob(filepath.Join(dest, ".fev-pull-*")); len(left) != 0 {
		t.Errorf("scratch left behind: %v", left)
	}

	outside := t.TempDir()
	for name, add := range map[string]func(tw *tar.Writer){
		"through a link": func(tw *tar.Writer) {
			entry(tw, "./x", tar.TypeSymlink, "", outside)
			entry(tw, "./x/evil", tar.TypeReg, "pwned", "")
		},
		"climbing": func(tw *tar.Writer) { entry(tw, "../evil", tar.TypeReg, "pwned", "") },
		"absolute": func(tw *tar.Writer) { entry(tw, filepath.Join(outside, "evil"), tar.TypeReg, "pwned", "") },
	} {
		d2 := t.TempDir()
		g := isolatedFake(t, sends(add))
		if r := g.run("pull", "~/x", d2); r.code != ExitFailed {
			t.Errorf("%s: %+v", name, r)
		}
		if _, err := os.Lstat(filepath.Join(outside, "evil")); err == nil {
			t.Fatalf("%s: a file was written outside the destination", name)
		}
		if _, err := os.Lstat(filepath.Join(filepath.Dir(d2), "evil")); err == nil {
			t.Fatalf("%s: a file was written beside the destination", name)
		}
	}
}
