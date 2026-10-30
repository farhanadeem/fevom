package machines

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	policy "fev/guest/internal/emulation"
	"fev/guest/internal/logbuf"
	"fev/guest/internal/reaper"
	"fev/guest/storage"
)

type Options struct {
	Emulation                  func() policy.Config
	DataDir, CgroupDir, RunDir string
	Self                       string
	Dev, SysBlock              string
	Reaper                     *reaper.Reaper
	Log                        *logbuf.Buffer
	Notify                     func(api.MachineState)
	Network                    Network
	View                       string
	Helpers                    string
	AgentPath                  string
	Resolver                   string
}

const (
	readyBound = 2 * time.Minute
	killWait   = 5 * time.Second
	trueBound  = 30 * time.Second
)

type Runtime struct {
	o Options

	mu               sync.Mutex
	why              error
	closed           bool
	majors           []uint32
	devs             []devNode
	byName           map[string]*machine
	nextID           uint64
	st               *storage.Machines
	runNames, making map[string]*machine
	rlogMu           sync.Mutex
	rlogs            map[string]*logbuf.Buffer
	trusted          *api.Trusted
	agentMu          sync.Mutex
	agentPath        string
}

func New(o Options) *Runtime {
	if o.Network == nil {
		o.Network = plugVeth{}
	}
	rt := &Runtime{o: o, why: errors.New("machines are not available yet: the guest is starting"), byName: map[string]*machine{}, runNames: map[string]*machine{}, making: map[string]*machine{}, agentPath: o.AgentPath}
	rt.followRuntimeLines()
	return rt
}

func (rt *Runtime) Unavailable(why error) {
	rt.mu.Lock()
	rt.why = fmt.Errorf("machines are not available: %w", why)
	rt.mu.Unlock()
}

func (rt *Runtime) Open(st *storage.Machines) error {
	err := func() error {
		if st == nil {
			return errors.New("the data filesystem holds no machines")
		}
		if st, err := os.Stat(rt.o.DataDir); err != nil || !st.IsDir() {
			return fmt.Errorf("no %s: %v", rt.o.DataDir, err)
		}
		if err := delegate(rt.o.CgroupDir); err != nil {
			return fmt.Errorf("delegate controllers to the machines' groups: %w", err)
		}
		majors, err := diskMajors(rt.o.SysBlock)
		if err != nil {
			return err
		}
		if z, ok := blockMajor(procDevices, "zram"); ok && !slicesContains(majors, z) {
			majors = append(majors, z)
		}
		raiseInotifyInstances(rt.o.Log)
		devs, missing := devNodes(rt.o.Dev)
		if len(missing) > 0 {
			rt.o.Log.Logf(logbuf.Warning, "machines: the VM has no %s; machines have none either", strings.Join(missing, ", "))
		}
		if err := os.MkdirAll(rt.o.RunDir, 0o711); err != nil {
			return err
		}
		if err := os.Chmod(rt.o.RunDir, 0o711); err != nil {
			return err
		}
		rt.mu.Lock()
		rt.majors, rt.devs, rt.st = majors, devs, st
		rt.mu.Unlock()
		rt.tidy()
		return nil
	}()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if err != nil {
		rt.why = fmt.Errorf("machines are not available: %w", err)
		return err
	}
	rt.why = nil
	return nil
}

const inotifyInstances = 8192

func raiseInotifyInstances(log *logbuf.Buffer) {
	const p = "/proc/sys/fs/inotify/max_user_instances"
	b, err := os.ReadFile(p)
	if err != nil {
		log.Logf(logbuf.Warning, "machines: %s: %v", p, err)
		return
	}
	var have int
	fmt.Sscan(strings.TrimSpace(string(b)), &have)
	if have >= inotifyInstances {
		return
	}
	if err := os.WriteFile(p, []byte(fmt.Sprint(inotifyInstances)), 0); err != nil {
		log.Logf(logbuf.Warning, "machines: cannot raise %s from %d: %v", p, have, err)
	}
}

func (rt *Runtime) available() error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.why
}

type machine struct {
	rt   *Runtime
	name string

	opMu sync.Mutex

	mu              sync.Mutex
	state           string
	pending         *Record
	removing        bool
	last, err, init string
	cur             *run
	started         float64
	readyAt         float64
	clog            *consoleLog
}

func (rt *Runtime) get(name string) *machine {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	m := rt.byName[name]
	if m == nil {
		m = &machine{rt: rt, name: name, state: api.MachineStopped}
		rt.byName[name] = m
	}
	return m
}

func (m *machine) Name() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.name
}

func (m *machine) info(rec *Record) api.MachineInfo {
	m.mu.Lock()
	i := m.infoLocked(rec)
	m.mu.Unlock()
	if i.State != api.MachineCreating {
		i.DiskBytes = m.rt.diskBytes(i.Name)
	}
	return i
}

func (m *machine) infoLocked(rec *Record) api.MachineInfo {
	i := api.MachineInfo{Name: m.name, State: m.state, LastExit: m.last, Init: m.init}
	if rec == nil {
		rec = m.pending
	}
	if rec != nil {
		i.UUID, i.Image = rec.UUID, rec.Image
	}
	if m.state == api.MachineFailed {
		i.Error = m.err
	}
	if r := m.cur; r != nil {
		if r.amd64 {
			i.Emulation, i.EmulationReason = r.emulation.Engine, r.emulation.Reason
		}
		if b := r.boot(); b != nil {
			i.PID = b.child.Pid
		}
		i.Address, i.Address6 = r.address(), r.address6()
		i.StartedMS, i.ReadyMS = m.started, m.readyAt
	}
	return i
}

func (m *machine) setState(state string, rec *Record) {
	m.mu.Lock()
	m.state = state
	i := m.infoLocked(rec)
	m.mu.Unlock()
	if m.rt.o.Notify != nil {
		m.rt.o.Notify(api.MachineState{MachineInfo: i})
	}
}

func (rt *Runtime) Info(name string) (api.MachineInfo, error) {
	if err := rt.available(); err != nil {
		return api.MachineInfo{}, err
	}
	rec, err := ReadRecord(rt.o.DataDir, name)
	if err != nil {
		if m := rt.creating(name); m != nil {
			return m.info(nil), nil
		}
		return api.MachineInfo{}, err
	}
	return rt.get(name).info(rec), nil
}

func (rt *Runtime) creating(name string) *machine {
	rt.mu.Lock()
	m := rt.byName[name]
	rt.mu.Unlock()
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != api.MachineCreating {
		return nil
	}
	return m
}

func (rt *Runtime) List() (api.MachineList, error) {
	l := api.MachineList{Machines: []api.MachineInfo{}, Images: []api.ImageInfo{}}
	if err := rt.available(); err != nil {
		return l, err
	}
	names, err := Names(rt.o.DataDir)
	if err != nil {
		return l, err
	}
	sort.Strings(names)
	for _, name := range names {
		rec, err := ReadRecord(rt.o.DataDir, name)
		if err != nil {
			rt.o.Log.Logf(logbuf.Warning, "machines: %v", err)
			continue
		}
		l.Machines = append(l.Machines, rt.get(name).info(rec))
	}
	rt.mu.Lock()
	var made []*machine
	for _, m := range rt.byName {
		made = append(made, m)
	}
	rt.mu.Unlock()
	for _, m := range made {
		if i := m.info(nil); i.State == api.MachineCreating {
			l.Machines = append(l.Machines, i)
		}
	}
	sort.Slice(l.Machines, func(a, b int) bool { return l.Machines[a].Name < l.Machines[b].Name })
	images, err := rt.storage().Images()
	if err != nil {
		rt.o.Log.Logf(logbuf.Warning, "machines: the image builds: %v", err)
	}
	for _, im := range images {
		if _, ok := ReadImageTemplates(rt.o.DataDir, im.ImageRef); ok {
			l.Images = append(l.Images, im)
		}
	}
	sort.Slice(l.Images, func(a, b int) bool {
		x, y := l.Images[a].ImageRef, l.Images[b].ImageRef
		return x.Distro+"/"+x.Release+"/"+x.Variant+"/"+x.Serial < y.Distro+"/"+y.Release+"/"+y.Variant+"/"+y.Serial
	})
	return l, nil
}

type run struct {
	emulation policy.Decision
	amd64     bool
	m         *machine
	rec       *Record
	cfg       api.MachineConfig
	kind      string
	name      string

	group, payload, dir string

	stopOnce  sync.Once
	stopping  chan struct{}
	done      chan struct{}
	readyOnce sync.Once
	ready     chan struct{}

	notify *net.UnixConn
	sessMu sync.Mutex

	mu    sync.RWMutex
	cur   *boot
	first *firstBoot
}

type boot struct {
	child                  *reaper.Child
	nsMu                   sync.RWMutex
	ns                     map[string]*os.File
	readyOnce, signalsOnce sync.Once
	ready, signals         chan struct{}
	ended                  chan struct{}
	console                *console
	net                    Plugged
	netErr                 error
	agentMu                sync.Mutex
	agentAt                string
	agentKnown, agentBound bool
}

func (r *run) boot() *boot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cur
}

func (r *run) address() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.cur == nil {
		return ""
	}
	return r.cur.net.Address
}

func (r *run) address6() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.cur == nil || r.cur.net.Address6 == nil {
		return ""
	}
	return r.cur.net.Address6()
}

func (m *machine) restate() {
	m.mu.Lock()
	if m.cur == nil {
		m.mu.Unlock()
		return
	}
	i := m.infoLocked(m.cur.rec)
	m.mu.Unlock()
	if m.rt.o.Notify != nil {
		m.rt.o.Notify(api.MachineState{MachineInfo: i})
	}
}

func (r *run) asked() bool {
	select {
	case <-r.stopping:
		return true
	default:
		return false
	}
}

func (rt *Runtime) Start(name string, cfg api.MachineConfig) (api.MachineInfo, error) {
	if err := rt.available(); err != nil {
		return api.MachineInfo{}, err
	}
	rec, err := ReadRecord(rt.o.DataDir, name)
	if err != nil {
		return api.MachineInfo{}, err
	}
	m := rt.get(name)
	for {
		rt.mu.Lock()
		closed := rt.closed
		rt.mu.Unlock()
		if closed {
			return api.MachineInfo{}, errors.New("the guest is shutting down")
		}
		m.mu.Lock()
		r := m.cur
		if r == nil && (m.removing || m.state == api.MachineCreating) {
			st := m.state
			if m.removing {
				st = "being deleted"
			}
			m.mu.Unlock()
			return api.MachineInfo{}, fmt.Errorf("%s is %s", name, st)
		}
		if r == nil {
			r, err = m.begin(rec, cfg)
			m.mu.Unlock()
			if err != nil {
				return m.info(rec), err
			}
			return m.waitReady(r, rec)
		}
		m.mu.Unlock()
		if r.asked() {
			<-r.done
			continue
		}
		return m.waitReady(r, rec)
	}
}

func (m *machine) waitReady(r *run, rec *Record) (api.MachineInfo, error) {
	select {
	case <-r.ready:
		if f := r.firstStep(); f != nil {
			select {
			case <-f.done:
			case <-r.done:
			case <-time.After(firstBootBound):
				return m.info(rec), fmt.Errorf("the first boot's online step of %s has not ended after %v; it goes on", m.Name(), firstBootBound)
			}
		}
		return m.info(rec), nil
	case <-r.done:
		i := m.info(rec)
		if i.State == api.MachineFailed {
			return i, fmt.Errorf("%s failed to start: %s", i.Name, i.Error)
		}
		return i, nil
	case <-time.After(readyBound):
		return m.info(rec), fmt.Errorf("%s has not said it is ready after %v; it goes on starting", m.Name(), readyBound)
	}
}

func (m *machine) begin(rec *Record, cfg api.MachineConfig) (*run, error) {
	rt := m.rt
	rt.mu.Lock()
	other := rt.runNames[m.name]
	rt.mu.Unlock()
	if other != nil && other != m {
		return nil, fmt.Errorf("a running machine was called %s when it started (%s now): %s cannot start until it stops", m.name, other.Name(), m.name)
	}
	if err := api.CheckIsolation(cfg); err != nil {
		err = fmt.Errorf("the machine's isolation: %w", err)
		m.state, m.err, m.last = api.MachineFailed, err.Error(), ""
		return nil, err
	}
	root := filepath.Join(rt.o.DataDir, m.name)
	kind, err := InitKind(root)
	if err != nil {
		m.state, m.err, m.last = api.MachineFailed, err.Error(), ""
		return nil, err
	}
	r := &run{
		m: m, rec: rec, cfg: cfg, kind: kind,
		name:     m.name,
		group:    filepath.Join(rt.o.CgroupDir, m.name),
		dir:      filepath.Join(rt.o.RunDir, m.name),
		stopping: make(chan struct{}), done: make(chan struct{}), ready: make(chan struct{}),
	}
	r.payload = filepath.Join(r.group, "payload")
	if rt.o.Emulation != nil {
		r.emulation, r.amd64, err = selectEmulation(rt.o.Emulation(), rec, cfg, root)
		if err != nil {
			err = fmt.Errorf("the machine's x86 engine: %w", err)
			m.state, m.err, m.last = api.MachineFailed, err.Error(), ""
			return nil, err
		}
	}
	if err := r.prepare(); err != nil {
		r.cleanup()
		m.state, m.err, m.last = api.MachineFailed, err.Error(), ""
		return nil, err
	}
	m.cur, m.init, m.err = r, kind, ""
	rt.mu.Lock()
	rt.runNames[r.name] = m
	rt.mu.Unlock()
	m.started, m.readyAt = monotonicMS(), 0
	m.state = api.MachineStarting
	i := m.infoLocked(rec)
	if rt.o.Notify != nil {
		rt.o.Notify(api.MachineState{MachineInfo: i})
	}
	go r.supervise()
	return r, nil
}

func selectEmulation(profile policy.Config, rec *Record, cfg api.MachineConfig, root string) (policy.Decision, bool, error) {
	override := cfg.Emulation
	if profile.Mode == policy.FEX && (override == "" || override == policy.Auto) {
		return policy.Decision{}, false, nil
	}
	facts, err := policy.Inspect(root, []string{MachineInit}, nil)
	if err != nil {
		return policy.Decision{}, false, err
	}
	facts.Machine = true
	facts.AMD64 = facts.AMD64 || rec.Image.Arch == "amd64"
	if !facts.AMD64 {
		if profile.Mode == policy.FEX {
			return policy.Decision{}, false, nil
		}
		override = ""
	}
	d, err := policy.Select(profile, override, facts)
	return d, facts.AMD64, err
}

func (r *run) prepare() error {
	rt := r.m.rt
	if isPopulated(r.group) {
		rt.o.Log.Logf(logbuf.Warning, "machines: %s: processes were left in its group; killing them", r.name)
		if !killGroup(r.group, killWait) {
			return fmt.Errorf("processes left in %s will not die", r.group)
		}
	}
	if err := removeTree(r.group); err != nil {
		return err
	}
	if err := os.Mkdir(r.group, 0o755); err != nil {
		return err
	}
	if err := delegate(r.group); err != nil {
		return err
	}
	if err := os.Mkdir(r.payload, 0o755); err != nil {
		return err
	}
	if r.cfg.Isolated {
		if err := delegateTo(r.payload, isoBase, isoBase); err != nil {
			return err
		}
	}
	rt.mu.Lock()
	majors := rt.majors
	rt.mu.Unlock()
	if err := attachDeviceProgram(r.group, majors); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(r.dir, "run"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(r.dir, "root"), 0o755); err != nil {
		return err
	}
	notifyPath := filepath.Join(r.dir, "notify")
	os.Remove(notifyPath)
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: notifyPath, Net: "unixgram"})
	if err != nil {
		return err
	}
	raw, err := c.SyscallConn()
	if err == nil {
		raw.Control(func(fd uintptr) {
			err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PASSCRED, 1)
		})
	}
	if err != nil {
		c.Close()
		return err
	}
	if r.cfg.Isolated {
		if err := os.Chown(notifyPath, isoBase, isoBase); err != nil {
			c.Close()
			return err
		}
	}
	r.notify = c
	go r.serveNotify()
	return nil
}

func (r *run) serveNotify() {
	buf := make([]byte, 4096)
	oob := make([]byte, unix.CmsgSpace(unix.SizeofUcred))
	for {
		n, oobn, _, _, err := r.notify.ReadMsgUnix(buf, oob)
		if err != nil {
			return
		}
		sender := -1
		if msgs, err := unix.ParseSocketControlMessage(oob[:oobn]); err == nil {
			for _, msg := range msgs {
				if cred, err := unix.ParseUnixCredentials(&msg); err == nil {
					sender = int(cred.Pid)
				}
			}
		}
		b := r.boot()
		if b == nil || sender != b.child.Pid {
			continue
		}
		for _, line := range strings.Split(string(buf[:n]), "\n") {
			switch line {
			case "READY=1":
				b.readyOnce.Do(func() { close(b.ready) })
			case "X_SYSTEMD_SIGNALS_LEVEL=2":
				b.signalsOnce.Do(func() { close(b.signals) })
			}
		}
	}
}

func (r *run) supervise() {
	m, rt := r.m, r.m.rt
	end := ending{state: api.MachineFailed}
	why := ""
	for n := 1; ; n++ {
		if r.asked() {
			end = ending{state: api.MachineStopped, last: api.ExitStopped}
			break
		}
		ws, tail, err := r.bootOnce(n)
		if err != nil {
			end, why = ending{state: api.MachineFailed, last: ""}, err.Error()
			rt.o.Log.Logf(logbuf.Error, "machines: %s: %v", m.Name(), err)
			break
		}
		end = endOf(ws.Signaled(), int(ws.Signal()), ws.ExitStatus(), r.asked())
		rt.o.Log.Logf(logbuf.Info, "machines: %s: its init ended (%s)", m.Name(), describe(ws))
		if end.state == api.MachineFailed {
			why = fmt.Sprintf("its init ended with %s", end.last)
			if t := lastLines(tail, 3); t != "" {
				why += "; the console said: " + t
			}
		}
		if !end.again {
			break
		}
		m.mu.Lock()
		m.last = end.last
		m.mu.Unlock()
		m.setState(api.MachineStopping, r.rec)
		m.setState(api.MachineStarting, r.rec)
	}
	r.cleanup()
	m.mu.Lock()
	m.cur = nil
	m.state, m.last, m.err = end.state, end.last, why
	m.started, m.readyAt = 0, 0
	m.mu.Unlock()
	rt.mu.Lock()
	if rt.runNames[r.name] == m {
		delete(rt.runNames, r.name)
	}
	rt.mu.Unlock()
	m.setState(end.state, r.rec)
	close(r.done)
}

func stageAttr(isolated bool, payload int) *syscall.SysProcAttr {
	a := &syscall.SysProcAttr{
		Cloneflags:   unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC | unix.CLONE_NEWNET,
		UseCgroupFD:  true,
		CgroupFD:     payload,
		Unshareflags: unix.CLONE_NEWCGROUP,
	}
	if isolated {
		a.Cloneflags |= unix.CLONE_NEWUSER
		a.UidMappings, a.GidMappings = isoMaps, isoMaps
		a.GidMappingsEnableSetgroups = true
		a.Credential = &syscall.Credential{Uid: 0, Gid: 0, NoSetGroups: true}
	}
	return a
}

func describe(ws unix.WaitStatus) string {
	if ws.Signaled() {
		return fmt.Sprintf("signal %d, %s", ws.Signal(), unix.SignalName(ws.Signal()))
	}
	return fmt.Sprintf("exit %d", ws.ExitStatus())
}

func (r *run) cleanup() {
	rt := r.m.rt
	if !killGroup(r.group, killWait) {
		rt.o.Log.Logf(logbuf.Error, "machines: %s: processes are left in its group after the kill", r.m.Name())
	}
	if err := removeTree(r.group); err != nil {
		rt.o.Log.Logf(logbuf.Warning, "machines: %s: %v", r.m.Name(), err)
	}
	if r.notify != nil {
		r.notify.Close()
	}
	os.Remove(filepath.Join(r.dir, "notify"))
}

func (r *run) bootOnce(n int) (unix.WaitStatus, string, error) {
	m, rt := r.m, r.m.rt
	if n > 1 {
		if !killGroup(r.payload, killWait) {
			return 0, "", errors.New("processes of the last boot will not die")
		}
		if err := removeTree(r.payload); err != nil {
			return 0, "", err
		}
		if err := os.Mkdir(r.payload, 0o755); err != nil {
			return 0, "", err
		}
		if r.cfg.Isolated {
			if err := delegateTo(r.payload, isoBase, isoBase); err != nil {
				return 0, "", err
			}
		}
	}
	if r.emulation.Engine != "" {
		if err := policy.SetCgroup(r.payload, r.emulation.Engine); err != nil {
			return 0, "", err
		}
	}
	con, err := openConsole(m.consoleLog())
	if err != nil {
		return 0, "", fmt.Errorf("its console: %w", err)
	}
	defer con.close()
	rt.mu.Lock()
	devs := rt.devs
	rt.mu.Unlock()
	spec := stageSpec{
		Name: m.Name(), UUID: r.rec.UUID,
		Subvolume: filepath.Join(rt.o.DataDir, m.Name()), Root: filepath.Join(r.dir, "root"),
		Console: con.slave, Notify: filepath.Join(r.dir, "notify"), Run: filepath.Join(r.dir, "run"),
		Devices: devs, Init: MachineInit, Share: r.macFiles(),
		View: rt.o.View, Keygen: r.rec.Keygen, Helpers: rt.o.Helpers,
		Agent: rt.AgentPath(), Resolver: rt.o.Resolver,
	}
	isoRoot := ""
	if r.cfg.Isolated {
		spec.Isolated, spec.Dev = true, rt.o.Dev
		spec.View, spec.Helpers, spec.Agent = "", "", ""
		var kept []devNode
		for _, d := range devs {
			if isoDevices[d.Path] {
				kept = append(kept, d)
			}
		}
		spec.Devices = kept
		if err := os.Chown(con.slave, isoBase, isoBase+5); err != nil {
			return 0, "", fmt.Errorf("give its console to its user namespace: %w", err)
		}
		said, err := rootFor(rt.o.Reaper, spec.Subvolume, spec.Root, r.cfg.Mounts)
		if err != nil {
			return 0, "", err
		}
		isoRoot = spec.Root
		defer func() {
			if isoRoot != "" {
				unix.Unmount(isoRoot, unix.MNT_DETACH)
			}
		}()
		for _, l := range said {
			r.m.runtimeLogf(logbuf.Warning, "a named mount is left out: %s", l)
		}
	}
	spec.Trust, _ = rt.trustForStage()
	if r.rec.Setup {
		u := r.rec.User
		spec.Setup = &u
	}
	specR, specW, err := os.Pipe()
	if err != nil {
		return 0, "", err
	}
	defer specR.Close()
	b, _ := json.Marshal(spec)
	_, err = specW.Write(b)
	specW.Close()
	if err != nil {
		return 0, "", err
	}
	goR, goW, err := os.Pipe()
	if err != nil {
		return 0, "", err
	}
	defer goW.Close()
	execR, execW, err := os.Pipe()
	if err != nil {
		goR.Close()
		return 0, "", err
	}
	defer execR.Close()
	payload, err := os.Open(r.payload)
	if err != nil {
		goR.Close()
		execW.Close()
		return 0, "", err
	}
	slave, err := os.OpenFile(con.slave, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		payload.Close()
		goR.Close()
		execW.Close()
		return 0, "", err
	}
	child, err := rt.o.Reaper.Start(rt.o.Self, []string{StageName}, &os.ProcAttr{
		Dir:   "/",
		Env:   []string{},
		Files: []*os.File{slave, slave, slave, specR, goR, execW},
		Sys:   stageAttr(r.cfg.Isolated, int(payload.Fd())),
	})
	if isoRoot != "" {
		unix.Unmount(isoRoot, unix.MNT_DETACH)
		isoRoot = ""
	}
	slave.Close()
	payload.Close()
	goR.Close()
	execW.Close()
	if err != nil {
		return 0, con.Tail(), fmt.Errorf("start its first stage: %w", err)
	}
	b2 := &boot{child: child, ns: map[string]*os.File{}, ready: make(chan struct{}), signals: make(chan struct{}), ended: make(chan struct{}), console: con}
	if agentPathInMachine(spec.Agent) {
		b2.agentAt = spec.Agent
	}
	kinds := []string{"mnt", "pid", "uts", "ipc", "net", "cgroup"}
	if r.cfg.Isolated {
		kinds = append(kinds, "user")
	}
	for _, kind := range kinds {
		f, err := os.Open(fmt.Sprintf("/proc/%d/ns/%s", child.Pid, kind))
		if err != nil {
			break
		}
		b2.ns[kind] = f
	}
	r.mu.Lock()
	r.cur = b2
	r.mu.Unlock()
	defer func() {
		close(b2.ended)
		r.mu.Lock()
		r.cur = nil
		r.mu.Unlock()
		b2.nsMu.Lock()
		for _, f := range b2.ns {
			f.Close()
		}
		b2.ns = nil
		b2.nsMu.Unlock()
	}()
	if len(b2.ns) == len(kinds) {
		plug := Plug{
			Name: m.Name(), UUID: r.rec.UUID, MAC: r.cfg.MAC, Address: r.cfg.Address, User: r.cfg.User,
			Root: filepath.Join(rt.o.DataDir, m.Name()), Run: filepath.Join(r.dir, "run"),
			NetNS:           int(b2.ns["net"].Fd()),
			Address6Changed: m.restate,
		}
		if r.cfg.Isolated {
			plug.Isolated, plug.Agent, plug.IDBase = true, r.cfg.ForwardSSHAgent, isoBase
			plug.IsolateNetwork = r.cfg.IsolateNetwork
			if !r.cfg.ForwardSSHAgent {
				plug.Run = ""
			}
		}
		pl, err := rt.o.Network.Plug(plug)
		if err != nil {
			rt.o.Log.Logf(logbuf.Warning, "machines: %s: no network: %v", m.Name(), err)
			r.mu.Lock()
			b2.netErr = err
			r.mu.Unlock()
		} else {
			r.mu.Lock()
			b2.net = pl
			r.mu.Unlock()
			defer pl.Unplug()
		}
		goW.Write([]byte{1})
	} else {
		rt.o.Log.Logf(logbuf.Error, "machines: %s: its namespaces could not be opened", m.Name())
		rt.o.Reaper.Signal(child, unix.SIGKILL, false)
	}
	goW.Close()
	go r.watchReady(b2, execR)
	ws := child.Wait()
	return ws, con.Tail(), nil
}

func (r *run) watchReady(b *boot, execR *os.File) {
	m := r.m
	said, _ := io.ReadAll(execR)
	if strings.ContainsRune(string(said), stageSetupDone) && r.rec.Setup {
		rec := *r.rec
		rec.Setup, rec.Online = false, true
		if err := WriteRecord(m.rt.o.DataDir, &rec); err != nil {
			m.rt.o.Log.Logf(logbuf.Error, "machines: %s: its setup is done and cannot be recorded: %v", m.Name(), err)
		} else {
			r.rec.Setup, r.rec.Online = false, true
			m.rt.o.Log.Logf(logbuf.Info, "machines: %s: the first boot's setup made %s (uid %d)", m.Name(), rec.User.Name, rec.User.UID)
		}
	}
	if strings.ContainsRune(string(said), stageKeysMade) && r.rec.Keygen {
		rec := *r.rec
		rec.Keygen = false
		if err := WriteRecord(m.rt.o.DataDir, &rec); err != nil {
			m.rt.o.Log.Logf(logbuf.Error, "machines: %s: its new SSH host keys are made and cannot be recorded: %v", m.Name(), err)
		} else {
			r.rec.Keygen = false
			m.rt.o.Log.Logf(logbuf.Info, "machines: %s: new SSH host keys made", m.Name())
		}
	}
	r.trustSaid(string(said))
	if r.kind != InitSystemd {
		if err := r.runTrue(b); err != nil {
			m.rt.o.Log.Logf(logbuf.Warning, "machines: %s: no command ran in it: %v", m.Name(), err)
			return
		}
		b.readyOnce.Do(func() { close(b.ready) })
	}
	select {
	case <-b.ready:
	case <-b.ended:
		return
	}
	want := m.rt.AgentPath()
	if !agentPathInMachine(want) {
		want = ""
	}
	b.agentMu.Lock()
	moved := b.agentAt != want
	b.agentMu.Unlock()
	if moved {
		r.followAgent(b)
	}
	m.mu.Lock()
	m.readyAt = monotonicMS()
	m.mu.Unlock()
	m.setState(api.MachineRunning, r.rec)
	if r.rec.Online {
		r.startFirstBoot(b)
	}
	r.readyOnce.Do(func() { close(r.ready) })
}

func (r *run) runTrue(b *boot) error {
	rt := r.m.rt
	var child *reaper.Child
	err := r.in(b, func(cgroup int) error {
		if r.cfg.Isolated {
			null, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
			if err != nil {
				return err
			}
			defer null.Close()
			child, err = b.startIsolated(rt.o.Reaper, rt.o.Self, "/bin/true", []string{"true"}, []string{"PATH=" + machinePath},
				[3]*os.File{null, null, null}, &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: cgroup})
			return err
		}
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			return err
		}
		if err := unix.Setns(int(b.ns["mnt"].Fd()), unix.CLONE_NEWNS); err != nil {
			return fmt.Errorf("enter the machine's mount namespace: %w", err)
		}
		null, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer null.Close()
		child, err = rt.o.Reaper.Start("/bin/true", []string{"true"}, &os.ProcAttr{
			Dir: "/", Env: []string{"PATH=" + machinePath}, Files: []*os.File{null, null, null},
			Sys: &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: cgroup},
		})
		if err != nil {
			err = fmt.Errorf("%w (%s)", err, whyNoStart(cgroup))
		}
		return err
	})
	if err != nil {
		return err
	}
	done := make(chan unix.WaitStatus, 1)
	go func() { done <- child.Wait() }()
	select {
	case ws := <-done:
		if ws.ExitStatus() != 0 || ws.Signaled() {
			return fmt.Errorf("/bin/true: %s", describe(ws))
		}
		return nil
	case <-time.After(trueBound):
		rt.o.Reaper.Signal(child, unix.SIGKILL, false)
		return fmt.Errorf("/bin/true did not end in %v", trueBound)
	}
}

func whyNoStart(cgroup int) string {
	var said []string
	if fd, err := unix.Openat(cgroup, "cgroup.type", unix.O_RDONLY|unix.O_CLOEXEC, 0); err != nil {
		said = append(said, fmt.Sprintf("the session's group: %v", err))
	} else {
		unix.Close(fd)
		said = append(said, "the session's group is there")
	}
	for _, p := range []string{"/bin/true", "/lib/ld-musl-aarch64.so.1", "/etc/os-release"} {
		var st unix.Stat_t
		if err := unix.Stat(p, &st); err != nil {
			said = append(said, fmt.Sprintf("%s: %v", p, err))
		} else {
			said = append(said, p+" is there")
		}
	}
	if exe, err := os.Readlink("/proc/1/exe"); err != nil {
		said = append(said, fmt.Sprintf("PID 1: %v", err))
	} else {
		said = append(said, "PID 1 is "+exe)
	}
	return strings.Join(said, "; ")
}

func haltSignal(kind string) unix.Signal {
	switch kind {
	case InitSystemd:
		return unix.Signal(37)
	case InitBusybox:
		return unix.SIGUSR2
	case InitRunit:
		return unix.SIGCONT
	default:
		return unix.SIGPWR
	}
}

func (rt *Runtime) Stop(name string, grace time.Duration, force bool) (api.MachineInfo, error) {
	if err := rt.available(); err != nil {
		return api.MachineInfo{}, err
	}
	rec, err := ReadRecord(rt.o.DataDir, name)
	if err != nil {
		return api.MachineInfo{}, err
	}
	m := rt.get(name)
	m.stop(grace, force)
	return m.info(rec), nil
}

func (m *machine) stop(grace time.Duration, force bool) {
	m.mu.Lock()
	r := m.cur
	m.mu.Unlock()
	if r == nil {
		return
	}
	first := false
	r.stopOnce.Do(func() {
		close(r.stopping)
		first = true
	})
	if first {
		m.setState(api.MachineStopping, r.rec)
	}
	deadline := time.Now().Add(grace)
	if !force {
		r.halt(deadline)
		select {
		case <-r.done:
			return
		case <-time.After(time.Until(deadline)):
		}
		m.rt.o.Log.Logf(logbuf.Warning, "machines: %s did not halt within %v; killing it", m.Name(), grace)
	}
	killGroup(r.group, killWait)
	<-r.done
}

func (r *run) halt(deadline time.Time) {
	b := r.boot()
	if b == nil {
		return
	}
	if r.kind == InitSystemd {
		select {
		case <-b.ready:
		case <-b.signals:
		case <-r.done:
			return
		case <-time.After(time.Until(deadline)):
			return
		}
	}
	if r.kind == InitRunit {
		if err := b.markRunitHaltIn(); err != nil {
			r.m.runtimeLogf(logbuf.Warning, "runit's halt marks: %v; its init may not halt by itself", err)
		}
	}
	r.m.rt.o.Reaper.Signal(b.child, haltSignal(r.kind), false)
}

func (rt *Runtime) Restart(name string, grace time.Duration, cfg api.MachineConfig) (api.MachineInfo, error) {
	if _, err := rt.Stop(name, grace, false); err != nil {
		return api.MachineInfo{}, err
	}
	return rt.Start(name, cfg)
}

func (rt *Runtime) Shutdown(grace time.Duration) {
	rt.mu.Lock()
	rt.closed = true
	all := make([]*machine, 0, len(rt.byName))
	for _, m := range rt.byName {
		all = append(all, m)
	}
	rt.mu.Unlock()
	var wg sync.WaitGroup
	for _, m := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.stop(grace, false)
		}()
	}
	wg.Wait()
}

func monotonicMS() float64 {
	var ts unix.Timespec
	unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return float64(ts.Nano()) / 1e6
}

type RunningMachine struct {
	Name, UUID, Group string
	PID               int
}

func (rt *Runtime) Running() []RunningMachine {
	rt.mu.Lock()
	all := make([]*machine, 0, len(rt.byName))
	for _, m := range rt.byName {
		all = append(all, m)
	}
	rt.mu.Unlock()
	var out []RunningMachine
	for _, m := range all {
		if !m.mu.TryLock() {
			continue
		}
		r, name := m.cur, m.name
		uuid := ""
		if r != nil {
			uuid = r.rec.UUID
		}
		m.mu.Unlock()
		if r == nil {
			continue
		}
		rm := RunningMachine{Name: name, UUID: uuid, Group: r.group}
		if b := r.boot(); b != nil {
			rm.PID = b.child.Pid
		}
		out = append(out, rm)
	}
	return out
}
