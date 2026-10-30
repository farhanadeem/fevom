package fsevents

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"fev/proto/version"
)

const HostPort = 1027

const Version = 1

const Magic = "BFSE"

const (
	TypeHello    = 1
	TypeWatch    = 2
	TypeWatching = 3
	TypeEvents   = 4
)

const (
	HeaderLen   = 8
	MaxBuild    = 128
	MaxHello    = 8 + MaxBuild
	MaxPaths    = 1024
	MaxPath     = 1023
	MaxWatch    = 256 << 10
	MaxWatching = 8 + MaxPaths
	MaxEvents   = 64 << 10
	RecordLen   = 40
	MaxName     = 255
	QueueMax    = 256 << 10
)

type Op uint8

const (
	OpChange   Op = 1
	OpCreate   Op = 2
	OpDelete   Op = 3
	OpRename   Op = 4
	OpInvalAll Op = 5
)

type Flags uint8

const (
	FlagIsDir      Flags = 1 << 0
	FlagLastLink   Flags = 1 << 1
	FlagQuiet      Flags = 1 << 2
	FlagVictimGone Flags = 1 << 3

	allFlags = FlagIsDir | FlagLastLink | FlagQuiet | FlagVictimGone
)

type Mask uint8

const (
	MaskModify     Mask = 1 << 0
	MaskAttrib     Mask = 1 << 1
	MaskCloseWrite Mask = 1 << 2
	MaskOpen       Mask = 1 << 3

	allMask = MaskModify | MaskAttrib | MaskCloseWrite | MaskOpen
)

const Content = MaskOpen | MaskModify | MaskCloseWrite

type Status uint8

const (
	StatusOK      Status = 0
	StatusOutside Status = 1
	StatusMissing Status = 2
	StatusFailed  Status = 3
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusOutside:
		return "outside"
	case StatusMissing:
		return "missing"
	case StatusFailed:
		return "failed"
	}
	return "status(" + strconv.Itoa(int(s)) + ")"
}

type Frame interface {
	Type() uint8
	appendPayload(dst []byte) ([]byte, error)
	describe(b *strings.Builder)
}

type Hello struct {
	Version uint16
	Build   string
}

type Watch struct {
	Seq   uint32
	Paths []string
}

type Watching struct {
	Seq    uint32
	Status []Status
}

type Events struct {
	Records []Event
}

type Event struct {
	Op          Op
	Flags       Flags
	Mask        Mask
	Parent      uint64
	Child       uint64
	Parent2     uint64
	Victim      uint64
	Name, Name2 string
}

func (Hello) Type() uint8    { return TypeHello }
func (Watch) Type() uint8    { return TypeWatch }
func (Watching) Type() uint8 { return TypeWatching }
func (Events) Type() uint8   { return TypeEvents }

type Side int

const (
	FromGuest Side = iota
	FromHost
)

type Order struct {
	from  Side
	hello bool
	seq   uint32
}

func NewOrder(from Side) *Order { return &Order{from: from} }

func (o *Order) Check(f Frame) error {
	if _, ok := f.(Hello); ok != !o.hello {
		if ok {
			return bad("a second hello")
		}
		return bad("a type %d frame before the hello", f.Type())
	}
	var seq uint32
	switch f := f.(type) {
	case Hello:
		o.hello = true
		return nil
	case Watch:
		seq = f.Seq
	case Watching:
		seq = f.Seq
	default:
		return nil
	}
	if seq == 0 || seq != o.seq+1 {
		return bad("sequence number %d follows %d", seq, o.seq)
	}
	o.seq = seq
	return nil
}

func (w Watching) Answers(sent Watch) error {
	if w.Seq != sent.Seq {
		return bad("an acknowledgement of set %d where %d is the oldest unanswered", w.Seq, sent.Seq)
	}
	if len(w.Status) != len(sent.Paths) {
		return bad("an acknowledgement of %d paths for a set of %d", len(w.Status), len(sent.Paths))
	}
	return nil
}

var ErrShort = errors.New("fsevents: incomplete frame")

func bad(format string, args ...any) error {
	return fmt.Errorf("fsevents: "+format, args...)
}

func limit(from Side, t uint8) (int, bool) {
	switch {
	case t == TypeHello:
		return MaxHello, true
	case t == TypeWatch && from == FromGuest:
		return MaxWatch, true
	case t == TypeWatching && from == FromHost:
		return MaxWatching, true
	case t == TypeEvents && from == FromHost:
		return MaxEvents, true
	}
	return 0, false
}

func (h Hello) Check(build, peer string) error {
	if h.Version != Version {
		return bad("the %s speaks version %d of the change feed, this side %d", peer, h.Version, Version)
	}
	if h.Build == "" || build == "" {
		return nil
	}
	return version.Check(build, h.Build, peer)
}

func ValidBuild(s string) error {
	if len(s) > MaxBuild {
		return bad("build identifier of %d bytes exceeds %d", len(s), MaxBuild)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return bad("build identifier contains byte %#x", s[i])
		}
	}
	return nil
}

func ValidPath(p string) error {
	switch {
	case p == "":
		return bad("empty path")
	case len(p) > MaxPath:
		return bad("path of %d bytes exceeds %d", len(p), MaxPath)
	case p[0] != '/':
		return bad("path %q is not absolute", p)
	case !utf8.ValidString(p):
		return bad("path %q is not UTF-8", p)
	case strings.IndexByte(p, 0) >= 0:
		return bad("path %q contains a zero byte", p)
	case p == "/":
		return nil
	}
	for _, part := range strings.Split(p[1:], "/") {
		if part == "" || part == "." || part == ".." {
			return bad("path %q is not clean", p)
		}
	}
	return nil
}

func validName(n string) error {
	switch {
	case len(n) > MaxName:
		return bad("name of %d bytes exceeds %d", len(n), MaxName)
	case strings.IndexByte(n, '/') >= 0 || strings.IndexByte(n, 0) >= 0:
		return bad("name %q contains a slash or a zero byte", n)
	case n == "." || n == "..":
		return bad("name %q is not a name", n)
	}
	return nil
}

func (e Event) Validate() error {
	if e.Flags&^allFlags != 0 {
		return bad("unknown flags %#x", uint8(e.Flags))
	}
	if e.Mask&^allMask != 0 {
		return bad("unknown mask %#x", uint8(e.Mask))
	}
	if err := validName(e.Name); err != nil {
		return err
	}
	if err := validName(e.Name2); err != nil {
		return err
	}
	allowed := FlagIsDir | FlagQuiet
	switch e.Op {
	case OpChange:
		if e.Child == 0 && (e.Parent == 0 || e.Name == "") {
			return bad("a change names neither an object nor a directory and a name")
		}
	case OpCreate:
		if e.Parent == 0 || e.Name == "" {
			return bad("a create needs a directory and a name")
		}
		if e.Child != 0 {
			return bad("a create names no object")
		}
	case OpDelete:
		allowed |= FlagLastLink
		if e.Name == "" || e.Parent == 0 && e.Child == 0 {
			return bad("a delete needs a name, and a directory or an object")
		}
	case OpRename:
		allowed |= FlagVictimGone
		if e.Name == "" || e.Name2 == "" || e.Parent == 0 && e.Parent2 == 0 && e.Child == 0 {
			return bad("a rename needs both names, and a directory or an object")
		}
	case OpInvalAll:
		if e != (Event{Op: OpInvalAll}) {
			return bad("an invalidation carries nothing else")
		}
	default:
		return bad("unknown operation %d", e.Op)
	}
	if e.Flags&^allowed != 0 {
		return bad("flags %#x do not belong to operation %d", uint8(e.Flags), e.Op)
	}
	if e.Op != OpChange && e.Mask != 0 {
		return bad("only a change has a mask")
	}
	if e.Op != OpRename && (e.Parent2 != 0 || e.Victim != 0 || e.Name2 != "") {
		return bad("only a rename has a second directory, a victim or a second name")
	}
	return nil
}

var le = binary.LittleEndian

func (h Hello) appendPayload(dst []byte) ([]byte, error) {
	if err := ValidBuild(h.Build); err != nil {
		return nil, err
	}
	dst = append(dst, Magic...)
	dst = le.AppendUint16(dst, h.Version)
	dst = le.AppendUint16(dst, uint16(len(h.Build)))
	return append(dst, h.Build...), nil
}

func (w Watch) appendPayload(dst []byte) ([]byte, error) {
	if w.Seq == 0 {
		return nil, bad("a watch set has a sequence number from 1")
	}
	if len(w.Paths) > MaxPaths {
		return nil, bad("a watch set of %d paths exceeds %d", len(w.Paths), MaxPaths)
	}
	dst = le.AppendUint32(dst, w.Seq)
	dst = le.AppendUint16(dst, uint16(len(w.Paths)))
	dst = le.AppendUint16(dst, 0)
	for _, p := range w.Paths {
		if err := ValidPath(p); err != nil {
			return nil, err
		}
		dst = le.AppendUint16(dst, uint16(len(p)))
		dst = append(dst, p...)
	}
	return dst, nil
}

func (w Watching) appendPayload(dst []byte) ([]byte, error) {
	if w.Seq == 0 {
		return nil, bad("an acknowledgement has a sequence number from 1")
	}
	if len(w.Status) > MaxPaths {
		return nil, bad("an acknowledgement of %d paths exceeds %d", len(w.Status), MaxPaths)
	}
	dst = le.AppendUint32(dst, w.Seq)
	dst = le.AppendUint16(dst, uint16(len(w.Status)))
	dst = le.AppendUint16(dst, 0)
	for _, s := range w.Status {
		if s > StatusFailed {
			return nil, bad("unknown status %d", s)
		}
		dst = append(dst, byte(s))
	}
	return dst, nil
}

func (ev Events) appendPayload(dst []byte) ([]byte, error) {
	if len(ev.Records) == 0 {
		return nil, bad("an events frame carries at least one record")
	}
	if len(ev.Records) > MaxEvents/RecordLen {
		return nil, bad("%d records do not fit one frame", len(ev.Records))
	}
	dst = le.AppendUint16(dst, uint16(len(ev.Records)))
	dst = le.AppendUint16(dst, 0)
	for _, e := range ev.Records {
		if err := e.Validate(); err != nil {
			return nil, err
		}
		dst = e.appendRecord(dst)
	}
	return dst, nil
}

func (e Event) appendRecord(dst []byte) []byte {
	dst = append(dst, byte(e.Op), byte(e.Flags), byte(e.Mask), 0)
	dst = le.AppendUint16(dst, uint16(len(e.Name)))
	dst = le.AppendUint16(dst, uint16(len(e.Name2)))
	dst = le.AppendUint64(dst, e.Parent)
	dst = le.AppendUint64(dst, e.Child)
	dst = le.AppendUint64(dst, e.Parent2)
	dst = le.AppendUint64(dst, e.Victim)
	dst = append(dst, e.Name...)
	return append(dst, e.Name2...)
}

func (e Event) Len() int { return RecordLen + len(e.Name) + len(e.Name2) }

func Append(dst []byte, f Frame) ([]byte, error) {
	start := len(dst)
	out := append(dst, 0, 0, 0, 0, f.Type(), 0, 0, 0)
	out, err := f.appendPayload(out)
	if err != nil {
		return dst, err
	}
	n := len(out) - start - HeaderLen
	lim := MaxHello
	switch f.Type() {
	case TypeWatch:
		lim = MaxWatch
	case TypeWatching:
		lim = MaxWatching
	case TypeEvents:
		lim = MaxEvents
	}
	if n > lim {
		return dst, bad("a payload of %d bytes exceeds the %d of its type", n, lim)
	}
	le.PutUint32(out[start:], uint32(n))
	return out, nil
}

func AppendEvents(dst []byte, records []Event) ([]byte, error) {
	out := dst
	for len(records) > 0 {
		n, size := 0, 4
		for n < len(records) && size+records[n].Len() <= MaxEvents {
			size += records[n].Len()
			n++
		}
		var err error
		if out, err = Append(out, Events{Records: records[:n]}); err != nil {
			return dst, err
		}
		records = records[n:]
	}
	return out, nil
}

func Write(w io.Writer, f Frame) error {
	buf, err := Append(nil, f)
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}

func header(from Side, b []byte) (int, uint8, error) {
	n, t := le.Uint32(b), b[4]
	if b[5] != 0 || b[6] != 0 || b[7] != 0 {
		return 0, 0, bad("non-zero padding in a frame header")
	}
	lim, ok := limit(from, t)
	if !ok {
		return 0, 0, bad("frame type %d is not one this peer may send", t)
	}
	if n > uint32(lim) {
		return 0, 0, bad("a type %d payload of %d bytes exceeds %d", t, n, lim)
	}
	return int(n), t, nil
}

func Decode(from Side, b []byte) (Frame, int, error) {
	if len(b) < HeaderLen {
		return nil, 0, ErrShort
	}
	n, t, err := header(from, b)
	if err != nil {
		return nil, 0, err
	}
	if len(b)-HeaderLen < n {
		return nil, 0, ErrShort
	}
	f, err := payload(t, b[HeaderLen:HeaderLen+n])
	if err != nil {
		return nil, 0, err
	}
	return f, HeaderLen + n, nil
}

func Read(r io.Reader, from Side) (Frame, error) {
	var head [HeaderLen]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, err
	}
	n, t, err := header(from, head[:])
	if err != nil {
		return nil, err
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return payload(t, p)
}

func payload(t uint8, p []byte) (Frame, error) {
	switch t {
	case TypeHello:
		return decodeHello(p)
	case TypeWatch:
		return decodeWatch(p)
	case TypeWatching:
		return decodeWatching(p)
	case TypeEvents:
		return decodeEvents(p)
	}
	return nil, bad("unknown frame type %d", t)
}

func decodeHello(p []byte) (Frame, error) {
	if len(p) < 8 {
		return nil, bad("a hello of %d bytes", len(p))
	}
	if string(p[:4]) != Magic {
		return nil, bad("not a change-feed hello")
	}
	h := Hello{Version: le.Uint16(p[4:])}
	if int(le.Uint16(p[6:])) != len(p)-8 {
		return nil, bad("a hello whose length disagrees with its build identifier")
	}
	h.Build = string(p[8:])
	if err := ValidBuild(h.Build); err != nil {
		return nil, err
	}
	return h, nil
}

func counted(p []byte) (seq uint32, count int, rest []byte, err error) {
	if len(p) < 8 {
		return 0, 0, nil, bad("a watch frame of %d bytes", len(p))
	}
	seq, count = le.Uint32(p), int(le.Uint16(p[4:]))
	switch {
	case seq == 0:
		err = bad("sequence number 0")
	case le.Uint16(p[6:]) != 0:
		err = bad("non-zero padding in a watch frame")
	case count > MaxPaths:
		err = bad("%d paths exceed %d", count, MaxPaths)
	}
	return seq, count, p[8:], err
}

func decodeWatch(p []byte) (Frame, error) {
	seq, count, p, err := counted(p)
	if err != nil {
		return nil, err
	}
	if count*2 > len(p) {
		return nil, bad("a watch set of %d paths in %d bytes", count, len(p))
	}
	w := Watch{Seq: seq, Paths: make([]string, 0, count)}
	for range count {
		if len(p) < 2 {
			return nil, bad("a watch set cut short")
		}
		n := int(le.Uint16(p))
		if n > len(p)-2 {
			return nil, bad("a watch path cut short")
		}
		path := string(p[2 : 2+n])
		if err := ValidPath(path); err != nil {
			return nil, err
		}
		w.Paths = append(w.Paths, path)
		p = p[2+n:]
	}
	if len(p) != 0 {
		return nil, bad("%d bytes after a watch set", len(p))
	}
	return w, nil
}

func decodeWatching(p []byte) (Frame, error) {
	seq, count, p, err := counted(p)
	if err != nil {
		return nil, err
	}
	if count != len(p) {
		return nil, bad("an acknowledgement of %d paths in %d bytes", count, len(p))
	}
	w := Watching{Seq: seq, Status: make([]Status, count)}
	for i, s := range p {
		if Status(s) > StatusFailed {
			return nil, bad("unknown status %d", s)
		}
		w.Status[i] = Status(s)
	}
	return w, nil
}

func decodeEvents(p []byte) (Frame, error) {
	if len(p) < 4 {
		return nil, bad("an events frame of %d bytes", len(p))
	}
	count := int(le.Uint16(p))
	if le.Uint16(p[2:]) != 0 {
		return nil, bad("non-zero padding in an events frame")
	}
	p = p[4:]
	if count == 0 || count*RecordLen > len(p) {
		return nil, bad("%d records in %d bytes", count, len(p))
	}
	ev := Events{Records: make([]Event, 0, count)}
	for range count {
		if len(p) < RecordLen {
			return nil, bad("a record cut short")
		}
		if p[3] != 0 {
			return nil, bad("non-zero padding in a record")
		}
		n, n2 := int(le.Uint16(p[4:])), int(le.Uint16(p[6:]))
		if n > MaxName || n2 > MaxName {
			return nil, bad("a name of %d bytes exceeds %d", max(n, n2), MaxName)
		}
		if n+n2 > len(p)-RecordLen {
			return nil, bad("a record's names cut short")
		}
		e := Event{
			Op: Op(p[0]), Flags: Flags(p[1]), Mask: Mask(p[2]),
			Parent: le.Uint64(p[8:]), Child: le.Uint64(p[16:]),
			Parent2: le.Uint64(p[24:]), Victim: le.Uint64(p[32:]),
			Name:  string(p[RecordLen : RecordLen+n]),
			Name2: string(p[RecordLen+n : RecordLen+n+n2]),
		}
		if err := e.Validate(); err != nil {
			return nil, err
		}
		ev.Records = append(ev.Records, e)
		p = p[RecordLen+n+n2:]
	}
	if len(p) != 0 {
		return nil, bad("%d bytes after the last record", len(p))
	}
	return ev, nil
}

const (
	IoctlLen  = 568
	IoctlEmit = 1<<30 | IoctlLen<<16 | 'b'<<8 | 1
	Device    = "/dev/fev-fsnotify"
)

const (
	HadParent  = 1 << 0
	HadChild   = 1 << 1
	HadEntry   = 1 << 2
	HadParent2 = 1 << 3
	HadVictim  = 1 << 4
	Moved      = 1 << 5
)

func (e Event) Ioctl(fd int32) [IoctlLen]byte {
	var b [IoctlLen]byte
	le.PutUint32(b[0:], uint32(e.Op))
	le.PutUint32(b[4:], uint32(e.Flags))
	le.PutUint32(b[8:], uint32(fd))
	le.PutUint32(b[12:], uint32(e.Mask))
	le.PutUint64(b[16:], e.Parent)
	le.PutUint64(b[24:], e.Child)
	le.PutUint64(b[32:], e.Parent2)
	le.PutUint64(b[40:], e.Victim)
	le.PutUint16(b[48:], uint16(len(e.Name)))
	le.PutUint16(b[50:], uint16(len(e.Name2)))
	copy(b[56:56+MaxName], e.Name)
	copy(b[312:312+MaxName], e.Name2)
	return b
}

func Describe(f Frame) string {
	var b strings.Builder
	f.describe(&b)
	return b.String()
}

func (h Hello) describe(b *strings.Builder) {
	fmt.Fprintf(b, "hello version=%d build=%s\n", h.Version, quote(h.Build))
}

func (w Watch) describe(b *strings.Builder) {
	fmt.Fprintf(b, "watch seq=%d paths=%d\n", w.Seq, len(w.Paths))
	for _, p := range w.Paths {
		fmt.Fprintf(b, "  %s\n", quote(p))
	}
}

func (w Watching) describe(b *strings.Builder) {
	fmt.Fprintf(b, "watching seq=%d paths=%d\n", w.Seq, len(w.Status))
	for _, s := range w.Status {
		fmt.Fprintf(b, "  %s\n", s)
	}
}

func (ev Events) describe(b *strings.Builder) {
	fmt.Fprintf(b, "events records=%d\n", len(ev.Records))
	for _, e := range ev.Records {
		fmt.Fprintf(b, "  %s flags=%s mask=%s parent=%d child=%d parent2=%d victim=%d name=%s name2=%s\n",
			e.Op, e.Flags, e.Mask, e.Parent, e.Child, e.Parent2, e.Victim, quote(e.Name), quote(e.Name2))
	}
}

func (o Op) String() string {
	switch o {
	case OpChange:
		return "change"
	case OpCreate:
		return "create"
	case OpDelete:
		return "delete"
	case OpRename:
		return "rename"
	case OpInvalAll:
		return "inval_all"
	}
	return "op(" + strconv.Itoa(int(o)) + ")"
}

func bits(v uint8, names [4]string) string {
	var out []string
	for i, name := range names {
		if v&(1<<i) != 0 {
			out = append(out, name)
		}
	}
	if out == nil {
		return "0"
	}
	return strings.Join(out, "|")
}

func (f Flags) String() string {
	return bits(uint8(f), [4]string{"isdir", "last_link", "quiet", "victim_gone"})
}

func (m Mask) String() string {
	return bits(uint8(m), [4]string{"modify", "attrib", "close_write", "open"})
}

func quote(s string) string {
	const hex = "0123456789abcdef"
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			b = append(b, '\\', c)
		case c < 0x20 || c > 0x7e:
			b = append(b, '\\', 'x', hex[c>>4], hex[c&15])
		default:
			b = append(b, c)
		}
	}
	return string(append(b, '"'))
}

const MarkDenied = "fs.denied"

const MarkGranted = "fs.granted"

type Denied struct {
	Mark string `json:"mark"`
	NS   uint64 `json:"ns"`
	Path string `json:"path"`
}
