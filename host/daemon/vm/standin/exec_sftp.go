package standin

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/pkg/sftp"

	api "fev/proto/agent"
	"fev/proto/relay"
)

var (
	sftpMu    sync.Mutex
	sftpFiles = map[string]sftp.Handlers{}
)

func handlers(machine string) sftp.Handlers {
	sftpMu.Lock()
	defer sftpMu.Unlock()
	h, ok := sftpFiles[machine]
	if !ok {
		h = sftp.InMemHandler()
		sftpFiles[machine] = h
	}
	return h
}

type framedStdio struct {
	in  <-chan []byte
	o   *out
	buf []byte
}

func (f *framedStdio) Read(p []byte) (int, error) {
	for len(f.buf) == 0 {
		b, ok := <-f.in
		if !ok {
			return 0, io.EOF
		}
		f.buf = b
	}
	n := copy(p, f.buf)
	f.buf = f.buf[n:]
	return n, nil
}

func (f *framedStdio) Write(p []byte) (int, error) {
	f.o.write(relay.Stdout, p)
	return len(p), nil
}

func (f *framedStdio) Close() error { return nil }

func (s *fakeSession) sftp(o *out, in <-chan []byte) (code, signal int) {
	srv := sftp.NewRequestServer(&framedStdio{in: in, o: o}, handlers(s.machine))
	defer srv.Close()
	if err := srv.Serve(); err != nil && err != io.EOF {
		o.write(relay.Stderr, []byte("fev-standin: sftp: "+err.Error()+"\n"))
		return 1, 0
	}
	return 0, 0
}

func (a *Agent) DialMachine(machine, target string, conn net.Conn) bool {
	m := a.machines()
	m.mu.Lock()
	fm := m.byName[machine]
	running := fm != nil && fm.Info.State == api.MachineRunning
	m.mu.Unlock()
	if !running {
		return false
	}
	peer, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		relay.WriteError(conn, relay.CodeRefused, err.Error())
		return true
	}
	defer peer.Close()
	if relay.WriteOK(conn) != nil {
		return true
	}
	done := make(chan struct{})
	go func() {
		io.Copy(peer, conn)
		if tc, ok := peer.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		close(done)
	}()
	io.Copy(conn, peer)
	if uc, ok := conn.(interface{ CloseWrite() error }); ok {
		uc.CloseWrite()
	}
	<-done
	return true
}
