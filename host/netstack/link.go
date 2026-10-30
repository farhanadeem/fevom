package netstack

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

const (
	netHdrLen  = 12
	lenPrefix  = 4
	maxMessage = netHdrLen + 65550
	ipMTU      = 1500

	controlLen          = 8
	controlGuestAccepts = 1
	acceptsPartialCsum  = 1
	acceptsTSO4         = 2
	acceptsTSO6         = 4

	hdrNeedsCsum = 1
	hdrGSOTCPv4  = 1
	hdrGSOTCPv6  = 4
	hdrGSOECN    = 0x80
	gsoMaxSize   = 65535
)

type linkEndpoint struct {
	conn *net.UnixConn
	addr tcpip.LinkAddress

	mu         sync.RWMutex
	dispatcher stack.NetworkDispatcher
	onClose    func()
	ended      func()

	region   *region
	mapped   bool
	received consumer

	writeMu sync.Mutex
	ringMu  sync.Mutex
	sent    producer
	closed  bool

	tunnel atomic.Pointer[Tunnel]

	data       chan struct{}
	room       chan struct{}
	socketDone chan struct{}
	socketErr  error

	done chan struct{}
	err  error

	offload   atomic.Bool
	offload6  atomic.Bool
	onAccepts func(flags uint32) error
	announced atomic.Bool
	onSend    func(frameLen int)
}

func newLinkEndpoint(conn *net.UnixConn, addr tcpip.LinkAddress) *linkEndpoint {
	return &linkEndpoint{
		conn: conn, addr: addr,
		data: make(chan struct{}, 1), room: make(chan struct{}, 1),
		socketDone: make(chan struct{}), done: make(chan struct{}),
	}
}

func (e *linkEndpoint) attach(r *region, receive, send int) {
	e.region = r
	e.received = r.consumer(receive)
	e.sent = r.producer(send)
}

func (e *linkEndpoint) start(onAccepts func(flags uint32) error) {
	e.onAccepts = onAccepts
	go e.run()
}

func (e *linkEndpoint) GSOMaxSize() uint32 { return gsoMaxSize }

func (e *linkEndpoint) SupportedGSO() stack.SupportedGSO {
	if e.offload.Load() {
		return stack.HostGSOSupported
	}
	return stack.GSONotSupported
}

func (e *linkEndpoint) MTU() uint32                      { return ipMTU + header.EthernetMinimumSize }
func (e *linkEndpoint) SetMTU(uint32)                    {}
func (e *linkEndpoint) MaxHeaderLength() uint16          { return 0 }
func (e *linkEndpoint) LinkAddress() tcpip.LinkAddress   { return e.addr }
func (e *linkEndpoint) SetLinkAddress(tcpip.LinkAddress) {}

func (e *linkEndpoint) Capabilities() stack.LinkEndpointCapabilities {
	return stack.CapabilityRXChecksumOffload
}
func (e *linkEndpoint) ARPHardwareType() header.ARPHardwareType { return header.ARPHardwareNone }
func (e *linkEndpoint) AddHeader(*stack.PacketBuffer)           {}
func (e *linkEndpoint) ParseHeader(*stack.PacketBuffer) bool    { return true }
func (e *linkEndpoint) Wait()                                   { <-e.done }

func (e *linkEndpoint) SetOnCloseAction(fn func()) {
	e.mu.Lock()
	e.onClose = fn
	e.mu.Unlock()
}

func (e *linkEndpoint) Close() {
	e.conn.Close()
}

func (e *linkEndpoint) IsAttached() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.dispatcher != nil
}

func (e *linkEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.mu.Lock()
	e.dispatcher = dispatcher
	e.mu.Unlock()
}

var wakeByte = []byte{1}

func (e *linkEndpoint) wakePeer() { e.conn.Write(wakeByte) }

func (e *linkEndpoint) waitForRoom(size int, wake *bool) bool {
	for {
		e.ringMu.Lock()
		if e.closed {
			e.ringMu.Unlock()
			return false
		}
		ok, err := e.sent.hasRoom(size)
		if err == nil && !ok {
			var full bool
			if full, err = e.sent.waitForRoom(size); err == nil && !full {
				e.ringMu.Unlock()
				continue
			}
		}
		if err != nil {
			e.ringMu.Unlock()
			e.conn.Close()
			return false
		}
		if ok {
			return true
		}
		e.ringMu.Unlock()
		if *wake {
			*wake = false
			e.wakePeer()
		}
		select {
		case <-e.room:
		case <-e.socketDone:
			return false
		}
	}
}

func (e *linkEndpoint) tryWrite(hdr *[netHdrLen]byte, frame []byte) (ok, wake bool) {
	size := netHdrLen + len(frame)
	if size > maxMessage {
		return false, false
	}
	e.ringMu.Lock()
	if e.closed || e.region == nil {
		e.ringMu.Unlock()
		return false, false
	}
	if ok, err := e.sent.room(size); err != nil || !ok {
		e.ringMu.Unlock()
		return false, false
	}
	e.sent.begin(size)
	e.sent.write(0, hdr[:])
	e.sent.write(netHdrLen, frame)
	wake = e.sent.publish(size)
	e.ringMu.Unlock()
	if traceFrames {
		traceFrame("to guest (tunnel)", frame)
	}
	return true, wake
}

func (e *linkEndpoint) guestTakesLarge(v4 bool) bool {
	if v4 {
		return e.offload.Load()
	}
	return e.offload6.Load()
}

func (e *linkEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	n := 0
	wake := false
	offload := e.offload.Load()
	for _, pkt := range pkts.AsSlice() {
		size := netHdrLen + pkt.Size()
		if size > maxMessage {
			break
		}
		if pkt.GSOOptions.Type == stack.GSOTCPv6 && pkt.Size() > ipMTU+header.EthernetMinimumSize && !e.offload6.Load() {
			if !e.writeSegmented(pkt, &wake) {
				return n, &tcpip.ErrClosedForSend{}
			}
			n++
			continue
		}
		if !offload && pkt.Size() > ipMTU+header.EthernetMinimumSize {
			n++
			continue
		}
		if !e.waitForRoom(size, &wake) {
			return n, &tcpip.ErrClosedForSend{}
		}
		var hdr [netHdrLen]byte
		if gso := pkt.GSOOptions; offload && gso.Type != stack.GSONone {
			binary.LittleEndian.PutUint16(hdr[2:], uint16(pkt.HeaderSize()))
			if gso.NeedsCsum {
				hdr[0] = hdrNeedsCsum
				binary.LittleEndian.PutUint16(hdr[6:], header.EthernetMinimumSize+gso.L3HdrLen)
				binary.LittleEndian.PutUint16(hdr[8:], gso.CsumOffset)
			}
			if uint16(pkt.Data().Size()) > gso.MSS {
				switch gso.Type {
				case stack.GSOTCPv4:
					hdr[1] = hdrGSOTCPv4
					binary.LittleEndian.PutUint16(hdr[4:], gso.MSS)
				case stack.GSOTCPv6:
					hdr[1] = hdrGSOTCPv6
					binary.LittleEndian.PutUint16(hdr[4:], gso.MSS)
				}
			}
		}
		e.sent.begin(size)
		e.sent.write(0, hdr[:])
		at := netHdrLen
		for _, s := range pkt.AsSlices() {
			e.sent.write(at, s)
			at += len(s)
		}
		wake = e.sent.publish(size) || wake
		e.ringMu.Unlock()
		if traceFrames {
			var frame []byte
			for _, s := range pkt.AsSlices() {
				frame = append(frame, s...)
			}
			traceFrame("to guest", frame)
		}
		if e.onSend != nil {
			e.onSend(size - netHdrLen)
		}
		n++
	}
	if wake {
		e.wakePeer()
	}
	return n, nil
}

func (e *linkEndpoint) writeSegmented(pkt *stack.PacketBuffer, wake *bool) bool {
	var whole []byte
	for _, s := range pkt.AsSlices() {
		whole = append(whole, s...)
	}
	if len(whole) < header.EthernetMinimumSize {
		return true
	}
	eth := whole[:header.EthernetMinimumSize]
	ok := true
	segmentTCP(whole[header.EthernetMinimumSize:], int(pkt.GSOOptions.MSS), header.EthernetMinimumSize, func(seg []byte) {
		if !ok {
			return
		}
		copy(seg, eth)
		size := netHdrLen + len(seg)
		if !e.waitForRoom(size, wake) {
			ok = false
			return
		}
		var hdr [netHdrLen]byte
		e.sent.begin(size)
		e.sent.write(0, hdr[:])
		e.sent.write(netHdrLen, seg)
		*wake = e.sent.publish(size) || *wake
		e.ringMu.Unlock()
		if e.onSend != nil {
			e.onSend(len(seg))
		}
	})
	return ok
}

func (e *linkEndpoint) run() {
	err := e.serve()
	e.conn.Close()
	<-e.socketDone
	if err == nil {
		err = e.socketErr
	}
	e.writeMu.Lock()
	e.ringMu.Lock()
	e.closed = true
	if e.mapped {
		e.region.unmap()
	}
	e.ringMu.Unlock()
	e.writeMu.Unlock()
	e.mu.Lock()
	e.err = err
	onClose, ended := e.onClose, e.ended
	e.mu.Unlock()
	close(e.done)
	if onClose != nil {
		onClose()
	}
	if ended != nil {
		ended()
	}
}

func (e *linkEndpoint) serve() error {
	if e.region == nil {
		r, err := receiveRegion(e.conn)
		if err != nil {
			close(e.socketDone)
			return err
		}
		e.ringMu.Lock()
		e.attach(r, guestToNetstack, netstackToGuest)
		e.ringMu.Unlock()
		e.mapped = true
	}
	go e.readWakeups()
	return e.consume()
}

func (e *linkEndpoint) readWakeups() {
	defer close(e.socketDone)
	var buf [64]byte
	for {
		if _, err := e.conn.Read(buf[:]); err != nil {
			if err != io.EOF && !errors.Is(err, net.ErrClosed) {
				e.socketErr = err
			}
			return
		}
		for _, ch := range []chan struct{}{e.data, e.room} {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

func (e *linkEndpoint) consume() error {
	for {
		for {
			size, ok, err := e.received.peek()
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			if err := e.receive(size); err != nil {
				return err
			}
		}
		if !e.received.sleepIfEmpty() {
			continue
		}
		select {
		case <-e.data:
		case <-e.socketDone:
			return nil
		}
	}
}

func (e *linkEndpoint) receive(size int) error {
	if size == controlLen {
		var msg [controlLen]byte
		e.received.read(0, msg[:])
		if e.received.advance(size) {
			e.wakePeer()
		}
		if binary.LittleEndian.Uint32(msg[:]) == controlGuestAccepts {
			return e.guestAccepts(binary.LittleEndian.Uint32(msg[4:]), true)
		}
		return nil
	}
	if size < netHdrLen+header.EthernetMinimumSize || size > maxMessage {
		return fmt.Errorf("netstack: the VMM sent a message of %d bytes", size)
	}
	if err := e.guestAccepts(0, false); err != nil {
		return err
	}
	frame := buffer.NewViewSize(size - netHdrLen)
	e.received.read(netHdrLen, frame.AsSlice())
	var vhdr [netHdrLen]byte
	t := e.tunnel.Load()
	toMac := t != nil && t.forMac(frame.AsSlice())
	if toMac {
		e.received.read(0, vhdr[:])
	}
	if e.received.advance(size) {
		e.wakePeer()
	}
	if traceFrames {
		traceFrame("from guest", frame.AsSlice())
	}
	if t != nil {
		t.learnGuestMAC(frame.AsSlice())
	}
	if toMac {
		t.fromGuest(vhdr, frame.AsSlice())
		frame.Release()
		return nil
	}
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithView(frame)})
	e.mu.RLock()
	dispatcher := e.dispatcher
	e.mu.RUnlock()
	if dispatcher != nil {
		dispatcher.DeliverNetworkPacket(0, pkt)
	}
	pkt.DecRef()
	return nil
}

func (e *linkEndpoint) guestAccepts(flags uint32, announcement bool) error {
	if e.announced.Load() && !announcement {
		return nil
	}
	first := !e.announced.Swap(true)
	e.offload.Store(flags&acceptsPartialCsum != 0 && flags&acceptsTSO4 != 0)
	e.offload6.Store(flags&acceptsPartialCsum != 0 && flags&acceptsTSO4 != 0 && flags&acceptsTSO6 != 0)
	if first && e.onAccepts != nil {
		return e.onAccepts(flags)
	}
	return nil
}

var traceFrames = os.Getenv("FEV_NET_TRACE") != ""

func traceFrame(direction string, frame []byte) {
	eth := header.Ethernet(frame)
	if len(frame) < header.EthernetMinimumSize+header.IPv4MinimumSize || eth.Type() != header.IPv4ProtocolNumber {
		fmt.Fprintf(os.Stderr, "trace: %s: %d bytes, ethertype %#04x\n", direction, len(frame), uint16(eth.Type()))
		return
	}
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	fmt.Fprintf(os.Stderr, "trace: %s: %v -> %v proto %d ttl %d len %d flags %#x frag %d\n", direction,
		ip.SourceAddress(), ip.DestinationAddress(), ip.Protocol(), ip.TTL(), ip.TotalLength(), ip.Flags(), ip.FragmentOffset())
}
