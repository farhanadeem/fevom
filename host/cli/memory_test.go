package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	api "fev/proto/daemon"
)

func TestMemoryReclaimAsksTheGuestNow(t *testing.T) {
	f := newFake(t)
	r := f.run("memory", "reclaim").want(t, 0)
	golden(t, "memory.reclaim", r.out)
	if got := f.params(api.MethodMemoryReclaim); got != "{}" {
		t.Errorf("asked %s", got)
	}

	f2 := newFake(t)
	f2.fixture(api.MethodMemoryReclaim, "memory.reclaim.skipped")
	r = f2.run("memory", "reclaim", "--floor", "256").want(t, 0)
	golden(t, "memory.reclaim.skipped", r.out)
	if got := f2.params(api.MethodMemoryReclaim); got != `{"floor_mib":256}` {
		t.Errorf("asked %s", got)
	}
	r = f2.run("--format", "json", "memory", "reclaim").want(t, 0)
	sameJSON(t, r.out, fixtureResult(t, "memory.reclaim.skipped"))
	var back api.Reclaimed
	if err := json.Unmarshal([]byte(r.out), &back); err != nil || back.Skipped != api.ReclaimSkippedFloor {
		t.Errorf("json %s: %v", r.out, err)
	}
	if r := f2.run("-q", "memory", "reclaim"); r.code != 0 || r.out != "" {
		t.Errorf("quiet: %+v", r)
	}
	if r := f2.run("memory", "reclaim", "--floor", "-1"); r.code != ExitUsage {
		t.Errorf("a negative floor: %+v", r)
	}

	started := noDaemon(t)
	r = fev(t, scratchHome(t), "", "memory", "reclaim")
	if r.code != ExitNotRunning || !strings.Contains(r.err, "not running") {
		t.Errorf("with no daemon: %+v", r)
	}
	if _, err := os.Stat(started); err == nil {
		t.Error("a daemon was started for a reclaim")
	}
}
