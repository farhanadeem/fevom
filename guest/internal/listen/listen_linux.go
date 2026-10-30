package listen

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"golang.org/x/sys/unix"
)

const RingBytes = 256 << 10

const maxBound = 1 << 16

type Watcher struct {
	ring, bound, lost *ebpf.Map
	progs             []*ebpf.Program
	links             []link.Link
	rd                *ringbuf.Reader

	attaches []ebpf.AttachType

	mu   sync.Mutex
	open map[uint64]Listener
}

func Attach(cgroup string) (_ *Watcher, err error) {
	w := &Watcher{open: map[uint64]Listener{}}
	defer func() {
		if err != nil {
			w.Close()
		}
	}()
	if w.ring, err = ebpf.NewMap(&ebpf.MapSpec{Name: "listen_ring", Type: ebpf.RingBuf, MaxEntries: RingBytes}); err != nil {
		return nil, fmt.Errorf("the ring: %w", err)
	}
	if w.bound, err = ebpf.NewMap(&ebpf.MapSpec{
		Name: "listen_bound", Type: ebpf.Hash, KeySize: 8, ValueSize: 1, MaxEntries: maxBound, Flags: unix.BPF_F_NO_PREALLOC,
	}); err != nil {
		return nil, fmt.Errorf("the bound sockets' map: %w", err)
	}
	if w.lost, err = ebpf.NewMap(&ebpf.MapSpec{Name: "listen_lost", Type: ebpf.Array, KeySize: 4, ValueSize: 8, MaxEntries: 1}); err != nil {
		return nil, fmt.Errorf("the lost records' count: %w", err)
	}
	progs := []struct {
		name   string
		typ    ebpf.ProgramType
		attach ebpf.AttachType
	}{
		{"listen_ask4", ebpf.CGroupSockAddr, ebpf.AttachCGroupInet4Bind},
		{"listen_ask6", ebpf.CGroupSockAddr, ebpf.AttachCGroupInet6Bind},
		{"listen_ops", ebpf.SockOps, ebpf.AttachCGroupSockOps},
		{"listen_bind4", ebpf.CGroupSock, ebpf.AttachCGroupInet4PostBind},
		{"listen_bind6", ebpf.CGroupSock, ebpf.AttachCGroupInet6PostBind},
		{"listen_release", ebpf.CGroupSock, ebpf.AttachCgroupInetSockRelease},
	}
	for _, p := range progs {
		ring, bound, lost := w.ring.FD(), w.bound.FD(), w.lost.FD()
		in := sockOps(ring, lost)
		switch p.attach {
		case ebpf.AttachCGroupInet4Bind, ebpf.AttachCGroupInet6Bind:
			in = bindHook(bound)
		case ebpf.AttachCGroupInet4PostBind, ebpf.AttachCGroupInet6PostBind:
			in = postBind(ring, bound, lost, p.attach == ebpf.AttachCGroupInet6PostBind)
		case ebpf.AttachCgroupInetSockRelease:
			in = release(ring, bound, lost)
		}
		prog, err := load(p.name, p.typ, p.attach, in)
		if err != nil {
			return nil, err
		}
		w.progs = append(w.progs, prog)
		w.attaches = append(w.attaches, p.attach)
		l, err := link.AttachCgroup(link.CgroupOptions{Path: cgroup, Attach: p.attach, Program: prog})
		if err != nil {
			return nil, fmt.Errorf("attach %s at %s: %w", p.name, cgroup, err)
		}
		w.links = append(w.links, l)
	}
	if w.rd, err = ringbuf.NewReader(w.ring); err != nil {
		return nil, fmt.Errorf("the ring's reader: %w", err)
	}
	return w, nil
}

func (w *Watcher) AttachAt(cgroup string) (detach func(), err error) {
	var links []link.Link
	detach = func() {
		for _, l := range links {
			l.Close()
		}
	}
	for i, prog := range w.progs {
		l, err := link.AttachCgroup(link.CgroupOptions{Path: cgroup, Attach: w.attaches[i], Program: prog})
		if err != nil {
			detach()
			return nil, fmt.Errorf("attach %v at %s: %w", w.attaches[i], cgroup, err)
		}
		links = append(links, l)
	}
	return detach, nil
}

func (w *Watcher) Close() {
	if w.rd != nil {
		w.rd.Close()
	}
	for _, l := range w.links {
		l.Close()
	}
	for _, p := range w.progs {
		p.Close()
	}
	for _, m := range []*ebpf.Map{w.ring, w.bound, w.lost} {
		if m != nil {
			m.Close()
		}
	}
}

func (w *Watcher) Run(fn func(Event)) error {
	var rec ringbuf.Record
	for {
		if err := w.rd.ReadInto(&rec); err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return nil
			}
			return err
		}
		ev, ok := w.apply(rec.RawSample)
		if ok && fn != nil {
			fn(ev)
		}
	}
}

func (w *Watcher) apply(b []byte) (Event, bool) {
	if len(b) < recordSize {
		return Event{}, false
	}
	kind := b[0]
	cookie := binary.NativeEndian.Uint64(b[8:])
	w.mu.Lock()
	defer w.mu.Unlock()
	switch kind {
	case kindListen, kindBound:
		l := Listener{
			Proto:  TCP,
			Port:   binary.NativeEndian.Uint16(b[4:]),
			NetNS:  binary.NativeEndian.Uint64(b[16:]),
			Cookie: cookie,
		}
		if kind == kindBound {
			l.Proto = UDP
		}
		switch binary.NativeEndian.Uint16(b[2:]) {
		case unix.AF_INET:
			l.Addr = netip.AddrFrom4([4]byte(b[24:28]))
		case unix.AF_INET6:
			l.Addr, l.V6 = netip.AddrFrom16([16]byte(b[24:40])).Unmap(), true
		default:
			return Event{}, false
		}
		w.open[cookie] = l
		return Event{Listener: l, Added: true}, true
	case kindClosed, kindReleased:
		l, ok := w.open[cookie]
		if !ok {
			return Event{}, false
		}
		delete(w.open, cookie)
		return Event{Listener: l}, true
	}
	return Event{}, false
}

func (w *Watcher) Lost() uint64 {
	var n uint64
	if w.lost == nil || w.lost.Lookup(uint32(0), &n) != nil {
		return 0
	}
	return n
}

func (w *Watcher) Listeners(keep func(netns uint64) bool) []Listener {
	w.mu.Lock()
	out := make([]Listener, 0, len(w.open))
	for _, l := range w.open {
		if keep == nil || keep(l.NetNS) {
			out = append(out, l)
		}
	}
	w.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Proto != b.Proto {
			return a.Proto < b.Proto
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		if c := a.Addr.Compare(b.Addr); c != 0 {
			return c < 0
		}
		return a.Cookie < b.Cookie
	})
	return out
}

func NetNSCookie(nsfd int) (uint64, error) {
	return netnsCookie(nsfd)
}

func load(name string, typ ebpf.ProgramType, attach ebpf.AttachType, in asm.Instructions) (*ebpf.Program, error) {
	p, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name: name, Type: typ, AttachType: attach, Instructions: in, License: license,
	})
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", name, err)
	}
	return p, nil
}
