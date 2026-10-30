package fsevents

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"fev/proto/contract"
	"fev/proto/version"
)

var update = flag.Bool("update", false, "rewrite the fixtures from the tables in this file")

type stream struct {
	name   string
	frames []Frame
}

var long = strings.Repeat("n", MaxName)

var valid = []stream{
	{"guest-hello", []Frame{Hello{Version, "v0.3.0-41-gde2d160-dirty.5c0ffee1"}}},
	{"guest-hello-unstamped", []Frame{Hello{Version, ""}}},
	{"host-hello", []Frame{Hello{Version, "v0.3.0-41-gde2d160-dirty.5c0ffee1"}}},
	{"host-hello-other-version", []Frame{Hello{2, "v0.4.0"}}},
	{"guest-watch", []Frame{Watch{1, []string{
		"/Users/someone/src/app",
		"/private/tmp/tmp.Xq3 f",
		"/Volumes/Données/projet",
		"/",
	}}}},
	{"guest-watch-empty", []Frame{Watch{2, nil}}},
	{"host-watching", []Frame{Watching{1, []Status{StatusOK, StatusOutside, StatusMissing, StatusFailed}}}},
	{"host-watching-empty", []Frame{Watching{2, nil}}},
	{"host-events-change", []Frame{Events{[]Event{
		{Op: OpChange, Mask: Content, Parent: 12, Child: 57, Name: "main.go"},
	}}}},
	{"host-events-attrib", []Frame{Events{[]Event{
		{Op: OpChange, Mask: MaskAttrib, Parent: 12, Child: 57, Name: "main.go"},
		{Op: OpChange, Mask: MaskAttrib, Flags: FlagIsDir, Child: 12},
		{Op: OpChange, Flags: FlagQuiet, Child: 57},
	}}}},
	{"host-events-create", []Frame{Events{[]Event{
		{Op: OpCreate, Parent: 12, Name: "new.txt"},
		{Op: OpChange, Mask: Content, Parent: 12, Name: "new.txt"},
		{Op: OpCreate, Flags: FlagIsDir, Parent: 12, Name: "sub"},
	}}}},
	{"host-events-delete", []Frame{Events{[]Event{
		{Op: OpDelete, Flags: FlagLastLink, Parent: 12, Child: 57, Name: "main.go"},
		{Op: OpDelete, Parent: 12, Child: 58, Name: "hardlink"},
		{Op: OpDelete, Flags: FlagIsDir | FlagLastLink, Parent: 12, Child: 60, Name: "sub"},
		{Op: OpDelete, Flags: FlagLastLink, Child: 61, Name: "orphan"},
	}}}},
	{"host-events-rename", []Frame{Events{[]Event{
		{Op: OpRename, Parent: 12, Child: 57, Parent2: 14, Name: "main.go", Name2: "main_old.go"},
		{Op: OpRename, Flags: FlagIsDir, Parent: 1, Child: 12, Parent2: 1, Name: "src", Name2: "source"},
	}}}},
	{"host-events-replace", []Frame{Events{[]Event{
		{Op: OpRename, Flags: FlagVictimGone, Parent: 12, Parent2: 12, Victim: 57, Name: ".main.go.swp", Name2: "main.go"},
		{Op: OpChange, Mask: Content, Parent: 12, Name: "main.go"},
	}}}},
	{"host-events-inval-all", []Frame{Events{[]Event{{Op: OpInvalAll}}}}},
	{"host-events-names", []Frame{Events{[]Event{
		{Op: OpCreate, Parent: 1<<64 - 1, Name: long},
		{Op: OpRename, Parent: 2, Child: 1<<63 + 5, Parent2: 3, Victim: 1 << 40, Name: long, Name2: long},
		{Op: OpCreate, Parent: 2, Name: `a "quoted\name`},
		{Op: OpCreate, Parent: 2, Name: "caf\xc3\xa9 \xff\x01"},
		{Op: OpCreate, Parent: 2, Name: "..."},
	}}}},
	{"guest-session", []Frame{
		Hello{Version, "v0.3.0"},
		Watch{1, []string{"/Users/someone/src/app"}},
		Watch{2, []string{"/Users/someone/src/app", "/Users/someone/src/lib"}},
		Watch{3, nil},
	}},
	{"host-session", []Frame{
		Hello{Version, "v0.3.0"},
		Watching{1, []Status{StatusOK}},
		Events{[]Event{{Op: OpChange, Mask: Content, Parent: 12, Child: 57, Name: "main.go"}}},
		Watching{2, []Status{StatusOK, StatusMissing}},
		Events{[]Event{{Op: OpInvalAll}}},
		Events{[]Event{
			{Op: OpCreate, Parent: 12, Name: "a"},
			{Op: OpDelete, Flags: FlagLastLink, Parent: 12, Child: 70, Name: "b"},
		}},
		Watching{3, nil},
	}},
}

func raw(t byte, payload []byte) []byte {
	b := le.AppendUint32(nil, uint32(len(payload)))
	return append(append(b, t, 0, 0, 0), payload...)
}

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func u16(v uint16) []byte { return le.AppendUint16(nil, v) }
func u32(v uint32) []byte { return le.AppendUint32(nil, v) }

func hello(magic string, v uint16, n uint16, build string) []byte {
	return raw(TypeHello, cat([]byte(magic), u16(v), u16(n), []byte(build)))
}

func watch(seq uint32, count uint16, paths ...string) []byte {
	p := cat(u32(seq), u16(count), u16(0))
	for _, path := range paths {
		p = cat(p, u16(uint16(len(path))), []byte(path))
	}
	return raw(TypeWatch, p)
}

func events(count uint16, records ...[]byte) []byte {
	return raw(TypeEvents, cat(u16(count), u16(0), cat(records...)))
}

func rec(e Event) []byte { return e.appendRecord(nil) }

func with(b []byte, at int, v byte) []byte {
	b = bytes.Clone(b)
	b[at] = v
	return b
}

func ok(f Frame) []byte {
	b, err := Append(nil, f)
	if err != nil {
		panic(err)
	}
	return b
}

var okEvent = Event{Op: OpChange, Mask: Content, Parent: 12, Child: 57, Name: "main.go"}

type badStream struct {
	name  string
	bytes []byte
}

var invalid = []badStream{
	{"guest-type-unknown", raw(9, nil)},
	{"guest-type-zero", raw(0, nil)},
	{"guest-header-padding", with(ok(Hello{Version, "v1"}), 6, 1)},
	{"guest-hello-too-long", raw(TypeHello, make([]byte, MaxHello+1))},
	{"guest-watch-over-limit-header-only", cat(u32(MaxWatch+1), []byte{TypeWatch, 0, 0, 0})},
	{"host-events-over-limit-header-only", cat(u32(MaxEvents+1), []byte{TypeEvents, 0, 0, 0})},
	{"host-length-huge-header-only", cat(u32(0xffffffff), []byte{TypeHello, 0, 0, 0})},
	{"guest-sends-watching", ok(Watching{1, []Status{StatusOK}})},
	{"guest-sends-events", ok(Events{[]Event{okEvent}})},
	{"host-sends-watch", ok(Watch{1, []string{"/a"}})},
	{"guest-hello-bad-magic", hello("BFSX", Version, 2, "v1")},
	{"guest-hello-short", raw(TypeHello, []byte("BFSE\x01"))},
	{"guest-hello-build-length-over", hello(Magic, Version, 5, "v1")},
	{"guest-hello-build-length-under", hello(Magic, Version, 1, "v1")},
	{"guest-hello-build-space", hello(Magic, Version, 3, "v 1")},
	{"guest-hello-build-control", hello(Magic, Version, 3, "v\n1")},
	{"guest-hello-build-high", hello(Magic, Version, 3, "v\xc3\xa9")},
	{"guest-watch-seq-zero", watch(0, 1, "/a")},
	{"guest-watch-short", raw(TypeWatch, u32(1))},
	{"guest-watch-padding", with(watch(1, 1, "/a"), HeaderLen+6, 1)},
	{"guest-watch-count-over", watch(1, 2, "/a")},
	{"guest-watch-count-under", watch(1, 1, "/a", "/b")},
	{"guest-watch-count-1025", watch(1, MaxPaths+1)},
	{"guest-watch-count-huge-no-paths", watch(1, 1000)},
	{"guest-watch-path-cut", raw(TypeWatch, cat(u32(1), u16(1), u16(0), u16(9), []byte("/a")))},
	{"guest-watch-path-empty", watch(1, 1, "")},
	{"guest-watch-path-relative", watch(1, 1, "Users/someone")},
	{"guest-watch-path-dotdot", watch(1, 1, "/Users/../etc")},
	{"guest-watch-path-dot", watch(1, 1, "/Users/./x")},
	{"guest-watch-path-double-slash", watch(1, 1, "/Users//x")},
	{"guest-watch-path-trailing-slash", watch(1, 1, "/Users/x/")},
	{"guest-watch-path-nul", watch(1, 1, "/Users/x\x00y")},
	{"guest-watch-path-not-utf8", watch(1, 1, "/Users/\xff")},
	{"guest-watch-path-too-long", watch(1, 1, "/"+strings.Repeat("p", MaxPath))},
	{"host-watching-seq-zero", raw(TypeWatching, cat(u32(0), u16(1), u16(0), []byte{0}))},
	{"host-watching-status-unknown", raw(TypeWatching, cat(u32(1), u16(1), u16(0), []byte{4}))},
	{"host-watching-count-over", raw(TypeWatching, cat(u32(1), u16(2), u16(0), []byte{0}))},
	{"host-watching-count-under", raw(TypeWatching, cat(u32(1), u16(1), u16(0), []byte{0, 0}))},
	{"host-watching-padding", raw(TypeWatching, cat(u32(1), u16(0), u16(7)))},
	{"host-events-short", raw(TypeEvents, u16(1))},
	{"host-events-none", events(0)},
	{"host-events-padding", raw(TypeEvents, cat(u16(1), u16(1), rec(okEvent)))},
	{"host-events-count-over", events(2, rec(okEvent))},
	{"host-events-count-under", events(1, rec(okEvent), rec(okEvent))},
	{"host-events-record-cut", events(1, rec(okEvent)[:RecordLen-1])},
	{"host-events-name-cut", events(1, rec(okEvent)[:RecordLen+3])},
	{"host-events-trailing-byte", events(1, rec(okEvent), []byte{0})},
	{"host-record-padding", events(1, with(rec(okEvent), 3, 1))},
	{"host-record-op-zero", events(1, rec(Event{Parent: 1, Name: "a"}))},
	{"host-record-op-unknown", events(1, rec(Event{Op: 6, Parent: 1, Name: "a"}))},
	{"host-record-flags-unknown", events(1, rec(Event{Op: OpCreate, Flags: 1 << 4, Parent: 1, Name: "a"}))},
	{"host-record-mask-unknown", events(1, rec(Event{Op: OpChange, Mask: 1 << 4, Child: 1}))},
	{"host-record-name-256", events(1, rec(Event{Op: OpCreate, Parent: 1, Name: long + "n"}))},
	{"host-record-name-slash", events(1, rec(Event{Op: OpCreate, Parent: 1, Name: "a/b"}))},
	{"host-record-name-nul", events(1, rec(Event{Op: OpCreate, Parent: 1, Name: "a\x00b"}))},
	{"host-record-name-dot", events(1, rec(Event{Op: OpCreate, Parent: 1, Name: "."}))},
	{"host-record-name2-dotdot", events(1, rec(Event{Op: OpRename, Parent: 1, Parent2: 1, Name: "a", Name2: ".."}))},
	{"host-change-names-nothing", events(1, rec(Event{Op: OpChange, Mask: Content, Parent: 1}))},
	{"host-change-last-link", events(1, rec(Event{Op: OpChange, Flags: FlagLastLink, Child: 1}))},
	{"host-change-second-name", events(1, rec(Event{Op: OpChange, Child: 1, Name: "a", Name2: "b"}))},
	{"host-create-no-parent", events(1, rec(Event{Op: OpCreate, Name: "a"}))},
	{"host-create-no-name", events(1, rec(Event{Op: OpCreate, Parent: 1}))},
	{"host-create-child", events(1, rec(Event{Op: OpCreate, Parent: 1, Child: 2, Name: "a"}))},
	{"host-create-mask", events(1, rec(Event{Op: OpCreate, Mask: MaskModify, Parent: 1, Name: "a"}))},
	{"host-create-last-link", events(1, rec(Event{Op: OpCreate, Flags: FlagLastLink, Parent: 1, Name: "a"}))},
	{"host-delete-no-name", events(1, rec(Event{Op: OpDelete, Parent: 1, Child: 2}))},
	{"host-delete-no-nodes", events(1, rec(Event{Op: OpDelete, Name: "a"}))},
	{"host-delete-victim", events(1, rec(Event{Op: OpDelete, Parent: 1, Victim: 3, Name: "a"}))},
	{"host-delete-victim-gone", events(1, rec(Event{Op: OpDelete, Flags: FlagVictimGone, Parent: 1, Name: "a"}))},
	{"host-rename-one-name", events(1, rec(Event{Op: OpRename, Parent: 1, Parent2: 1, Name: "a"}))},
	{"host-rename-no-nodes", events(1, rec(Event{Op: OpRename, Victim: 9, Name: "a", Name2: "b"}))},
	{"host-rename-last-link", events(1, rec(Event{Op: OpRename, Flags: FlagLastLink, Parent: 1, Parent2: 1, Name: "a", Name2: "b"}))},
	{"host-rename-mask", events(1, rec(Event{Op: OpRename, Mask: MaskAttrib, Parent: 1, Parent2: 1, Name: "a", Name2: "b"}))},
	{"host-inval-all-with-node", events(1, rec(Event{Op: OpInvalAll, Parent: 1}))},
	{"host-inval-all-quiet", events(1, rec(Event{Op: OpInvalAll, Flags: FlagQuiet}))},
	{"host-second-frame-bad", cat(ok(Hello{Version, "v1"}), events(0))},
}

var misordered = []stream{
	{"guest-watch-before-hello", []Frame{Watch{1, []string{"/a"}}}},
	{"guest-hello-twice", []Frame{Hello{Version, "v1"}, Hello{Version, "v1"}}},
	{"guest-hello-after-watch", []Frame{Hello{Version, "v1"}, Watch{1, nil}, Hello{Version, "v1"}}},
	{"guest-watch-seq-starts-at-2", []Frame{Hello{Version, "v1"}, Watch{2, nil}}},
	{"guest-watch-seq-skips", []Frame{Hello{Version, "v1"}, Watch{1, nil}, Watch{3, nil}}},
	{"guest-watch-seq-repeats", []Frame{Hello{Version, "v1"}, Watch{1, nil}, Watch{1, nil}}},
	{"guest-watch-seq-goes-back", []Frame{Hello{Version, "v1"}, Watch{1, nil}, Watch{2, nil}, Watch{1, nil}}},
	{"host-events-before-hello", []Frame{Events{[]Event{{Op: OpInvalAll}}}}},
	{"host-watching-before-hello", []Frame{Watching{1, nil}}},
	{"host-hello-twice", []Frame{Hello{Version, "v1"}, Hello{Version, "v1"}}},
	{"host-watching-seq-starts-at-2", []Frame{Hello{Version, "v1"}, Watching{2, nil}}},
	{"host-watching-seq-skips", []Frame{Hello{Version, "v1"}, Watching{1, nil}, Events{[]Event{okEvent}}, Watching{3, nil}}},
	{"host-watching-seq-repeats", []Frame{Hello{Version, "v1"}, Watching{1, nil}, Watching{1, nil}}},
}

func inOrder(from Side, frames []Frame) error {
	o := NewOrder(from)
	for _, f := range frames {
		if err := o.Check(f); err != nil {
			return err
		}
	}
	return nil
}

func TestOrderIsPartOfTheFormat(t *testing.T) {
	sessions := 0
	for _, s := range valid {
		if !strings.HasSuffix(s.name, "-session") {
			continue
		}
		sessions++
		if err := inOrder(side(t, s.name), s.frames); err != nil {
			t.Errorf("%s: %v", s.name, err)
		}
	}
	if sessions < 2 {
		t.Errorf("%d session fixtures", sessions)
	}
	var names []string
	for _, s := range misordered {
		names = append(names, s.name)
		from := side(t, s.name)
		data := read(t, "fixtures/order/"+s.name+".bin")
		if !bytes.Equal(data, encodeAll(t, s.frames)) {
			t.Errorf("order/%s.bin is not what the table encodes to; run go test ./fsevents -update", s.name)
		}
		frames, err := decodeAll(from, data)
		if err != nil {
			t.Errorf("order/%s: %v", s.name, err)
			continue
		}
		if err := inOrder(from, frames[:len(frames)-1]); err != nil {
			t.Errorf("order/%s is refused before its last frame: %v", s.name, err)
		}
		if err := inOrder(from, frames); err == nil {
			t.Errorf("order/%s: accepted", s.name)
		}
	}
	sort.Strings(names)
	if on := files(t, "fixtures/order"); strings.Join(on, " ") != strings.Join(names, " ") {
		t.Errorf("fixtures/order holds %v, the table %v", on, names)
	}
	o := NewOrder(FromGuest)
	o.Check(Hello{Version, ""})
	o.seq = 1<<32 - 1
	for _, seq := range []uint32{0, 1} {
		if o.Check(Watch{Seq: seq}) == nil {
			t.Errorf("seq %d was taken after the largest", seq)
		}
	}
}

func TestAWatchingAnswersItsWatch(t *testing.T) {
	sent := Watch{4, []string{"/a", "/b"}}
	if err := (Watching{4, []Status{StatusOK, StatusMissing}}).Answers(sent); err != nil {
		t.Error(err)
	}
	if (Watching{5, []Status{StatusOK, StatusMissing}}).Answers(sent) == nil {
		t.Error("an acknowledgement of another set was taken")
	}
	if (Watching{4, []Status{StatusOK}}).Answers(sent) == nil {
		t.Error("an acknowledgement of fewer paths was taken")
	}
	if err := (Watching{1, nil}).Answers(Watch{1, nil}); err != nil {
		t.Error(err)
	}
}

func side(t *testing.T, name string) Side {
	t.Helper()
	switch {
	case strings.HasPrefix(name, "guest-"):
		return FromGuest
	case strings.HasPrefix(name, "host-"):
		return FromHost
	}
	t.Fatalf("%s: a fixture's name begins with guest- or host-", name)
	return 0
}

func decodeAll(from Side, b []byte) ([]Frame, error) {
	var out []Frame
	for len(b) > 0 {
		f, n, err := Decode(from, b)
		if err != nil {
			return out, err
		}
		out, b = append(out, f), b[n:]
	}
	return out, nil
}

func describeAll(frames []Frame) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString(Describe(f))
	}
	return b.String()
}

func encodeAll(t *testing.T, frames []Frame) []byte {
	t.Helper()
	var b []byte
	for _, f := range frames {
		var err error
		if b, err = Append(b, f); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func files(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".bin"); ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMain(m *testing.M) {
	flag.Parse()
	if *update {
		for _, dir := range []string{"fixtures", "fixtures/bad", "fixtures/order"} {
			old, _ := filepath.Glob(dir + "/*.bin")
			txt, _ := filepath.Glob(dir + "/*.txt")
			for _, f := range append(old, txt...) {
				os.Remove(f)
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				panic(err)
			}
		}
		for _, s := range valid {
			var b []byte
			for _, f := range s.frames {
				b = append(b, ok(f)...)
			}
			must(os.WriteFile("fixtures/"+s.name+".bin", b, 0o644))
			must(os.WriteFile("fixtures/"+s.name+".txt", []byte(describeAll(s.frames)), 0o644))
		}
		for _, s := range invalid {
			must(os.WriteFile("fixtures/bad/"+s.name+".bin", s.bytes, 0o644))
		}
		for _, s := range misordered {
			var b []byte
			for _, f := range s.frames {
				b = append(b, ok(f)...)
			}
			must(os.WriteFile("fixtures/order/"+s.name+".bin", b, 0o644))
		}
	}
	os.Exit(m.Run())
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func TestFixturesDecodeDescribeAndEncodeBack(t *testing.T) {
	var names []string
	for _, s := range valid {
		names = append(names, s.name)
		from := side(t, s.name)
		data := read(t, "fixtures/"+s.name+".bin")
		if want := encodeAll(t, s.frames); !bytes.Equal(data, want) {
			t.Errorf("%s.bin is not what the table encodes to; run go test ./fsevents -update", s.name)
			continue
		}
		frames, err := decodeAll(from, data)
		if err != nil {
			t.Errorf("%s: %v", s.name, err)
			continue
		}
		if got, want := describeAll(frames), string(read(t, "fixtures/"+s.name+".txt")); got != want {
			t.Errorf("%s decodes to\n%swhere its text says\n%s", s.name, got, want)
		}
		if again := encodeAll(t, frames); !bytes.Equal(again, data) {
			t.Errorf("%s does not encode back to its own bytes", s.name)
		}
	}
	sort.Strings(names)
	if on := files(t, "fixtures"); strings.Join(on, " ") != strings.Join(names, " ") {
		t.Errorf("fixtures/ holds %v, the table %v", on, names)
	}
}

func TestBadFixturesAreRefused(t *testing.T) {
	var names []string
	for _, s := range invalid {
		names = append(names, s.name)
		data := read(t, "fixtures/bad/"+s.name+".bin")
		if !bytes.Equal(data, s.bytes) {
			t.Errorf("bad/%s.bin is not what the table holds; run go test ./fsevents -update", s.name)
		}
		_, err := decodeAll(side(t, s.name), data)
		if err == nil || errors.Is(err, ErrShort) {
			t.Errorf("bad/%s: decoded with %v", s.name, err)
		}
		if _, err := readAll(side(t, s.name), data); err == nil || errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("bad/%s: read with %v", s.name, err)
		}
	}
	sort.Strings(names)
	if on := files(t, "fixtures/bad"); strings.Join(on, " ") != strings.Join(names, " ") {
		t.Errorf("fixtures/bad holds %v, the table %v", on, names)
	}
}

func readAll(from Side, data []byte) ([]Frame, error) {
	r := bytes.NewReader(data)
	var out []Frame
	for {
		f, err := Read(r, from)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, f)
	}
}

func TestEveryPrefixOfAFrameIsShort(t *testing.T) {
	for _, s := range valid {
		from := side(t, s.name)
		whole := ok(s.frames[0])
		for n := range whole {
			if _, _, err := Decode(from, whole[:n]); !errors.Is(err, ErrShort) {
				t.Fatalf("%s: %d of %d bytes: %v", s.name, n, len(whole), err)
			}
			_, err := readAll(from, whole[:n])
			if n == 0 && err != nil || n > 0 && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("%s: a stream of %d of %d bytes: %v", s.name, n, len(whole), err)
			}
		}
	}
}

func TestReadingAStreamGivesTheSameFrames(t *testing.T) {
	for _, s := range valid {
		frames, err := readAll(side(t, s.name), encodeAll(t, s.frames))
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if got, want := describeAll(frames), describeAll(s.frames); got != want {
			t.Errorf("%s read as\n%swant\n%s", s.name, got, want)
		}
	}
}

func TestTheEncoderRefusesWhatTheDecoderWould(t *testing.T) {
	many := make([]string, MaxPaths+1)
	for i := range many {
		many[i] = "/p"
	}
	fat := make([]string, MaxPaths)
	for i := range fat {
		fat[i] = "/" + strings.Repeat("p", 300)
	}
	for name, f := range map[string]Frame{
		"build with a space": Hello{Version, "v 1"},
		"build too long":     Hello{Version, strings.Repeat("b", MaxBuild+1)},
		"seq 0":              Watch{0, nil},
		"too many paths":     Watch{1, many},
		"set too large":      Watch{1, fat},
		"relative path":      Watch{1, []string{"a"}},
		"ack seq 0":          Watching{0, nil},
		"ack unknown status": Watching{1, []Status{9}},
		"no records":         Events{nil},
		"bad record":         Events{[]Event{{Op: OpCreate}}},
		"too many records":   Events{make([]Event, MaxEvents/RecordLen+1)},
		"records over a frame": Events{func() []Event {
			r := make([]Event, 200)
			for i := range r {
				r[i] = Event{Op: OpRename, Parent: 1, Parent2: 1, Name: long, Name2: long}
			}
			return r
		}()},
	} {
		dst := []byte("kept")
		out, err := Append(dst, f)
		if err == nil {
			t.Errorf("%s: encoded", name)
		}
		if string(out) != "kept" {
			t.Errorf("%s: dst became %q", name, out)
		}
	}
}

func TestALargeBatchIsSplitInOrder(t *testing.T) {
	records := make([]Event, 500)
	for i := range records {
		records[i] = Event{Op: OpRename, Parent: uint64(i + 1), Parent2: 1, Name: long, Name2: long}
	}
	b, err := AppendEvents(nil, records)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := decodeAll(FromHost, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) < 2 {
		t.Fatalf("%d bytes of records in %d frame", len(b), len(frames))
	}
	var got []Event
	for _, f := range frames {
		got = append(got, f.(Events).Records...)
	}
	if len(got) != len(records) {
		t.Fatalf("%d records arrived of %d", len(got), len(records))
	}
	for i := range got {
		if got[i] != records[i] {
			t.Fatalf("record %d arrived as %+v", i, got[i])
		}
	}
	if b, err := AppendEvents([]byte("x"), nil); err != nil || string(b) != "x" {
		t.Errorf("no records encoded as %q, %v", b, err)
	}
}

func TestHelloChecksVersionAndBuild(t *testing.T) {
	if err := (Hello{Version, "v1"}).Check("v1", "VMM"); err != nil {
		t.Error(err)
	}
	if err := (Hello{Version, ""}).Check("v1", "guest"); err != nil {
		t.Error(err)
	}
	var m *version.Mismatch
	if err := (Hello{Version, "v2"}).Check("v1", "VMM"); !errors.As(err, &m) || m.Ours != "v1" || m.Theirs != "v2" {
		t.Errorf("another build: %v", err)
	}
	if err := (Hello{Version + 1, "v1"}).Check("v1", "VMM"); err == nil || errors.As(err, &m) {
		t.Errorf("another version: %v", err)
	}
}

func TestTheIoctlLayoutIsTheKernelsStructure(t *testing.T) {
	e := Event{Op: OpRename, Flags: FlagIsDir | FlagVictimGone, Parent: 0x1111, Child: 0x2222,
		Parent2: 0x3333, Victim: 0x4444, Name: long, Name2: "b"}
	b := e.Ioctl(7)
	for _, c := range []struct {
		what string
		at   int
		want uint64
		size int
	}{
		{"op", 0, 4, 4}, {"flags", 4, 9, 4}, {"fd", 8, 7, 4}, {"mask", 12, 0, 4},
		{"parent", 16, 0x1111, 8}, {"child", 24, 0x2222, 8}, {"parent2", 32, 0x3333, 8}, {"victim", 40, 0x4444, 8},
		{"namelen", 48, MaxName, 2}, {"namelen2", 50, 1, 2}, {"reserved", 52, 0, 4},
	} {
		var got uint64
		switch c.size {
		case 2:
			got = uint64(le.Uint16(b[c.at:]))
		case 4:
			got = uint64(le.Uint32(b[c.at:]))
		default:
			got = le.Uint64(b[c.at:])
		}
		if got != c.want {
			t.Errorf("%s at %d is %#x, want %#x", c.what, c.at, got, c.want)
		}
	}
	if string(b[56:56+MaxName]) != long || b[56+MaxName] != 0 {
		t.Error("name is not at 56, terminated")
	}
	if b[312] != 'b' || b[313] != 0 {
		t.Error("name2 is not at 312, terminated")
	}
	if IoctlEmit != 0x42386201 {
		t.Errorf("FEV_FSN_EMIT is %#x", IoctlEmit)
	}
}

func TestTheConstantsAreTheKernelHeaders(t *testing.T) {
	const rel = "include/uapi/linux/fev_fsnotify.h"
	if _, err := os.Stat("../../kernel"); err != nil {
		t.Skip("no kernel directory beside proto: not in the fev tree")
	}
	src, err := os.ReadFile("../../kernel/src/" + rel)
	if err != nil {
		t.Fatalf("the kernel interface's header: %v", err)
	}
	defines := map[string]uint64{}
	for _, m := range regexp.MustCompile(`(?m)^#define\s+(FEV_FSN_\w+)\s+(?:\(1 << (\d+)\)|(\d+))`).FindAllStringSubmatch(string(src), -1) {
		if m[2] != "" {
			shift, _ := strconv.Atoi(m[2])
			defines[m[1]] = 1 << shift
		} else {
			defines[m[1]], _ = strconv.ParseUint(m[3], 10, 64)
		}
	}
	want := map[string]uint64{
		"FEV_FSN_NAME_MAX": MaxName,
		"FEV_FSN_CHANGE":   uint64(OpChange), "FEV_FSN_CREATE": uint64(OpCreate),
		"FEV_FSN_DELETE": uint64(OpDelete), "FEV_FSN_RENAME": uint64(OpRename),
		"FEV_FSN_INVAL_ALL": uint64(OpInvalAll),
		"FEV_FSN_F_ISDIR":   uint64(FlagIsDir), "FEV_FSN_F_LAST_LINK": uint64(FlagLastLink),
		"FEV_FSN_F_QUIET": uint64(FlagQuiet), "FEV_FSN_F_VICTIM_GONE": uint64(FlagVictimGone),
		"FEV_FSN_M_MODIFY": uint64(MaskModify), "FEV_FSN_M_ATTRIB": uint64(MaskAttrib),
		"FEV_FSN_M_CLOSE_WRITE": uint64(MaskCloseWrite), "FEV_FSN_M_OPEN": uint64(MaskOpen),
		"FEV_FSN_HAD_PARENT": HadParent, "FEV_FSN_HAD_CHILD": HadChild, "FEV_FSN_HAD_ENTRY": HadEntry,
		"FEV_FSN_HAD_PARENT2": HadParent2, "FEV_FSN_HAD_VICTIM": HadVictim, "FEV_FSN_MOVED": Moved,
	}
	for name, v := range want {
		if got, ok := defines[name]; !ok || got != v {
			t.Errorf("%s is %d in the header (present: %v) and %d here", name, got, ok, v)
		}
	}
	for name := range defines {
		if _, ok := want[name]; !ok {
			t.Errorf("the header defines %s, which this package does not have", name)
		}
	}
	if !strings.Contains(string(src), "_IOW('b', 1, struct fev_fsn_event)") {
		t.Error("FEV_FSN_EMIT is no longer _IOW('b', 1, struct fev_fsn_event)")
	}
}

func TestTheDeniedLineFitsItsType(t *testing.T) {
	line := read(t, "fixtures/trace/fs.denied.json")
	var d Denied
	if err := contract.StrictDecode(line, &d); err != nil {
		t.Fatal(err)
	}
	if d.Mark != MarkDenied || d.Path == "" || d.NS == 0 {
		t.Errorf("decoded as %+v", d)
	}
	again, _ := json.Marshal(d)
	if string(again)+"\n" != string(line) {
		t.Errorf("the line %sencodes back as %s", line, again)
	}
}

func FuzzDecode(f *testing.F) {
	for _, s := range valid {
		var b []byte
		for _, fr := range s.frames {
			b = append(b, ok(fr)...)
		}
		f.Add(b)
	}
	for _, s := range invalid {
		f.Add(s.bytes)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, from := range []Side{FromGuest, FromHost} {
			fr, n, err := Decode(from, data)
			if err != nil {
				if fr != nil || n != 0 {
					t.Fatalf("an error came with a frame or a length: %v %d", fr, n)
				}
				continue
			}
			if n < HeaderLen || n > len(data) {
				t.Fatalf("decoded %d bytes of %d", n, len(data))
			}
			again, err := Append(nil, fr)
			if err != nil || !bytes.Equal(again, data[:n]) {
				t.Fatalf("%x decoded and encoded back as %x (%v)", data[:n], again, err)
			}
			_ = Describe(fr)
			_ = NewOrder(from).Check(fr)
			if ev, ok := fr.(Events); ok {
				for _, e := range ev.Records {
					_ = e.Ioctl(3)
				}
			}
		}
	})
}
