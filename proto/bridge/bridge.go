package bridge

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
)

const (
	Socket      = "/run/fev/mac.sock"
	AgentSocket = "/run/fev/ssh-auth.sock"
)

const (
	HelperDir   = "/opt/fev"
	HelperPath  = HelperDir + "/fev-guest"
	BinDir      = HelperDir + "/bin"
	ExecHandler = HelperDir + "/fev-macexec"
)

var (
	Defaults = []string{
		"open", "xdg-open", "code", "osascript", "pbcopy", "pbpaste",
		"mdfind", "mdls", "qlmanage", "screencapture",
		"caffeinate", "pmset", "softwareupdate", "system_profiler",
	}
	Own = []string{"mac", "macctl"}
)

func Builtin(name string) bool {
	for _, set := range [][]string{Defaults, Own} {
		for _, n := range set {
			if n == name {
				return true
			}
		}
	}
	return false
}

const (
	VerbRun    = "run"
	VerbExec   = "exec"
	VerbInfo   = "info"
	VerbNotify = "notify"
	VerbLink   = "link"
	VerbUnlink = "unlink"
)

const MaxHeader = 256 << 10

type Header struct {
	Verb    string   `json:"verb"`
	Argv    []string `json:"argv,omitempty"`
	Path    string   `json:"path,omitempty"`
	Dir     string   `json:"dir,omitempty"`
	Env     []string `json:"env,omitempty"`
	Stdin   bool     `json:"stdin_tty,omitempty"`
	Stdout  bool     `json:"stdout_tty,omitempty"`
	Stderr  bool     `json:"stderr_tty,omitempty"`
	Rows    int      `json:"rows,omitempty"`
	Cols    int      `json:"cols,omitempty"`
	User    string   `json:"user,omitempty"`
	Title   string   `json:"title,omitempty"`
	Message string   `json:"message,omitempty"`
	Names   []string `json:"names,omitempty"`
}

func (h *Header) TTY() bool { return h.Stdin || h.Stdout || h.Stderr }

func (h *Header) Check() error {
	switch h.Verb {
	case VerbRun, VerbExec:
		if len(h.Argv) == 0 || h.Argv[0] == "" {
			return errors.New("no command")
		}
		if h.Verb == VerbExec && (!path.IsAbs(h.Path) || path.Clean(h.Path) != h.Path || strings.ContainsRune(h.Path, 0)) {
			return fmt.Errorf("a program's path %q that is not absolute and clean", clip(h.Path, 80))
		}
		if h.Rows < 0 || h.Cols < 0 || h.Rows > 0xffff || h.Cols > 0xffff {
			return fmt.Errorf("a terminal of %d by %d", h.Rows, h.Cols)
		}
	case VerbInfo:
	case VerbNotify:
		if h.Message == "" && h.Title == "" {
			return errors.New("nothing to say")
		}
	case VerbLink, VerbUnlink:
		if len(h.Names) == 0 {
			return errors.New("no name")
		}
		for _, n := range h.Names {
			if err := CheckLinkName(n); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("no verb %q", clip(h.Verb, 40))
	}
	for _, kv := range h.Env {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
			return fmt.Errorf("an environment entry %q", clip(kv, 40))
		}
	}
	return nil
}

func CheckLinkName(name string) error {
	switch {
	case name == "", name == ".", name == "..":
		return fmt.Errorf("%q is not a command's name", name)
	case len(name) > 255:
		return fmt.Errorf("a name of %d bytes", len(name))
	case strings.ContainsAny(name, "/\x00"):
		return fmt.Errorf("%q is not a command's name: it has a / or a NUL", clip(name, 40))
	}
	for _, r := range name {
		if r < ' ' || r == 0x7f {
			return fmt.Errorf("%q is not a command's name: it has a control character", clip(name, 40))
		}
	}
	return nil
}

func WriteHeader(w io.Writer, h *Header) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if len(b)+1 > MaxHeader {
		return fmt.Errorf("the command is %d bytes long; at most %d", len(b), MaxHeader-1)
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func ReadHeader(r *bufio.Reader) (*Header, []byte, error) {
	line, err := readLine(r, MaxHeader)
	if err != nil {
		return nil, nil, err
	}
	var h Header
	if err := json.Unmarshal(line, &h); err != nil {
		return nil, nil, fmt.Errorf("the header: %w", err)
	}
	return &h, line, nil
}

func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > max {
			return nil, fmt.Errorf("a line longer than %d bytes", max)
		}
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

func WriteOK(w io.Writer) error {
	_, err := io.WriteString(w, "ok\n")
	return err
}

func WriteError(w io.Writer, text string) error {
	_, err := io.WriteString(w, "err "+oneLine(text)+"\n")
	return err
}

type Refused struct{ Text string }

func (e *Refused) Error() string { return e.Text }

func ReadReply(r *bufio.Reader) error {
	line, err := readLine(r, 4096)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("the connection was closed with no answer")
		}
		return err
	}
	s := strings.TrimSuffix(string(line), "\n")
	switch {
	case s == "ok":
		return nil
	case strings.HasPrefix(s, "err "):
		return &Refused{Text: s[4:]}
	}
	return fmt.Errorf("an answer %q", clip(s, 60))
}

func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return clip(s, 1000)
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

const (
	FrameStdin   byte = 0
	FrameStdout  byte = 1
	FrameStderr  byte = 2
	FrameControl byte = 3
	FrameExit    byte = 4
)

const MaxFrame = 64 << 10

const frameHeader = 5

func WriteFrame(w io.Writer, stream byte, p []byte) error {
	if stream > FrameExit {
		return fmt.Errorf("bridge: unknown stream %d", stream)
	}
	for first := true; first || len(p) > 0; first = false {
		n := min(len(p), MaxFrame)
		buf := make([]byte, frameHeader+n)
		buf[0] = stream
		binary.BigEndian.PutUint32(buf[1:], uint32(n))
		copy(buf[frameHeader:], p[:n])
		if _, err := w.Write(buf); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

func ReadFrame(r io.Reader, buf []byte) (stream byte, n int, err error) {
	var head [frameHeader]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, 0, err
	}
	stream = head[0]
	size := binary.BigEndian.Uint32(head[1:])
	if stream > FrameExit {
		return 0, 0, fmt.Errorf("bridge: unknown stream %d", stream)
	}
	if size > MaxFrame || int(size) > len(buf) {
		return 0, 0, fmt.Errorf("bridge: a frame of %d bytes exceeds the limit", size)
	}
	if _, err := io.ReadFull(r, buf[:size]); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, 0, err
	}
	return stream, int(size), nil
}

type Control struct {
	Rows   int    `json:"rows,omitempty"`
	Cols   int    `json:"cols,omitempty"`
	Signal string `json:"signal,omitempty"`
}

var Signals = []string{"HUP", "INT", "QUIT", "TERM", "USR1", "USR2"}

type Exit struct {
	Code   int    `json:"code"`
	Signal int    `json:"signal,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (e Exit) Status() int {
	if e.Signal > 0 {
		return 128 + e.Signal
	}
	return e.Code
}

type Info struct {
	Home  string `json:"home"`
	User  string `json:"user"`
	Share bool   `json:"share"`
	Root  string `json:"root,omitempty"`
	View  string `json:"view,omitempty"`
}

const ShareTarget = "/mnt/mac"

var SamePaths = []string{"/Users", "/Volumes", "/private", "/Applications", "/Library"}

func under(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

func MacDir(dir, root string, share bool) (string, bool) {
	if !share || !path.IsAbs(dir) {
		return "", false
	}
	dir = path.Clean(dir)
	if under(dir, ShareTarget) {
		return path.Join(root, "/"+strings.TrimPrefix(dir, ShareTarget)), true
	}
	for _, p := range SamePaths {
		if under(dir, p) {
			return path.Join(root, dir), true
		}
	}
	return "", false
}

func MacDirIn(dir, root string, share bool, view string) (string, bool) {
	if d, ok := MacDir(dir, root, share); ok {
		return d, true
	}
	if view == "" || !path.IsAbs(dir) || !path.IsAbs(view) {
		return "", false
	}
	return path.Join(view, path.Clean(dir)), true
}

func MacProgram(p, root string, share bool, view string) (string, bool) {
	if !path.IsAbs(p) {
		return "", false
	}
	return MacDirIn(p, root, share, view)
}

func LinuxPathIn(mac, home, root, view string) (string, bool) {
	if !path.IsAbs(mac) {
		mac = path.Join(home, mac)
	}
	mac = path.Clean(mac)
	if view != "" && path.IsAbs(view) {
		v := path.Clean(view)
		if under(mac, v) {
			return path.Clean("/" + strings.TrimPrefix(mac, v)), true
		}
	}
	return LinuxPath(mac, home, root)
}

func MacArg(arg, root string, share bool) string {
	if !share {
		return arg
	}
	prefix, value := "", arg
	if strings.HasPrefix(arg, "-") {
		k, v, ok := strings.Cut(arg, "=")
		if !ok {
			return arg
		}
		prefix, value = k+"=", v
	}
	if !under(value, ShareTarget) || strings.Contains(value, "/../") || strings.HasSuffix(value, "/..") {
		return arg
	}
	return prefix + path.Join(root, "/"+strings.TrimPrefix(value, ShareTarget))
}

func LinuxPath(mac, home, root string) (string, bool) {
	if !path.IsAbs(mac) {
		mac = path.Join(home, mac)
	}
	mac = path.Clean(mac)
	if root != "" && root != "/" {
		root = path.Clean(root)
		if !under(mac, root) {
			return "", false
		}
		mac = "/" + strings.TrimPrefix(strings.TrimPrefix(mac, root), "/")
	}
	for _, p := range SamePaths {
		if under(mac, p) {
			return mac, true
		}
	}
	if mac == "/" {
		return ShareTarget, true
	}
	return ShareTarget + mac, true
}

const AgentPathParam = "fev.ssh_auth_sock"

func AgentPathWord(p string) string {
	if !path.IsAbs(p) {
		return ""
	}
	return AgentPathParam + "=" + url.PathEscape(path.Clean(p))
}

func AgentPath(value string) (string, bool) {
	p, err := url.PathUnescape(value)
	if err != nil || !path.IsAbs(p) || path.Clean(p) != p || p == "/" || strings.ContainsRune(p, 0) {
		return "", false
	}
	return p, true
}
