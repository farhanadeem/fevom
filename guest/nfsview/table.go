package nfsview

import (
	"fmt"
	"sort"
	"sync"
)

const (
	FlagReadOnly      = 0x0001
	FlagInsecurePort  = 0x0002
	FlagRootSquash    = 0x0004
	FlagAllSquash     = 0x0008
	FlagAsync         = 0x0010
	FlagNoSubtreeChk  = 0x0400
	FlagFSID          = 0x2000
	FlagCrossMount    = 0x4000
	FlagNoHide        = 0x0200
	FlagNoReaddirPlus = 0x0040
)

const FlavorSys = 1

type Export struct {
	FSID    uint32
	Flags   int
	AnonUID uint32
	AnonGID uint32
}

type Table struct {
	mu     sync.Mutex
	byPath map[string]Export
	byFSID map[uint32]string
	asked  map[string]bool
	askedF map[uint32]bool
	keys   map[string]string
}

func NewTable() *Table {
	return &Table{
		byPath: map[string]Export{},
		byFSID: map[uint32]string{},
		asked:  map[string]bool{},
		askedF: map[uint32]bool{},
		keys:   map[string]string{},
	}
}

func (t *Table) SetKey(path string, typ int, key []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.keys[fmt.Sprintf("%d:%x", typ, key)] = path
}

func (t *Table) PathOfKey(typ int, key []byte) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.keys[fmt.Sprintf("%d:%x", typ, key)]
	if ok {
		if _, exported := t.byPath[p]; !exported {
			return "", false
		}
	}
	return p, ok
}

func (t *Table) Set(path string, e Export) (flush, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if p, held := t.byFSID[e.FSID]; held && p != path {
		return false, false
	}
	old, had := t.byPath[path]
	if had && old.FSID != e.FSID {
		delete(t.byFSID, old.FSID)
	}
	t.byPath[path] = e
	t.byFSID[e.FSID] = path
	flush = t.asked[path] || t.askedF[e.FSID] || (had && old != e)
	return flush, true
}

func (t *Table) Remove(path string) (flush bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, had := t.byPath[path]
	if !had {
		return false
	}
	delete(t.byPath, path)
	delete(t.byFSID, e.FSID)
	return t.asked[path] || t.askedF[e.FSID]
}

func (t *Table) Flushed() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.asked = map[string]bool{}
	t.askedF = map[uint32]bool{}
}

func (t *Table) Lookup(path string) (Export, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.asked[path] = true
	e, ok := t.byPath[path]
	return e, ok
}

func (t *Table) PathOf(fsid uint32) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.askedF[fsid] = true
	p, ok := t.byFSID[fsid]
	return p, ok
}

func (t *Table) Paths() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.byPath))
	for p := range t.byPath {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
