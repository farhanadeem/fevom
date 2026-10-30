//go:build darwin && cgo

package macnet

/*
#cgo LDFLAGS: -framework SystemConfiguration -framework CoreFoundation
#include <stdlib.h>
#include "watch_darwin.h"
*/
import "C"

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

var active struct {
	mu sync.Mutex
	w  *Watcher
}

func Start(log *slog.Logger) (*Watcher, error) {
	active.mu.Lock()
	defer active.mu.Unlock()
	if active.w != nil {
		return nil, fmt.Errorf("macnet: the Mac's network is already watched in this process")
	}
	w := newWatcher(log, readMac)
	if r := C.fev_macnet_watch(); r != 0 {
		return nil, fmt.Errorf("macnet: cannot watch the System Configuration store (step %d)", int(r))
	}
	active.w = w
	w.unwatch = func() {
		C.fev_macnet_unwatch()
		active.mu.Lock()
		if active.w == w {
			active.w = nil
		}
		active.mu.Unlock()
	}
	return w, nil
}

//export fevMacnetChanged
func fevMacnetChanged(keys *C.char) {
	list := strings.Fields(C.GoString(keys))
	active.mu.Lock()
	w := active.w
	active.mu.Unlock()
	if w != nil {
		go w.changed(list)
	}
}
