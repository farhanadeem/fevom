package kube

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	"golang.org/x/net/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"fev/host/k8s"
	api "fev/proto/daemon"
)

const execProtocol = "v5.channel.k8s.io"

const (
	chanStdin  = 0
	chanStdout = 1
	chanStderr = 2
	chanError  = 3
	chanResize = 4
	chanClose  = 255
)

var frameHook func(out bool, msg []byte)

func (c *Cluster) ExecTarget(ctx context.Context, p api.K8sExecParams) (string, error) {
	cs, _, err := c.clients()
	if err != nil {
		return "", err
	}
	pod, name, err := container(ctx, cs, p.Namespace, p.Pod, p.Container)
	if err != nil {
		return "", err
	}
	if pod.Status.Phase != "Running" {
		return "", k8s.Errorf(api.CodeFailed, "pod %s/%s is %s: a terminal needs it running", p.Namespace, p.Pod, pod.Status.Phase)
	}
	return name, nil
}

func execURL(rc *rest.Config, p api.K8sExecParams) (*url.URL, error) {
	u, err := url.Parse(rc.Host)
	if err != nil {
		return nil, err
	}
	argv := p.Argv
	if len(argv) == 0 {
		argv = []string{"sh"}
	}
	q := url.Values{}
	for _, a := range argv {
		q.Add("command", a)
	}
	q.Set("container", p.Container)
	q.Set("stdin", "true")
	q.Set("stdout", "true")
	if p.TTY {
		q.Set("tty", "true")
	} else {
		q.Set("stderr", "true")
	}
	u.Path = fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s/exec", u.Path, url.PathEscape(p.Namespace), url.PathEscape(p.Pod))
	u.RawQuery = q.Encode()
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return nil, fmt.Errorf("the API server's address %q is not http or https", rc.Host)
	}
	return u, nil
}

func dialExec(ctx context.Context, rc *rest.Config, u *url.URL) (*websocket.Conn, error) {
	dial := rc.Dial
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "443")
	}
	conn, err := dial(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	var rwc io.ReadWriteCloser = conn
	if u.Scheme == "wss" {
		tc, err := rest.TLSConfigFor(rc)
		if err != nil {
			conn.Close()
			return nil, err
		}
		if tc == nil {
			tc = &tls.Config{}
		}
		tc = tc.Clone()
		if tc.ServerName == "" {
			tc.ServerName = u.Hostname()
		}
		tc.NextProtos = []string{"http/1.1"}
		t := tls.Client(conn, tc)
		if err := t.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		rwc = t
	}
	origin := *u
	origin.Scheme, origin.Path, origin.RawQuery = "https", "", ""
	cfg, err := websocket.NewConfig(u.String(), origin.String())
	if err != nil {
		rwc.Close()
		return nil, err
	}
	cfg.Protocol = []string{execProtocol}
	cfg.Header = http.Header{"User-Agent": {"fev-k8s"}}
	if rc.BearerToken != "" {
		cfg.Header.Set("Authorization", "Bearer "+rc.BearerToken)
	}
	stop := context.AfterFunc(ctx, func() { rwc.Close() })
	ws, err := websocket.NewClient(cfg, rwc)
	stop()
	if err != nil {
		rwc.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("the API server refused the exec: %w", err)
	}
	ws.PayloadType = websocket.BinaryFrame
	return ws, nil
}

func (c *Cluster) Exec(ctx context.Context, p api.K8sExecParams, stream io.ReadWriter, resize <-chan [2]int) (int, error) {
	c.mu.Lock()
	rc := c.rest
	c.mu.Unlock()
	if rc == nil {
		return -1, k8s.Errorf(api.CodeFailed, "the cluster's client has not started")
	}
	if p.Container == "" {
		name, err := c.ExecTarget(ctx, p)
		if err != nil {
			return -1, err
		}
		p.Container = name
	}
	u, err := execURL(rc, p)
	if err != nil {
		return -1, err
	}
	ws, err := dialExec(ctx, rc, u)
	if err != nil {
		return -1, err
	}
	defer ws.Close()
	ectx, cancel := context.WithCancel(ctx)
	defer cancel()
	context.AfterFunc(ectx, func() { ws.Close() })

	var wmu sync.Mutex
	send := func(msg []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		if frameHook != nil {
			frameHook(true, msg)
		}
		return websocket.Message.Send(ws, msg)
	}
	go func() {
		for {
			select {
			case sz := <-resize:
				b, _ := json.Marshal(struct {
					Width  uint16
					Height uint16
				}{uint16(sz[1]), uint16(sz[0])})
				if send(append([]byte{chanResize}, b...)) != nil {
					return
				}
			case <-ectx.Done():
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				if send(append([]byte{chanStdin}, buf[:n]...)) != nil {
					return
				}
			}
			if err != nil {
				if ectx.Err() == nil {
					send([]byte{chanClose, chanStdin})
				}
				return
			}
		}
	}()
	var status *metav1.Status
	for {
		var msg []byte
		err := websocket.Message.Receive(ws, &msg)
		if err != nil {
			if status != nil {
				return exitOf(status)
			}
			if ctx.Err() != nil {
				return -1, nil
			}
			if errors.Is(err, io.EOF) {
				return -1, errors.New("the exec's stream ended with no exit status")
			}
			return -1, fmt.Errorf("the exec's stream: %w", err)
		}
		if frameHook != nil {
			frameHook(false, msg)
		}
		if len(msg) < 2 {
			continue
		}
		switch msg[0] {
		case chanStdout, chanStderr:
			if _, err := stream.Write(msg[1:]); err != nil {
				return -1, fmt.Errorf("the exec's terminal: %w", err)
			}
		case chanError:
			var s metav1.Status
			if err := json.Unmarshal(msg[1:], &s); err != nil {
				return -1, fmt.Errorf("the exec's status %q: %w", msg[1:], err)
			}
			status = &s
		}
	}
}

func exitOf(s *metav1.Status) (int, error) {
	if s.Status == metav1.StatusSuccess {
		return 0, nil
	}
	if s.Reason == "NonZeroExitCode" && s.Details != nil {
		for _, c := range s.Details.Causes {
			if c.Type == "ExitCode" {
				n, err := strconv.Atoi(c.Message)
				if err == nil {
					return n, nil
				}
			}
		}
	}
	return -1, errors.New(oneLine(s.Message))
}
