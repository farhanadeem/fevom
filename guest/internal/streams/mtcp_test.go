package streams

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"fev/proto/relay"
)

func TestRelayMTCPDialsInTheNamedMachine(t *testing.T) {
	l := listenTCP(t)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				data, _ := io.ReadAll(c)
				c.Write(bytes.ToUpper(data))
			}()
		}
	}()
	closed := listenTCP(t)
	closedAddr := closed.Addr().String()
	closed.Close()
	var asked []string
	r := &Relay{Machine: func(machine string, target netip.AddrPort, timeout time.Duration) (net.Conn, error) {
		asked = append(asked, machine+" "+target.String())
		if machine != "u24" {
			return nil, fmt.Errorf("%s: %w", machine, ErrNoMachine)
		}
		if timeout <= 0 {
			return nil, errors.New("no time limit on the dial")
		}
		return net.DialTimeout("tcp", target.String(), timeout)
	}}
	host := serve(t, r)
	if err := relay.Open(host, relay.Request{Kind: relay.MTCP, Machine: "u24", Target: l.Addr().String()}); err != nil {
		t.Fatal(err)
	}
	host.Write([]byte("in the machine"))
	host.CloseWrite()
	if got, err := io.ReadAll(host); err != nil || string(got) != "IN THE MACHINE" {
		t.Fatalf("got %q, %v", got, err)
	}
	if len(asked) != 1 || asked[0] != "u24 "+l.Addr().String() {
		t.Errorf("the dialer was asked %q", asked)
	}
	for name, c := range map[string]struct {
		r    *Relay
		req  relay.Request
		code string
	}{
		"no such machine":   {r, relay.Request{Kind: relay.MTCP, Machine: "f43", Target: l.Addr().String()}, relay.CodeDenied},
		"nothing listening": {r, relay.Request{Kind: relay.MTCP, Machine: "u24", Target: closedAddr}, relay.CodeRefused},
		"no machines":       {&Relay{}, relay.Request{Kind: relay.MTCP, Machine: "u24", Target: l.Addr().String()}, relay.CodeDenied},
	} {
		host := serve(t, c.r)
		err := relay.Open(host, c.req)
		var refused *relay.Error
		if !errors.As(err, &refused) || refused.Code != c.code {
			t.Errorf("%s: got %v, want code %s", name, err, c.code)
		}
	}
}
