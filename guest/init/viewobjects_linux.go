package main

import (
	"context"
	"net"
	"path/filepath"
	"strconv"

	"fev/guest/internal/logbuf"
	"fev/guest/machines"
	"fev/guest/nfsview"
	api "fev/proto/agent"
)

const (
	engineContainerd = engineRun + "/docker/containerd/containerd.sock"
	engineBundles    = engineRun + "/docker/containerd/daemon/io.containerd.runtime.v2.task/moby"
	engineDataName   = "/var/lib/docker"
)

type viewFeed struct {
	s     *system
	o     *nfsview.Objects
	fol   *nfsview.Follower
	dirty chan struct{}
}

func (s *system) viewObjects(tree *nfsview.Tree, run nfsview.Running, p api.ViewStartParams) *nfsview.Objects {
	nsr, ok := run.(interface{ Namespaces() *nfsview.Namespaces })
	if !ok {
		return nil
	}
	ns := nsr.Namespaces()
	logf := func(format string, args ...any) { s.log.Logf(logbuf.Warning, "view: "+format, args...) }
	ctr := &nfsview.Containerd{Dial: dialUnix(engineContainerd), Namespace: "moby", Snapshotter: "overlayfs"}
	images := &nfsview.LinuxImages{NS: ns, Ctr: ctr, From: engineDataName, Root: engineData, Logf: logf}
	o := nfsview.NewObjects(tree, ns, nfsview.ObjectsOptions{
		Images: images, Removed: s.viewRemoved, Logf: logf, Isolated: p.Isolated,
	})
	f := &viewFeed{s: s, o: o, dirty: make(chan struct{}, 1)}
	f.fol = &nfsview.Follower{
		Objects:  o,
		Dial:     dialUnix(engineSocket),
		EngineNS: s.engineMountNS,
		Bundle:   bundleOf,
		DataRoot: engineData,
		Logf:     logf,
	}
	s.viewFeed.Store(f)
	go f.machines()
	go f.engine()
	return o
}

func dialUnix(path string) func(context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}
}

func bundleOf(id string) (int, []string, error) {
	return nfsview.ReadBundle(filepath.Join(engineBundles, id))
}

func (s *system) viewRemoved(paths []string) {
	if link := s.host.Load(); link != nil {
		link.Conn.Notify(api.NotifyViewRemoved, api.ViewRemoved{Paths: paths})
	}
}

func (s *system) engineMountNS() (string, func(), bool) {
	e := s.engine
	e.mu.Lock()
	run := e.cur
	e.mu.Unlock()
	if run == nil {
		return "", nil, false
	}
	run.ns.RLock()
	if run.mnt == nil {
		run.ns.RUnlock()
		return "", nil, false
	}
	return "/proc/self/fd/" + strconv.Itoa(int(run.mnt.Fd())), run.ns.RUnlock, true
}

func (s *system) viewMachinesChanged() {
	if f := s.viewFeed.Load(); f != nil {
		select {
		case f.dirty <- struct{}{}:
		default:
		}
	}
}

func (s *system) viewIsolated(name string, isolated bool) {
	if o := s.view.Objects(); o != nil {
		o.SetIsolated(name, isolated)
	}
}

func (s *system) viewEngineEnded() {
	if o := s.view.Objects(); o != nil {
		o.ReleaseContainers()
	}
}

func (f *viewFeed) machines() {
	for {
		f.syncMachines()
		select {
		case <-f.o.Ended():
			if f.s.viewFeed.Load() == f {
				f.s.viewFeed.CompareAndSwap(f, nil)
			}
			return
		case <-f.dirty:
		}
	}
}

func (f *viewFeed) syncMachines() {
	if f.s.machines == nil {
		return
	}
	list, err := f.s.machines.List()
	if err != nil {
		return
	}
	var specs []nfsview.MachineSpec
	for _, m := range list.Machines {
		rec, err := machines.ReadRecord(machinesData, m.Name)
		if err != nil {
			continue
		}
		root := filepath.Join(machinesData, m.Name)
		uid := uint32(rec.User.UID)
		gid := uid
		if u, g, err := machines.UserIDs(root, rec.User.Name); err == nil {
			uid, gid = u, g
		}
		specs = append(specs, nfsview.MachineSpec{UUID: rec.UUID, Name: m.Name, Root: root, UID: uid, GID: gid})
	}
	f.o.SyncMachines(specs)
}

func (f *viewFeed) engine() {
	ready := make(chan struct{}, 1)
	stop := f.s.engine.sup.Subscribe(func(st api.EngineStatus) {
		if st.State == api.EngineReady {
			select {
			case ready <- struct{}{}:
			default:
			}
		}
	})
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-f.o.Ended()
		cancel()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ready:
		}
		if err := f.fol.Run(ctx); err != nil && ctx.Err() == nil {
			f.s.log.Logf(logbuf.Info, "view: the engine's events: %v", err)
		}
	}
}

func (s *system) serveViewControl() {
	logf := func(format string, args ...any) { s.log.Logf(logbuf.Warning, "view: "+format, args...) }
	l, err := nfsview.ListenControl()
	if err != nil {
		logf("the control socket: %v", err)
		return
	}
	c := &nfsview.Control{
		Objects:  s.view.Objects,
		EngineNS: s.engineMountNS,
		Name: func(ctx context.Context, id string) string {
			if f := s.viewFeed.Load(); f != nil {
				return f.fol.Name(ctx, id)
			}
			return ""
		},
		Bundle: bundleOf,
		Logf:   logf,
	}
	if err := c.Serve(l); err != nil {
		logf("the control socket: %v", err)
	}
}
