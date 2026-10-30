package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/smallfz/libnfs-go/memfs"
)

func makeObjects(mfs *memfs.MemFS, list string) {
	write := func(name, body string) {
		if f, err := mfs.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o644); err == nil {
			f.Write([]byte(body))
			f.Close()
		}
	}
	for i := 1; i <= 20; i++ {
		write(fmt.Sprintf("/r%d", i), fmt.Sprintf("root r%d\n", i))
	}
	for _, o := range strings.Split(list, ",") {
		if o == "" {
			continue
		}
		mfs.MkdirAll("/"+o, 0o755)
		for i := 1; i <= 20; i++ {
			write(fmt.Sprintf("/%s/f%d", o, i), fmt.Sprintf("%s f%d\n", o, i))
		}
	}
}

const (
	modeNormal  = "normal"
	modeNothing = "nothing"
	modeDelay   = "delay"
	modeRenewOK = "renewok"
)

var (
	mode   atomic.Value
	logMu  sync.Mutex
	connNo atomic.Int32
)

func logf(format string, args ...any) {
	logMu.Lock()
	fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05.000")+" "+format+"\n", args...)
	logMu.Unlock()
}

func readMode(file string) {
	b, err := os.ReadFile(file)
	m := strings.TrimSpace(string(b))
	switch {
	case err != nil:
		m = modeNormal
	case m != modeNormal && m != modeNothing && m != modeDelay && m != modeRenewOK:
		logf("switch: unknown mode %q, normal", m)
		m = modeNormal
	}
	mode.Store(m)
	logf("switch: mode %s", m)
}

func startSwitch(front net.Listener, inner, file string) error {
	readMode(file)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			readMode(file)
		}
	}()
	go func() {
		for {
			c, err := front.Accept()
			if err != nil {
				logf("switch: accept: %v", err)
				return
			}
			go carry(c, inner, int(connNo.Add(1)))
		}
	}()
	return nil
}

func readRecord(r io.Reader) ([]byte, error) {
	var rec []byte
	for {
		var mark [4]byte
		if _, err := io.ReadFull(r, mark[:]); err != nil {
			return nil, err
		}
		v := binary.BigEndian.Uint32(mark[:])
		frag := make([]byte, v&0x7fffffff)
		if _, err := io.ReadFull(r, frag); err != nil {
			return nil, err
		}
		rec = append(rec, frag...)
		if v&0x80000000 != 0 {
			return rec, nil
		}
	}
}

func writeRecord(w io.Writer, rec []byte) error {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(rec))|0x80000000)
	_, err := w.Write(append(b, rec...))
	return err
}

type call struct {
	xid, proc  uint32
	tag        []byte
	numops, op uint32
	ok         bool
}

var opNames = map[uint32]string{
	3: "ACCESS", 4: "CLOSE", 5: "COMMIT", 6: "CREATE", 9: "GETATTR", 10: "GETFH", 12: "LOCK",
	15: "LOOKUP", 16: "LOOKUPP", 18: "OPEN", 20: "OPEN_CONFIRM", 22: "PUTFH", 24: "PUTROOTFH",
	25: "READ", 26: "READDIR", 28: "REMOVE", 29: "RENAME", 30: "RENEW", 31: "RESTOREFH", 32: "SAVEFH",
	33: "SECINFO", 34: "SETATTR", 35: "SETCLIENTID", 36: "SETCLIENTID_CONFIRM", 38: "WRITE",
	39: "RELEASE_LOCKOWNER",
}

func (c call) String() string {
	if c.proc == 0 {
		return fmt.Sprintf("xid %08x NULL", c.xid)
	}
	name := opNames[c.op]
	if name == "" {
		name = fmt.Sprintf("op%d", c.op)
	}
	return fmt.Sprintf("xid %08x COMPOUND %q ops %d first %s", c.xid, c.tag, c.numops, name)
}

func parseCall(rec []byte) call {
	var c call
	off := 0
	u32 := func() (uint32, bool) {
		if off+4 > len(rec) {
			return 0, false
		}
		v := binary.BigEndian.Uint32(rec[off:])
		off += 4
		return v, true
	}
	opaque := func() ([]byte, bool) {
		n, ok := u32()
		if !ok || int(n) > len(rec)-off {
			return nil, false
		}
		b := rec[off : off+int(n)]
		off += int(n+3) &^ 3
		return b, off <= len(rec)
	}
	var ok bool
	if c.xid, ok = u32(); !ok {
		return c
	}
	var mtype, rpcvers, prog, vers uint32
	mtype, _ = u32()
	rpcvers, _ = u32()
	prog, _ = u32()
	vers, _ = u32()
	if c.proc, ok = u32(); !ok || mtype != 0 || rpcvers != 2 || prog != 100003 || vers != 4 {
		return c
	}
	for range 2 {
		if _, ok := u32(); !ok {
			return c
		}
		if _, ok := opaque(); !ok {
			return c
		}
	}
	if c.proc == 0 {
		c.ok = true
		return c
	}
	if c.tag, ok = opaque(); !ok {
		return c
	}
	if _, ok = u32(); !ok {
		return c
	}
	if c.numops, ok = u32(); !ok || c.numops == 0 {
		return c
	}
	c.op, c.ok = u32()
	return c
}

const (
	nfs4OK    = 0
	nfs4Delay = 10008
	opRenew   = 30
)

func answer(c call, st uint32) []byte {
	b := binary.BigEndian.AppendUint32(nil, c.xid)
	b = binary.BigEndian.AppendUint32(b, 1)
	b = binary.BigEndian.AppendUint32(b, 0)
	b = binary.BigEndian.AppendUint32(b, 0)
	b = binary.BigEndian.AppendUint32(b, 0)
	b = binary.BigEndian.AppendUint32(b, 0)
	if c.proc == 0 {
		return b
	}
	b = binary.BigEndian.AppendUint32(b, st)
	b = binary.BigEndian.AppendUint32(b, uint32(len(c.tag)))
	b = append(b, c.tag...)
	b = append(b, make([]byte, (4-len(c.tag)%4)%4)...)
	b = binary.BigEndian.AppendUint32(b, 1)
	b = binary.BigEndian.AppendUint32(b, c.op)
	return binary.BigEndian.AppendUint32(b, st)
}

func carry(c net.Conn, inner string, n int) {
	logf("conn %d opened", n)
	defer logf("conn %d closed", n)
	defer c.Close()
	var wmu sync.Mutex
	var g net.Conn
	defer func() {
		if g != nil {
			g.Close()
		}
	}()
	for {
		rec, err := readRecord(c)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				logf("conn %d read: %v", n, err)
			}
			return
		}
		cl := parseCall(rec)
		m := mode.Load().(string)
		if !cl.ok {
			m = modeNormal
		}
		switch m {
		case modeNothing:
			logf("conn %d call %v: dropped", n, cl)
			continue
		case modeDelay, modeRenewOK:
			st := uint32(nfs4Delay)
			if m == modeRenewOK && cl.numops == 1 && cl.op == opRenew {
				st = nfs4OK
			}
			what := "DELAY"
			if cl.proc == 0 {
				what = "ok"
			} else if st == nfs4OK {
				what = "OK"
			}
			logf("conn %d call %v: answered %s", n, cl, what)
			wmu.Lock()
			err = writeRecord(c, answer(cl, st))
			wmu.Unlock()
			if err != nil {
				return
			}
			continue
		}
		if g == nil {
			if g, err = net.Dial("unix", inner); err != nil {
				logf("conn %d dial: %v", n, err)
				return
			}
			go func(g net.Conn) {
				defer c.Close()
				for {
					rep, err := readRecord(g)
					if err != nil {
						return
					}
					st := ""
					if len(rep) >= 28 {
						st = fmt.Sprintf(" status %d", binary.BigEndian.Uint32(rep[24:]))
					}
					if len(rep) >= 4 {
						logf("conn %d reply xid %08x%s", n, binary.BigEndian.Uint32(rep), st)
					}
					wmu.Lock()
					err = writeRecord(c, rep)
					wmu.Unlock()
					if err != nil {
						return
					}
				}
			}(g)
		}
		logf("conn %d call %v: forwarded", n, cl)
		if err := writeRecord(g, rec); err != nil {
			return
		}
	}
}
