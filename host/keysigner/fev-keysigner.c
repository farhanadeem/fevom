#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>
#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#pragma clang diagnostic ignored "-Wdeprecated-declarations"

#define MAXREQ 65536
#define NAME "fev-keysigner 1"

static SecKeyRef cur;

static int readfull(int fd, uint8_t *b, size_t n) {
	while (n > 0) {
		ssize_t r = read(fd, b, n);
		if (r < 0 && errno == EINTR) continue;
		if (r <= 0) return -1;
		b += r;
		n -= (size_t)r;
	}
	return 0;
}

static int writefull(int fd, const uint8_t *b, size_t n) {
	while (n > 0) {
		ssize_t r = write(fd, b, n);
		if (r < 0 && errno == EINTR) continue;
		if (r <= 0) return -1;
		b += r;
		n -= (size_t)r;
	}
	return 0;
}

static uint8_t *out;
static size_t outn, outcap;

static void put(const void *p, size_t n) {
	if (outn + n > outcap) {
		outcap = (outn + n) * 2;
		out = realloc(out, outcap);
		if (out == NULL) _exit(3);
	}
	memcpy(out + outn, p, n);
	outn += n;
}
static void put16(uint16_t v) { uint8_t b[2] = {v >> 8, v}; put(b, 2); }
static void put32(uint32_t v) { uint8_t b[4] = {v >> 24, v >> 16, v >> 8, v}; put(b, 4); }
static void putf(const void *p, size_t n) { put16((uint16_t)n); put(p, n); }
static void putcf(CFDataRef d) {
	if (d == NULL) { put32(0); return; }
	put32((uint32_t)CFDataGetLength(d));
	put(CFDataGetBytePtr(d), (size_t)CFDataGetLength(d));
}

static int reply(OSStatus st) {
	uint8_t h[8];
	uint32_t n = (uint32_t)(outn - 8);
	h[0] = n >> 24; h[1] = n >> 16; h[2] = n >> 8; h[3] = n;
	uint32_t s = (uint32_t)st;
	h[4] = s >> 24; h[5] = s >> 16; h[6] = s >> 8; h[7] = s;
	memcpy(out, h, 8);
	return writefull(1, out, outn);
}

static const uint8_t *in, *inend;

static int field(char *dst, size_t max, size_t *len) {
	if (inend - in < 2) return -1;
	size_t n = ((size_t)in[0] << 8) | in[1];
	in += 2;
	if ((size_t)(inend - in) < n || n >= max) return -1;
	memcpy(dst, in, n);
	dst[n] = 0;
	in += n;
	if (len) *len = n;
	return 0;
}

static CFStringRef cfstr(const char *s) { return CFStringCreateWithCString(NULL, s, kCFStringEncodingUTF8); }

static Boolean quiet(void) {
	Boolean was = true;
	SecKeychainGetUserInteractionAllowed(&was);
	SecKeychainSetUserInteractionAllowed(false);
	return was;
}
static void restore(Boolean was) { SecKeychainSetUserInteractionAllowed(was); }

static OSStatus openkc(const char *path, SecKeychainRef *kc) {
	if (path[0] == 0) return SecKeychainCopyDefault(kc);
	return SecKeychainOpen(path, kc);
}

static void hex(const uint8_t *b, size_t n, char *dst) {
	static const char d[] = "0123456789abcdef";
	for (size_t i = 0; i < n; i++) {
		dst[2 * i] = d[b[i] >> 4];
		dst[2 * i + 1] = d[b[i] & 15];
	}
	dst[2 * n] = 0;
}

static void op_version(void) {
	putf(NAME, strlen(NAME));
	char h[2 * 64 + 1] = "";
	SecCodeRef code = NULL;
	SecStaticCodeRef st = NULL;
	CFDictionaryRef info = NULL;
	if (SecCodeCopySelf(kSecCSDefaultFlags, &code) == 0 && SecCodeCopyStaticCode(code, kSecCSDefaultFlags, &st) == 0 &&
	    SecCodeCopySigningInformation(st, kSecCSDefaultFlags, &info) == 0 && info) {
		CFDataRef u = CFDictionaryGetValue(info, kSecCodeInfoUnique);
		if (u && CFDataGetLength(u) <= 64) hex(CFDataGetBytePtr(u), (size_t)CFDataGetLength(u), h);
	}
	if (info) CFRelease(info);
	if (st) CFRelease(st);
	if (code) CFRelease(code);
	putf(h, strlen(h));
}

static OSStatus find(const char *path, const char *label) {
	if (cur) { CFRelease(cur); cur = NULL; }
	SecKeychainRef kc = NULL;
	OSStatus st = openkc(path, &kc);
	if (st) return st;
	CFStringRef l = cfstr(label);
	CFArrayRef list = CFArrayCreate(NULL, (const void **)&kc, 1, &kCFTypeArrayCallBacks);
	const void *k[] = {kSecClass, kSecAttrKeyClass, kSecAttrLabel, kSecMatchSearchList, kSecReturnRef, kSecReturnAttributes};
	const void *v[] = {kSecClassKey, kSecAttrKeyClassPrivate, l, list, kCFBooleanTrue, kCFBooleanTrue};
	CFDictionaryRef q = CFDictionaryCreate(NULL, k, v, 6, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFTypeRef r = NULL;
	Boolean was = quiet();
	st = SecItemCopyMatching(q, &r);
	restore(was);
	CFRelease(q); CFRelease(list); CFRelease(l); CFRelease(kc);
	if (st) return st;
	CFDataRef tag = NULL;
	if (CFGetTypeID(r) == CFDictionaryGetTypeID()) {
		const void *ref = CFDictionaryGetValue((CFDictionaryRef)r, kSecValueRef);
		if (ref) cur = (SecKeyRef)CFRetain(ref);
		const void *t = CFDictionaryGetValue((CFDictionaryRef)r, kSecAttrApplicationTag);
		if (t && CFGetTypeID(t) == CFDataGetTypeID()) tag = CFRetain(t);
	}
	CFRelease(r);
	if (cur == NULL) { if (tag) CFRelease(tag); return errSecItemNotFound; }
	SecKeyRef pub = SecKeyCopyPublicKey(cur);
	CFDataRef pd = pub ? SecKeyCopyExternalRepresentation(pub, NULL) : NULL;
	if (pub) CFRelease(pub);
	if (pd == NULL) { if (tag) CFRelease(tag); return errSecInvalidKeyRef; }
	if (tag) putf(CFDataGetBytePtr(tag), (size_t)CFDataGetLength(tag)); else put16(0);
	putf(CFDataGetBytePtr(pd), (size_t)CFDataGetLength(pd));
	if (tag) CFRelease(tag);
	CFRelease(pd);
	return 0;
}

static OSStatus keyaccess(CFStringRef label, int self, SecAccessRef *a) {
	OSStatus st;
	CFArrayRef apps;
	if (self) {
		SecTrustedApplicationRef me = NULL;
		st = SecTrustedApplicationCreateFromPath(NULL, &me);
		if (st) return st;
		apps = CFArrayCreate(NULL, (const void **)&me, 1, &kCFTypeArrayCallBacks);
		CFRelease(me);
	} else {
		apps = CFArrayCreate(NULL, NULL, 0, &kCFTypeArrayCallBacks);
	}
	st = SecAccessCreate(label, apps, a);
	CFRelease(apps);
	if (st) return st;
	CFArrayRef acls = NULL;
	st = SecAccessCopyACLList(*a, &acls);
	if (st) return st;
	const void *one[1] = {kSecACLAuthorizationSign};
	CFArrayRef sign = CFArrayCreate(NULL, one, 1, &kCFTypeArrayCallBacks);
	int narrowed = 0;
	for (CFIndex i = 0; i < CFArrayGetCount(acls) && !st; i++) {
		SecACLRef acl = (SecACLRef)CFArrayGetValueAtIndex(acls, i);
		CFArrayRef auths = SecACLCopyAuthorizations(acl);
		int signs = auths && CFArrayContainsValue(auths, CFRangeMake(0, CFArrayGetCount(auths)), kSecACLAuthorizationSign);
		if (auths) CFRelease(auths);
		if (!signs) continue;
		st = SecACLUpdateAuthorizations(acl, sign);
		if (!st && !self) {
			CFArrayRef was = NULL;
			CFStringRef desc = NULL;
			SecKeychainPromptSelector prompt = 0;
			SecACLCopyContents(acl, &was, &desc, &prompt);
			st = SecACLSetContents(acl, NULL, desc ? desc : label, prompt);
			if (was) CFRelease(was);
			if (desc) CFRelease(desc);
		}
		narrowed++;
	}
	CFRelease(sign);
	CFRelease(acls);
	if (!st && narrowed == 0) st = errSecInvalidACL;
	return st;
}

static OSStatus make(const char *path, const char *label, const char *tag, int self) {
	SecKeychainRef kc = NULL;
	OSStatus st = openkc(path, &kc);
	if (st) return st;
	CFStringRef l = cfstr(label);
	char pl[600];
	snprintf(pl, sizeof pl, "%s.pub", label);
	CFStringRef publ = cfstr(pl);
	CFDataRef t = CFDataCreate(NULL, (const UInt8 *)tag, (CFIndex)strlen(tag));
	SecAccessRef a = NULL;
	st = keyaccess(l, self, &a);
	if (st == 0) {
		int bits = 256;
		CFNumberRef nb = CFNumberCreate(NULL, kCFNumberIntType, &bits);
		const void *pk[] = {kSecAttrIsPermanent, kSecAttrIsExtractable, kSecAttrLabel, kSecAttrApplicationTag};
		const void *pv[] = {kCFBooleanTrue, kCFBooleanFalse, l, t};
		CFDictionaryRef priv = CFDictionaryCreate(NULL, pk, pv, 4, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
		const void *uk[] = {kSecAttrIsPermanent, kSecAttrLabel};
		const void *uv[] = {kCFBooleanTrue, publ};
		CFDictionaryRef pub = CFDictionaryCreate(NULL, uk, uv, 2, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
		const void *k[] = {kSecAttrKeyType, kSecAttrKeySizeInBits, kSecUseKeychain, kSecAttrLabel, kSecAttrAccess,
		                   kSecAttrIsExtractable, kSecAttrApplicationTag, kSecPrivateKeyAttrs, kSecPublicKeyAttrs};
		const void *v[] = {kSecAttrKeyTypeECSECPrimeRandom, nb, kc, l, a, kCFBooleanFalse, t, priv, pub};
		CFDictionaryRef attrs = CFDictionaryCreate(NULL, k, v, 9, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
		CFErrorRef e = NULL;
		Boolean was = quiet();
		SecKeyRef key = SecKeyCreateRandomKey(attrs, &e);
		restore(was);
		if (key) CFRelease(key);
		else st = e ? (OSStatus)CFErrorGetCode(e) : errSecInternalComponent;
		if (e) CFRelease(e);
		CFRelease(attrs); CFRelease(pub); CFRelease(priv); CFRelease(nb); CFRelease(a);
	}
	CFRelease(t); CFRelease(publ); CFRelease(l); CFRelease(kc);
	return st;
}

static OSStatus sign(const uint8_t *dg, size_t n) {
	if (cur == NULL) return errSecItemNotFound;
	if (n != 32) return errSecParam;
	CFDataRef d = CFDataCreate(NULL, dg, (CFIndex)n);
	CFErrorRef e = NULL;
	Boolean was = quiet();
	CFDataRef sig = SecKeyCreateSignature(cur, kSecKeyAlgorithmECDSASignatureDigestX962SHA256, d, &e);
	restore(was);
	CFRelease(d);
	OSStatus st = 0;
	if (sig) {
		put(CFDataGetBytePtr(sig), (size_t)CFDataGetLength(sig));
		CFRelease(sig);
	} else {
		st = e ? (OSStatus)CFErrorGetCode(e) : errSecInternalComponent;
	}
	if (e) CFRelease(e);
	return st;
}

static OSStatus del(const char *path, const char *label) {
	SecKeychainRef kc = NULL;
	OSStatus st = openkc(path, &kc);
	if (st) return st;
	CFArrayRef list = CFArrayCreate(NULL, (const void **)&kc, 1, &kCFTypeArrayCallBacks);
	char pl[600];
	snprintf(pl, sizeof pl, "%s.pub", label);
	const char *labels[2] = {label, pl};
	OSStatus first = errSecItemNotFound;
	Boolean was = quiet();
	for (int i = 0; i < 2; i++) {
		CFStringRef l = cfstr(labels[i]);
		const void *k[] = {kSecClass, kSecAttrLabel, kSecMatchSearchList};
		const void *v[] = {kSecClassKey, l, list};
		CFDictionaryRef q = CFDictionaryCreate(NULL, k, v, 3, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
		OSStatus s = SecItemDelete(q);
		if (i == 0) first = s;
		CFRelease(q); CFRelease(l);
	}
	restore(was);
	CFRelease(list); CFRelease(kc);
	if (cur) { CFRelease(cur); cur = NULL; }
	return first;
}

static OSStatus kcstatus(const char *path) {
	SecKeychainRef kc = NULL;
	OSStatus st = openkc(path, &kc);
	if (st) return st;
	SecKeychainStatus s = 0;
	st = SecKeychainGetStatus(kc, &s);
	CFRelease(kc);
	if (st == 0) put32(s);
	return st;
}

static void way(const char *name, OSStatus st, CFDataRef d) {
	putf(name, strlen(name));
	put32((uint32_t)st);
	putcf(d);
	if (d) CFRelease(d);
}

static OSStatus probe(void) {
	if (cur == NULL) return errSecItemNotFound;
	Boolean was = quiet();
	CFErrorRef e = NULL;
	CFDataRef d = SecKeyCopyExternalRepresentation(cur, &e);
	way("SecKeyCopyExternalRepresentation", d ? 0 : (e ? (OSStatus)CFErrorGetCode(e) : -1), d);
	if (e) CFRelease(e);
	SecExternalFormat fmts[3] = {kSecFormatWrappedPKCS8, kSecFormatPKCS12, kSecFormatOpenSSL};
	const char *names[3] = {"SecItemExport(wrapped PKCS#8)", "SecItemExport(PKCS#12)", "SecItemExport(OpenSSL)"};
	for (int i = 0; i < 3; i++) {
		SecItemImportExportKeyParameters p;
		memset(&p, 0, sizeof p);
		p.version = SEC_KEY_IMPORT_EXPORT_PARAMS_VERSION;
		p.passphrase = CFSTR("fev-export-probe");
		CFDataRef x = NULL;
		OSStatus s = SecItemExport(cur, fmts[i], 0, &p, &x);
		if (s != 0 && x != NULL) {
			CFRelease(x);
			x = NULL;
		}
		way(names[i], s, x);
	}
	const void *k[] = {kSecValueRef, kSecReturnData};
	const void *v[] = {cur, kCFBooleanTrue};
	CFDictionaryRef q = CFDictionaryCreate(NULL, k, v, 2, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFTypeRef r = NULL;
	OSStatus s = SecItemCopyMatching(q, &r);
	CFRelease(q);
	CFDataRef raw = (s == 0 && r && CFGetTypeID(r) == CFDataGetTypeID()) ? (CFDataRef)r : NULL;
	if (raw == NULL && r) CFRelease(r);
	way("SecItemCopyMatching(kSecReturnData)", s, raw);
	restore(was);
	return 0;
}

static OSStatus acltext(void) {
	if (cur == NULL) return errSecItemNotFound;
	SecAccessRef a = NULL;
	Boolean was = quiet();
	OSStatus st = SecKeychainItemCopyAccess((SecKeychainItemRef)cur, &a);
	restore(was);
	if (st) return st;
	CFArrayRef acls = NULL;
	st = SecAccessCopyACLList(a, &acls);
	if (st) { CFRelease(a); return st; }
	char line[4096];
	for (CFIndex i = 0; i < CFArrayGetCount(acls); i++) {
		SecACLRef acl = (SecACLRef)CFArrayGetValueAtIndex(acls, i);
		CFArrayRef apps = NULL;
		CFStringRef desc = NULL;
		SecKeychainPromptSelector prompt = 0;
		SecACLCopyContents(acl, &apps, &desc, &prompt);
		int n = apps == NULL ? snprintf(line, sizeof line, "any") : snprintf(line, sizeof line, "apps=%ld", (long)CFArrayGetCount(apps));
		if (apps) CFRelease(apps);
		CFArrayRef auths = SecACLCopyAuthorizations(acl);
		for (CFIndex j = 0; auths && j < CFArrayGetCount(auths) && n < 1000; j++) {
			char s[128];
			CFStringGetCString(CFArrayGetValueAtIndex(auths, j), s, sizeof s, kCFStringEncodingUTF8);
			n += snprintf(line + n, sizeof line - (size_t)n, " %s", s);
		}
		int partition = auths && CFArrayContainsValue(auths, CFRangeMake(0, CFArrayGetCount(auths)), kSecACLAuthorizationPartitionID);
		if (auths) CFRelease(auths);
		if (partition && desc) {
			char s[4096];
			if (CFStringGetCString(desc, s, sizeof s, kCFStringEncodingUTF8)) {
				n += snprintf(line + n, sizeof line - (size_t)n, " desc=%.3500s", s);
			} else {
				n += snprintf(line + n, sizeof line - (size_t)n, " desc-length=%ld", (long)CFStringGetLength(desc));
			}
		} else if (partition) {
			n += snprintf(line + n, sizeof line - (size_t)n, " no-desc");
		}
		if (desc) CFRelease(desc);
		line[n] = '\n';
		put(line, (size_t)n + 1);
	}
	CFRelease(acls);
	CFRelease(a);
	return 0;
}

int main(int argc, char **argv) {
	if (argc != 2 || strcmp(argv[1], "serve") != 0) {
		fprintf(stderr, "usage: fev-keysigner serve (a child of fevd; requests on standard input)\n");
		return 2;
	}
	static uint8_t req[MAXREQ];
	out = malloc(outcap = 4096);
	if (out == NULL) return 3;
	for (;;) {
		uint8_t h[4];
		if (readfull(0, h, 4)) _exit(0);
		uint32_t n = ((uint32_t)h[0] << 24) | ((uint32_t)h[1] << 16) | ((uint32_t)h[2] << 8) | h[3];
		if (n == 0 || n > MAXREQ || readfull(0, req, n)) return 1;
		in = req + 1;
		inend = req + n;
		outn = 8;
		static char kc[1024], label[512], tag[512], mode[16], dg[64];
		size_t dn = 0;
		OSStatus st;
		switch (req[0]) {
		case 'V': op_version(); st = 0; break;
		case 'F': st = field(kc, sizeof kc, NULL) || field(label, sizeof label, NULL) ? errSecParam : find(kc, label); break;
		case 'M':
			if (field(kc, sizeof kc, NULL) || field(label, sizeof label, NULL) || field(tag, sizeof tag, NULL) ||
			    field(mode, sizeof mode, NULL) || (strcmp(mode, "any") && strcmp(mode, "self"))) {
				st = errSecParam;
				break;
			}
			st = make(kc, label, tag, strcmp(mode, "self") == 0);
			if (st == 0) st = find(kc, label);
			break;
		case 'S': st = field(dg, sizeof dg, &dn) ? errSecParam : sign((const uint8_t *)dg, dn); break;
		case 'D': st = field(kc, sizeof kc, NULL) || field(label, sizeof label, NULL) ? errSecParam : del(kc, label); break;
		case 'K': st = field(kc, sizeof kc, NULL) ? errSecParam : kcstatus(kc); break;
		case 'P': st = probe(); break;
		case 'A': st = acltext(); break;
		default: st = errSecUnimplemented;
		}
		if (st) outn = 8;
		if (reply(st)) return 1;
	}
}
