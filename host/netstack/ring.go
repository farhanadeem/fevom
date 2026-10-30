package netstack

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const (
	ringMagic        = "BRTHNET2"
	helloLen         = 16
	ringHeaderLen    = 16 << 10
	ringHeaderStride = 1024
	minRingBytes     = 1 << 16
	maxRingBytes     = 1 << 26

	ringHead            = 0
	ringTail            = 128
	ringConsumerWaiting = 256
	ringProducerWaiting = 384

	guestToNetstack = 0
	netstackToGuest = 1
)

var errRingCorrupt = errors.New("netstack: the VMM put an impossible value in the shared ring")

func span(n int) uint64 { return (uint64(lenPrefix+n) + 7) &^ 7 }

type region struct {
	mem      []byte
	capacity int
}

func receiveRegion(conn *net.UnixConn) (*region, error) {
	hello := make([]byte, helloLen)
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, err := conn.ReadMsgUnix(hello, oob)
	if err != nil {
		return nil, err
	}
	var fds []int
	if msgs, err := syscall.ParseSocketControlMessage(oob[:oobn]); err == nil {
		for _, m := range msgs {
			got, _ := syscall.ParseUnixRights(&m)
			fds = append(fds, got...)
		}
	}
	defer func() {
		for _, fd := range fds {
			syscall.Close(fd)
		}
	}()
	if n != helloLen || string(hello[:8]) != ringMagic {
		return nil, errors.New("netstack: the VMM did not say hello")
	}
	if len(fds) != 1 {
		return nil, errors.New("netstack: the VMM's hello came without its memory")
	}
	return mapRegion(fds[0], int(binary.LittleEndian.Uint32(hello[8:])))
}

func mapRegion(fd, capacity int) (*region, error) {
	if capacity < minRingBytes || capacity > maxRingBytes || capacity&(capacity-1) != 0 {
		return nil, fmt.Errorf("netstack: a ring of %d bytes is not one we can use", capacity)
	}
	size := ringHeaderLen + 2*capacity
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Size < int64(size) {
		return nil, errors.New("netstack: the shared region is smaller than its rings")
	}
	mem, err := syscall.Mmap(fd, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	return &region{mem: mem, capacity: capacity}, nil
}

func (r *region) unmap() { syscall.Munmap(r.mem) }

type ring struct {
	head, tail                       *atomic.Uint64
	consumerWaiting, producerWaiting *atomic.Uint32
	data                             []byte
	mask                             uint64
}

func (r *region) ring(index int) ring {
	h := r.mem[index*ringHeaderStride:]
	return ring{
		head:            (*atomic.Uint64)(unsafe.Pointer(&h[ringHead])),
		tail:            (*atomic.Uint64)(unsafe.Pointer(&h[ringTail])),
		consumerWaiting: (*atomic.Uint32)(unsafe.Pointer(&h[ringConsumerWaiting])),
		producerWaiting: (*atomic.Uint32)(unsafe.Pointer(&h[ringProducerWaiting])),
		data:            r.mem[ringHeaderLen+index*r.capacity:][:r.capacity:r.capacity],
		mask:            uint64(r.capacity - 1),
	}
}

func (r *ring) copyOut(pos uint64, dst []byte) {
	off := int(pos & r.mask)
	n := copy(dst, r.data[off:])
	copy(dst[n:], r.data)
}

func (r *ring) copyIn(pos uint64, src []byte) {
	off := int(pos & r.mask)
	n := copy(r.data[off:], src)
	copy(r.data, src[n:])
}

type consumer struct {
	ring
	pos     uint64
	waiting bool
}

func (r *region) consumer(index int) consumer { return consumer{ring: r.ring(index)} }

func (c *consumer) peek() (int, bool, error) {
	if c.waiting {
		c.waiting = false
		c.consumerWaiting.Store(0)
	}
	available := c.tail.Load() - c.pos
	if available == 0 {
		return 0, false, nil
	}
	if available > uint64(len(c.data)) || available%8 != 0 {
		return 0, false, errRingCorrupt
	}
	var prefix [lenPrefix]byte
	c.copyOut(c.pos, prefix[:])
	n := uint64(binary.LittleEndian.Uint32(prefix[:]))
	if n > available || span(int(n)) > available {
		return 0, false, errRingCorrupt
	}
	return int(n), true, nil
}

func (c *consumer) read(offset int, dst []byte) {
	c.copyOut(c.pos+uint64(lenPrefix+offset), dst)
}

func (c *consumer) advance(n int) bool {
	c.pos += span(n)
	c.head.Store(c.pos)
	return c.producerWaiting.Swap(0) == 1
}

func (c *consumer) sleepIfEmpty() bool {
	c.consumerWaiting.Store(1)
	if c.tail.Load() == c.pos {
		c.waiting = true
		return true
	}
	c.consumerWaiting.Store(0)
	return false
}

type producer struct {
	ring
	pos     uint64
	waiting bool
}

func (r *region) producer(index int) producer { return producer{ring: r.ring(index)} }

func (p *producer) room(n int) (bool, error) {
	used := p.pos - p.head.Load()
	if used > uint64(len(p.data)) {
		return false, errRingCorrupt
	}
	return uint64(len(p.data))-used >= span(n), nil
}

func (p *producer) hasRoom(n int) (bool, error) {
	if p.waiting {
		p.waiting = false
		p.producerWaiting.Store(0)
	}
	return p.room(n)
}

func (p *producer) waitForRoom(n int) (bool, error) {
	p.producerWaiting.Store(1)
	ok, err := p.room(n)
	if err != nil {
		return false, err
	}
	if !ok {
		p.waiting = true
		return true, nil
	}
	p.producerWaiting.Store(0)
	return false, nil
}

func (p *producer) begin(n int) {
	var prefix [lenPrefix]byte
	binary.LittleEndian.PutUint32(prefix[:], uint32(n))
	p.copyIn(p.pos, prefix[:])
}

func (p *producer) write(offset int, src []byte) {
	p.copyIn(p.pos+uint64(lenPrefix+offset), src)
}

func (p *producer) publish(n int) bool {
	p.pos += span(n)
	p.tail.Store(p.pos)
	return p.consumerWaiting.Swap(0) == 1
}
