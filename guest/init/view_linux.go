package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fev/guest/agent"
	"fev/guest/internal/logbuf"
	"fev/guest/internal/netcfg"
	"fev/guest/internal/streams"
	"fev/guest/nfsview"
	api "fev/proto/agent"
)

func newView(s *system) *nfsview.View {
	v := nfsview.NewView(&viewStarter{s: s}, func(st api.ViewStatus) {
		if st.Error != "" {
			s.log.Logf(logbuf.Warning, "view: %s: %s", st.State, st.Error)
		} else {
			s.log.Logf(logbuf.Info, "view: %s", st.State)
		}
		if link := s.host.Load(); link != nil {
			link.Conn.Notify(api.NotifyViewChanged, st)
		}
	})
	v.SetObjects(s.viewObjects)
	return v
}

type viewStarter struct {
	s *system
}

func (v *viewStarter) Start(p api.ViewStartParams, lost func(error)) (nfsview.Running, *nfsview.Tree, error) {
	if _, err := os.Stat(nfsview.MacNamesParam); err != nil {
		return nil, nil, fmt.Errorf("this kernel has no %s (patch 9004): the view is not started, as it would take the Mac's ._* and .DS_Store files", nfsview.MacNamesParam)
	}
	ls := &nfsview.LinuxStarter{
		RefuseMacNames: true,
		VsockPort:      viewVsockPort(v.s.cmdline),
		Logf:           func(format string, args ...any) { v.s.log.Logf(logbuf.Warning, "view: "+format, args...) },
	}
	before := nfsdThreads("/proc")
	running, tree, err := ls.Start(p, lost)
	if err == nil {
		if n, err := placeViewThreads("/proc", viewCgroup, before); err != nil {
			v.s.log.Logf(logbuf.Warning, "view: %d of nfsd's threads in %s, then: %v; what the rest serve stays charged to no cgroup", n, viewCgroup, err)
		}
	}
	return running, tree, err
}

const viewCgroup = cgroupRoot + "/fev.view"

const pfKthread = 0x00200000

func nfsdThreads(proc string) map[string]string {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		if comm, err := os.ReadFile(proc + "/" + e.Name() + "/comm"); err != nil || strings.TrimSpace(string(comm)) != "nfsd" {
			continue
		}
		stat, err := os.ReadFile(proc + "/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
		if len(fields) < 20 {
			continue
		}
		if flags, err := strconv.ParseUint(fields[6], 10, 64); err != nil || flags&pfKthread == 0 {
			continue
		}
		out[e.Name()] = fields[19]
	}
	return out
}

var moveToCgroup = func(cgroup, pid string) error {
	return os.WriteFile(cgroup+"/cgroup.procs", []byte(pid), 0)
}

func placeViewThreads(proc, cgroup string, before map[string]string) (int, error) {
	if err := os.Mkdir(cgroup, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return 0, err
	}
	n := 0
	var errs []error
	for pid, start := range nfsdThreads(proc) {
		if was, ok := before[pid]; ok && was == start {
			continue
		}
		if err := moveToCgroup(cgroup, pid); err != nil {
			if !errors.Is(err, syscall.ESRCH) {
				errs = append(errs, fmt.Errorf("nfsd thread %s: %w", pid, err))
			}
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

var _ agent.View = (*system)(nil)

func (s *system) ViewStart(p api.ViewStartParams) api.ViewStatus { return s.view.Start(p) }
func (s *system) ViewStop() (api.ViewStatus, error)              { return s.view.Stop() }
func (s *system) ViewStatus() api.ViewStatus                     { return s.view.Status() }

func (s *system) ViewObjects() api.ViewObjectList {
	l := api.ViewObjectList{Objects: []api.ViewObject{}}
	if o := s.view.Objects(); o != nil {
		if objs := o.List(); objs != nil {
			l.Objects = objs
		}
	}
	return l
}

func (s *system) dialView(timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c, err := s.view.Dial(ctx)
	if errors.Is(err, nfsview.ErrViewStopped) {
		return nil, fmt.Errorf("%w", streams.ErrViewStopped)
	}
	return c, err
}

func viewHopsFile(cmdline string) string {
	if netcfg.Arg(cmdline, "fev.viewhops") != "1" {
		return ""
	}
	return shareMount + "/private/tmp/viewhops-guest.bin"
}

func viewVsockPort(cmdline string) uint32 {
	if netcfg.Arg(cmdline, "fev.viewvsock") == "0" {
		return 0
	}
	return api.ViewVsockPort
}
