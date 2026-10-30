package vm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTheKernelManifestIsReadBack(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "Image")
	os.WriteFile(image+ManifestSuffix, []byte("# fev kernel manifest\nrelease 7.2.9\nversion Linux version 7.2.9 (fev@fev) #1 SMP\nsha256 6da46b9f\n"), 0o644)
	m, err := ReadKernelManifest(image)
	if err != nil || m.Release != "7.2.9" || m.Version != "Linux version 7.2.9 (fev@fev) #1 SMP" || m.SHA256 != "6da46b9f" {
		t.Errorf("read %+v, %v", m, err)
	}
	os.WriteFile(image+ManifestSuffix, []byte("version x\n"), 0o644)
	if _, err := ReadKernelManifest(image); err == nil || NoManifest(err) {
		t.Errorf("a manifest without its release: %v", err)
	}
	if _, err := ReadKernelManifest(filepath.Join(dir, "Other")); !NoManifest(err) {
		t.Errorf("no manifest: %v", err)
	}
}
