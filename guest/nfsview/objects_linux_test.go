package nfsview

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	api "fev/proto/agent"

	"golang.org/x/sys/unix"
)

const helperEnv = "NFSVIEW_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "bind":
		dir := os.Getenv("DIR")
		unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, "")
		if err := unix.Mount("tmpfs", dir, "tmpfs", 0, "size=1m"); err != nil {
			fmt.Println("err", err)
			os.Exit(1)
		}
		os.WriteFile(filepath.Join(dir, "f"), []byte("bound\n"), 0o644)
		fmt.Println("ready")
		io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	case "dial":
		c, err := net.Dial("unix", ControlSocket)
		if err != nil {
			fmt.Println("err", err)
			os.Exit(1)
		}
		fmt.Fprintln(c, os.Getenv("LINE"))
		b, _ := bufio.NewReader(c).ReadString('\n')
		fmt.Print(b)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func startView(t *testing.T) (*View, *Namespaces, *Tree) {
	t.Helper()
	needNFSD(t)
	root := filepath.Join(t.TempDir(), "tree")
	told := make(chan api.ViewStatus, 16)
	v := NewView(&LinuxStarter{Root: root, Logf: t.Logf}, func(s api.ViewStatus) { told <- s })
	v.Start(api.ViewStartParams{UID: 501, GID: 20})
	for {
		select {
		case s := <-told:
			if s.State == api.ViewFailed {
				t.Fatalf("the view failed: %s", s.Error)
			}
			if s.State != api.ViewReady {
				continue
			}
			t.Cleanup(func() { v.Stop() })
			r := v.run.(*linuxRun)
			return v, r.ns, v.tree
		case <-time.After(30 * time.Second):
			t.Fatal("the view is not ready in 30 s")
		}
	}
}

func engineNS(t *testing.T, roots, id string, lower string) string {
	t.Helper()
	base := t.TempDir()
	if err := unix.Mount("tmpfs", base, "tmpfs", 0, "size=4m"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Unmount(base, unix.MNT_DETACH) })
	upper, work := filepath.Join(base, "upper"), filepath.Join(base, "work")
	os.Mkdir(upper, 0o755)
	os.Mkdir(work, 0o755)
	at := filepath.Join(roots, id)
	os.MkdirAll(at, 0o755)
	ch := make(chan error, 1)
	var fd *os.File
	go func() {
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_FS | unix.CLONE_NEWNS); err != nil {
			ch <- err
			return
		}
		unix.Mount("", "/", "", unix.MS_REC|unix.MS_SLAVE, "")
		opts := "lowerdir=" + lower + ",upperdir=" + upper + ",workdir=" + work + ",index=on,nfs_export=on"
		if err := unix.Mount("overlay", at, "overlay", 0, opts); err != nil {
			ch <- fmt.Errorf("the engine's overlay: %w", err)
			return
		}
		var err error
		fd, err = os.Open("/proc/thread-self/ns/mnt")
		ch <- err
	}()
	if err := <-ch; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fd.Close() })
	return "/proc/self/fd/" + strconv.Itoa(int(fd.Fd()))
}

func inView(t *testing.T, ns *Namespaces, f func() error) {
	t.Helper()
	if err := ns.Do(f); err != nil {
		t.Fatal(err)
	}
}

func readIn(t *testing.T, ns *Namespaces, p string) string {
	t.Helper()
	var s string
	ns.Do(func() error {
		b, err := os.ReadFile(p)
		s = string(b)
		if err != nil {
			s = "error: " + err.Error()
		}
		return nil
	})
	return s
}

func TestObjectsAreMountedInTheView(t *testing.T) {
	_, ns, tree := startView(t)
	var mu sync.Mutex
	var logs []string
	logf := func(f string, a ...any) { mu.Lock(); logs = append(logs, fmt.Sprintf(f, a...)); mu.Unlock() }
	var gone [][]string
	roots := filepath.Join(t.TempDir(), "roots")
	o := NewObjects(tree, ns, ObjectsOptions{EngineRoots: roots, Logf: logf, Removed: func(p []string) { mu.Lock(); gone = append(gone, p); mu.Unlock() }})
	lower := t.TempDir()
	os.WriteFile(filepath.Join(lower, "hello"), []byte("from the image\n"), 0o644)
	os.Mkdir(filepath.Join(lower, "data"), 0o755)
	id := strings.Repeat("ab", 32)
	eng := engineNS(t, roots, id, lower)
	if err := o.Attach(eng, id, "web"); err != nil {
		t.Fatal(err)
	}
	web := filepath.Join(tree.Root(), DirContainers, "web")
	if got := readIn(t, ns, filepath.Join(web, "hello")); got != "from the image\n" {
		t.Fatalf("the container's root in the view: %q", got)
	}
	if mounted(web) {
		t.Error("the container's copy is in init's namespace")
	}
	const dest = "/nfsview-test-bind"
	os.Mkdir(dest, 0o755)
	defer os.Remove(dest)
	os.Mkdir(filepath.Join(lower, dest), 0o755)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"=bind", "DIR="+dest)
	cmd.SysProcAttr = &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS}
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { in.Close(); cmd.Wait() }()
	if l, _ := bufio.NewReader(out).ReadString('\n'); l != "ready\n" {
		t.Fatalf("the helper: %q", l)
	}
	if err := o.Binds("/proc/self/ns/mnt", id, cmd.Process.Pid, []string{dest}); err != nil {
		t.Fatal(err)
	}
	if got := readIn(t, ns, filepath.Join(web, dest, "f")); got != "bound\n" {
		t.Fatalf("the bind in the view: %q", got)
	}
	if e, ok := tree.Table().Lookup(filepath.Join(web, dest)); !ok || e.AnonUID != 0 {
		t.Errorf("the bind's export: %+v %v", e, ok)
	}
	mroot, vdata := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(mroot, "m"), []byte("machine\n"), 0o644)
	os.WriteFile(filepath.Join(vdata, "v"), []byte("volume\n"), 0o644)
	o.SyncMachines([]MachineSpec{{UUID: "u", Name: "ubuntu", Root: mroot, UID: 501, GID: 501}})
	o.SyncVolumes([]VolumeSpec{{Name: "pg", Data: vdata}})
	if got := readIn(t, ns, filepath.Join(tree.Root(), "ubuntu", "m")); got != "machine\n" {
		t.Errorf("the machine in the view: %q", got)
	}
	if got := readIn(t, ns, filepath.Join(tree.Root(), DirVolumes, "pg", "v")); got != "volume\n" {
		t.Errorf("the volume in the view: %q", got)
	}
	o.SyncMachines([]MachineSpec{{UUID: "u", Name: "noble", Root: mroot, UID: 501, GID: 501}})
	if got := readIn(t, ns, filepath.Join(tree.Root(), "noble", "m")); got != "machine\n" {
		t.Errorf("the renamed machine: %q", got)
	}
	o.Release(id)
	o.SyncMachines(nil)
	o.SyncVolumes(nil)
	var left []string
	ns.Do(func() error {
		b, _ := os.ReadFile("/proc/thread-self/mountinfo")
		for _, l := range strings.Split(string(b), "\n") {
			if f := strings.Fields(l); len(f) > 4 && strings.HasPrefix(f[4], tree.Root()+"/") {
				left = append(left, f[4])
			}
		}
		return nil
	})
	if len(left) != 0 {
		t.Errorf("mounts left in the view: %v", left)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, l := range logs {
		if strings.Contains(l, "release") || strings.Contains(l, "revoke") {
			t.Errorf("logged: %s", l)
		}
	}
	all := fmt.Sprint(gone)
	for _, want := range []string{"docker/containers/web", "ubuntu", "noble", "docker/volumes/pg"} {
		if !strings.Contains(all, want) {
			t.Errorf("removed did not tell %s: %s", want, all)
		}
	}
}

func TestOnlyTheEnginesOverlaysAreCopied(t *testing.T) {
	_, ns, tree := startView(t)
	roots := filepath.Join(t.TempDir(), "roots")
	id := strings.Repeat("cd", 32)
	os.MkdirAll(filepath.Join(roots, id), 0o755)
	dst := filepath.Join(tree.Root(), "x")
	inView(t, ns, func() error { return MountPoint(dst) })
	errs, err := ns.CloneFrom("/proc/self/ns/mnt", 0, []Clone{{Src: filepath.Join(roots, id), Dst: dst, Overlay: true}})
	if err != nil || errs[0] != ErrNotMounted {
		t.Errorf("a bare directory: %v %v", err, errs)
	}
	tm := filepath.Join(t.TempDir(), "tmpfs")
	os.Mkdir(tm, 0o755)
	if err := unix.Mount("tmpfs", tm, "tmpfs", 0, "size=1m"); err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount(tm, unix.MNT_DETACH)
	errs, err = ns.CloneFrom("/proc/self/ns/mnt", 0, []Clone{{Src: tm, Dst: dst, Overlay: true}})
	if err != nil || errs[0] != ErrNotOverlay {
		t.Errorf("a tmpfs for an overlay: %v %v", err, errs)
	}
}

type fakeCtr struct {
	mu     sync.Mutex
	layers []string
	views  map[string]bool
	leases map[string]bool
}

func (f *fakeCtr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Trailer", "grpc-status")
	var fields map[int][][]byte
	if len(body) >= 5 {
		fields, _ = pbFields(body[5:])
	}
	key := ""
	if len(fields[2]) > 0 {
		key = string(fields[2][0])
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var ans []byte
	mount := func() []byte {
		m := append(append(pbString(1, "overlay"), pbString(2, "overlay")...), pbString(4, "lowerdir="+strings.Join(f.layers, ":"))...)
		return append(binary.AppendUvarint(binary.AppendUvarint(nil, 1<<3|2), uint64(len(m))), m...)
	}
	switch r.URL.Path {
	case leasesService + "Create":
		f.leases[string(fields[1][0])] = true
	case leasesService + "Delete":
		delete(f.leases, string(fields[1][0]))
	case leasesService + "List":
	case snapshotsService + "View":
		f.views[key] = true
		ans = mount()
	case snapshotsService + "Mounts":
		ans = mount()
	case snapshotsService + "Remove":
		delete(f.views, key)
	}
	frame := make([]byte, 5+len(ans))
	binary.BigEndian.PutUint32(frame[1:], uint32(len(ans)))
	copy(frame[5:], ans)
	w.WriteHeader(200)
	w.Write(frame)
	w.Header().Set("grpc-status", "0")
}

func TestAnImagesViewIsMadeAtItsFirstLook(t *testing.T) {
	_, ns, tree := startView(t)
	sock := filepath.Join(t.TempDir(), "ctr.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeCtr{views: map[string]bool{}, leases: map[string]bool{}}
	layers := t.TempDir()
	for i, name := range []string{"top", "base"} {
		d := filepath.Join(layers, strconv.Itoa(i), "fs")
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, name), []byte(name+"\n"), 0o644)
		fake.layers = append(fake.layers, "/fake/"+strconv.Itoa(i)+"/fs")
	}
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: fake, Protocols: &p}
	go srv.Serve(l)
	defer srv.Close()
	ctr := &Containerd{Dial: func(ctx context.Context) (net.Conn, error) { return net.Dial("unix", sock) }, Namespace: "moby", Snapshotter: "overlayfs"}
	images := &LinuxImages{NS: ns, Ctr: ctr, From: "/fake", Root: layers, Logf: t.Logf}
	o := NewObjects(tree, ns, ObjectsOptions{Images: images, Logf: t.Logf})
	o.SyncImages([]ImageSpec{
		{ID: "sha256:1", Name: "app:1", DiffIDs: []string{"sha256:a", "sha256:b"}},
		{ID: "sha256:2", Name: "lib:2", DiffIDs: []string{"sha256:c", "sha256:d"}},
	})
	app := filepath.Join(tree.Root(), DirImages, "app:1")
	if images.Made(app) {
		t.Fatal("made before any look")
	}
	look := func(dir string, enter bool) []string {
		t.Helper()
		ch := make(chan []string, 1)
		go func() {
			var names []string
			f := func() error {
				ents, err := os.ReadDir(dir)
				for _, e := range ents {
					names = append(names, e.Name())
				}
				if err != nil {
					names = append(names, "error: "+err.Error())
				}
				return nil
			}
			if enter {
				ns.Do(f)
			} else {
				f()
			}
			sort.Strings(names)
			ch <- names
		}()
		select {
		case n := <-ch:
			return n
		case <-time.After(20 * time.Second):
			panic("a look into an image's place waited 20 s")
		}
	}
	if got := strings.Join(look(app, true), " "); got != "base top" {
		t.Errorf("app:1 from the view's namespace: %q", got)
	}
	if !images.Made(app) {
		t.Error("not made after the look")
	}
	var dirfd int
	inView(t, ns, func() error {
		var err error
		dirfd, err = unix.Open(filepath.Join(tree.Root(), DirImages), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		return err
	})
	defer unix.Close(dirfd)
	if got := strings.Join(look("/proc/self/fd/"+strconv.Itoa(dirfd)+"/lib:2", false), " "); got != "base top" {
		t.Errorf("lib:2 looked into from another mount namespace: %q", got)
	}
	o.SyncImages(nil)
	fake.mu.Lock()
	if len(fake.views) != 0 || len(fake.leases) != 0 {
		t.Errorf("left in containerd: views %v leases %v", fake.views, fake.leases)
	}
	fake.mu.Unlock()
	o2 := NewObjects(tree, ns, ObjectsOptions{Images: images, Logf: t.Logf})
	o2.SyncImages([]ImageSpec{{ID: "sha256:3", Name: "late:3", DiffIDs: []string{"sha256:e"}}})
	o2.Close()
	if got := strings.Join(look(filepath.Join(tree.Root(), DirImages, "late:3"), true), " "); strings.Contains(got, "top") {
		t.Errorf("a look after the stop: %q", got)
	}
	if images.Made(filepath.Join(tree.Root(), DirImages, "late:3")) {
		t.Error("made after the stop")
	}
}

func TestAnImageOfManyLongLayersIsMade(t *testing.T) {
	_, ns, tree := startView(t)
	sock := filepath.Join(t.TempDir(), "ctr.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeCtr{views: map[string]bool{}, leases: map[string]bool{}}
	layers := filepath.Join(t.TempDir(), strings.Repeat("snapshots-of-the-engine-", 5))
	var want []string
	for i := 0; i < 8; i++ {
		d := filepath.Join(layers, strconv.Itoa(i), "fs")
		os.MkdirAll(d, 0o755)
		name := "layer" + strconv.Itoa(i)
		os.WriteFile(filepath.Join(d, name), []byte(name+"\n"), 0o644)
		os.WriteFile(filepath.Join(d, "which"), []byte(name+"\n"), 0o644)
		fake.layers = append(fake.layers, "/fake/"+strconv.Itoa(i)+"/fs")
		want = append(want, name)
	}
	if n := len(strings.Join(fake.layers, ":")) + 8*(len(layers)-len("/fake")); n <= 256 {
		t.Fatalf("the layers' paths are %d bytes, not past fsconfig's 256", n)
	}
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: fake, Protocols: &p}
	go srv.Serve(l)
	defer srv.Close()
	ctr := &Containerd{Dial: func(ctx context.Context) (net.Conn, error) { return net.Dial("unix", sock) }, Namespace: "moby", Snapshotter: "overlayfs"}
	images := &LinuxImages{NS: ns, Ctr: ctr, From: "/fake", Root: layers, Logf: t.Logf}
	o := NewObjects(tree, ns, ObjectsOptions{Images: images, Logf: t.Logf})
	defer o.SyncImages(nil)
	o.SyncImages([]ImageSpec{{ID: "sha256:9", Name: "many:8", DiffIDs: []string{"sha256:a", "sha256:b", "sha256:c", "sha256:d", "sha256:e", "sha256:f", "sha256:g", "sha256:h"}}})
	app := filepath.Join(tree.Root(), DirImages, "many:8")
	var got []string
	var which []byte
	done := make(chan error, 1)
	go func() {
		done <- ns.Do(func() error {
			ents, err := os.ReadDir(app)
			for _, e := range ents {
				if e.Name() != "which" {
					got = append(got, e.Name())
				}
			}
			if err == nil {
				which, err = os.ReadFile(filepath.Join(app, "which"))
			}
			return err
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a look into the image's place waited 20 s")
	}
	sort.Strings(got)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("the image's view has %v, not %v", got, want)
	}
	if string(which) != "layer0\n" {
		t.Fatalf("the top layer's file is %q, not layer0's", which)
	}
	if !images.Made(app) {
		t.Fatal("not made after the look")
	}
}

func TestTheControlSocketChecksItsPeer(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	l, err := ListenControl()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c := &Control{
		Objects:  func() *Objects { return nil },
		EngineNS: func() (string, func(), bool) { return "/proc/self/ns/mnt", func() {}, true },
		Name:     func(context.Context, string) string { return "" },
		Bundle:   func(string) (int, []string, error) { return 0, nil, nil },
		Logf:     t.Logf,
	}
	go c.Serve(l)
	call := func(line string, own bool) string {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), helperEnv+"=dial", "LINE="+line)
		if own {
			cmd.SysProcAttr = &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS}
		}
		out, err := cmd.Output()
		if err != nil {
			return "error: " + err.Error()
		}
		return strings.TrimSpace(string(out))
	}
	id := strings.Repeat("e", 64)
	if got := call("release "+id, false); got != "ok no view" {
		t.Errorf("the engine's namespace: %q", got)
	}
	if got := call("release "+id, true); got != "err peer" {
		t.Errorf("another mount namespace: %q", got)
	}
	if got := call("release ../../x", false); !strings.HasPrefix(got, "err a call") {
		t.Errorf("a bad id: %q", got)
	}
}
