package logs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	api "fev/proto/daemon"
)

var Sources = []string{api.LogDaemon, api.LogVMM, api.LogConsole, api.LogKernel, api.LogInit, api.LogEngine}

var fileOf = map[string]string{
	api.LogDaemon:  "fevd.log",
	api.LogVMM:     "vmm.log",
	api.LogConsole: "console.log",
	api.LogKernel:  "guest.log",
	api.LogInit:    "guest.log",
	api.LogEngine:  "guest.log",
}

func Files() []string { return []string{"fevd.log", "vmm.log", "console.log", "guest.log"} }

const (
	DefaultMaxBytes = 10 << 20
	Generations     = 3
	RingLines       = 200
	QueueLines      = 1024
	MaxLine         = 8 << 10
)

var lineBreaks = strings.NewReplacer("\r", `\r`, "\n", `\n`)

type Entry struct {
	Source  string
	Time    time.Time
	GuestMS float64
	Level   int
	Text    string

	End bool

	held bool
	boot uint64
}

type Options struct {
	Dir      string
	MaxBytes int64
	Echo     io.Writer
}

type Hub struct {
	opts Options

	mu     sync.Mutex
	files  map[string]*rotating
	rings  map[string]*ring
	subs   map[*Sub]struct{}
	failed bool
}

func Open(opts Options) (*Hub, error) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, err
	}
	h := &Hub{opts: opts, files: map[string]*rotating{}, rings: map[string]*ring{}, subs: map[*Sub]struct{}{}}
	for _, name := range Files() {
		f, err := openRotating(filepath.Join(opts.Dir, name), opts.MaxBytes)
		if err != nil {
			h.Close()
			return nil, err
		}
		h.files[name] = f
	}
	for _, s := range Sources {
		h.rings[s] = &ring{}
	}
	return h, nil
}

func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, f := range h.files {
		f.close()
		delete(h.files, name)
	}
}

func (h *Hub) Write(e Entry) { h.write(e) }

func (h *Hub) WriteHeld(e Entry, boot uint64) {
	e.held, e.boot = true, boot
	h.write(e)
}

func (h *Hub) write(e Entry) {
	if _, ok := fileOf[e.Source]; !ok {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if strings.ContainsAny(e.Text, "\r\n") {
		e.Text = lineBreaks.Replace(e.Text)
	}
	if len(e.Text) > MaxLine {
		e.Text = e.Text[:MaxLine]
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if f := h.files[fileOf[e.Source]]; f != nil {
		line := fmt.Sprintf("%s %s %d %s\n", e.Time.UTC().Format("2006-01-02T15:04:05.000000Z"), e.Source, e.Level, e.Text)
		if err := f.write(line); err != nil && !h.failed {
			h.failed = true
			fmt.Fprintf(os.Stderr, "fevd: cannot write %s: %v\n", f.path, err)
		}
		if h.opts.Echo != nil {
			io.WriteString(h.opts.Echo, line)
		}
	}
	h.rings[e.Source].add(e)
	if e.held {
		return
	}
	for s := range h.subs {
		if s.sources[e.Source] {
			s.push(e)
		}
	}
}

func (h *Hub) Release(source string, boot uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rings[source]
	if r == nil {
		return
	}
	for i := range r.lines {
		e := &r.lines[i]
		if !e.held || e.boot != boot {
			continue
		}
		e.held = false
		for s := range h.subs {
			if s.sources[source] {
				s.push(*e)
			}
		}
	}
}

func (h *Hub) Tail(source string, n int) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rings[source]
	if r == nil {
		return nil
	}
	lines := r.lines
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]string, len(lines))
	for i, e := range lines {
		out[i] = e.Text
	}
	return out
}

type ring struct{ lines []Entry }

func (r *ring) add(e Entry) {
	if len(r.lines) == RingLines {
		copy(r.lines, r.lines[1:])
		r.lines = r.lines[:RingLines-1]
	}
	r.lines = append(r.lines, e)
}

type Sub struct {
	hub     *Hub
	sources map[string]bool

	queue   []queued
	backlog int
	mark    bool
	dropped int
	closed  bool
	wake    chan struct{}
}

type queued struct {
	Entry
	dropped int
}

func (h *Hub) Subscribe(sources []string, backlog int) *Sub {
	s := &Sub{hub: h, sources: map[string]bool{}, wake: make(chan struct{}, 1)}
	if len(sources) == 0 {
		sources = Sources
	}
	for _, name := range sources {
		s.sources[name] = true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if backlog > 0 {
		for _, name := range Sources {
			if !s.sources[name] {
				continue
			}
			var visible []queued
			for _, e := range h.rings[name].lines {
				if !e.held {
					visible = append(visible, queued{Entry: e})
				}
			}
			if len(visible) > backlog {
				visible = visible[len(visible)-backlog:]
			}
			s.queue = append(s.queue, visible...)
		}
		sort.SliceStable(s.queue, func(i, j int) bool { return s.queue[i].Time.Before(s.queue[j].Time) })
		s.backlog = len(s.queue)
		s.mark = true
		s.wake <- struct{}{}
	}
	h.subs[s] = struct{}{}
	return s
}

func (s *Sub) push(e Entry) {
	if len(s.queue)-s.backlog >= QueueLines {
		s.dropped++
		return
	}
	s.queue = append(s.queue, queued{Entry: e, dropped: s.dropped})
	s.dropped = 0
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Sub) Next(stop <-chan struct{}) (Entry, int, bool) {
	for {
		s.hub.mu.Lock()
		if s.closed {
			s.hub.mu.Unlock()
			return Entry{}, 0, false
		}
		if s.mark && s.backlog == 0 {
			s.mark = false
			s.hub.mu.Unlock()
			return Entry{End: true}, 0, true
		}
		if len(s.queue) > 0 {
			q := s.queue[0]
			s.queue = s.queue[1:]
			if s.backlog > 0 {
				s.backlog--
			}
			s.hub.mu.Unlock()
			return q.Entry, q.dropped, true
		}
		s.hub.mu.Unlock()
		select {
		case <-s.wake:
		case <-stop:
			return Entry{}, 0, false
		}
	}
}

func (s *Sub) Close() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.closed = true
	delete(s.hub.subs, s)
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

type rotating struct {
	path string
	max  int64
	f    *os.File
	size int64
}

func openRotating(path string, max int64) (*rotating, error) {
	r := &rotating{path: path, max: max}
	if err := r.open(); err != nil {
		return nil, err
	}
	if r.size >= r.max {
		if err := r.rotate(); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *rotating) open() error {
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	f.Chmod(0o600)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, info.Size()
	return nil
}

func (r *rotating) rotate() error {
	r.f.Close()
	r.f = nil
	os.Remove(fmt.Sprintf("%s.%d", r.path, Generations))
	for g := Generations - 1; g >= 1; g-- {
		os.Rename(fmt.Sprintf("%s.%d", r.path, g), fmt.Sprintf("%s.%d", r.path, g+1))
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil {
		return err
	}
	return r.open()
}

func (r *rotating) write(line string) error {
	if r.f == nil {
		if err := r.open(); err != nil {
			return err
		}
	}
	if r.size > 0 && r.size+int64(len(line)) > r.max {
		if err := r.rotate(); err != nil {
			return err
		}
	}
	n, err := r.f.WriteString(line)
	r.size += int64(n)
	return err
}

func (r *rotating) close() {
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
}
