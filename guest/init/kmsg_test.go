package main

import "testing"

func TestParseKmsg(t *testing.T) {
	ms, level, text, ok := parseKmsg("6,339,5140900,-;NET: Registered PF_VSOCK protocol family\n SUBSYSTEM=net\n")
	if !ok || ms != 5140.9 || level != 6 || text != "NET: Registered PF_VSOCK protocol family" {
		t.Fatalf("got %v %v %q %v", ms, level, text, ok)
	}
	if _, level, _, _ := parseKmsg("30,1,0,-;daemon message\n"); level != 6 {
		t.Fatalf("level %d", level)
	}
	if _, _, text, _ := parseKmsg("4,2,10,c;a;b,c\n"); text != "a;b,c" {
		t.Fatalf("text %q", text)
	}
	for _, bad := range []string{"", "no separator", "6,1;short header\n", "x,1,2,-;bad priority\n", "6,1,y,-;bad time\n"} {
		if _, _, _, ok := parseKmsg(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}
