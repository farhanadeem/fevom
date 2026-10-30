package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	api "fev/proto/agent"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

const maxFetchBody = 64 << 20

func Fetch(ctx context.Context, p api.FetchParams) (*api.FetchResult, error) {
	u, err := url.Parse(p.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("url must be http or https, got %q", p.URL)
	}

	var mu sync.Mutex
	var remote string
	dialer := &net.Dialer{}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err == nil {
				mu.Lock()
				remote = conn.RemoteAddr().String()
				mu.Unlock()
			}
			return conn, err
		},
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "fev-agent")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxFetchBody))
	if err != nil {
		return nil, fmt.Errorf("reading body after %d bytes: %w", n, err)
	}

	r := &api.FetchResult{
		Status:    resp.StatusCode,
		Bytes:     n,
		ElapsedMS: float64(time.Since(start).Microseconds()) / 1000,
	}
	mu.Lock()
	r.RemoteAddr = remote
	mu.Unlock()
	if resp.TLS != nil {
		r.TLS = tls.VersionName(resp.TLS.Version)
	}
	return r, nil
}

func Ping(ctx context.Context, p api.PingParams) (*api.PingResult, error) {
	addrs, err := net.DefaultResolver.LookupIP(ctx, "ip4", p.Address)
	if err != nil {
		return nil, err
	}
	target := &net.IPAddr{IP: addrs[0]}

	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if p.TTL > 0 {
		if err := conn.IPv4PacketConn().SetTTL(p.TTL); err != nil {
			return nil, err
		}
	}
	id := int(time.Now().UnixNano() & 0xffff)
	result := &api.PingResult{}
	var total time.Duration
	buf := make([]byte, 1500)

	for seq := range p.Count {
		msg := icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: id, Seq: seq, Data: []byte("fev-agent ping")}}
		wire, err := msg.Marshal(nil)
		if err != nil {
			return nil, err
		}
		sent := time.Now()
		if _, err := conn.WriteTo(wire, target); err != nil {
			return nil, err
		}
		result.Sent++

		deadline := time.Now().Add(time.Second)
		if d, ok := ctx.Deadline(); ok {
			share := time.Until(d) / time.Duration(p.Count-seq)
			deadline = time.Now().Add(share)
		}
		conn.SetReadDeadline(deadline)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break
				}
				return nil, err
			}
			reply, err := icmp.ParseMessage(1, buf[:n])
			if err != nil {
				continue
			}
			exceeded, ours := matches(reply, id, seq)
			if !ours {
				continue
			}
			result.Received++
			result.From = from.String()
			result.TTLExceeded = result.TTLExceeded || exceeded
			total += time.Since(sent)
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	if result.Received > 0 {
		result.AvgMS = float64(total.Microseconds()) / 1000 / float64(result.Received)
	}
	return result, nil
}

func matches(m *icmp.Message, id, seq int) (exceeded, ours bool) {
	switch body := m.Body.(type) {
	case *icmp.Echo:
		return false, m.Type == ipv4.ICMPTypeEchoReply && body.ID == id && body.Seq == seq
	case *icmp.TimeExceeded:
		data := body.Data
		if len(data) < 1 {
			return false, false
		}
		ihl := int(data[0]&0x0f) * 4
		if ihl < 20 || len(data) < ihl+8 {
			return false, false
		}
		inner, err := icmp.ParseMessage(1, data[ihl:ihl+8])
		if err != nil {
			return false, false
		}
		echo, ok := inner.Body.(*icmp.Echo)
		return true, ok && echo.ID == id && echo.Seq == seq
	}
	return false, false
}
