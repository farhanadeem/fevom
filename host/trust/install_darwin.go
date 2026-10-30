//go:build darwin && cgo

package trust

/*
#cgo CFLAGS: -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <stdlib.h>
#include <string.h>
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>

static OSStatus fev_kc_open(const char *path, SecKeychainRef *kc) {
	if (path == NULL || path[0] == 0) {
		return SecKeychainCopyDefault(kc);
	}
	return SecKeychainOpen(path, kc);
}

static SecCertificateRef fev_cert(const unsigned char *der, long n) {
	CFDataRef d = CFDataCreate(NULL, der, n);
	SecCertificateRef c = SecCertificateCreateWithData(NULL, d);
	CFRelease(d);
	return c;
}

static OSStatus fev_cert_add(const char *path, const unsigned char *der, long n) {
	SecCertificateRef c = fev_cert(der, n);
	if (c == NULL) return errSecParam;
	SecKeychainRef kc = NULL;
	OSStatus st = fev_kc_open(path, &kc);
	if (st) { CFRelease(c); return st; }
	const void *k[] = {kSecClass, kSecValueRef, kSecUseKeychain};
	const void *v[] = {kSecClassCertificate, c, kc};
	CFDictionaryRef a = CFDictionaryCreate(NULL, k, v, 3, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	st = SecItemAdd(a, NULL);
	CFRelease(a); CFRelease(kc); CFRelease(c);
	return st == errSecDuplicateItem ? 0 : st;
}

static OSStatus fev_trust_ssl(const unsigned char *der, long n) {
	SecCertificateRef c = fev_cert(der, n);
	if (c == NULL) return errSecParam;
	SecPolicyRef ssl = SecPolicyCreateSSL(true, NULL);
	SInt32 r = kSecTrustSettingsResultTrustRoot;
	CFNumberRef num = CFNumberCreate(NULL, kCFNumberSInt32Type, &r);
	const void *k[] = {kSecTrustSettingsPolicy, kSecTrustSettingsResult};
	const void *v[] = {ssl, num};
	CFDictionaryRef d = CFDictionaryCreate(NULL, k, v, 2, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	const void *one[1] = {d};
	CFArrayRef settings = CFArrayCreate(NULL, one, 1, &kCFTypeArrayCallBacks);
	OSStatus st = SecTrustSettingsSetTrustSettings(c, kSecTrustSettingsDomainUser, settings);
	CFRelease(settings); CFRelease(d); CFRelease(num); CFRelease(ssl); CFRelease(c);
	return st;
}

static OSStatus fev_untrust(const unsigned char *der, long n) {
	SecCertificateRef c = fev_cert(der, n);
	if (c == NULL) return errSecParam;
	OSStatus st = SecTrustSettingsRemoveTrustSettings(c, kSecTrustSettingsDomainUser);
	CFRelease(c);
	return st;
}

static OSStatus fev_cert_delete(const char *path, const unsigned char *der, long n) {
	SecCertificateRef c = fev_cert(der, n);
	if (c == NULL) return errSecParam;
	SecKeychainRef kc = NULL;
	OSStatus st = fev_kc_open(path, &kc);
	if (st) { CFRelease(c); return st; }
	const void *one[1] = {c};
	CFArrayRef items = CFArrayCreate(NULL, one, 1, &kCFTypeArrayCallBacks);
	const void *kcs[1] = {kc};
	CFArrayRef list = CFArrayCreate(NULL, kcs, 1, &kCFTypeArrayCallBacks);
	const void *k[] = {kSecClass, kSecMatchItemList, kSecMatchSearchList};
	const void *v[] = {kSecClassCertificate, items, list};
	CFDictionaryRef q = CFDictionaryCreate(NULL, k, v, 3, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	st = SecItemDelete(q);
	CFRelease(q); CFRelease(list); CFRelease(items); CFRelease(kc); CFRelease(c);
	return st;
}

static int fev_cert_present(const char *path, const unsigned char *der, long n, OSStatus *st) {
	SecCertificateRef c = fev_cert(der, n);
	if (c == NULL) { *st = errSecParam; return 0; }
	CFStringRef label = SecCertificateCopySubjectSummary(c);
	CFRelease(c);
	if (label == NULL) { *st = errSecParam; return 0; }
	SecKeychainRef kc = NULL;
	CFArrayRef list = NULL;
	if (path != NULL && path[0] != 0) {
		*st = SecKeychainOpen(path, &kc);
		if (*st) { CFRelease(label); return 0; }
		list = CFArrayCreate(NULL, (const void **)&kc, 1, &kCFTypeArrayCallBacks);
	}
	const void *k[5] = {kSecClass, kSecAttrLabel, kSecMatchLimit, kSecReturnData, kSecMatchSearchList};
	const void *v[5] = {kSecClassCertificate, label, kSecMatchLimitAll, kCFBooleanTrue, list};
	CFDictionaryRef q = CFDictionaryCreate(NULL, k, v, list ? 5 : 4, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFTypeRef r = NULL;
	*st = SecItemCopyMatching(q, &r);
	CFRelease(q); CFRelease(label);
	if (list) CFRelease(list);
	if (kc) CFRelease(kc);
	int found = 0;
	if (*st == 0 && r != NULL && CFGetTypeID(r) == CFArrayGetTypeID()) {
		for (CFIndex i = 0; i < CFArrayGetCount(r); i++) {
			CFDataRef d = CFArrayGetValueAtIndex(r, i);
			if (CFGetTypeID(d) == CFDataGetTypeID() && CFDataGetLength(d) == n && memcmp(CFDataGetBytePtr(d), der, n) == 0) {
				found = 1;
			}
		}
	}
	if (r) CFRelease(r);
	if (*st == errSecItemNotFound) *st = 0;
	return found;
}
*/
import "C"

import (
	"bytes"
	"errors"
	"fmt"
	"unsafe"

	"fev/proto/daemon"
)

const (
	errAuthorizationCanceled            = -60006
	errAuthorizationInteractionNotAllow = -60007
)

var trustSSL = func(der []byte) int {
	p, n := derArgs(der)
	return int(C.fev_trust_ssl(p, n))
}

var ErrDeclined = errors.New("the trust dialog was cancelled")

var ErrNoGUI = errors.New("the trust dialog needs the Mac's own login session (not SSH)")

func derArgs(der []byte) (*C.uchar, C.long) {
	return (*C.uchar)(unsafe.Pointer(&der[0])), C.long(len(der))
}

func Install(der []byte, keychain string) error {
	if len(der) == 0 {
		return errors.New("no certificate")
	}
	ck := cstr(keychain)
	defer freeC(ck)
	p, n := derArgs(der)
	keychainMu.Lock()
	defer keychainMu.Unlock()
	if st := C.fev_cert_add(ck, p, n); st != 0 {
		return fmt.Errorf("add the root to the keychain: OSStatus %d", int(st))
	}
	switch st := trustSSL(der); st {
	case 0:
		return nil
	case errAuthorizationCanceled:
		return ErrDeclined
	case errAuthorizationInteractionNotAllow:
		return ErrNoGUI
	default:
		return fmt.Errorf("trust the root: OSStatus %d", int(st))
	}
}

func Remove(der []byte, keychain string) error {
	if len(der) == 0 {
		return errors.New("no certificate")
	}
	ck := cstr(keychain)
	defer freeC(ck)
	p, n := derArgs(der)
	keychainMu.Lock()
	defer keychainMu.Unlock()
	switch st := C.fev_untrust(p, n); st {
	case 0, errSecNoTrustSettings, errSecItemNotFound:
	case errAuthorizationCanceled:
		return ErrDeclined
	case errAuthorizationInteractionNotAllow:
		return ErrNoGUI
	default:
		return fmt.Errorf("remove the root's trust: OSStatus %d", int(st))
	}
	if st := C.fev_cert_delete(ck, p, n); st != 0 && st != errSecItemNotFound {
		return fmt.Errorf("remove the root from the keychain: OSStatus %d", int(st))
	}
	return nil
}

func State(der []byte, keychain string) (string, error) {
	if len(der) == 0 {
		return daemon.TrustAbsent, nil
	}
	admin, err := domainEntries(domainAdmin)
	if err != nil {
		return "", err
	}
	user, err := domainEntries(domainUser)
	if err != nil {
		return "", err
	}
	for _, c := range choose(admin, user) {
		if bytes.Equal(c.Raw, der) {
			return daemon.TrustTrusted, nil
		}
	}
	ck := cstr(keychain)
	defer freeC(ck)
	p, n := derArgs(der)
	var st C.OSStatus
	found := C.fev_cert_present(ck, p, n, &st)
	if st != 0 {
		return "", fmt.Errorf("look for the root in the keychain: OSStatus %d", int(st))
	}
	if found != 0 {
		return daemon.TrustUntrusted, nil
	}
	return daemon.TrustAbsent, nil
}
