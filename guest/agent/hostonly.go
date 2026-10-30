package agent

import "net"

func HostOnly(l net.Listener, fromHost func(net.Addr) bool) net.Listener {
	return &hostOnly{Listener: l, fromHost: fromHost}
}

type hostOnly struct {
	net.Listener
	fromHost func(net.Addr) bool
}

func (h *hostOnly) Accept() (net.Conn, error) {
	for {
		conn, err := h.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if h.fromHost(conn.RemoteAddr()) {
			return conn, nil
		}
		conn.Close()
	}
}
