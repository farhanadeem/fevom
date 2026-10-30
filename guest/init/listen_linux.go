package main

import (
	"fev/guest/internal/listen"
	"fev/guest/internal/logbuf"
	api "fev/proto/agent"
)

func (s *system) startListen() error {
	w, err := listen.Attach(machinesCgroup)
	if err != nil {
		s.log.Logf(logbuf.Warning, "init: machines' listening ports are not followed: %v", err)
		return err
	}
	s.net.SetListeners(func(netns uint64) []listen.Listener {
		return w.Listeners(func(n uint64) bool { return n == netns })
	})
	if cookie, err := initNetNSCookie(); err != nil {
		s.log.Logf(logbuf.Warning, "init: host-network containers' ports are not followed: %v", err)
	} else {
		s.engPorts.watcher(w, cookie)
	}
	go func() {
		err := w.Run(func(ev listen.Event) {
			if !s.engPorts.event(ev) {
				s.net.ListenEvent(ev)
			}
		})
		s.log.Logf(logbuf.Error, "init: listen events ended (%d lost): %v", w.Lost(), err)
	}()
	return nil
}

func (s *system) MachinePorts() api.MachinePortsList { return s.net.Ports() }
