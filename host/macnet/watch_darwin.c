#include "watch_darwin.h"

#include <CoreFoundation/CoreFoundation.h>
#include <SystemConfiguration/SystemConfiguration.h>
#include <dispatch/dispatch.h>
#include <stdlib.h>
#include <string.h>

extern void fevMacnetChanged(char *keys);

static SCDynamicStoreRef store;
static dispatch_queue_t queue;

static void changed(SCDynamicStoreRef s, CFArrayRef keys, void *info) {
	(void)s;
	(void)info;
	CFIndex n = CFArrayGetCount(keys);
	size_t cap = 1, len = 0;
	char *out = malloc(cap);
	if (!out) return;
	out[0] = 0;
	for (CFIndex i = 0; i < n; i++) {
		char k[256];
		if (!CFStringGetCString(CFArrayGetValueAtIndex(keys, i), k, sizeof k, kCFStringEncodingUTF8)) continue;
		size_t kl = strlen(k);
		char *grown = realloc(out, cap + kl + 1);
		if (!grown) break;
		out = grown;
		cap += kl + 1;
		memcpy(out + len, k, kl);
		len += kl;
		out[len++] = '\n';
		out[len] = 0;
	}
	fevMacnetChanged(out);
	free(out);
}

int fev_macnet_watch(void) {
	if (store) return -2;
	store = SCDynamicStoreCreate(NULL, CFSTR("fev macnet"), changed, NULL);
	if (!store) return -1;
	CFStringRef keys[] = {
		CFSTR("State:/Network/Global/IPv4"),
		CFSTR("State:/Network/Global/IPv6"),
		CFSTR("State:/Network/Global/DNS"),
		CFSTR("State:/Network/Global/Proxies"),
	};
	CFStringRef pats[] = {
		CFSTR("State:/Network/Interface/.*/IPv4"),
		CFSTR("State:/Network/Interface/.*/IPv6"),
		CFSTR("State:/Network/Interface/.*/NAT64"),
	};
	CFArrayRef ka = CFArrayCreate(NULL, (const void **)keys, 4, &kCFTypeArrayCallBacks);
	CFArrayRef pa = CFArrayCreate(NULL, (const void **)pats, 3, &kCFTypeArrayCallBacks);
	Boolean ok = ka && pa && SCDynamicStoreSetNotificationKeys(store, ka, pa);
	if (ka) CFRelease(ka);
	if (pa) CFRelease(pa);
	if (!ok) {
		CFRelease(store);
		store = NULL;
		return -3;
	}
	queue = dispatch_queue_create("fev.macnet", DISPATCH_QUEUE_SERIAL);
	if (!SCDynamicStoreSetDispatchQueue(store, queue)) {
		CFRelease(store);
		store = NULL;
		dispatch_release(queue);
		queue = NULL;
		return -4;
	}
	return 0;
}

void fev_macnet_unwatch(void) {
	if (!store) return;
	SCDynamicStoreSetDispatchQueue(store, NULL);
	dispatch_sync(queue, ^{});
	CFRelease(store);
	store = NULL;
	dispatch_release(queue);
	queue = NULL;
}
