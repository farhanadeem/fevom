//go:build darwin && cgo

package trust

/*
#cgo CFLAGS: -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <stdlib.h>
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>

static OSStatus fev_keychain_certs(const char *path, CFArrayRef *out) {
	SecKeychainRef kc = NULL;
	OSStatus st = SecKeychainOpen(path, &kc);
	if (st != 0) {
		return st;
	}
	const void *one[1] = { kc };
	CFArrayRef list = CFArrayCreate(NULL, one, 1, &kCFTypeArrayCallBacks);
	const void *keys[] = { kSecClass, kSecMatchSearchList, kSecMatchLimit, kSecReturnRef };
	const void *values[] = { kSecClassCertificate, list, kSecMatchLimitAll, kCFBooleanTrue };
	CFDictionaryRef q = CFDictionaryCreate(NULL, keys, values, 4, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFTypeRef result = NULL;
	st = SecItemCopyMatching(q, &result);
	CFRelease(q);
	CFRelease(list);
	CFRelease(kc);
	*out = (CFArrayRef)result;
	return st;
}

static CFDataRef fev_keychain_der(CFArrayRef certs, CFIndex i) {
	return SecCertificateCopyData((SecCertificateRef)CFArrayGetValueAtIndex(certs, i));
}

static CFArrayRef fev_trust_synth(int kind) {
	CFStringRef kPolicy = CFStringCreateWithCString(NULL, "kSecTrustSettingsPolicy", kCFStringEncodingUTF8);
	CFStringRef kPolicyString = CFStringCreateWithCString(NULL, "kSecTrustSettingsPolicyString", kCFStringEncodingUTF8);
	CFStringRef kResult = CFStringCreateWithCString(NULL, "kSecTrustSettingsResult", kCFStringEncodingUTF8);
	SecPolicyRef ssl = SecPolicyCreateSSL(true, NULL);
	SecPolicyRef basic = SecPolicyCreateBasicX509();
	CFMutableArrayRef a = CFArrayCreateMutable(NULL, 0, &kCFTypeArrayCallBacks);
	#define DICT(var) CFMutableDictionaryRef var = CFDictionaryCreateMutable(NULL, 0, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks)
	#define RESULT(var, n) do { SInt32 r = (n); CFNumberRef num = CFNumberCreate(NULL, kCFNumberSInt32Type, &r); CFDictionarySetValue(var, kResult, num); CFRelease(num); } while (0)
	switch (kind) {
	case 1: { DICT(d); RESULT(d, 1); CFArrayAppendValue(a, d); CFRelease(d); break; }
	case 2: { DICT(d); CFDictionarySetValue(d, kPolicy, ssl); RESULT(d, 1); CFArrayAppendValue(a, d); CFRelease(d); break; }
	case 3: { DICT(d); CFDictionarySetValue(d, kPolicy, basic); RESULT(d, 1); CFArrayAppendValue(a, d); CFRelease(d); break; }
	case 4: { DICT(d); CFDictionarySetValue(d, kPolicyString, CFSTR("example.com")); RESULT(d, 1); CFArrayAppendValue(a, d); CFRelease(d); break; }
	case 5: { DICT(d); RESULT(d, 3); CFArrayAppendValue(a, d); CFRelease(d); break; }
	case 6: {
		DICT(d); CFDictionarySetValue(d, kPolicy, basic); RESULT(d, 3); CFArrayAppendValue(a, d); CFRelease(d);
		DICT(e); CFDictionarySetValue(e, kPolicy, ssl); RESULT(e, 2); CFArrayAppendValue(a, e); CFRelease(e);
		break;
	}
	case 7: { DICT(d); CFArrayAppendValue(a, d); CFRelease(d); break; }
	}
	CFRelease(ssl);
	CFRelease(basic);
	CFRelease(kPolicy);
	CFRelease(kPolicyString);
	CFRelease(kResult);
	return a;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

func keychainCertificates(path string) ([][]byte, error) {
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))
	var certs C.CFArrayRef
	st := C.fev_keychain_certs(p, &certs)
	if st == C.errSecItemNotFound {
		return nil, nil
	}
	if st != 0 {
		return nil, fmt.Errorf("the keychain %s: OSStatus %d", path, int(st))
	}
	defer C.CFRelease(C.CFTypeRef(certs))
	var out [][]byte
	for i := 0; i < int(C.CFArrayGetCount(certs)); i++ {
		if der := cfBytes(C.fev_keychain_der(certs, C.CFIndex(i))); der != nil {
			out = append(out, der)
		}
	}
	return out, nil
}

func synthSettings(kind int) []Setting {
	a := C.fev_trust_synth(C.int(kind))
	defer C.CFRelease(C.CFTypeRef(a))
	return convert(a)
}
