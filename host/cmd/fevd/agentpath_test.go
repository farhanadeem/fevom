package main

import (
	"testing"

	"fev/host/daemon/vm"
)

func TestTheEnginesAgentPathIsTheSharesView(t *testing.T) {
	root := &vm.Share{Root: "/"}
	scratch := &vm.Share{Root: "/tmp/s/mac"}
	for _, c := range []struct {
		p     string
		share *vm.Share
		want  string
	}{
		{"/private/tmp/com.apple.launchd.x/Listeners", root, "/private/tmp/com.apple.launchd.x/Listeners"},
		{"/tmp/ba.1/agent.sock", root, "/tmp/ba.1/agent.sock"},
		{"/tmp/s/mac/Users/t/agent.sock", scratch, "/Users/t/agent.sock"},
		{"/tmp/other/agent.sock", scratch, ""},
		{"/tmp/s/macx/agent.sock", scratch, ""},
		{"/x/agent.sock", nil, ""},
		{"relative.sock", root, ""},
		{"", root, ""},
	} {
		if got := engineAgentPath(c.p, c.share); got != c.want {
			t.Errorf("%q under %+v: %q, want %q", c.p, c.share, got, c.want)
		}
	}
}
