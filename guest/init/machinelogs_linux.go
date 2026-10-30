package main

import (
	api "fev/proto/agent"

	"fev/guest/internal/logbuf"
)

func (s *system) MachineLogSubscribe(name string, runtime, backlog bool, deliver func(logbuf.Entry), backlogDone func()) (func(), error) {
	if runtime {
		stop, err := s.machines.RuntimeSubscribe(name, backlog, deliver, backlogDone)
		return stop, machineErr(err)
	}
	stop, err := s.machines.ConsoleSubscribe(name, backlog, deliver, backlogDone)
	return stop, machineErr(err)
}

func (s *system) MachineShareBind(names []string) ([]api.MachineShareBound, error) {
	b, err := s.machines.ShareBind(names)
	return b, machineErr(err)
}
