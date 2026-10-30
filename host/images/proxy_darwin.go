package images

/*
#cgo LDFLAGS: -framework CoreFoundation -framework SystemConfiguration
#include <CoreFoundation/CoreFoundation.h>
#include <SystemConfiguration/SystemConfiguration.h>
#include <string.h>

static int fev_https_proxy(char *host, int hostlen, int *port, char *except, int exceptlen) {
	CFDictionaryRef d = SCDynamicStoreCopyProxies(NULL);
	if (!d) return 0;
	int on = 0, pac = 0;
	CFNumberRef n = CFDictionaryGetValue(d, kSCPropNetProxiesHTTPSEnable);
	if (n) CFNumberGetValue(n, kCFNumberIntType, &on);
	CFNumberRef a = CFDictionaryGetValue(d, kSCPropNetProxiesProxyAutoConfigEnable);
	if (a) CFNumberGetValue(a, kCFNumberIntType, &pac);
	int r = 0;
	if (on) {
		CFStringRef h = CFDictionaryGetValue(d, kSCPropNetProxiesHTTPSProxy);
		CFNumberRef p = CFDictionaryGetValue(d, kSCPropNetProxiesHTTPSPort);
		if (h && CFStringGetCString(h, host, hostlen, kCFStringEncodingUTF8)) {
			*port = 0;
			if (p) CFNumberGetValue(p, kCFNumberIntType, port);
			r = 1;
		}
		except[0] = 0;
		CFArrayRef ex = CFDictionaryGetValue(d, kSCPropNetProxiesExceptionsList);
		int used = 0;
		for (CFIndex i = 0; ex && i < CFArrayGetCount(ex); i++) {
			CFStringRef s = CFArrayGetValueAtIndex(ex, i);
			if (CFGetTypeID(s) != CFStringGetTypeID()) continue;
			char buf[256];
			if (!CFStringGetCString(s, buf, sizeof buf, kCFStringEncodingUTF8)) continue;
			int l = (int)strlen(buf);
			if (used + l + 2 > exceptlen) break;
			memcpy(except + used, buf, l);
			used += l;
			except[used++] = '\n';
			except[used] = 0;
		}
	} else if (pac) {
		r = 2;
	}
	CFRelease(d);
	return r;
}
*/
import "C"

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unsafe"
)

func macProxy(req *http.Request) (*url.URL, error) {
	if u, err := http.ProxyFromEnvironment(req); u != nil || err != nil {
		return u, err
	}
	host := make([]byte, 1024)
	except := make([]byte, 8192)
	var port C.int
	if C.fev_https_proxy((*C.char)(unsafe.Pointer(&host[0])), C.int(len(host)), &port, (*C.char)(unsafe.Pointer(&except[0])), C.int(len(except))) != 1 {
		return nil, nil
	}
	h := C.GoString((*C.char)(unsafe.Pointer(&host[0])))
	if h == "" || excepted(req.URL.Hostname(), C.GoString((*C.char)(unsafe.Pointer(&except[0])))) {
		return nil, nil
	}
	p := int(port)
	if p == 0 {
		p = 443
	}
	return &url.URL{Scheme: "http", Host: net.JoinHostPort(h, strconv.Itoa(p))}, nil
}

func excepted(host, list string) bool {
	host = strings.ToLower(host)
	for _, e := range strings.Split(list, "\n") {
		e = strings.ToLower(strings.TrimSpace(e))
		switch {
		case e == "":
		case e == host:
			return true
		case strings.HasPrefix(e, "*."):
			if strings.HasSuffix(host, e[1:]) {
				return true
			}
		case strings.HasPrefix(e, "."):
			if strings.HasSuffix(host, e) {
				return true
			}
		}
	}
	return false
}
