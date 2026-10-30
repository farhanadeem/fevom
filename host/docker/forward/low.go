package forward

import (
	"errors"
	"fmt"
	"net"
	"os"
)

type LowBinder interface {
	BindLow(proto string, family int, port uint16) (*os.File, error)
}

func (t *Table) SetLowBinder(b LowBinder) {
	t.mu.Lock()
	t.low = b
	t.mu.Unlock()
}

func (t *Table) bindLow(r *row) (made []any, addrs []string, ok bool) {
	closeAll := func() {
		for _, l := range made {
			l.(interface{ Close() error }).Close()
		}
	}
	for _, family := range []int{4, 6} {
		l, addr, err := t.lowSocket(r.key, family)
		if err != nil {
			closeAll()
			t.log.Info("port forward: a low port for this Mac alone is bound on the wildcard addresses: fev's admin helper did not bind it on loopback",
				"id", r.id, "port", r.key.port, "proto", r.key.proto, "family", family, "err", err)
			return nil, nil, false
		}
		made = append(made, l)
		addrs = append(addrs, addr)
	}
	return made, addrs, true
}

func (t *Table) lowSocket(k key, family int) (any, string, error) {
	f, err := t.low.BindLow(k.proto, family, k.port)
	if err != nil {
		return nil, "", err
	}
	if f == nil {
		return nil, "", errors.New("no socket came with the answer")
	}
	defer f.Close()
	if k.proto == TCP {
		l, err := net.FileListener(f)
		if err != nil {
			return nil, "", err
		}
		tl, ok := l.(*net.TCPListener)
		if !ok {
			l.Close()
			return nil, "", fmt.Errorf("not a TCP socket: %T", l)
		}
		return tl, tl.Addr().String(), nil
	}
	pc, err := net.FilePacketConn(f)
	if err != nil {
		return nil, "", err
	}
	uc, ok := pc.(*net.UDPConn)
	if !ok {
		pc.Close()
		return nil, "", fmt.Errorf("not a UDP socket: %T", pc)
	}
	return uc, uc.LocalAddr().String(), nil
}
