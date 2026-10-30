package btrfs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"

	"fev/guest/internal/looptest"
)

func TestStructSizesMatchTheKernelHeader(t *testing.T) {
	for name, c := range map[string]struct{ got, want uintptr }{
		"btrfs_ioctl_vol_args":           {unsafe.Sizeof(volArgs{}), 4096},
		"btrfs_ioctl_vol_args_v2":        {unsafe.Sizeof(volArgsV2{}), 4096},
		"btrfs_ioctl_fs_info_args":       {unsafe.Sizeof(fsInfoArgs{}), 1024},
		"btrfs_ioctl_dev_info_args":      {unsafe.Sizeof(devInfoArgs{}), 4096},
		"btrfs_ioctl_get_dev_stats":      {unsafe.Sizeof(devStatsArgs{}), 1032},
		"btrfs_ioctl_subvol_wait":        {unsafe.Sizeof(subvolWait{}), 16},
		"fstrim_range":                   {unsafe.Sizeof(fstrimRange{}), 24},
		"btrfs_ioctl_ino_lookup_args":    {unsafe.Sizeof(inoLookupArgs{}), 4096},
		"btrfs_ioctl_quota_ctl_args":     {unsafe.Sizeof(quotaCtlArgs{}), 16},
		"btrfs_ioctl_qgroup_assign_args": {unsafe.Sizeof(qgroupAssignArgs{}), 24},
		"btrfs_ioctl_qgroup_create_args": {unsafe.Sizeof(qgroupCreateArgs{}), 16},
		"btrfs_ioctl_qgroup_limit_args":  {unsafe.Sizeof(qgroupLimitArgs{}), 48},
		"btrfs_scrub_progress":           {unsafe.Sizeof(ScrubProgress{}), 120},
		"btrfs_ioctl_scrub_args":         {unsafe.Sizeof(scrubArgs{}), 1024},
		"btrfs_ioctl_search_key":         {unsafe.Sizeof(searchKey{}), 104},
		"btrfs_ioctl_search_args":        {unsafe.Sizeof(searchArgs{}), 4096},
		"btrfs_ioctl_search_header":      {unsafe.Sizeof(searchHeader{}), 32},
		"btrfs_qgroup_inherit":           {unsafe.Sizeof(qgroupInheritHead{}), 72},
	} {
		if c.got != c.want {
			t.Errorf("%s is %d bytes, the header's is %d", name, c.got, c.want)
		}
	}
	if iocSync != 0x9408 || iocFitrim != 0xc0185879 || iocFSInfo != 0x8400941f {
		t.Errorf("ioctl numbers: sync %#x fitrim %#x fs_info %#x", iocSync, iocFitrim, iocFSInfo)
	}
	if iocSnapCreateV2 != 0x50009417 || iocSnapDestroyV2 != 0x5000943f || iocTreeSearch != 0xd0009411 ||
		iocSubvolGetFlags != 0x80089419 || iocSubvolSetFlags != 0x4008941a {
		t.Errorf("ioctl numbers: snap_create_v2 %#x snap_destroy_v2 %#x tree_search %#x getflags %#x setflags %#x",
			iocSnapCreateV2, iocSnapDestroyV2, iocTreeSearch, iocSubvolGetFlags, iocSubvolSetFlags)
	}
	if iocScrub != 0xc400941b || iocScrubCancel != 0x941c || iocScrubProgress != 0xc400941d {
		t.Errorf("ioctl numbers: scrub %#x cancel %#x progress %#x", iocScrub, iocScrubCancel, iocScrubProgress)
	}
}

func mounted(t *testing.T, size int64) (root *os.File, dev, backing string) {
	t.Helper()
	tools := looptest.Tools(t)
	dev, backing = looptest.Device(t, size)
	if out, err := exec.Command(filepath.Join(tools, "mkfs.btrfs"), "-q", "-m", "dup", dev).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.btrfs: %v: %s", err, out)
	}
	target := t.TempDir()
	if err := unix.Mount(dev, target, "btrfs", 0, ""); err != nil {
		t.Fatalf("mount: %v", err)
	}
	root, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		root.Close()
		unix.Unmount(target, 0)
	})
	return root, dev, backing
}

func TestIoctlsAgainstARealFilesystem(t *testing.T) {
	root, dev, backing := mounted(t, 1<<30)

	info, err := GetFSInfo(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Devices != 1 || info.NodeSize != 16384 || info.SectorSize != 4096 || info.CsumType != 0 || info.FSID == [16]byte{} || info.Generation == 0 {
		t.Fatalf("fs info: %+v", info)
	}

	if err := CreateSubvolume(root, "@one"); err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(root.Name(), "@one"), &st); err != nil || st.Ino != 256 {
		t.Fatalf("@one: inode %d err %v", st.Ino, err)
	}
	if err := CreateSubvolume(root, "@one"); err != unix.EEXIST {
		t.Fatalf("creating it twice: %v", err)
	}
	if err := CreateSubvolume(root, ""); err == nil {
		t.Fatal("an empty name was accepted")
	}

	if err := Sync(root); err != nil {
		t.Fatal(err)
	}
	stats, err := GetDevStats(root, 1)
	if err != nil || stats != (DevStats{}) {
		t.Fatalf("device stats on a new filesystem: %+v err %v", stats, err)
	}
	if _, err := GetDevStats(root, 9); err == nil {
		t.Fatal("stats for a device that does not exist")
	}
	if err := WaitDeletedSubvolumes(root); err != nil {
		t.Fatalf("waiting with nothing queued: %v", err)
	}

	before, err := GetDevInfo(root, 1)
	if err != nil || before.TotalBytes != 1<<30 {
		t.Fatalf("device info: %+v err %v", before, err)
	}
	if err := os.Truncate(backing, 2<<30); err != nil {
		t.Fatal(err)
	}
	looptest.Grow(t, dev)
	if err := GrowToDevice(root, 1); err != nil {
		t.Fatal(err)
	}
	if after, _ := GetDevInfo(root, 1); after.TotalBytes != 2<<30 {
		t.Fatalf("after growing: %+v", after)
	}
}

func TestTrimReturnsDeletedSpaceToTheBackingFile(t *testing.T) {
	root, _, backing := mounted(t, 2<<30)
	allocated := func() int64 {
		var st unix.Stat_t
		if err := unix.Stat(backing, &st); err != nil {
			t.Fatal(err)
		}
		return st.Blocks * 512
	}
	file := filepath.Join(root.Name(), "big")
	data := make([]byte, 256<<20)
	for i := range data {
		data[i] = byte(i*31 + i>>12)
	}
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Sync(root); err != nil {
		t.Fatal(err)
	}
	full := allocated()
	if full < 256<<20 {
		t.Fatalf("backing file holds %d bytes after writing 256 MiB", full)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := Sync(root); err != nil {
		t.Fatal(err)
	}
	n, err := Trim(root, 0)
	if err != nil || n < 256<<20 {
		t.Fatalf("trimmed %d bytes, err %v", n, err)
	}
	if after := allocated(); full-after < 240<<20 {
		t.Fatalf("backing file went from %d to %d bytes; the 256 MiB did not come back", full, after)
	}
}

func copiesOf(t *testing.T, dev string, tree string) []int64 {
	t.Helper()
	tool := filepath.Join(looptest.Tools(t), "btrfs")
	dump := func(args ...string) string {
		out, err := exec.Command(tool, append([]string{"inspect-internal", "dump-tree"}, append(args, dev)...)...).Output()
		if err != nil {
			t.Fatalf("dump-tree %v: %v", args, err)
		}
		return string(out)
	}
	m := regexp.MustCompile(`key \(` + tree + ` ROOT_ITEM 0\).*\n.*bytenr (\d+)`).FindStringSubmatch(dump("-t", "root"))
	if m == nil {
		t.Fatalf("no root item for tree %s", tree)
	}
	logical, _ := strconv.ParseInt(m[1], 10, 64)
	for _, item := range regexp.MustCompile(`\n\titem \d+ `).Split(dump("-t", "chunk"), -1) {
		c := regexp.MustCompile(`key \(FIRST_CHUNK_TREE CHUNK_ITEM (\d+)\).*\n\s+length (\d+) .* type (\S+)`).FindStringSubmatch(item)
		if c == nil {
			continue
		}
		start, _ := strconv.ParseInt(c[1], 10, 64)
		length, _ := strconv.ParseInt(c[2], 10, 64)
		if logical < start || logical >= start+length {
			continue
		}
		var at []int64
		for _, s := range regexp.MustCompile(`stripe \d+ devid 1 offset (\d+)`).FindAllStringSubmatch(item, -1) {
			p, _ := strconv.ParseInt(s[1], 10, 64)
			at = append(at, p+logical-start)
		}
		if !strings.Contains(c[3], "DUP") || len(at) != 2 {
			t.Fatalf("tree %s's root at %d is in a %s chunk with %d stripes", tree, logical, c[3], len(at))
		}
		return at
	}
	t.Fatalf("no chunk holds %d", logical)
	return nil
}

func TestScrubFindsAndRepairsABadTreeBlock(t *testing.T) {
	for _, c := range []struct {
		name     string
		both     bool
		readOnly bool
	}{{"one copy", false, false}, {"one copy, read-only", false, true}, {"both copies", true, false}} {
		t.Run(c.name, func(t *testing.T) {
			root, dev, _ := mounted(t, 1<<30)
			if err := CreateSubvolume(root, "@one"); err != nil {
				t.Fatal(err)
			}
			for i := range 200 {
				os.WriteFile(filepath.Join(root.Name(), "@one", fmt.Sprintf("f%03d", i)), make([]byte, 8192), 0o644)
			}
			if err := Sync(root); err != nil {
				t.Fatal(err)
			}
			clean, err := Scrub(root, 1, false)
			if err != nil || clean.TreeBytesScrubbed == 0 || clean.DataBytesScrubbed == 0 ||
				clean.UncorrectableErrors+clean.CorrectedErrors+clean.CsumErrors+clean.VerifyErrors+clean.ReadErrors != 0 {
				t.Fatalf("a scrub of a sound filesystem: %+v err %v", clean, err)
			}
			if _, err := ScrubStatus(root, 1); err != ErrNoScrub {
				t.Fatalf("status with no scrub running: %v", err)
			}
			if err := CancelScrub(root); err != ErrNoScrub {
				t.Fatalf("cancel with no scrub running: %v", err)
			}

			target := root.Name()
			root.Close()
			if err := unix.Unmount(target, 0); err != nil {
				t.Fatal(err)
			}
			at := copiesOf(t, dev, "256")
			if !c.both {
				at = at[:1]
			}
			d, err := os.OpenFile(dev, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, off := range at {
				if _, err := d.WriteAt(make([]byte, 16384), off); err != nil {
					t.Fatal(err)
				}
			}
			d.Sync()
			d.Close()
			flags := uintptr(0)
			if c.readOnly {
				flags = unix.MS_RDONLY
			}
			if err := unix.Mount(dev, target, "btrfs", flags, ""); err != nil {
				t.Fatalf("mount after the damage: %v", err)
			}
			if root, err = os.Open(target); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { root.Close() })
			found := func(p ScrubProgress) uint64 { return p.CsumErrors + p.VerifyErrors + p.ReadErrors }
			if c.readOnly {
				if p, err := Scrub(root, 1, false); err == nil {
					t.Errorf("a repairing scrub ran on a read-only mount: %+v", p)
				}
			}
			got, err := Scrub(root, 1, c.readOnly)
			switch {
			case err != nil:
				t.Fatalf("scrub: %v", err)
			case found(got) == 0:
				t.Errorf("the zeroed block was not found: %+v", got)
			case c.both && got.UncorrectableErrors == 0:
				t.Errorf("both copies zeroed, nothing uncorrectable: %+v", got)
			case !c.both && got.UncorrectableErrors != 0:
				t.Errorf("one copy zeroed, and uncorrectable: %+v", got)
			case !c.both && !c.readOnly && got.CorrectedErrors == 0:
				t.Errorf("one copy zeroed, nothing corrected: %+v", got)
			}
			again, err := Scrub(root, 1, c.readOnly)
			if err != nil {
				t.Fatal(err)
			}
			if repaired := !c.both && !c.readOnly; repaired != (found(again) == 0) {
				t.Errorf("the second scrub: %+v", again)
			}
			t.Logf("first %+v\nagain %+v", got, again)
		})
	}
}
