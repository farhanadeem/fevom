package version

import (
	"fmt"
	"strings"
)

const Unstamped = "dev"

var Build = Unstamped

type Mismatch struct {
	Ours, Theirs string
	Peer         string
}

func (m *Mismatch) Error() string {
	return fmt.Sprintf("build mismatch: this is %q and the %s is %q: %s",
		m.Ours, m.Peer, m.Theirs, Explain(m.Ours, m.Theirs))
}

func Check(ours, theirs, peer string) error {
	if ours == theirs {
		return nil
	}
	return &Mismatch{Ours: ours, Theirs: theirs, Peer: peer}
}

func Explain(a, b string) string {
	const rebuild = "rebuild everything in one pass (guest/build.sh, host/build.sh, and the VMM)"
	switch {
	case a == b:
		return "the builds are the same"
	case a == "" || b == "":
		return "one side sent no build identifier; it predates the check, " + rebuild
	case a == Unstamped || b == Unstamped:
		return "one side was built without tools/build-id.sh (go run, go test, or a bare cargo build); " + rebuild
	}
	ca, da := split(a)
	cb, db := split(b)
	switch {
	case ca != cb:
		return "they were built from different commits; " + rebuild
	case da == "" || db == "":
		return "same commit, but one was built with uncommitted changes; " + rebuild
	default:
		return "same commit, different uncommitted changes: one side is stale; " + rebuild
	}
}

func split(id string) (commit, dirty string) {
	if i := strings.LastIndex(id, "-dirty"); i >= 0 {
		return id[:i], id[i+len("-dirty"):]
	}
	return id, ""
}
