package agent

import (
	"strings"
	"testing"
)

func TestANodeNameIsAHostName(t *testing.T) {
	for _, ok := range []string{"fev", "swarm-a", "a", "node1.example", strings.Repeat("a", 63), DefaultNodeName} {
		if err := CheckNodeName(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a b", "a=b", "fev.x86=0 x", "-a", "a-", "a..b", ".a", "a_b", "é", strings.Repeat("a", 64), strings.Repeat("a.", 127) + "aa"} {
		if err := CheckNodeName(bad); err == nil {
			t.Errorf("%q taken", bad)
		}
	}
}
