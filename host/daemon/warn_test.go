package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	api "fev/proto/daemon"

	"fev/host/storage"
)

func TestAModulesWarningStaysInTheStatusUntilItIsWithdrawn(t *testing.T) {
	f := started(t)
	host := moduleHost{f.d}
	c := f.client()
	warnings := func() []string {
		t.Helper()
		st, err := c.Status(bg)
		if err != nil {
			t.Fatal(err)
		}
		return st.Warnings
	}
	before := f.d.hub.Status().Seq
	host.Warn("ports", "port 8080 is taken")
	host.Warn("disk", "the disk is low")
	host.Warn("ports", "port 8080 is taken")
	if got := warnings(); !reflect.DeepEqual(got, []string{"the disk is low", "port 8080 is taken"}) {
		t.Fatalf("two warnings set, one twice: %q", got)
	}
	if seq := f.d.hub.Status().Seq; seq != before+2 {
		t.Errorf("%d transitions for two warnings, one of them set twice", seq-before)
	}
	if _, err := c.VMStart(bg, api.StartParams{}); err != nil {
		t.Fatal(err)
	}
	if got := warnings(); len(got) != 2 {
		t.Fatalf("after a boot: %q", got)
	}
	if _, err := c.VMStop(bg, api.StopParams{}); err != nil {
		t.Fatal(err)
	}
	if got := warnings(); len(got) != 2 {
		t.Fatalf("after a stop: %q", got)
	}
	host.Warn("ports", "ports 8080 and 8081 are taken")
	if got := warnings(); !reflect.DeepEqual(got, []string{"the disk is low", "ports 8080 and 8081 are taken"}) {
		t.Fatalf("one replaced: %q", got)
	}
	host.Warn("disk", "")
	host.Warn("ports", "")
	host.Warn("ports", "")
	if got := warnings(); len(got) != 0 {
		t.Fatalf("both withdrawn: %q", got)
	}
}

type warned struct {
	Host
	got map[string]string
}

func (w *warned) Warn(key, text string) {
	if text == "" {
		delete(w.got, key)
	} else {
		w.got[key] = text
	}
}

func TestTheDataImagesModuleWarnsOfALowDiskAndAMissingImage(t *testing.T) {
	f := started(t)
	dir := t.TempDir()
	w := &warned{Host: moduleHost{f.d}, got: map[string]string{}}
	m := &DataImage{Path: filepath.Join(dir, "data.img")}
	if err := m.Start(w); err != nil {
		t.Fatal(err)
	}
	free := uint64(200 << 30)
	m.free = func(string) (uint64, error) { return free, nil }

	if err := m.BeforeBoot(); err != nil {
		t.Fatal(err)
	}
	if len(w.got) != 0 {
		t.Fatalf("a first boot with a roomy disk: %q", w.got)
	}
	free = LowDiskBytes - 1
	m.Trimmed()
	low := w.got[WarnDiskLow]
	if !strings.Contains(low, "GiB free") || !strings.Contains(low, dir) || !strings.Contains(low, "No space left on device") {
		t.Fatalf("a low disk: %q", low)
	}
	free = LowDiskBytes + 1
	m.Trimmed()
	if _, still := w.got[WarnDiskLow]; still {
		t.Error("the warning outlived the low disk")
	}
	free = 3 << 30
	if err := m.BeforeBoot(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.got[WarnDiskLow], "3.0 GiB free") {
		t.Errorf("a low disk at a boot: %q", w.got[WarnDiskLow])
	}
	free = 200 << 30

	if err := os.Remove(m.Path); err != nil {
		t.Fatal(err)
	}
	m.Trimmed()
	if got := w.got[WarnDataMissing]; !strings.Contains(got, "was removed while the VM") || !strings.Contains(got, m.Path) {
		t.Fatalf("the image removed under the VM: %q", got)
	}
	if err := m.BeforeBoot(); err != nil {
		t.Fatal(err)
	}
	if got := w.got[WarnDataMissing]; !strings.Contains(got, "a new, empty one was made") || !strings.Contains(got, "images, containers and volumes") {
		t.Fatalf("an image that was missing at a boot: %q", got)
	}
	delete(w.got, WarnDataMissing)
	m.forget()
	if err := os.Remove(m.Path); err != nil {
		t.Fatal(err)
	}
	if err := m.BeforeBoot(); err != nil {
		t.Fatal(err)
	}
	if got := w.got[WarnDataMissing]; got != "" {
		t.Errorf("after a reset: %q", got)
	}

	gone := &DataImage{Path: "/Volumes/fev-test-no-such-volume/fev/data.img"}
	gone.Start(w)
	err := gone.BeforeBoot()
	if err == nil || !strings.Contains(err.Error(), "/Volumes/fev-test-no-such-volume") || !strings.Contains(err.Error(), "is it mounted") {
		t.Errorf("an image on a volume that is not there: %v", err)
	}
}

func TestASwapImageThatCannotBeMadeIsAWarningNotAFailedBoot(t *testing.T) {
	f := started(t)
	dir := t.TempDir()
	w := &warned{Host: moduleHost{f.d}, got: map[string]string{}}
	m := &DataImage{Path: filepath.Join(dir, "data.img")}
	if err := m.Start(w); err != nil {
		t.Fatal(err)
	}
	m.free = func(string) (uint64, error) { return 200 << 30, nil }
	swap := storage.SwapImagePath(m.Path)
	if err := os.MkdirAll(filepath.Join(swap, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.BeforeBoot(); err != nil {
		t.Fatalf("a boot with no swap image: %v", err)
	}
	if got := w.got[WarnSwap]; !strings.Contains(got, "swap image") || !strings.Contains(got, "compressed memory alone") {
		t.Fatalf("the warning: %q", got)
	}
	os.RemoveAll(swap)
	if err := m.BeforeBoot(); err != nil {
		t.Fatal(err)
	}
	if got, still := w.got[WarnSwap]; still {
		t.Errorf("the warning outlived the swap image's making: %q", got)
	}
}
