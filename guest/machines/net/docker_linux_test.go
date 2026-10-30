package machnet

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fev/guest/machines"
)

func TestTheEngineSocketServesRootAndTheDefaultUserOnly(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	passwd := "root:x:0:0:root:/root:/bin/sh\ntester:x:1234:1234::/home/tester:/bin/sh\n"
	if err := os.WriteFile(filepath.Join(root, "etc/passwd"), []byte(passwd), 0o644); err != nil {
		t.Fatal(err)
	}
	engine := filepath.Join(dir, "engine.sock")
	el, err := net.Listen("unix", engine)
	if err != nil {
		t.Fatal(err)
	}
	defer el.Close()
	go http.Serve(el, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "engine "+r.URL.Path) }))
	run := filepath.Join(dir, "run")
	os.Mkdir(run, 0o755)
	p := machines.Plug{Name: "m", User: "tester", Root: root, Run: run}
	d, err := listenDocker(p, func() (net.Conn, error) { return net.Dial("unix", engine) }, func(int, string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(run, DockerSocket)
	if st, err := os.Stat(sock); err != nil || st.Mode().Perm() != 0o666 {
		t.Fatalf("socket: %v %v", st, err)
	}
	client := &http.Client{Transport: &http.Transport{Dial: func(_, _ string) (net.Conn, error) { return net.Dial("unix", sock) }}}
	if os.Getuid() == 0 {
		resp, err := client.Get("http://d/_ping")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != "engine /_ping" {
			t.Fatalf("root was answered %q", b)
		}
	}
	for uid, want := range map[uint32]bool{0: true, 1234: true, 1235: false, 65534: false} {
		if d.admits(uid) != want {
			t.Errorf("uid %d admitted: %v", uid, !want)
		}
	}
	os.WriteFile(filepath.Join(root, "etc/passwd"), []byte(passwd+"late:x:1235:1235::/:/bin/sh\n"), 0o644)
	d.user = "late"
	if !d.admits(1235) {
		t.Error("a user made after the start was refused")
	}
	os.WriteFile(filepath.Join(dir, "outside"), []byte("late:x:2000:2000::/:/bin/sh\n"), 0o644)
	os.Remove(filepath.Join(root, "etc/passwd"))
	os.Symlink("../../outside", filepath.Join(root, "etc/passwd"))
	if d.admits(2000) {
		t.Error("followed a passwd link out of the machine's root")
	}
	d.user = ""
	if d.admits(1234) || !d.admits(0) {
		t.Error("with no default user only root is served")
	}

	a, b := net.Pipe()
	go func() {
		io.WriteString(b, "GET /_ping HTTP/1.1\r\nHost: d\r\n\r\n")
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.user = "tester"
		d.refuse(a, 65534)
	}()
	resp, err := http.ReadResponse(bufio.NewReader(b), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	<-done
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), `uid 65534 may not use the engine from this machine; its socket serves root and tester`) {
		t.Fatalf("refusal: %d %s", resp.StatusCode, body)
	}

	d.close()
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("the socket outlived its close: %v", err)
	}
}

func TestTheEngineSocketFollowsARenamedMachine(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "m")
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("tester:x:1234:1234::/home/tester:/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(dir, "run")
	os.Mkdir(run, 0o755)
	d, err := listenDocker(machines.Plug{Name: "m", User: "tester", Root: root, Run: run}, func() (net.Conn, error) { return nil, os.ErrNotExist }, func(int, string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	n := New(Options{})
	n.byName["m"] = &member{name: "m", docker: d}
	if !d.admits(1234) {
		t.Fatal("the default user was refused before the rename")
	}
	if err := os.Rename(root, filepath.Join(dir, "M2")); err != nil {
		t.Fatal(err)
	}
	n.Rename("m", "M2")
	if !d.admits(1234) {
		t.Error("the default user was refused after the running machine was renamed")
	}
}
