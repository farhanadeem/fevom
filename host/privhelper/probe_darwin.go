//go:build darwin

package privhelper

/*
#include <dns_sd.h>
#include <sys/select.h>
#include <sys/time.h>
#include <arpa/inet.h>
#include <net/if.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct { char *buf; int cap; int len; } fev_acc;
typedef struct { fev_acc *a; int name; int kind; } fev_q;

static long fev_since(struct timeval *s) {
	struct timeval n; gettimeofday(&n, 0);
	return (n.tv_sec - s->tv_sec) * 1000000L + (n.tv_usec - s->tv_usec);
}

static void DNSSD_API fev_answer(DNSServiceRef sd, DNSServiceFlags flags, uint32_t ifi, DNSServiceErrorType err,
	const char *fullname, uint16_t rrtype, uint16_t rrclass, uint16_t rdlen, const void *rdata, uint32_t ttl, void *ctx) {
	fev_q *q = (fev_q *)ctx;
	fev_acc *a = q->a;
	if (err != kDNSServiceErr_NoError || !(flags & kDNSServiceFlagsAdd)) return;
	char ip[64] = "-", ifn[IF_NAMESIZE + 1] = "-";
	const char *t;
	switch (rrtype) {
	case kDNSServiceType_A:
		if (rdlen != 4) return;
		inet_ntop(AF_INET, rdata, ip, sizeof ip); t = "A"; break;
	case kDNSServiceType_AAAA:
		if (rdlen != 16) return;
		inet_ntop(AF_INET6, rdata, ip, sizeof ip); t = "AAAA"; break;
	case kDNSServiceType_SOA: t = "SOA"; break;
	case kDNSServiceType_NS: t = "NS"; break;
	default: return;
	}
	if (ifi != 0 && ifi != kDNSServiceInterfaceIndexLocalOnly && !if_indextoname(ifi, ifn)) strcpy(ifn, "-");
	int room = a->cap - a->len;
	if (room < 160) { a->len = a->cap; return; }
	a->len += snprintf(a->buf + a->len, room, "%d %d %s %s %s\n", q->name, q->kind, t, ip, ifn);
}

static int fev_probe(const char *names, int n, const int *kinds, int k, int ms, char *buf, int cap) {
	DNSServiceRef conn = NULL;
	int e = DNSServiceCreateConnection(&conn);
	if (e) return e;
	fev_acc a = { buf, cap, 0 };
	buf[0] = 0;
	int max = n * k * 2;
	fev_q *qs = calloc(max, sizeof *qs);
	DNSServiceRef *refs = calloc(max, sizeof *refs);
	if (!qs || !refs) { free(qs); free(refs); DNSServiceRefDeallocate(conn); return kDNSServiceErr_NoMemory; }
	int used = 0;
	const char *name = names;
	for (int i = 0; i < n && !e; i++, name += strlen(name) + 1) {
		for (int j = 0; j < k && !e; j++) {
			uint16_t types[2] = { kDNSServiceType_A, kDNSServiceType_AAAA };
			DNSServiceFlags fl = kDNSServiceFlagsShareConnection;
			if (kinds[j] == 1) { types[0] = kDNSServiceType_SOA; types[1] = kDNSServiceType_NS; }
			if (kinds[j] == 2) fl |= kDNSServiceFlagsForceMulticast;
			for (int t = 0; t < 2 && !e; t++) {
				qs[used].a = &a; qs[used].name = i; qs[used].kind = kinds[j];
				refs[used] = conn;
				e = DNSServiceQueryRecord(&refs[used], fl, 0, name, types[t], kDNSServiceClass_IN, fev_answer, &qs[used]);
				if (e) { refs[used] = NULL; break; }
				used++;
			}
		}
	}
	if (!e) {
		int fd = DNSServiceRefSockFD(conn);
		struct timeval start;
		gettimeofday(&start, 0);
		for (;;) {
			long left = (long)ms * 1000L - fev_since(&start);
			if (left <= 0) break;
			fd_set rs; FD_ZERO(&rs); FD_SET(fd, &rs);
			struct timeval tv = { left / 1000000L, left % 1000000L };
			int r = select(fd + 1, &rs, NULL, NULL, &tv);
			if (r < 0) { e = kDNSServiceErr_Unknown; break; }
			if (r == 0) continue;
			e = DNSServiceProcessResult(conn);
			if (e) break;
		}
	}
	for (int i = 0; i < used; i++) DNSServiceRefDeallocate(refs[i]);
	DNSServiceRefDeallocate(conn);
	free(qs);
	free(refs);
	if (!e && a.len >= cap) return -1;
	return e;
}
*/
import "C"

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

const probeBuf = 64 << 10

func probe(names []string, kinds []ProbeKind, d time.Duration) (map[string][]Answer, error) {
	out := map[string][]Answer{}
	if len(names) == 0 || len(kinds) == 0 {
		return out, nil
	}
	joined := strings.Join(names, "\x00") + "\x00"
	cnames := C.CString(joined)
	defer C.free(unsafe.Pointer(cnames))
	ks := make([]C.int, len(kinds))
	for i, k := range kinds {
		ks[i] = C.int(k)
	}
	buf := (*C.char)(C.malloc(probeBuf))
	defer C.free(unsafe.Pointer(buf))
	e := C.fev_probe(cnames, C.int(len(names)), &ks[0], C.int(len(ks)), C.int(d/time.Millisecond), buf, probeBuf)
	switch {
	case e == -1:
		return nil, fmt.Errorf("more answers than the probe keeps")
	case e != 0:
		return nil, fmt.Errorf("mDNSResponder: DNSServiceErrorType %d", int(e))
	}
	for _, line := range strings.Split(C.GoString(buf), "\n") {
		f := strings.Fields(line)
		if len(f) != 5 {
			continue
		}
		i, err1 := strconv.Atoi(f[0])
		k, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil || i < 0 || i >= len(names) {
			continue
		}
		a := Answer{Kind: ProbeKind(k), Type: f[2]}
		if f[3] != "-" {
			a.Addr = f[3]
		}
		if f[4] != "-" {
			a.Ifname = f[4]
		}
		out[names[i]] = append(out[names[i]], a)
	}
	return out, nil
}
