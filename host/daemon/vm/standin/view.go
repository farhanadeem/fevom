package standin

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"
	"fev/proto/relay"
)

type viewState struct {
	mu      sync.Mutex
	st      api.ViewStatus
	owner   *api.ViewStartParams
	objects []api.ViewObject
}

func (a *Agent) SetViewObjects(objs []api.ViewObject) {
	a.view.mu.Lock()
	a.view.objects = append([]api.ViewObject(nil), objs...)
	a.view.mu.Unlock()
}

func (a *Agent) ViewRemoved(paths []string) {
	a.mu.Lock()
	host := a.host
	a.mu.Unlock()
	if host != nil {
		host.Notify(api.NotifyViewRemoved, api.ViewRemoved{Paths: paths})
	}
}

const maxViewThreads = 64

func viewStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (a *Agent) View() (api.ViewStatus, *api.ViewStartParams) {
	a.view.mu.Lock()
	defer a.view.mu.Unlock()
	return a.view.statusLocked(), a.view.owner
}

func (v *viewState) statusLocked() api.ViewStatus {
	if v.st.State == "" {
		return api.ViewStatus{State: api.ViewStopped, Since: viewStamp()}
	}
	return v.st
}

func (a *Agent) viewNotify(st api.ViewStatus) {
	a.mu.Lock()
	host := a.host
	a.mu.Unlock()
	if host != nil {
		host.Notify(api.NotifyViewChanged, st)
	}
}

func (a *Agent) viewSet(state string) api.ViewStatus {
	a.view.st = api.ViewStatus{State: state, Since: viewStamp()}
	if state == api.ViewReady {
		a.view.st.Objects = &api.ViewObjects{}
	}
	st := a.view.st
	go a.viewNotify(st)
	return st
}

func (a *Agent) viewCall(method string, params json.RawMessage) (any, error) {
	a.view.mu.Lock()
	defer a.view.mu.Unlock()
	switch method {
	case api.MethodViewStart:
		p, err := jsonrpc.Bind[api.ViewStartParams](params)
		if err != nil {
			return nil, err
		}
		if p.Threads < 0 || p.Threads > maxViewThreads {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "threads must be between 0 (the default, 4) and %d", maxViewThreads)
		}
		switch a.view.statusLocked().State {
		case api.ViewStarting, api.ViewReady:
			return a.view.statusLocked(), nil
		}
		a.view.owner = &p
		st := a.viewSet(api.ViewStarting)
		go func() {
			a.view.mu.Lock()
			defer a.view.mu.Unlock()
			if a.view.st.State == api.ViewStarting {
				a.viewSet(api.ViewReady)
			}
		}()
		return st, nil
	case api.MethodViewObjects:
		l := api.ViewObjectList{Objects: []api.ViewObject{}}
		if a.view.statusLocked().State == api.ViewReady {
			l.Objects = append(l.Objects, a.view.objects...)
		}
		return l, nil
	case api.MethodViewStop:
		a.view.owner = nil
		if a.view.statusLocked().State == api.ViewStopped {
			return a.view.statusLocked(), nil
		}
		return a.viewSet(api.ViewStopped), nil
	default:
		return a.view.statusLocked(), nil
	}
}

type viewRelay struct {
	net.Listener
	agent *Agent
	nfs   string
	wait  func()
}

func (v *viewRelay) Accept() (net.Conn, error) {
	for {
		c, err := v.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uc, ok := c.(*net.UnixConn)
		if !ok {
			return c, nil
		}
		line, err := readLine(uc)
		if err != nil {
			uc.Close()
			continue
		}
		req, err := relay.ReadRequest(strings.NewReader(line))
		if err != nil || req.Kind != relay.View {
			return &replayConn{UnixConn: uc, prefix: []byte(line)}, nil
		}
		go v.serve(uc)
	}
}

func (v *viewRelay) serve(c *net.UnixConn) {
	defer c.Close()
	st, _ := v.agent.View()
	if v.nfs == "" || st.State != api.ViewReady {
		relay.WriteError(c, relay.CodeRefused, "the view is not running")
		return
	}
	peer, err := net.Dial("unix", v.nfs)
	if err != nil {
		relay.WriteError(c, relay.CodeRefused, err.Error())
		return
	}
	defer peer.Close()
	if relay.WriteOK(c) != nil {
		return
	}
	done := make(chan struct{}, 2)
	half := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 64<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				v.wait()
				if _, werr := dst.Write(buf[:n]); werr != nil {
					dst.Close()
					src.Close()
					return
				}
			}
			if err != nil {
				if hc, ok := dst.(interface{ CloseWrite() error }); ok && errors.Is(err, io.EOF) {
					hc.CloseWrite()
				} else {
					dst.Close()
					src.Close()
				}
				return
			}
		}
	}
	go half(peer, c)
	go half(c, peer)
	<-done
	<-done
}

func readLine(c net.Conn) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for len(b) < relay.MaxLine+1 {
		if _, err := io.ReadFull(c, one); err != nil {
			return "", err
		}
		b = append(b, one[0])
		if one[0] == '\n' {
			return string(b), nil
		}
	}
	return string(b), nil
}

type replayConn struct {
	*net.UnixConn
	prefix []byte
}

func (r *replayConn) Read(p []byte) (int, error) {
	if len(r.prefix) > 0 {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		return n, nil
	}
	return r.UnixConn.Read(p)
}
