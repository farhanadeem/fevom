package daemon

import (
	"net"

	api "fev/proto/daemon"
)

func (t *sessionTable) openHost(caller Caller, stream *net.UnixConn, resize func(rows, cols int), closeFn func(), wait func() api.SessionExit) (uint64, string) {
	sizes := make(chan [2]int, 4)
	t.mu.Lock()
	t.nextID++
	s := &session{id: t.nextID, caller: caller, over: make(chan struct{}),
		remote: &remoteRun{appEnd: stream, resize: sizes, cancel: closeFn}}
	t.byID[s.id] = s
	t.mu.Unlock()
	token := newSessionToken()
	t.armRemote(s, token)
	go func() {
		for {
			select {
			case sz := <-sizes:
				resize(sz[0], sz[1])
			case <-s.over:
				return
			}
		}
	}()
	go func() {
		e := wait()
		t.forget(s)
		s.exit(e)
	}()
	return s.id, token
}
