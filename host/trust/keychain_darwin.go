//go:build darwin && cgo

package trust

/*
#cgo CFLAGS: -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <stdlib.h>
#include <string.h>
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>

static CFStringRef fev_cfstr(const char *s) {
	return CFStringCreateWithCString(NULL, s, kCFStringEncodingUTF8);
}

static OSStatus fev_kc(const char *path, SecKeychainRef *kc) {
	if (path == NULL || path[0] == 0) {
		return SecKeychainCopyDefault(kc);
	}
	return SecKeychainOpen(path, kc);
}

static Boolean fev_quiet(void) {
	Boolean was = true;
	SecKeychainGetUserInteractionAllowed(&was);
	SecKeychainSetUserInteractionAllowed(false);
	return was;
}
static void fev_restore(Boolean was) { SecKeychainSetUserInteractionAllowed(was); }

static OSStatus fev_key_find(const char *path, const char *label, SecKeyRef *key, char *tag, int tagmax) {
	SecKeychainRef kc = NULL;
	OSStatus st = fev_kc(path, &kc);
	if (st) return st;
	Boolean was = fev_quiet();
	CFStringRef l = fev_cfstr(label);
	CFArrayRef list = CFArrayCreate(NULL, (const void **)&kc, 1, &kCFTypeArrayCallBacks);
	const void *k[] = {kSecClass, kSecAttrKeyClass, kSecAttrLabel, kSecMatchSearchList, kSecReturnRef, kSecReturnAttributes};
	const void *v[] = {kSecClassKey, kSecAttrKeyClassPrivate, l, list, kCFBooleanTrue, kCFBooleanTrue};
	CFDictionaryRef q = CFDictionaryCreate(NULL, k, v, 6, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFTypeRef r = NULL;
	st = SecItemCopyMatching(q, &r);
	CFRelease(q); CFRelease(list); CFRelease(l); CFRelease(kc);
	fev_restore(was);
	if (st) return st;
	tag[0] = 0;
	*key = NULL;
	if (CFGetTypeID(r) == CFDictionaryGetTypeID()) {
		CFDictionaryRef d = (CFDictionaryRef)r;
		const void *ref = CFDictionaryGetValue(d, kSecValueRef);
		if (ref != NULL) {
			*key = (SecKeyRef)CFRetain(ref);
		}
		const void *t = CFDictionaryGetValue(d, kSecAttrApplicationTag);
		if (t != NULL && CFGetTypeID(t) == CFDataGetTypeID()) {
			CFIndex n = CFDataGetLength((CFDataRef)t);
			if (n >= tagmax) n = tagmax - 1;
			memcpy(tag, CFDataGetBytePtr((CFDataRef)t), n);
			tag[n] = 0;
		}
	}
	CFRelease(r);
	return *key ? 0 : errSecItemNotFound;
}

static OSStatus fev_access(CFStringRef label, int bound, SecAccessRef *access) {
	OSStatus st;
	if (bound) {
		SecTrustedApplicationRef me = NULL;
		st = SecTrustedApplicationCreateFromPath(NULL, &me);
		if (st) return st;
		const void *apps[1] = {me};
		CFArrayRef list = CFArrayCreate(NULL, apps, 1, &kCFTypeArrayCallBacks);
		st = SecAccessCreate(label, list, access);
		CFRelease(list); CFRelease(me);
	} else {
		CFArrayRef none = CFArrayCreate(NULL, NULL, 0, &kCFTypeArrayCallBacks);
		st = SecAccessCreate(label, none, access);
		CFRelease(none);
	}
	if (st) return st;
	CFArrayRef acls = NULL;
	st = SecAccessCopyACLList(*access, &acls);
	if (st) return st;
	const void *signOnly[1] = {kSecACLAuthorizationSign};
	CFArrayRef sign = CFArrayCreate(NULL, signOnly, 1, &kCFTypeArrayCallBacks);
	int narrowed = 0;
	for (CFIndex i = 0; i < CFArrayGetCount(acls); i++) {
		SecACLRef acl = (SecACLRef)CFArrayGetValueAtIndex(acls, i);
		CFArrayRef auths = SecACLCopyAuthorizations(acl);
		int signs = auths && CFArrayContainsValue(auths, CFRangeMake(0, CFArrayGetCount(auths)), kSecACLAuthorizationSign);
		if (auths) CFRelease(auths);
		if (!signs) continue;
		st = SecACLUpdateAuthorizations(acl, sign);
		if (st) break;
		if (!bound) {
			CFArrayRef apps = NULL;
			CFStringRef desc = NULL;
			SecKeychainPromptSelector prompt = 0;
			SecACLCopyContents(acl, &apps, &desc, &prompt);
			st = SecACLSetContents(acl, NULL, desc ? desc : label, prompt);
			if (apps) CFRelease(apps);
			if (desc) CFRelease(desc);
			if (st) break;
		}
		narrowed++;
	}
	CFRelease(sign);
	CFRelease(acls);
	if (!st && narrowed == 0) st = errSecInvalidACL;
	return st;
}

static OSStatus fev_key_make(const char *path, const char *label, const char *tag, int bound, int misplace, long *code) {
	*code = 0;
	SecKeychainRef kc = NULL;
	OSStatus st = fev_kc(path, &kc);
	if (st) return st;
	Boolean was = fev_quiet();
	CFStringRef l = fev_cfstr(label);
	char pubLabel[512];
	snprintf(pubLabel, sizeof pubLabel, "%s.pub", label);
	CFStringRef pl = fev_cfstr(pubLabel);
	CFDataRef t = CFDataCreate(NULL, (const UInt8 *)tag, strlen(tag));
	SecAccessRef access = NULL;
	if (misplace) {
		CFArrayRef none = CFArrayCreate(NULL, NULL, 0, &kCFTypeArrayCallBacks);
		st = SecAccessCreate(l, none, &access);
		CFRelease(none);
		if (!st) {
			CFArrayRef acls = NULL;
			SecAccessCopyACLList(access, &acls);
			for (CFIndex i = 0; acls && i < CFArrayGetCount(acls); i++) {
				SecACLRef acl = (SecACLRef)CFArrayGetValueAtIndex(acls, i);
				CFArrayRef auths = SecACLCopyAuthorizations(acl);
				int change = auths && CFArrayContainsValue(auths, CFRangeMake(0, CFArrayGetCount(auths)), kSecACLAuthorizationChangeACL);
				if (auths) CFRelease(auths);
				if (change) continue;
				SecACLSetContents(acl, NULL, l, 0);
			}
			if (acls) CFRelease(acls);
		}
	} else {
		st = fev_access(l, bound, &access);
	}
	if (st) {
		CFRelease(t); CFRelease(pl); CFRelease(l); CFRelease(kc);
		fev_restore(was);
		return st;
	}
	int bits = 256;
	CFNumberRef nb = CFNumberCreate(NULL, kCFNumberIntType, &bits);
	const void *pk[] = {kSecAttrIsPermanent, kSecAttrIsExtractable, kSecAttrLabel, kSecAttrApplicationTag, kSecAttrAccess};
	const void *pv[] = {kCFBooleanTrue, kCFBooleanFalse, l, t, access};
	CFDictionaryRef priv = CFDictionaryCreate(NULL, pk, pv, misplace ? 5 : 4, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	const void *uk[] = {kSecAttrIsPermanent, kSecAttrLabel};
	const void *uv[] = {kCFBooleanTrue, pl};
	CFDictionaryRef pub = CFDictionaryCreate(NULL, uk, uv, 2, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	const void *k[] = {kSecAttrKeyType, kSecAttrKeySizeInBits, kSecUseKeychain, kSecAttrLabel, kSecAttrAccess, kSecAttrIsExtractable, kSecAttrApplicationTag, kSecPrivateKeyAttrs, kSecPublicKeyAttrs};
	const void *v[] = {kSecAttrKeyTypeECSECPrimeRandom, nb, kc, l, access, kCFBooleanFalse, t, priv, pub};
	CFDictionaryRef a;
	if (misplace) {
		const void *mk[] = {kSecAttrKeyType, kSecAttrKeySizeInBits, kSecUseKeychain, kSecAttrLabel, kSecAttrApplicationTag, kSecPrivateKeyAttrs, kSecPublicKeyAttrs};
		const void *mv[] = {kSecAttrKeyTypeECSECPrimeRandom, nb, kc, l, t, priv, pub};
		a = CFDictionaryCreate(NULL, mk, mv, 7, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	} else {
		a = CFDictionaryCreate(NULL, k, v, 9, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	}
	CFErrorRef e = NULL;
	SecKeyRef key = SecKeyCreateRandomKey(a, &e);
	if (key == NULL) {
		*code = e ? CFErrorGetCode(e) : -1;
		st = errSecInternalComponent;
	} else {
		CFRelease(key);
	}
	if (e) CFRelease(e);
	CFRelease(a); CFRelease(priv); CFRelease(pub); CFRelease(nb); CFRelease(access);
	CFRelease(t); CFRelease(pl); CFRelease(l); CFRelease(kc);
	fev_restore(was);
	return st;
}

static OSStatus fev_key_delete(const char *path, const char *label) {
	SecKeychainRef kc = NULL;
	OSStatus st = fev_kc(path, &kc);
	if (st) return st;
	Boolean was = fev_quiet();
	CFArrayRef list = CFArrayCreate(NULL, (const void **)&kc, 1, &kCFTypeArrayCallBacks);
	char pubLabel[512];
	snprintf(pubLabel, sizeof pubLabel, "%s.pub", label);
	const char *labels[2] = {label, pubLabel};
	OSStatus first = errSecItemNotFound;
	for (int i = 0; i < 2; i++) {
		CFStringRef l = fev_cfstr(labels[i]);
		const void *k[] = {kSecClass, kSecAttrLabel, kSecMatchSearchList};
		const void *v[] = {kSecClassKey, l, list};
		CFDictionaryRef q = CFDictionaryCreate(NULL, k, v, 3, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
		OSStatus s = SecItemDelete(q);
		if (i == 0) first = s;
		CFRelease(q); CFRelease(l);
	}
	CFRelease(list); CFRelease(kc);
	fev_restore(was);
	return first;
}

static unsigned char *fev_public_x963(SecKeyRef key, long *n) {
	SecKeyRef pub = SecKeyCopyPublicKey(key);
	if (pub == NULL) return NULL;
	CFDataRef d = SecKeyCopyExternalRepresentation(pub, NULL);
	CFRelease(pub);
	if (d == NULL) return NULL;
	*n = CFDataGetLength(d);
	unsigned char *out = malloc(*n);
	memcpy(out, CFDataGetBytePtr(d), *n);
	CFRelease(d);
	return out;
}

static unsigned char *fev_sign_digest(SecKeyRef key, const unsigned char *dg, long dn, long *n, long *code) {
	Boolean was = fev_quiet();
	CFDataRef in = CFDataCreate(NULL, dg, dn);
	CFErrorRef e = NULL;
	CFDataRef sig = SecKeyCreateSignature(key, kSecKeyAlgorithmECDSASignatureDigestX962SHA256, in, &e);
	CFRelease(in);
	fev_restore(was);
	if (!sig) {
		*code = e ? CFErrorGetCode(e) : -1;
		if (e) CFRelease(e);
		return NULL;
	}
	*n = CFDataGetLength(sig);
	unsigned char *out = malloc(*n);
	memcpy(out, CFDataGetBytePtr(sig), *n);
	CFRelease(sig);
	*code = 0;
	return out;
}

static void fev_release(CFTypeRef r) { if (r) CFRelease(r); }

static OSStatus fev_key_peek(const char *path, const char *label, char *tag, int tagmax) {
	SecKeychainRef kc = NULL;
	OSStatus st = fev_kc(path, &kc);
	if (st) return st;
	Boolean was = fev_quiet();
	CFStringRef l = fev_cfstr(label);
	CFArrayRef list = CFArrayCreate(NULL, (const void **)&kc, 1, &kCFTypeArrayCallBacks);
	const void *k[] = {kSecClass, kSecAttrKeyClass, kSecAttrLabel, kSecMatchSearchList, kSecReturnAttributes};
	const void *v[] = {kSecClassKey, kSecAttrKeyClassPrivate, l, list, kCFBooleanTrue};
	CFDictionaryRef q = CFDictionaryCreate(NULL, k, v, 5, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFTypeRef r = NULL;
	st = SecItemCopyMatching(q, &r);
	CFRelease(q); CFRelease(list); CFRelease(l); CFRelease(kc);
	fev_restore(was);
	if (st) return st;
	tag[0] = 0;
	if (CFGetTypeID(r) == CFDictionaryGetTypeID()) {
		const void *t = CFDictionaryGetValue((CFDictionaryRef)r, kSecAttrApplicationTag);
		if (t != NULL && CFGetTypeID(t) == CFDataGetTypeID()) {
			CFIndex n = CFDataGetLength((CFDataRef)t);
			if (n >= tagmax) n = tagmax - 1;
			memcpy(tag, CFDataGetBytePtr((CFDataRef)t), n);
			tag[n] = 0;
		}
	}
	CFRelease(r);
	return 0;
}

static OSStatus fev_cdhash(const char *path, char *out, int max) {
	out[0] = 0;
	CFURLRef u = CFURLCreateFromFileSystemRepresentation(NULL, (const UInt8 *)path, strlen(path), false);
	SecStaticCodeRef code = NULL;
	OSStatus st = SecStaticCodeCreateWithPath(u, kSecCSDefaultFlags, &code);
	CFRelease(u);
	if (st) return st;
	CFDictionaryRef info = NULL;
	st = SecCodeCopySigningInformation(code, kSecCSDefaultFlags, &info);
	CFRelease(code);
	if (st) return st;
	CFDataRef h = info ? CFDictionaryGetValue(info, kSecCodeInfoUnique) : NULL;
	if (h == NULL || CFDataGetLength(h) * 2 >= max) {
		if (info) CFRelease(info);
		return errSecCSUnsigned;
	}
	static const char d[] = "0123456789abcdef";
	const UInt8 *b = CFDataGetBytePtr(h);
	for (CFIndex i = 0; i < CFDataGetLength(h); i++) {
		out[2 * i] = d[b[i] >> 4];
		out[2 * i + 1] = d[b[i] & 15];
	}
	out[2 * CFDataGetLength(h)] = 0;
	CFRelease(info);
	return 0;
}

static int fev_self_identity(char *who, int whomax) {
	who[0] = 0;
	SecCodeRef code = NULL;
	if (SecCodeCopySelf(kSecCSDefaultFlags, &code)) return 1;
	SecStaticCodeRef st = NULL;
	if (SecCodeCopyStaticCode(code, kSecCSDefaultFlags, &st)) { CFRelease(code); return 1; }
	CFDictionaryRef info = NULL;
	int adhoc = 1;
	if (SecCodeCopySigningInformation(st, kSecCSSigningInformation, &info) == 0 && info) {
		CFNumberRef f = CFDictionaryGetValue(info, kSecCodeInfoFlags);
		uint32_t flags = 0;
		if (f) CFNumberGetValue(f, kCFNumberSInt32Type, &flags);
		CFArrayRef certs = CFDictionaryGetValue(info, kSecCodeInfoCertificates);
		if (!(flags & kSecCodeSignatureAdhoc) && certs && CFArrayGetCount(certs) > 0) {
			adhoc = 0;
			CFStringRef s = SecCertificateCopySubjectSummary((SecCertificateRef)CFArrayGetValueAtIndex(certs, 0));
			if (s) {
				CFStringGetCString(s, who, whomax, kCFStringEncodingUTF8);
				CFRelease(s);
			}
		}
		CFRelease(info);
	}
	CFRelease(st); CFRelease(code);
	return adhoc;
}
*/
import "C"

import (
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"fev/proto/daemon"
)

const (
	errSecItemNotFound        = -25300
	errSecAuthFailed          = -25293
	errSecInteractionNotAllow = -25308
)

const DefaultKeyLabel = "dev.fevom.ca"

const (
	tagAny      = "k2"
	tagIdentity = "k5:"
)

var ErrKeyUnusable = errors.New("the CA's key cannot be used by this build")

var ErrKeyOtherProgram = errors.New("the CA's key was made by another program than fev's key signer")

type KeychainOptions struct {
	Keychain       string
	Label          string
	RootPath       string
	Root           RootOptions
	AnyApplication bool
	Signer         string
}

func Identity() string {
	var buf [512]C.char
	if C.fev_self_identity(&buf[0], C.int(len(buf))) != 0 {
		return ""
	}
	return C.GoString(&buf[0])
}

var keychainMu sync.Mutex

func OpenKeychainCA(o KeychainOptions) (*CA, error) {
	if o.Label == "" {
		o.Label = DefaultKeyLabel
	}
	if self := Identity(); self != "" && !o.AnyApplication {
		return openBoundCA(o, self)
	}
	return openSignerCA(o)
}

func openSignerCA(o KeychainOptions) (*CA, error) {
	p, err := startSigner(o.Signer)
	if err != nil {
		return nil, err
	}
	want := tagSigner + p.cdhash
	tag, pub, st, err := p.find(o.Keychain, o.Label)
	if err == nil && st == errSecItemNotFound {
		pub, st, err = p.make(o.Keychain, o.Label, want)
		tag = want
		if err == nil && st != 0 {
			err = fmt.Errorf("make the CA's key: OSStatus %d", st)
		}
	}
	switch {
	case err != nil:
	case st == errSecInvalidKeyRef || st == errSecAuthFailed || st == errSecInteractionNotAllow:
		err = fmt.Errorf("%w: %w: a key labelled %s is in the keychain that fev's key signer %s is not admitted to (OSStatus %d); \"fev setup --https\" replaces it with a new key and root", ErrKeyUnusable, ErrKeyOtherProgram, o.Label, p.cdhash, st)
	case st != 0:
		err = fmt.Errorf("find the CA's key: OSStatus %d", st)
	case tag != want:
		err = fmt.Errorf("%w: %w: the key labelled %s was made by %s, not by fev's key signer %s; \"fev setup --https\" replaces it with a new key and root", ErrKeyUnusable, ErrKeyOtherProgram, o.Label, madeBy(tag), p.cdhash)
	}
	if err != nil {
		p.Close()
		if errors.Is(err, ErrKeyUnusable) {
			return &CA{Key: daemon.KeyUnusable}, err
		}
		return nil, err
	}
	ca := &CA{Signer: &signerKey{p: p, pub: pub}, Key: daemon.KeySigner, release: func() { p.Close() }}
	return withRoot(ca, o)
}

func madeBy(tag string) string {
	switch {
	case strings.HasPrefix(tag, tagSigner):
		return "another key signer (" + strings.TrimPrefix(tag, tagSigner) + ")"
	case strings.HasPrefix(tag, tagIdentity):
		return "a fev signed by " + strings.TrimPrefix(tag, tagIdentity)
	case tag == tagAny:
		return "an older fev that held the key itself"
	}
	return fmt.Sprintf("another program (tag %q)", tag)
}

func withRoot(ca *CA, o KeychainOptions) (*CA, error) {
	if _, err := ca.Signer.Sign(nil, make([]byte, 32), crypto.SHA256); err != nil {
		ca.close()
		ca.Key = daemon.KeyUnusable
		if errors.Is(err, ErrKeyUnusable) {
			return ca, err
		}
		return ca, fmt.Errorf("%w: %v", ErrKeyUnusable, err)
	}
	pub := ca.Signer.Public().(*ecdsa.PublicKey)
	if b, err := os.ReadFile(o.RootPath); err == nil {
		if root, err := ParsePEM(b); err == nil && root.PublicKey.(*ecdsa.PublicKey).Equal(pub) {
			ca.Root = root
			return ca, nil
		}
	}
	root, err := NewRoot(ca.Signer, o.Root)
	if err != nil {
		ca.close()
		return nil, err
	}
	if err := WriteRootPEM(o.RootPath, root); err != nil {
		ca.close()
		return nil, err
	}
	ca.Root = root
	return ca, nil
}

func openBoundCA(o KeychainOptions, self string) (*CA, error) {
	signer, tag, err := findKey(o.Keychain, o.Label)
	if errors.Is(err, errNotAdmitted) {
		return openSignerCA(o)
	}
	if errors.Is(err, errNotFound) {
		tag = tagIdentity + self
		if err := makeKey(o.Keychain, o.Label, tag, true); err != nil {
			return nil, err
		}
		signer, tag, err = findKey(o.Keychain, o.Label)
	}
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(tag, tagSigner) {
		return openSignerCA(o)
	}
	ca := &CA{Signer: signer, Key: daemon.KeyIdentity, Identity: strings.TrimPrefix(tag, tagIdentity)}
	if !strings.HasPrefix(tag, tagIdentity) {
		ca.Key, ca.Identity = daemon.KeyUnusable, ""
		return ca, fmt.Errorf("%w: %w: the key labelled %s was made by %s", ErrKeyUnusable, ErrKeyOtherProgram, o.Label, madeBy(tag))
	}
	return withRoot(ca, o)
}

func DeleteKeychainKey(keychain, label, signer string) error {
	if label == "" {
		label = DefaultKeyLabel
	}
	p, err := startSigner(signer)
	if err != nil {
		return err
	}
	defer p.Close()
	st, err := p.delete(keychain, label)
	if err != nil {
		return err
	}
	if st != 0 && st != errSecItemNotFound {
		return fmt.Errorf("delete the CA's key: OSStatus %d", st)
	}
	return nil
}

var (
	errNotFound    = errors.New("no such key")
	errNotAdmitted = errors.New("a key this program is not admitted to")
)

func peekKey(keychain, label, signer string) (kind, identity string) {
	ck, cl := cstr(keychain), C.CString(label)
	defer C.free(unsafe.Pointer(cl))
	defer freeC(ck)
	var tag [600]C.char
	keychainMu.Lock()
	st := C.fev_key_peek(ck, cl, &tag[0], C.int(len(tag)))
	keychainMu.Unlock()
	switch {
	case st == errSecItemNotFound:
		return daemon.KeyNone, ""
	case st != 0:
		return daemon.KeyUnusable, ""
	}
	t := C.GoString(&tag[0])
	switch {
	case strings.HasPrefix(t, tagIdentity):
		return daemon.KeyIdentity, strings.TrimPrefix(t, tagIdentity)
	case strings.HasPrefix(t, tagSigner):
		if h, err := signerCDHash(signer); err == nil && t == tagSigner+h {
			return daemon.KeySigner, ""
		}
	}
	return daemon.KeyUnusable, ""
}

func signerCDHash(path string) (string, error) {
	p, err := signerPath(path)
	if err != nil {
		return "", err
	}
	cp := C.CString(p)
	defer C.free(unsafe.Pointer(cp))
	var out [200]C.char
	if st := C.fev_cdhash(cp, &out[0], C.int(len(out))); st != 0 {
		return "", fmt.Errorf("%s: no code signature (OSStatus %d)", p, int(st))
	}
	return C.GoString(&out[0]), nil
}

func cstr(s string) *C.char {
	if s == "" {
		return nil
	}
	return C.CString(s)
}

func freeC(p *C.char) {
	if p != nil {
		C.free(unsafe.Pointer(p))
	}
}

func findKey(keychain, label string) (*keySigner, string, error) {
	ck, cl := cstr(keychain), C.CString(label)
	defer C.free(unsafe.Pointer(cl))
	defer freeC(ck)
	var key C.SecKeyRef
	var tag [600]C.char
	keychainMu.Lock()
	st := C.fev_key_find(ck, cl, &key, &tag[0], C.int(len(tag)))
	keychainMu.Unlock()
	if st == errSecItemNotFound {
		return nil, "", errNotFound
	}
	if st == errSecInvalidKeyRef || st == errSecAuthFailed {
		return nil, "", fmt.Errorf("%w: OSStatus %d", errNotAdmitted, int(st))
	}
	if st != 0 {
		return nil, "", fmt.Errorf("find the CA's key: OSStatus %d", int(st))
	}
	s, err := wrapKey(key)
	if err != nil {
		C.fev_release(C.CFTypeRef(key))
		return nil, "", err
	}
	return s, C.GoString(&tag[0]), nil
}

func makeKey(keychain, label, tag string, bound bool) error {
	return makeKeyAs(keychain, label, tag, bound, false)
}

func makeKeyAs(keychain, label, tag string, bound, misplaced bool) error {
	ck, cl, ct := cstr(keychain), C.CString(label), C.CString(tag)
	defer C.free(unsafe.Pointer(cl))
	defer C.free(unsafe.Pointer(ct))
	defer freeC(ck)
	b := C.int(0)
	if bound {
		b = 1
	}
	m := C.int(0)
	if misplaced {
		m = 1
	}
	var code C.long
	keychainMu.Lock()
	st := C.fev_key_make(ck, cl, ct, b, m, &code)
	keychainMu.Unlock()
	if st != 0 {
		return fmt.Errorf("make the CA's key: OSStatus %d (error %d)", int(st), int(code))
	}
	return nil
}

type keySigner struct {
	key C.SecKeyRef
	pub *ecdsa.PublicKey
}

func wrapKey(key C.SecKeyRef) (*keySigner, error) {
	var n C.long
	p := C.fev_public_x963(key, &n)
	if p == nil {
		return nil, fmt.Errorf("%w: the CA key's public half cannot be read", errNotAdmitted)
	}
	raw := C.GoBytes(unsafe.Pointer(p), C.int(n))
	C.free(unsafe.Pointer(p))
	if _, err := ecdh.P256().NewPublicKey(raw); err != nil {
		return nil, fmt.Errorf("the CA's key is not P-256: %w", err)
	}
	s := &keySigner{key: key, pub: &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(raw[1:33]), Y: new(big.Int).SetBytes(raw[33:65])}}
	runtime.SetFinalizer(s, func(s *keySigner) { C.fev_release(C.CFTypeRef(s.key)) })
	return s, nil
}

func (s *keySigner) Public() crypto.PublicKey { return s.pub }

func (s *keySigner) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	if opts != nil && opts.HashFunc() != crypto.SHA256 || len(digest) != 32 {
		return nil, errors.New("the CA's key signs SHA-256 digests only")
	}
	var n, code C.long
	keychainMu.Lock()
	p := C.fev_sign_digest(s.key, (*C.uchar)(unsafe.Pointer(&digest[0])), C.long(len(digest)), &n, &code)
	keychainMu.Unlock()
	runtime.KeepAlive(s)
	if p == nil {
		if code == errSecAuthFailed || code == errSecInteractionNotAllow {
			return nil, fmt.Errorf("%w: the keychain is locked or does not admit this build (OSStatus %d)", ErrKeyUnusable, int(code))
		}
		return nil, fmt.Errorf("SecKeyCreateSignature: %d", int(code))
	}
	defer C.free(unsafe.Pointer(p))
	return C.GoBytes(unsafe.Pointer(p), C.int(n)), nil
}
