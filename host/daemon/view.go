package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fev/host/docker/vsock"
	"fev/host/nfsview"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/relay"

	"golang.org/x/sys/unix"
)

type View struct {
	Dir      string
	Guard    func(dir, source string) (stop func(), err error)
	Isolated func() []string

	host      Host
	d         *Daemon
	log       *slog.Logger
	srv       *nfsview.Server
	endpoint  nfsview.Endpoint
	stopGuard func()

	op           sync.Mutex
	mu           sync.Mutex
	block        api.FilesView
	boot         string
	ended        string
	stopping     string
	folderErr    string
	paused       bool
	mounted      bool
	keeper       *nfsview.Keeper
	cancelMount  context.CancelFunc
	slowSaid     bool
	settingValue bool
	vsockBoot    string
	vsockPort    uint32

	answeredBefore atomic.Int64
	quieting       atomic.Bool
}

const (
	viewCallBound    = 10 * time.Second
	viewMountBound   = 30 * time.Second
	viewUnmountBound = 5 * time.Second
	viewAliveBound   = time.Second
	viewMountAgain   = time.Second
)

func (v *View) Name() string    { return "view" }
func (v *View) Marks() []string { return nil }

func (v *View) Start(h Host) error {
	if v.Dir == "" {
		return nil
	}
	v.host, v.log = h, h.Log()
	if mh, ok := h.(moduleHost); ok {
		v.d = mh.d
	}
	v.settingValue = h.Config().Bool(api.KeyFilesViewHidden)
	v.block = api.FilesView{State: api.ViewUnmounted, Path: v.Dir, Hidden: v.settingValue}
	if err := v.ready(); err != nil {
		v.folderErr = err.Error()
		v.failLocked(v.folderErr)
	}
	srv, err := nfsview.ListenLoopback(viewOpener{v}, v.log)
	if err != nil {
		return fmt.Errorf("the view's server: %w", err)
	}
	v.srv = srv
	v.endpoint = srv.Endpoint()
	v.block.Socket = v.endpoint.String()
	srv.OnStall(v.stalled)
	if v.Guard != nil {
		stop, err := v.Guard(v.Dir, nfsview.Source(v.endpoint))
		if err != nil {
			v.log.Warn("view: no guard; a view this daemon leaves when it is killed is removed at its next start", "err", err)
		} else {
			v.stopGuard = stop
		}
	}
	if v.d != nil {
		v.d.setFilesView(v.status)
	}
	v.publish()
	return nil
}

func (v *View) ready() error {
	removed, err := nfsview.Recover(v.Dir)
	if err != nil {
		return err
	}
	if removed {
		v.log.Warn("view: removed the view an earlier daemon left mounted", "path", v.Dir)
	}
	return nfsview.Prepare(v.Dir)
}

func (v *View) status() *api.FilesView {
	v.mu.Lock()
	defer v.mu.Unlock()
	b := v.block
	return &b
}

func (v *View) publish() {
	b := v.status()
	updateStatus(v.host, func(st *api.Status) { st.View = b })
	if v.d != nil {
		v.d.publishView()
	}
}

func (v *View) setLocked(state string) {
	v.block.State, v.block.Error = state, ""
}

func (v *View) failLocked(why string) {
	v.block.State, v.block.Error = api.ViewFailed, why
	if v.log != nil {
		v.log.Warn("view: " + why)
	}
}

type viewOpener struct{ v *View }

func (o viewOpener) Open(ctx context.Context, req relay.Request) (*net.UnixConn, error) {
	vm, ok := o.v.host.VM()
	if !ok {
		return nil, errors.New("the VM is not running")
	}
	o.v.mu.Lock()
	boot, port := o.v.vsockBoot, o.v.vsockPort
	o.v.mu.Unlock()
	if port != 0 && boot == vm.BootID() {
		return vsock.Dial(ctx, vm.VsockPath(port))
	}
	return vsock.Relay{Path: vm.VsockPath(relay.Port)}.Open(ctx, req)
}

func (v *View) StateChanged(st api.Status) {
	if v.Dir == "" {
		return
	}
	v.mu.Lock()
	wasPaused := v.paused
	v.paused = st.State == api.StatePaused
	start := st.State == api.StateRunning && st.BootID != "" && st.BootID != v.boot && st.BootID != v.ended
	if start {
		v.boot = st.BootID
		v.slowSaid = false
	}
	v.mu.Unlock()
	if wasPaused && st.State == api.StateRunning {
		v.srv.Rearm()
	}
	if start {
		go v.start(st.BootID)
	}
}

func (v *View) Paused(paused bool) {
	if v.srv == nil {
		return
	}
	if paused {
		if !v.quieting.Load() {
			v.answeredBefore.Store(v.srv.Answered())
		}
		v.srv.SetPaused(true)
		go func() {
			v.log.Debug("view: the VM is paused; the Mac's calls are answered in the guest's place", "out", v.srv.Out())
		}()
		return
	}
	v.quieting.Store(false)
	v.srv.SetPaused(false)
	n := v.srv.Answered() - v.answeredBefore.Load()
	go v.log.Debug("view: the VM runs; the Mac's calls go to the guest again", "answered", n)
}

func (v *View) Quiet() {
	if v.srv != nil {
		v.answeredBefore.Store(v.srv.Answered())
		v.quieting.Store(true)
		v.srv.SetPaused(true)
	}
}

func (v *View) QuietWait(deadline time.Time) int {
	if v.srv == nil {
		return 0
	}
	return v.srv.WaitQuiet(deadline)
}

func (v *View) Unquiet() {
	if v.srv != nil {
		v.quieting.Store(false)
		v.srv.SetPaused(false)
	}
}

func (v *View) start(boot string) {
	vm, ok := v.host.VM()
	if !ok || vm.BootID() != boot {
		return
	}
	client := vm.Agent()
	client.OnViewChanged(func(st agent.ViewStatus) { v.guestChanged(boot, st) })
	client.OnViewRemoved(func(r agent.ViewRemoved) { v.removed(r.Paths) })
	v.mu.Lock()
	if v.ended != boot {
		v.setLocked(api.ViewMounting)
	}
	v.mu.Unlock()
	v.publish()
	ctx, cancel := context.WithTimeout(context.Background(), viewCallBound)
	var isolated []string
	if v.Isolated != nil {
		isolated = v.Isolated()
	}
	name, group := macNames()
	st, err := client.ViewStart(ctx, agent.ViewStartParams{UID: uint32(os.Getuid()), GID: uint32(os.Getgid()), User: name, Group: group, Isolated: isolated})
	cancel()
	if err != nil {
		v.mu.Lock()
		if v.ended != boot {
			v.failLocked(fmt.Sprintf("the guest did not start its view: %v", err))
		}
		v.mu.Unlock()
		v.publish()
		return
	}
	v.guestChanged(boot, *st)
}

func (v *View) guestChanged(boot string, st agent.ViewStatus) {
	v.mu.Lock()
	current := v.boot == boot && v.ended != boot && v.stopping != boot
	mounted := v.mounted
	v.mu.Unlock()
	if !current {
		return
	}
	switch st.State {
	case agent.ViewReady:
		v.mu.Lock()
		v.vsockBoot, v.vsockPort = boot, st.VsockPort
		v.mu.Unlock()
		if !mounted {
			go v.mount(boot)
		}
	case agent.ViewFailed, agent.ViewStopped:
		why := "the guest's view stopped"
		if st.Error != "" {
			why = "the guest's view failed: " + st.Error
		}
		go func() {
			v.op.Lock()
			v.unmount(true)
			v.mu.Lock()
			if v.ended != boot {
				v.failLocked(why + "; it is mounted again at the VM's next start")
			}
			v.mu.Unlock()
			v.op.Unlock()
			v.publish()
		}()
	}
}

func (v *View) mount(boot string) {
	v.op.Lock()
	defer v.op.Unlock()
	v.mu.Lock()
	if v.mounted || v.boot != boot || v.ended == boot || v.stopping == boot {
		v.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), viewMountBound)
	v.cancelMount = cancel
	hidden := v.settingValue
	v.mu.Unlock()
	err := v.ready()
	if err == nil {
		t := time.Now()
		err = nfsview.Mount(ctx, v.endpoint, v.Dir, hidden)
		if err != nil && ctx.Err() == nil {
			v.log.Warn("view: the mount failed; trying once more in 1 s", "err", err)
			select {
			case <-time.After(viewMountAgain):
				t = time.Now()
				err = nfsview.Mount(ctx, v.endpoint, v.Dir, hidden)
			case <-ctx.Done():
			}
		}
		if err == nil {
			v.log.Info("view: mounted", "path", v.Dir, "hidden", hidden, "ms", time.Since(t).Milliseconds())
		}
	}
	cancel()
	v.mu.Lock()
	v.cancelMount = nil
	if v.ended == boot {
		v.mu.Unlock()
		if err == nil {
			nfsview.ForceUnmount(v.Dir)
		}
		return
	}
	if err != nil {
		var ne *nfsview.NotEmptyError
		if errors.As(err, &ne) || errors.Is(err, nfsview.ErrForeignMount) || strings.Contains(err.Error(), "is not a folder") {
			v.folderErr = err.Error()
		}
		v.failLocked(err.Error())
		v.mu.Unlock()
		v.publish()
		return
	}
	v.folderErr = ""
	k, kerr := nfsview.Keep(v.Dir, v.log)
	if kerr != nil {
		v.log.Warn("view: the mirror mounts are not held; the Mac's client unmounts each idle one every 120 s", "err", kerr)
	}
	v.mounted, v.keeper = true, k
	v.block.Hidden = hidden
	v.setLocked(api.ViewMounted)
	v.mu.Unlock()
	v.publish()
}

func (v *View) unmount(force bool) {
	v.mu.Lock()
	k := v.keeper
	v.keeper = nil
	mounted := v.mounted
	v.mounted = false
	v.mu.Unlock()
	if k != nil {
		k.Close()
	}
	if !mounted {
		if m, ok, _ := nfsview.Find(v.Dir); ok && m.View() && m.From == nfsview.Source(v.endpoint) {
			nfsview.ForceUnmount(v.Dir)
		}
		return
	}
	t := time.Now()
	var err error
	forced := force
	if force {
		err = nfsview.ForceUnmount(v.Dir)
	} else {
		forced, err = nfsview.Unmount(v.Dir)
	}
	if err != nil {
		v.log.Warn("view: the unmount failed", "path", v.Dir, "err", err)
	} else {
		v.log.Info("view: unmounted", "path", v.Dir, "forced", forced, "ms", time.Since(t).Milliseconds())
	}
	v.mu.Lock()
	if v.block.State != api.ViewFailed {
		v.setLocked(api.ViewUnmounted)
	}
	v.block.Hidden = v.settingValue
	v.mu.Unlock()
	v.publish()
}

func (v *View) BeforeStop(boot string) {
	if v.Dir == "" {
		return
	}
	v.mu.Lock()
	v.stopping = boot
	if v.cancelMount != nil {
		v.cancelMount()
	}
	v.mu.Unlock()
	done := make(chan struct{})
	go func() {
		v.op.Lock()
		v.unmount(false)
		v.op.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(viewUnmountBound):
		v.log.Warn("view: the unmount did not end in time; forcing it", "bound", viewUnmountBound)
		nfsview.ForceUnmount(v.Dir)
		select {
		case <-done:
		case <-time.After(viewUnmountBound):
		}
	}
	if vm, ok := v.host.VM(); ok && vm.BootID() == boot {
		ctx, cancel := context.WithTimeout(context.Background(), viewAliveBound*2)
		if _, err := vm.Agent().ViewStop(ctx); err != nil {
			v.log.Debug("view: view.stop", "err", err)
		}
		cancel()
	}
}

func (v *View) VMEnded(boot string) {
	if v.Dir == "" {
		return
	}
	v.mu.Lock()
	v.ended = boot
	if v.cancelMount != nil {
		v.cancelMount()
	}
	v.mu.Unlock()
	go func() {
		v.op.Lock()
		v.unmount(true)
		v.mu.Lock()
		if v.folderErr != "" {
			v.failLocked(v.folderErr)
		} else {
			v.setLocked(api.ViewUnmounted)
		}
		v.mu.Unlock()
		v.op.Unlock()
		v.publish()
	}()
}

func (v *View) stalled() bool {
	st := v.host.Status()
	if st.State != api.StateRunning {
		return false
	}
	vm, ok := v.host.VM()
	if !ok {
		return false
	}
	var wg sync.WaitGroup
	var vmmOK, guestOK bool
	var guestErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		v.host.CheckAlive()
		vmmOK = !slices.Contains(v.host.Status().Warnings, NotRespondingWarning)
	}()
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), viewAliveBound)
		defer cancel()
		s, err := vm.Agent().ViewStatus(ctx)
		guestOK = err == nil && s.State == agent.ViewReady
		guestErr = err
		if err == nil && !guestOK {
			guestErr = fmt.Errorf("its view is %s", s.State)
		}
	}()
	wg.Wait()
	if vmmOK && guestOK {
		v.mu.Lock()
		said := v.slowSaid
		v.slowSaid = true
		v.mu.Unlock()
		if !said {
			v.log.Warn("view: a request of the Mac's has waited; the VM and the guest answer, so the view is slow, not gone", "after", nfsview.StallAfter)
		}
		return true
	}
	why := "the VM did not answer"
	if vmmOK {
		why = fmt.Sprintf("the guest did not answer for its view (%v)", guestErr)
	}
	v.op.Lock()
	v.unmount(true)
	v.mu.Lock()
	v.failLocked(fmt.Sprintf("a request of the Mac's had nothing back for %v and %s: the view was unmounted so that nothing on the Mac waits; it is mounted again at the VM's next start", nfsview.StallAfter, why))
	v.mu.Unlock()
	v.op.Unlock()
	v.publish()
	return false
}

func (v *View) ConfigChanged(ch api.ConfigChange) {
	if v.Dir == "" || ch.Key != api.KeyFilesViewHidden {
		return
	}
	hidden, ok := ch.Value.(bool)
	if !ok {
		return
	}
	v.mu.Lock()
	v.settingValue = hidden
	mounted := v.mounted
	if !mounted {
		v.block.Hidden = hidden
	}
	v.mu.Unlock()
	if !mounted {
		v.publish()
		return
	}
	go func() {
		v.op.Lock()
		defer v.op.Unlock()
		v.mu.Lock()
		still := v.mounted && v.settingValue == hidden
		v.mu.Unlock()
		if !still {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), viewCallBound)
		err := nfsview.SetHidden(ctx, v.endpoint, v.Dir, hidden)
		cancel()
		v.mu.Lock()
		if err != nil {
			v.log.Warn("view: files.view_hidden was not applied to the mount; it is at the next mount", "err", err)
		} else {
			v.block.Hidden = hidden
			v.log.Info("view: files.view_hidden applied to the mount", "hidden", hidden)
		}
		v.mu.Unlock()
		v.publish()
	}()
}

func (v *View) Event(event any) {
	if v.Dir == "" {
		return
	}
	v.removed(goneFromView(event))
}

var viewForget = (*nfsview.Keeper).ForgetUnder

func (v *View) removed(paths []string) {
	v.mu.Lock()
	k := v.keeper
	v.mu.Unlock()
	if k == nil {
		return
	}
	for _, rel := range paths {
		if path, was := viewForget(k, rel); was {
			v.log.Debug("view: let go of a mirror mount whose object went", "path", path)
			go unix.Unmount(path, 0)
		}
	}
}

func (v *View) MachinePath(machine string) string {
	if v == nil || v.Dir == "" || machine == "" || strings.ContainsAny(machine, "/\x00") {
		return ""
	}
	v.mu.Lock()
	mounted := v.mounted
	v.mu.Unlock()
	if !mounted || (v.Isolated != nil && slices.Contains(v.Isolated(), machine)) {
		return ""
	}
	return filepath.Join(v.Dir, machine)
}

func goneFromView(event any) []string {
	name := func(s string) string { return strings.ReplaceAll(strings.TrimPrefix(s, "/"), "/", "_") }
	switch e := event.(type) {
	case api.DockerEvent:
		switch {
		case e.Type == "container" && (e.Action == "die" || e.Action == "destroy"):
			if n := e.Name; n != "" {
				return []string{"docker/containers/" + name(n)}
			}
			if n := e.Attributes["name"]; n != "" {
				return []string{"docker/containers/" + name(n)}
			}
		case e.Type == "container" && e.Action == "rename":
			if n := e.Attributes["oldName"]; n != "" {
				return []string{"docker/containers/" + name(n)}
			}
		case e.Type == "volume" && e.Action == "destroy":
			return []string{"docker/volumes/" + name(e.ID)}
		case e.Type == "image" && (e.Action == "delete" || e.Action == "untag"):
			var out []string
			for _, n := range []string{e.Name, e.Attributes["name"]} {
				if n != "" && !slices.Contains(out, "docker/images/"+name(n)) {
					out = append(out, "docker/images/"+name(n))
				}
			}
			return out
		}
	case api.MachinesChanged:
		if e.Removed && e.Machine.Name != "" {
			return []string{name(e.Machine.Name)}
		}
	}
	return nil
}

func (v *View) Close() {
	if v.Dir == "" {
		return
	}
	v.op.Lock()
	v.unmount(true)
	v.op.Unlock()
	if v.srv != nil {
		v.srv.Close()
	}
	if v.stopGuard != nil {
		v.stopGuard()
	}
}

func macNames() (name, group string) {
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if g, err := user.LookupGroupId(strconv.Itoa(os.Getgid())); err == nil {
		group = g.Name
	}
	return name, group
}
