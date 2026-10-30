package agent

import (
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

type peer struct {
	net.Conn
	from string
}

type addr string

func (a addr) Network() string { return "test" }
func (a addr) String() string  { return string(a) }

func (p peer) RemoteAddr() net.Addr { return addr(p.from) }

type claims struct {
	net.Listener
	from []string
}

func (c *claims) Accept() (net.Conn, error) {
	conn, err := c.Listener.Accept()
	if err != nil {
		return nil, err
	}
	from := c.from[0]
	c.from = c.from[1:]
	return peer{conn, from}, nil
}

func TestOnlyTheHostsConnectionsAreAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s")
	inner, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	l := HostOnly(&claims{inner, []string{"guest", "host"}}, func(a net.Addr) bool { return a.String() == "host" })

	accepted := make(chan net.Conn, 2)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()

	local, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	local.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := local.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("a connection from inside the guest was not closed: %v", err)
	}
	select {
	case <-accepted:
		t.Fatal("a connection from inside the guest was accepted")
	default:
	}

	host, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	select {
	case conn := <-accepted:
		if conn.RemoteAddr().String() != "host" {
			t.Errorf("accepted a connection from %s", conn.RemoteAddr())
		}
		conn.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the host's connection was not accepted")
	}
}
