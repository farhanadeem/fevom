package forward

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"reflect"
	"sort"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"fev/host/netstack"
	api "fev/proto/daemon"
	"fev/proto/relay"
)

type Opener interface {
	Open(ctx context.Context, req relay.Request) (*net.UnixConn, error)
}

const (
	TCP = "tcp"
	UDP = "udp"
)

type Want struct {
	Proto string
	Port  uint16
	Guest netip.AddrPort
	LAN   bool
}

type key struct {
	proto string
	port  uint16
}

const PrivilegedBelow = 1024

const reuseWindow = 30 * time.Second

const maxRows = 1 << 18

type row struct {
	id, source, owner string
	key               key
	guest             netip.AddrPort
	state, err        string
	lan, wide         bool
	mac               []string
	closers           []func()
	held              int
	table             *Table
	conns             map[net.Conn]struct{}
	closed            bool
}

type Table struct {
	log                *slog.Logger
	notify             func(f api.Forward, removed bool)
	unreplied, assured time.Duration
	now                func() time.Time

	mu                      sync.Mutex
	opener                  Opener
	lan                     bool
	rows                    map[key][]*row
	count                   int
	full                    bool
	released                map[key]time.Time
	wg                      sync.WaitGroup
	listeners, maxListeners int
	low                     LowBinder
}

const ephemeralFrom = 49152

func (t *Table) SetListenerCap(n int) {
	t.mu.Lock()
	t.maxListeners = n
	t.mu.Unlock()
}

func New(log *slog.Logger, notify func(f api.Forward, removed bool)) *Table {
	if log == nil {
		log = slog.Default()
	}
	if notify == nil {
		notify = func(api.Forward, bool) {}
	}
	return &Table{
		log: log, notify: notify, now: time.Now, lan: true,
		unreplied: netstack.UDPUnreplied, assured: netstack.UDPAssured,
		rows: map[key][]*row{}, released: map[key]time.Time{},
	}
}

func (t *Table) SetOpener(o Opener) {
	t.mu.Lock()
	t.opener = o
	t.mu.Unlock()
}

func (t *Table) SetLAN(on bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lan == on {
		return
	}
	t.lan = on
	t.forget()
	keys := make([]key, 0, len(t.rows))
	for k := range t.rows {
		keys = append(keys, k)
	}
	sortKeys(keys)
	for _, k := range keys {
		for _, r := range append([]*row{}, t.rows[k]...) {
			if r.lan && r.wide != on {
				t.rebind(r)
			}
		}
	}
}

func sortKeys(keys []key) {
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].port < keys[j].port || (keys[i].port == keys[j].port && keys[i].proto < keys[j].proto)
	})
}

func (t *Table) rebind(r *row) {
	if r.state == api.ForwardActive {
		t.released[r.key] = t.now()
	}
	t.claim(r)
}

func rowID(source, owner string, k key) string {
	if len(owner) > 12 {
		owner = owner[:12]
	}
	return fmt.Sprintf("%s/%s/%s/%d", source, owner, k.proto, k.port)
}

func (r *row) forward() api.Forward {
	return api.Forward{
		ID: r.id, Source: r.source, Owner: r.owner, Proto: r.key.proto,
		Mac: append([]string{}, r.mac...), Guest: r.guest.String(), State: r.state, Error: r.err,
	}
}

func (t *Table) Set(source, owner string, wants []Want) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.forget()
	wanted := map[key]Want{}
	for _, w := range wants {
		if (w.Proto != TCP && w.Proto != UDP) || w.Port == 0 || !w.Guest.IsValid() || w.Guest.Port() == 0 {
			continue
		}
		k := key{w.Proto, w.Port}
		first, twice := wanted[k]
		if !twice {
			wanted[k] = w
			continue
		}
		first.LAN = first.LAN || w.LAN
		wanted[k] = first
	}
	for _, r := range t.owned(source, owner) {
		if _, still := wanted[r.key]; !still {
			t.remove(r)
		}
	}
	keys := make([]key, 0, len(wanted))
	for k := range wanted {
		keys = append(keys, k)
	}
	sortKeys(keys)
	for _, k := range keys {
		w := wanted[k]
		var mine *row
		for _, r := range t.rows[k] {
			if r.source == source && r.owner == owner {
				mine = r
			}
		}
		switch {
		case mine == nil && t.count >= maxRows:
			if !t.full {
				t.full = true
				t.log.Error("port forward: the table is full; further forwards are not made", "rows", t.count)
			}
		case mine == nil:
			t.count++
			mine = &row{id: rowID(source, owner, k), source: source, owner: owner, key: k, guest: w.Guest, lan: w.LAN, conns: map[net.Conn]struct{}{}, table: t}
			t.rows[k] = append(t.rows[k], mine)
			t.claim(mine)
		case mine.state != api.ForwardActive:
			mine.guest, mine.lan = w.Guest, w.LAN
			t.claim(mine)
		case mine.lan != w.LAN:
			moved := mine.guest != w.Guest
			mine.guest, mine.lan = w.Guest, w.LAN
			if (mine.lan && t.lan) != mine.wide {
				t.rebind(mine)
			} else if moved {
				t.notify(mine.forward(), false)
			}
		case mine.guest != w.Guest:
			mine.guest = w.Guest
			t.notify(mine.forward(), false)
		}
	}
}

func (t *Table) owned(source, owner string) []*row {
	var out []*row
	for _, rows := range t.rows {
		for _, r := range rows {
			if r.source == source && (owner == "" || r.owner == owner) {
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func (t *Table) Owners(source string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, r := range t.owned(source, "") {
		if !seen[r.owner] {
			seen[r.owner] = true
			out = append(out, r.owner)
		}
	}
	return out
}

func (t *Table) Drop(source string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range t.owned(source, "") {
		t.remove(r)
	}
}

func (t *Table) List() []api.Forward {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []api.Forward{}
	for _, rows := range t.rows {
		for _, r := range rows {
			out = append(out, r.forward())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

var ErrNoForward = errors.New("no such forward")

func (t *Table) Retry(id string) ([]api.Forward, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var tried []*row
	found := false
	for _, rows := range t.rows {
		for _, r := range rows {
			if id != "" && r.id != id {
				continue
			}
			found = true
			if r.state == api.ForwardConflict || id != "" {
				tried = append(tried, r)
			}
		}
	}
	if id != "" && !found {
		return nil, fmt.Errorf("%w: %s", ErrNoForward, id)
	}
	sort.Slice(tried, func(i, j int) bool { return tried[i].id < tried[j].id })
	out := []api.Forward{}
	for _, r := range tried {
		if r.state == api.ForwardConflict {
			t.claim(r)
		}
		out = append(out, r.forward())
	}
	return out, nil
}

func (t *Table) Close() {
	t.mu.Lock()
	for _, rows := range t.rows {
		for _, r := range append([]*row{}, rows...) {
			t.remove(r)
		}
	}
	t.opener = nil
	t.mu.Unlock()
	t.wg.Wait()
}

func (t *Table) claim(r *row) {
	before := r.forward()
	r.wide = r.lan && t.lan
	rows := t.rows[r.key]
	if rows[0] != r {
		holder := rows[0]
		r.release()
		r.state = api.ForwardConflict
		r.mac, _ = addrsFor(r.key, r.wide)
		r.err = fmt.Sprintf("port %d/%s on the Mac is already forwarded for %s %s; this forward takes over when that one ends",
			r.key.port, r.key.proto, holder.source, short(holder.owner))
		if !reflect.DeepEqual(before, r.forward()) {
			t.log.Warn("port forward: two owners want one Mac port", "id", r.id, "port", r.key.port, "proto", r.key.proto, "has_it", holder.id)
			t.notify(r.forward(), false)
		}
		return
	}
	t.bind(r)
	if reflect.DeepEqual(before, r.forward()) {
		return
	}
	switch r.state {
	case api.ForwardActive:
		t.log.Info("port forward: listening", "id", r.id, "mac", r.mac, "guest", r.guest)
	case api.ForwardConflict:
		t.log.Warn("port forward: the Mac port is taken", "id", r.id, "port", r.key.port, "proto", r.key.proto, "err", r.err)
	default:
		t.log.Error("port forward: cannot listen", "id", r.id, "err", r.err)
	}
	t.notify(r.forward(), false)
}

func short(owner string) string {
	if len(owner) > 12 {
		return owner[:12]
	}
	return owner
}

func (t *Table) remove(r *row) {
	rows := t.rows[r.key]
	for i, other := range rows {
		if other == r {
			rows = append(rows[:i:i], rows[i+1:]...)
			break
		}
	}
	if len(rows) == 0 {
		delete(t.rows, r.key)
	} else {
		t.rows[r.key] = rows
	}
	t.count--
	t.full = false
	if r.state == api.ForwardActive {
		t.released[r.key] = t.now()
	}
	r.release()
	t.log.Info("port forward: removed", "id", r.id)
	t.notify(r.forward(), true)
	if len(rows) > 0 && rows[0].state != api.ForwardActive {
		t.claim(rows[0])
	}
}

func (t *Table) forget() {
	for k, at := range t.released {
		if t.now().Sub(at) > reuseWindow {
			delete(t.released, k)
		}
	}
}

func (r *row) release() {
	r.closed = true
	if r.table != nil {
		r.table.listeners -= r.held
	}
	r.held = 0
	for _, closer := range r.closers {
		closer()
	}
	r.closers = nil
	for c := range r.conns {
		c.Close()
	}
	r.conns = map[net.Conn]struct{}{}
}

var loopback4 = netip.AddrFrom4([4]byte{127, 0, 0, 1})

func addrsFor(k key, wide bool) (addrs []string, localOnly bool) {
	if wide || k.port < PrivilegedBelow {
		return []string{netip.AddrPortFrom(netip.IPv4Unspecified(), k.port).String(), netip.AddrPortFrom(netip.IPv6Unspecified(), k.port).String()}, !wide
	}
	return []string{netip.AddrPortFrom(loopback4, k.port).String(), netip.AddrPortFrom(netip.IPv6Loopback(), k.port).String()}, false
}

func noReuse(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 0)
	}); err != nil {
		return err
	}
	return serr
}

func withReuse(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return serr
}

func listen(proto, addr string, reuse bool) (any, error) {
	lc := net.ListenConfig{Control: noReuse}
	if reuse {
		lc.Control = withReuse
	}
	ap := netip.MustParseAddrPort(addr)
	family := "4"
	if ap.Addr().Is6() {
		family = "6"
	}
	if proto == TCP {
		return lc.Listen(context.Background(), TCP+family, addr)
	}
	return lc.ListenPacket(context.Background(), UDP+family, addr)
}

func loopbackOf(wildcard string) string {
	ap := netip.MustParseAddrPort(wildcard)
	lo := loopback4
	if ap.Addr().Is6() {
		lo = netip.IPv6Loopback()
	}
	return netip.AddrPortFrom(lo, ap.Port()).String()
}

func listenAgain(proto, addr string) (any, error) {
	if netip.MustParseAddrPort(addr).Addr().IsUnspecified() {
		probe, err := listen(proto, loopbackOf(addr), true)
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, err
		}
		if err == nil {
			probe.(interface{ Close() error }).Close()
		}
	}
	return listen(proto, addr, true)
}

func listenBeside(addr string) (any, error) {
	ap := netip.MustParseAddrPort(addr)
	probes := []netip.Addr{netip.MustParseAddrPort(loopbackOf(addr)).Addr()}
	if ifaddrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range ifaddrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipnet.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			if ip.Is4() != ap.Addr().Is4() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			probes = append(probes, ip)
		}
	}
	for _, ip := range probes {
		probe, err := listen(TCP, netip.AddrPortFrom(ip, ap.Port()).String(), true)
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, err
		}
		if err == nil {
			probe.(interface{ Close() error }).Close()
		}
	}
	return listen(TCP, addr, true)
}

func (t *Table) bind(r *row) {
	r.release()
	r.closed = false
	r.mac, r.err = nil, ""
	last, was := t.released[r.key]
	ours := r.key.proto == TCP && was && t.now().Sub(last) <= reuseWindow
	var made []any
	addrs, local := addrsFor(r.key, r.wide)
	fail := func(state string, err error) {
		for _, l := range made {
			l.(interface{ Close() error }).Close()
		}
		r.state, r.err, r.mac = state, err.Error(), addrs
	}
	need := len(addrs)
	if r.wide {
		need *= 2
	}
	if t.maxListeners > 0 && t.listeners+need > t.maxListeners {
		fail(api.ForwardError, fmt.Errorf("too many published ports: fev holds %d listening sockets of the %d it allows itself, and this port is not forwarded; it is tried again when its container next starts",
			t.listeners, t.maxListeners))
		return
	}
	high := r.wide && r.key.port >= ephemeralFrom
	if !r.wide && r.key.port < PrivilegedBelow && t.low != nil {
		if lm, la, ok := t.bindLow(r); ok {
			made, r.mac, local, addrs = lm, la, false, nil
		}
	}
	inUse := func(addr string) error {
		also := ""
		if high && r.key.proto == UDP {
			also = "; nothing may be listening there: from 49152 up a connection from this Mac may be using it as its source port"
		}
		return fmt.Errorf("port %d/%s on the Mac is in use by another program (%s)%s", r.key.port, r.key.proto, addr, also)
	}
	for _, addr := range addrs {
		l, err := listen(r.key.proto, addr, false)
		if errors.Is(err, syscall.EADDRINUSE) && ours {
			l, err = listenAgain(r.key.proto, addr)
		} else if errors.Is(err, syscall.EADDRINUSE) && high && r.key.proto == TCP {
			l, err = listenBeside(addr)
		}
		switch {
		case err == nil:
			made = append(made, l)
			r.mac = append(r.mac, addr)
		case errors.Is(err, syscall.EADDRINUSE):
			fail(api.ForwardConflict, inUse(addr))
			return
		case errors.Is(err, syscall.EADDRNOTAVAIL), errors.Is(err, syscall.EAFNOSUPPORT):
			t.log.Debug("port forward: address not available", "addr", addr, "err", err)
		default:
			fail(api.ForwardError, err)
			return
		}
	}
	if len(made) == 0 {
		fail(api.ForwardError, fmt.Errorf("no address on the Mac could be listened on for port %d", r.key.port))
		return
	}
	if r.wide {
		for _, addr := range r.mac {
			lo := loopbackOf(addr)
			l, err := listen(r.key.proto, lo, true)
			switch {
			case err == nil:
				made = append(made, l)
			case errors.Is(err, syscall.EADDRINUSE):
				fail(api.ForwardConflict, inUse(lo))
				return
			case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EADDRNOTAVAIL):
				t.log.Debug("port forward: loopback address not held", "addr", lo, "err", err)
			default:
				fail(api.ForwardError, err)
				return
			}
		}
	}
	r.state = api.ForwardActive
	r.held = len(made)
	t.listeners += r.held
	for _, l := range made {
		switch l := l.(type) {
		case *net.TCPListener:
			r.closers = append(r.closers, func() { l.Close() })
			t.wg.Add(1)
			go t.acceptTCP(r, l, local)
		case *net.UDPConn:
			u := &udpListener{t: t, r: r, pc: l, localOnly: local, sessions: map[flow]*udpSession{}}
			if at := l.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap(); at.IsUnspecified() {
				if err := tellArrival(l, at.Is6()); err != nil {
					t.log.Warn("port forward: cannot learn where datagrams arrive; answers leave from the address the Mac routes by", "id", r.id, "err", err)
				} else {
					u.wildcard = true
				}
			}
			r.closers = append(r.closers, func() { l.Close() })
			t.wg.Add(1)
			go u.serve()
		}
	}
}

func (t *Table) acceptTCP(r *row, l *net.TCPListener, localOnly bool) {
	defer t.wg.Done()
	var delay time.Duration
	for {
		conn, err := l.AcceptTCP()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			t.log.Warn("port forward: accept failed", "id", r.id, "err", err, "retry_in", delay)
			time.Sleep(delay)
			continue
		}
		delay = 0
		if localOnly && !fromThisMac(conn.RemoteAddr()) {
			conn.Close()
			continue
		}
		t.mu.Lock()
		if r.closed {
			t.mu.Unlock()
			conn.Close()
			continue
		}
		r.conns[conn] = struct{}{}
		opener, guest := t.opener, r.guest
		t.mu.Unlock()
		t.wg.Add(1)
		go t.carry(r, conn, opener, guest)
	}
}

func fromThisMac(addr net.Addr) bool {
	ap, err := netip.ParseAddrPort(addr.String())
	return err == nil && ap.Addr().Unmap().IsLoopback()
}

const openTimeout = 30 * time.Second

func (t *Table) carry(r *row, conn *net.TCPConn, opener Opener, guest netip.AddrPort) {
	defer t.wg.Done()
	forget := func(c net.Conn) {
		t.mu.Lock()
		delete(r.conns, c)
		t.mu.Unlock()
	}
	defer forget(conn)
	var stream *net.UnixConn
	err := errors.New("there is no guest")
	if opener != nil {
		ctx, cancel := context.WithTimeout(context.Background(), openTimeout)
		stream, err = opener.Open(ctx, relay.Request{Kind: relay.TCP, Target: guest.String()})
		cancel()
	}
	if err != nil {
		t.log.Debug("port forward: the guest did not take the connection", "id", r.id, "guest", guest, "err", err)
		conn.SetLinger(0)
		conn.Close()
		return
	}
	t.mu.Lock()
	if r.closed {
		t.mu.Unlock()
		stream.Close()
		conn.Close()
		return
	}
	r.conns[stream] = struct{}{}
	t.mu.Unlock()
	defer forget(stream)
	netstack.Splice(conn, stream)
	conn.Close()
	stream.Close()
}
