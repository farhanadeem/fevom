package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fev/host/daemon/config"
	"fev/host/images"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/relay"
)

type Machines struct {
	ImageServer  string
	MacUser      string
	UID          int
	Trust        func() ([]string, error)
	SSHAuthSock  string
	Agent        *SSHAgent
	ShareRoot    string
	Notify       func(title, message string) error
	OnState      func(st agent.MachineState)
	OnGone       func()
	BeforeResume func(boot string)
	View         func(machine string) string

	DebugDockerConfig  string
	DebugDefaultHost   string
	DebugDockerCLI     string
	DebugAgentDir      string
	DebugOwnDockerHost string

	host   Host
	log    *slog.Logger
	cfg    *config.Store
	path   string
	ops    *Operations
	source *images.Source

	mu       sync.Mutex
	entries  map[string]*machineEntry
	def      string
	busy     map[string]int
	gone     map[string]bool
	boot     string
	client   *agent.Client
	vm       VM
	resumed  string
	attachMu sync.Mutex

	sessions sessionTable
	bridge   *bridgeServer
	holdFor  time.Duration
}

type machineEntry struct {
	Emulation       string    `json:"-"`
	EmulationReason string    `json:"-"`
	Name            string    `json:"name"`
	Image           api.Image `json:"image"`
	UID             int       `json:"uid"`
	MAC             string    `json:"mac"`
	Address         string    `json:"address"`
	Created         string    `json:"created"`
	Address6        string    `json:"-"`
	Running         bool      `json:"running,omitempty"`
	DiskBytes       int64     `json:"disk_bytes,omitempty"`
	LastExit        string    `json:"last_exit,omitempty"`

	Isolated       bool `json:"isolated,omitempty"`
	IsolateNetwork bool `json:"isolate_network,omitempty"`
	startValues    map[string]any

	State string `json:"-"`
	Since string `json:"-"`
	Error string `json:"-"`
}

type machinesFile struct {
	Version  int             `json:"version"`
	Default  string          `json:"default,omitempty"`
	Machines []*machineEntry `json:"machines"`
}

func (m *Machines) machinesPrefix() netip.Prefix { return planInEffect(m.cfg).Machines() }

func (m *Machines) planned(addr string) string {
	a, err := netip.ParseAddr(addr)
	prefix := m.machinesPrefix()
	if err != nil || !a.Is4() || prefix.Contains(a) {
		return addr
	}
	b, p := a.As4(), prefix.Addr().As4()
	return netip.AddrFrom4([4]byte{p[0], p[1], p[2], b[3]}).String()
}

func (m *Machines) Name() string    { return "machines" }
func (m *Machines) Marks() []string { return nil }

func (m *Machines) Start(h Host) error {
	m.host, m.log, m.cfg = h, h.Log(), h.Config()
	m.path = h.Layout().MachinesFile()
	m.entries, m.busy, m.gone = map[string]*machineEntry{}, map[string]int{}, map[string]bool{}
	if m.MacUser == "" || m.UID == 0 {
		u, err := user.Current()
		if err != nil {
			return fmt.Errorf("the Mac user: %w", err)
		}
		uid, _ := strconv.Atoi(u.Uid)
		if m.MacUser == "" {
			m.MacUser = u.Username
		}
		if m.UID == 0 {
			m.UID = uid
		}
	}
	if err := config.CheckUsername(m.MacUser); err != nil {
		m.log.Warn("machines: the Mac user's name is not one a Linux machine takes; a create must name its user", "user", m.MacUser, "err", err)
	}
	if err := m.load(); err != nil {
		return err
	}
	for name := range m.entries {
		if err := m.cfg.AddScope(config.MachinePrefix, name, config.MachineKeys(m.MacUser), nil); err != nil {
			m.log.Warn("machines: a machine's settings cannot be read; its defaults are in use", "machine", name, "err", err)
		}
	}
	m.cfg.PruneScope(config.MachinePrefix, func(name string) bool { return m.entries[name] != nil })
	src, err := images.NewSource(m.ImageServer, h.Layout().ImagesDir())
	if err != nil {
		return err
	}
	m.source = src
	m.ops = NewOperations(h.PublishOperation, h.WorkEnded)
	m.bridge = newBridgeServer(m, m.log)
	if err := m.sessions.start(m, h.Layout().Home); err != nil {
		return err
	}
	for method, fn := range map[string]MethodFunc{
		api.MethodMachinesList:       m.list,
		api.MethodMachinesInfo:       m.info,
		api.MethodMachinesCreate:     m.create,
		api.MethodMachinesStart:      m.start,
		api.MethodMachinesStop:       m.stop,
		api.MethodMachinesRestart:    m.restart,
		api.MethodMachinesDelete:     m.delete,
		api.MethodMachinesClone:      m.clone,
		api.MethodMachinesRename:     m.rename,
		api.MethodMachinesExport:     m.export,
		api.MethodMachinesImport:     m.importMachine,
		api.MethodMachinesSetDefault: m.setDefault,
		api.MethodVolumesClone:       m.cloneVolume,
		api.MethodVolumesExport:      m.exportVolume,
		api.MethodVolumesImport:      m.importVolume,
		api.MethodOperationGet:       m.ops.handle(api.MethodOperationGet),
		api.MethodOperationCancel:    m.ops.handle(api.MethodOperationCancel),
		api.MethodSessionStart:       m.sessions.startSession,
		api.MethodSessionResize:      m.sessions.call(api.MethodSessionResize),
		api.MethodSessionSignal:      m.sessions.call(api.MethodSessionSignal),
		api.MethodSessionClose:       m.sessions.call(api.MethodSessionClose),
		api.MethodDebugStart:         m.sessions.startDebug,
		api.MethodDebugStatus:        m.debugStatus,
		api.MethodDebugReset:         m.debugReset,
		api.MethodContextsList:       m.contextsList,
		api.MethodContextContainers:  m.contextContainers,
		api.MethodContextImages:      m.contextImages,
	} {
		h.Handle(method, fn)
	}
	return nil
}

func (m *Machines) Operations() *Operations { return m.ops }

func (m *Machines) Close() {
	m.ops.Close()
	m.sessions.close()
}

func (m *Machines) load() error {
	data, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f machinesFile
	if err := json.Unmarshal(data, &f); err != nil || f.Version != 1 {
		aside := m.path + ".corrupt-" + time.Now().UTC().Format("20060102T150405")
		os.Rename(m.path, aside)
		m.log.Warn("machines: the record could not be read and was set aside; the guest's machines are taken up again at the next boot", "aside", aside, "err", err)
		return nil
	}
	now := stamp(time.Now())
	for _, e := range f.Machines {
		if relay.CheckMachineName(e.Name) != nil {
			continue
		}
		e.State, e.Since = api.MachineStopped, now
		m.entries[e.Name] = e
	}
	if m.entries[f.Default] != nil {
		m.def = f.Default
	}
	return nil
}

func (m *Machines) saveLocked() {
	f := machinesFile{Version: 1, Default: m.def, Machines: []*machineEntry{}}
	for _, e := range m.sorted() {
		if e.State != api.MachineCreating {
			f.Machines = append(f.Machines, e)
		}
	}
	data, _ := json.MarshalIndent(f, "", "  ")
	tmp := m.path + ".tmp"
	err := os.WriteFile(tmp, append(data, '\n'), 0o600)
	if err == nil {
		err = os.Rename(tmp, m.path)
	}
	if err != nil {
		m.log.Error("machines: cannot write the record", "path", m.path, "err", err)
	}
}

func (m *Machines) sorted() []*machineEntry {
	out := make([]*machineEntry, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m *Machines) view(e *machineEntry) api.Machine {
	state := e.State
	if state == "" {
		state = api.MachineStopped
	}
	return api.Machine{Name: e.Name, State: state, Image: e.Image, User: m.username(e.Name), Default: e.Name == m.def,
		Emulation: e.Emulation, EmulationReason: e.EmulationReason,
		Address: m.planned(e.Address), Address6: e.Address6, DiskBytes: e.DiskBytes, Since: e.Since, LastExit: e.LastExit, Error: e.Error,
		Isolated: e.Isolated, IsolateNetwork: e.IsolateNetwork}
}

func (m *Machines) username(name string) string {
	if u := m.cfg.String(config.MachinePrefix + name + "." + config.KeyUsername); u != "" {
		return u
	}
	return m.MacUser
}

func (m *Machines) changedLocked(e *machineEntry, removed bool) {
	m.host.PublishMachine(api.MachinesChanged{Machine: m.view(e), Removed: removed})
}

func setStateLocked(e *machineEntry, state string) {
	if e.State != state {
		e.State, e.Since = state, stamp(time.Now())
	}
}

func (m *Machines) applyLocked(e *machineEntry, i agent.MachineInfo, vmStopping bool) {
	setStateLocked(e, i.State)
	switch i.State {
	case api.MachineRunning, api.MachineStarting:
		if e.startValues != nil {
			m.cfg.MachineStarted(e.Name, e.startValues)
		}
	case api.MachineStopped, api.MachineFailed:
		m.cfg.MachineStopped(e.Name)
	}
	e.LastExit, e.Error, e.DiskBytes = i.LastExit, i.Error, i.DiskBytes
	e.Emulation, e.EmulationReason = i.Emulation, i.EmulationReason
	e.Address6 = ""
	if i.State == api.MachineRunning || i.State == api.MachineStarting {
		e.Address6 = i.Address6
	}
	if i.State == api.MachineFailed && e.Error == "" {
		e.Error = "the machine failed"
	}
	if !vmStopping {
		switch i.State {
		case api.MachineRunning, api.MachineStarting:
			e.Running = true
		case api.MachineStopped, api.MachineFailed:
			e.Running = false
		}
	}
}

func (m *Machines) vmStopping() bool {
	st := m.host.Status().State
	return st == api.StateStopping || st == api.StateStopped
}

func (m *Machines) machineShare(name string) (share, known bool) {
	m.mu.Lock()
	_, known = m.entries[name]
	m.mu.Unlock()
	if !known {
		return false, false
	}
	return m.configFor(name).Share, true
}

func (m *Machines) configFor(name string) agent.MachineConfig {
	m.mu.Lock()
	e := m.entries[name]
	m.mu.Unlock()
	cfg := agent.MachineConfig{User: m.username(name)}
	if value, _ := m.machineKey(name, api.MachineKeyEmulation).(string); value != "auto" {
		cfg.Emulation = value
	}
	if e != nil {
		cfg.MAC, cfg.Address = e.MAC, m.planned(e.Address)
	}
	share, _ := m.cfg.InEffect(api.KeyFilesShare)
	cfg.Share = share == true
	cfg.Isolated = m.machineKey(name, api.MachineKeyIsolated) == true
	if cfg.Isolated {
		cfg.IsolateNetwork = m.machineKey(name, api.MachineKeyIsolateNetwork) == true
		cfg.ForwardSSHAgent = m.machineKey(name, api.MachineKeyForwardSSHAgent) == true
		mounts, _ := m.machineKey(name, api.MachineKeyMounts).([]string)
		cfg.Mounts = m.mounts(mounts)
	}
	return cfg
}

func (m *Machines) machineKey(name, short string) any {
	key := config.MachinePrefix + name + "." + short
	got, err := m.cfg.Get(key)
	if err != nil {
		return nil
	}
	return got[key].Value
}

func (m *Machines) IsolatedNames() []string {
	m.mu.Lock()
	names := make([]string, 0, len(m.entries))
	var out []string
	for name, e := range m.entries {
		if e.Isolated {
			out = append(out, name)
		} else {
			names = append(names, name)
		}
	}
	m.mu.Unlock()
	if m.cfg != nil {
		for _, name := range names {
			if m.machineKey(name, api.MachineKeyIsolated) == true {
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return out
}

func (m *Machines) mounts(values []string) []agent.MachineMount {
	var out []agent.MachineMount
	for _, v := range values {
		mm, err := agent.ParseMachineMount(v)
		if err != nil {
			m.log.Warn("machines: a mount is not taken", "mount", v, "err", err)
			continue
		}
		src, ok := sharePath(m.ShareRoot, mm.Source)
		if !ok {
			m.log.Warn("machines: a mount's source is outside what the VM is given of the Mac", "mount", v, "share", m.ShareRoot)
			continue
		}
		if mm.Dest == "" {
			mm.Dest = mm.Source
		}
		mm.Source = src
		out = append(out, mm)
	}
	return out
}

func sharePath(root, p string) (string, bool) {
	root = filepath.Clean(root)
	if root == "" || root == "/" || root == "." {
		return filepath.Clean(p), true
	}
	if rel, ok := strings.CutPrefix(filepath.Clean(p), root+"/"); ok {
		return "/" + rel, true
	}
	if filepath.Clean(p) == root {
		return "/", true
	}
	return "", false
}

func (m *Machines) startConfig(name string) agent.MachineConfig {
	cfg := m.configFor(name)
	m.startedWith(name, cfg)
	return cfg
}

func (m *Machines) startedWith(name string, cfg agent.MachineConfig) {
	values := m.cfg.MachineValues(name)
	m.mu.Lock()
	if e := m.entries[name]; e != nil {
		e.Isolated, e.IsolateNetwork = cfg.Isolated, cfg.IsolateNetwork
		e.startValues = values
	}
	m.mu.Unlock()
}

func (m *Machines) allocLocked() (addr, mac string, err error) {
	taken := map[string]bool{}
	for _, e := range m.entries {
		taken[m.planned(e.Address)] = true
	}
	prefix := m.machinesPrefix()
	gateway := prefix.Addr().Next()
	for a := gateway.Next(); prefix.Contains(a); a = a.Next() {
		b := a.As4()
		if b[3] == 255 {
			break
		}
		if !taken[a.String()] {
			return a.String(), fmt.Sprintf("02:42:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3]), nil
		}
	}
	return "", "", fmt.Errorf("every address of the machines' subnet (%s) is a machine's: delete one first", prefix)
}

func (m *Machines) hold(names ...string) func() {
	m.mu.Lock()
	for _, n := range names {
		m.busy[n]++
	}
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		for _, n := range names {
			if m.busy[n]--; m.busy[n] <= 0 {
				delete(m.busy, n)
			}
		}
		m.mu.Unlock()
	}
}

func (m *Machines) StateChanged(st api.Status) {
	if cur := m.currentBoot(); st.State == api.StateStopped || (cur != "" && st.BootID != cur) {
		m.vmGone()
	}
	switch {
	case st.State == api.StateRunning || (st.State == api.StateStarting && st.Phase == api.PhaseEngine):
		boot := st.BootID
		resume := st.State == api.StateRunning || st.Engine == api.EngineFailed
		go func() {
			if _, err := m.attach(boot); err != nil {
				return
			}
			if resume {
				m.resume(boot)
			}
		}()
	}
}

func (m *Machines) ConfigChanged(ch api.ConfigChange) {
	name, short, ok := scopedName(ch.Key)
	if !ok || short != config.KeyUsername {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.entries[name]; e != nil {
		m.changedLocked(e, false)
	}
}

func scopedName(key string) (name, short string, ok bool) {
	rest, ok := cutPrefix(key, config.MachinePrefix)
	if !ok {
		return "", "", false
	}
	for i := len(rest) - 1; i >= 0; i-- {
		if rest[i] == '.' {
			return rest[:i], rest[i+1:], true
		}
	}
	return "", "", false
}

func cutPrefix(s, p string) (string, bool) {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):], true
	}
	return "", false
}

func (m *Machines) currentBoot() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.boot
}

func (m *Machines) vmGone() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.boot, m.client, m.vm = "", nil, nil
	m.gone = map[string]bool{}
	if m.OnGone != nil {
		m.OnGone()
	}
	for _, e := range m.sorted() {
		if e.State == api.MachineCreating {
			continue
		}
		e.Address6 = ""
		m.cfg.MachineStopped(e.Name)
		if e.State != api.MachineStopped {
			setStateLocked(e, api.MachineStopped)
			if e.State != api.MachineFailed {
				e.Error = ""
			}
			m.changedLocked(e, false)
		}
	}
}

func (m *Machines) attach(boot string) (*agent.Client, error) {
	m.attachMu.Lock()
	defer m.attachMu.Unlock()
	m.mu.Lock()
	if m.boot == boot && m.client != nil {
		c := m.client
		m.mu.Unlock()
		return c, nil
	}
	m.mu.Unlock()
	vm, ok := m.host.VM()
	if !ok || vm.BootID() != boot {
		return nil, errors.New("the VM is not running")
	}
	client := vm.Agent()
	client.OnMachineState(m.guestState)
	client.OnMachineStreamRequest(m.bridge.requests(vm))
	m.sendTrust(boot, client)
	ctx, cancel := context.WithTimeout(context.Background(), machineCallBound)
	list, err := client.MachineList(ctx)
	cancel()
	if err != nil && noMachinesOnThisDisk(err) {
		m.log.Warn("machines: none in this boot; the record is kept as it is", "boot", boot, "why", err)
		m.mu.Lock()
		m.boot, m.client, m.vm = boot, client, vm
		m.mu.Unlock()
		return client, nil
	}
	if err != nil {
		m.log.Warn("machines: the guest did not list its machines", "boot", boot, "err", err)
		return nil, fmt.Errorf("the guest did not list its machines: %w", err)
	}
	m.mu.Lock()
	m.boot, m.client, m.vm = boot, client, vm
	m.reconcileLocked(list.Machines)
	m.mu.Unlock()
	return client, nil
}

func noMachinesOnThisDisk(err error) bool {
	var e *jsonrpc.Error
	if !errors.As(err, &e) || e.Code != agent.CodeFailed || !strings.HasPrefix(e.Message, "machines are not available: ") {
		return false
	}
	return strings.Contains(e.Message, agent.DataReadOnly) || strings.Contains(e.Message, agent.NoDataFilesystem)
}

const machineCallBound = 30 * time.Second

func (m *Machines) reconcileLocked(guest []agent.MachineInfo) {
	stopping := m.vmStopping()
	seen := map[string]bool{}
	for _, i := range guest {
		if relay.CheckMachineName(i.Name) != nil {
			continue
		}
		seen[i.Name] = true
		e := m.entries[i.Name]
		if e == nil {
			if m.busy[i.Name] > 0 {
				continue
			}
			e = m.adoptLocked(i)
			if e == nil {
				continue
			}
		}
		running := e.Running
		m.applyLocked(e, i, stopping)
		if m.OnState != nil {
			m.OnState(agent.MachineState{MachineInfo: i})
		}
		if i.State == api.MachineStopped {
			e.Running = running
		}
		m.changedLocked(e, false)
	}
	for _, e := range m.sorted() {
		if !seen[e.Name] && m.busy[e.Name] == 0 && e.State != api.MachineCreating {
			m.log.Warn("machines: the guest has no machine of this name; its entry goes", "machine", e.Name)
			m.removeLocked(e)
		}
	}
	m.saveLocked()
}

func (m *Machines) adoptLocked(i agent.MachineInfo) *machineEntry {
	addr, mac, err := m.allocLocked()
	if err != nil {
		m.log.Error("machines: a machine of the guest's cannot be taken up", "machine", i.Name, "err", err)
		return nil
	}
	e := &machineEntry{Name: i.Name, Image: api.Image(i.Image), UID: m.UID, MAC: mac, Address: addr, Created: stamp(time.Now())}
	if a, err := netip.ParseAddr(i.Address); err == nil && m.machinesPrefix().Contains(a) {
		e.Address = i.Address
	}
	m.entries[i.Name] = e
	if err := m.cfg.AddScope(config.MachinePrefix, i.Name, config.MachineKeys(m.MacUser), nil); err != nil {
		m.log.Warn("machines: a machine's settings cannot be made", "machine", i.Name, "err", err)
	}
	if m.def == "" {
		m.def = i.Name
	}
	m.log.Info("machines: took up a machine the guest has and the record had not", "machine", i.Name)
	return e
}

func (m *Machines) removeLocked(e *machineEntry) {
	delete(m.entries, e.Name)
	if err := m.cfg.RemoveScope(config.MachinePrefix, e.Name); err != nil {
		m.log.Warn("machines: a machine's settings could not be removed", "machine", e.Name, "err", err)
	}
	view := m.view(e)
	if m.def == e.Name {
		m.def = ""
		view.Default = false
	}
	m.host.PublishMachine(api.MachinesChanged{Machine: view, Removed: true})
}

func (m *Machines) guestState(st agent.MachineState) {
	if relay.CheckMachineName(st.Name) != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[st.Name]
	busy := m.busy[st.Name] > 0
	if m.OnState != nil {
		m.OnState(st)
	}
	if e != nil && e.State == api.MachineRunning && (st.Removed || st.State != agent.MachineRunning) {
		m.workEnded()
	}
	switch {
	case st.Removed:
		delete(m.gone, st.Name)
		if e != nil && !busy {
			m.removeLocked(e)
			m.saveLocked()
		}
		return
	case e == nil && (busy || m.gone[st.Name]):
		return
	case e == nil:
		if e = m.adoptLocked(st.MachineInfo); e == nil {
			return
		}
	}
	m.applyLocked(e, st.MachineInfo, m.vmStopping())
	m.saveLocked()
	m.changedLocked(e, false)
}

func (m *Machines) resume(boot string) {
	m.mu.Lock()
	if m.resumed == boot || m.boot != boot {
		m.mu.Unlock()
		return
	}
	m.resumed = boot
	if m.BeforeResume != nil {
		m.mu.Unlock()
		m.BeforeResume(boot)
		m.mu.Lock()
		if m.boot != boot {
			m.mu.Unlock()
			return
		}
	}
	client := m.client
	var names []string
	for _, e := range m.sorted() {
		if e.Running && e.State != api.MachineRunning && e.State != api.MachineCreating && m.busy[e.Name] == 0 {
			names = append(names, e.Name)
		}
	}
	m.mu.Unlock()
	for _, name := range names {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), machineStartBound)
			defer cancel()
			i, err := client.MachineStart(ctx, name, m.startConfig(name))
			if err != nil {
				m.log.Error("machines: a machine that ran when the VM stopped did not start again; it is left stopped", "machine", name, "err", err)
				m.mu.Lock()
				if e := m.entries[name]; e != nil && e.Running {
					e.Running = false
					m.saveLocked()
					m.changedLocked(e, false)
				}
				m.mu.Unlock()
				return
			}
			m.log.Info("machines: started again, as it ran when the VM stopped", "machine", name)
			m.mu.Lock()
			if e := m.entries[name]; e != nil {
				m.applyLocked(e, *i, false)
				m.saveLocked()
				m.changedLocked(e, false)
			}
			m.mu.Unlock()
		}()
	}
}

const machineStartBound = 3 * time.Minute

func (m *Machines) guest(ctx context.Context, start bool) (*agent.Client, error) {
	usable := func(st api.Status) bool {
		return st.State == api.StateRunning || st.State == api.StatePaused || (st.State == api.StateStarting && st.Phase == api.PhaseEngine)
	}
	if !start {
		st := m.host.Status()
		if !usable(st) || st.State == api.StatePaused {
			return nil, errVMDown
		}
		return m.attach(st.BootID)
	}
	if err := m.host.StartAsked(); err != nil {
		return nil, shutting(err)
	}
	st, err := m.host.WaitStatus(ctx, func(st api.Status) bool { return usable(st) || st.State == api.StateStopped })
	if err != nil {
		return nil, err
	}
	if st.State == api.StateStopped {
		if st.Error != "" {
			return nil, jsonrpc.Errorf(api.CodeFailed, "the VM did not start: %s", st.Error)
		}
		return nil, jsonrpc.Errorf(api.CodeFailed, "the VM was stopped before it was running")
	}
	return m.attach(st.BootID)
}

var errVMDown = errors.New("the VM is not running")

func (m *Machines) entry(name string) (*machineEntry, error) {
	if err := relay.CheckMachineName(name); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[name]
	switch {
	case e == nil:
		return nil, jsonrpc.Errorf(api.CodeNoMachine, "no machine %q", name)
	case e.State == api.MachineCreating:
		return nil, jsonrpc.Errorf(api.CodeFailed, "the machine %s is being made", name)
	}
	return e, nil
}

func guestError(err error) error {
	if agent.IsNoMachine(err) {
		var e *jsonrpc.Error
		errors.As(err, &e)
		return jsonrpc.Errorf(api.CodeNoMachine, "%s", e.Message)
	}
	if agent.IsDebugToolsUnavailable(err) {
		var e *jsonrpc.Error
		errors.As(err, &e)
		return jsonrpc.Errorf(api.CodeDebugToolsUnavailable, "%s", e.Message)
	}
	if agent.IsBusy(err) {
		var e *jsonrpc.Error
		errors.As(err, &e)
		return jsonrpc.Errorf(api.CodeBusy, "%s", e.Message)
	}
	var e *jsonrpc.Error
	if errors.As(err, &e) {
		return jsonrpc.Errorf(api.CodeFailed, "%s", e.Message)
	}
	return err
}

func (m *Machines) list(ctx context.Context, _ json.RawMessage) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := api.Machines{Machines: []api.Machine{}, Default: m.def}
	for _, e := range m.sorted() {
		r.Machines = append(r.Machines, m.view(e))
	}
	return r, nil
}

func (m *Machines) info(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.MachineName](params)
	if err != nil {
		return nil, err
	}
	e, err := m.entry(p.Name)
	if err != nil {
		return nil, err
	}
	if client, err := m.guest(ctx, false); err == nil {
		cctx, cancel := context.WithTimeout(ctx, machineCallBound)
		i, err := client.Machine(cctx, p.Name)
		cancel()
		if err == nil {
			m.mu.Lock()
			m.applyLocked(e, *i, m.vmStopping())
			m.saveLocked()
			m.mu.Unlock()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.view(e), nil
}

func (m *Machines) setDefault(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.MachineName](params)
	if err != nil {
		return nil, err
	}
	e, err := m.entry(p.Name)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	old := m.entries[m.def]
	m.def = e.Name
	m.saveLocked()
	if old != nil && old != e {
		m.changedLocked(old, false)
	}
	m.changedLocked(e, false)
	m.mu.Unlock()
	m.log.Info("machines: the default machine is now this one", "machine", e.Name)
	return m.list(ctx, nil)
}

func (m *Machines) startMachine(ctx context.Context, name string) (api.Machine, error) {
	e, err := m.entry(name)
	if err != nil {
		return api.Machine{}, err
	}
	client, err := m.guest(ctx, true)
	if err != nil {
		return api.Machine{}, err
	}
	m.mu.Lock()
	running := e.State == api.MachineRunning
	arch := e.Image.Arch
	m.mu.Unlock()
	if !running {
		if err := m.amd64Refusal(arch); err != nil {
			return api.Machine{}, err
		}
		cctx, cancel := context.WithTimeout(ctx, machineStartBound)
		i, err := client.MachineStart(cctx, name, m.startConfig(name))
		cancel()
		if err != nil {
			return api.Machine{}, guestError(err)
		}
		m.mu.Lock()
		m.applyLocked(e, *i, false)
		m.saveLocked()
		m.changedLocked(e, false)
		m.mu.Unlock()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.view(e), nil
}

func (m *Machines) start(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.MachineName](params)
	if err != nil {
		return nil, err
	}
	return m.startMachine(ctx, p.Name)
}

func stopParams(params json.RawMessage) (api.MachinesStopParams, error) {
	p, err := jsonrpc.Bind[api.MachinesStopParams](params)
	if err == nil && (p.GraceMS < 0 || p.GraceMS > MaxStopTimeoutMS) {
		err = jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "grace_ms must be between 0 and %d", MaxStopTimeoutMS)
	}
	return p, err
}

func (m *Machines) stop(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := stopParams(params)
	if err != nil {
		return nil, err
	}
	e, err := m.entry(p.Name)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	e.Running = false
	m.saveLocked()
	m.mu.Unlock()
	client, err := m.guest(ctx, false)
	if errors.Is(err, errVMDown) {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.view(e), nil
	}
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, machineStartBound)
	i, err := client.MachineStop(cctx, agent.MachineStopParams{Name: p.Name, GraceMS: p.GraceMS, Force: p.Force})
	cancel()
	if err != nil {
		return nil, guestError(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applyLocked(e, *i, false)
	m.saveLocked()
	m.changedLocked(e, false)
	m.workEnded()
	return m.view(e), nil
}

func (m *Machines) workEnded() {
	if m.host != nil {
		go m.host.WorkEnded()
	}
}

func (m *Machines) restart(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := stopParams(params)
	if err != nil {
		return nil, err
	}
	e, err := m.entry(p.Name)
	if err != nil {
		return nil, err
	}
	client, err := m.guest(ctx, true)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 2*machineStartBound)
	i, err := client.MachineRestart(cctx, agent.MachineRestartParams{Name: p.Name, GraceMS: p.GraceMS, Config: m.startConfig(p.Name)})
	cancel()
	if err != nil {
		return nil, guestError(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applyLocked(e, *i, false)
	m.saveLocked()
	m.changedLocked(e, false)
	return m.view(e), nil
}

func (m *Machines) delete(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := stopParams(params)
	if err != nil {
		return nil, err
	}
	e, err := m.entry(p.Name)
	if err != nil {
		return nil, err
	}
	release := m.hold(p.Name)
	defer release()
	client, err := m.guest(ctx, true)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, machineStartBound)
	err = client.MachineDelete(cctx, agent.MachineStopParams{Name: p.Name, GraceMS: p.GraceMS, Force: p.Force})
	cancel()
	if err != nil && !agent.IsNoMachine(err) {
		return nil, guestError(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries[p.Name] == e {
		m.removeLocked(e)
		m.gone[p.Name] = true
		m.saveLocked()
	}
	m.log.Info("machines: deleted", "machine", p.Name)
	m.workEnded()
	return api.Empty{}, nil
}

func (m *Machines) clone(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.MachinesRenameParams](params)
	if err != nil {
		return nil, err
	}
	src, err := m.entry(p.Name)
	if err != nil {
		return nil, err
	}
	if err := m.free(p.NewName, ""); err != nil {
		return nil, err
	}
	release := m.hold(p.NewName)
	defer release()
	client, err := m.guest(ctx, true)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	addr, mac, err := m.allocLocked()
	m.mu.Unlock()
	if err != nil {
		return nil, jsonrpc.Errorf(api.CodeFailed, "%v", err)
	}
	cfg := m.configFor(p.Name)
	cfg.MAC, cfg.Address = mac, addr
	cctx, cancel := context.WithTimeout(ctx, machineStartBound)
	i, err := client.MachineClone(cctx, agent.MachineCloneParams{Name: p.Name, NewName: p.NewName, Config: cfg})
	cancel()
	if err != nil {
		return nil, guestError(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := &machineEntry{Name: p.NewName, Image: src.Image, UID: src.UID, MAC: mac, Address: addr, Created: stamp(time.Now())}
	m.applyLocked(e, *i, false)
	values := map[string]any{config.KeyUsername: m.username(p.Name)}
	for _, short := range []string{api.MachineKeyIsolated, api.MachineKeyIsolateNetwork, api.MachineKeyMounts, api.MachineKeyForwardSSHAgent, api.MachineKeyHTTPPort, api.MachineKeyHTTPSPort, api.MachineKeyEmulation} {
		key := config.MachinePrefix + p.Name + "." + short
		if !m.cfg.IsDefault(key) {
			if got, err := m.cfg.Get(key); err == nil {
				values[short] = got[key].Value
			}
		}
	}
	if err := m.cfg.AddScope(config.MachinePrefix, p.NewName, config.MachineKeys(m.MacUser), values); err != nil {
		m.log.Warn("machines: a clone's settings cannot be made", "machine", p.NewName, "err", err)
	}
	m.entries[p.NewName] = e
	m.saveLocked()
	m.changedLocked(e, false)
	return m.view(e), nil
}

func (m *Machines) free(name, self string) error {
	if err := relay.CheckMachineName(name); err != nil {
		return jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.takenLocked(name, self); err != nil {
		return err
	}
	return nil
}

func (m *Machines) takenLocked(name, self string) error {
	taken := ""
	for n := range m.entries {
		if n != self && relay.SameMachineName(n, name) {
			taken = n
		}
	}
	for n := range m.busy {
		if taken == "" && n != self && relay.SameMachineName(n, name) {
			taken = n
		}
	}
	switch {
	case taken == name:
		return jsonrpc.Errorf(api.CodeFailed, "a machine named %s exists", name)
	case taken != "":
		return jsonrpc.Errorf(api.CodeFailed, "a machine named %s exists: names that differ only in case are one name", taken)
	}
	return nil
}

func (m *Machines) rename(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.MachinesRenameParams](params)
	if err != nil {
		return nil, err
	}
	e, err := m.entry(p.Name)
	if err != nil {
		return nil, err
	}
	if err := m.free(p.NewName, p.Name); err != nil {
		return nil, err
	}
	release := m.hold(p.Name, p.NewName)
	defer release()
	client, err := m.guest(ctx, true)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, machineStartBound)
	i, err := client.MachineRename(cctx, p.Name, p.NewName)
	cancel()
	if err != nil {
		return nil, guestError(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.view(e)
	delete(m.entries, p.Name)
	m.gone[p.Name] = true
	e.Name = p.NewName
	m.entries[p.NewName] = e
	if err := m.cfg.RenameScope(config.MachinePrefix, p.Name, p.NewName); err != nil {
		m.log.Warn("machines: a renamed machine's settings could not follow it", "machine", p.NewName, "err", err)
	}
	if m.def == p.Name {
		m.def = p.NewName
	}
	m.applyLocked(e, *i, false)
	m.saveLocked()
	old.Default = false
	m.host.PublishMachine(api.MachinesChanged{Machine: old, Removed: true})
	m.changedLocked(e, false)
	m.log.Info("machines: renamed", "machine", p.Name, "new_name", p.NewName)
	return m.view(e), nil
}

func (m *Machines) Forget() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.sorted() {
		if e.State != api.MachineCreating {
			m.removeLocked(e)
		}
	}
	m.def = ""
	m.saveLocked()
	m.log.Info("machines: the data disk was reset; every machine's entry and settings are gone")
}
