package machines

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const consoleTail = 4 << 10

type console struct {
	master *os.File
	slave  string
	hold   *os.File
	log    *consoleLog

	mu    sync.Mutex
	tail  []byte
	ended bool
	done  chan struct{}
}

func openConsole(log *consoleLog) (*console, error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		unix.Close(fd)
		return nil, err
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80})
	c := &console{master: os.NewFile(uintptr(fd), "ptmx"), slave: fmt.Sprintf("/dev/pts/%d", n), done: make(chan struct{})}
	if c.hold, err = c.reopen(); err != nil {
		c.master.Close()
		return nil, err
	}
	c.log = log
	go c.drain()
	return c, nil
}

func (c *console) reopen() (*os.File, error) {
	return os.OpenFile(c.slave, os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
}

func (c *console) drain() {
	defer close(c.done)
	defer c.log.endBoot()
	buf := make([]byte, 32<<10)
	for {
		n, err := c.master.Read(buf)
		if n > 0 {
			c.log.Write(buf[:n])
			c.mu.Lock()
			c.tail = append(c.tail, buf[:n]...)
			if len(c.tail) > consoleTail {
				c.tail = append([]byte(nil), c.tail[len(c.tail)-consoleTail:]...)
			}
			c.mu.Unlock()
		}
		if err == nil {
			continue
		}
		c.mu.Lock()
		ended := c.ended
		c.mu.Unlock()
		if ended || errors.Is(err, os.ErrClosed) {
			return
		}
		if f, rerr := c.reopen(); rerr == nil {
			c.mu.Lock()
			old := c.hold
			c.hold = f
			c.mu.Unlock()
			old.Close()
			continue
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (c *console) Tail() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.tail)
}

func (c *console) close() {
	c.mu.Lock()
	c.ended = true
	hold := c.hold
	c.mu.Unlock()
	hold.Close()
	c.master.Close()
	<-c.done
}
