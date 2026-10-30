package relay

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
)

const Port = 1026

const MaxLine = 512

type Kind string

const (
	TCP    Kind = "tcp"
	UDP    Kind = "udp"
	Exec   Kind = "exec"
	Stream Kind = "stream"
	MTCP   Kind = "mtcp"
	View   Kind = "view"
)

const ViewNFS = "nfs"

type Request struct {
	Kind    Kind
	Target  string
	Machine string
}

const (
	MaxMachineName  = 64
	MaxMachineLabel = 63
)

func CheckMachineName(name string) error {
	if name == "" || len(name) > MaxMachineName {
		return fmt.Errorf("a machine's name is 1 to %d characters", MaxMachineName)
	}
	for _, label := range strings.Split(name, ".") {
		switch {
		case label == "":
			return fmt.Errorf("a machine's name has dots only between its parts: %q", name)
		case len(label) > MaxMachineLabel:
			return fmt.Errorf("a machine's name has at most %d characters between dots: %q", MaxMachineLabel, name)
		case label[0] == '-' || label[len(label)-1] == '-':
			return fmt.Errorf("no part of a machine's name begins or ends with a hyphen: %q", name)
		}
		for _, c := range []byte(label) {
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			default:
				return fmt.Errorf("a machine's name is letters, digits and hyphens, with dots between parts: %q", name)
			}
		}
	}
	switch MachineNameKey(name) {
	case ReservedMachineName:
		return fmt.Errorf("%q is not a machine's name: it means the default machine", name)
	case EngineViewName:
		return fmt.Errorf("%q is not a machine's name: /mnt/machines/%s holds the Docker engine's volumes", name, EngineViewName)
	}
	return nil
}

func MachineNameKey(name string) string {
	b := []byte(name)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func SameMachineName(a, b string) bool { return MachineNameKey(a) == MachineNameKey(b) }

const ReservedMachineName = "default"

const EngineViewName = "docker"

const TokenLen = 32

func (r Request) Validate() error {
	if (r.Kind == MTCP) != (r.Machine != "") {
		return fmt.Errorf("relay: a machine is named in an mtcp request and in no other")
	}
	switch r.Kind {
	case TCP, UDP, MTCP:
		if r.Kind == MTCP {
			if err := CheckMachineName(r.Machine); err != nil {
				return fmt.Errorf("relay: %v", err)
			}
		}
		ap, err := netip.ParseAddrPort(r.Target)
		if err != nil {
			return fmt.Errorf("relay: %q is not an address and port", r.Target)
		}
		if ap.Port() == 0 {
			return fmt.Errorf("relay: %q has no port", r.Target)
		}
		if ap.Addr().Zone() != "" {
			return fmt.Errorf("relay: %q has a zone, which is not accepted", r.Target)
		}
	case Exec, Stream:
		if len(r.Target) != TokenLen {
			return fmt.Errorf("relay: a token is %d hex digits", TokenLen)
		}
		for _, c := range []byte(r.Target) {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return fmt.Errorf("relay: a token is %d hex digits", TokenLen)
			}
		}
	case View:
		if r.Target != ViewNFS {
			return fmt.Errorf("relay: the view's target is %q", ViewNFS)
		}
	default:
		return fmt.Errorf("relay: unknown kind %q", string(r.Kind))
	}
	return nil
}

func (r Request) String() string {
	if r.Machine != "" {
		return string(r.Kind) + " " + r.Machine + " " + r.Target
	}
	return string(r.Kind) + " " + r.Target
}

func WriteRequest(w io.Writer, r Request) error {
	if err := r.Validate(); err != nil {
		return err
	}
	_, err := io.WriteString(w, r.String()+"\n")
	return err
}

func ReadRequest(r io.Reader) (Request, error) {
	line, err := readLine(r)
	if err != nil {
		return Request{}, err
	}
	kind, target, ok := strings.Cut(line, " ")
	if !ok {
		return Request{}, fmt.Errorf("relay: malformed request line")
	}
	req := Request{Kind: Kind(kind), Target: target}
	if req.Kind == MTCP {
		if req.Machine, req.Target, ok = strings.Cut(target, " "); !ok {
			return Request{}, fmt.Errorf("relay: malformed request line")
		}
	}
	return req, req.Validate()
}

const (
	CodeRefused     = "refused"
	CodeUnreachable = "unreachable"
	CodeTimeout     = "timeout"
	CodeDenied      = "denied"
	CodeBadRequest  = "bad_request"
	CodeFailed      = "failed"
)

type Error struct {
	Code string
	Text string
}

func (e *Error) Error() string {
	if e.Text == "" {
		return "relay: " + e.Code
	}
	return "relay: " + e.Code + ": " + e.Text
}

func WriteOK(w io.Writer) error {
	_, err := io.WriteString(w, "ok\n")
	return err
}

func WriteError(w io.Writer, code, text string) error {
	if code == "" || code != printable(code) || strings.Contains(code, " ") {
		code = CodeFailed
	}
	line := "err " + code
	if text = printable(strings.Join(strings.Fields(text), " ")); text != "" {
		line += " " + text
	}
	if len(line) > MaxLine-1 {
		line = line[:MaxLine-1]
	}
	_, err := io.WriteString(w, line+"\n")
	return err
}

func printable(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}

func ReadReply(r io.Reader) error {
	line, err := readLine(r)
	if err != nil {
		return err
	}
	if line == "ok" {
		return nil
	}
	rest, ok := strings.CutPrefix(line, "err ")
	if !ok || rest == "" {
		return fmt.Errorf("relay: malformed reply line")
	}
	code, text, _ := strings.Cut(rest, " ")
	return &Error{Code: code, Text: text}
}

func Open(conn io.ReadWriter, r Request) error {
	if err := WriteRequest(conn, r); err != nil {
		return err
	}
	return ReadReply(conn)
}

func readLine(r io.Reader) (string, error) {
	var line [MaxLine]byte
	var one [1]byte
	for n := 0; n < MaxLine; {
		if _, err := io.ReadFull(r, one[:]); err != nil {
			if errors.Is(err, io.EOF) && n > 0 {
				return "", io.ErrUnexpectedEOF
			}
			return "", err
		}
		if one[0] == '\n' {
			return string(line[:n]), nil
		}
		if one[0] < 0x20 || one[0] > 0x7e {
			return "", fmt.Errorf("relay: line contains byte %#x", one[0])
		}
		line[n] = one[0]
		n++
	}
	return "", fmt.Errorf("relay: line longer than %d bytes", MaxLine)
}

const MaxDatagram = 1<<16 - 1

func WriteDatagram(w io.Writer, p []byte) error {
	if len(p) > MaxDatagram {
		return fmt.Errorf("relay: datagram of %d bytes exceeds %d", len(p), MaxDatagram)
	}
	buf := make([]byte, 2+len(p))
	binary.BigEndian.PutUint16(buf, uint16(len(p)))
	copy(buf[2:], p)
	_, err := w.Write(buf)
	return err
}

func ReadDatagram(r io.Reader, buf []byte) (int, error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(head[:]))
	if n > len(buf) {
		return 0, fmt.Errorf("relay: datagram of %d bytes does not fit a buffer of %d", n, len(buf))
	}
	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	return n, nil
}

const (
	Stdin  byte = 0
	Stdout byte = 1
	Stderr byte = 2
)

const MaxFrame = 64 << 10

const frameHeader = 5

func WriteFrame(w io.Writer, stream byte, p []byte) error {
	if stream > Stderr {
		return fmt.Errorf("relay: unknown stream %d", stream)
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
	if stream > Stderr {
		return 0, 0, fmt.Errorf("relay: unknown stream %d", stream)
	}
	if size > MaxFrame || int(size) > len(buf) {
		return 0, 0, fmt.Errorf("relay: frame of %d bytes exceeds the limit", size)
	}
	if _, err := io.ReadFull(r, buf[:size]); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, 0, err
	}
	return stream, int(size), nil
}
