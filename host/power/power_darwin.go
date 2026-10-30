//go:build darwin && cgo

package power

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <stdlib.h>
#include "power_darwin.h"
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

var listener struct {
	mu sync.Mutex
	ch chan Event
}

type system struct{ ch chan Event }

func Listen() (Source, error) {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	if listener.ch != nil {
		return nil, fmt.Errorf("power notifications are already registered for in this process")
	}
	if r := C.fev_power_register(); r != 0 {
		return nil, fmt.Errorf("IORegisterForSystemPower failed (%d)", int(r))
	}
	listener.ch = make(chan Event, 16)
	return &system{ch: listener.ch}, nil
}

func (s *system) Events() <-chan Event { return s.ch }

func (s *system) Close() {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	if listener.ch != s.ch {
		return
	}
	for {
		select {
		case e := <-s.ch:
			e.Ack()
			continue
		default:
		}
		break
	}
	C.fev_power_unregister()
	listener.ch = nil
}

var allow = func(id int64) { C.fev_power_allow(C.long(id)) }

//export fevPowerEvent
func fevPowerEvent(kind C.int, id C.long) {
	listener.mu.Lock()
	ch := listener.ch
	listener.mu.Unlock()
	e := Event{Kind: Kind(kind)}
	if e.Kind == WillSleep {
		var once sync.Once
		e.Allow = func() { once.Do(func() { allow(int64(id)) }) }
	}
	if ch == nil {
		e.Ack()
		return
	}
	select {
	case ch <- e:
	default:
		e.Ack()
	}
}

func Assert(reason string, display bool) (release func(), err error) {
	name := C.CString(reason)
	defer C.free(unsafe.Pointer(name))
	var id C.uint32_t
	d := C.int(0)
	if display {
		d = 1
	}
	if r := C.fev_power_assert(name, d, &id); r != 0 {
		return nil, fmt.Errorf("IOPMAssertionCreateWithName failed (%#x)", uint32(r))
	}
	var once sync.Once
	return func() { once.Do(func() { C.fev_power_release(id) }) }, nil
}
