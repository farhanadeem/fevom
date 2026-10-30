package machines

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type entry struct {
	h    tar.Header
	body string
}

func tarOf(t *testing.T, entries []entry) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, e := range entries {
		h := e.h
		h.Size = int64(len(e.body))
		h.Format = tar.FormatPAX
		if h.ModTime.IsZero() {
			h.ModTime = time.Unix(1700000000, 0)
		}
		if err := w.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	return &b
}

func rootFd(t *testing.T, dir string) int {
	t.Helper()
	fd, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	return fd
}

func TestUnpackKeepsWhatTheImageSays(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root")
	}
	dir := t.TempDir()
	capV2 := make([]byte, 20)
	binary.LittleEndian.PutUint32(capV2[0:], 0x02000001)
	binary.LittleEndian.PutUint32(capV2[4:], 1<<13)
	b := tarOf(t, []entry{
		{h: tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}},
		{h: tar.Header{Name: "usr/", Typeflag: tar.TypeDir, Mode: 0o755}},
		{h: tar.Header{Name: "usr/bin/", Typeflag: tar.TypeDir, Mode: 0o755}},
		{h: tar.Header{Name: "usr/bin/sudo", Typeflag: tar.TypeReg, Mode: 0o4755}, body: "sudo"},
		{h: tar.Header{Name: "usr/bin/ping", Typeflag: tar.TypeReg, Mode: 0o755, PAXRecords: map[string]string{"SCHILY.xattr.security.capability": string(capV2)}}, body: "ping"},
		{h: tar.Header{Name: "usr/bin/sudoedit", Typeflag: tar.TypeLink, Linkname: "usr/bin/sudo"}},
		{h: tar.Header{Name: "bin", Typeflag: tar.TypeSymlink, Linkname: "usr/bin"}},
		{h: tar.Header{Name: "home/", Typeflag: tar.TypeDir, Mode: 0o755}},
		{h: tar.Header{Name: "home/u/", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 1000}},
		{h: tar.Header{Name: "home/u/f", Typeflag: tar.TypeReg, Mode: 0o600, Uid: 1000, Gid: 1000}, body: "old"},
		{h: tar.Header{Name: "home/u/f", Typeflag: tar.TypeReg, Mode: 0o640, Uid: 1000, Gid: 1000}, body: "new"},
		{h: tar.Header{Name: "dev/", Typeflag: tar.TypeDir, Mode: 0o755}},
		{h: tar.Header{Name: "dev/null", Typeflag: tar.TypeChar, Mode: 0o666, Devmajor: 1, Devminor: 3}},
		{h: tar.Header{Name: "ro/", Typeflag: tar.TypeDir, Mode: 0o555}},
		{h: tar.Header{Name: "ro/inside", Typeflag: tar.TypeReg, Mode: 0o644}, body: "x"},
	})
	n, warnings, err := Unpack(rootFd(t, dir), b)
	if err != nil || n != 15 {
		t.Fatalf("%d entries, %v", n, err)
	}
	if len(warnings) > 0 {
		t.Logf("warnings: %v", warnings)
	}
	mode := func(p string) (os.FileMode, uint32) {
		var st unix.Stat_t
		if err := unix.Lstat(filepath.Join(dir, p), &st); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Lstat(filepath.Join(dir, p))
		return fi.Mode(), st.Uid
	}
	if m, _ := mode("usr/bin/sudo"); m&os.ModeSetuid == 0 {
		t.Errorf("sudo lost its setuid bit: %v", m)
	}
	if v, err := unix.Getxattr(filepath.Join(dir, "usr/bin/ping"), "security.capability", make([]byte, 64)); err != nil || v != 20 {
		t.Errorf("ping's capability: %d, %v", v, err)
	}
	var a, c unix.Stat_t
	unix.Stat(filepath.Join(dir, "usr/bin/sudo"), &a)
	unix.Stat(filepath.Join(dir, "usr/bin/sudoedit"), &c)
	if a.Ino != c.Ino {
		t.Error("the hard link is a copy")
	}
	if l, _ := os.Readlink(filepath.Join(dir, "bin")); l != "usr/bin" {
		t.Errorf("bin -> %q", l)
	}
	if m, uid := mode("home/u"); m.Perm() != 0o700 || uid != 1000 {
		t.Errorf("home/u: %v uid %d", m, uid)
	}
	if body, _ := os.ReadFile(filepath.Join(dir, "home/u/f")); string(body) != "new" {
		t.Errorf("the later entry did not win: %q", body)
	}
	if m, _ := mode("home/u/f"); m.Perm() != 0o640 {
		t.Errorf("f: %v", m)
	}
	if m, _ := mode("dev/null"); m&os.ModeCharDevice == 0 {
		t.Errorf("dev/null: %v", m)
	}
	if m, _ := mode("ro"); m.Perm() != 0o555 {
		t.Errorf("ro: %v", m)
	}
	if body, _ := os.ReadFile(filepath.Join(dir, "ro/inside")); string(body) != "x" {
		t.Error("a file in a read-only directory")
	}
	if m, _ := mode("."); m.Perm() != 0o755 {
		t.Errorf("the root: %v", m)
	}

	over := t.TempDir()
	os.MkdirAll(filepath.Join(over, "home/u"), 0o755)
	os.WriteFile(filepath.Join(over, "home/u/f"), []byte("rendered"), 0o666)
	written, err := Overlay(rootFd(t, dir), over)
	if err != nil || strings.Join(written, " ") != "/home/u/f" {
		t.Fatalf("overlay %v, %v", written, err)
	}
	if m, uid := mode("home/u/f"); m.Perm() != 0o640 || uid != 1000 {
		t.Errorf("a template's file: %v uid %d", m, uid)
	}
}

func TestUnpackStaysInsideTheRoot(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root")
	}
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	os.WriteFile(victim, []byte("intact"), 0o600)
	for name, entries := range map[string][]entry{
		"dot-dot": {{h: tar.Header{Name: "../../victim", Typeflag: tar.TypeReg, Mode: 0o644}, body: "x"}},
		"absolute link": {
			{h: tar.Header{Name: "out", Typeflag: tar.TypeSymlink, Linkname: outside}},
			{h: tar.Header{Name: "out/victim", Typeflag: tar.TypeReg, Mode: 0o644}, body: "x"},
		},
		"relative link": {
			{h: tar.Header{Name: "out", Typeflag: tar.TypeSymlink, Linkname: "../../../../../../../.." + outside}},
			{h: tar.Header{Name: "out/victim", Typeflag: tar.TypeReg, Mode: 0o644}, body: "x"},
		},
		"hard link": {{h: tar.Header{Name: "h", Typeflag: tar.TypeLink, Linkname: "../../../../../../.." + victim}}},
		"dir over link": {
			{h: tar.Header{Name: "d", Typeflag: tar.TypeSymlink, Linkname: outside}},
			{h: tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o777}},
		},
	} {
		dir := t.TempDir()
		_, _, err := Unpack(rootFd(t, dir), tarOf(t, entries))
		if body, _ := os.ReadFile(victim); string(body) != "intact" {
			t.Fatalf("%s: wrote outside the root", name)
		}
		if st, _ := os.Stat(outside); st.Mode().Perm() == 0o777 {
			t.Fatalf("%s: changed a mode outside the root", name)
		}
		if err == nil {
			found := false
			filepath.WalkDir(dir, func(p string, d os.DirEntry, _ error) error {
				if d != nil && d.Name() == "victim" {
					found = true
				}
				return nil
			})
			if !found && name != "hard link" {
				t.Errorf("%s: neither refused nor written inside", name)
			}
		}
		if name == "dir over link" && err == nil {
			t.Errorf("%s: a directory entry over a link was taken", name)
		}
	}
}

func TestTheDeviceProgramRefusesOnlyTheVMsDisks(t *testing.T) {
	const blk, chr = 1, 2
	for _, majors := range [][]uint32{{254}, {254, 259}} {
		p := deviceProgram(majors)
		for _, c := range []struct {
			kind, major uint32
			access      uint32
			want        uint32
		}{
			{blk, 254, 1, 0}, {blk, 254, 2, 0}, {blk, 254, 4, 0},
			{blk, 7, 2, 1},
			{chr, 254, 2, 1}, {chr, 1, 2, 1}, {chr, 10, 6, 1},
			{blk, majors[len(majors)-1], 6, 0},
		} {
			if got := runDevice(t, p, c.kind|c.access<<16, c.major); got != c.want {
				t.Errorf("majors %v: type %d major %d access %d: %d, want %d", majors, c.kind, c.major, c.access, got, c.want)
			}
		}
	}
}

func runDevice(t *testing.T, p []byte, accessType, major uint32) uint32 {
	t.Helper()
	var r [11]uint64
	ctx := [3]uint32{accessType, major, 0}
	for pc := 0; pc < len(p)/8; pc++ {
		i := p[pc*8:]
		code, dst, src := i[0], i[1]&0xf, i[1]>>4
		off := int16(binary.LittleEndian.Uint16(i[2:]))
		imm := int32(binary.LittleEndian.Uint32(i[4:]))
		switch code {
		case 0x61:
			if src != 1 {
				t.Fatal("load from other than the context")
			}
			r[dst] = uint64(ctx[off/4])
		case 0x54:
			r[dst] = uint64(uint32(r[dst]) & uint32(imm))
		case 0xb7:
			r[dst] = uint64(imm)
		case 0x55:
			if r[dst] != uint64(imm) {
				pc += int(off)
			}
		case 0x15:
			if r[dst] == uint64(imm) {
				pc += int(off)
			}
		case 0x05:
			pc += int(off)
		case 0x95:
			return uint32(r[0])
		default:
			t.Fatalf("instruction %#x", code)
		}
	}
	t.Fatal("ran off the end")
	return 0
}

func TestRemoveTree(t *testing.T) {
	dir := t.TempDir()
	g := filepath.Join(dir, "m")
	os.MkdirAll(filepath.Join(g, "payload/.fev/1"), 0o755)
	os.MkdirAll(filepath.Join(g, "payload/init.scope"), 0o755)
	if err := removeTree(g); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(g); !os.IsNotExist(err) {
		t.Errorf("left: %v", err)
	}
	if err := removeTree(g); err != nil {
		t.Errorf("again: %v", err)
	}
	os.MkdirAll(filepath.Join(dir, "p/.fev/1"), 0o755)
	os.MkdirAll(filepath.Join(dir, "p/.fev/2"), 0o755)
	os.WriteFile(filepath.Join(dir, "p/.fev/2/f"), nil, 0o644)
	sweepSessions(filepath.Join(dir, "p"))
	if _, err := os.Stat(filepath.Join(dir, "p/.fev/1")); !os.IsNotExist(err) {
		t.Error("an empty session group was kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "p/.fev/2")); err != nil {
		t.Error("a session group that holds something was removed")
	}
	if haltSignal(InitSystemd) != 37 || haltSignal(InitBusybox) != unix.SIGUSR2 || haltSignal(InitSysvinit) != unix.SIGPWR || haltSignal(InitRunit) != unix.SIGCONT {
		t.Error("halt signals")
	}
	if l := HostLink("a-machine-with-a-very-long-name-indeed"); len(l) > 15 || l == HostLink("another") {
		t.Errorf("host link %q", l)
	}
}
