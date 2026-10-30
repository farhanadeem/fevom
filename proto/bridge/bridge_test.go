package bridge

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	api "fev/proto/agent"
)

func TestTheSharePathsAreTheAgentAPIs(t *testing.T) {
	if ShareTarget != api.ShareTarget {
		t.Errorf("ShareTarget %q, the Agent API's %q", ShareTarget, api.ShareTarget)
	}
	var want []string
	for _, p := range api.MachineSharePaths {
		if p.Source != p.Target {
			t.Errorf("%v is not a same-path folder", p)
		}
		want = append(want, p.Target)
	}
	if strings.Join(want, " ") != strings.Join(SamePaths, " ") {
		t.Errorf("SamePaths %v, the Agent API's %v", SamePaths, want)
	}
	if Socket != "/run/fev/mac.sock" || AgentSocket != "/run/fev/ssh-auth.sock" {
		t.Errorf("the sockets are %s and %s", Socket, AgentSocket)
	}
}

func TestAHeaderTravelsAsOneLine(t *testing.T) {
	h := &Header{Verb: VerbRun, Argv: []string{"uname", "-s"}, Dir: "/mnt/mac/Users/me", Env: []string{"TERM=xterm"}, Stdin: true, Stdout: true, Rows: 40, Cols: 120, User: "me"}
	var buf bytes.Buffer
	if err := WriteHeader(&buf, h); err != nil {
		t.Fatal(err)
	}
	buf.WriteString("after")
	r := bufio.NewReader(&buf)
	got, line, err := ReadHeader(r)
	if err != nil {
		t.Fatal(err)
	}
	if got.Verb != VerbRun || strings.Join(got.Argv, " ") != "uname -s" || got.Rows != 40 || !got.TTY() || got.Stderr {
		t.Errorf("read back %+v", got)
	}
	if !bytes.HasSuffix(line, []byte("\n")) || bytes.Count(line, []byte("\n")) != 1 {
		t.Errorf("the line %q", line)
	}
	rest, _ := io.ReadAll(r)
	if string(rest) != "after" {
		t.Errorf("what followed the header: %q", rest)
	}
	if err := got.Check(); err != nil {
		t.Error(err)
	}
}

func TestAHeaderLongerThanTheBoundIsRefused(t *testing.T) {
	long := strings.Repeat("x", MaxHeader)
	if err := WriteHeader(io.Discard, &Header{Verb: VerbRun, Argv: []string{long}}); err == nil {
		t.Error("a header over the bound was written")
	}
	r := bufio.NewReader(strings.NewReader(`{"verb":"run","argv":["` + long + `"]}` + "\n"))
	if _, _, err := ReadHeader(r); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Errorf("a line over the bound: %v", err)
	}
	if _, _, err := ReadHeader(bufio.NewReader(strings.NewReader(`{"verb":"run"`))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a cut header: %v", err)
	}
}

func TestHeadersAreChecked(t *testing.T) {
	for _, c := range []struct {
		h  Header
		ok bool
	}{
		{Header{Verb: VerbRun, Argv: []string{"true"}}, true},
		{Header{Verb: VerbRun}, false},
		{Header{Verb: VerbRun, Argv: []string{""}}, false},
		{Header{Verb: VerbRun, Argv: []string{"x"}, Rows: -1}, false},
		{Header{Verb: VerbRun, Argv: []string{"x"}, Env: []string{"NOEQUALS"}}, false},
		{Header{Verb: VerbRun, Argv: []string{"x"}, Env: []string{"=v"}}, false},
		{Header{Verb: VerbInfo}, true},
		{Header{Verb: VerbNotify, Message: "done"}, true},
		{Header{Verb: VerbNotify}, false},
		{Header{Verb: VerbLink, Names: []string{"sw_vers"}}, true},
		{Header{Verb: VerbLink}, false},
		{Header{Verb: VerbUnlink, Names: []string{"../x"}}, false},
		{Header{Verb: VerbUnlink, Names: []string{".."}}, false},
		{Header{Verb: VerbLink, Names: []string{"a\nb"}}, false},
		{Header{Verb: VerbExec}, false},
		{Header{Verb: VerbExec, Argv: []string{"sw_vers"}, Path: "/mnt/mac/usr/bin/sw_vers"}, true},
		{Header{Verb: VerbExec, Argv: []string{"sw_vers"}}, false},
		{Header{Verb: VerbExec, Argv: []string{"x"}, Path: "x"}, false},
		{Header{Verb: VerbExec, Argv: []string{"x"}, Path: "/mnt/mac/../x"}, false},
		{Header{Verb: VerbExec, Argv: []string{"x"}, Path: "/a\x00b"}, false},
		{Header{Verb: VerbExec, Path: "/x"}, false},
	} {
		err := c.h.Check()
		if (err == nil) != c.ok {
			t.Errorf("%+v: %v", c.h, err)
		}
	}
}

func TestTheAnswerIsOkOrOneLineWhyNot(t *testing.T) {
	var buf bytes.Buffer
	WriteOK(&buf)
	WriteError(&buf, "uid 65534 may not\nuse it")
	r := bufio.NewReader(&buf)
	if err := ReadReply(r); err != nil {
		t.Errorf("ok: %v", err)
	}
	err := ReadReply(r)
	var refused *Refused
	if !errors.As(err, &refused) || refused.Text != "uid 65534 may not use it" {
		t.Errorf("err: %v", err)
	}
	if err := ReadReply(r); err == nil || !strings.Contains(err.Error(), "no answer") {
		t.Errorf("nothing: %v", err)
	}
}

func TestFramesSplitAndCarryEveryStream(t *testing.T) {
	var buf bytes.Buffer
	big := bytes.Repeat([]byte("a"), MaxFrame+10)
	WriteFrame(&buf, FrameStdout, big)
	WriteFrame(&buf, FrameStdin, nil)
	WriteFrame(&buf, FrameExit, []byte(`{"code":3}`))
	if err := WriteFrame(&buf, 9, nil); err == nil {
		t.Error("stream 9 was written")
	}
	b := make([]byte, MaxFrame)
	var got []byte
	for _, want := range []struct {
		s byte
		n int
	}{{FrameStdout, MaxFrame}, {FrameStdout, 10}, {FrameStdin, 0}, {FrameExit, 10}} {
		s, n, err := ReadFrame(&buf, b)
		if err != nil || s != want.s || n != want.n {
			t.Fatalf("frame %d of %d: %v, want %d of %d", s, n, err, want.s, want.n)
		}
		if s == FrameStdout {
			got = append(got, b[:n]...)
		}
	}
	if !bytes.Equal(got, big) {
		t.Error("the payload changed")
	}
	if _, _, err := ReadFrame(&buf, b); err != io.EOF {
		t.Errorf("after the last frame: %v", err)
	}
	if _, _, err := ReadFrame(bytes.NewReader([]byte{7, 0, 0, 0, 0}), b); err == nil {
		t.Error("stream 7 was read")
	}
	if _, _, err := ReadFrame(bytes.NewReader([]byte{1, 0, 2, 0, 0}), b); err == nil {
		t.Error("a frame over the bound was read")
	}
}

func TestAnExitIsTheStatusAShellGives(t *testing.T) {
	for _, c := range []struct {
		e    Exit
		want int
	}{{Exit{Code: 0}, 0}, {Exit{Code: 200}, 200}, {Exit{Signal: 9}, 137}, {Exit{Code: 127, Error: "no such"}, 127}} {
		if got := c.e.Status(); got != c.want {
			t.Errorf("%+v: %d, want %d", c.e, got, c.want)
		}
	}
}

func TestTheWorkingDirectoryIsTheMacs(t *testing.T) {
	for _, c := range []struct {
		dir   string
		share bool
		want  string
	}{
		{"/mnt/mac/tmp/x", true, "/tmp/x"},
		{"/mnt/mac", true, "/"},
		{"/mnt/mac/", true, "/"},
		{"/mnt/macx", true, ""},
		{"/Users/me/src", true, "/Users/me/src"},
		{"/private/tmp", true, "/private/tmp"},
		{"/Volumes/USB", true, "/Volumes/USB"},
		{"/home/me", true, ""},
		{"/", true, ""},
		{"/tmp", true, ""},
		{"/Users/me/src", false, ""},
		{"relative", true, ""},
		{"", true, ""},
		{"/mnt/mac/a/../b", true, "/b"},
	} {
		got, ok := MacDir(c.dir, "/", c.share)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("MacDir(%q, share %v) = %q %v, want %q", c.dir, c.share, got, ok, c.want)
		}
	}
}

func TestArgumentsUnderTheShareAreTheMacs(t *testing.T) {
	for _, c := range []struct{ arg, want string }{
		{"/mnt/mac/Users/me/f.txt", "/Users/me/f.txt"},
		{"/mnt/mac", "/"},
		{"--file=/mnt/mac/tmp/x", "--file=/tmp/x"},
		{"-f", "-f"},
		{"--file", "--file"},
		{"/Users/me/f", "/Users/me/f"},
		{"/etc/hosts", "/etc/hosts"},
		{"https://example.com/mnt/mac/x", "https://example.com/mnt/mac/x"},
		{"/mnt/macx/y", "/mnt/macx/y"},
		{"/mnt/mac/../etc", "/mnt/mac/../etc"},
		{"relative/mnt/mac", "relative/mnt/mac"},
	} {
		if got := MacArg(c.arg, "/", true); got != c.want {
			t.Errorf("MacArg(%q) = %q, want %q", c.arg, got, c.want)
		}
	}
	if got := MacArg("/mnt/mac/x", "/", false); got != "/mnt/mac/x" {
		t.Errorf("without the share: %q", got)
	}
	if got := MacArg("--in=/mnt/mac/Users/t", "/tmp/m", true); got != "--in=/tmp/m/Users/t" {
		t.Errorf("under a share root: %q", got)
	}
	if got, _ := MacDir("/Users/t/src", "/tmp/m", true); got != "/tmp/m/Users/t/src" {
		t.Errorf("a same-path folder under a share root: %q", got)
	}
	if got, _ := MacDir("/mnt/mac", "/tmp/m", true); got != "/tmp/m" {
		t.Errorf("the share under a share root: %q", got)
	}
}

func TestAMacPathIsFoundInTheMachine(t *testing.T) {
	const home = "/Users/me"
	for _, c := range []struct{ mac, want string }{
		{"/Users/me/f", "/Users/me/f"},
		{"f", "/Users/me/f"},
		{"Downloads/a.zip", "/Users/me/Downloads/a.zip"},
		{"/tmp/x", "/mnt/mac/tmp/x"},
		{"/opt/homebrew", "/mnt/mac/opt/homebrew"},
		{"/private/tmp/x", "/private/tmp/x"},
		{"/", "/mnt/mac"},
	} {
		if got, ok := LinuxPath(c.mac, home, "/"); got != c.want || !ok {
			t.Errorf("LinuxPath(%q) = %q, want %q", c.mac, got, c.want)
		}
	}
	for _, c := range []struct{ mac, want string }{
		{"/tmp/m/Users/t/f", "/Users/t/f"},
		{"f", "/Users/t/f"},
		{"/tmp/m/opt/x", "/mnt/mac/opt/x"},
		{"/tmp/m", "/mnt/mac"},
	} {
		if got, ok := LinuxPath(c.mac, "/tmp/m/Users/t", "/tmp/m"); got != c.want || !ok {
			t.Errorf("under a share root, LinuxPath(%q) = %q, want %q", c.mac, got, c.want)
		}
	}
	if _, ok := LinuxPath("/etc/hosts", "/tmp/m/Users/t", "/tmp/m"); ok {
		t.Error("a path outside the share's root was found in the machine")
	}
}

func TestLinkNames(t *testing.T) {
	for _, n := range append(append([]string{}, Defaults...), "sw_vers", "pbcopy", "a.b", "x-y") {
		if err := CheckLinkName(n); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	for _, n := range []string{"", ".", "..", "a/b", "a\x00", "a\tb", strings.Repeat("x", 256)} {
		if CheckLinkName(n) == nil {
			t.Errorf("%q was taken", n)
		}
	}
	for _, n := range []string{"open", "osascript", "code", "mac", "macctl"} {
		if !Builtin(n) {
			t.Errorf("%s is not built in", n)
		}
	}
	if Builtin("sw_vers") {
		t.Error("sw_vers is built in")
	}
	if len(Defaults) != 14 {
		t.Errorf("%d default links; the helper set has 14", len(Defaults))
	}
}

func TestTheAgentsMacPathTravelsOnTheCommandLine(t *testing.T) {
	for _, p := range []string{
		"/private/tmp/com.apple.launchd.AbC/Listeners",
		"/Users/me/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock",
		"/Users/me/.ssh/a=b%c.sock",
	} {
		w := AgentPathWord(p)
		k, v, ok := strings.Cut(w, "=")
		if !ok || k != AgentPathParam || strings.ContainsAny(v, " \t\n") {
			t.Errorf("%q: the word %q", p, w)
			continue
		}
		got, ok := AgentPath(v)
		if !ok || got != p {
			t.Errorf("%q came back %q (%v)", p, got, ok)
		}
	}
	if AgentPathWord("relative/sock") != "" {
		t.Error("a relative path made a word")
	}
	for _, v := range []string{"relative", "%2", "/a/../b", "/", "/a%00b"} {
		if _, ok := AgentPath(v); ok {
			t.Errorf("%q was taken", v)
		}
	}
}

func TestTheViewTranslatesAMachinesOwnPaths(t *testing.T) {
	view := "/Users/u/Fevom/m"
	for _, c := range []struct {
		dir, want string
		ok        bool
	}{
		{"/home/u/proj", view + "/home/u/proj", true},
		{"/", view, true},
		{"/home/u/../u/./p", view + "/home/u/p", true},
		{"/mnt/mac/Users/u/src", "/Users/u/src", true},
		{"/Users/u/src", "/Users/u/src", true},
		{"relative", "", false},
	} {
		got, ok := MacDirIn(c.dir, "/", true, view)
		if got != c.want || ok != c.ok {
			t.Errorf("MacDirIn(%q) = %q %v, want %q %v", c.dir, got, ok, c.want, c.ok)
		}
	}
	if got, ok := MacDirIn("/home/u/proj", "/", true, ""); ok || got != "" {
		t.Errorf("no view: %q %v", got, ok)
	}
	if got, ok := MacDirIn("/home/u", "/", false, view); !ok || got != view+"/home/u" {
		t.Errorf("no share: %q %v", got, ok)
	}
	for _, c := range []struct{ mac, want string }{
		{view + "/home/u/f", "/home/u/f"},
		{view, "/"},
		{"Fevom/m/etc/hosts", "/etc/hosts"},
		{"/Users/u/x", "/Users/u/x"},
		{"/tmp/y", "/mnt/mac/tmp/y"},
		{"/Users/u/Fevom/mm/x", "/Users/u/Fevom/mm/x"},
	} {
		got, ok := LinuxPathIn(c.mac, "/Users/u", "/", view)
		if !ok || got != c.want {
			t.Errorf("LinuxPathIn(%q) = %q %v, want %q", c.mac, got, ok, c.want)
		}
	}
	if got, _ := LinuxPathIn(view+"/home/u/f", "/Users/u", "/", ""); got != view+"/home/u/f" {
		t.Errorf("no view: %q", got)
	}
}

func TestAMacProgramRunByPathIsTheMacsFile(t *testing.T) {
	view := "/Users/u/Fevom/m"
	for _, c := range []struct {
		p, view, want string
		share, ok     bool
	}{
		{"/mnt/mac/usr/bin/sw_vers", "", "/usr/bin/sw_vers", true, true},
		{"/mnt/mac/bin/echo", view, "/bin/echo", true, true},
		{"/Users/u/bin/tool", "", "/Users/u/bin/tool", true, true},
		{"/Applications/X.app/Contents/MacOS/X", "", "/Applications/X.app/Contents/MacOS/X", true, true},
		{"/home/u/a.out", view, view + "/home/u/a.out", true, true},
		{"/home/u/a.out", "", "", true, false},
		{"/usr/bin/sw_vers", "", "", true, false},
		{"/mnt/mac/bin/echo", "", "", false, false},
		{"bin/echo", view, "", true, false},
	} {
		got, ok := MacProgram(c.p, "/", c.share, c.view)
		if got != c.want || ok != c.ok {
			t.Errorf("MacProgram(%q, share %v, view %q) = %q %v, want %q %v", c.p, c.share, c.view, got, ok, c.want, c.ok)
		}
	}
	if got, ok := MacProgram("/mnt/mac/usr/bin/sw_vers", "/tmp/s/mac", true, ""); !ok || got != "/tmp/s/mac/usr/bin/sw_vers" {
		t.Errorf("under a scratch root: %q %v", got, ok)
	}
}
