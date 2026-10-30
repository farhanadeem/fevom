package relay

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

const token = "0123456789abcdef0123456789abcdef"

func TestRequestLinesRoundTrip(t *testing.T) {
	for _, c := range []struct {
		req  Request
		wire string
	}{
		{Request{Kind: TCP, Target: "127.0.0.1:8080"}, "tcp 127.0.0.1:8080\n"},
		{Request{Kind: TCP, Target: "[::1]:8080"}, "tcp [::1]:8080\n"},
		{Request{Kind: UDP, Target: "172.17.0.2:53"}, "udp 172.17.0.2:53\n"},
		{Request{Kind: Exec, Target: token}, "exec " + token + "\n"},
		{Request{Kind: Stream, Target: token}, "stream " + token + "\n"},
		{Request{Kind: MTCP, Machine: "ubuntu", Target: "127.0.0.1:22"}, "mtcp ubuntu 127.0.0.1:22\n"},
		{Request{Kind: MTCP, Machine: "f43-dev", Target: "[::1]:8080"}, "mtcp f43-dev [::1]:8080\n"},
		{Request{Kind: MTCP, Machine: "Dev.1box", Target: "127.0.0.1:22"}, "mtcp Dev.1box 127.0.0.1:22\n"},
		{Request{Kind: View, Target: ViewNFS}, "view nfs\n"},
	} {
		var buf bytes.Buffer
		if err := WriteRequest(&buf, c.req); err != nil {
			t.Fatalf("%v: %v", c.req, err)
		}
		if buf.String() != c.wire {
			t.Errorf("%v encodes as %q, want %q", c.req, buf.String(), c.wire)
		}
		got, err := ReadRequest(&buf)
		if err != nil || got != c.req {
			t.Errorf("%q decodes as %v, %v", c.wire, got, err)
		}
	}
}

func TestMalformedRequestsAreRefused(t *testing.T) {
	for _, line := range []string{
		"\n",
		"tcp\n",
		"tcp \n",
		"tcp localhost:80\n",
		"tcp 127.0.0.1\n",
		"tcp 127.0.0.1:0\n",
		"tcp 127.0.0.1:99999\n",
		"tcp [fe80::1%eth0]:80\n",
		"tcp 127.0.0.1:80 extra\n",
		"unix /run/docker.sock\n",
		"TCP 127.0.0.1:80\n",
		"exec short\n",
		"exec " + strings.ToUpper(token) + "\n",
		"exec " + token + "0\n",
		"stream short\n",
		"stream " + strings.ToUpper(token) + "\n",
		"stream 127.0.0.1:22\n",
		"mtcp 127.0.0.1:22\n",
		"mtcp ubuntu\n",
		"mtcp ub_untu 127.0.0.1:22\n",
		"mtcp Default 127.0.0.1:22\n",
		"mtcp default 127.0.0.1:22\n",
		"mtcp ubuntu localhost:22\n",
		"mtcp ubuntu 127.0.0.1:22 extra\n",
		"mtcp ubuntu " + token + "\n",
		"mtcp ub@untu 127.0.0.1:22\n",
		"mtcp " + token + token + "x 127.0.0.1:22\n",
		"mtcp .ubuntu 127.0.0.1:22\n",
		"view\n",
		"view 127.0.0.1:2049\n",
		"view nfs extra\n",
		"view NFS\n",
		"view ubuntu nfs\n",
		"tcp 127.0.0.1:80\r\n",
		"tcp 127.0.0.1:80\x00\n",
		"tcp 127.0.0.1:80",
		"tcp " + strings.Repeat("1", MaxLine) + "\n",
	} {
		if req, err := ReadRequest(strings.NewReader(line)); err == nil {
			t.Errorf("%q was accepted as %v", line, req)
		}
	}
	for _, req := range []Request{{Kind: TCP, Target: "localhost:80"}, {Kind: Exec, Target: "x"}, {Kind: "unix", Target: "/a"}, {Kind: UDP, Target: "1.2.3.4:80\ntcp 127.0.0.1:22"},
		{Kind: TCP, Machine: "ubuntu", Target: "127.0.0.1:22"}, {Kind: MTCP, Target: "127.0.0.1:22"}, {Kind: MTCP, Machine: "a b", Target: "127.0.0.1:22"},
		{Kind: View, Target: "127.0.0.1:2049"}, {Kind: View, Machine: "u", Target: ViewNFS}} {
		var buf bytes.Buffer
		if err := WriteRequest(&buf, req); err == nil || buf.Len() != 0 {
			t.Errorf("%v was sent as %q", req, buf.String())
		}
	}
	if _, err := ReadRequest(strings.NewReader("")); !errors.Is(err, io.EOF) {
		t.Errorf("an empty connection gives %v, want io.EOF", err)
	}
}

func TestTheMachineNameRule(t *testing.T) {
	label63 := strings.Repeat("x", MaxMachineLabel)
	for _, name := range []string{
		"1abc", "UPPER", "a.b", "x-y",
		"ubuntu", "a", "f43", "my-box-2", "Ubuntu", "1box", "123", "a.b.c", "u24.Dev-1", "MiXeD.CaSe",
		label63, label63[:62] + ".y", "y." + label63[:62], "a-b.c-d", "x--y",
		"default.x", "x.docker", "dockers", "defaults",
	} {
		if err := CheckMachineName(name); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	for _, name := range []string{
		"a_b", "docker", "default",
		"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijkl",
		"Docker", "DOCKER", "Default", "dEfAuLt",
		"", "-box", "box-", "a.-b", "a-.b", "my_box", "u@m", "a b", "a/b", "a:b", "a\nb", "a\x00b",
		".images", ".records", ".a", "a.", "a..b", ".", "..", "caf\u00e9", "\u212aelvin",
		label63 + "x", strings.Repeat("x", MaxMachineName+1), label63[:32] + "." + label63[:32],
	} {
		if err := CheckMachineName(name); err == nil {
			t.Errorf("%q was accepted as a machine's name", name)
		}
	}
	if n := "abcdefghijklmnopqrstuvwxyzabcdefghijkl"; len(n) != 38 || len(label63[:32]+"."+label63[:32]) != 65 {
		t.Fatal("the test's own lengths are wrong")
	}
}

func TestMachineNameKeysFoldCase(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"UPPER", "upper", true}, {"Mixed.Case", "mixed.CASE", true}, {"a-1", "A-1", true},
		{"a.b", "ab", false}, {"u24", "u26", false}, {"x", "x", true},
	} {
		if got := SameMachineName(c.a, c.b); got != c.same {
			t.Errorf("SameMachineName(%q, %q) = %v", c.a, c.b, got)
		}
	}
	if k := MachineNameKey("Fed43.Dev-B"); k != "fed43.dev-b" {
		t.Errorf("the key of Fed43.Dev-B is %q", k)
	}
}

type countingReader struct {
	r    io.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}

func TestNothingIsReadPastTheLine(t *testing.T) {
	const line = "tcp 127.0.0.1:8080\n"
	in := &countingReader{r: strings.NewReader(line + "GET / HTTP/1.1\r\n")}
	if _, err := ReadRequest(in); err != nil {
		t.Fatal(err)
	}
	if in.read != len(line) {
		t.Fatalf("read %d bytes for a %d byte line: the start of the stream was swallowed", in.read, len(line))
	}
	rest, _ := io.ReadAll(in)
	if string(rest) != "GET / HTTP/1.1\r\n" {
		t.Fatalf("stream after the line is %q", rest)
	}

	in = &countingReader{r: strings.NewReader("ok\nHTTP/1.1 200 OK\r\n")}
	if err := ReadReply(in); err != nil || in.read != 3 {
		t.Fatalf("reply: err %v after %d bytes", err, in.read)
	}
}

func TestRepliesRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	WriteOK(&buf)
	if buf.String() != "ok\n" {
		t.Fatalf("ok encodes as %q", buf.String())
	}
	if err := ReadReply(&buf); err != nil {
		t.Fatal(err)
	}

	WriteError(&buf, CodeRefused, "dial tcp 127.0.0.1:8080:\n connection refused")
	if buf.String() != "err refused dial tcp 127.0.0.1:8080: connection refused\n" {
		t.Fatalf("refusal encodes as %q", buf.String())
	}
	var e *Error
	if err := ReadReply(&buf); !errors.As(err, &e) || e.Code != CodeRefused || e.Text != "dial tcp 127.0.0.1:8080: connection refused" {
		t.Fatalf("refusal decodes as %v", err)
	}

	WriteError(&buf, "two words", strings.Repeat("x", 2*MaxLine))
	if buf.Len() != MaxLine || !strings.HasPrefix(buf.String(), "err failed x") {
		t.Fatalf("long refusal is %d bytes: %.20q", buf.Len(), buf.String())
	}
	if err := ReadReply(&buf); !errors.As(err, &e) || e.Code != CodeFailed {
		t.Fatalf("long refusal decodes as %v", err)
	}

	WriteError(&buf, CodeFailed, "exec /usr/bin/caf\u00e9\x01: no such file")
	if err := ReadReply(&buf); !errors.As(err, &e) || e.Code != CodeFailed || e.Text != "exec /usr/bin/caf???: no such file" {
		t.Fatalf("a refusal with non-ASCII text decodes as %v", err)
	}
	buf.Reset()
	WriteError(&buf, "caf\u00e9", "x")
	if err := ReadReply(&buf); !errors.As(err, &e) || e.Code != CodeFailed || e.Text != "x" {
		t.Fatalf("a refusal with a non-ASCII code decodes as %v", err)
	}
	buf.Reset()

	WriteError(&buf, CodeDenied, "")
	if err := ReadReply(&buf); !errors.As(err, &e) || e.Code != CodeDenied || e.Text != "" {
		t.Fatalf("bare refusal decodes as %v", err)
	}

	for _, line := range []string{"OK\n", "err\n", "err \n", "yes\n", "ok", ""} {
		err := ReadReply(strings.NewReader(line))
		if err == nil || errors.As(err, &e) {
			t.Errorf("reply %q gives %v, want a protocol error", line, err)
		}
	}
}

func TestOpenIsTheHostsHalfOfTheHandshake(t *testing.T) {
	serve := func(answer func(net.Conn, Request)) net.Conn {
		host, guest := net.Pipe()
		go func() {
			req, err := ReadRequest(guest)
			if err != nil {
				WriteError(guest, CodeBadRequest, err.Error())
				guest.Close()
				return
			}
			answer(guest, req)
		}()
		return host
	}

	conn := serve(func(c net.Conn, req Request) {
		WriteOK(c)
		io.Copy(c, c)
	})
	if err := Open(conn, Request{Kind: TCP, Target: "127.0.0.1:8080"}); err != nil {
		t.Fatal(err)
	}
	go conn.Write([]byte("hello"))
	got := make([]byte, 5)
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != "hello" {
		t.Fatalf("stream after ok: %q, %v", got, err)
	}
	conn.Close()

	conn = serve(func(c net.Conn, req Request) {
		WriteError(c, CodeRefused, "connection refused")
		c.Close()
	})
	var e *Error
	if err := Open(conn, Request{Kind: TCP, Target: "127.0.0.1:1"}); !errors.As(err, &e) || e.Code != CodeRefused {
		t.Fatalf("refused dial gives %v", err)
	}
}

func TestDatagramsKeepTheirBoundaries(t *testing.T) {
	var buf bytes.Buffer
	sent := [][]byte{[]byte("one"), {}, bytes.Repeat([]byte{0xab}, MaxDatagram), []byte("last")}
	for _, d := range sent {
		if err := WriteDatagram(&buf, d); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteDatagram(&buf, make([]byte, MaxDatagram+1)); err == nil {
		t.Fatal("a datagram too long for its length field was sent")
	}
	scratch := make([]byte, MaxDatagram)
	for i, want := range sent {
		n, err := ReadDatagram(&buf, scratch)
		if err != nil || !bytes.Equal(scratch[:n], want) {
			t.Fatalf("datagram %d: %d bytes, %v", i, n, err)
		}
	}
	if _, err := ReadDatagram(&buf, scratch); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream gives %v, want io.EOF", err)
	}

	if _, err := ReadDatagram(bytes.NewReader([]byte{0, 5, 'a', 'b'}), scratch); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated datagram gives %v", err)
	}
	if _, err := ReadDatagram(bytes.NewReader([]byte{0}), scratch); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated length gives %v", err)
	}
	if _, err := ReadDatagram(bytes.NewReader([]byte{0xff, 0xff, 1, 2, 3}), make([]byte, 16)); err == nil {
		t.Fatal("a datagram longer than the buffer was accepted")
	}
}

func TestFramesKeepStreamsApart(t *testing.T) {
	var buf bytes.Buffer
	big := bytes.Repeat([]byte("0123456789abcdef"), (MaxFrame+MaxFrame/2)/16)
	WriteFrame(&buf, Stdout, []byte("out"))
	WriteFrame(&buf, Stderr, []byte("err"))
	WriteFrame(&buf, Stdout, big)
	WriteFrame(&buf, Stdin, nil)
	if err := WriteFrame(&buf, 3, []byte("x")); err == nil {
		t.Fatal("a frame on an unknown stream was sent")
	}

	scratch := make([]byte, MaxFrame)
	var out, errs []byte
	for {
		stream, n, err := ReadFrame(&buf, scratch)
		if errors.Is(err, io.EOF) {
			t.Fatal("stream ended before the end-of-file frame")
		}
		if err != nil {
			t.Fatal(err)
		}
		if stream == Stdin {
			if n != 0 {
				t.Fatalf("stdin frame of %d bytes", n)
			}
			break
		}
		if stream == Stdout {
			out = append(out, scratch[:n]...)
		} else {
			errs = append(errs, scratch[:n]...)
		}
	}
	if string(errs) != "err" || !bytes.Equal(out, append([]byte("out"), big...)) {
		t.Fatalf("got %d bytes of stdout and %q on stderr", len(out), errs)
	}
	if _, _, err := ReadFrame(&buf, scratch); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream gives %v, want io.EOF", err)
	}

	for name, wire := range map[string][]byte{
		"unknown stream":   {9, 0, 0, 0, 1, 'x'},
		"oversized length": {1, 0xff, 0xff, 0xff, 0xff},
		"truncated header": {1, 0, 0},
		"truncated body":   {1, 0, 0, 0, 4, 'a'},
	} {
		if _, _, err := ReadFrame(bytes.NewReader(wire), scratch); err == nil || err == io.EOF {
			t.Errorf("%s: got %v, want an error that is not a clean end", name, err)
		}
	}
}
