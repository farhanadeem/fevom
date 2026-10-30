package daemon

import (
	"strings"
	"testing"

	api "fev/proto/daemon"
)

func TestAKernelOtherThanTheManifestsIsRefusedAndNotRetried(t *testing.T) {
	f := newFixture(t, []string{"kernel=7.2.8"})
	f.opts.KernelRelease = "7.2.9"
	f.start()
	_, err := f.client().VMStart(bg, api.StartParams{})
	if code(err) != api.CodeFailed {
		t.Fatalf("vm.start: %v", err)
	}
	for _, want := range []string{`"7.2.8"`, `"7.2.9"`, "kernel", "manifest"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q lacks %s", err, want)
		}
	}
	if st := f.stopped(); f.launch.started() != 1 || st.LastExit == nil || st.LastExit.Expected {
		t.Errorf("status %+v after %d boots", st, f.launch.started())
	}
}

func TestAKernelOfTheManifestsReleaseRuns(t *testing.T) {
	for _, release := range []string{"7.2.9", ""} {
		f := newFixture(t, []string{"kernel=7.2.9"})
		f.opts.KernelRelease = release
		f.start()
		if _, err := f.client().VMStart(bg, api.StartParams{}); err != nil {
			t.Fatalf("manifest %q: vm.start: %v", release, err)
		}
		f.running()
	}
}
