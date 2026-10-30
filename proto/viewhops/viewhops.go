package viewhops

import (
	"encoding/binary"
	"os"
	"sync"
	"time"
	_ "unsafe"
)

//go:linkname nanotime runtime.nanotime
func nanotime() int64

func Now() int64 { return nanotime() }

const (
	Fevd  = 'B'
	Guest = 'G'
	VMM   = 'V'
	Call  = 'C'
	Reply = 'R'
)

type Recorder struct {
	mu    sync.Mutex
	f     *os.File
	buf   []byte
	place byte
	timer *time.Timer
}

func Open(path string, place byte) *Recorder {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil
	}
	r := &Recorder{f: f, place: place, buf: make([]byte, 0, 1<<20)}
	r.timer = time.AfterFunc(time.Hour, r.Flush)
	r.timer.Stop()
	return r
}

func (r *Recorder) Add(dir byte, op uint16, xid, size uint32, read, written int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	var b [28]byte
	b[0], b[1] = r.place, dir
	binary.LittleEndian.PutUint16(b[2:], op)
	binary.LittleEndian.PutUint32(b[4:], xid)
	binary.LittleEndian.PutUint32(b[8:], size)
	binary.LittleEndian.PutUint64(b[12:], uint64(read))
	binary.LittleEndian.PutUint64(b[20:], uint64(written))
	empty := len(r.buf) == 0
	r.buf = append(r.buf, b[:]...)
	full := len(r.buf) >= cap(r.buf)-64
	r.mu.Unlock()
	if full {
		r.Flush()
	} else if empty {
		r.timer.Reset(500 * time.Millisecond)
	}
}

func (r *Recorder) Flush() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) > 0 {
		r.f.Write(r.buf)
		r.buf = r.buf[:0]
	}
}

type Marks struct {
	head      [8]byte
	nhead     int
	left      uint32
	midRecord bool
}

func (m *Marks) Feed(p []byte, each func(xid, size uint32, rest []byte)) {
	for len(p) > 0 {
		if m.left > 0 {
			n := uint32(len(p))
			if n > m.left {
				n = m.left
			}
			m.left -= n
			p = p[n:]
			continue
		}
		want := 4
		if !m.midRecord {
			want = 8
		}
		n := copy(m.head[m.nhead:want], p)
		m.nhead += n
		p = p[n:]
		if m.nhead < want {
			return
		}
		mark := binary.BigEndian.Uint32(m.head[:4])
		size := mark & 0x7fffffff
		last := mark&0x80000000 != 0
		if !m.midRecord {
			each(binary.BigEndian.Uint32(m.head[4:8]), size, p[:min(len(p), int(size)-min(int(size), 4))])
			size -= min(size, 4)
		}
		m.nhead = 0
		m.left = size
		m.midRecord = !last
	}
}

func MainOp(p []byte) uint16 {
	const none = 0xffff
	u32 := func() (uint32, bool) {
		if len(p) < 4 {
			return 0, false
		}
		v := binary.BigEndian.Uint32(p)
		p = p[4:]
		return v, true
	}
	opaque := func() bool {
		n, ok := u32()
		if !ok {
			return false
		}
		n = (n + 3) &^ 3
		if uint32(len(p)) < n {
			return false
		}
		p = p[n:]
		return true
	}
	var f [5]uint32
	for i := range f {
		v, ok := u32()
		if !ok {
			return none
		}
		f[i] = v
	}
	if f[4] == 0 {
		return 0
	}
	for range 2 {
		if _, ok := u32(); !ok || !opaque() {
			return none
		}
	}
	if !opaque() {
		return none
	}
	if _, ok := u32(); !ok {
		return none
	}
	n, ok := u32()
	if !ok {
		return none
	}
	for range n {
		op, ok := u32()
		if !ok {
			return none
		}
		switch op {
		case 22:
			if !opaque() {
				return none
			}
		case 24:
		default:
			return uint16(op)
		}
	}
	return none
}
