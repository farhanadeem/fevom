package daemon

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"fev/proto/relay"
)

func sessionServer(t *testing.T, reply string, pass func() int) string {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "bh.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	os.MkdirAll(filepath.Join(home, "run"), 0o700)
	l, err := net.Listen("unix", SessionSocketPath(home))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		uc := c.(*net.UnixConn)
		if _, err := relay.ReadRequest(uc); err != nil {
			return
		}
		var rights []byte
		if pass != nil {
			fd := pass()
			defer syscall.Close(fd)
			rights = syscall.UnixRights(fd)
		}
		uc.WriteMsgUnix([]byte(reply), rights, nil)
		io.Copy(io.Discard, uc)
	}()
	return home
}

const testToken = "0123456789abcdef0123456789abcdef"

func TestAttachSessionTakesThePassedStream(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[1])
	syscall.Write(fds[1], []byte("from the guest"))
	home := sessionServer(t, "ok\n", func() int { return fds[0] })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := AttachSession(ctx, home, testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	buf := make([]byte, 64)
	n, _ := stream.Read(buf)
	if string(buf[:n]) != "from the guest" {
		t.Fatalf("read %q", buf[:n])
	}
}

func TestAttachSessionRefusesAReplyWithoutTheStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AttachSession(ctx, sessionServer(t, "ok\n", nil), testToken); err == nil || !strings.Contains(err.Error(), "without the session's stream") {
		t.Errorf("ok with no descriptor: %v", err)
	}
	if _, err := AttachSession(ctx, sessionServer(t, "err denied no such session\n", nil), testToken); err == nil || !strings.Contains(err.Error(), "no such session") {
		t.Errorf("a refusal: %v", err)
	}
	if _, err := AttachSession(ctx, "/tmp", "not-a-token"); err == nil {
		t.Error("a token that is not one was sent")
	}
}
