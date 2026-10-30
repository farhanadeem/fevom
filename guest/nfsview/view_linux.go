package nfsview

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	api "fev/proto/agent"
)

const TreeRoot = "/run/fev/view/tree"

const NFSAddress = "127.0.0.1:2049"

type LinuxStarter struct {
	Root           string
	LeaseSeconds   int
	RefuseMacNames bool
	VsockPort      uint32
	Logf           func(string, ...any)
}

func (s *LinuxStarter) Start(p api.ViewStartParams, lost func(error)) (Running, *Tree, error) {
	if _, err := os.Stat(OwnerChangeParam); err != nil {
		return nil, nil, ErrNoOwnerChangeParam
	}
	root := s.Root
	if root == "" {
		root = TreeRoot
	}
	logf := s.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ns, err := NewNamespaces()
	if err != nil {
		return nil, nil, err
	}
	owner := Owner{UID: p.UID, GID: p.GID}
	tree := NewTree(root, owner)
	srv := &Server{Threads: p.Threads, LeaseSeconds: s.LeaseSeconds, RefuseMacNames: s.RefuseMacNames, VsockPort: s.VsockPort}
	if err := ns.Do(func() error {
		if err := tree.MakeRoot(); err != nil {
			return err
		}
		if err := srv.Mount(); err != nil {
			return err
		}
		if err := MountPipeFS(PipeFS); err != nil {
			return err
		}
		if srv.Running() {
			return errors.New("nfsview: nfsd already runs in a new namespace")
		}
		return FlushAll(time.Now())
	}); err != nil {
		ns.Close()
		return nil, nil, err
	}
	a := &Answerer{
		Table:   tree.Table(),
		Clients: []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		IDs:     MacNames{UID: owner.UID, GID: owner.GID, User: p.User, Group: p.Group},
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &linuxRun{ns: ns, srv: srv, cancel: cancel}
	ready := make(chan struct{})
	var once sync.Once
	cldEnded := make(chan error, 1)
	part := func(name string, early chan<- error) func(error) {
		return func(err error) {
			r.wg.Done()
			if ctx.Err() != nil {
				return
			}
			err = fmt.Errorf("nfsview: %s ended: %v", name, err)
			if early != nil {
				select {
				case early <- err:
				default:
				}
			}
			once.Do(func() { lost(err) })
		}
	}
	r.wg.Add(2)
	ns.Go(func() error { return ServeChannels(ctx, a, logf) }, part("the upcall answers", nil))
	ns.Go(func() error {
		return ServeCLDReady(ctx, PipeFS, func() { close(ready) }, logf)
	}, part("the client tracking's answers", cldEnded))
	select {
	case <-ready:
	case err := <-cldEnded:
		r.Stop()
		return nil, nil, err
	}
	if err := ns.Do(srv.Start); err != nil {
		r.Stop()
		return nil, nil, err
	}
	if _, err := srv.Vsock(); err != nil {
		logf("nfsd is reached through the relay alone: %v", err)
	}
	return r, tree, nil
}

type linuxRun struct {
	ns     *Namespaces
	srv    *Server
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (r *linuxRun) Stop() error {
	err := r.ns.Do(r.srv.Stop)
	r.cancel()
	r.wg.Wait()
	if e := r.ns.Close(); err == nil {
		err = e
	}
	return err
}

func (r *linuxRun) Namespaces() *Namespaces { return r.ns }

func (r *linuxRun) VsockPort() uint32 {
	if ok, _ := r.srv.Vsock(); ok {
		return r.srv.VsockPort
	}
	return 0
}

func (r *linuxRun) Dial(ctx context.Context) (net.Conn, error) {
	return r.ns.Dial(ctx, NFSAddress)
}
