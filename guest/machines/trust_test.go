package machines

import (
	"encoding/pem"
	"reflect"
	"sort"
	"testing"
)

func TestEachDistributionTakesRootsWithItsOwnTool(t *testing.T) {
	for _, c := range []struct {
		name string
		has  []string
		want trustTool
		ok   bool
	}{
		{"debian", []string{"/usr/sbin/update-ca-certificates"}, trustTool{localCertificates, ".crt", []string{"/usr/sbin/update-ca-certificates"}}, true},
		{"alpine with ca-certificates", []string{"/usr/sbin/update-ca-certificates", "/sbin/apk"}, trustTool{localCertificates, ".crt", []string{"/usr/sbin/update-ca-certificates"}}, true},
		{"fedora", []string{"/usr/bin/update-ca-trust", "/etc/pki/ca-trust/source/anchors"}, trustTool{fedoraAnchors, ".pem", []string{"/usr/bin/update-ca-trust"}}, true},
		{"arch", []string{"/usr/bin/update-ca-trust", archSource, archAnchors}, trustTool{archAnchors, ".pem", []string{"/usr/bin/update-ca-trust"}}, true},
		{"alpine without", []string{"/sbin/apk"}, trustTool{localCertificates, ".crt", nil}, false},
		{"opensuse", []string{"/usr/sbin/update-ca-certificates", "/etc/pki/trust"}, trustTool{suseAnchors, ".pem", []string{"/usr/sbin/update-ca-certificates"}}, true},
		{"gentoo and void", []string{"/usr/bin/update-ca-certificates"}, trustTool{localCertificates, ".crt", []string{"/usr/bin/update-ca-certificates"}}, true},
		{"almalinux", []string{"/usr/bin/update-ca-trust", "/etc/pki/ca-trust/source/anchors"}, trustTool{fedoraAnchors, ".pem", []string{"/usr/bin/update-ca-trust"}}, true},
	} {
		has := func(p string) bool {
			for _, h := range c.has {
				if h == p {
					return true
				}
			}
			return false
		}
		got, ok := chooseTrustTool(has)
		if !reflect.DeepEqual(got, c.want) || ok != c.ok {
			t.Errorf("%s: %+v %v, want %+v %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestTheTrustPlanReplacesOnlyFevomsFiles(t *testing.T) {
	a, b := []byte("root a"), []byte("root b")
	p := planTrust([][]byte{a, b}, ".crt", []string{trustFileName(a, ".crt"), trustFileName([]byte("gone"), ".crt"), "fev-notes.txt", "corp.crt", trustFileName([]byte("gone"), ".pem")})
	var names []string
	for n := range p.write {
		names = append(names, n)
	}
	sort.Strings(names)
	want := []string{trustFileName(a, ".crt"), trustFileName(b, ".crt")}
	sort.Strings(want)
	if !reflect.DeepEqual(names, want) {
		t.Errorf("writes %v, want %v", names, want)
	}
	if !reflect.DeepEqual(p.remove, []string{trustFileName([]byte("gone"), ".crt"), trustFileName([]byte("gone"), ".pem")}) {
		t.Errorf("removes %v", p.remove)
	}
	blk, _ := pem.Decode(p.write[trustFileName(a, ".crt")])
	if blk == nil || blk.Type != "CERTIFICATE" || string(blk.Bytes) != "root a" {
		t.Errorf("the file of a: %q", p.write[trustFileName(a, ".crt")])
	}
	if got := parseTrustPEM(append(p.write[want[0]], p.write[want[1]]...)); len(got) != 2 {
		t.Errorf("parsed %d roots", len(got))
	}
	if n := trustFileName(a, ".crt"); len(n) != len("fev-")+16+4 {
		t.Errorf("name %s", n)
	}
}
