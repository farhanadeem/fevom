//go:build darwin && cgo

package netstack

/*
#cgo LDFLAGS: -framework CoreFoundation -framework CFNetwork
#include <CFNetwork/CFNetwork.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>

static void cstr(CFStringRef s, char *buf, size_t n) {
	buf[0] = 0;
	if (s && CFGetTypeID(s) == CFStringGetTypeID()) CFStringGetCString(s, buf, n, kCFStringEncodingUTF8);
}

struct pacresult {
	CFArrayRef list;
	int done, failed;
};

static void pac_cb(void *client, CFArrayRef proxies, CFErrorRef error) {
	struct pacresult *r = client;
	if (proxies) r->list = CFRetain(proxies);
	if (error) r->failed = 1;
	r->done = 1;
	CFRunLoopStop(CFRunLoopGetCurrent());
}

static CFArrayRef run_pac(CFURLRef pacurl, CFStringRef script, CFURLRef target, double timeout, int *failed) {
	struct pacresult r = {0};
	CFStreamClientContext ctx = {0, &r, NULL, NULL, NULL};
	CFRunLoopSourceRef src = pacurl ? CFNetworkExecuteProxyAutoConfigurationURL(pacurl, target, pac_cb, &ctx)
	                                : CFNetworkExecuteProxyAutoConfigurationScript(script, target, pac_cb, &ctx);
	if (!src) {
		*failed = 1;
		return NULL;
	}
	CFRunLoopAddSource(CFRunLoopGetCurrent(), src, kCFRunLoopDefaultMode);
	CFAbsoluteTime end = CFAbsoluteTimeGetCurrent() + timeout;
	while (!r.done) {
		double left = end - CFAbsoluteTimeGetCurrent();
		if (left <= 0) break;
		CFRunLoopRunInMode(kCFRunLoopDefaultMode, left, true);
	}
	CFRunLoopRemoveSource(CFRunLoopGetCurrent(), src, kCFRunLoopDefaultMode);
	CFRunLoopSourceInvalidate(src);
	CFRelease(src);
	if (!r.done || r.failed) *failed = 1;
	return r.list;
}

static void append_line(char **out, size_t *len, size_t *cap, const char *line) {
	size_t n = strlen(line);
	if (*len + n + 1 > *cap) {
		size_t c = *cap ? *cap * 2 : 256;
		while (c < *len + n + 1) c *= 2;
		char *nb = realloc(*out, c);
		if (!nb) return;
		*out = nb;
		*cap = c;
	}
	memcpy(*out + *len, line, n + 1);
	*len += n;
}

static void describe(CFArrayRef list, char **out, size_t *len, size_t *cap) {
	if (!list) return;
	for (CFIndex i = 0; i < CFArrayGetCount(list); i++) {
		CFDictionaryRef p = CFArrayGetValueAtIndex(list, i);
		if (CFGetTypeID(p) != CFDictionaryGetTypeID()) continue;
		CFStringRef type = CFDictionaryGetValue(p, kCFProxyTypeKey);
		const char *t = NULL;
		if (!type) continue;
		if (CFEqual(type, kCFProxyTypeNone)) t = "none";
		else if (CFEqual(type, kCFProxyTypeHTTP)) t = "http";
		else if (CFEqual(type, kCFProxyTypeHTTPS)) t = "https";
		else if (CFEqual(type, kCFProxyTypeSOCKS)) t = "socks";
		else continue;
		char host[512] = "", line[640];
		int port = 0;
		cstr(CFDictionaryGetValue(p, kCFProxyHostNameKey), host, sizeof host);
		CFNumberRef pn = CFDictionaryGetValue(p, kCFProxyPortNumberKey);
		if (pn && CFGetTypeID(pn) == CFNumberGetTypeID()) CFNumberGetValue(pn, kCFNumberIntType, &port);
		snprintf(line, sizeof line, "%s %s %d\n", t, host[0] ? host : "-", port);
		append_line(out, len, cap, line);
	}
}

static int fev_proxies_for_url(const char *url, const char *settings, const char *script, double timeout, char **out) {
	*out = NULL;
	size_t len = 0, cap = 0;
	CFStringRef us = CFStringCreateWithCString(NULL, url, kCFStringEncodingUTF8);
	CFURLRef target = us ? CFURLCreateWithString(NULL, us, NULL) : NULL;
	if (us) CFRelease(us);
	if (!target) return 2;
	int rc = 0, failed = 0;
	if (script) {
		CFStringRef s = CFStringCreateWithCString(NULL, script, kCFStringEncodingUTF8);
		CFArrayRef list = s ? run_pac(NULL, s, target, timeout, &failed) : NULL;
		if (s) CFRelease(s);
		describe(list, out, &len, &cap);
		if (list) CFRelease(list);
		if (failed && !list) rc = 1;
		CFRelease(target);
		if (!*out) *out = strdup("");
		return rc;
	}
	CFDictionaryRef dict = NULL;
	if (settings) {
		CFDataRef data = CFDataCreate(NULL, (const UInt8 *)settings, strlen(settings));
		CFPropertyListRef pl = data ? CFPropertyListCreateWithData(NULL, data, kCFPropertyListImmutable, NULL, NULL) : NULL;
		if (data) CFRelease(data);
		if (!pl || CFGetTypeID(pl) != CFDictionaryGetTypeID()) {
			if (pl) CFRelease(pl);
			CFRelease(target);
			return 2;
		}
		dict = pl;
	} else {
		dict = CFNetworkCopySystemProxySettings();
	}
	CFArrayRef list = dict ? CFNetworkCopyProxiesForURL(target, dict) : NULL;
	if (list && CFArrayGetCount(list) > 0) {
		CFDictionaryRef p = CFArrayGetValueAtIndex(list, 0);
		CFStringRef type = CFDictionaryGetValue(p, kCFProxyTypeKey);
		CFArrayRef pac = NULL;
		int ran = 0;
		if (type && CFEqual(type, kCFProxyTypeAutoConfigurationURL)) {
			CFURLRef pu = CFDictionaryGetValue(p, kCFProxyAutoConfigurationURLKey);
			if (pu) {
				pac = run_pac(pu, NULL, target, timeout, &failed);
				ran = 1;
			}
		} else if (type && CFEqual(type, kCFProxyTypeAutoConfigurationJavaScript)) {
			CFStringRef js = CFDictionaryGetValue(p, kCFProxyAutoConfigurationJavaScriptKey);
			if (js) {
				pac = run_pac(NULL, js, target, timeout, &failed);
				ran = 1;
			}
		}
		if (ran) {
			describe(pac, out, &len, &cap);
			if (failed && !pac) rc = 1;
			if (pac) CFRelease(pac);
		} else {
			describe(list, out, &len, &cap);
		}
	}
	if (list) CFRelease(list);
	if (dict) CFRelease(dict);
	CFRelease(target);
	if (!*out) *out = strdup("");
	return rc;
}

static int on(CFDictionaryRef d, const void *key) {
	CFNumberRef n = CFDictionaryGetValue(d, key);
	int v = 0;
	if (n && CFGetTypeID(n) == CFNumberGetTypeID()) CFNumberGetValue(n, kCFNumberIntType, &v);
	return v != 0;
}

static int fev_proxies_configured(const char *settings) {
	CFDictionaryRef dict = NULL;
	if (settings) {
		CFDataRef data = CFDataCreate(NULL, (const UInt8 *)settings, strlen(settings));
		CFPropertyListRef pl = data ? CFPropertyListCreateWithData(NULL, data, kCFPropertyListImmutable, NULL, NULL) : NULL;
		if (data) CFRelease(data);
		if (!pl || CFGetTypeID(pl) != CFDictionaryGetTypeID()) {
			if (pl) CFRelease(pl);
			return -1;
		}
		dict = pl;
	} else {
		dict = CFNetworkCopySystemProxySettings();
	}
	if (!dict) return 0;
	int any = on(dict, kCFNetworkProxiesHTTPEnable) || on(dict, kCFNetworkProxiesHTTPSEnable) ||
	          on(dict, CFSTR("SOCKSEnable")) || on(dict, kCFNetworkProxiesProxyAutoConfigEnable) ||
	          on(dict, CFSTR("ProxyAutoDiscoveryEnable"));
	CFRelease(dict);
	return any;
}
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

const pacTimeout = 10 * time.Second

var proxySlots = make(chan struct{}, 16)

type cfDecider struct {
	settings string
	script   string
}

func SystemProxies() ProxyDecider { return cfDecider{} }

func ProxiesFromSettings(plist string) ProxyDecider { return cfDecider{settings: plist} }

func (d cfDecider) Decide(ctx context.Context, url string) ([]Upstream, error) {
	select {
	case proxySlots <- struct{}{}:
		defer func() { <-proxySlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	timeout := pacTimeout
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < timeout {
		timeout = time.Until(deadline)
	}
	curl := C.CString(url)
	defer C.free(unsafe.Pointer(curl))
	var csettings, cscript *C.char
	if d.settings != "" {
		csettings = C.CString(d.settings)
		defer C.free(unsafe.Pointer(csettings))
	}
	if d.script != "" {
		cscript = C.CString(d.script)
		defer C.free(unsafe.Pointer(cscript))
	}
	var out *C.char
	rc := C.fev_proxies_for_url(curl, csettings, cscript, C.double(timeout.Seconds()), &out)
	defer C.free(unsafe.Pointer(out))
	switch rc {
	case 0:
	case 1:
		return nil, errors.New("the Mac's proxy auto-configuration gave no answer")
	default:
		return nil, fmt.Errorf("CFNetwork cannot decide for %s", url)
	}
	return parseDecisions(C.GoString(out))
}

func (d cfDecider) Configured() bool {
	if d.script != "" {
		return true
	}
	var csettings *C.char
	if d.settings != "" {
		csettings = C.CString(d.settings)
		defer C.free(unsafe.Pointer(csettings))
	}
	return C.fev_proxies_configured(csettings) == 1
}

func parseDecisions(text string) ([]Upstream, error) {
	var out []Upstream
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		port, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		switch f[0] {
		case "none":
			out = append(out, Upstream{Kind: Direct})
		case "http", "https":
			if port == 0 {
				port = 80
			}
			out = append(out, Upstream{Kind: ViaHTTP, Addr: hostPort(f[1], port)})
		case "socks":
			if port == 0 {
				port = 1080
			}
			out = append(out, Upstream{Kind: ViaSOCKS5, Addr: hostPort(f[1], port), RemoteNames: true})
		}
	}
	if len(out) == 0 {
		out = []Upstream{{Kind: Direct}}
	}
	return out, nil
}
