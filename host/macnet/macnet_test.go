package macnet

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fakeMac struct {
	mu  sync.Mutex
	s   Snapshot
	err error
	n   int
}

func (f *fakeMac) read() (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return f.s, f.err
}

func (f *fakeMac) set(s Snapshot) {
	f.mu.Lock()
	f.s = s
	f.mu.Unlock()
}

func TestAStoreChangePublishesANewGenerationToEverySubscriber(t *testing.T) {
	mac := &fakeMac{s: office()}
	w := NewWithReader(nil, mac.read)
	defer w.Close()
	if g := w.Current().Gen; g != 1 || w.Current().Cause[0] != "start" {
		t.Fatalf("first snapshot %d %v", g, w.Current().Cause)
	}
	var mu sync.Mutex
	var seen []uint64
	cancel := w.Subscribe(func(s Snapshot) {
		mu.Lock()
		seen = append(seen, s.Gen)
		mu.Unlock()
	})
	w.Notify("State:/Network/Global/Proxies")
	s := w.Current()
	if s.Gen != 2 || s.Cause[0] != "State:/Network/Global/Proxies" {
		t.Fatalf("after a store change: %d %v", s.Gen, s.Cause)
	}
	cancel()
	w.Notify("State:/Network/Global/IPv4")
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != 2 {
		t.Fatalf("subscriber saw %v", seen)
	}
}

func TestARefreshPublishesOnlyWhatChanged(t *testing.T) {
	mac := &fakeMac{s: office()}
	w := NewWithReader(nil, mac.read)
	defer w.Close()
	if s := w.Refresh("routes.set"); s.Gen != 1 {
		t.Fatalf("a refresh that found nothing new published generation %d", s.Gen)
	}
	mac.set(with(office(), Route{Prefix: pfx("198.19.2.0/24"), Interface: "utun5"}))
	s := w.Refresh("routes.set")
	if s.Gen != 2 || s.Cause[0] != "refresh: routes.set" {
		t.Fatalf("refresh: %d %v", s.Gen, s.Cause)
	}
	if got := Check(routed, "utun9", s).Withdrawn(); len(got) != 1 {
		t.Fatalf("withdrawn %v", got)
	}
}

func TestAFailedReadKeepsTheLastSnapshot(t *testing.T) {
	mac := &fakeMac{s: office()}
	w := NewWithReader(nil, mac.read)
	defer w.Close()
	mac.mu.Lock()
	mac.err = errors.New("sysctl failed")
	mac.mu.Unlock()
	w.Notify("State:/Network/Global/IPv4")
	if s := w.Current(); s.Gen != 1 || len(s.Routes) == 0 {
		t.Fatalf("after a failed read: %+v", s)
	}
}

func TestNothingInThePackageWakesByItself(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "time" {
				switch sel.Sel.Name {
				case "Tick", "NewTicker", "AfterFunc", "NewTimer", "After", "Sleep":
					t.Errorf("%s: time.%s", fset.Position(sel.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
	src, _ := os.ReadFile("watch_darwin.c")
	for _, bad := range []string{"dispatch_source", "CFRunLoopTimer", "PF_ROUTE", "sleep("} {
		if strings.Contains(string(src), bad) {
			t.Errorf("watch_darwin.c uses %s", bad)
		}
	}
}
