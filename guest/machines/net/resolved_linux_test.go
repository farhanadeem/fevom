package machnet

import (
	"strings"
	"testing"

	"fev/guest/machines"
)

func TestTheResolvedDropInNamesThisResolver(t *testing.T) {
	if machines.ResolverAddress != Gateway(Prefix).String() || machines.LocalZone != LocalZone {
		t.Fatalf("machines says %s and %s; the network is %s and %s", machines.ResolverAddress, machines.LocalZone, Gateway(Prefix), LocalZone)
	}
	if d := machines.ResolvedDropIn(machines.ResolverAddress); !strings.Contains(d, "\nDNS=198.19.0.1\nDomains=~fev.local\n") {
		t.Fatalf("drop-in:\n%s", d)
	}
}
