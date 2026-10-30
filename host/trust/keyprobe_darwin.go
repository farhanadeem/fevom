//go:build darwin && cgo

package trust

/*
#cgo CFLAGS: -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <stdlib.h>
#include <string.h>
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>


static CFDataRef fev_probe_external(SecKeyRef key) {
	Boolean was = true;
	SecKeychainGetUserInteractionAllowed(&was);
	SecKeychainSetUserInteractionAllowed(false);
	CFDataRef d = SecKeyCopyExternalRepresentation(key, NULL);
	SecKeychainSetUserInteractionAllowed(was);
	return d;
}

static OSStatus fev_probe_export(SecKeyRef key, int which, CFDataRef *out) {
	SecExternalFormat fmts[3] = {kSecFormatWrappedPKCS8, kSecFormatPKCS12, kSecFormatOpenSSL};
	SecItemImportExportKeyParameters params;
	memset(&params, 0, sizeof params);
	params.version = SEC_KEY_IMPORT_EXPORT_PARAMS_VERSION;
	CFStringRef pass = CFSTR("fev-export-probe");
	params.passphrase = pass;
	Boolean was = true;
	SecKeychainGetUserInteractionAllowed(&was);
	SecKeychainSetUserInteractionAllowed(false);
	OSStatus st = SecItemExport(key, fmts[which], 0, &params, out);
	SecKeychainSetUserInteractionAllowed(was);
	return st;
}

static OSStatus fev_probe_raw(const char *path, const char *label, CFDataRef *out) {
	SecKeychainRef kc = NULL;
	OSStatus st = SecKeychainOpen(path, &kc);
	if (st) return st;
	CFStringRef l = CFStringCreateWithCString(NULL, label, kCFStringEncodingUTF8);
	CFArrayRef list = CFArrayCreate(NULL, (const void **)&kc, 1, &kCFTypeArrayCallBacks);
	const void *k[] = {kSecClass, kSecAttrKeyClass, kSecAttrLabel, kSecMatchSearchList, kSecReturnData};
	const void *v[] = {kSecClassKey, kSecAttrKeyClassPrivate, l, list, kCFBooleanTrue};
	CFDictionaryRef q = CFDictionaryCreate(NULL, k, v, 5, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	Boolean was = true;
	SecKeychainGetUserInteractionAllowed(&was);
	SecKeychainSetUserInteractionAllowed(false);
	CFTypeRef r = NULL;
	st = SecItemCopyMatching(q, &r);
	SecKeychainSetUserInteractionAllowed(was);
	CFRelease(q); CFRelease(list); CFRelease(l); CFRelease(kc);
	*out = (st == 0 && r && CFGetTypeID(r) == CFDataGetTypeID()) ? (CFDataRef)r : NULL;
	if (*out == NULL && r) CFRelease(r);
	return st;
}

static OSStatus fev_probe_acl(SecKeyRef key, char *buf, int max) {
	buf[0] = 0;
	SecAccessRef access = NULL;
	OSStatus st = SecKeychainItemCopyAccess((SecKeychainItemRef)key, &access);
	if (st) return st;
	CFArrayRef acls = NULL;
	st = SecAccessCopyACLList(access, &acls);
	if (st) { CFRelease(access); return st; }
	int used = 0;
	for (CFIndex i = 0; i < CFArrayGetCount(acls); i++) {
		SecACLRef acl = (SecACLRef)CFArrayGetValueAtIndex(acls, i);
		CFArrayRef apps = NULL;
		CFStringRef desc = NULL;
		SecKeychainPromptSelector prompt = 0;
		SecACLCopyContents(acl, &apps, &desc, &prompt);
		if (apps == NULL) {
			used += snprintf(buf + used, max - used, "any");
		} else {
			used += snprintf(buf + used, max - used, "apps=%ld", (long)CFArrayGetCount(apps));
			CFRelease(apps);
		}
		if (desc) CFRelease(desc);
		CFArrayRef auths = SecACLCopyAuthorizations(acl);
		for (CFIndex j = 0; auths && j < CFArrayGetCount(auths); j++) {
			char a[128];
			CFStringGetCString(CFArrayGetValueAtIndex(auths, j), a, sizeof a, kCFStringEncodingUTF8);
			used += snprintf(buf + used, max - used, " %s", a);
		}
		if (auths) CFRelease(auths);
		used += snprintf(buf + used, max - used, "\n");
		if (used >= max) break;
	}
	CFRelease(acls); CFRelease(access);
	return 0;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

type exportAttempt struct {
	Way    string
	Status int
	Data   []byte
}

func probeExport(s *keySigner, keychain, label string) []exportAttempt {
	keychainMu.Lock()
	defer keychainMu.Unlock()
	var out []exportAttempt
	ext := C.fev_probe_external(s.key)
	out = append(out, exportAttempt{Way: "SecKeyCopyExternalRepresentation", Data: cfBytes(ext)})
	for i, name := range []string{"SecItemExport(wrapped PKCS#8)", "SecItemExport(PKCS#12)", "SecItemExport(OpenSSL)"} {
		var d C.CFDataRef
		st := C.fev_probe_export(s.key, C.int(i), &d)
		out = append(out, exportAttempt{Way: name, Status: int(st), Data: cfBytes(d)})
	}
	ck, cl := C.CString(keychain), C.CString(label)
	defer C.free(unsafe.Pointer(ck))
	defer C.free(unsafe.Pointer(cl))
	var d C.CFDataRef
	st := C.fev_probe_raw(ck, cl, &d)
	out = append(out, exportAttempt{Way: "SecItemCopyMatching(kSecReturnData)", Status: int(st), Data: cfBytes(d)})
	return out
}

func probeACL(s *keySigner) (string, error) {
	keychainMu.Lock()
	defer keychainMu.Unlock()
	var buf [4096]C.char
	if st := C.fev_probe_acl(s.key, &buf[0], C.int(len(buf))); st != 0 {
		return "", fmt.Errorf("read the access list: OSStatus %d", int(st))
	}
	return C.GoString(&buf[0]), nil
}
