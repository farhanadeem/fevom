package attach

import (
	"reflect"
	"testing"
)

func TestBuildPrefixExcludesPerfEventOpen(t *testing.T) {
	p := BuildPrefix(ArchARM64, true)
	for _, in := range p {
		if in.Code == insnJEQK && in.K == 241 {
			t.Fatalf("the prefix permits perf_event_open (nr 241): %+v", p)
		}
	}
	want := map[uint32]bool{ArchARM64.Ptrace: false, ArchARM64.VMReadv: false, ArchARM64.VMWritev: false}
	for _, in := range p {
		if in.Code == insnJEQK && in.K != ArchARM64.Audit {
			if _, ok := want[in.K]; !ok {
				t.Fatalf("the prefix permits an unexpected call nr %d", in.K)
			}
			want[in.K] = true
		}
	}
	for nr, seen := range want {
		if !seen {
			t.Fatalf("the prefix does not permit call nr %d", nr)
		}
	}
}

func TestBuildPrefixArchGuardJumpsPastPrefix(t *testing.T) {
	p := BuildPrefix(ArchARM64, true)
	if len(p) == 0 {
		t.Fatal("no prefix")
	}
	if p[0].Code != insnLDWABS || p[0].K != dataArch {
		t.Fatalf("prefix[0] is not LD arch: %+v", p[0])
	}
	guard := p[1]
	if guard.Code != insnJEQK || guard.K != ArchARM64.Audit || guard.JT != 0 {
		t.Fatalf("prefix[1] is not the arch guard: %+v", guard)
	}
	if land := 1 + 1 + int(guard.JF); land != len(p) {
		t.Fatalf("arch-mismatch lands at %d, want %d (past the prefix)", land, len(p))
	}
	last := p[len(p)-1]
	if last.Code != insnRETK || last.K != retAllow {
		t.Fatalf("prefix does not end in RET ALLOW: %+v", last)
	}
}

func TestBuildPrefixCallsJumpToReturnAllow(t *testing.T) {
	p := BuildPrefix(ArchARM64, false)
	retIdx := len(p) - 1
	for i, in := range p {
		if in.Code == insnJEQK {
			if land := i + 1 + int(in.JT); land != retIdx {
				t.Fatalf("JEQ at %d (nr %d) lands at %d, want RET ALLOW at %d", i, in.K, land, retIdx)
			}
		}
	}
	ja := p[retIdx-1]
	if ja.Code != insnJAK || ja.K != 1 {
		t.Fatalf("the skip before RET ALLOW is not JA 1: %+v", ja)
	}
}

func TestWithPrefixKeepsTargetProgramVerbatim(t *testing.T) {
	prog := []Insn{{Code: insnLDWABS, K: dataNR}, {Code: insnRETK, K: 0x7fff0000}}
	p := BuildPrefix(ArchARM64, true)
	all := WithPrefix(p, prog)
	if len(all) != len(p)+len(prog) {
		t.Fatalf("combined length %d, want %d", len(all), len(p)+len(prog))
	}
	if !reflect.DeepEqual(all[len(p):], prog) {
		t.Fatalf("the target program was altered: %+v", all[len(p):])
	}
}

func TestRunningCapsAddsPtrace(t *testing.T) {
	got := RunningCaps(DockerDefaultCaps)
	if got&(1<<CapSysPtrace) == 0 {
		t.Fatal("CAP_SYS_PTRACE not added")
	}
	if got&^(1<<CapSysPtrace) != DockerDefaultCaps {
		t.Fatalf("running caps changed the target's set: %#x", got)
	}
	if RunningCaps(^uint64(0)) != ^uint64(0) {
		t.Fatal("an all-caps target lost a capability")
	}
}

func TestStoppedImageCapsDropsAdminAndMknod(t *testing.T) {
	got := StoppedImageCaps(0, false)
	if got&(1<<CapSysAdmin) != 0 {
		t.Fatal("CAP_SYS_ADMIN not dropped for a stopped/image session")
	}
	if got&(1<<CapMknod) != 0 {
		t.Fatal("CAP_MKNOD not dropped for a stopped/image session")
	}
	if got&(1<<CapSysPtrace) == 0 {
		t.Fatal("CAP_SYS_PTRACE not added for a stopped/image session")
	}
	if got&(1<<0) == 0 {
		t.Fatal("CAP_CHOWN was dropped unexpectedly")
	}
}

func TestStoppedCapsIntersectInspect(t *testing.T) {
	inspect := uint64(1 << 0)
	got := StoppedImageCaps(inspect, true)
	want := uint64(1<<0) | (1 << CapSysPtrace)
	if got != want {
		t.Fatalf("intersected caps %#x, want %#x", got, want)
	}
	inspectWithAdmin := DockerDefaultCaps | (1 << CapSysAdmin)
	got = StoppedImageCaps(inspectWithAdmin, true)
	if got&(1<<CapSysAdmin) != 0 {
		t.Fatal("a stopped container with --cap-add SYS_ADMIN regained it")
	}
}

func TestDropListIsTheComplement(t *testing.T) {
	keep := uint64(1<<0 | 1<<CapSysPtrace)
	drop := DropList(keep)
	for _, c := range drop {
		if keep&(1<<uint(c)) != 0 {
			t.Fatalf("drop list contains a kept capability %d", c)
		}
	}
	if len(drop) != 62 {
		t.Fatalf("drop list length %d, want 62 (64 minus 2 kept)", len(drop))
	}
}

func TestOverlayLowersA3Order(t *testing.T) {
	gd := GraphDriver{Name: "overlay2", Data: map[string]string{
		"UpperDir": "/data/@docker/overlay2/ctr/diff",
		"LowerDir": "/data/@docker/overlay2/l2/diff:/data/@docker/overlay2/l1/diff",
	}}
	lowers, err := OverlayLowers(gd)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/data/@docker/overlay2/ctr/diff",
		"/data/@docker/overlay2/l2/diff",
		"/data/@docker/overlay2/l1/diff",
	}
	if !reflect.DeepEqual(lowers, want) {
		t.Fatalf("lowers %v, want %v (container upper topmost, then image layers)", lowers, want)
	}
	if ContainerUpper(gd) != want[0] {
		t.Fatalf("container upper %q", ContainerUpper(gd))
	}
}

func TestOverlayLowersRejectsOtherDriver(t *testing.T) {
	if _, err := OverlayLowers(GraphDriver{Name: "btrfs", Data: map[string]string{}}); err == nil {
		t.Fatal("a non-overlay driver was accepted")
	}
}

func TestBuildEnvPutsToolsFirstAndKeepsTarget(t *testing.T) {
	target := []string{"PATH=/usr/bin:/bin", "FOO=bar", "TERM=dumb"}
	env := BuildEnv(target, "xterm-256color", "web")
	path := envValue(env, "PATH")
	wantPrefix := "/nix/var/fev/profile/bin:/nix/var/fev/bin:/usr/bin:/bin"
	if path != wantPrefix {
		t.Fatalf("PATH %q, want %q", path, wantPrefix)
	}
	if envValue(env, "FOO") != "bar" {
		t.Fatal("the target's own variable was dropped")
	}
	if envValue(env, "TERM") != "xterm-256color" {
		t.Fatalf("TERM %q, want the Mac's", envValue(env, "TERM"))
	}
	if envValue(env, "HOME") != HomeDir {
		t.Fatalf("HOME %q, want %q", envValue(env, "HOME"), HomeDir)
	}
	if envValue(env, "LANG") != "C.UTF-8" {
		t.Fatal("LANG is not C.UTF-8")
	}
	if envValue(env, "FEV_DEBUG_TARGET") != "web" {
		t.Fatal("FEV_DEBUG_TARGET not set to the target name")
	}
	if envValue(env, "SSL_CERT_FILE") != CABundle {
		t.Fatal("SSL_CERT_FILE not set to fev's bundle")
	}
}

func TestBuildEnvDefaultsPathWhenTargetHasNone(t *testing.T) {
	env := BuildEnv([]string{"HOSTNAME=x"}, "", "scratch")
	path := envValue(env, "PATH")
	want := "/nix/var/fev/profile/bin:/nix/var/fev/bin:" + defaultTargetPath
	if path != want {
		t.Fatalf("PATH %q, want %q", path, want)
	}
	if envValue(env, "TERM") != "xterm-256color" {
		t.Fatal("TERM default is not xterm-256color")
	}
}

func TestSpecRoundTrip(t *testing.T) {
	s := Spec{
		Flags:   SpecFlagJoinUser,
		UID:     0,
		GID:     0,
		CapKeep: RunningCaps(DockerDefaultCaps),
		Dir:     "/usr/share/nginx/html",
		Argv:    []string{"/nix/var/fev/profile/bin/zsh", "-i"},
		Env:     []string{"PATH=/x", "HOME=/nix/var/fev/home"},
		Seccomp: [][]Insn{
			WithPrefix(BuildPrefix(ArchARM64, true), []Insn{{Code: insnRETK, K: retAllow}}),
			WithPrefix(BuildPrefix(ArchARM64, true), []Insn{{Code: insnLDWABS, K: dataNR}, {Code: insnRETK, K: 0}}),
		},
	}
	b, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("round trip differs:\n got %+v\nwant %+v", got, s)
	}
}

func TestSpecRejectsNoProgram(t *testing.T) {
	if _, err := (Spec{}).Encode(); err == nil {
		t.Fatal("a spec with no program was encoded")
	}
}

func TestDecodeRejectsBadMagic(t *testing.T) {
	if _, err := Decode([]byte("XXXX")); err == nil {
		t.Fatal("bad magic accepted")
	}
}

func TestGraphFromMount(t *testing.T) {
	gd, err := GraphFromMount("overlay", "", []string{
		"index=off",
		"workdir=/var/lib/docker/.../work",
		"upperdir=/var/lib/docker/.../fs",
		"lowerdir=/var/lib/docker/.../L1:/var/lib/docker/.../L2",
	})
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}
	if got := ContainerUpper(gd); got != "/var/lib/docker/.../fs" {
		t.Fatalf("upper = %q", got)
	}
	lowers, err := OverlayLowers(gd)
	if err != nil {
		t.Fatalf("lowers: %v", err)
	}
	want := []string{"/var/lib/docker/.../fs", "/var/lib/docker/.../L1", "/var/lib/docker/.../L2"}
	if len(lowers) != len(want) {
		t.Fatalf("lowers = %v", lowers)
	}
	for i := range want {
		if lowers[i] != want[i] {
			t.Fatalf("lowers[%d] = %q, want %q", i, lowers[i], want[i])
		}
	}

	gd, err = GraphFromMount("bind", "/var/lib/docker/.../onlylayer", []string{"rbind", "ro"})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if ContainerUpper(gd) != "" {
		t.Fatalf("a bind mount has no upper: %q", ContainerUpper(gd))
	}
	lowers, _ = OverlayLowers(gd)
	if len(lowers) != 1 || lowers[0] != "/var/lib/docker/.../onlylayer" {
		t.Fatalf("bind lowers = %v", lowers)
	}

	if _, err := GraphFromMount("overlay", "", []string{"index=off"}); err == nil {
		t.Fatal("an overlay mount with no layers should be refused")
	}
	if _, err := GraphFromMount("fuse", "x", nil); err == nil {
		t.Fatal("an unsupported mount type should be refused")
	}
}
