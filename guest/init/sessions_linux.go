package main

import (
	"path/filepath"
)

const initSessions = cgroupRoot + "/fev.init/sessions"

func (e *engine) SessionGroups() string { return filepath.Join(engineCgroup, "exec") }

func (e *engine) EntryPath() string { return containerdName }
