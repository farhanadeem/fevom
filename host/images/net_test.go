package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"fev/proto/agent"
)

func TestTheServersDefaultBuildsFetchCheckAndDecompress(t *testing.T) {
	if os.Getenv("IMAGES_NET") == "" {
		t.Skip("IMAGES_NET=1 fetches from the image server")
	}
	variant := agent.VariantDefault
	if v := os.Getenv("IMAGES_VARIANT"); v != "" {
		variant = v
	}
	arch, err := Architecture(os.Getenv("IMAGES_ARCH"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := NewSource("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	start := time.Now()
	entries, err := src.Index(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("index: %d builds in %v", len(entries), time.Since(start).Round(time.Millisecond))
	for _, distro := range []string{"ubuntu", "debian", "fedora", "alpine", "archlinux"} {
		e, ok := Newest(entries, distro, Defaults[distro], arch, variant)
		if !ok {
			t.Logf("%s %s: no %s %s build", distro, Defaults[distro], arch, variant)
			continue
		}
		t0 := time.Now()
		b, err := src.Fetch(ctx, e, "", nil)
		if err != nil {
			t.Errorf("%s: %v", distro, err)
			continue
		}
		fetched := time.Since(t0)
		h := sha256.New()
		t1 := time.Now()
		n, parallel, err := b.WriteTar(ctx, h)
		if err != nil {
			t.Errorf("%s: %v", distro, err)
		}
		t.Logf("%s %s %s %s %s: signed %s, rootfs %d bytes, %d templates; fetch and check %v; decompress %v (parallel %v) to %d bytes, sha256 %s",
			e.Distro, e.Release, e.Arch, e.Variant, e.Serial, b.Signed.UTC().Format(time.RFC3339), b.RootfsBytes, len(b.Templates),
			fetched.Round(time.Millisecond), time.Since(t1).Round(time.Millisecond), parallel, n, hex.EncodeToString(h.Sum(nil))[:16])
		b.Close()
	}
}
