package storage

import (
	"testing"

	api "fev/proto/agent"

	"fev/guest/internal/btrfs"
)

func TestAnImageRefIsPlainNames(t *testing.T) {
	good := api.ImageRef{Distro: "alpine", Release: "3.23", Arch: "arm64", Variant: "default", Serial: "20261013_13:00"}
	if err := checkRef(good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []func(*api.ImageRef){
		func(r *api.ImageRef) { r.Distro = "" },
		func(r *api.ImageRef) { r.Release = ".." },
		func(r *api.ImageRef) { r.Arch = "a/b" },
		func(r *api.ImageRef) { r.Variant = ".hidden" },
		func(r *api.ImageRef) { r.Serial = "x\x00y" },
	} {
		r := good
		bad(&r)
		if checkRef(r) == nil {
			t.Errorf("%+v was taken", r)
		}
	}
}

func TestDescendantsAndDepth(t *testing.T) {
	refs := []btrfs.SubvolumeRef{
		{ID: 256, Parent: 5}, {ID: 300, Parent: 256}, {ID: 301, Parent: 300},
		{ID: 302, Parent: 256}, {ID: 400, Parent: 5},
	}
	got := descendants(refs, 256)
	want := map[uint64]bool{300: true, 301: true, 302: true}
	if len(got) != 3 {
		t.Fatalf("%v", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("%d is not under 256: %v", id, got)
		}
	}
	if depthOf(refs, 301) != 3 || depthOf(refs, 302) != 2 || depthOf(refs, 5) != 0 {
		t.Fatalf("depths %d %d", depthOf(refs, 301), depthOf(refs, 302))
	}
	if machineGroup(300) != 1<<48|300 {
		t.Fatal("a machine's group is 1/ID")
	}
}
