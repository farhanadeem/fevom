package nfsview

import (
	"strings"
	"testing"
)

type event struct {
	set    *Object
	remove *Object
	bind   [2]string
}

func play(tr *Tree, evs []event) []Change {
	var out []Change
	for _, e := range evs {
		switch {
		case e.set != nil:
			out = append(out, tr.Set(*e.set))
		case e.remove != nil:
			out = append(out, tr.Remove(e.remove.Kind, e.remove.Key))
		default:
			out = append(out, tr.SetBind(e.bind[0], e.bind[1], 0, 0))
		}
	}
	return out
}

var (
	cWeb  = Object{Kind: KindContainer, Key: strings.Repeat("1", 64), Name: "web"}
	cAPI  = Object{Kind: KindContainer, Key: strings.Repeat("2", 64), Name: "shop-api-1"}
	iApp  = Object{Kind: KindImage, Key: "sha256:aa ghcr.io/org/app:1.2", Name: "ghcr.io/org/app:1.2", ReadOnly: true}
	iApp2 = Object{Kind: KindImage, Key: "sha256:bb ghcr.io_org_app:1.2", Name: "ghcr.io_org_app:1.2", ReadOnly: true}
	iNone = Object{Kind: KindImage, Key: "sha256:cc", Name: "cc0123456789", ReadOnly: true}
	vData = Object{Kind: KindVolume, Key: "data", Name: "data"}
	mU    = Object{Kind: KindMachine, Key: "u24", Name: "u24", UID: 501, GID: 501}
)

func stream() []event {
	return []event{
		{set: &cWeb}, {set: &cAPI}, {set: &iApp}, {set: &iApp2}, {set: &iNone}, {set: &vData}, {set: &mU},
		{bind: [2]string{cWeb.Key, "/data"}},
	}
}

func TestTheTreeFollowsTheObjects(t *testing.T) {
	tr := NewTree("/run/fev/view", Owner{501, 20})
	got := play(tr, stream())
	want := []string{
		"/run/fev/view/docker/containers/web",
		"/run/fev/view/docker/containers/shop-api-1",
		"/run/fev/view/docker/images/ghcr.io_org_app:1.2",
		"",
		"/run/fev/view/docker/images/cc0123456789",
		"/run/fev/view/docker/volumes/data",
		"/run/fev/view/u24",
		"/run/fev/view/docker/containers/web/data",
	}
	for i, ch := range got {
		if ch.Refused != "" {
			t.Errorf("event %d refused: %s", i, ch.Refused)
		}
		if want[i] != "" && ch.Path != want[i] {
			t.Errorf("event %d: %s, want %s", i, ch.Path, want[i])
		}
	}
	if p := got[3].Path; !strings.HasPrefix(p, "/run/fev/view/docker/images/ghcr.io_org_app:1.2~") || p == got[2].Path {
		t.Errorf("the second image's place: %s", p)
	}
	seen := map[uint32]string{0: tr.Root()}
	for _, p := range tr.Objects() {
		if o, dup := seen[p.FSID]; dup {
			t.Errorf("fsid %#x of %s and %s", p.FSID, p.Path, o)
		}
		seen[p.FSID] = p.Path
		e, ok := tr.Table().Lookup(p.Path)
		if !ok {
			t.Errorf("%s not exported", p.Path)
			continue
		}
		ro := e.Flags&FlagReadOnly != 0
		if ro != (p.Kind == KindImage) {
			t.Errorf("%s: read-only %v", p.Path, ro)
		}
		if p.Kind == KindMachine && (e.AnonUID != 501 || e.AnonGID != 501) {
			t.Errorf("%s squashed to %d:%d", p.Path, e.AnonUID, e.AnonGID)
		}
		if p.Kind != KindMachine && (e.AnonUID != 0 || e.AnonGID != 0) {
			t.Errorf("%s squashed to %d:%d", p.Path, e.AnonUID, e.AnonGID)
		}
		if e.Flags&FlagAllSquash == 0 || e.Flags&FlagInsecurePort == 0 || e.Flags&FlagFSID == 0 {
			t.Errorf("%s: flags %#x", p.Path, e.Flags)
		}
	}
	root, _ := tr.Table().Lookup(tr.Root())
	if root.FSID != 0 || root.Flags&FlagReadOnly == 0 || root.Flags&FlagAllSquash != 0 || root.Flags&FlagRootSquash == 0 || root.AnonUID != tr.Owner().UID || root.AnonGID != tr.Owner().GID {
		t.Errorf("the root's export: %+v", root)
	}
}

func TestFSIDsAreStableAcrossStartsAndRenames(t *testing.T) {
	a := NewTree("/v", Owner{501, 20})
	play(a, stream())
	b := NewTree("/v", Owner{501, 20})
	evs := stream()
	for i, j := 0, len(evs)-2; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
	play(b, evs)
	for _, p := range a.Objects() {
		q, ok := b.Lookup(p.Kind, p.Key)
		if !ok || q.FSID != p.FSID {
			t.Errorf("%s: fsid %#x at one start, %+v at another", p.Path, p.FSID, q)
		}
	}
	before, _ := a.Lookup(KindContainer, cWeb.Key)
	renamed := cWeb
	renamed.Name = "web-renamed"
	ch := a.Set(renamed)
	if ch.FSID != before.FSID || ch.OldPath != before.Path || ch.Path != "/v/docker/containers/web-renamed" {
		t.Errorf("rename: %+v (was %s %#x)", ch, before.Path, before.FSID)
	}
	if _, ok := a.Table().Lookup(before.Path); ok {
		t.Error("the old name is still exported")
	}
}

func TestRefusalsAndRemovals(t *testing.T) {
	tr := NewTree("/v", Owner{501, 20})
	for _, o := range []Object{
		{Kind: KindMachine, Key: "README.txt", Name: "README.txt"},
		{Kind: KindMachine, Key: "Docker", Name: "Docker"},
		{Kind: KindVolume, Key: "..", Name: ".."},
		{Kind: KindBind, Key: "x", Name: "x"},
	} {
		if ch := tr.Set(o); ch.Refused == "" {
			t.Errorf("%+v shown at %s", o, ch.Path)
		}
	}
	long := strings.Repeat("é", 200)
	ch := tr.Set(Object{Kind: KindVolume, Key: long, Name: long})
	if ch.Refused != "" || len(ch.Path)-len("/v/docker/volumes/") > 255 || !strings.Contains(ch.Path, "~") {
		t.Errorf("a long name: %q %s", ch.Path, ch.Refused)
	}
	tr.Set(cWeb)
	b := tr.SetBind(cWeb.Key, "data", 0, 0)
	if b.Path != "/v/docker/containers/web/data" {
		t.Errorf("bind: %+v", b)
	}
	if ch := tr.SetBind(cAPI.Key, "/x", 0, 0); ch.Refused == "" {
		t.Error("a bind of a container not shown")
	}
	tr.Remove(KindContainer, cWeb.Key)
	if _, ok := tr.Table().Lookup(b.Path); ok {
		t.Error("a removed container's bind is still exported")
	}
	if _, ok := tr.Lookup(KindBind, BindKey(cWeb.Key, "/data")); ok {
		t.Error("a removed container's bind is still in the tree")
	}
}

func TestTheRootsOwnFiles(t *testing.T) {
	names := map[string]bool{}
	for _, f := range RootFiles() {
		names[f.Name] = true
	}
	for _, n := range []string{"README.txt", ".metadata_never_index", ".metadata_never_index_unless_rootfs", ".metadata-never-index", ".fseventsd", DirContainers, DirImages, DirVolumes} {
		if !names[n] {
			t.Errorf("no %s", n)
		}
	}
	if !strings.Contains(README, "RUNNING") {
		t.Error("the README does not say fev is running")
	}
}
