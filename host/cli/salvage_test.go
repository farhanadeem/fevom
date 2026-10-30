package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

func salvageDaemon(f *fake, end api.Operation) {
	f.on(api.MethodStorageSalvage, func(c *jsonrpc.Conn, params json.RawMessage) (any, error) {
		p, _ := jsonrpc.Bind[api.SalvageParams](params)
		op := api.Operation{ID: "op-1", Method: api.MethodStorageSalvage, Target: p.Dest, State: api.OperationRunning, Started: "2026-10-14T11:00:00Z"}
		end.ID, end.Method, end.Target, end.Started = op.ID, op.Method, op.Target, op.Started
		go func() {
			moving := op
			moving.Phase = "copy"
			c.Notify(api.NotifyOperationProgress, api.OperationProgress{Subscription: 2, Operation: moving})
			c.Notify(api.NotifyOperationProgress, api.OperationProgress{Subscription: 2, Operation: end})
		}()
		return op, nil
	})
}

func doneWith(r api.Salvaged) api.Operation {
	b, _ := json.Marshal(r)
	return api.Operation{State: api.OperationDone, Phase: "copy", Ended: "2026-10-14T11:03:12Z", Result: b}
}

func TestSalvageFollowsTheOperationToItsEnd(t *testing.T) {
	f := newFake(t)
	salvageDaemon(f, doneWith(api.Salvaged{Method: "copy", Files: 18234, Bytes: 2 << 30,
		Errors: []string{"/data/@docker/volumes/pg/_data/base/1: input/output error, 8192 bytes not read"}, Lost: 3}))
	r := f.run("salvage", "/tmp/fev-salvage-test", "--volume", "pgdata", "--machine", "dev", "--machine", "f43").want(t, 0)
	golden(t, "salvage", r.out)
	if !strings.Contains(r.err, "copying from the disk mounted read-only") {
		t.Errorf("no word of the phase: %q", r.err)
	}
	if got := f.params(api.MethodStorageSalvage); got != `{"dest":"/tmp/fev-salvage-test","volumes":["pgdata"],"machines":["dev","f43"]}` {
		t.Errorf("asked %s", got)
	}
	made := f.made()
	if made[0].Method != api.MethodEventsSubscribe {
		t.Errorf("not subscribed before the call: %v", made)
	}

	f2 := newFake(t)
	salvageDaemon(f2, doneWith(api.Salvaged{Method: "restore", Files: 2, Bytes: 10}))
	r = f2.run("--format", "json", "salvage", "rel", "--docker").want(t, 0)
	wd, _ := filepath.Abs("rel")
	var op api.Operation
	if err := json.Unmarshal([]byte(r.out), &op); err != nil || op.State != api.OperationDone || op.Target != wd {
		t.Errorf("json: %s (%v)", r.out, err)
	}
	if got := f2.params(api.MethodStorageSalvage); got != `{"dest":"`+wd+`","docker":true}` {
		t.Errorf("asked %s", got)
	}

	f3 := newFake(t)
	salvageDaemon(f3, api.Operation{State: api.OperationFailed, Error: "salvage: the data filesystem is mounted read-write and needs no salvage"})
	r = f3.run("salvage", "/tmp/x").want(t, ExitFailed)
	if !strings.Contains(r.err, "mounted read-write and needs no salvage") {
		t.Errorf("failure: %q", r.err)
	}
	if r := f3.run("salvage"); r.code != ExitUsage {
		t.Errorf("no folder: %+v", r)
	}
}
