package cli

import (
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type fakeKeychain struct {
	state   string
	removed [][]byte
	deleted []string
}

func (k *fakeKeychain) State([]byte) (string, error) { return k.state, nil }
func (k *fakeKeychain) Remove(der []byte) error {
	k.removed = append(k.removed, der)
	return nil
}
func (k *fakeKeychain) DeleteKey(signer string) error {
	k.deleted = append(k.deleted, signer)
	return nil
}

func installedApp(t *testing.T, apps, id string) string {
	t.Helper()
	app := filepath.Join(apps, "Fevom.app")
	for _, f := range []string{"MacOS/Fevom", "MacOS/bin/fev", "Helpers/fevd", "Helpers/fev-keysigner"} {
		p := filepath.Join(app, "Contents", f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>` + id + `</string></dict></plist>
`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	old := selfPath
	selfPath = func() (string, error) { return filepath.Join(app, "Contents", "MacOS", "bin", "fev"), nil }
	t.Cleanup(func() { selfPath = old })
	return app
}

func macCalls(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	old := macCommand
	macCommand = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	t.Cleanup(func() { macCommand = old })
	return &calls
}

func fevUninstall(t *testing.T, home string, adm Admin, kc Keychain, argv ...string) result {
	t.Helper()
	var out, errw lockedBuffer
	code := Main(Env{
		Name: "fev", Args: append([]string{"--home", home, "uninstall"}, argv...),
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errw, Admin: adm, Keychain: kc,
	})
	return result{out.String(), errw.String(), code}
}

func writeFile(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func TestUninstallRemovesEverythingOfTheUsersOwnInstall(t *testing.T) {
	user := t.TempDir()
	t.Setenv("HOME", user)
	app := installedApp(t, filepath.Join(user, "Applications"), appID)
	calls := macCalls(t)
	fev := filepath.Join(user, ".fev")
	writeFile(t, filepath.Join(fev, "config", "config.json"), "{}", 0o644)
	der := []byte("the root")
	writeFile(t, filepath.Join(fev, "https", "ca.pem"), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), 0o644)
	root := t.TempDir()
	t.Setenv("FEV_ADMIN_ROOT", root)
	writeFile(t, filepath.Join(root, "Library/Application Support/dev.fevom/privhelper/install.json"), "{}", 0o644)
	agent := filepath.Join(user, "Library", "LaunchAgents", "dev.fevom.fevd.plist")
	writeFile(t, agent, "<string>"+app+"/Contents/Helpers/fevd</string>", 0o644)
	data := filepath.Join(user, "Library", "Application Support", "dev.fevom")
	writeFile(t, filepath.Join(data, "data", "data.img"), "image", 0o644)
	cache := filepath.Join(user, "Library", "Caches", appID)
	writeFile(t, filepath.Join(cache, "org.sparkle-project.Sparkle", "x"), "", 0o644)
	view := filepath.Join(user, "Fevom")
	writeFile(t, filepath.Join(view, "README.txt"), "fev is stopped", 0o444)
	os.Chmod(view, 0o555)

	adm, kc := &fakeAdmin{}, &fakeKeychain{state: "trusted"}
	r := fevUninstall(t, fev, adm, kc, "-y", "--delete-data").want(t, 0)
	for _, p := range []string{app, agent, data, cache, view, fev} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("left %s", p)
		}
	}
	if !slices.Equal(adm.undos, []string{root}) {
		t.Errorf("the admin helper's undo: %q", adm.undos)
	}
	if len(kc.removed) != 1 || string(kc.removed[0]) != string(der) {
		t.Errorf("the trust removed: %q", kc.removed)
	}
	if !slices.Equal(kc.deleted, []string{filepath.Join(app, "Contents", "Helpers", "fev-keysigner")}) {
		t.Errorf("the key deleted through %q", kc.deleted)
	}
	for _, want := range []string{app + "/Contents/MacOS/Fevom --uninstall-cleanup", "/usr/bin/osascript -e if application id \"dev.fevom.app\" is running", "/usr/bin/defaults delete dev.fevom.app"} {
		found := false
		for _, c := range *calls {
			found = found || strings.HasPrefix(c, want)
		}
		if !found {
			t.Errorf("did not run %q: %q", want, *calls)
		}
	}
	if !strings.Contains(r.out, "Fevom is uninstalled.") {
		t.Errorf("output: %q", r.out)
	}
}

func TestUninstallOfAHomeOfItsOwnKeepsItsData(t *testing.T) {
	user := t.TempDir()
	t.Setenv("HOME", user)
	home := scratchHome(t)
	apps := filepath.Join(user, "Applications")
	app := installedApp(t, apps, appID)
	calls := macCalls(t)
	writeFile(t, filepath.Join(home, "install.record"), "app "+app+"\napps_made 1\n", 0o644)
	writeFile(t, filepath.Join(home, "config", "config.json"), "{}", 0o644)
	writeFile(t, filepath.Join(home, "https", "ca.pem"), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("a root")})), 0o644)
	adm, kc := &fakeAdmin{}, &fakeKeychain{state: "trusted"}

	before := snapshot(t, user)
	r := fevUninstall(t, home, adm, kc, "--dry-run", "--delete-data").want(t, 0)
	if !strings.Contains(r.out, "Would remove "+app) || !strings.Contains(r.out, "Would delete fev's home") {
		t.Errorf("dry run said %q", r.out)
	}
	fevUninstall(t, home, adm, kc).want(t, ExitUsage)
	if after := snapshot(t, user); after != before {
		t.Errorf("changed:\n%s\nafter:\n%s", before, after)
	}

	fevUninstall(t, home, adm, kc, "-y").want(t, 0)
	if _, err := os.Stat(apps); err == nil {
		t.Errorf("left %s, which the install made", apps)
	}
	if _, err := os.Stat(filepath.Join(home, "config", "config.json")); err != nil {
		t.Errorf("the home's settings went: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "install.record")); err == nil {
		t.Error("the install record is left")
	}
	if len(adm.undos)+len(kc.removed)+len(kc.deleted)+len(*calls) != 0 {
		t.Errorf("touched the Mac's: undos %q, trust %q, key %q, commands %q", adm.undos, kc.removed, kc.deleted, *calls)
	}
}

func TestUninstallRemovesNoOtherApp(t *testing.T) {
	home := scratchHome(t)
	app := installedApp(t, t.TempDir(), "com.example.other")
	macCalls(t)
	writeFile(t, filepath.Join(home, "install.record"), "app "+app+"\n", 0o644)
	r := fevUninstall(t, home, nil, nil, "-y", "--delete-data").want(t, ExitFailed)
	if _, err := os.Stat(app); err != nil || !strings.Contains(r.err, "is not Fevom.app") {
		t.Errorf("%q: %v", r.err, err)
	}
	if _, err := os.Stat(filepath.Join(home, "install.record")); err != nil {
		t.Errorf("the home was changed: %v", err)
	}
}
