package cli

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fev/host/update"
)

func fakeRelease(t *testing.T, version, feed string) ed25519.PrivateKey {
	t.Helper()
	k := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, 32))
	app := filepath.Join(t.TempDir(), "Fevom.app")
	bin := filepath.Join(app, "Contents", "MacOS", "bin")
	os.MkdirAll(bin, 0o755)
	keys := ""
	if feed != "" {
		keys = "<key>SUFeedURL</key><string>" + feed + "</string><key>SUPublicEDKey</key><string>" + update.Public(k) + "</string>"
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>CFBundleShortVersionString</key><string>` +
		version + `</string>` + keys + `</dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	old := selfPath
	selfPath = func() (string, error) { return filepath.Join(bin, "fev"), nil }
	t.Cleanup(func() { selfPath = old })
	return k
}

func serveFeed(t *testing.T, k ed25519.PrivateKey, version string) string {
	t.Helper()
	zip := []byte("archive")
	feed, err := update.Render(update.Item{Version: version, Date: "Tue, 27 Oct 2026 12:00:00 +0000",
		URL: "https://example.invalid/Fevom-v" + version + ".zip", Length: len(zip), Signature: update.Sign(k, zip), MinOS: "26.0"}, k)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(feed) }))
	t.Cleanup(s.Close)
	return s.URL + "/appcast.xml"
}

func TestUpdateAsksTheAppAndCheckReadsTheSignedFeed(t *testing.T) {
	h := t.TempDir()
	var opened []string
	old := openApp
	openApp = func(path, url string) error { opened = append(opened, path+" "+url); return nil }
	t.Cleanup(func() { openApp = old })

	self := selfPath
	t.Cleanup(func() { selfPath = self })
	selfPath = func() (string, error) { return "/x/host/out/fev", nil }
	if r := fev(t, h, "", "update"); r.code != ExitFailed || !strings.Contains(r.err, "only a release's Fevom.app updates itself") {
		t.Fatalf("the build tree's fev: %+v", r)
	}
	fakeRelease(t, "0.0", "")
	if r := fev(t, h, "", "update"); r.code != ExitFailed || !strings.Contains(r.err, "does not update itself") {
		t.Fatalf("a bundle with no feed: %+v", r)
	}

	k := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, 32))
	fakeRelease(t, "0.1.13", serveFeed(t, k, "0.1.14"))
	r := fev(t, h, "", "update").want(t, 0)
	if len(opened) != 1 || !strings.HasSuffix(opened[0], "/Fevom.app fev://update") || !strings.Contains(r.out, "its window says") {
		t.Fatalf("update: opened %q, said %q", opened, r.out)
	}
	if r := fev(t, h, "", "update", "--check"); r.code != ExitFailed || !strings.Contains(r.out, "Fevom 0.1.14 is available (this is 0.1.13)") || r.err != "" {
		t.Fatalf("--check, a newer one: %+v", r)
	}
	r = fev(t, h, "", "--format", "json", "update", "--check")
	var j map[string]any
	if err := json.Unmarshal([]byte(r.out), &j); err != nil || j["latest"] != "0.1.14" || j["available"] != true || r.code != ExitFailed {
		t.Fatalf("--check json: %+v", r)
	}

	fakeRelease(t, "0.1.14", serveFeed(t, k, "0.1.14"))
	if r := fev(t, h, "", "update", "--check").want(t, 0); !strings.Contains(r.out, "0.1.14 is the newest") {
		t.Fatalf("--check, the newest: %q", r.out)
	}

	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{4}, 32))
	fakeRelease(t, "0.1.13", serveFeed(t, other, "0.1.14"))
	if r := fev(t, h, "", "update", "--check"); r.code != ExitFailed || !strings.Contains(r.err, "signature does not verify") {
		t.Fatalf("--check, another key's feed: %+v", r)
	}
}
