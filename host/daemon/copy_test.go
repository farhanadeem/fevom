package daemon

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"testing"

	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

func TestACopySessionReachesTheGuest(t *testing.T) {
	f := machinesDaemon(t)
	f.create("dev", "ubuntu")
	op, err := f.c.CreateMachine(bg, api.MachinesCreateParams{Name: "iso", Distro: "ubuntu", Isolated: true})
	if err != nil {
		t.Fatal(err)
	}
	if end := f.ended(op.ID); end.State != api.OperationDone {
		t.Fatalf("the isolated machine's create: %+v", end)
	}
	out, exit, err := f.run(f.c, api.SessionStartParams{Machine: "iso", Copy: agent.CopyOut, Argv: []string{"./made"}})
	if err != nil || exit.ExitCode != 0 {
		t.Fatalf("a copy out of iso: %v %+v", err, exit)
	}
	if _, err := tar.NewReader(bytes.NewReader([]byte(out))).Next(); !errors.Is(err, io.EOF) {
		t.Errorf("the stand-in's copy out is not an empty archive: %v (%d bytes)", err, len(out))
	}
	if _, err := f.c.SessionStart(bg, api.SessionStartParams{Machine: "dev", Copy: agent.CopyOut, Argv: []string{"x"}}, nil); err == nil {
		t.Errorf("a machine that is not isolated took a copy session")
	}
	for why, p := range map[string]api.SessionStartParams{
		"no names":   {Machine: "iso", Copy: agent.CopyIn, Argv: []string{"."}},
		"a terminal": {Machine: "iso", Copy: agent.CopyOut, Argv: []string{"x"}, TTY: true},
		"a shell":    {Machine: "iso", Copy: agent.CopyOut, Argv: []string{"x"}, Login: true, Shell: true},
		"a name":     {Machine: "iso", Copy: agent.CopyIn, Argv: []string{".", "../x"}},
	} {
		var e *jsonrpc.Error
		if _, err := f.c.SessionStart(bg, p, nil); !errors.As(err, &e) || e.Code != jsonrpc.CodeInvalidParams {
			t.Errorf("%s: %v", why, err)
		}
	}
}
