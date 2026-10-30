// A stand-in for one build of fevd: it opens the CA as fevd
// does, in the keychain it is given, and signs one leaf. The test links it
// with another stamp for each build, so each has its own code directory hash,
// as every rebuild of fevd has.
//
//	fevd-build KEYCHAIN LABEL ROOTPATH [N SECONDS]
//
// One line: build=STAMP key=KIND root=SHA256 signed=BOOL err=TEXT
//
// With N and SECONDS it is fevd as it runs: the CA opened once and kept,
// with its one resident signer, signing N times SECONDS apart, a line each
// with the time and the signer's pid; it exits 1 if any signature failed.
package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/sha256"
	"fmt"
	"os"
	"strconv"
	"time"

	"fev/host/trust"
)

var stamp = "0"

func main() {
	ca, err := trust.OpenKeychainCA(trust.KeychainOptions{Keychain: os.Args[1], Label: os.Args[2], RootPath: os.Args[3], AnyApplication: true})
	root, kind := "-", "-"
	if ca != nil {
		kind = ca.Key
		if ca.Root != nil {
			root = trust.Fingerprint(ca.Root.Raw)
		}
	}
	n, every := 1, time.Duration(0)
	if len(os.Args) == 6 {
		n, _ = strconv.Atoi(os.Args[4])
		s, _ := strconv.Atoi(os.Args[5])
		every = time.Duration(s) * time.Second
	}
	failed := false
	for i := 0; i < n; i++ {
		if i > 0 {
			time.Sleep(every)
		}
		signed, serr := false, err
		if err == nil {
			d := sha256.Sum256([]byte("fev " + stamp))
			var sig []byte
			sig, serr = ca.Signer.Sign(nil, d[:], crypto.SHA256)
			signed = serr == nil && ecdsa.VerifyASN1(ca.Signer.Public().(*ecdsa.PublicKey), d[:], sig)
		}
		if every > 0 {
			fmt.Printf("%s resident pid=%d ", time.Now().Format("15:04:05"), os.Getpid())
		}
		fmt.Printf("build=%s key=%s root=%s signed=%t err=%v\n", stamp, kind, root, signed, serr)
		failed = failed || !signed
	}
	if failed {
		os.Exit(1)
	}
}
