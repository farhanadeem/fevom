package netstack

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func record(name string, rrtype uint16, ttl uint32, rdata []byte) []byte {
	b := append([]byte(name), 0)
	b = binary.NativeEndian.AppendUint16(b, rrtype)
	b = binary.NativeEndian.AppendUint16(b, dns.ClassINET)
	b = binary.NativeEndian.AppendUint32(b, ttl)
	b = binary.NativeEndian.AppendUint16(b, uint16(len(rdata)))
	return append(b, rdata...)
}

func TestParseRecordsFromTheSystemResolver(t *testing.T) {
	target := []byte("\x07example\x03com\x00")
	buf := append(record("www.example.com.", dns.TypeCNAME, 60, target), record("example.com.", dns.TypeA, 300, []byte{93, 184, 215, 14})...)
	rrs, err := parseRecords(buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(rrs) != 2 || rrs[0].(*dns.CNAME).Target != "example.com." || !rrs[1].(*dns.A).A.Equal(net.IPv4(93, 184, 215, 14)) || rrs[1].Header().Ttl != 300 {
		t.Fatalf("parsed %v", rrs)
	}
	if rrs, err := parseRecords(nil); err != nil || len(rrs) != 0 {
		t.Fatalf("empty: %v %v", rrs, err)
	}
	for cut := 1; cut < len(buf); cut++ {
		if cut == len(record("www.example.com.", dns.TypeCNAME, 60, target)) {
			continue
		}
		if _, err := parseRecords(buf[:cut]); err == nil {
			t.Fatalf("a buffer cut at %d of %d bytes parsed", cut, len(buf))
		}
	}
	if _, err := parseRecords(record("x.", dns.TypeA, 1, []byte{1, 2})); err == nil {
		t.Fatal("a two-byte A record parsed")
	}
}

func TestTheMacResolverAnswers(t *testing.T) {
	var r macResolver
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rrs, err := r.Resolve(ctx, "localhost.", dns.TypeA)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rr := range rrs {
		if a, ok := rr.(*dns.A); ok && a.A.IsLoopback() {
			found = true
		}
	}
	if !found {
		t.Fatalf("localhost. A: %v", rrs)
	}
	start := time.Now()
	rrs, err = r.Resolve(ctx, "no-such-host.invalid.", dns.TypeA)
	if err != nil || len(rrs) != 0 {
		t.Fatalf("no-such-host.invalid.: %v %v", rrs, err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("a negative answer took %v", time.Since(start))
	}
	expired, cancel2 := context.WithTimeout(context.Background(), -time.Second)
	defer cancel2()
	if _, err := r.Resolve(expired, "localhost.", dns.TypeA); err == nil {
		t.Fatal("resolved with an expired deadline")
	}
	_ = r.SearchDomains()
}
