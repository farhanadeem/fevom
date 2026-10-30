package k3spayload

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func fakeWrapper(t *testing.T, version string, corrupt bool) ([]byte, string) {
	t.Helper()
	files := map[string]string{
		"bin/k3s":         "#!/inner k3s " + version + "\n",
		"bin/cni":         "cni " + version,
		"bin/containerd":  "containerd",
		"bin/aux/ipset":   "ipset",
		"etc/crictl.yaml": "runtime-endpoint: x",
	}
	links := map[string]string{"bin/host-local": "cni", "bin/flannel": "cni", "bin/crictl": "k3s", "bin/ctr": "k3s"}
	var sums, ls strings.Builder
	for _, n := range []string{"k3s", "cni", "containerd", "aux/ipset"} {
		s := sha256.Sum256([]byte(files["bin/"+n]))
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(s[:]), n)
	}
	for _, n := range []string{"host-local", "flannel", "crictl", "ctr"} {
		fmt.Fprintf(&ls, "%s %s\n", n, links["bin/"+n])
	}
	files["bin/.sha256sums"] = sums.String()
	files["bin/.links"] = ls.String()
	if corrupt {
		files["bin/containerd"] = "tampered"
	}
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	tw.WriteHeader(&tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755})
	for _, n := range []string{"bin/k3s", "bin/cni", "bin/containerd", "bin/aux/ipset", "bin/.sha256sums", "bin/.links", "etc/crictl.yaml"} {
		tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(files[n]))})
		tw.Write([]byte(files[n]))
	}
	for n, l := range links {
		tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeSymlink, Linkname: l})
	}
	tw.Close()
	var zb bytes.Buffer
	half := tb.Len() / 2
	for _, part := range [][]byte{tb.Bytes()[:half], tb.Bytes()[half:]} {
		zw, _ := zstd.NewWriter(&zb)
		zw.Write(part)
		zw.Close()
	}
	sum := sha256.Sum256(zb.Bytes())
	hash := hex.EncodeToString(sum[:])
	var bin bytes.Buffer
	bin.WriteString("\x7fELF wrapper ")
	bin.Write(bytes.Repeat([]byte("x"), 5000))
	bin.Write(zstdMagic)
	bin.WriteString(" more bytes embed/" + hash + ".tar.zst ")
	bin.Write(bytes.Repeat([]byte("y"), 3000))
	bin.Write(zb.Bytes())
	bin.WriteString("trailer")
	return bin.Bytes(), hash
}

func TestExtractLaysThePayloadOutAsK3sWould(t *testing.T) {
	dir := t.TempDir()
	bin, hash := fakeWrapper(t, "v1", false)
	w := filepath.Join(dir, "k3s")
	os.WriteFile(w, bin, 0o755)
	dest := filepath.Join(dir, "cache")
	if h, err := PayloadHash(w); err != nil || h != hash {
		t.Fatalf("PayloadHash %q %v, want %q", h, err, hash)
	}
	ex, err := Extract(w, dest, "/usr/lib/k3s")
	if err != nil {
		t.Fatal(err)
	}
	if ex.Hash != hash || ex.Files != 4 || ex.Links != 4 || ex.CNILinks != 2 {
		t.Errorf("extracted %+v", ex)
	}
	if got, _ := os.Readlink(filepath.Join(dest, "data", "current")); got != "/usr/lib/k3s/data/"+hash {
		t.Errorf("current -> %q", got)
	}
	for _, n := range []string{"cni", "host-local", "flannel"} {
		if got, _ := os.Readlink(filepath.Join(dest, "data", "cni", n)); got != "/usr/lib/k3s/data/"+hash+"/bin/cni" {
			t.Errorf("cni/%s -> %q", n, got)
		}
	}
	if got, _ := os.Readlink(filepath.Join(dest, "data", hash, "bin", "ctr")); got != "k3s" {
		t.Errorf("bin/ctr -> %q", got)
	}
	if st, err := os.Stat(filepath.Join(dest, "data", hash, "bin", "k3s")); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("bin/k3s: %v %v", st, err)
	}
	if !Verified(dest, hash) {
		t.Fatal("not verified")
	}
	mark := filepath.Join(dest, "data", hash, "bin", "k3s")
	os.WriteFile(mark, []byte("left alone"), 0o755)
	if _, err := Extract(w, dest, "/usr/lib/k3s"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(mark); string(b) != "left alone" {
		t.Errorf("a verified payload was extracted again")
	}
	bin2, hash2 := fakeWrapper(t, "v2", false)
	os.WriteFile(w, bin2, 0o755)
	if _, err := Extract(w, dest, "/usr/lib/k3s"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "data", hash)); !os.IsNotExist(err) {
		t.Errorf("the old payload is still there: %v", err)
	}
	if got, _ := os.Readlink(filepath.Join(dest, "data", "current")); got != "/usr/lib/k3s/data/"+hash2 {
		t.Errorf("current -> %q after the upgrade", got)
	}
}

func TestExtractRefusesAPayloadThatFailsItsCheck(t *testing.T) {
	dir := t.TempDir()
	bin, hash := fakeWrapper(t, "v1", true)
	w := filepath.Join(dir, "k3s")
	os.WriteFile(w, bin, 0o755)
	dest := filepath.Join(dir, "cache")
	_, err := Extract(w, dest, "/usr/lib/k3s")
	if err == nil || !strings.Contains(err.Error(), "containerd") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "data", hash)); !os.IsNotExist(err) {
		t.Errorf("left: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "data", hash+"-tmp")); !os.IsNotExist(err) {
		t.Errorf("left tmp: %v", err)
	}
}

func TestExtractWithoutAPayload(t *testing.T) {
	dir := t.TempDir()
	w := filepath.Join(dir, "k3s")
	os.WriteFile(w, []byte("no payload here"), 0o755)
	if _, err := Extract(w, filepath.Join(dir, "c"), "/x"); err == nil {
		t.Fatal("extracted nothing without an error")
	}
	os.WriteFile(w, []byte("embed/"+strings.Repeat("ab", 32)+".tar.zst and no frames"), 0o755)
	if _, err := Extract(w, filepath.Join(dir, "c"), "/x"); err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("err %v", err)
	}
}

func TestExtractWithTheHashAndALaterCheck(t *testing.T) {
	dir := t.TempDir()
	bin, hash := fakeWrapper(t, "v1", false)
	w := filepath.Join(dir, "k3s")
	os.WriteFile(w, bin, 0o755)
	dest := filepath.Join(dir, "cache")
	if _, err := ExtractWith(w, dest, "/usr/lib/k3s", Options{Hash: hash}); err != nil {
		t.Fatal(err)
	}
	if !Verified(dest, hash) {
		t.Fatal("not marked")
	}
	if f, l, err := Check(dest, hash); err != nil || f != 4 || l != 4 {
		t.Fatalf("check %d %d %v", f, l, err)
	}
	os.WriteFile(filepath.Join(dest, "data", hash, "bin", "containerd"), []byte("tampered"), 0o755)
	if _, _, err := Check(dest, hash); err == nil {
		t.Fatal("a tampered payload passed")
	}
	if Verified(dest, hash) {
		t.Error("a payload that failed its check is still marked")
	}
	if _, err := ExtractWith(w, filepath.Join(dir, "c2"), "/x", Options{Hash: strings.Repeat("0", 64)}); err == nil {
		t.Error("extracted under a hash the binary has not")
	}
}
