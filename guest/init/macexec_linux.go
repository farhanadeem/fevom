package main

import (
	"os"

	"fev/guest/internal/logbuf"
	"fev/guest/internal/macexec"
	"fev/guest/internal/stage"
	"fev/proto/bridge"
)

const macExecHandler = systemMount + bridge.ExecHandler

func (s *system) registerMacPrograms() error {
	if _, err := os.Stat(macExecHandler); err != nil {
		return stage.Skip("the system image has no %s: Mac programs do not run by path", bridge.ExecHandler)
	}
	if err := macexec.Mount(macexec.Dir); err != nil {
		return err
	}
	added, err := macexec.Ensure(macexec.Dir, macExecHandler, macexec.Entries)
	if err != nil {
		return err
	}
	s.log.Logf(logbuf.Debug, "init: binfmt_misc: Mac programs by path, %v", added)
	go func() {
		err := macexec.Watch(macexec.Dir, macExecHandler, macexec.Entries, func(format string, args ...any) {
			s.log.Logf(logbuf.Info, "init: "+format, args...)
		})
		s.log.Logf(logbuf.Warning, "init: binfmt_misc: the Mac programs' entries are no longer watched: %v", err)
	}()
	return nil
}
