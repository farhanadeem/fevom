package nat64

import (
	"runtime"
	"testing"
	"unsafe"
)

func TestTheMacsNAT64PrefixesAreRead(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the Mac's")
	}
	if n := unsafe.Sizeof(ifNAT64Req{}); n != 96 {
		t.Fatalf("struct if_nat64req is %d bytes, want 96", n)
	}
	if siocGIFNAT64Prefix != 0xc06069c1 {
		t.Fatalf("SIOCGIFNAT64PREFIX is %#x", siocGIFNAT64Prefix)
	}
	ps, err := Prefixes()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("NAT64 prefixes: %v", ps)
	if _, err := Of("nosuch9"); err == nil {
		t.Error("an interface nobody has was read")
	}
}
