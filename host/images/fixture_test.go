package images

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"testing"
	"time"
)

func rootfsTar() []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	at := time.Date(2026, 10, 14, 7, 42, 0, 0, time.UTC)
	tw.WriteHeader(&tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: at, Format: tar.FormatPAX})
	tw.WriteHeader(&tar.Header{Name: "usr/bin/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: at, Format: tar.FormatPAX})
	x := uint32(2463534242)
	for i := range 100 {
		var body bytes.Buffer
		for j := range 400 {
			x ^= x << 13
			x ^= x >> 17
			x ^= x << 5
			fmt.Fprintf(&body, "line %d of file %d: value %08x %s\n", j, i, x%9973, []string{"alpha", "beta", "gamma", "delta"}[x%4])
		}
		h := &tar.Header{Name: fmt.Sprintf("usr/share/doc/f%03d.txt", i), Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(body.Len()), ModTime: at, Format: tar.FormatPAX}
		if i == 7 {
			h.Name, h.Mode = "usr/bin/ping", 0o755
			h.PAXRecords = map[string]string{"SCHILY.xattr.security.capability": "\x01\x00\x00\x02\x00\x20\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"}
		}
		tw.WriteHeader(h)
		tw.Write(body.Bytes())
	}
	tw.WriteHeader(&tar.Header{Name: "usr/bin/ping6", Typeflag: tar.TypeLink, Linkname: "usr/bin/ping", ModTime: at, Format: tar.FormatPAX})
	tw.WriteHeader(&tar.Header{Name: "etc/os-release", Typeflag: tar.TypeSymlink, Linkname: "../usr/lib/os-release", ModTime: at, Format: tar.FormatPAX})
	tw.Close()
	return buf.Bytes()
}

func TestMakeTheTarOfTheFixtures(t *testing.T) {
	path := os.Getenv("MAKE_TAR")
	if path == "" {
		t.Skip("MAKE_TAR=path writes the tar the xz fixtures were made from")
	}
	if err := os.WriteFile(path, rootfsTar(), 0o644); err != nil {
		t.Fatal(err)
	}
}
