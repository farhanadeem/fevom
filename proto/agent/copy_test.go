package agent

import "testing"

func TestCheckExecCopy(t *testing.T) {
	ok := []ExecStartParams{
		{Context: ExecMachine, Machine: "m", Copy: CopyIn, Argv: []string{".", "proj", "one"}},
		{Context: ExecMachine, Machine: "m", Copy: CopyIn, Argv: []string{"/tmp/", "f"}},
		{Context: ExecMachine, Machine: "m", Copy: CopyOut, Argv: []string{".", "/etc/hostname"}},
		{Context: ExecMachine, Machine: "m", Argv: []string{"true"}},
	}
	for _, p := range ok {
		if err := CheckExecCopy(p); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	bad := map[string]ExecStartParams{
		"init's context":   {Context: ExecInit, Copy: CopyOut, Argv: []string{"x"}},
		"a direction":      {Context: ExecMachine, Copy: "sideways", Argv: []string{"x"}},
		"no names":         {Context: ExecMachine, Copy: CopyIn, Argv: []string{"."}},
		"nothing out":      {Context: ExecMachine, Copy: CopyOut},
		"a path as a name": {Context: ExecMachine, Copy: CopyIn, Argv: []string{".", "a/b"}},
		"dot dot":          {Context: ExecMachine, Copy: CopyIn, Argv: []string{".", ".."}},
		"twice":            {Context: ExecMachine, Copy: CopyIn, Argv: []string{".", "a", "a"}},
		"an empty path":    {Context: ExecMachine, Copy: CopyOut, Argv: []string{""}},
		"a terminal":       {Context: ExecMachine, Copy: CopyOut, Argv: []string{"x"}, TTY: true},
		"a shell":          {Context: ExecMachine, Copy: CopyOut, Argv: []string{"x"}, Shell: true, Login: true},
		"a subsystem":      {Context: ExecMachine, Copy: CopyOut, Argv: []string{"x"}, Subsystem: SubsystemSFTP},
	}
	for name, p := range bad {
		if err := CheckExecCopy(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
