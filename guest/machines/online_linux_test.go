package machines

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"fev/guest/internal/logbuf"
)

func TestTheSeedIsWrittenInsideTheRoot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	outside := filepath.Join(dir, "outside")
	for _, d := range []string{root + "/var", root + "/etc/cloud", outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(root+cloudDisabled, nil, 0o644)
	if err := os.Symlink(outside, root+"/var/lib"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root+outside+"/cloud/seed/nocloud-net", 0o755); err != nil {
		t.Fatal(err)
	}
	seed := root + outside + "/cloud/seed/nocloud-net"
	os.WriteFile(seed+"/network-config", []byte("the template's\n"), 0o644)
	os.WriteFile(seed+"/meta-data", []byte("instance-id: dev\nlocal-hostname: dev\n"), 0o644)
	os.WriteFile(outside+"/victim", []byte("untouched"), 0o644)
	os.Symlink(outside+"/victim", seed+"/user-data")

	fd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := WriteCloudSeed(fd, CloudSeed{Name: "dev", UUID: "u-1", User: "ada", UserData: "#cloud-config\n{}\n"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(outside + "/victim"); string(b) != "untouched" {
		t.Fatalf("written through a link: %q", b)
	}
	if fi, err := os.Lstat(seed + "/user-data"); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("user-data: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(seed + "/network-config"); string(b) != "the template's\n" {
		t.Fatalf("network-config: %q", b)
	}
	if b, _ := os.ReadFile(seed + "/meta-data"); !strings.HasPrefix(string(b), "instance-id: u-1\n") {
		t.Fatalf("meta-data: %q", b)
	}
	if b, _ := os.ReadFile(seed + "/vendor-data"); string(b) != "#cloud-config\n{}\n" {
		t.Fatalf("vendor-data: %q", b)
	}
	if b, _ := os.ReadFile(root + CloudConfigFile); !strings.Contains(string(b), `name: "ada"`) {
		t.Fatalf("90-fev.cfg: %q", b)
	}
	if _, err := os.Lstat(root + cloudDisabled); err == nil {
		t.Fatal("cloud-init is still turned off")
	}
	if _, err := os.Stat(outside + "/cloud/seed/nocloud-net/meta-data"); err == nil {
		t.Fatal("the seed went outside the root")
	}
}

func TestTheConsoleLogOutlivesTheVM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.console")
	c := newConsoleLog(path, nil)
	line := strings.Repeat("x", 99) + "\r\n"
	n := consoleFileCap/len(line) + 50
	for i := 0; i < n; i++ {
		c.Write([]byte(line))
	}
	c.Write([]byte("the last line\r\n"))
	c.endBoot()
	if st, err := os.Stat(path); err != nil || st.Size() >= consoleFileCap {
		t.Fatalf("the file: %v %v", st, err)
	}
	if st, err := os.Stat(path + ".1"); err != nil || st.Size() > consoleFileCap {
		t.Fatalf("the file before it: %v %v", st, err)
	}
	again := newConsoleLog(path, nil)
	var got []string
	done := make(chan struct{})
	stop := again.ring.SubscribeMarked(true, func(e logbuf.Entry) { got = append(got, e.Text) }, func() { close(done) })
	<-done
	stop()
	if len(got) != consoleRingLines || got[len(got)-1] != "the last line" || got[0] != strings.Repeat("x", 99) {
		t.Fatalf("%d lines, the last %q", len(got), got[len(got)-1])
	}
}

func TestTheOnlineStepWaitsForTheResolver(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(root+"/etc", 0o755)
	os.WriteFile(root+"/etc/resolv.conf", nil, 0o644)
	go func() {
		time.Sleep(200 * time.Millisecond)
		os.WriteFile(root+"/etc/resolv.conf", []byte("search .\n"), 0o644)
		time.Sleep(100 * time.Millisecond)
		os.WriteFile(root+"/etc/resolv.conf", []byte("nameserver 198.19.0.1\n"), 0o644)
	}()
	t0 := time.Now()
	if err := waitResolver(root, time.Now().Add(10*time.Second), nil); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d < 250*time.Millisecond || d > 5*time.Second {
		t.Fatalf("returned after %v", d)
	}
	stub := t.TempDir()
	os.MkdirAll(stub+"/etc", 0o755)
	os.Symlink("../run/systemd/resolve/stub-resolv.conf", stub+"/etc/resolv.conf")
	if err := waitResolver(stub, time.Now().Add(time.Second), nil); err != nil {
		t.Fatalf("the stub: %v", err)
	}
	if err := waitResolver(root+"/none", time.Now().Add(time.Second), nil); err != nil {
		t.Fatalf("no file: %v", err)
	}
	empty := t.TempDir()
	os.MkdirAll(empty+"/etc", 0o755)
	os.WriteFile(empty+"/etc/resolv.conf", nil, 0o644)
	if err := waitResolver(empty, time.Now().Add(300*time.Millisecond), nil); err == nil {
		t.Fatal("an empty one that stays empty was waited past its bound")
	}
}

func TestTheOnlineStepEndsAtOnceWithNoNetwork(t *testing.T) {
	data := t.TempDir()
	root := filepath.Join(data, "alp")
	for _, d := range []string{"etc", "sbin"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "sbin", "apk"), nil, 0o755)
	os.WriteFile(filepath.Join(root, "etc", "resolv.conf"), nil, 0o644)
	m := &machine{rt: &Runtime{o: Options{DataDir: data}}, name: "alp"}
	r := &run{m: m}
	refused := errors.New("isolate_network: the Mac's addresses were not told within 30s (mac_addresses.set): not plugged in")
	b := &boot{ended: make(chan struct{}), netErr: refused}
	got := make(chan error, 1)
	go func() {
		_, err := r.online(b)
		got <- err
	}()
	select {
	case err := <-got:
		if err == nil || !strings.Contains(err.Error(), "has no network") || !strings.Contains(err.Error(), refused.Error()) {
			t.Fatalf("the step's end: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the online step of a machine with no network is still waiting after 5 s")
	}
}

func TestTheOnlineStepWaitsForNoCloudInitWithNoNetwork(t *testing.T) {
	refused := errors.New("isolate_network: the Mac's addresses were not told within 30s (mac_addresses.set): not plugged in")
	for _, c := range []struct {
		name  string
		files []string
		want  string
	}{
		{"u", []string{"usr/bin/cloud-init", "usr/bin/sudo"}, "cloud-init's run is not waited for"},
		{"a", []string{"usr/bin/cloud-init", "sbin/apk"}, "cloud-init's run is not waited for and sudo and ca-certificates cannot be added"},
		{"n", []string{"usr/bin/sudo"}, ""},
	} {
		data := t.TempDir()
		root := filepath.Join(data, c.name)
		for _, f := range c.files {
			if err := os.MkdirAll(filepath.Join(root, filepath.Dir(f)), 0o755); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(root, f), nil, 0o755)
		}
		os.MkdirAll(filepath.Join(root, "etc"), 0o755)
		os.WriteFile(filepath.Join(root, "etc", "resolv.conf"), nil, 0o644)
		m := &machine{rt: &Runtime{o: Options{DataDir: data}}, name: c.name}
		r := &run{m: m}
		b := &boot{ended: make(chan struct{}), netErr: refused}
		got := make(chan error, 1)
		t0 := time.Now()
		go func() {
			_, err := r.online(b)
			got <- err
		}()
		select {
		case err := <-got:
			if c.want == "" {
				if err != nil {
					t.Errorf("%s: a step with nothing that needs the network: %v", c.name, err)
				}
				continue
			}
			if err == nil || !strings.Contains(err.Error(), "has no network") || !strings.Contains(err.Error(), refused.Error()) || !strings.HasSuffix(err.Error(), c.want) {
				t.Errorf("%s: the step's end: %v (want the network's refusal and %q)", c.name, err, c.want)
			}
			if d := time.Since(t0); d > time.Second {
				t.Errorf("%s: the step ended after %v", c.name, d)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the online step of a machine with no network is still waiting after 5 s", c.name)
		}
	}
}

func TestTheOnlineStepKeepsItsRootThroughARename(t *testing.T) {
	data := t.TempDir()
	os.MkdirAll(filepath.Join(data, "dev", "etc"), 0o755)
	os.WriteFile(filepath.Join(data, "dev", "etc", "resolv.conf"), []byte("nameserver 198.19.0.1\n"), 0o644)
	rt := &Runtime{o: Options{DataDir: data}}
	m := &machine{rt: rt, name: "dev"}
	fd, err := m.openRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := os.Rename(filepath.Join(data, "dev"), filepath.Join(data, "web")); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.name = "web"
	m.mu.Unlock()
	if _, err := os.Stat(filepath.Join(data, "dev", "etc", "resolv.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old path after the rename: %v", err)
	}
	if b, err := readIn(rootPath(fd), "/etc/resolv.conf"); err != nil || !hasNameserver(string(b)) {
		t.Fatalf("after the rename: %q %v", b, err)
	}
	if err := waitResolver(rootPath(fd), time.Now().Add(time.Second), nil); err != nil {
		t.Fatalf("the resolver after the rename: %v", err)
	}
	m.opMu.Lock()
	os.Rename(filepath.Join(data, "web"), filepath.Join(data, "app"))
	got := make(chan error, 1)
	go func() {
		fd, err := m.openRoot()
		if err == nil {
			_, err = readIn(rootPath(fd), "/etc/resolv.conf")
			unix.Close(fd)
		}
		got <- err
	}()
	time.Sleep(50 * time.Millisecond)
	m.mu.Lock()
	m.name = "app"
	m.mu.Unlock()
	m.opMu.Unlock()
	if err := <-got; err != nil {
		t.Fatalf("opened during a rename: %v", err)
	}
}
