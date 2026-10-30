//go:build darwin && cgo

package vmnet

/*
#cgo LDFLAGS: -framework vmnet
#include <stdlib.h>
#include "shim_darwin.h"
*/
import "C"

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime/cgo"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const Supported = true

type Interface struct {
	cfg    Config
	handle cgo.Handle

	mu   sync.Mutex
	shim *C.fev_shim
	last Stats
	tun  *os.File
	mdns *net.UnixConn

	name      string
	ourMAC    net.HardwareAddr
	bridgeMAC net.HardwareAddr
	mtu       int
	maxPacket int

	stopped     chan struct{}
	stoppedOnce sync.Once
	closeOnce   sync.Once
	serving     sync.WaitGroup
}

func Start(cfg Config) (*Interface, error) { return start(cfg, -1) }

func start(cfg Config, testFD int) (*Interface, error) {
	closeTest := func() {
		if testFD >= 0 {
			syscall.Close(testFD)
		}
	}
	if err := cfg.check(); err != nil {
		closeTest()
		return nil, err
	}
	tun, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		closeTest()
		return nil, err
	}
	mdns, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		closeTest()
		syscall.Close(tun[0])
		syscall.Close(tun[1])
		return nil, err
	}
	i := &Interface{cfg: cfg, stopped: make(chan struct{})}
	i.handle = cgo.NewHandle(i)
	var c C.fev_shim_config
	macText := C.CString(cfg.Mac.String())
	maskText := C.CString(net.IP(net.CIDRMask(cfg.Base.Bits(), 32)).String())
	defer C.free(unsafe.Pointer(macText))
	defer C.free(unsafe.Pointer(maskText))
	c.mac_ip_text, c.mask_text = macText, maskText
	m4, p4 := cfg.Mac.As4(), cfg.Peer.As4()
	for k := range 4 {
		c.mac_ip[k], c.peer_ip[k] = C.uint8_t(m4[k]), C.uint8_t(p4[k])
	}
	c.mtu = C.uint32_t(cfg.mtu())
	if cfg.TSO {
		c.tso = 1
	}
	c.test_fd = C.int(testFD)
	c.tun_fd, c.mdns_fd = C.int(tun[1]), C.int(mdns[1])
	c.handle = C.uintptr_t(i.handle)
	var macOut [32]C.char
	var errOut [256]C.char
	var mtu C.uint32_t
	var maxPkt C.uint64_t
	var s *C.fev_shim
	if C.fev_shim_start(&c, &s, &macOut[0], C.size_t(len(macOut)), &mtu, &maxPkt, &errOut[0], C.size_t(len(errOut))) != 0 {
		i.handle.Delete()
		for _, fd := range []int{tun[0], tun[1], mdns[0], mdns[1]} {
			syscall.Close(fd)
		}
		closeTest()
		return nil, fmt.Errorf("vmnet: %s", C.GoString(&errOut[0]))
	}
	i.shim = s
	i.mtu = int(mtu)
	i.maxPacket = int(maxPkt)
	i.ourMAC, _ = net.ParseMAC(C.GoString(&macOut[0]))
	for _, fd := range []int{tun[0], mdns[0]} {
		for _, opt := range []int{syscall.SO_RCVBUF, syscall.SO_SNDBUF} {
			syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, opt, 1<<20)
		}
		syscall.CloseOnExec(fd)
		syscall.SetNonblock(fd, true)
	}
	i.tun = os.NewFile(uintptr(tun[0]), "vmnet-tunnel")
	mf := os.NewFile(uintptr(mdns[0]), "vmnet-mdns")
	conn, err := net.FileConn(mf)
	mf.Close()
	if err != nil {
		i.Close()
		return nil, fmt.Errorf("vmnet: the answerer's socket: %w", err)
	}
	i.mdns = conn.(*net.UnixConn)
	if testFD < 0 {
		i.name, i.bridgeMAC = findBridge(cfg.Mac)
		if len(i.bridgeMAC) == 6 {
			C.fev_shim_set_mac_mac(s, (*C.uint8_t)(unsafe.Pointer(&i.bridgeMAC[0])))
		}
	} else {
		i.name = "test"
	}
	i.SetPrefixes(cfg.Prefixes)
	return i, nil
}

func findBridge(addr netip.Addr) (string, net.HardwareAddr) {
	wait := 20 * time.Millisecond
	for deadline := time.Now().Add(10 * time.Second); ; {
		if name, mac := bridgeWith(addr); name != "" {
			return name, mac
		}
		if time.Now().After(deadline) {
			return "", nil
		}
		time.Sleep(wait)
		wait = min(2*wait, 500*time.Millisecond)
	}
}

func bridgeWith(addr netip.Addr) (string, net.HardwareAddr) {
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if !strings.HasPrefix(ifc.Name, "bridge") {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap() == addr {
					return ifc.Name, ifc.HardwareAddr
				}
			}
		}
	}
	return "", nil
}

func Bridges() string {
	var out []string
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if !strings.HasPrefix(ifc.Name, "bridge") {
			continue
		}
		var as []string
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				as = append(as, n.String())
			}
		}
		out = append(out, ifc.Name+" "+strings.Join(as, ","))
	}
	return strings.Join(out, "; ")
}

func (i *Interface) Name() string { return i.name }

func (i *Interface) MTU() int { return i.mtu }

func (i *Interface) PacketMTU() int { return min(i.maxPacket-14, 65535) }

func (i *Interface) MAC() net.HardwareAddr { return i.ourMAC }

func (i *Interface) Tunnel() *os.File {
	i.mu.Lock()
	defer i.mu.Unlock()
	f := i.tun
	i.tun = nil
	return f
}

func (i *Interface) Stopped() <-chan struct{} { return i.stopped }

func (i *Interface) SetPrefixes(ps []netip.Prefix) {
	addrs, bits := prefixBytes(ps)
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.shim == nil {
		return
	}
	if len(bits) == 0 {
		C.fev_shim_set_prefixes(i.shim, nil, nil, 0)
		return
	}
	C.fev_shim_set_prefixes(i.shim, (*C.uint8_t)(unsafe.Pointer(&addrs[0])), (*C.uint8_t)(unsafe.Pointer(&bits[0])), C.int(len(bits)))
}

func (i *Interface) SetZones(zones []string) {
	b := zoneBytes(zones)
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.shim == nil {
		return
	}
	C.fev_shim_set_zones(i.shim, (*C.char)(unsafe.Pointer(&b[0])), C.size_t(len(b)))
}

func (i *Interface) Stats() Stats {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.shim == nil {
		return i.last
	}
	var s C.fev_shim_stats
	C.fev_shim_stats_read(i.shim, &s)
	return statsOf(&s)
}

func (i *Interface) Close() error {
	i.closeOnce.Do(func() {
		i.mu.Lock()
		s := i.shim
		if s != nil {
			var st C.fev_shim_stats
			C.fev_shim_stats_read(s, &st)
			i.last = statsOf(&st)
		}
		i.shim = nil
		tun := i.tun
		i.tun = nil
		i.mu.Unlock()
		if s != nil {
			C.fev_shim_stop(s)
		}
		if tun != nil {
			tun.Close()
		}
		if i.mdns != nil {
			i.mdns.Close()
		}
		i.serving.Wait()
		i.handle.Delete()
	})
	return nil
}

func statsOf(s *C.fev_shim_stats) Stats {
	return Stats{
		FramesIn: uint64(s.frames_in), FramesOut: uint64(s.frames_out), Wakeups: uint64(s.wakeups),
		ToTunnel: uint64(s.to_tunnel), FromTunnel: uint64(s.from_tunnel),
		ARPAnswered: uint64(s.arp_answered), ARPIgnored: uint64(s.arp_ignored),
		MDNSUp: uint64(s.mdns_up), MDNSIgnored: uint64(s.mdns_ignored), MDNSDown: uint64(s.mdns_down),
		DroppedIPv6: uint64(s.dropped_ipv6), DroppedOther: uint64(s.dropped_other), DroppedNoMac: uint64(s.dropped_no_mac),
		WriteFailed: uint64(s.write_failed), Malformed: uint64(s.malformed),
	}
}

//export fevVmnetStopped
func fevVmnetStopped(h C.uintptr_t) {
	if i, ok := cgo.Handle(h).Value().(*Interface); ok {
		i.stoppedOnce.Do(func() { close(i.stopped) })
	}
}

func (i *Interface) ServeMDNS(a *Answerer) {
	i.serving.Add(1)
	go func() {
		defer i.serving.Done()
		buf := make([]byte, 9000)
		for {
			n, err := i.mdns.Read(buf)
			if err != nil {
				return
			}
			from, msg, ok := upFrame(buf[:n])
			if !ok {
				continue
			}
			if out, to, ok := a.Respond(msg, from); ok {
				i.mdns.Write(downFrame(to, out))
			}
		}
	}()
}
