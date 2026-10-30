//go:build darwin && cgo

package macnet

import (
	"testing"
	"time"
	"unsafe"
)

func TestTheStoresWatchStartsTakesACallbackAndStops(t *testing.T) {
	w, err := Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Start(nil); err == nil {
		t.Fatal("a second watch was allowed")
	}
	if s := w.Current(); s.Gen != 1 || len(s.Routes) == 0 {
		t.Fatalf("first snapshot: gen %d, %d routes", s.Gen, len(s.Routes))
	}
	got := make(chan Snapshot, 1)
	w.Subscribe(func(s Snapshot) { got <- s })
	keys := []byte("State:/Network/Global/IPv4\nState:/Network/Interface/en0/IPv4\n\x00")
	fevMacnetChanged((*_Ctype_char)(unsafe.Pointer(&keys[0])))
	select {
	case s := <-got:
		if s.Gen != 2 || len(s.Cause) != 2 || s.Cause[1] != "State:/Network/Interface/en0/IPv4" {
			t.Fatalf("callback published %d %v", s.Gen, s.Cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no snapshot after a callback")
	}
	w.Close()
	w2, err := Start(nil)
	if err != nil {
		t.Fatalf("after Close: %v", err)
	}
	w2.Close()
}
