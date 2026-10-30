// The decider's fixture: every branch has a known answer (proxy_darwin_test.go), so the
// decisions CFNetwork returns can be checked line by line.
function FindProxyForURL(url, host) {
	if (isPlainHostName(host)) return "DIRECT";
	if (dnsDomainIs(host, ".local") || shExpMatch(host, "*.fev.local")) return "DIRECT";
	if (isInNet(host, "10.0.0.0", "255.0.0.0") || isInNet(host, "192.168.0.0", "255.255.0.0")) return "DIRECT";
	if (shExpMatch(host, "*.docker.io") || host == "registry-1.docker.io") return "PROXY pac-a.test:8080";
	if (shExpMatch(url, "https://*") && dnsDomainIs(host, ".github.com")) return "HTTPS pac-tls.test:8443";
	if (dnsDomainIs(host, ".socks.example")) return "SOCKS5 pac-socks.test:1080";
	if (dnsDomainIs(host, ".socks4.example")) return "SOCKS pac-socks.test:1081";
	// What the PAC is given as url: the whole URL, or the scheme and host alone?
	if (url == "http://path.example/") return "PROXY stripped.test:1";
	if (url == "http://path.example/a/b?q=1") return "PROXY whole.test:2";
	if (dnsDomainIs(host, ".fallback.example")) return "PROXY pac-down.test:1; PROXY pac-b.test:3128; DIRECT";
	if (shExpMatch(url, "*/no-proxy/*")) return "DIRECT";
	return "PROXY pac-default.test:3128";
}
