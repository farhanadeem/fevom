package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const testRoot = "-----BEGIN CERTIFICATE-----\nMIIBfevtestroot\n-----END CERTIFICATE-----"

func image(t *testing.T, entries []string, env ...string) (bundle, rootfs string) {
	t.Helper()
	bundle = t.TempDir()
	rootfs = filepath.Join(bundle, "rootfs")
	os.MkdirAll(rootfs, 0o755)
	for _, e := range entries {
		f := strings.SplitN(e, " ", 3)
		p := filepath.Join(rootfs, f[1])
		os.MkdirAll(filepath.Dir(p), 0o755)
		switch f[0] {
		case "f":
			os.WriteFile(p, []byte(f[2]), 0o644)
		case "l":
			if err := os.Symlink(f[2], p); err != nil {
				t.Fatal(err)
			}
		case "d":
			os.MkdirAll(p, 0o755)
		case "p":
			if err := syscall.Mkfifo(p, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	spec := map[string]any{"root": map[string]any{"path": "rootfs"}, "process": map[string]any{"env": env}}
	b, _ := json.Marshal(spec)
	os.WriteFile(filepath.Join(bundle, "config.json"), b, 0o644)
	return bundle, rootfs
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return string(b)
}

func TestTheRootIsWrittenIntoTheImage(t *testing.T) {
	bundle, rootfs := image(t, []string{"f /etc/ssl/certs/ca-certificates.crt PUBLIC"})
	done, err := inject(bundle, "c1", []byte(testRoot+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(rootfs, "etc/ssl/certs/ca-certificates.crt")); got != "PUBLIC\n"+testRoot+"\n" {
		t.Errorf("bundle %q", got)
	}
	if got := read(t, filepath.Join(rootfs, "usr/local/share/ca-certificates/fev-root.crt")); got != testRoot+"\n" {
		t.Errorf("anchor %q", got)
	}
	t.Logf("did %q", done)
	done, _ = inject(bundle, "c1", []byte(testRoot))
	if got := read(t, filepath.Join(rootfs, "etc/ssl/certs/ca-certificates.crt")); strings.Count(got, "MIIBfevtestroot") != 1 || !strings.Contains(strings.Join(done, " "), "had it") {
		t.Errorf("a second create: %q, %q", got, done)
	}

	bundle, rootfs = image(t, []string{
		"f /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem FEDORA",
		"l /etc/pki/tls/certs/ca-bundle.crt /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
		"f /cacert.pem CURL",
	}, "PATH=/usr/bin", "CURL_CA_BUNDLE=/cacert.pem")
	if _, err := inject(bundle, "c2", []byte(testRoot)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(rootfs, "etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem")); strings.Count(got, "MIIBfevtestroot") != 1 {
		t.Errorf("Fedora's bundle %q", got)
	}
	if got := read(t, filepath.Join(rootfs, "etc/pki/ca-trust/source/anchors/fev-root.pem")); !strings.Contains(got, "MIIBfevtestroot") {
		t.Errorf("Fedora's anchor %q", got)
	}
	if got := read(t, filepath.Join(rootfs, "cacert.pem")); !strings.Contains(got, "MIIBfevtestroot") {
		t.Errorf("the environment's bundle %q", got)
	}
}

func TestNothingIsWrittenWhenItShouldNotBe(t *testing.T) {
	bundle, rootfs := image(t, []string{"f /app hello"})
	if done, err := inject(bundle, "c3", []byte(testRoot)); err != nil || len(done) != 0 {
		t.Errorf("scratch: %q, %v", done, err)
	}
	if _, err := os.Stat(filepath.Join(rootfs, "usr")); err == nil {
		t.Error("a directory was made in an image with no bundle")
	}
	records := t.TempDir()
	was := containersDir
	containersDir = records
	defer func() { containersDir = was }()
	id := strings.Repeat("ab", 32)
	os.MkdirAll(filepath.Join(records, id), 0o700)
	os.WriteFile(filepath.Join(records, id, "config.v2.json"), []byte(`{"Config":{"Labels":{"dev.fevom.ca":"False"}}}`), 0o600)
	bundle, rootfs = image(t, []string{"f /etc/ssl/certs/ca-certificates.crt PUBLIC"})
	if done, _ := inject(bundle, id, []byte(testRoot)); len(done) != 1 || done[0] != "opted out" {
		t.Errorf("opted out: %q", done)
	}
	if got := read(t, filepath.Join(rootfs, "etc/ssl/certs/ca-certificates.crt")); got != "PUBLIC" {
		t.Errorf("an opted-out image's bundle: %q", got)
	}
	if done, _ := inject(bundle, "c4", nil); len(done) != 0 {
		t.Errorf("no root: %q", done)
	}
}

func TestLinksInTheImageStayInsideIt(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.crt")
	os.WriteFile(victim, []byte("VICTIM"), 0o644)
	os.MkdirAll(filepath.Join(outside, "share", "ca-certificates"), 0o755)
	climb := strings.Repeat("../", 12) + strings.TrimPrefix(outside, "/")

	bundle, rootfs := image(t, []string{
		"f /etc/ssl/certs/ca-certificates.crt PUBLIC",
		"l /usr/local /",
		"l /etc/ssl/cert.pem " + filepath.Join("/", climb, "victim.crt"),
		"l /etc/ssl/ca-bundle.pem " + victim,
		"p /etc/pki/tls/certs/ca-bundle.crt",
	})
	finished := make(chan struct{})
	var done []string
	var err error
	go func() { done, err = inject(bundle, "c5", []byte(testRoot)); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("inject blocked (on the FIFO?)")
	}
	t.Logf("did %q, %v", done, err)
	if got := read(t, victim); got != "VICTIM" {
		t.Errorf("a file outside the root was written: %q", got)
	}
	if entries, _ := os.ReadDir(filepath.Join(outside, "share", "ca-certificates")); len(entries) != 0 {
		t.Errorf("an anchor went outside the root: %v", entries)
	}
	if got := read(t, filepath.Join(rootfs, "share/ca-certificates/fev-root.crt")); !strings.Contains(got, "MIIBfevtestroot") {
		t.Errorf("the anchor through /usr/local -> /: %q", got)
	}

	bundle, _ = image(t, []string{
		"f /etc/ssl/certs/ca-certificates.crt PUBLIC",
		"l /usr/local/share/ca-certificates/fev-root.crt " + victim,
	})
	if _, err := inject(bundle, "c6", []byte(testRoot)); err == nil {
		t.Error("an anchor that is a link was written through")
	}
	if got := read(t, victim); got != "VICTIM" {
		t.Errorf("written through an anchor link: %q", got)
	}
	if !bytes.Equal([]byte(read(t, victim)), []byte("VICTIM")) {
		t.Fail()
	}
}

func watchOpens(t *testing.T, p string) func() []string {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	if _, err := unix.InotifyAddWatch(fd, p, unix.IN_OPEN|unix.IN_CLOSE_WRITE|unix.IN_CLOSE_NOWRITE|unix.IN_MODIFY); err != nil {
		t.Fatal(err)
	}
	return func() []string {
		var seen []string
		buf := make([]byte, 4096)
		n, _ := unix.Read(fd, buf)
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			for _, m := range []struct {
				bit  uint32
				name string
			}{{unix.IN_OPEN, "open"}, {unix.IN_CLOSE_WRITE, "close-write"}, {unix.IN_CLOSE_NOWRITE, "close-nowrite"}, {unix.IN_MODIFY, "modify"}} {
				if ev.Mask&m.bit != 0 {
					seen = append(seen, m.name)
				}
			}
			off += unix.SizeofInotifyEvent + int(ev.Len)
		}
		return seen
	}
}

func TestADeviceNodeInTheImageIsNeverOpened(t *testing.T) {
	mknod := func(p string) {
		t.Helper()
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := unix.Mknod(p, unix.S_IFCHR|0o666, int(unix.Mkdev(1, 3))); err != nil {
			t.Skipf("mknod: %v (needs CAP_MKNOD)", err)
		}
	}
	bundle, rootfs := image(t, []string{"f /etc/ssl/cert.pem PUBLIC"})
	dev := filepath.Join(rootfs, "etc/ssl/certs/ca-certificates.crt")
	mknod(dev)
	seen := watchOpens(t, dev)
	done, err := inject(bundle, "c7", []byte(testRoot))
	if err != nil {
		t.Fatal(err)
	}
	if got := seen(); len(got) != 0 {
		t.Errorf("the device node at a bundle's path was opened: %q", got)
	}
	if !strings.Contains(read(t, filepath.Join(rootfs, "etc/ssl/cert.pem")), "MIIBfevtestroot") {
		t.Errorf("the file beside it was not written: %q", done)
	}
	bundle, rootfs = image(t, []string{"f /etc/ssl/certs/ca-certificates.crt PUBLIC"})
	dev = filepath.Join(rootfs, "usr/local/share/ca-certificates/fev-root.crt")
	mknod(dev)
	seen = watchOpens(t, dev)
	if _, err := inject(bundle, "c8", []byte(testRoot)); err == nil {
		t.Error("an anchor that is a device node was written")
	}
	if got := seen(); len(got) != 0 {
		t.Errorf("the device node at the anchor was opened: %q", got)
	}
	var st unix.Stat_t
	if unix.Stat(dev, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFCHR {
		t.Error("the anchor's device node was replaced")
	}
}

func TestTheAnchorIsMadeOrRewritten(t *testing.T) {
	bundle, rootfs := image(t, []string{"f /etc/ssl/certs/ca-certificates.crt PUBLIC", "f /usr/local/share/ca-certificates/fev-root.crt OLD ROOT, LONGER THAN THE NEW ONE ...................................................................................."})
	if _, err := inject(bundle, "c9", []byte(testRoot)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(rootfs, "usr/local/share/ca-certificates/fev-root.crt")); got != testRoot+"\n" {
		t.Errorf("the anchor rewritten: %q", got)
	}
}

func TestFedorasAnchorFollowsItsFamilyNotItsLinks(t *testing.T) {
	bundle, rootfs := image(t, []string{
		"f /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem FEDORA",
		"d /etc/pki/ca-trust/source/anchors",
		"l /etc/ssl/certs/ca-certificates.crt /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
		"l /etc/ssl/cert.pem /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	})
	done, err := inject(bundle, "c10", []byte(testRoot))
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(rootfs, "etc/pki/ca-trust/source/anchors/fev-root.pem")); !strings.Contains(got, "MIIBfevtestroot") {
		t.Errorf("no ca-trust anchor: %q (did %q)", got, done)
	}
	if _, err := os.Stat(filepath.Join(rootfs, "usr/local/share/ca-certificates")); err == nil {
		t.Errorf("Debian's anchor was made in a ca-trust image (did %q)", done)
	}
	if got := read(t, filepath.Join(rootfs, "etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem")); strings.Count(got, "MIIBfevtestroot") != 1 {
		t.Errorf("the extracted bundle: %q", got)
	}
}
