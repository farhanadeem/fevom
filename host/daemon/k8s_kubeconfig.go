package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"

	"fev/host/k8s/kubeconfig"
)

const k8sGuestAPIPort = 6443

func (k *K8s) wantedAPIPort() int {
	if k.APIPort != 0 {
		return k.APIPort
	}
	return k8sGuestAPIPort
}

func (k *K8s) macAPIPort() uint16 {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.api == "" {
		return 0
	}
	ap, err := netip.ParseAddrPort(k.api)
	if err != nil {
		return 0
	}
	return ap.Port()
}

func (k *K8s) chooseAPI() (addr, note string) {
	want := k.wantedAPIPort()
	if apiPortFree(want) {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(want)), ""
	}
	for i := 0; i < 8; i++ {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			break
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		if apiPortFree(port) {
			return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), fmt.Sprintf("Port %d is in use on this Mac, so the API is at 127.0.0.1:%d, which the fev context uses", want, port)
		}
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(want)), fmt.Sprintf("Port %d is in use on this Mac and no free port was found", want)
}

func apiPortFree(port int) bool {
	for _, a := range []struct{ net, lo, any string }{{"tcp4", "127.0.0.1", "0.0.0.0"}, {"tcp6", "::1", "::"}} {
		err := bindOnce(a.net, net.JoinHostPort(a.lo, strconv.Itoa(port)), false)
		if err == nil {
			continue
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			continue
		}
		if bindOnce(a.net, net.JoinHostPort(a.lo, strconv.Itoa(port)), true) != nil {
			return false
		}
		if bindOnce(a.net, net.JoinHostPort(a.any, strconv.Itoa(port)), true) != nil {
			return false
		}
	}
	return true
}

func bindOnce(network, addr string, reuse bool) error {
	v := 0
	if reuse {
		v = 1
	}
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, v)
		}); err != nil {
			return err
		}
		return serr
	}}
	l, err := lc.Listen(context.Background(), network, addr)
	if err != nil {
		return err
	}
	return l.Close()
}

func (k *K8s) mergeContext(raw, api string) {
	if k.Kubeconfig == "" {
		return
	}
	m, err := kubeconfig.Merge(k.Kubeconfig, k.KubeconfigNote, raw, api)
	k.mu.Lock()
	k.kcErr = ""
	if err != nil {
		k.kcErr = "the fev context could not be written to " + k.Kubeconfig + ": " + err.Error()
	}
	k.mu.Unlock()
	switch {
	case err != nil:
		k.log.Warn("k8s: the fev context", "file", k.Kubeconfig, "err", err)
	case m.Added:
		k.log.Info("k8s: added context fev", "file", m.Path, "server", api, "current", m.Current)
	}
}

func (k *K8s) removeContext(why string) {
	if k.Kubeconfig == "" && k.KubeconfigNote == "" {
		return
	}
	r, err := kubeconfig.Remove(k.KubeconfigNote, k.Kubeconfig)
	k.mu.Lock()
	k.kcErr = ""
	if err != nil {
		k.kcErr = "the fev context could not be removed from " + r.Path + ": " + err.Error()
	}
	k.mu.Unlock()
	switch {
	case err != nil:
		k.log.Warn("k8s: the fev context's removal", "file", r.Path, "why", why, "err", err)
	case !r.Nothing:
		k.log.Info("k8s: removed context fev", "file", r.Path, "why", why, "put_back", r.Restored)
	}
}
