package images

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/ulikunitz/xz"

	"fev/proto/agent"
)

type signer struct {
	entity *openpgp.Entity
	ring   openpgp.EntityList
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	made := func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	e, err := openpgp.NewEntity("fev test", "", "test@fev.invalid", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Time: made})
	if err != nil {
		t.Fatal(err)
	}
	return &signer{entity: e, ring: openpgp.EntityList{e}}
}

func (s *signer) sign(t *testing.T, data []byte, at time.Time) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&buf, s.entity, bytes.NewReader(data), &packet.Config{Time: func() time.Time { return at }}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type server struct {
	mu      sync.Mutex
	files   map[string][]byte
	gets    map[string]int
	cuts    map[string]int
	noRange bool
	ranges  []string
	srv     *httptest.Server
}

func newServer(t *testing.T) *server {
	s := &server{files: map[string][]byte{}, gets: map[string]int{}, cuts: map[string]int{}}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		data, ok := s.files[r.URL.Path]
		s.gets[r.URL.Path]++
		cut := s.cuts[r.URL.Path] > 0
		if cut {
			s.cuts[r.URL.Path]--
		}
		noRange := s.noRange
		if rg := r.Header.Get("Range"); rg != "" {
			s.ranges = append(s.ranges, rg)
		}
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		status := http.StatusOK
		if off, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), "-")); err == nil && !noRange {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, len(data)-1, len(data)))
			data, status = data[off:], http.StatusPartialContent
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.WriteHeader(status)
		if cut {
			w.Write(data[:len(data)/3])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		w.Write(data)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *server) put(path string, data []byte) {
	s.mu.Lock()
	s.files[path] = data
	s.mu.Unlock()
}

func (s *server) got(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets[path]
}

var now = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func (s *server) source(t *testing.T, key *signer, clock time.Time) *Source {
	t.Helper()
	src := &Source{Server: s.srv.URL, Client: s.srv.Client(), TempDir: t.TempDir()}
	src.Now = func() time.Time { return clock }
	src.v = newVerifier(key.ring, src.now)
	return src
}

type build struct {
	ref       agent.ImageRef
	rootfs    []byte
	incus     []byte
	sums      []byte
	sumsAsc   []byte
	tarSHA256 string
}

func xzBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

func incusTar(t *testing.T, ref agent.ImageRef, extra func(tw *tar.Writer)) []byte {
	t.Helper()
	meta := fmt.Sprintf(`architecture: %s
properties:
  architecture: %s
  os: %s
  release: %q
  serial: %q
  variant: %s
templates:
  /etc/hostname:
    when:
    - create
    - copy
    create_only: false
    template: hostname.tpl
    properties: {}
  /var/lib/cloud/seed/nocloud-net/user-data:
    when:
    - create
    - copy
    create_only: false
    template: cloud-init-user-data.tpl
    properties:
      default: |-
        #cloud-config
        {}
`, KernelArch(ref.Arch), KernelArch(ref.Arch), ref.Distro, ref.Release, ref.Serial, ref.Variant)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name, body string) {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	add("metadata.yaml", meta)
	add("templates/hostname.tpl", "{{ container.name }}\n")
	add("templates/cloud-init-user-data.tpl", `{%- if config_get("cloud-init.user-data", properties.default) == properties.default -%}
{{ config_get("user.user-data", properties.default) }}
{%- else -%}
{{- config_get("cloud-init.user-data", "") }}
{%- endif %}
`)
	if extra != nil {
		extra(tw)
	}
	tw.Close()
	return xzBytes(t, buf.Bytes())
}

func makeBuild(t *testing.T, key *signer, ref agent.ImageRef, rootfs []byte, signed time.Time) *build {
	t.Helper()
	b := &build{ref: ref, rootfs: rootfs, incus: incusTar(t, ref, nil)}
	r, i := sha256.Sum256(b.rootfs), sha256.Sum256(b.incus)
	b.sums = fmt.Appendf(nil, "%x  rootfs.tar.xz\n%x  rootfs.squashfs\n%x  incus.tar.xz\n", r, sha256.Sum256([]byte("squashfs")), i)
	b.sumsAsc = key.sign(t, b.sums, signed)
	return b
}

func (b *build) path() string {
	return "/images/" + strings.Join([]string{b.ref.Distro, b.ref.Release, b.ref.Arch, b.ref.Variant, b.ref.Serial}, "/") + "/"
}

func (b *build) entry() Entry { return Entry{ImageRef: b.ref, Path: b.path()} }

func (s *server) publish(b *build) {
	s.put(b.path()+"SHA256SUMS", b.sums)
	s.put(b.path()+"SHA256SUMS.asc", b.sumsAsc)
	s.put(b.path()+"rootfs.tar.xz", b.rootfs)
	s.put(b.path()+"incus.tar.xz", b.incus)
}

func (s *server) index(t *testing.T, key *signer, builds ...*build) {
	t.Helper()
	var buf bytes.Buffer
	for _, b := range builds {
		fmt.Fprintf(&buf, "%s;%s;%s;%s;%s;%s\n", b.ref.Distro, b.ref.Release, b.ref.Arch, b.ref.Variant, b.ref.Serial, b.path())
	}
	s.put(indexPath, buf.Bytes())
	s.put(indexPath+".asc", key.sign(t, buf.Bytes(), now.Add(-time.Hour)))
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func ref(distro, release, variant, serial string) agent.ImageRef {
	return agent.ImageRef{Distro: distro, Release: release, Arch: Arch, Variant: variant, Serial: serial}
}

func sum(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }

func flip(data []byte, off int) []byte {
	out := bytes.Clone(data)
	out[off] ^= 0x01
	return out
}

func tempFiles(t *testing.T, s *Source) []string {
	entries, err := os.ReadDir(s.TempDir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestThePinnedKeyLoadsAndIsLXCs(t *testing.T) {
	kr, err := Keyring()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.ToUpper(hex.EncodeToString(kr[0].PrimaryKey.Fingerprint)); got != "E7FB0CAEC8173D669066514CBAEFF88C22F6E216" {
		t.Fatalf("fingerprint %s", got)
	}
	if len(kr[0].Subkeys) != 0 {
		t.Errorf("the key has %d subkeys; the server signs with the primary", len(kr[0].Subkeys))
	}
	other := newSigner(t)
	var buf bytes.Buffer
	w, _ := armorPublic(&buf)
	other.entity.Serialize(w)
	w.Close()
	if _, err := keyring(buf.Bytes(), Fingerprint); err == nil || !strings.Contains(err.Error(), "not "+Fingerprint) {
		t.Errorf("another key was taken for the pinned one: %v", err)
	}
}

func TestTheServersOwnChainVerifiesWithThePinnedKeyAndAFlippedByteAtEachLinkFails(t *testing.T) {
	kr, err := Keyring()
	if err != nil {
		t.Fatal(err)
	}
	v := newVerifier(kr, func() time.Time { return now })
	index, indexAsc := fixture(t, "server/index-system"), fixture(t, "server/index-system.asc")
	if _, err := v.check(index, indexAsc); err != nil {
		t.Fatalf("index-system: %v", err)
	}
	if _, err := v.check(flip(index, len(index)/2), indexAsc); err == nil {
		t.Error("index-system with a byte flipped verified")
	}
	entries, err := ParseIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	dirs, _ := filepath.Glob("testdata/server/*_*")
	if len(dirs) != 5 {
		t.Fatalf("%d builds in testdata", len(dirs))
	}
	for _, dir := range dirs {
		sums, asc, incus := fixture(t, dir[9:]+"/SHA256SUMS"), fixture(t, dir[9:]+"/SHA256SUMS.asc"), fixture(t, dir[9:]+"/incus.tar.xz")
		made, err := v.check(sums, asc)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if err := v.fresh(made); err != nil {
			t.Errorf("%s: signed %v: %v", dir, made, err)
		}
		digests, err := ParseSums(sums)
		if err != nil {
			t.Fatal(err)
		}
		if got := sha256.Sum256(incus); !bytes.Equal(got[:], digests[IncusFile]) || digests[RootfsFile] == nil {
			t.Fatalf("%s: incus.tar.xz is not what SHA256SUMS says", dir)
		}
		parts := strings.SplitN(filepath.Base(dir), "_", 5)
		e, ok := Newest(entries, parts[0], parts[1], parts[2], parts[3])
		if !ok || strings.ReplaceAll(e.Serial, ":", "_") != parts[4] {
			t.Fatalf("%s: the index has %+v", dir, e)
		}
		if _, err := readTemplates(incus, e.ImageRef); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
		for _, off := range []int{0, len(sums) / 2, len(sums) - 2} {
			if _, err := v.check(flip(sums, off), asc); err == nil {
				t.Errorf("%s: SHA256SUMS flipped at %d verified", dir, off)
			}
		}
		body := bytes.Index(asc, []byte("\n\n")) + 10
		if _, err := v.check(sums, flip(asc, body)); err == nil {
			t.Errorf("%s: SHA256SUMS.asc flipped verified", dir)
		}
		for _, off := range []int{0, len(incus) / 2, len(incus) - 1} {
			if got := sha256.Sum256(flip(incus, off)); bytes.Equal(got[:], digests[IncusFile]) {
				t.Errorf("%s: incus.tar.xz flipped at %d matches", dir, off)
			}
		}
		late := newVerifier(kr, func() time.Time { return made.Add(15 * 24 * time.Hour) })
		if _, err := late.check(sums, asc); err != nil {
			t.Fatal(err)
		}
		if err := late.fresh(made); !errors.Is(err, ErrStale) {
			t.Errorf("%s: a signature 15 days old: %v", dir, err)
		}
	}
}

func TestTheBuildsTemplatesAreReadAsMachineCreateCarriesThem(t *testing.T) {
	entries, err := ParseIndex(fixture(t, "server/index-system"))
	if err != nil {
		t.Fatal(err)
	}
	dirs, _ := filepath.Glob("testdata/server/*_*")
	for _, dir := range dirs {
		parts := strings.SplitN(filepath.Base(dir), "_", 5)
		e, _ := Newest(entries, parts[0], parts[1], parts[2], parts[3])
		got, err := readTemplates(fixture(t, dir[9:]+"/incus.tar.xz"), e.ImageRef)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.MarshalIndent(got, "", "  ")
		data = append(data, '\n')
		golden := filepath.Join(dir, "templates.golden.json")
		if os.Getenv("UPDATE_GOLDEN") != "" {
			os.WriteFile(golden, data, 0o644)
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, want) {
			t.Errorf("%s: templates differ from %s:\n%s", dir, golden, data)
		}
	}
	alpine, _ := os.ReadFile("testdata/server/alpine_3.24_arm64_default_20261013_13_01/templates.golden.json")
	if !bytes.Contains(alpine, []byte(`"path": "/etc/inittab"`)) || bytes.Contains(alpine, []byte("tty1")) {
		t.Errorf("Alpine's inittab template is not the console-only one:\n%s", alpine)
	}
	fedora, _ := os.ReadFile("testdata/server/fedora_44_arm64_cloud_20261013_20_33/templates.golden.json")
	if !bytes.Contains(fedora, []byte(`"/var/lib/cloud/seed/nocloud-net/network-config"`)) || !bytes.Contains(fedora, []byte(`"default": "#cloud-config\n{}"`)) {
		t.Errorf("Fedora cloud's seeds are not there:\n%s", fedora)
	}
}

func TestABuildIsFetchedCheckedAndDecompressedToItsTar(t *testing.T) {
	key := newSigner(t)
	srv := newServer(t)
	b := makeBuild(t, key, ref("ubuntu", "resolute", "default", "20261013_07:42"), fixture(t, "rootfs-blocks.tar.xz"), now.Add(-20*time.Hour))
	srv.publish(b)
	src := srv.source(t, key, now)
	var reports []string
	got, err := src.Fetch(context.Background(), b.entry(), "", func(file string, done, total int64) {
		reports = append(reports, fmt.Sprintf("%s %d/%d", file, done, total))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	if got.ImageRef != b.ref || got.RootfsBytes != int64(len(b.rootfs)) || len(got.Templates) != 2 || got.Templates[0].Path != "/etc/hostname" {
		t.Fatalf("build: %+v", got)
	}
	if want := fmt.Sprintf("rootfs.tar.xz %d/%d", len(b.rootfs), len(b.rootfs)); len(reports) == 0 || reports[len(reports)-1] != want {
		t.Errorf("progress: %q, want it to end %q", reports, want)
	}
	if left := tempFiles(t, src); len(left) != 0 {
		t.Errorf("files named in the temporary directory: %q", left)
	}
	var out bytes.Buffer
	n, parallel, err := got.WriteTar(context.Background(), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !parallel || n != int64(out.Len()) || !bytes.Equal(out.Bytes(), rootfsTar()) {
		t.Fatalf("decompressed %d bytes (parallel %v), sha256 %s; want the fixture's tar, %d bytes", n, parallel, sum(out.Bytes()), len(rootfsTar()))
	}
	got.Close()
	if _, _, err := got.WriteTar(context.Background(), io.Discard); err == nil {
		t.Error("a closed build still decompresses")
	}
}

func TestADownloadCutBeforeItsEndResumesWhereItStopped(t *testing.T) {
	key := newSigner(t)
	srv := newServer(t)
	b := makeBuild(t, key, ref("ubuntu", "resolute", "default", "20261013_07:42"), fixture(t, "rootfs-blocks.tar.xz"), now.Add(-20*time.Hour))
	srv.publish(b)
	rootfs := b.path() + "rootfs.tar.xz"
	srv.cuts[rootfs] = 2
	src := srv.source(t, key, now)
	got, err := src.Fetch(context.Background(), b.entry(), "", nil)
	if err != nil {
		t.Fatalf("cut twice: %v", err)
	}
	var out bytes.Buffer
	if _, _, err := got.WriteTar(context.Background(), &out); err != nil || !bytes.Equal(out.Bytes(), rootfsTar()) {
		t.Fatalf("the resumed build's tar: %v", err)
	}
	got.Close()
	if srv.got(rootfs) != 3 || len(srv.ranges) != 2 {
		t.Errorf("rootfs fetched %d times, ranges %q", srv.got(rootfs), srv.ranges)
	}

	srv.cuts[rootfs] = resumes + 1
	if _, err := src.Fetch(context.Background(), b.entry(), "", nil); err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("cut %d times: %v", resumes+1, err)
	}
	srv.cuts[rootfs], srv.noRange = 1, true
	if _, err := src.Fetch(context.Background(), b.entry(), "", nil); err == nil || !strings.Contains(err.Error(), "range") {
		t.Errorf("no range: %v", err)
	}
	if left := tempFiles(t, src); len(left) != 0 {
		t.Errorf("files left: %q", left)
	}
}

func TestAFlippedByteAtEachLinkIsRefusedBeforeAnythingIsDecompressed(t *testing.T) {
	key := newSigner(t)
	r := ref("fedora", "44", "default", "20261013_20:33")
	good := makeBuild(t, key, r, fixture(t, "rootfs-blocks.tar.xz"), now.Add(-20*time.Hour))
	for _, c := range []struct {
		name  string
		build func() *build
		want  string
	}{
		{"rootfs flipped at its start", func() *build { b := *good; b.rootfs = flip(b.rootfs, 0); return &b }, "rootfs.tar.xz: does not match SHA256SUMS"},
		{"rootfs flipped in its middle", func() *build { b := *good; b.rootfs = flip(b.rootfs, len(b.rootfs)/2); return &b }, "rootfs.tar.xz: does not match SHA256SUMS"},
		{"rootfs flipped at its end", func() *build { b := *good; b.rootfs = flip(b.rootfs, len(b.rootfs)-1); return &b }, "rootfs.tar.xz: does not match SHA256SUMS"},
		{"rootfs cut short", func() *build { b := *good; b.rootfs = b.rootfs[:len(b.rootfs)-100]; return &b }, "rootfs.tar.xz: does not match SHA256SUMS"},
		{"incus.tar.xz flipped", func() *build { b := *good; b.incus = flip(b.incus, len(b.incus)/2); return &b }, "incus.tar.xz does not match SHA256SUMS"},
		{"SHA256SUMS flipped", func() *build { b := *good; b.sums = flip(b.sums, 10); return &b }, "does not verify with the pinned key"},
		{"SHA256SUMS.asc flipped", func() *build {
			b := *good
			b.sumsAsc = flip(b.sumsAsc, bytes.Index(b.sumsAsc, []byte("\n\n"))+8)
			return &b
		}, "SHA256SUMS.asc"},
		{"signed by another key", func() *build { b := *good; b.sumsAsc = newSigner(t).sign(t, b.sums, now.Add(-time.Hour)); return &b }, "does not verify with the pinned key"},
		{"signed 15 days ago", func() *build { b := *good; b.sumsAsc = key.sign(t, b.sums, now.Add(-15*24*time.Hour)); return &b }, "signed too long ago"},
		{"no incus.tar.xz listed", func() *build {
			b := *good
			b.sums = fmt.Appendf(nil, "%x  rootfs.tar.xz\n", sha256.Sum256(b.rootfs))
			b.sumsAsc = key.sign(t, b.sums, now.Add(-time.Hour))
			return &b
		}, "does not list incus.tar.xz"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := newServer(t)
			b := c.build()
			srv.publish(b)
			src := srv.source(t, key, now)
			got, err := src.Fetch(context.Background(), b.entry(), "", nil)
			if err == nil {
				got.Close()
				t.Fatal("fetched")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q, want %q", err, c.want)
			}
			if left := tempFiles(t, src); len(left) != 0 {
				t.Errorf("left behind: %q", left)
			}
		})
	}
	srv := newServer(t)
	b := *good
	b.sumsAsc = key.sign(t, b.sums, now.Add(-13*24*time.Hour))
	srv.publish(&b)
	got, err := srv.source(t, key, now).Fetch(context.Background(), b.entry(), "", nil)
	if err != nil {
		t.Fatalf("a signature 13 days old: %v", err)
	}
	got.Close()
}

func TestABuildServedUnderAnotherSerialIsRefused(t *testing.T) {
	key := newSigner(t)
	old := makeBuild(t, key, ref("debian", "bookworm", "default", "20261008_05:24"), fixture(t, "rootfs-blocks.tar.xz"), now.Add(-6*24*time.Hour))
	newer := *old
	newer.ref.Serial = "20261014_05:24"
	srv := newServer(t)
	srv.publish(&newer)
	_, err := srv.source(t, key, now).Fetch(context.Background(), newer.entry(), "", nil)
	if err == nil || !strings.Contains(err.Error(), `incus.tar.xz describes the build "bookworm" "default" "20261008_05:24"`) {
		t.Fatalf("another serial's files under this one: %v", err)
	}
}

func TestABuildOfAnotherArchitectureServedUnderThisOnesPathIsRefused(t *testing.T) {
	key := newSigner(t)
	arm := ref("debian", "trixie", "default", "20261014_05:24")
	amd := arm
	amd.Arch = AMD64
	b := makeBuild(t, key, amd, fixture(t, "rootfs-blocks.tar.xz"), now.Add(-time.Hour))
	served := *b
	served.ref = arm
	srv := newServer(t)
	srv.publish(&served)
	_, err := srv.source(t, key, now).Fetch(context.Background(), served.entry(), "", nil)
	if err == nil || !strings.Contains(err.Error(), `describes a build for "x86_64"`) {
		t.Fatalf("an amd64 build at the arm64 build's path: %v", err)
	}
	if n := srv.got(served.path() + "rootfs.tar.xz"); n != 0 {
		t.Errorf("its rootfs was downloaded %d times", n)
	}
	srv.publish(b)
	got, err := srv.source(t, key, now).Fetch(context.Background(), b.entry(), "", nil)
	if err != nil || got.ImageRef != amd {
		t.Fatalf("the amd64 build at its own path: %+v %v", got, err)
	}
	got.Close()
	debianMeta := func(top, prop string) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		body := "architecture: " + top + "\nproperties:\n  architecture: " + prop + "\n  serial: \"20261014_05:24\"\n  variant: default\n  release: trixie\ntemplates: {}\n"
		tw.WriteHeader(&tar.Header{Name: "metadata.yaml", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		tw.Write([]byte(body))
		tw.Close()
		return xzBytes(t, buf.Bytes())
	}
	for _, c := range []struct {
		top, prop string
		ref       agent.ImageRef
		ok        bool
	}{
		{"arm64", "arm64", arm, true},
		{"aarch64", "aarch64", arm, true},
		{"amd64", "amd64", amd, true},
		{"x86_64", "x86_64", amd, true},
		{"amd64", "amd64", arm, false},
		{"arm64", "x86_64", arm, false},
		{"", "", arm, false},
		{"riscv64", "riscv64", arm, false},
	} {
		if _, err := readTemplates(debianMeta(c.top, c.prop), c.ref); (err == nil) != c.ok {
			t.Errorf("metadata for %q (%q) read as %s: %v", c.top, c.prop, c.ref.Arch, err)
		}
	}
}

func TestAnOlderSerialThanTheGuestKeepsIsRefused(t *testing.T) {
	key := newSigner(t)
	srv := newServer(t)
	b := makeBuild(t, key, ref("alpine", "3.24", "default", "20261012_13:01"), fixture(t, "rootfs-stream.tar.xz"), now.Add(-2*24*time.Hour))
	srv.publish(b)
	srv.index(t, key, b)
	src := srv.source(t, key, now)
	_, err := src.Fetch(context.Background(), b.entry(), "20261013_13:01", nil)
	if !errors.Is(err, ErrOlder) {
		t.Fatalf("an older serial: %v", err)
	}
	if n := srv.got(b.path() + "rootfs.tar.xz"); n != 0 {
		t.Errorf("the older rootfs was downloaded %d times", n)
	}
	kept := []agent.ImageInfo{{ImageRef: ref("alpine", "3.24", "default", "20261013_13:01")}}
	p, err := src.Prepare(context.Background(), kept, "alpine", "", "", agent.VariantDefault, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Kept || p.Build != nil || p.Image.Serial != "20261013_13:01" || p.Offline != nil || len(p.Image.Templates) != 0 {
		t.Fatalf("with the guest's build newer than the list's: %+v", p)
	}
	kept[0].Serial = b.ref.Serial
	if p, err := src.Prepare(context.Background(), kept, "alpine", "3.24", "", agent.VariantDefault, nil); err != nil || !p.Kept {
		t.Fatalf("the same serial: %+v %v", p, err)
	}
	kept[0].Serial = "20261011_13:01"
	p, err = src.Prepare(context.Background(), kept, "alpine", "3.24", "", agent.VariantDefault, nil)
	if err != nil || p.Kept || p.Build == nil || p.Image.Serial != b.ref.Serial || len(p.Image.Templates) != 2 {
		t.Fatalf("the guest's build older than the list's: %+v %v", p, err)
	}
	p.Close()
	srv.put(indexPath+".asc", []byte("garbage"))
	p, err = src.Prepare(context.Background(), kept, "alpine", "3.24", "", agent.VariantDefault, nil)
	if err != nil || !p.Kept || p.Offline == nil {
		t.Fatalf("with no list: %+v %v", p, err)
	}
	if _, err := src.Prepare(context.Background(), nil, "alpine", "3.24", "", agent.VariantDefault, nil); err == nil {
		t.Fatal("with no list and nothing kept")
	}
}

func TestASignatureWithAKeyIDButNoIssuerFingerprintIsRefused(t *testing.T) {
	key := newSigner(t)
	v := newVerifier(key.ring, func() time.Time { return now })
	id := key.entity.PrimaryKey.KeyId
	if err := v.issuer(&packet.Signature{IssuerKeyId: &id}); err == nil || !strings.Contains(err.Error(), "no issuer fingerprint") {
		t.Errorf("a key ID alone: %v", err)
	}
	if err := v.issuer(&packet.Signature{IssuerKeyId: &id, IssuerFingerprint: newSigner(t).entity.PrimaryKey.Fingerprint}); err == nil {
		t.Error("another key's fingerprint")
	}
	if err := v.issuer(&packet.Signature{IssuerKeyId: &id, IssuerFingerprint: key.entity.PrimaryKey.Fingerprint}); err != nil {
		t.Error(err)
	}
	data := []byte("x")
	a, b := key.sign(t, data, now), key.sign(t, data, now)
	if _, err := v.check(data, a); err != nil {
		t.Fatal(err)
	}
	var both bytes.Buffer
	for _, asc := range [][]byte{a, b} {
		block, err := armor.Decode(bytes.NewReader(asc))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(&both, block.Body)
	}
	var two bytes.Buffer
	w, _ := armor.Encode(&two, openpgp.SignatureType, nil)
	w.Write(both.Bytes())
	w.Close()
	if _, err := v.check(data, two.Bytes()); err == nil || !strings.Contains(err.Error(), "2 packets") {
		t.Errorf("two signatures in one armor: %v", err)
	}
}

func TestTheXZBlocksDecodeInParallelToWhatTheStreamReaderGives(t *testing.T) {
	want := rootfsTar()
	ctx := context.Background()
	for _, c := range []struct {
		name     string
		data     []byte
		parallel bool
	}{
		{"blocks with sizes (xz -T2)", fixture(t, "rootfs-blocks.tar.xz"), true},
		{"one block with no sizes (xz -T1)", fixture(t, "rootfs-stream.tar.xz"), false},
		{"ulikunitz's writer", xzBytes(t, want), false},
	} {
		var out bytes.Buffer
		n, parallel, err := decompressXZ(ctx, bytes.NewReader(c.data), int64(len(c.data)), &out)
		if err != nil || parallel != c.parallel || n != int64(len(want)) || !bytes.Equal(out.Bytes(), want) {
			t.Errorf("%s: %d bytes, parallel %v, err %v", c.name, n, parallel, err)
		}
	}
	blocks := fixture(t, "rootfs-blocks.tar.xz")
	bs, ok := scanBlocks(bytes.NewReader(blocks), int64(len(blocks)))
	if !ok || len(bs) != 14 {
		t.Fatalf("%d blocks", len(bs))
	}
	for _, workers := range []int{1, len(bs)} {
		var out bytes.Buffer
		if _, err := parallelXZ(ctx, bytes.NewReader(blocks), bs, &out, workers); err != nil || !bytes.Equal(out.Bytes(), want) {
			t.Errorf("%d workers: %v", workers, err)
		}
	}
	bad := flip(blocks, int(bs[7].data+bs[7].size/2))
	var out bytes.Buffer
	if _, _, err := decompressXZ(ctx, bytes.NewReader(bad), int64(len(bad)), &out); err == nil && bytes.Equal(out.Bytes(), want) {
		t.Error("a damaged block decoded to the right bytes")
	}
	padded := append(bytes.Clone(blocks), 0, 0, 0, 0)
	out.Reset()
	if _, parallel, err := decompressXZ(ctx, bytes.NewReader(padded), int64(len(padded)), &out); err != nil || parallel || !bytes.Equal(out.Bytes(), want) {
		t.Errorf("padded: parallel %v, %v", parallel, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := decompressXZ(cctx, bytes.NewReader(blocks), int64(len(blocks)), io.Discard); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestTemplatesThatIncusRefusesAreRefused(t *testing.T) {
	r := ref("alpine", "3.24", "default", "20261013_13:01")
	if got, err := readTemplates(incusTar(t, r, nil), r); err != nil || len(got) != 2 || got[1].Properties["default"] != "#cloud-config\n{}" {
		t.Fatalf("a good one: %+v %v", got, err)
	}
	other := r
	other.Serial = "20261012_13:01"
	if _, err := readTemplates(incusTar(t, other, nil), r); err == nil {
		t.Error("another serial's metadata")
	}
	withMeta := func(meta string, extra func(tw *tar.Writer)) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		body := "architecture: aarch64\nproperties:\n  serial: \"20261013_13:01\"\n  variant: default\n  release: \"3.24\"\ntemplates:\n" + meta
		tw.WriteHeader(&tar.Header{Name: "metadata.yaml", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		tw.Write([]byte(body))
		tw.WriteHeader(&tar.Header{Name: "templates/a.tpl", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
		tw.Write([]byte("a"))
		if extra != nil {
			extra(tw)
		}
		tw.Close()
		return xzBytes(t, buf.Bytes())
	}
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"a slash in the name", withMeta("  /etc/a:\n    when: [create]\n    template: ../a.tpl\n", nil)},
		{"a link", withMeta("  /etc/a:\n    when: [create]\n    template: l.tpl\n", func(tw *tar.Writer) {
			tw.WriteHeader(&tar.Header{Name: "templates/l.tpl", Typeflag: tar.TypeSymlink, Linkname: "/etc/shadow"})
		})},
		{"a mode", withMeta("  /etc/a:\n    when: [create]\n    template: a.tpl\n    mode: \"0600\"\n", nil)},
		{"a relative target", withMeta("  etc/a:\n    when: [create]\n    template: a.tpl\n", nil)},
		{"a target that climbs", withMeta("  /etc/../a:\n    when: [create]\n    template: a.tpl\n", nil)},
		{"a missing template", withMeta("  /etc/a:\n    when: [create]\n    template: b.tpl\n", nil)},
	} {
		if _, err := readTemplates(c.data, r); err == nil {
			t.Errorf("%s: taken", c.name)
		}
	}
	if got, err := readTemplates(withMeta("  /etc/a:\n    when: [create, copy]\n    template: a.tpl\n", nil), r); err != nil || len(got) != 1 || got[0].Template != "a" {
		t.Errorf("a plain one: %+v %v", got, err)
	}
	kali := ref("kali", "current", "default", "20261013_17:14")
	rolling := kali
	rolling.Release = "kali-rolling"
	if _, err := readTemplates(incusTar(t, rolling, nil), kali); err != nil {
		t.Errorf("Kali's own name for current: %v", err)
	}
	other.Serial, other.Release = r.Serial, "3.23"
	if _, err := readTemplates(incusTar(t, other, nil), r); err == nil {
		t.Error("another release's metadata")
	}
}

func TestTheIndexListsBuildsAndTheDefaultsAreNamedForTheServer(t *testing.T) {
	if _, err := ParseIndex([]byte("ubuntu;noble;arm64;default;20261013_07:42;/images/ubuntu/jammy/arm64/default/20261013_07:42/\n")); err == nil {
		t.Error("a path that is not its fields' directory")
	}
	if _, err := ParseIndex([]byte("ubuntu;..;arm64;default;x;/images/ubuntu/../arm64/default/x/\n")); err == nil {
		t.Error("a release of ..")
	}
	entries, err := ParseIndex(fixture(t, "server/index-system"))
	if err != nil {
		t.Fatal(err)
	}
	noCloud := map[string]bool{"archlinux": true, "voidlinux": true, "devuan": true, "gentoo": true}
	for distro, release := range Defaults {
		for _, v := range append([]string{BaseVariant(distro)}, Variants[distro]...) {
			if _, ok := Newest(entries, distro, release, Arch, v); !ok {
				t.Errorf("the default %s %s %s is not on the server", distro, release, v)
			}
		}
		if _, ok := Newest(entries, distro, release, Arch, agent.VariantCloud); ok == noCloud[distro] {
			t.Errorf("the default %s %s: a cloud variant %v, and fev says %v", distro, release, ok, !noCloud[distro])
		}
	}
	for _, c := range []struct{ distro, release, wantD, wantR string }{
		{"ubuntu", "", "ubuntu", "resolute"},
		{"Ubuntu", "24.04", "ubuntu", "noble"},
		{"debian", "13", "debian", "trixie"},
		{"debian", "", "debian", "trixie"},
		{"arch", "", "archlinux", "current"},
		{"fedora", "43", "fedora", "43"},
		{"alpine", "edge", "alpine", "edge"},
	} {
		d, r, err := Name(c.distro, c.release)
		if err != nil || d != c.wantD || r != c.wantR {
			t.Errorf("Name(%q, %q) = %q %q %v", c.distro, c.release, d, r, err)
		}
	}
	if _, _, err := Name("openeuler", ""); err == nil {
		t.Error("a default for a distribution fev has none for")
	}
	if err := missing(entries, "archlinux", "current", Arch, agent.VariantCloud); err == nil || !strings.Contains(err.Error(), "no archlinux current image with cloud-init") {
		t.Errorf("Arch with user data: %v", err)
	}
	if err := missing(entries, "ubuntu", "questing", Arch, agent.VariantDefault); err == nil || !strings.Contains(err.Error(), "it has jammy, noble, resolute") {
		t.Errorf("a release the server dropped: %v", err)
	}
	two, _ := ParseIndex([]byte("a;1;arm64;default;20261014_01:00;/images/a/1/arm64/default/20261014_01:00/\na;1;arm64;default;20261009_23:59;/images/a/1/arm64/default/20261009_23:59/\n"))
	if e, _ := Newest(two, "a", "1", "arm64", "default"); e.Serial != "20261014_01:00" {
		t.Errorf("newest: %s", e.Serial)
	}
}

func armorPublic(w io.Writer) (io.WriteCloser, error) {
	return armor.Encode(w, openpgp.PublicKeyType, nil)
}

func TestADistributionsReleaseAndVariantAreResolvedAsTheServerNamesThem(t *testing.T) {
	for _, c := range []struct {
		distro, release string
		userData        bool
		want            Image
	}{
		{"voidlinux", "", false, Image{"voidlinux", "current", Arch, "default"}},
		{"void", "musl", false, Image{"voidlinux", "current", Arch, "musl"}},
		{"voidlinux", "current:musl", false, Image{"voidlinux", "current", Arch, "musl"}},
		{"voidlinux", ":glibc", false, Image{"voidlinux", "current", Arch, "default"}},
		{"gentoo", "", false, Image{"gentoo", "current", Arch, "openrc"}},
		{"gentoo", "systemd", false, Image{"gentoo", "current", Arch, "systemd"}},
		{"gentoo", "current:openrc", false, Image{"gentoo", "current", Arch, "openrc"}},
		{"kali", "", false, Image{"kali", "current", Arch, "default"}},
		{"kali", "kali-rolling", true, Image{"kali", "current", Arch, "cloud"}},
		{"centos", "10", false, Image{"centos", "10-Stream", Arch, "default"}},
		{"alma", "", true, Image{"almalinux", "10", Arch, "cloud"}},
		{"rocky", "9", false, Image{"rockylinux", "9", Arch, "default"}},
		{"opensuse", "", false, Image{"opensuse", "16.0", Arch, "default"}},
		{"oracle", "", false, Image{"oracle", "10", Arch, "default"}},
		{"devuan", "", false, Image{"devuan", "excalibur", Arch, "default"}},
		{"ubuntu", "24.04", true, Image{"ubuntu", "noble", Arch, "cloud"}},
		{"arch", "", false, Image{"archlinux", "current", Arch, "default"}},
	} {
		got, err := Resolve(c.distro, c.release, "", c.userData)
		if err != nil || got != c.want {
			t.Errorf("Resolve(%q, %q, %v) = %+v %v, want %+v", c.distro, c.release, c.userData, got, err, c.want)
		}
	}
	for _, c := range []struct {
		distro, release string
		userData        bool
		why             string
	}{
		{"gentoo", "", true, "no gentoo current openrc image with cloud-init"},
		{"voidlinux", "musl", true, "no voidlinux current musl image with cloud-init"},
		{"ubuntu", "noble:cloud", false, "when user data is given"},
		{"voidlinux", "current:", false, "names no variant"},
		{"voidlinux", "current:musl:x", false, "is not RELEASE[:VARIANT]"},
		{"voidlinux", "current:../x", false, "is not a variant"},
	} {
		if _, err := Resolve(c.distro, c.release, "", c.userData); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("Resolve(%q, %q, %v): %v, want %q", c.distro, c.release, c.userData, err, c.why)
		}
	}
	entries, err := ParseIndex(fixture(t, "server/index-system"))
	if err != nil {
		t.Fatal(err)
	}
	if err := missing(entries, "voidlinux", "current", Arch, agent.VariantCloud); err == nil || !strings.Contains(err.Error(), "no voidlinux current image with cloud-init") {
		t.Errorf("Void with user data: %v", err)
	}
	if err := missing(entries, "voidlinux", "current", Arch, "uclibc"); err == nil || !strings.Contains(err.Error(), "it has default, musl") {
		t.Errorf("an unknown variant: %v", err)
	}
	musl, ok1 := Newest(entries, "voidlinux", "current", Arch, "musl")
	glibc, ok2 := Newest(entries, "voidlinux", "current", Arch, agent.VariantDefault)
	if !ok1 || !ok2 || musl.Serial != glibc.Serial || musl.Path == glibc.Path {
		t.Errorf("Void's two builds: %+v %+v", musl, glibc)
	}
}

func TestAMachinesArchitectureIsArm64OrAmd64AndAnyOtherIsRefusedByName(t *testing.T) {
	for in, want := range map[string]string{"": Arch, "arm64": Arch, "aarch64": Arch, "AMD64": AMD64, "amd64": AMD64, "x86_64": AMD64} {
		if got, err := Architecture(in); err != nil || got != want {
			t.Errorf("Architecture(%q) = %q %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"riscv64", "i386", "x86", "arm", "s390x", "amd64 ", "../arm64"} {
		if _, err := Architecture(in); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("no architecture %q", in)) {
			t.Errorf("Architecture(%q): %v", in, err)
		}
	}
	if got, err := Resolve("ubuntu", "24.04", "x86_64", true); err != nil || got != (Image{"ubuntu", "noble", AMD64, "cloud"}) {
		t.Errorf("an amd64 Ubuntu with user data: %+v %v", got, err)
	}
	if _, err := Resolve("gentoo", "", "amd64", true); err == nil || !strings.Contains(err.Error(), "no gentoo current openrc image with cloud-init for amd64") {
		t.Errorf("an amd64 Gentoo with user data: %v", err)
	}
	if _, err := Resolve("ubuntu", "", "riscv64", false); err == nil || !strings.Contains(err.Error(), `no architecture "riscv64"`) {
		t.Errorf("riscv64: %v", err)
	}
	entries, err := ParseIndex(fixture(t, "server/index-system"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"ubuntu", "debian", "fedora", "archlinux", "alpine"} {
		a, ok1 := Newest(entries, d, Defaults[d], AMD64, agent.VariantDefault)
		b, ok2 := Newest(entries, d, Defaults[d], Arch, agent.VariantDefault)
		if !ok1 || !ok2 || a.Path == b.Path || !strings.Contains(a.Path, "/amd64/") {
			t.Errorf("%s %s: amd64 %+v %v, arm64 %+v %v", d, Defaults[d], a, ok1, b, ok2)
		}
	}
	if err := missing(entries, "ubuntu", "plucky", AMD64, agent.VariantDefault); err == nil || !strings.Contains(err.Error(), "no ubuntu plucky for amd64; it has") {
		t.Errorf("a release the server has not for amd64: %v", err)
	}
}
