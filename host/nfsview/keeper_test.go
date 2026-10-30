package nfsview

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func entry(on, typ string, flags uint32, owner uint32) unix.Statfs_t {
	var s unix.Statfs_t
	copy(s.Mntonname[:], on)
	copy(s.Fstypename[:], typ)
	s.Flags = flags
	s.Owner = owner
	return s
}

type fakeMounts struct {
	mu    sync.Mutex
	table []unix.Statfs_t
	opens []string
}

func (f *fakeMounts) install(t *testing.T) {
	oldTable, oldHold := mountTable, holdMount
	t.Cleanup(func() { mountTable, holdMount = oldTable, oldHold })
	mountTable = func() ([]unix.Statfs_t, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Clone(f.table), nil
	}
	holdMount = func(path string) (*os.File, error) {
		f.mu.Lock()
		f.opens = append(f.opens, path)
		f.mu.Unlock()
		return os.Open(os.DevNull)
	}
}

func (f *fakeMounts) set(rows ...unix.Statfs_t) {
	f.mu.Lock()
	f.table = rows
	f.mu.Unlock()
}

func TestTheKeeperHoldsTheViewsMirrorMountsOnly(t *testing.T) {
	f := &fakeMounts{}
	f.install(t)
	dir := filepath.Join(t.TempDir(), "Fevom")
	real, _ := filepath.EvalSymlinks(filepath.Dir(dir))
	view := filepath.Join(real, "Fevom")
	uid := uint32(os.Getuid())
	auto := uint32(unix.MNT_AUTOMOUNTED | unix.MNT_NODEV)
	f.set(
		entry(view, "nfs", 0, uid),
		entry(view+"/u", "nfs", auto, uid),
		entry(view+"/docker/volumes/v", "nfs", auto, uid),
		entry(view+"/x", "nfs", unix.MNT_NODEV, uid),
		entry(view+"/y", "smbfs", auto, uid),
		entry(view+"/z", "nfs", auto, uid+1),
		entry(view+"Other/w", "nfs", auto, uid),
	)
	k, err := Keep(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	k.settle()
	got := k.Held()
	slices.Sort(got)
	want := []string{view + "/docker/volumes/v", view + "/u"}
	if !slices.Equal(got, want) {
		t.Fatalf("held %v, want %v", got, want)
	}
	f.set(entry(view+"/u", "nfs", auto, uid))
	k.scan()
	k.settle()
	if got := k.Held(); !slices.Equal(got, []string{view + "/u"}) {
		t.Fatalf("after the volume's mount went: %v", got)
	}
	if len(f.opens) != 2 {
		t.Fatalf("opened %v", f.opens)
	}
	if !k.Forget(view + "/u") {
		t.Fatal("Forget did not know the mount")
	}
	if len(k.Held()) != 0 {
		t.Fatal("Forget kept the mount")
	}
	k.Close()
	k.Close()
}

func TestTheKeeperWakesOnAMountAndNotOtherwise(t *testing.T) {
	k, err := Keep(filepath.Join(t.TempDir(), "Fevom"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	scans := func() int { k.mu.Lock(); defer k.mu.Unlock(); return k.scans }
	time.Sleep(300 * time.Millisecond)
	if n := scans(); n > 2 {
		t.Fatalf("%d looks in 300 ms with nothing mounted", n)
	}
	before := scans()
	v := newView(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Mount(ctx, v.server.Endpoint(), v.dir, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for scans() <= before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if scans() <= before {
		t.Fatal("no look after a mount")
	}
	done := make(chan struct{})
	go func() { k.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not end the wait")
	}
}

func TestTheKeepersLockIsNotHeldWhileItOpensAMount(t *testing.T) {
	f := &fakeMounts{}
	f.install(t)
	release := make(chan struct{})
	var mu sync.Mutex
	var opened []*os.File
	holdMount = func(path string) (*os.File, error) {
		<-release
		file, err := os.Open(os.DevNull)
		mu.Lock()
		opened = append(opened, file)
		mu.Unlock()
		return file, err
	}
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	dir := filepath.Join(t.TempDir(), "Fevom")
	real, _ := filepath.EvalSymlinks(filepath.Dir(dir))
	view := filepath.Join(real, "Fevom")
	f.set(entry(view+"/u", "nfs", unix.MNT_AUTOMOUNTED, uint32(os.Getuid())))
	within := func(what string, fn func()) {
		t.Helper()
		done := make(chan struct{})
		go func() { fn(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s waited on a mount being opened", what)
		}
	}
	var k *Keeper
	within("Keep", func() {
		var err error
		if k, err = Keep(dir, nil); err != nil {
			t.Error(err)
		}
	})
	within("Held", func() {
		if got := k.Held(); len(got) != 0 {
			t.Errorf("held %v before the open ended", got)
		}
	})
	within("Forget", func() { k.Forget(view + "/x") })
	within("Close", k.Close)
	close(release)
	released = true
	k.settle()
	mu.Lock()
	defer mu.Unlock()
	if len(opened) != 1 {
		t.Fatalf("opened %d", len(opened))
	}
	if _, err := opened[0].Stat(); err == nil {
		t.Fatal("the mount opened after Close is still held")
	}
	if got := k.Held(); len(got) != 0 {
		t.Fatalf("held %v after Close", got)
	}
}

func unmountTable(t *testing.T, f *fakeMounts, view, dir string, answers map[string]error, orphans map[string]bool) *[]string {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []string
	)
	old := unmount
	t.Cleanup(func() { unmount = old })
	unmount = func(path string, flags int) error {
		call := path
		if flags&unix.MNT_FORCE != 0 {
			call += " -f"
		}
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
		if err := answers[call]; err != nil {
			return err
		}
		on := path
		if path == dir {
			on = view
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.table = slices.DeleteFunc(f.table, func(s unix.Statfs_t) bool {
			m := unix.ByteSliceToString(s.Mntonname[:])
			return m == on || (on == view && strings.HasPrefix(m, view+"/") && !orphans[m])
		})
		return nil
	}
	return &calls
}

func byDepth(calls []string) [][]string {
	var out [][]string
	last := -1
	for _, c := range calls {
		d := strings.Count(strings.TrimSuffix(c, " -f"), "/")
		if d != last {
			out = append(out, nil)
			last = d
		}
		out[len(out)-1] = append(out[len(out)-1], c)
	}
	for _, run := range out {
		sort.Strings(run)
	}
	return out
}

func sameRuns(got, want [][]string) bool {
	return slices.EqualFunc(got, want, func(a, b []string) bool { return slices.Equal(a, b) })
}

func viewTable(t *testing.T) (f *fakeMounts, dir, view string) {
	f = &fakeMounts{}
	f.install(t)
	dir = filepath.Join(t.TempDir(), "Fevom")
	real, _ := filepath.EvalSymlinks(filepath.Dir(dir))
	view = filepath.Join(real, "Fevom")
	uid := uint32(os.Getuid())
	auto := uint32(unix.MNT_AUTOMOUNTED | unix.MNT_NODEV)
	f.set(
		entry(view, "nfs", 0, uid),
		entry(view+"/docker/images/node:22", "nfs", auto, uid),
		entry(view+"/docker/volumes/v", "nfs", auto, uid),
		entry(view+"/docker/volumes/gone", "nfs", auto, uid),
		entry(view+"/docker/volumes/odd", "nfs", auto, uid),
		entry(view+"/docker/volumes/bad", "nfs", auto, uid),
		entry(view+"/u", "nfs", auto, uid),
		entry(view+"/u/home", "nfs", auto, uid),
		entry(view+"/z", "nfs", auto, uid+1),
		entry(view+"Other/w", "nfs", auto, uid),
	)
	return f, dir, view
}

func TestTheViewsUnmountTakesItsMirrorMountsFirst(t *testing.T) {
	f, dir, view := viewTable(t)
	calls := unmountTable(t, f, view, dir, map[string]error{
		view + "/u":                   unix.EBUSY,
		view + "/docker/volumes/gone": unix.ENOENT,
		view + "/docker/volumes/odd":  unix.EINVAL,
		view + "/docker/volumes/bad":  unix.EIO,
	}, nil)
	forced, err := Unmount(dir)
	want := [][]string{
		{view + "/docker/images/node:22", view + "/docker/volumes/bad", view + "/docker/volumes/gone", view + "/docker/volumes/odd", view + "/docker/volumes/v"},
		{view + "/u/home"},
		{view + "/u", view + "/u -f"},
		{dir},
	}
	if !forced || err == nil || !strings.Contains(err.Error(), "docker/volumes/bad") || strings.Contains(err.Error(), "volumes/gone") || strings.Contains(err.Error(), "volumes/odd") || !sameRuns(byDepth(*calls), want) {
		t.Fatalf("plain: forced %v, err %v, calls %v, want %v", forced, err, byDepth(*calls), want)
	}
	if slices.Contains(*calls, view+"/docker/volumes/bad -f") {
		t.Errorf("a mirror mount whose plain unmount failed (EIO) was forced: %v", *calls)
	}
}

func TestABusyViewIsForcedAfterItsMirrorMounts(t *testing.T) {
	f, dir, view := viewTable(t)
	calls := unmountTable(t, f, view, dir, map[string]error{dir: unix.EBUSY}, nil)
	forced, err := Unmount(dir)
	if !forced || err != nil || len(*calls) != 9 || (*calls)[7] != dir || (*calls)[8] != dir+" -f" {
		t.Fatalf("forced %v, err %v, calls %v", forced, err, *calls)
	}
}

func TestAForcedUnmountForcesTheViewFirstThenWhatItLeft(t *testing.T) {
	f, dir, view := viewTable(t)
	calls := unmountTable(t, f, view, dir, nil, nil)
	if err := ForceUnmount(dir); err != nil || !slices.Equal(*calls, []string{dir + " -f"}) {
		t.Fatalf("err %v, calls %v; want the view alone", err, *calls)
	}

	f, dir, view = viewTable(t)
	orphan, kept := view+"/docker/images/node:22", view+"/u/home"
	calls = unmountTable(t, f, view, dir, map[string]error{orphan + " -f": unix.ENOENT}, map[string]bool{orphan: true, kept: true})
	err := ForceUnmount(dir)
	want := [][]string{{dir + " -f"}, {orphan + " -f"}, {kept + " -f"}}
	if !sameRuns(byDepth(*calls), want) {
		t.Fatalf("calls %v, want %v", byDepth(*calls), want)
	}
	if err == nil || !strings.Contains(err.Error(), orphan+" is still mounted") || strings.Contains(err.Error(), kept) {
		t.Errorf("err %v; want the orphan named, and only it", err)
	}

	f, dir, view = viewTable(t)
	calls = unmountTable(t, f, view, dir, map[string]error{dir + " -f": unix.EBUSY}, nil)
	if err := ForceUnmount(dir); err == nil || !slices.Equal(*calls, []string{dir + " -f"}) {
		t.Errorf("err %v, calls %v", err, *calls)
	}
}
