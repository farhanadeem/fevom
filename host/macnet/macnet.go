package macnet

import (
	"log/slog"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
)

type Route struct {
	Prefix            netip.Prefix
	Gateway           netip.Addr
	Interface         string
	Scoped            bool
	Reject, Blackhole bool
}

type Held struct {
	Prefix    netip.Prefix
	Interface string
}

func (s Snapshot) WithoutHeld(held []Held) Snapshot {
	if len(held) == 0 {
		return s
	}
	out := s
	out.Routes = slices.DeleteFunc(slices.Clone(s.Routes), func(r Route) bool {
		for _, h := range held {
			if r.Prefix == h.Prefix.Masked() && r.Interface == h.Interface && r.Reject && !r.Blackhole && !r.Scoped && !r.Gateway.IsValid() {
				return true
			}
		}
		return false
	})
	return out
}

type Network struct {
	Prefix    netip.Prefix
	Interface string
}

type Address struct {
	Addr      netip.Addr
	Interface string
}

type Snapshot struct {
	Gen        uint64
	Cause      []string
	Routes     []Route
	Networks   []Network
	Addresses  []Address
	GlobalIPv6 bool
	NAT64      []netip.Prefix
}

func (s Snapshot) HasAddress(a netip.Addr) bool {
	a = a.Unmap()
	for _, x := range s.Addresses {
		if x.Addr == a {
			return true
		}
	}
	return false
}

func (s Snapshot) MacAddresses() []netip.Addr {
	var out []netip.Addr
	for _, x := range s.Addresses {
		a := x.Addr
		if a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
			continue
		}
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b netip.Addr) int { return a.Compare(b) })
	return slices.Compact(out)
}

func (s Snapshot) SameNetwork(o Snapshot) bool { return s.equal(o) }

func (s Snapshot) equal(o Snapshot) bool {
	return s.GlobalIPv6 == o.GlobalIPv6 && slices.Equal(s.Routes, o.Routes) &&
		slices.Equal(s.Networks, o.Networks) && slices.Equal(s.Addresses, o.Addresses) && slices.Equal(s.NAT64, o.NAT64)
}

type Watcher struct {
	log  *slog.Logger
	read func() (Snapshot, error)

	mu  sync.Mutex
	gen uint64
	cur atomic.Pointer[Snapshot]

	subsMu sync.Mutex
	subs   map[int]func(Snapshot)
	next   int

	closeOnce sync.Once
	unwatch   func()
}

func newWatcher(log *slog.Logger, read func() (Snapshot, error)) *Watcher {
	if log == nil {
		log = slog.Default()
	}
	w := &Watcher{log: log, read: read, subs: map[int]func(Snapshot){}}
	w.update([]string{"start"}, true)
	return w
}

func NewWithReader(log *slog.Logger, read func() (Snapshot, error)) *Watcher {
	return newWatcher(log, read)
}

func (w *Watcher) Notify(keys ...string) { w.changed(keys) }

func (w *Watcher) Current() Snapshot {
	if s := w.cur.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

func (w *Watcher) Subscribe(fn func(Snapshot)) (cancel func()) {
	w.subsMu.Lock()
	id := w.next
	w.next++
	w.subs[id] = fn
	w.subsMu.Unlock()
	return func() {
		w.subsMu.Lock()
		delete(w.subs, id)
		w.subsMu.Unlock()
	}
}

func (w *Watcher) Refresh(reason string) Snapshot {
	return w.update([]string{"refresh: " + reason}, false)
}

func (w *Watcher) changed(keys []string) {
	w.update(keys, true)
}

func (w *Watcher) update(cause []string, always bool) Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, err := w.read()
	if err != nil {
		w.log.Warn("cannot read the Mac's network", "cause", cause, "err", err)
		return w.Current()
	}
	if prev := w.cur.Load(); prev != nil && !always && prev.equal(s) {
		return *prev
	}
	w.gen++
	s.Gen = w.gen
	s.Cause = cause
	w.cur.Store(&s)
	w.subsMu.Lock()
	fns := make([]func(Snapshot), 0, len(w.subs))
	for _, fn := range w.subs {
		fns = append(fns, fn)
	}
	w.subsMu.Unlock()
	for _, fn := range fns {
		fn(s)
	}
	return s
}

func (w *Watcher) Close() {
	w.closeOnce.Do(func() {
		if w.unwatch != nil {
			w.unwatch()
		}
		w.subsMu.Lock()
		w.subs = map[int]func(Snapshot){}
		w.subsMu.Unlock()
	})
}
