//go:build darwin && cgo

package trust

/*
#cgo CFLAGS: -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <stdlib.h>
#include <string.h>
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>

typedef struct {
	int policy, ssl, policyString, hasResult;
	int result;
} fev_trust_setting;

static CFStringRef fev_str(const char *s) {
	return CFStringCreateWithCString(NULL, s, kCFStringEncodingUTF8);
}

static int fev_policy_is_ssl(SecPolicyRef p) {
	CFDictionaryRef props = SecPolicyCopyProperties(p);
	if (props == NULL) {
		return 0;
	}
	CFTypeRef oid = CFDictionaryGetValue(props, kSecPolicyOid);
	int ssl = oid != NULL && CFEqual(oid, kSecPolicyAppleSSL);
	CFRelease(props);
	return ssl;
}

static int fev_trust_convert(CFArrayRef settings, fev_trust_setting *out, int max) {
	CFStringRef kPolicy = fev_str("kSecTrustSettingsPolicy");
	CFStringRef kPolicyString = fev_str("kSecTrustSettingsPolicyString");
	CFStringRef kResult = fev_str("kSecTrustSettingsResult");
	CFIndex n = CFArrayGetCount(settings);
	for (CFIndex i = 0; i < n && i < max; i++) {
		fev_trust_setting *s = &out[i];
		memset(s, 0, sizeof *s);
		CFTypeRef d = CFArrayGetValueAtIndex(settings, i);
		if (d == NULL || CFGetTypeID(d) != CFDictionaryGetTypeID()) {
			s->policy = 1;
			continue;
		}
		const void *v = NULL;
		if (CFDictionaryGetValueIfPresent((CFDictionaryRef)d, kPolicy, &v)) {
			s->policy = 1;
			if (v != NULL && CFGetTypeID(v) == SecPolicyGetTypeID()) {
				s->ssl = fev_policy_is_ssl((SecPolicyRef)v);
			}
		}
		if (CFDictionaryContainsKey((CFDictionaryRef)d, kPolicyString)) {
			s->policyString = 1;
		}
		if (CFDictionaryGetValueIfPresent((CFDictionaryRef)d, kResult, &v) && v != NULL && CFGetTypeID(v) == CFNumberGetTypeID()) {
			SInt32 r = 0;
			if (CFNumberGetValue((CFNumberRef)v, kCFNumberSInt32Type, &r)) {
				s->hasResult = 1;
				s->result = r;
			}
		}
	}
	CFRelease(kPolicy);
	CFRelease(kPolicyString);
	CFRelease(kResult);
	return (int)n;
}

static OSStatus fev_trust_certs(int domain, CFArrayRef *out) {
	return SecTrustSettingsCopyCertificates((SecTrustSettingsDomain)domain, out);
}

static OSStatus fev_trust_settings(SecCertificateRef c, int domain, CFArrayRef *out) {
	return SecTrustSettingsCopyTrustSettings(c, (SecTrustSettingsDomain)domain, out);
}

static CFDataRef fev_cert_der(CFArrayRef certs, CFIndex i) {
	return SecCertificateCopyData((SecCertificateRef)CFArrayGetValueAtIndex(certs, i));
}

static SecCertificateRef fev_cert_at(CFArrayRef certs, CFIndex i) {
	return (SecCertificateRef)CFArrayGetValueAtIndex(certs, i);
}
*/
import "C"

import (
	"crypto/x509"
	"fmt"
	"unsafe"
)

const (
	domainUser  = 0
	domainAdmin = 1
)

const errSecNoTrustSettings = -25263

const maxSettings = 64

func Roots() (certs []*x509.Certificate, err error) {
	admin, err := domainEntries(domainAdmin)
	if err != nil {
		return nil, err
	}
	user, err := domainEntries(domainUser)
	if err != nil {
		return nil, err
	}
	return choose(admin, user), nil
}

func domainEntries(domain int) ([]entry, error) {
	var certs C.CFArrayRef
	st := C.fev_trust_certs(C.int(domain), &certs)
	if st == errSecNoTrustSettings {
		return nil, nil
	}
	if st != 0 {
		return nil, fmt.Errorf("SecTrustSettingsCopyCertificates (domain %d): OSStatus %d", domain, int(st))
	}
	defer C.CFRelease(C.CFTypeRef(certs))
	n := int(C.CFArrayGetCount(certs))
	out := make([]entry, 0, n)
	for i := 0; i < n; i++ {
		der := cfBytes(C.fev_cert_der(certs, C.CFIndex(i)))
		if der == nil {
			continue
		}
		var settings C.CFArrayRef
		if st := C.fev_trust_settings(C.fev_cert_at(certs, C.CFIndex(i)), C.int(domain), &settings); st != 0 {
			continue
		}
		out = append(out, entry{der: der, settings: convert(settings)})
		C.CFRelease(C.CFTypeRef(settings))
	}
	return out, nil
}

func convert(settings C.CFArrayRef) []Setting {
	buf := make([]C.fev_trust_setting, maxSettings)
	n := int(C.fev_trust_convert(settings, &buf[0], maxSettings))
	if n > maxSettings {
		n = maxSettings
	}
	out := make([]Setting, n)
	for i := range out {
		s := buf[i]
		out[i] = Setting{Policy: s.policy != 0, PolicySSL: s.ssl != 0, PolicyString: s.policyString != 0, HasResult: s.hasResult != 0, Result: int32(s.result)}
	}
	return out
}

func cfBytes(d C.CFDataRef) []byte {
	if d == 0 {
		return nil
	}
	defer C.CFRelease(C.CFTypeRef(d))
	n := C.CFDataGetLength(d)
	if n == 0 {
		return []byte{}
	}
	return C.GoBytes(unsafe.Pointer(C.CFDataGetBytePtr(d)), C.int(n))
}
