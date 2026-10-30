package machines

import (
	"strings"
	"testing"
)

func TestTheConsoleBecomesLines(t *testing.T) {
	var s lineSplitter
	var got []string
	for _, chunk := range []string{
		"\x1b[0;32m  OK  \x1b[0m] Reached target ", "multi-user.target.\r\n",
		"Welcome to \x1b]0;dev\x07Ubuntu\r\n", "10%\r50%\r100%\r\n",
		"tab\there\x00\r\n", "no end",
	} {
		got = append(got, s.feed([]byte(chunk))...)
	}
	got = append(got, s.flush()...)
	want := []string{"  OK  ] Reached target multi-user.target.", "Welcome to Ubuntu", "100%", "tab\there", "no end"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	long := strings.Repeat("x", consoleMaxLine+10) + "\n"
	got = s.feed([]byte(long))
	if len(got) != 2 || len(got[0]) != consoleMaxLine || len(got[1]) != 10 {
		t.Fatalf("a long line: %d lines", len(got))
	}
	if s.flush() != nil {
		t.Fatal("something left after a whole line")
	}
}

func TestTheRuntimesLinesNameTheirMachine(t *testing.T) {
	for text, want := range map[string]string{
		RuntimeLinePrefix("dev") + "its init ended (signal 2, SIGINT)": "dev",
		AgentLinePrefix("u24") + "start requested by \"daemon\"":       "u24",
		"machines: the VM has no vsock; machines have none either":     "",
		"machines: B_d: x":               "",
		RuntimeLinePrefix("Dev.1") + "x": "Dev.1",
		"agent: engine start requested":  "",
	} {
		got, ok := runtimeLineMachine(text)
		if got != want || ok != (want != "") {
			t.Errorf("%q: %q %v, want %q", text, got, ok, want)
		}
	}
}
