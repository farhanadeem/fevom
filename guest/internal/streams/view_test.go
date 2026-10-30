package streams

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"fev/proto/relay"
)

func TestRelayViewDialsTheView(t *testing.T) {
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
	running := true
	asked := 0
	r := &Relay{View: func(timeout time.Duration) (net.Conn, error) {
		asked++
		if timeout <= 0 {
			return nil, errors.New("no time limit on the dial")
		}
		if !running {
			return nil, ErrViewStopped
		}
		return net.DialTimeout("tcp", l.Addr().String(), timeout)
	}}
	host := serve(t, r)
	if err := relay.Open(host, relay.Request{Kind: relay.View, Target: relay.ViewNFS}); err != nil {
		t.Fatal(err)
	}
	host.Write([]byte("compound"))
	host.CloseWrite()
	if got, err := io.ReadAll(host); err != nil || string(got) != "COMPOUND" {
		t.Fatalf("got %q, %v", got, err)
	}
	running = false
	for name, c := range map[string]struct {
		r    *Relay
		code string
	}{
		"stopped": {r, relay.CodeRefused},
		"no view": {&Relay{}, relay.CodeDenied},
	} {
		host := serve(t, c.r)
		err := relay.Open(host, relay.Request{Kind: relay.View, Target: relay.ViewNFS})
		var refused *relay.Error
		if !errors.As(err, &refused) || refused.Code != c.code {
			t.Errorf("%s: got %v, want code %s", name, err, c.code)
		}
	}
	if asked != 2 {
		t.Errorf("the dialer was asked %d times", asked)
	}
}
