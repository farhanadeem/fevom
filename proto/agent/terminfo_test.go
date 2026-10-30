package agent

import (
	"encoding/base64"
	"strings"
	"testing"
)

var tinyEntry, _ = base64.StdEncoding.DecodeString("GgEgAAIAAwAGAAoAeHRlcm0tZmV2dGVzdHxmZXYgdGVzdCB0ZXJtaW5hbAAAAVAA//8YAP//AAD///////8CAAcAG1tIG1sySgA=")

func entry(magic uint16, names string) []byte {
	n := len(names) + 1
	b := []byte{byte(magic), byte(magic >> 8), byte(n), byte(n >> 8), 0, 0, 0, 0, 0, 0, 0, 0}
	b = append(b, names...)
	return append(b, 0, 0, 0)
}

func TestCheckTerminfoTakesACompiledEntryForTheName(t *testing.T) {
	if err := CheckTerminfo("xterm-fevtest", tinyEntry); err != nil {
		t.Fatalf("the Mac's tic's entry: %v", err)
	}
	for _, c := range []struct {
		name string
		b    []byte
	}{
		{"xterm-ghostty", entry(0o1036, "xterm-ghostty|ghostty|Ghostty")},
		{"ghostty", entry(0o1036, "xterm-ghostty|ghostty|Ghostty")},
		{"foot", entry(0o432, "foot|foot terminal emulator")},
		{"x", entry(0o432, "x")},
	} {
		if err := CheckTerminfo(c.name, c.b); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestCheckTerminfoRefusesWhatIsNotAnEntryForTheName(t *testing.T) {
	long := append(entry(0o432, "big"), make([]byte, MaxTerminfo)...)
	badSize := entry(0o432, "xterm-ghostty|g")
	badSize[2], badSize[3] = 0xff, 0x7f
	twoStrings := entry(0o432, "xterm-ghostty\x00x|g")
	noNUL := entry(0o432, "xterm-ghostty|g")
	noNUL[12+len("xterm-ghostty|g")] = 'x'
	for _, c := range []struct {
		why, name string
		b         []byte
	}{
		{"empty", "xterm-ghostty", nil},
		{"short", "xterm-ghostty", tinyEntry[:10]},
		{"a shell script", "xterm-ghostty", []byte("#!/bin/sh\nrm -rf /\n")},
		{"magic 0433", "xterm-ghostty", entry(0o433, "xterm-ghostty|g")},
		{"another terminal's", "xterm-ghostty", tinyEntry},
		{"the description is not a name", "Ghostty", entry(0o1036, "xterm-ghostty|ghostty|Ghostty")},
		{"a name inside another", "xterm", entry(0o432, "xterm-ghostty|g")},
		{"names past the end", "xterm-ghostty", badSize},
		{"two strings", "xterm-ghostty", twoStrings},
		{"no NUL", "xterm-ghostty", noNUL},
		{"too long", "big", long},
		{"a path", "../x", entry(0o432, "../x|x")},
		{"a slash", "a/b", entry(0o432, "a/b|x")},
		{"hidden", ".x", entry(0o432, ".x|x")},
		{"129 bytes", strings.Repeat("x", 129), entry(0o432, strings.Repeat("x", 129)+"|x")},
		{"no name", "", entry(0o432, "|x")},
		{"a newline", "x\nfake log line", entry(0o432, "x\nfake log line|x")},
		{"an escape", "x\x1b[2J", entry(0o432, "x\x1b[2J|x")},
		{"a blank", "x y", entry(0o432, "x y|x")},
		{"a DEL", "x\x7f", entry(0o432, "x\x7f|x")},
	} {
		if err := CheckTerminfo(c.name, c.b); err == nil {
			t.Errorf("%s: taken", c.why)
		}
	}
	if err := CheckTerminfoName(strings.Repeat("x", 128)); err != nil {
		t.Errorf("128 bytes: %v", err)
	}
}

func TestTermOfIsTheLastTERM(t *testing.T) {
	if got := TermOf([]string{"TERM=a", "TERMINFO=/x", "TERM=b"}); got != "b" {
		t.Errorf("got %q", got)
	}
	if got := TermOf([]string{"XTERM=a"}); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestExecStartTakesAnEntryOnlyInAMachineOrDebugSessionWithATerminalAndATERM(t *testing.T) {
	ok := ExecStartParams{Context: ExecMachine, Machine: "dev", Argv: []string{"htop"}, Env: []string{"TERM=xterm-fevtest"}, TTY: true, Terminfo: tinyEntry}
	if err := CheckExecTerminfo(ok); err != nil {
		t.Fatal(err)
	}
	stdin := ok
	stdin.TTY, stdin.StdinTTY = false, true
	if err := CheckExecTerminfo(stdin); err != nil {
		t.Errorf("stdin_tty: %v", err)
	}
	dbg := ok
	dbg.Context, dbg.Machine, dbg.Debug = ExecDebug, "", &DebugTarget{Target: "web"}
	if err := CheckExecTerminfo(dbg); err != nil {
		t.Errorf("debug: %v", err)
	}
	junk := ok
	junk.Terminfo = []byte("junk")
	if err := CheckExecTerminfo(junk); err != nil {
		t.Errorf("junk: %v", err)
	}
	for why, p := range map[string]ExecStartParams{
		"init's":      {Context: ExecInit, Argv: []string{"x"}, Env: ok.Env, TTY: true, Terminfo: tinyEntry},
		"no terminal": {Context: ExecMachine, Machine: "dev", Argv: []string{"x"}, Env: ok.Env, Terminfo: tinyEntry},
		"no TERM":     {Context: ExecMachine, Machine: "dev", Argv: []string{"x"}, TTY: true, Terminfo: tinyEntry},
		"too large":   {Context: ExecMachine, Machine: "dev", Argv: []string{"x"}, Env: ok.Env, TTY: true, Terminfo: make([]byte, MaxTerminfo+1)},
	} {
		if CheckExecTerminfo(p) == nil {
			t.Errorf("%s: taken", why)
		}
	}
}
