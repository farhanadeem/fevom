package main

import (
	"errors"
	"reflect"
	"testing"

	api "fev/proto/agent"

	"fev/guest/internal/logbuf"
	"fev/guest/internal/stage"
)

func TestStageGraphIsWellFormed(t *testing.T) {
	s := &system{}
	stages := s.stages(logbuf.New(1, func() float64 { return 0 }, nil))
	for i := range stages {
		stages[i].Run = func() error { return nil }
	}
	results, err := stage.Run(stages, func() float64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != len(stages) {
		t.Fatalf("%d results for %d stages", len(results), len(stages))
	}
	for _, r := range results {
		if r.NotRun || r.Err != nil {
			t.Fatalf("stage %s: %+v", r.Name, r)
		}
	}
}

func TestTheEngineStageNeedsCgroupsTheDataDiskAndTheNetworkBase(t *testing.T) {
	for name, wantRun := range map[string]bool{
		"cgroups": false, "data-disk": false, "net-base": false,
		"network": true, "streams": true, "agent": true, "fileshare": true,
	} {
		s := &system{}
		stages := s.stages(logbuf.New(1, func() float64 { return 0 }, nil))
		ran := false
		for i := range stages {
			switch stages[i].Name {
			case name:
				stages[i].Run = func() error { return errors.New("broken") }
			case "engine":
				stages[i].Run = func() error { ran = true; return nil }
			default:
				stages[i].Run = func() error { return nil }
			}
		}
		results, err := stage.Run(stages, func() float64 { return 0 })
		if err != nil {
			t.Fatal(err)
		}
		last := results[len(results)-1]
		if last.Name != "engine" || ran != wantRun || last.NotRun == wantRun {
			t.Errorf("with %s failed: engine ran=%v, result %+v", name, ran, last)
		}
	}
}

func TestTheFileShareComesBeforeTheEngine(t *testing.T) {
	s := &system{}
	stages := s.stages(logbuf.New(1, func() float64 { return 0 }, nil))
	var order []string
	release := make(chan struct{})
	for i := range stages {
		name := stages[i].Name
		switch name {
		case "fileshare":
			stages[i].Run = func() error {
				<-release
				order = append(order, name)
				return stage.Skip("no file share attached")
			}
		case "engine":
			stages[i].Run = func() error { order = append(order, name); return nil }
		case "net-base":
			stages[i].Run = func() error { close(release); return nil }
		default:
			stages[i].Run = func() error { return nil }
		}
	}
	if _, err := stage.Run(stages, func() float64 { return 0 }); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "fileshare" || order[1] != "engine" {
		t.Fatalf("order %v", order)
	}
}

func TestTheSharesBindsLeaveTheGuestsOwnPathsAlone(t *testing.T) {
	seen := map[string]bool{api.ShareTarget: true}
	for _, b := range api.SharePaths {
		if seen[b.Target] {
			t.Errorf("%s is bound twice", b.Target)
		}
		seen[b.Target] = true
		for _, own := range []string{"/", "/var", "/var/run", "/var/lib", "/etc", "/usr", "/run", "/dev", "/proc", "/sys"} {
			if b.Target == own {
				t.Errorf("%s is the guest's own and must not be the Mac's", own)
			}
		}
	}
	for _, want := range []string{"/mnt/mac", "/Users", "/Volumes", "/private", "/Applications", "/Library", "/System", "/opt/homebrew", "/usr/local", "/tmp", "/var/folders"} {
		if !seen[want] {
			t.Errorf("the engine's mount points lack %s", want)
		}
	}
}

func TestTheSharesFollowersAreToldOnlyOfChanges(t *testing.T) {
	f := newFileShare(logbuf.New(8, func() float64 { return 0 }, nil))
	var got []api.FileShare
	stop := f.follow(func(s api.FileShare) { got = append(got, s) })
	f.announce()
	if len(got) != 0 {
		t.Fatalf("a guest with no share told its follower %+v", got)
	}
	stop()

	f.mounted = true
	stop = f.follow(func(s api.FileShare) { got = append(got, s) })
	if len(got) != 1 || got[0].Tag != shareTag || got[0].Target != shareMount || got[0].Device || got[0].Feed != api.FeedNone {
		t.Fatalf("the state at once: %+v", got)
	}
	f.announce()
	f.announce()
	if len(got) != 1 {
		t.Fatalf("told again of a state that had not changed: %+v", got)
	}
	f.device = 99
	f.announce()
	f.helper.Emit = nil
	f.helper.Connect()
	if len(got) != 3 || !got[1].Device || got[2].FeedError == "" || got[2].Feed != api.FeedNone {
		t.Fatalf("after the device and a failed dial: %+v", got)
	}
	stop()
	f.device = -1
	f.announce()
	if len(got) != 3 {
		t.Fatalf("told after it stopped following: %+v", got)
	}
	var late []api.FileShare
	defer f.follow(func(s api.FileShare) { late = append(late, s) })()
	if len(late) != 1 || late[0].Device || !reflect.DeepEqual(late[0], *f.info()) {
		t.Fatalf("a later follower: %+v", late)
	}
}

func TestTheShutdownUnmountsWhatHoldsTheDisksFirst(t *testing.T) {
	mountinfo := `1 1 0:2 / / rw - rootfs rootfs rw
22 1 0:20 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
30 1 0:25 / /run rw,nosuid,nodev - tmpfs tmpfs rw
34 1 254:16 / /system ro,nosuid,nodev,relatime - erofs /dev/vdb ro
36 1 0:31 / /data rw,nosuid,nodev,noatime - btrfs /dev/vda rw,subvolid=5,subvol=/
35 34 0:31 /@system/mac-bin /system/opt/fev/bin ro,nosuid,nodev,noatime - btrfs /dev/vda rw,subvolid=256,subvol=/@system
40 30 0:40 / /run/fev/mnt-machines rw,nosuid,nodev,noexec shared:9 - tmpfs tmpfs rw,size=1024k
41 40 0:31 /@docker/volumes /run/fev/mnt-machines/docker/volumes rw shared:9 - btrfs /dev/vda rw
42 40 0:31 /@machines/m\040one /run/fev/mnt-machines/m\040one rw shared:9 - btrfs /dev/vda rw
`
	mounts := []api.Mount{
		{Source: "/dev/vdb", Target: "/system", FSType: "erofs"},
		{Source: "/dev/vda", Target: "/data", FSType: "btrfs"},
	}
	got := mountsHolding(mountinfo, mounts)
	want := []string{"/run/fev/mnt-machines/m one", "/run/fev/mnt-machines/docker/volumes", "/system/opt/fev/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("held:\n got %q\nwant %q", got, want)
	}
	if got := mountsHolding(mountinfo, nil); len(got) != 0 {
		t.Errorf("with no mounts of init's: %q", got)
	}
}
