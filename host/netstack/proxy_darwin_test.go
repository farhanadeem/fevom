//go:build darwin && cgo

package netstack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const manualSettings = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>HTTPEnable</key><integer>1</integer><key>HTTPProxy</key><string>proxy-a.test</string><key>HTTPPort</key><integer>3128</integer>
<key>HTTPSEnable</key><integer>1</integer><key>HTTPSProxy</key><string>proxy-b.test</string><key>HTTPSPort</key><integer>3129</integer>
<key>ExceptionsList</key><array><string>*.local</string><string>169.254/16</string><string>internal.example</string><string>10.0.0.0/8</string></array>
<key>ExcludeSimpleHostnames</key><integer>1</integer>
</dict></plist>`

func pacSettings(url string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>ProxyAutoConfigEnable</key><integer>1</integer>
<key>ProxyAutoConfigURLString</key><string>` + url + `</string></dict></plist>`
}

var decisions = []struct{ url, pac, manual string }{
	{"http://intranet/", "direct", "direct"},
	{"http://web.fev.local/", "direct", "direct"},
	{"http://10.1.2.3/x", "direct", "direct"},
	{"https://registry-1.docker.io/v2/", "http proxy pac-a.test:8080", "http proxy proxy-b.test:3129"},
	{"https://api.github.com/repos", "direct", "http proxy proxy-b.test:3129"},
	{"http://a.socks.example/", "direct", "http proxy proxy-a.test:3128"},
	{"http://a.socks4.example/", "socks5 proxy pac-socks.test:1081", "http proxy proxy-a.test:3128"},
	{"http://b.fallback.example/", "http proxy pac-down.test:1; http proxy pac-b.test:3128; direct", "http proxy proxy-a.test:3128"},
	{"http://example.com/no-proxy/y", "http proxy pac-default.test:3128", "http proxy proxy-a.test:3128"},
	{"http://path.example/a/b?q=1", "http proxy stripped.test:1", "http proxy proxy-a.test:3128"},
	{"https://example.com/", "http proxy pac-default.test:3128", "http proxy proxy-b.test:3129"},
	{"http://internal.example/", "http proxy pac-default.test:3128", "direct"},
}

func describeUpstreams(ups []Upstream) string {
	var parts []string
	for _, u := range ups {
		parts = append(parts, u.String())
	}
	return strings.Join(parts, "; ")
}

func TestTheProxyDecisionsAreCFNetworksForEveryKindOfSetting(t *testing.T) {
	pac, err := os.ReadFile("testdata/proxy.pac")
	if err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
		w.Write(pac)
	}))
	defer srv.Close()
	arms := []struct {
		name    string
		d       cfDecider
		manual  bool
		timeout time.Duration
	}{
		{"manual", cfDecider{settings: manualSettings}, true, 5 * time.Second},
		{"pac script", cfDecider{script: string(pac)}, false, 10 * time.Second},
		{"pac url", cfDecider{settings: pacSettings(srv.URL + "/proxy.pac")}, false, 20 * time.Second},
	}
	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			for _, tc := range decisions {
				ctx, cancel := context.WithTimeout(context.Background(), arm.timeout)
				ups, err := arm.d.Decide(ctx, tc.url)
				cancel()
				want := tc.pac
				if arm.manual {
					want = tc.manual
				}
				if err != nil || describeUpstreams(ups) != want {
					t.Errorf("%s: %q, %v; want %q", tc.url, describeUpstreams(ups), err, want)
				}
			}
		})
	}
	if fetches.Load() == 0 {
		t.Fatal("the PAC by URL was never fetched")
	}
}

func TestTheMacsOwnProxySettingsGiveADecision(t *testing.T) {
	ups, err := SystemProxies().Decide(context.Background(), "https://registry-1.docker.io/")
	if err != nil || len(ups) == 0 {
		t.Fatalf("%v %v", ups, err)
	}
	t.Logf("this Mac: %s", describeUpstreams(ups))
}

func TestProxySettingsSayWhetherAnyProxyIsOn(t *testing.T) {
	for _, tc := range []struct {
		keys string
		want bool
	}{
		{`<key>HTTPSEnable</key><integer>1</integer><key>HTTPSProxy</key><string>127.0.0.1</string><key>HTTPSPort</key><integer>3128</integer>`, true},
		{`<key>HTTPEnable</key><integer>1</integer><key>HTTPProxy</key><string>127.0.0.1</string>`, true},
		{`<key>SOCKSEnable</key><integer>1</integer><key>SOCKSProxy</key><string>127.0.0.1</string>`, true},
		{`<key>ProxyAutoConfigEnable</key><integer>1</integer><key>ProxyAutoConfigURLString</key><string>http://127.0.0.1/p.pac</string>`, true},
		{`<key>ProxyAutoDiscoveryEnable</key><integer>1</integer>`, true},
		{`<key>HTTPSEnable</key><integer>0</integer><key>HTTPSProxy</key><string>127.0.0.1</string><key>ProxyAutoConfigEnable</key><integer>0</integer>`, false},
		{``, false},
	} {
		plist := `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict>` + tc.keys + `</dict></plist>`
		if got := ProxiesFromSettings(plist).(ProxyPresence).Configured(); got != tc.want {
			t.Errorf("%s: %v", tc.keys, got)
		}
	}
	if !(cfDecider{script: "function FindProxyForURL(u, h) { return \"DIRECT\"; }"}).Configured() {
		t.Error("a PAC's text alone is not a proxy")
	}
}
