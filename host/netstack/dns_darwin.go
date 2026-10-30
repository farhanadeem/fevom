package netstack

/*
#cgo LDFLAGS: -framework CoreFoundation -framework SystemConfiguration
#include <dns_sd.h>
#include <poll.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <SystemConfiguration/SystemConfiguration.h>

typedef struct {
	unsigned char *buf;
	size_t len, cap;
	uint16_t want;
	int done;
	DNSServiceErrorType err;
} fev_dns_result;

static void fev_append(fev_dns_result *r, const void *p, size_t n) {
	if (r->len + n > r->cap) {
		size_t cap = r->cap ? r->cap * 2 : 512;
		while (cap < r->len + n) cap *= 2;
		unsigned char *nb = realloc(r->buf, cap);
		if (!nb) { r->err = kDNSServiceErr_NoMemory; r->done = 1; return; }
		r->buf = nb;
		r->cap = cap;
	}
	memcpy(r->buf + r->len, p, n);
	r->len += n;
}

static void fev_dns_cb(DNSServiceRef ref, DNSServiceFlags flags, uint32_t ifindex,
		DNSServiceErrorType err, const char *fullname, uint16_t rrtype, uint16_t rrclass,
		uint16_t rdlen, const void *rdata, uint32_t ttl, void *ctx) {
	fev_dns_result *r = ctx;
	if (err == kDNSServiceErr_NoSuchRecord || err == kDNSServiceErr_NoSuchName) { r->done = 1; return; }
	if (err != kDNSServiceErr_NoError) { r->err = err; r->done = 1; return; }
	if (flags & kDNSServiceFlagsAdd) {
		fev_append(r, fullname, strlen(fullname) + 1);
		fev_append(r, &rrtype, 2);
		fev_append(r, &rrclass, 2);
		fev_append(r, &ttl, 4);
		fev_append(r, &rdlen, 2);
		fev_append(r, rdata, rdlen);
	}
	if (rrtype == r->want && !(flags & kDNSServiceFlagsMoreComing)) r->done = 1;
}

static int64_t fev_now_ms(void) {
	struct timespec ts;
	clock_gettime(CLOCK_MONOTONIC, &ts);
	return (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
}

static int fev_dns_query(const char *name, uint16_t rrtype, int timeout_ms, fev_dns_result *r) {
	DNSServiceRef ref;
	r->want = rrtype;
	DNSServiceErrorType e = DNSServiceQueryRecord(&ref,
		kDNSServiceFlagsReturnIntermediates | kDNSServiceFlagsTimeout, 0,
		name, rrtype, kDNSServiceClass_IN, fev_dns_cb, r);
	if (e != kDNSServiceErr_NoError) return e;
	int64_t deadline = fev_now_ms() + timeout_ms;
	struct pollfd pfd = { .fd = DNSServiceRefSockFD(ref), .events = POLLIN };
	int timed_out = 0;
	while (!r->done) {
		int64_t left = deadline - fev_now_ms();
		if (left <= 0 || poll(&pfd, 1, (int)left) <= 0) { timed_out = 1; break; }
		e = DNSServiceProcessResult(ref);
		if (e != kDNSServiceErr_NoError) { r->err = e; break; }
	}
	DNSServiceRefDeallocate(ref);
	if (r->err != kDNSServiceErr_NoError) return r->err;
	return (timed_out && r->len == 0) ? 1 : 0;
}

static char *fev_search_domains(void) {
	CFPropertyListRef value = SCDynamicStoreCopyValue(NULL, CFSTR("State:/Network/Global/DNS"));
	if (!value) return NULL;
	char *out = NULL;
	if (CFGetTypeID(value) == CFDictionaryGetTypeID()) {
		CFArrayRef domains = CFDictionaryGetValue(value, CFSTR("SearchDomains"));
		if (domains && CFGetTypeID(domains) == CFArrayGetTypeID()) {
			CFStringRef joined = CFStringCreateByCombiningStrings(NULL, domains, CFSTR("\n"));
			if (joined) {
				CFIndex size = CFStringGetMaximumSizeForEncoding(CFStringGetLength(joined), kCFStringEncodingUTF8) + 1;
				out = malloc(size);
				if (out && !CFStringGetCString(joined, out, size, kCFStringEncodingUTF8)) { free(out); out = NULL; }
				CFRelease(joined);
			}
		}
	}
	CFRelease(value);
	return out;
}
*/
import "C"

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"github.com/miekg/dns"
)

type macResolver struct{}

var macResolverSlots = make(chan struct{}, 64)

func (macResolver) Resolve(ctx context.Context, name string, qtype uint16) ([]dns.RR, error) {
	select {
	case macResolverSlots <- struct{}{}:
		defer func() { <-macResolverSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	timeout := dnsTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	if timeout <= 0 {
		return nil, context.DeadlineExceeded
	}
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	var result C.fev_dns_result
	rc := C.fev_dns_query(cname, C.uint16_t(qtype), C.int(timeout.Milliseconds()), &result)
	defer C.free(unsafe.Pointer(result.buf))
	switch {
	case rc == 1:
		return nil, context.DeadlineExceeded
	case rc != 0:
		return nil, fmt.Errorf("system resolver error %d for %s", int(rc), name)
	}
	return parseRecords(C.GoBytes(unsafe.Pointer(result.buf), C.int(result.len)))
}

func parseRecords(buf []byte) ([]dns.RR, error) {
	var out []dns.RR
	for len(buf) > 0 {
		end := 0
		for end < len(buf) && buf[end] != 0 {
			end++
		}
		if end == len(buf) || len(buf) < end+1+10 {
			return nil, errors.New("system resolver returned a truncated record")
		}
		name := string(buf[:end])
		rest := buf[end+1:]
		rrtype := binary.NativeEndian.Uint16(rest[0:])
		class := binary.NativeEndian.Uint16(rest[2:])
		ttl := binary.NativeEndian.Uint32(rest[4:])
		rdlen := int(binary.NativeEndian.Uint16(rest[8:]))
		if len(rest) < 10+rdlen {
			return nil, errors.New("system resolver returned a truncated record")
		}
		hdr := dns.RR_Header{Name: dns.Fqdn(name), Rrtype: rrtype, Class: class, Ttl: ttl, Rdlength: uint16(rdlen)}
		rr, _, err := dns.UnpackRRWithHeader(hdr, rest[10:10+rdlen], 0)
		if err != nil {
			return nil, fmt.Errorf("system resolver returned a record we cannot read: %w", err)
		}
		out = append(out, rr)
		buf = rest[10+rdlen:]
	}
	return out, nil
}

func (macResolver) SearchDomains() []string {
	joined := C.fev_search_domains()
	if joined == nil {
		return nil
	}
	defer C.free(unsafe.Pointer(joined))
	return strings.Fields(C.GoString(joined))
}
