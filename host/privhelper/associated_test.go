package privhelper_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"fev/host/privhelper"
)

func TestTheHelpersJobsNameTheAppsBundle(t *testing.T) {
	dir := t.TempDir()
	for name, plist := range map[string][]byte{"job": privhelper.LaunchdPlist(501), "boot": privhelper.BootPlist()} {
		path := filepath.Join(dir, name+".plist")
		os.WriteFile(path, plist, 0o644)
		out, err := exec.Command("/usr/bin/plutil", "-extract", "AssociatedBundleIdentifiers", "json", "-o", "-", path).CombinedOutput()
		if got := strings.TrimSpace(string(out)); err != nil || got != `["`+privhelper.AppBundleID+`"]` {
			t.Errorf("%s: AssociatedBundleIdentifiers %s (%v)", name, out, err)
		}
	}
}
