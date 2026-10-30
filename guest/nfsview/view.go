package nfsview

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	api "fev/proto/agent"
)

var ErrViewStopped = errors.New("nfsview: the view is not running")

type Starter interface {
	Start(p api.ViewStartParams, lost func(error)) (Running, *Tree, error)
}

type Running interface {
	Stop() error
	Dial(ctx context.Context) (net.Conn, error)
}

type View struct {
	starter Starter
	notify  func(api.ViewStatus)
	now     func() time.Time

	op sync.Mutex

	mu         sync.Mutex
	st         api.ViewStatus
	run        Running
	tree       *Tree
	objects    *Objects
	objectsFor func(*Tree, Running, api.ViewStartParams) *Objects
}

func (v *View) SetObjects(f func(*Tree, Running, api.ViewStartParams) *Objects) {
	v.mu.Lock()
	v.objectsFor = f
	v.mu.Unlock()
}

func (v *View) Objects() *Objects {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.st.State != api.ViewReady {
		return nil
	}
	return v.objects
}

func endObjects(o *Objects) func() {
	if o == nil {
		return func() {}
	}
	o.Close()
	return o.AfterStop
}

func NewView(s Starter, notify func(api.ViewStatus)) *View {
	v := &View{starter: s, notify: notify, now: time.Now}
	v.st = api.ViewStatus{State: api.ViewStopped, Since: v.stamp()}
	return v
}

func (v *View) stamp() string { return v.now().UTC().Format(time.RFC3339Nano) }

func (v *View) set(state string, err error) api.ViewStatus {
	v.st = api.ViewStatus{State: state, Since: v.stamp()}
	if err != nil {
		v.st.Error = err.Error()
	}
	st := v.statusLocked()
	if v.notify != nil {
		v.notify(st)
	}
	return st
}

func (v *View) statusLocked() api.ViewStatus {
	st := v.st
	if vp, ok := v.run.(interface{ VsockPort() uint32 }); ok && st.State == api.ViewReady {
		st.VsockPort = vp.VsockPort()
	}
	if st.State == api.ViewReady && v.tree != nil {
		var o api.ViewObjects
		for _, p := range v.tree.Objects() {
			switch p.Kind {
			case KindMachine:
				o.Machines++
			case KindVolume:
				o.Volumes++
			case KindContainer:
				o.Containers++
			case KindImage:
				o.Images++
			}
		}
		st.Objects = &o
	}
	return st
}

func (v *View) Status() api.ViewStatus {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.statusLocked()
}

func (v *View) Tree() *Tree {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.st.State != api.ViewReady {
		return nil
	}
	return v.tree
}

func (v *View) Start(p api.ViewStartParams) api.ViewStatus {
	v.mu.Lock()
	defer v.mu.Unlock()
	switch v.st.State {
	case api.ViewStarting, api.ViewReady:
		return v.statusLocked()
	}
	st := v.set(api.ViewStarting, nil)
	go v.start(p)
	return st
}

func (v *View) start(p api.ViewStartParams) {
	v.op.Lock()
	defer v.op.Unlock()
	v.mu.Lock()
	if v.st.State != api.ViewStarting {
		v.mu.Unlock()
		return
	}
	v.mu.Unlock()
	var (
		lmu     sync.Mutex
		started Running
		early   error
	)
	lost := func(err error) {
		lmu.Lock()
		r := started
		if r == nil && early == nil {
			early = err
		}
		lmu.Unlock()
		if r != nil {
			go v.lose(r, err)
		}
	}
	r, tree, err := v.starter.Start(p, lost)
	if err == nil {
		lmu.Lock()
		started = r
		e := early
		lmu.Unlock()
		if e != nil {
			if se := r.Stop(); se != nil {
				e = errors.Join(e, se)
			}
			err = e
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if err != nil {
		v.set(api.ViewFailed, err)
		return
	}
	v.run, v.tree = r, tree
	if v.objectsFor != nil {
		v.objects = v.objectsFor(tree, r, p)
	}
	v.set(api.ViewReady, nil)
}

func (v *View) lose(run Running, err error) {
	v.op.Lock()
	defer v.op.Unlock()
	v.mu.Lock()
	if run == nil || v.run != run {
		v.mu.Unlock()
		return
	}
	v.run, v.tree = nil, nil
	after := endObjects(v.objects)
	v.objects = nil
	v.mu.Unlock()
	if e := run.Stop(); e != nil {
		err = errors.Join(err, e)
	}
	after()
	v.mu.Lock()
	v.set(api.ViewFailed, err)
	v.mu.Unlock()
}

func (v *View) Stop() (api.ViewStatus, error) {
	v.op.Lock()
	defer v.op.Unlock()
	v.mu.Lock()
	run := v.run
	v.run, v.tree = nil, nil
	objects := v.objects
	v.objects = nil
	if run == nil {
		defer v.mu.Unlock()
		if v.st.State == api.ViewStopped {
			return v.statusLocked(), nil
		}
		return v.set(api.ViewStopped, nil), nil
	}
	v.mu.Unlock()
	after := endObjects(objects)
	err := run.Stop()
	after()
	v.mu.Lock()
	defer v.mu.Unlock()
	if err != nil {
		return v.set(api.ViewFailed, err), err
	}
	return v.set(api.ViewStopped, nil), nil
}

func (v *View) Dial(ctx context.Context) (net.Conn, error) {
	v.mu.Lock()
	run := v.run
	ready := v.st.State == api.ViewReady
	v.mu.Unlock()
	if !ready || run == nil {
		return nil, ErrViewStopped
	}
	return run.Dial(ctx)
}
