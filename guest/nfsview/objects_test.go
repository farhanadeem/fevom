package nfsview

import (
	"errors"
	"fmt"
	"path"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

type fakeOps struct {
	mu        sync.Mutex
	dirs      map[string]bool
	mounts    map[string]string
	calls     []string
	flushes   int
	fail      map[string]error
	unmounted map[string]bool
}

func newFakeOps() *fakeOps {
	return &fakeOps{dirs: map[string]bool{}, mounts: map[string]string{}, fail: map[string]error{}, unmounted: map[string]bool{}}
}

func (f *fakeOps) log(s string) { f.calls = append(f.calls, s) }

func (f *fakeOps) MakeDir(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[p] = true
	return nil
}
func (f *fakeOps) RemoveDir(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, m := f.mounts[p]; m {
		return fmt.Errorf("%s is busy", p)
	}
	delete(f.dirs, p)
	return nil
}
func (f *fakeOps) Bind(src, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail["bind:"+dst]; err != nil {
		return err
	}
	if !f.dirs[dst] {
		return fmt.Errorf("no place %s", dst)
	}
	f.mounts[dst] = src
	f.log("bind " + src + " " + dst)
	return nil
}
func (f *fakeOps) Move(from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.mounts[from]
	if !ok {
		return fmt.Errorf("nothing at %s", from)
	}
	delete(f.mounts, from)
	f.mounts[to] = m
	f.log("move " + from + " " + to)
	return nil
}
func (f *fakeOps) Release(p string, whole bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for q := range f.mounts {
		if strings.HasPrefix(q, p+"/") {
			return fmt.Errorf("release %s with %s under it", p, q)
		}
	}
	delete(f.mounts, p)
	f.log(fmt.Sprintf("release %s whole=%v", p, whole))
	return nil
}
func (f *fakeOps) Flush() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushes++
	f.log("flush")
	return nil
}
func (f *fakeOps) CloneFrom(ns string, pid int, clones []Clone) ([]error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	errs := make([]error, len(clones))
	for i, c := range clones {
		if f.unmounted[c.Src] {
			errs[i] = ErrNotMounted
			continue
		}
		if err := f.fail["clone:"+c.Src]; err != nil {
			errs[i] = err
			continue
		}
		f.mounts[c.Dst] = fmt.Sprintf("clone:%s:%d:%s", ns, pid, c.Src)
		f.log(fmt.Sprintf("clone %s pid=%d %s %s", ns, pid, c.Src, c.Dst))
	}
	return errs, nil
}

func (f *fakeOps) places() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for p, m := range f.mounts {
		out = append(out, p+"="+m)
	}
	sort.Strings(out)
	return out
}

type fakeLazy struct {
	prepared map[string]ImageSpec
	stopped  bool
}

func (l *fakeLazy) Prepare(p string, img ImageSpec) error { l.prepared[p] = img; return nil }
func (l *fakeLazy) Release(p string) error                { delete(l.prepared, p); return nil }
func (l *fakeLazy) Made(p string) bool                    { return false }
func (l *fakeLazy) Stop()                                 { l.stopped = true }

const root = "/v"

func newObjects(t *testing.T) (*Objects, *fakeOps, *[][]string) {
	t.Helper()
	ops := newFakeOps()
	var removed [][]string
	var mu sync.Mutex
	tree := NewTree(root, Owner{UID: 501, GID: 20})
	o := NewObjects(tree, ops, ObjectsOptions{
		Images:  &fakeLazy{prepared: map[string]ImageSpec{}},
		Removed: func(p []string) { mu.Lock(); removed = append(removed, p); mu.Unlock() },
		Logf:    func(f string, a ...any) { t.Logf(f, a...) },
	})
	return o, ops, &removed
}

func exportOf(t *testing.T, o *Objects, p string) Export {
	t.Helper()
	e, ok := o.tree.Table().Lookup(p)
	if !ok {
		t.Fatalf("%s is not exported", p)
	}
	return e
}

func TestMachinesAreShownAsTheirSubvolumes(t *testing.T) {
	o, ops, removed := newObjects(t)
	o.SyncMachines([]MachineSpec{
		{UUID: "u1", Name: "ubuntu", Root: "/data/@machines/ubuntu", UID: 501, GID: 501},
		{UUID: "u2", Name: "box", Root: "/data/@machines/box", UID: 1000, GID: 1000},
	})
	o.SetIsolated("box", true)
	if got, want := ops.places(), []string{"/v/ubuntu=/data/@machines/ubuntu"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("places: %v, want %v", got, want)
	}
	e := exportOf(t, o, "/v/ubuntu")
	if e.AnonUID != 501 || e.AnonGID != 501 || e.Flags&FlagAllSquash == 0 || e.Flags&FlagReadOnly != 0 {
		t.Errorf("ubuntu's export: %+v", e)
	}
	fsid := e.FSID
	o.SyncMachines([]MachineSpec{
		{UUID: "u1", Name: "noble", Root: "/data/@machines/noble", UID: 501, GID: 501},
		{UUID: "u2", Name: "box", Root: "/data/@machines/box", UID: 1000, GID: 1000},
	})
	if got, want := ops.places(), []string{"/v/noble=/data/@machines/ubuntu"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after the rename: %v, want %v (one mount, moved)", got, want)
	}
	if e := exportOf(t, o, "/v/noble"); e.FSID != fsid {
		t.Errorf("the rename changed the fsid: %d, was %d", e.FSID, fsid)
	}
	if _, ok := o.tree.Table().Lookup("/v/ubuntu"); ok {
		t.Error("the old name is still exported")
	}
	o.SetIsolated("box", false)
	if len(ops.places()) != 2 {
		t.Errorf("box not shown when trusted: %v", ops.places())
	}
	o.SyncMachines([]MachineSpec{{UUID: "u2", Name: "box", Root: "/data/@machines/box", UID: 1000, GID: 1000}})
	if got := ops.places(); len(got) != 1 || !strings.HasPrefix(got[0], "/v/box=") {
		t.Errorf("after noble's delete: %v", got)
	}
	if ops.dirs["/v/noble"] || ops.dirs["/v/ubuntu"] {
		t.Errorf("places left: %v", ops.dirs)
	}
	all := fmt.Sprint(*removed)
	for _, want := range []string{"ubuntu", "noble"} {
		if !strings.Contains(all, want) {
			t.Errorf("removed did not say %s: %s", want, all)
		}
	}
	if !strings.Contains(strings.Join(ops.calls, "\n"), "release /v/noble whole=false") {
		t.Errorf("calls: %v", ops.calls)
	}
}

func TestAnIsolatedMachineStaysOutWhenRenamed(t *testing.T) {
	o, ops, _ := newObjects(t)
	o.SetIsolated("box", true)
	o.SyncMachines([]MachineSpec{{UUID: "u2", Name: "box", Root: "/data/@machines/box", UID: 1000, GID: 1000}})
	if got := ops.places(); len(got) != 0 {
		t.Fatalf("isolated box shown: %v", got)
	}
	o.SyncMachines([]MachineSpec{{UUID: "u2", Name: "crate", Root: "/data/@machines/crate", UID: 1000, GID: 1000}})
	if got := ops.places(); len(got) != 0 {
		t.Errorf("isolated box renamed crate is shown: %v", got)
	}
	if !o.Isolated("crate") || o.Isolated("box") {
		t.Errorf("isolation after the rename: crate %v, box %v", o.Isolated("crate"), o.Isolated("box"))
	}
	o.SyncMachines([]MachineSpec{
		{UUID: "u2", Name: "crate", Root: "/data/@machines/crate", UID: 1000, GID: 1000},
		{UUID: "u3", Name: "box", Root: "/data/@machines/box", UID: 1000, GID: 1000},
	})
	if got := ops.places(); len(got) != 1 || !strings.HasPrefix(got[0], "/v/box=") {
		t.Errorf("a new box after the rename: %v", got)
	}
}

func TestAMachineNamedLikeTheRootsFilesIsRefused(t *testing.T) {
	o, ops, _ := newObjects(t)
	o.SyncMachines([]MachineSpec{{UUID: "x", Name: "docker", Root: "/data/@machines/docker", UID: 501, GID: 501}})
	if len(ops.places()) != 0 {
		t.Errorf("places: %v", ops.places())
	}
}

func TestVolumesAndImagesFollowTheEngine(t *testing.T) {
	o, ops, removed := newObjects(t)
	o.SyncVolumes([]VolumeSpec{{Name: "pg", Data: "/data/@docker/volumes/pg/_data"}})
	img := ImageSpec{ID: "sha256:aa", Name: "ghcr.io/org/app:1", DiffIDs: []string{"sha256:1"}}
	o.SyncImages([]ImageSpec{img})
	if got, want := ops.places(), []string{"/v/docker/volumes/pg=/data/@docker/volumes/pg/_data"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("places: %v, want %v", got, want)
	}
	if e := exportOf(t, o, "/v/docker/volumes/pg"); e.AnonUID != 0 || e.AnonGID != 0 || e.Flags&FlagReadOnly != 0 {
		t.Errorf("the volume's export: %+v", e)
	}
	lazy := o.images.(*fakeLazy)
	if _, ok := lazy.prepared["/v/docker/images/ghcr.io_org_app:1"]; !ok {
		t.Errorf("image places: %v", lazy.prepared)
	}
	if e := exportOf(t, o, "/v/docker/images/ghcr.io_org_app:1"); e.Flags&FlagReadOnly == 0 {
		t.Errorf("the image's export is writable: %+v", e)
	}
	o.SyncImages(nil)
	o.SyncVolumes(nil)
	if len(lazy.prepared) != 0 || len(ops.places()) != 0 {
		t.Errorf("left: %v %v", lazy.prepared, ops.places())
	}
	if got := fmt.Sprint(*removed); !strings.Contains(got, "docker/images/ghcr.io_org_app:1") || !strings.Contains(got, "docker/volumes/pg") {
		t.Errorf("removed: %s", got)
	}
}

func TestARunningContainersRootAndBindsComeAndGo(t *testing.T) {
	o, ops, removed := newObjects(t)
	id := strings.Repeat("ab", 32)
	if err := o.Attach("/proc/self/fd/9", id, "web"); err != nil {
		t.Fatal(err)
	}
	if err := o.Binds("/proc/self/fd/9", id, 231, []string{"/data", "/t"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/v/docker/containers/web/data=clone:/proc/self/fd/9:231:/data",
		"/v/docker/containers/web/t=clone:/proc/self/fd/9:231:/t",
		"/v/docker/containers/web=clone:/proc/self/fd/9:0:" + path.Join(EngineRoots, id),
	}
	if got := ops.places(); !reflect.DeepEqual(got, want) {
		t.Fatalf("places:\n%v\nwant\n%v", got, want)
	}
	for _, p := range []string{"/v/docker/containers/web", "/v/docker/containers/web/data"} {
		if e := exportOf(t, o, p); e.AnonUID != 0 || e.Flags&FlagCrossMount == 0 {
			t.Errorf("%s: %+v", p, e)
		}
	}
	o.Release(id)
	if len(ops.places()) != 0 {
		t.Errorf("left: %v", ops.places())
	}
	calls := strings.Join(ops.calls, "\n")
	i, j := strings.Index(calls, "release /v/docker/containers/web/t whole=false"), strings.Index(calls, "release /v/docker/containers/web whole=true")
	if i < 0 || j < 0 || i > j || !strings.HasSuffix(calls, "flush") {
		t.Errorf("calls:\n%s", calls)
	}
	if got := fmt.Sprint(*removed); !strings.Contains(got, "docker/containers/web/data") || !strings.Contains(got, "docker/containers/web]") {
		t.Errorf("removed: %s", got)
	}
	if _, ok := o.tree.Table().Lookup("/v/docker/containers/web/data"); ok {
		t.Error("a bind is still exported")
	}
}

func TestContainersAcrossRestartsAndTheEnginesEnd(t *testing.T) {
	o, ops, _ := newObjects(t)
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	ops.unmounted[path.Join(EngineRoots, b)] = true
	if err := o.Attach("ns", b, "late"); err != nil {
		t.Errorf("an unmounted root: %v", err)
	}
	if len(ops.places()) != 0 {
		t.Errorf("places: %v", ops.places())
	}
	delete(ops.unmounted, path.Join(EngineRoots, b))
	for i := 0; i < 3; i++ {
		if err := o.Attach("ns", a, "web"); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(strings.Join(ops.calls, "\n"), "release /v/docker/containers/web whole=true"); n != 2 {
		t.Errorf("%d releases of the old copies, want 2: %v", n, ops.calls)
	}
	o.Rename(a, "api")
	if got := ops.places(); len(got) != 1 || !strings.HasPrefix(got[0], "/v/docker/containers/api=") {
		t.Errorf("after the rename: %v", got)
	}
	if err := o.Attach("ns", b, "late"); err != nil {
		t.Fatal(err)
	}
	o.ReleaseContainers()
	if len(ops.places()) != 0 || len(o.Containers()) != 0 {
		t.Errorf("left: %v %v", ops.places(), o.Containers())
	}
}

func TestFailuresLeaveNothingAndCloseStopsEverything(t *testing.T) {
	o, ops, _ := newObjects(t)
	id := strings.Repeat("c", 64)
	ops.fail["clone:"+path.Join(EngineRoots, id)] = errors.New("boom")
	if err := o.Attach("ns", id, "web"); err == nil {
		t.Error("a failed copy is no error")
	}
	if len(o.tree.Table().Paths()) != 1 || ops.dirs["/v/docker/containers/web"] {
		t.Errorf("left: %v %v", o.tree.Table().Paths(), ops.dirs)
	}
	ops.fail["bind:/v/pg"] = errors.New("no")
	o.Close()
	if err := o.Attach("ns", strings.Repeat("d", 64), "x"); !errors.Is(err, ErrClosed) {
		t.Errorf("after Close: %v", err)
	}
	o.SyncVolumes([]VolumeSpec{{Name: "pg", Data: "/d"}})
	if len(ops.places()) != 0 || !o.images.(*fakeLazy).stopped {
		t.Errorf("after Close: %v", ops.places())
	}
}
