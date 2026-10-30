package machines

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	"fev/guest/internal/logbuf"
)

const consoleFileCap = 1 << 20

func ConsoleFile(dataDir, name string) string {
	return filepath.Join(dataDir, RecordsDir, name+".console")
}

type consoleLog struct {
	path string
	ring *logbuf.Buffer
	log  *logbuf.Buffer

	mu     sync.Mutex
	f      *os.File
	size   int64
	split  lineSplitter
	failed bool
}

func newConsoleLog(path string, log *logbuf.Buffer) *consoleLog {
	c := &consoleLog{path: path, ring: logbuf.New(consoleRingLines, monotonicMS, nil), log: log}
	var lines []string
	for _, p := range []string{path + ".1", path} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var s lineSplitter
		lines = append(lines, s.feed(b)...)
		lines = append(lines, s.flush()...)
	}
	if len(lines) > consoleRingLines {
		lines = lines[len(lines)-consoleRingLines:]
	}
	for _, l := range lines {
		c.ring.Append(logbuf.Entry{Level: logbuf.Info, Text: l})
	}
	if st, err := os.Stat(path); err == nil {
		c.size = st.Size()
	}
	return c
}

func (c *consoleLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.write(p)
	for _, l := range c.split.feed(p) {
		c.ring.Logf(logbuf.Info, "%s", l)
	}
	return len(p), nil
}

func (c *consoleLog) write(p []byte) {
	if c.f != nil && c.size+int64(len(p)) > consoleFileCap {
		c.f.Close()
		c.f = nil
		if err := os.Rename(c.path, c.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
			c.fail(err)
		}
		c.size = 0
	}
	if c.f == nil {
		f, err := os.OpenFile(c.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			c.fail(err)
			return
		}
		c.f = f
	}
	n, err := c.f.Write(p)
	c.size += int64(n)
	if err != nil {
		c.fail(err)
	}
}

func (c *consoleLog) fail(err error) {
	if !c.failed && c.log != nil {
		c.log.Logf(logbuf.Warning, "machines: the console's file %s: %v; its lines are kept in memory only", c.path, err)
	}
	c.failed = true
}

func (c *consoleLog) endBoot() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.split.flush() {
		c.ring.Logf(logbuf.Info, "%s", l)
	}
	if c.f != nil {
		c.f.Close()
		c.f = nil
	}
}

func (m *machine) consoleLog() *consoleLog {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.clog == nil {
		m.clog = newConsoleLog(ConsoleFile(m.rt.o.DataDir, m.name), m.rt.o.Log)
	}
	return m.clog
}

func (rt *Runtime) ConsoleSubscribe(name string, backlog bool, deliver func(logbuf.Entry), backlogDone func()) (stop func(), err error) {
	if err := rt.available(); err != nil {
		return nil, err
	}
	if _, err := ReadRecord(rt.o.DataDir, name); err != nil {
		return nil, err
	}
	return rt.get(name).consoleLog().ring.SubscribeMarked(backlog, deliver, backlogDone), nil
}

const runtimeRingLines = 200

func (rt *Runtime) followRuntimeLines() {
	if rt.o.Log == nil {
		return
	}
	rt.o.Log.Subscribe(true, func(e logbuf.Entry) {
		name, ok := runtimeLineMachine(e.Text)
		if !ok {
			return
		}
		rt.runtimeRing(name).Append(e)
	})
}

func (rt *Runtime) runtimeRing(name string) *logbuf.Buffer {
	rt.rlogMu.Lock()
	defer rt.rlogMu.Unlock()
	if rt.rlogs == nil {
		rt.rlogs = map[string]*logbuf.Buffer{}
	}
	b := rt.rlogs[name]
	if b == nil {
		b = logbuf.New(runtimeRingLines, monotonicMS, nil)
		rt.rlogs[name] = b
	}
	return b
}

func (rt *Runtime) RuntimeSubscribe(name string, backlog bool, deliver func(logbuf.Entry), backlogDone func()) (stop func(), err error) {
	if err := rt.available(); err != nil {
		return nil, err
	}
	if _, err := ReadRecord(rt.o.DataDir, name); err != nil {
		return nil, err
	}
	return rt.runtimeRing(name).SubscribeMarked(backlog, deliver, backlogDone), nil
}
