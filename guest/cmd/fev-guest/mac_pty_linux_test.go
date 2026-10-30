package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"fev/proto/bridge"
)

func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return m, s
}

func TestAResizeJustBeforeInputReachesTheMacBeforeIt(t *testing.T) {
	type event struct {
		resize     bool
		rows, cols int
		data       string
	}
	events := make(chan event, 16)
	f := newFakeBridge(t, func(h *bridge.Header, r *bufio.Reader, c net.Conn) {
		bridge.WriteOK(c)
		buf := make([]byte, bridge.MaxFrame)
		for {
			s, n, err := bridge.ReadFrame(r, buf)
			if err != nil {
				return
			}
			switch s {
			case bridge.FrameControl:
				var ctl bridge.Control
				json.Unmarshal(buf[:n], &ctl)
				if ctl.Rows > 0 {
					events <- event{resize: true, rows: ctl.Rows, cols: ctl.Cols}
				}
			case bridge.FrameStdin:
				events <- event{data: string(buf[:n])}
				b, _ := json.Marshal(bridge.Exit{Code: 0})
				bridge.WriteFrame(c, bridge.FrameExit, b)
				return
			}
		}
	})
	m, s := openPTY(t)
	if err := unix.IoctlSetWinsize(int(m.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 30, Col: 100}); err != nil {
		t.Fatal(err)
	}
	oi, oo, oe := stdin, stdout, stderr
	stdin, stdout, stderr = s, s, s
	t.Cleanup(func() { stdin, stdout, stderr = oi, oo, oe })
	done := make(chan int, 1)
	go func() { done <- macMain([]string{"sh"}) }()
	select {
	case h := <-f.headers:
		if !h.TTY() || h.Rows != 30 || h.Cols != 100 {
			t.Fatalf("header %+v", h)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no header")
	}
	if err := unix.IoctlSetWinsize(int(m.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 50, Col: 132}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Write([]byte("go\r")); err != nil {
		t.Fatal(err)
	}
	var seen []event
	for {
		select {
		case e := <-events:
			seen = append(seen, e)
			if e.data == "" {
				continue
			}
			if len(seen) < 2 || !seen[len(seen)-2].resize || seen[len(seen)-2].rows != 50 || seen[len(seen)-2].cols != 132 {
				t.Fatalf("the keystroke came with no resize to 50x132 before it: %+v", seen)
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the helper did not end")
			}
			return
		case <-time.After(10 * time.Second):
			t.Fatalf("nothing more from the helper: %+v", seen)
		}
	}
}
