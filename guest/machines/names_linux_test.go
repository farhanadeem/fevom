package machines

import (
	"errors"
	"path/filepath"
	"testing"

	"fev/guest/storage"
)

func TestANameIsRefusedBesideOneDifferingOnlyInCase(t *testing.T) {
	dir := t.TempDir()
	data, cg := filepath.Join(dir, "data"), filepath.Join(dir, "cgroup")
	writeFiles(t, data, map[string]string{
		"Dev/etc/hostname": "Dev\n", ".records/Dev.json": "{}", ".images/x": "",
	})
	writeFiles(t, cg, map[string]string{"cgroup.procs": "", "memory.max": "max\n", "Dev/cgroup.procs": ""})
	rt := New(Options{DataDir: data, CgroupDir: cg})
	making, running, dev := &machine{name: "Web.1"}, &machine{name: "app"}, &machine{name: "Dev"}
	rt.making["Web.1"] = making
	rt.runNames["Old"] = running
	taken := func(name string, self *machine) error {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return rt.takenLocked(name, self)
	}
	for _, c := range []struct {
		name   string
		self   *machine
		exists bool
		ok     bool
	}{
		{"Dev", nil, true, false},
		{"dev", nil, true, false},
		{"DEV", nil, true, false},
		{"web.1", nil, true, false},
		{"WEB.1", nil, true, false},
		{"old", nil, false, false},
		{"memory.max", nil, false, false},
		{"cgroup.procs", nil, false, false},
		{".images", nil, false, false},
		{"Default", nil, false, false},
		{"dev2", nil, false, true},
		{"memory.maxi", nil, false, true},
		{"1abc", nil, false, true},
		{"x.y-z", nil, false, true},
		{"DEV", dev, false, true},
		{"dEv", dev, false, true},
		{"OLD", running, false, true},
		{"old", dev, false, false},
		{"Web.1", making, false, true},
	} {
		err := taken(c.name, c.self)
		switch {
		case c.ok && err != nil:
			t.Errorf("%q (self %v): refused: %v", c.name, c.self != nil, err)
		case !c.ok && err == nil:
			t.Errorf("%q (self %v): accepted", c.name, c.self != nil)
		case c.exists && !errors.Is(err, storage.ErrMachineExists):
			t.Errorf("%q: %v, want a machine that exists", c.name, err)
		}
	}
}
