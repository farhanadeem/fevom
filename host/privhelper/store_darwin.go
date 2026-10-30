//go:build darwin

package privhelper

/*
#cgo LDFLAGS: -framework CoreFoundation -framework SystemConfiguration
#include <CoreFoundation/CoreFoundation.h>
#include <SystemConfiguration/SystemConfiguration.h>
#include <libproc.h>
#include <os/log.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct { char *buf; int cap; int len; } fev_out;

static void fev_put(fev_out *o, char tag, CFStringRef s) {
	char tmp[1024];
	if (!s || CFGetTypeID(s) != CFStringGetTypeID()) return;
	if (!CFStringGetCString(s, tmp, sizeof tmp, kCFStringEncodingUTF8)) return;
	int room = o->cap - o->len;
	int n = snprintf(o->buf + o->len, room, "%c %s\n", tag, tmp);
	if (n < 0 || n >= room) { o->len = o->cap; return; }
	o->len += n;
}

static void fev_put_list(fev_out *o, char tag, CFTypeRef v) {
	if (!v) return;
	if (CFGetTypeID(v) == CFStringGetTypeID()) { fev_put(o, tag, (CFStringRef)v); return; }
	if (CFGetTypeID(v) != CFArrayGetTypeID()) return;
	CFArrayRef a = (CFArrayRef)v;
	for (CFIndex i = 0; i < CFArrayGetCount(a); i++) fev_put(o, tag, (CFStringRef)CFArrayGetValueAtIndex(a, i));
}

typedef struct { fev_out *o; } fev_ctx;

static void fev_one(const void *key, const void *value, void *ctx) {
	fev_out *o = ((fev_ctx *)ctx)->o;
	if (!value || CFGetTypeID(value) != CFDictionaryGetTypeID()) return;
	CFDictionaryRef d = (CFDictionaryRef)value;
	fev_put(o, 'K', (CFStringRef)key);
	fev_put_list(o, 'D', CFDictionaryGetValue(d, CFSTR("DomainName")));
	fev_put_list(o, 'D', CFDictionaryGetValue(d, CFSTR("SupplementalMatchDomains")));
	fev_put_list(o, 'S', CFDictionaryGetValue(d, CFSTR("SearchDomains")));
}

static int fev_dns_store(char *buf, int cap) {
	fev_out o = { buf, cap, 0 };
	buf[0] = 0;
	SCDynamicStoreRef st = SCDynamicStoreCreate(NULL, CFSTR("fev-privhelper"), NULL, NULL);
	if (!st) return -2;
	CFStringRef pat = CFSTR("^(State|Setup):/Network/(Global|Service/[^/]+)/DNS$");
	CFArrayRef pats = CFArrayCreate(NULL, (const void **)&pat, 1, &kCFTypeArrayCallBacks);
	CFDictionaryRef all = SCDynamicStoreCopyMultiple(st, NULL, pats);
	CFRelease(pats);
	int r = 0;
	if (!all) {
		r = SCError() == kSCStatusOK ? 0 : -2;
	} else {
		fev_ctx c = { &o };
		CFDictionaryApplyFunction(all, fev_one, &c);
		CFRelease(all);
		if (o.len >= cap) r = -1;
	}
	CFRelease(st);
	return r;
}

static int fev_local_host_name(char *buf, int cap) {
	CFStringRef s = SCDynamicStoreCopyLocalHostName(NULL);
	buf[0] = 0;
	if (!s) return -1;
	Boolean ok = CFStringGetCString(s, buf, cap, kCFStringEncodingUTF8);
	CFRelease(s);
	return ok ? 0 : -1;
}

static int fev_store_set(const char *key, const char *domain, int port, double written, int search) {
	SCDynamicStoreRef st = SCDynamicStoreCreate(NULL, CFSTR("fev-privhelper"), NULL, NULL);
	if (!st) return SCError();
	CFStringRef k = CFStringCreateWithCString(NULL, key, kCFStringEncodingUTF8);
	CFStringRef d = CFStringCreateWithCString(NULL, domain, kCFStringEncodingUTF8);
	CFStringRef lo = CFSTR("127.0.0.1");
	CFArrayRef doms = CFArrayCreate(NULL, (const void **)&d, 1, &kCFTypeArrayCallBacks);
	CFArrayRef srv = CFArrayCreate(NULL, (const void **)&lo, 1, &kCFTypeArrayCallBacks);
	int one = 1;
	CFNumberRef nosearch = CFNumberCreate(NULL, kCFNumberIntType, &one);
	CFNumberRef p = CFNumberCreate(NULL, kCFNumberIntType, &port);
	CFNumberRef w = CFNumberCreate(NULL, kCFNumberDoubleType, &written);
	const void *keys[] = { kSCPropNetDNSSupplementalMatchDomains, kSCPropNetDNSServerAddresses, kSCPropNetDNSServerPort,
		CFSTR("FevomWritten"), CFSTR("SupplementalMatchDomainsNoSearch") };
	const void *vals[] = { doms, srv, p, w, nosearch };
	CFDictionaryRef dict = CFDictionaryCreate(NULL, keys, vals, search ? 4 : 5, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	int r = 0;
	if (!k || !d || !dict || !SCDynamicStoreSetValue(st, k, dict)) r = SCError() ? SCError() : -1;
	if (dict) CFRelease(dict);
	CFRelease(w); CFRelease(p); CFRelease(nosearch); CFRelease(srv); CFRelease(doms);
	if (d) CFRelease(d);
	if (k) CFRelease(k);
	CFRelease(st);
	return r;
}

static int fev_store_remove(const char *key) {
	SCDynamicStoreRef st = SCDynamicStoreCreate(NULL, CFSTR("fev-privhelper"), NULL, NULL);
	if (!st) return SCError();
	CFStringRef k = CFStringCreateWithCString(NULL, key, kCFStringEncodingUTF8);
	int r = 0;
	if (!k) r = -1;
	else if (!SCDynamicStoreRemoveValue(st, k)) {
		r = SCError();
		if (r == kSCStatusNoKey) r = 0;
	}
	if (k) CFRelease(k);
	CFRelease(st);
	return r;
}

static void fev_entry(const void *key, const void *value, void *ctx) {
	fev_out *o = ((fev_ctx *)ctx)->o;
	if (!value || CFGetTypeID(value) != CFDictionaryGetTypeID()) return;
	CFDictionaryRef d = (CFDictionaryRef)value;
	fev_put(o, 'K', (CFStringRef)key);
	char tmp[64];
	int room, n;
	CFNumberRef p = CFDictionaryGetValue(d, kSCPropNetDNSServerPort);
	int port = 0;
	if (p && CFGetTypeID(p) == CFNumberGetTypeID()) CFNumberGetValue(p, kCFNumberIntType, &port);
	CFNumberRef w = CFDictionaryGetValue(d, CFSTR("FevomWritten"));
	double written = 0;
	if (w && CFGetTypeID(w) == CFNumberGetTypeID()) CFNumberGetValue(w, kCFNumberDoubleType, &written);
	n = snprintf(tmp, sizeof tmp, "P %d\nT %.6f\n", port, written);
	room = o->cap - o->len;
	if (n < 0 || n >= room) { o->len = o->cap; return; }
	memcpy(o->buf + o->len, tmp, n + 1);
	o->len += n;
}

static int fev_store_list(char *buf, int cap) {
	fev_out o = { buf, cap, 0 };
	buf[0] = 0;
	SCDynamicStoreRef st = SCDynamicStoreCreate(NULL, CFSTR("fev-privhelper"), NULL, NULL);
	if (!st) return -2;
	CFStringRef pat = CFSTR("^State:/Network/Service/dev\\.fevom\\.resolver\\.[^/]+/DNS$");
	CFArrayRef pats = CFArrayCreate(NULL, (const void **)&pat, 1, &kCFTypeArrayCallBacks);
	CFDictionaryRef all = SCDynamicStoreCopyMultiple(st, NULL, pats);
	CFRelease(pats);
	int r = 0;
	if (!all) {
		r = SCError() == kSCStatusOK ? 0 : -2;
	} else {
		fev_ctx c = { &o };
		CFDictionaryApplyFunction(all, fev_entry, &c);
		CFRelease(all);
		if (o.len >= cap) r = -1;
	}
	CFRelease(st);
	return r;
}

static int fev_pid_path(int pid, char *buf, int cap) {
	return proc_pidpath(pid, buf, (uint32_t)cap);
}

static os_log_t fev_log_handle;

static void fev_log(const char *s) {
	if (!fev_log_handle) fev_log_handle = os_log_create("dev.fevom.privhelper", "helper");
	os_log(fev_log_handle, "%{public}s", s);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	api "fev/proto/privhelper"
)

const storeBuf = 256 << 10

func (Darwin) DNSResolvers() ([]Resolver, error) {
	buf := (*C.char)(C.malloc(storeBuf))
	defer C.free(unsafe.Pointer(buf))
	switch C.fev_dns_store(buf, storeBuf) {
	case 0:
	case -1:
		return nil, errors.New("the DNS configuration is larger than the helper reads")
	default:
		return nil, errors.New("the System Configuration store cannot be read")
	}
	return parseStoreLines(C.GoString(buf)), nil
}

func parseStoreLines(s string) []Resolver {
	var out []Resolver
	for _, line := range strings.Split(s, "\n") {
		tag, v, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		switch tag {
		case "K":
			out = append(out, Resolver{Source: v})
		case "D":
			if len(out) > 0 {
				out[len(out)-1].Domains = append(out[len(out)-1].Domains, v)
			}
		case "S":
			if len(out) > 0 {
				out[len(out)-1].Search = append(out[len(out)-1].Search, v)
			}
		}
	}
	return out
}

type DarwinStore struct{}

func (DarwinStore) Entries() (map[string]StoreEntry, error) {
	buf := (*C.char)(C.malloc(storeBuf))
	defer C.free(unsafe.Pointer(buf))
	switch C.fev_store_list(buf, storeBuf) {
	case 0:
	case -1:
		return nil, errors.New("fev's resolver entries are more than the helper reads")
	default:
		return nil, errors.New("the System Configuration store cannot be read")
	}
	return parseEntryLines(C.GoString(buf)), nil
}

func parseEntryLines(s string) map[string]StoreEntry {
	out := map[string]StoreEntry{}
	name := ""
	for _, line := range strings.Split(s, "\n") {
		tag, v, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		switch tag {
		case "K":
			n, ok := strings.CutPrefix(v, "State:/Network/Service/"+api.StoreServicePrefix)
			n, ok2 := strings.CutSuffix(n, "/DNS")
			name = ""
			if ok && ok2 && api.StoreKey(n) == v && entryName(n) {
				name = n
				out[name] = StoreEntry{}
			}
		case "P":
			if e, ok := out[name]; ok && name != "" {
				e.Port, _ = strconv.Atoi(v)
				out[name] = e
			}
		case "T":
			if e, ok := out[name]; ok && name != "" {
				if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
					sec := int64(f)
					e.Written = time.Unix(sec, int64((f-float64(sec))*1e9))
				}
				out[name] = e
			}
		}
	}
	return out
}

func (DarwinStore) Set(name string, port int, written time.Time) error {
	ck, cn := C.CString(api.StoreKey(name)), C.CString(name)
	defer C.free(unsafe.Pointer(ck))
	defer C.free(unsafe.Pointer(cn))
	search := C.int(0)
	if api.StoreSearches(name) {
		search = 1
	}
	if e := C.fev_store_set(ck, cn, C.int(port), C.double(float64(written.UnixNano())/1e9), search); e != 0 {
		return fmt.Errorf("setting %s: %s", api.StoreKey(name), scError(int(e)))
	}
	return nil
}

func (DarwinStore) Remove(name string) error {
	ck := C.CString(api.StoreKey(name))
	defer C.free(unsafe.Pointer(ck))
	if e := C.fev_store_remove(ck); e != 0 {
		return fmt.Errorf("removing %s: %s", api.StoreKey(name), scError(int(e)))
	}
	return nil
}

func scError(e int) string {
	switch e {
	case 1003:
		return "access denied (the store is written by root)"
	case 1004:
		return "no such key"
	case -1:
		return "out of memory"
	}
	return fmt.Sprintf("System Configuration status %d", e)
}

func (Darwin) LocalHostName() (string, error) {
	var buf [256]C.char
	if C.fev_local_host_name(&buf[0], C.int(len(buf))) != 0 {
		return "", errors.New("the Mac has no local host name")
	}
	return C.GoString(&buf[0]), nil
}

func PeerOf(uc *net.UnixConn) (Peer, error) {
	raw, err := uc.SyscallConn()
	if err != nil {
		return Peer{}, err
	}
	var cred *unix.Xucred
	var pid int
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		pid, _ = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return Peer{}, err
	}
	if cerr != nil {
		return Peer{}, cerr
	}
	if cred.Version != 0 {
		return Peer{}, fmt.Errorf("xucred version %d", cred.Version)
	}
	p := Peer{UID: int(cred.Uid), PID: pid}
	if pid > 0 {
		var buf [4096]C.char
		if C.fev_pid_path(C.int(pid), &buf[0], C.int(len(buf))) > 0 {
			p.Path = C.GoString(&buf[0])
		}
	}
	return p, nil
}

func SystemLog(format string, a ...any) {
	s := C.CString(fmt.Sprintf(format, a...))
	defer C.free(unsafe.Pointer(s))
	C.fev_log(s)
}
