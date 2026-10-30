package debugremote

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Context struct {
	DockerCLI string
	Config    string
	Name      string
	Host      string
	Extra     []string
}

func unixSocketPath(host string) (string, bool) {
	for _, p := range []string{"unix://", "unix:"} {
		if rest, ok := strings.CutPrefix(host, p); ok {
			rest = strings.TrimPrefix(rest, "//")
			if !strings.HasPrefix(rest, "/") {
				rest = "/" + rest
			}
			return rest, true
		}
	}
	return "", false
}

func (c Context) args(sub ...string) []string {
	var a []string
	if c.Config != "" {
		a = append(a, "--config", c.Config)
	}
	if c.Name != "" && c.Name != "default" {
		a = append(a, "--context", c.Name)
	}
	a = append(a, c.Extra...)
	return append(a, sub...)
}

type Transport struct {
	Ctx      Context
	Env      []string
	DialFunc func(ctx context.Context) (net.Conn, error)
}

type dialConn struct {
	cmd    *exec.Cmd
	w      io.WriteCloser
	r      io.ReadCloser
	stderr *strings.Builder
	once   sync.Once
	closed chan struct{}
}

func (d *dialConn) Read(p []byte) (int, error) { return d.r.Read(p) }

func (d *dialConn) CloseWrite() error           { return d.w.Close() }
func (d *dialConn) Write(p []byte) (int, error) { return d.w.Write(p) }

func (d *dialConn) Close() error {
	d.once.Do(func() {
		d.w.Close()
		d.r.Close()
		if d.cmd.Process != nil {
			d.cmd.Process.Kill()
		}
		go func() { d.cmd.Wait(); close(d.closed) }()
	})
	return nil
}

type dialAddr struct{}

func (dialAddr) Network() string { return "dial-stdio" }
func (dialAddr) String() string  { return "docker" }

func (d *dialConn) LocalAddr() net.Addr                { return dialAddr{} }
func (d *dialConn) RemoteAddr() net.Addr               { return dialAddr{} }
func (d *dialConn) SetDeadline(t time.Time) error      { return nil }
func (d *dialConn) SetReadDeadline(t time.Time) error  { return nil }
func (d *dialConn) SetWriteDeadline(t time.Time) error { return nil }

func (t *Transport) dial(ctx context.Context) (net.Conn, error) {
	if t.DialFunc != nil {
		return t.DialFunc(ctx)
	}
	if p, ok := unixSocketPath(t.Ctx.Host); ok {
		var d net.Dialer
		return d.DialContext(ctx, "unix", p)
	}
	cmd := exec.Command(t.Ctx.DockerCLI, t.Ctx.args("system", "dial-stdio")...)
	cmd.Env = t.Env
	w, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	r, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("docker system dial-stdio: %w", err)
	}
	return &dialConn{cmd: cmd, w: w, r: r, stderr: &stderr, closed: make(chan struct{})}, nil
}

func (t *Transport) Client() *http.Client {
	tr := &http.Transport{
		DialContext:           func(ctx context.Context, _, _ string) (net.Conn, error) { return t.dial(ctx) },
		DisableCompression:    true,
		MaxConnsPerHost:       0,
		ResponseHeaderTimeout: 0,
	}
	return &http.Client{Transport: tr}
}

func (t *Transport) probe(ctx context.Context) error {
	c, err := t.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	fmt.Fprintf(c, "GET /_ping HTTP/1.1\r\nHost: docker\r\nConnection: close\r\n\r\n")
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		if dc, ok := c.(*dialConn); ok {
			if s := strings.TrimSpace(dc.stderr.String()); s != "" {
				return fmt.Errorf("%s", firstLine(s))
			}
		}
		return fmt.Errorf("reaching the engine: %w", err)
	}
	if !strings.Contains(line, "200") {
		return fmt.Errorf("the engine answered %q to a ping", strings.TrimSpace(line))
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
