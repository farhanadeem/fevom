package standin

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	api "fev/proto/agent"
	"fev/proto/jsonrpc"
	"fev/proto/relay"
)

const StandinSerial = "20261014_00:00"

var standinBuilds = [][2]string{
	{"alpine", "3.24"}, {"archlinux", "current"}, {"debian", "trixie"}, {"fedora", "44"}, {"ubuntu", "resolute"},
}

type fakeMachine struct {
	Info   api.MachineInfo   `json:"info"`
	Config api.MachineConfig `json:"config"`
	User   api.MachineUser   `json:"user"`
	ports  []api.MachinePort
}

type fakeOp struct {
	id     uint64
	method string
	conn   *jsonrpc.Conn
	cancel context.CancelFunc
	ctx    context.Context
	token  string
	stream func(conn net.Conn)
}

type machineTable struct {
	a *Agent

	mu       sync.Mutex
	file     string
	byName   map[string]*fakeMachine
	kept     []api.ImageInfo
	ops      map[uint64]*fakeOp
	nextOp   uint64
	creating map[string]bool
	portSeq  uint64
}

type MachinesOptions struct {
	File         string
	Builds       []api.ImageInfo
	NoBuilds     bool
	CreateTime   time.Duration
	CreateFail   string
	StartingTime time.Duration
}

func (a *Agent) SetMachines(o MachinesOptions) {
	m := a.machines()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.file = o.File
	a.CreateTime, a.CreateFail, a.StartingTime = o.CreateTime, o.CreateFail, o.StartingTime
	switch {
	case o.NoBuilds:
		m.kept = []api.ImageInfo{}
	case o.Builds != nil:
		m.kept = o.Builds
	}
	if m.file == "" {
		return
	}
	var saved struct {
		Machines []*fakeMachine  `json:"machines"`
		Kept     []api.ImageInfo `json:"kept"`
	}
	data, err := os.ReadFile(m.file)
	if err != nil || json.Unmarshal(data, &saved) != nil {
		return
	}
	for _, fm := range saved.Machines {
		fm.Info.State, fm.Info.PID, fm.Info.Address, fm.Info.StartedMS, fm.Info.ReadyMS = api.MachineStopped, 0, "", 0, 0
		fm.Info.Address6 = ""
		if fm.Info.LastExit == "" {
			fm.Info.LastExit = api.ExitStopped
		}
		m.byName[fm.Info.Name] = fm
	}
	if saved.Kept != nil && !o.NoBuilds && o.Builds == nil {
		m.kept = saved.Kept
	}
}

func (a *Agent) machines() *machineTable {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mtable == nil {
		m := &machineTable{a: a, byName: map[string]*fakeMachine{}, ops: map[uint64]*fakeOp{}, creating: map[string]bool{}}
		for _, b := range standinBuilds {
			for _, v := range []string{api.VariantDefault, api.VariantCloud} {
				if b[0] == "archlinux" && v == api.VariantCloud {
					continue
				}
				m.kept = append(m.kept, api.ImageInfo{ImageRef: api.ImageRef{Distro: b[0], Release: b[1], Arch: "arm64", Variant: v, Serial: StandinSerial}, DiskBytes: 400 << 20})
			}
		}
		m.kept = append(m.kept, api.ImageInfo{ImageRef: api.ImageRef{Distro: "ubuntu", Release: "resolute", Arch: "amd64", Variant: api.VariantDefault, Serial: StandinSerial}, DiskBytes: 400 << 20})
		a.mtable = m
	}
	return a.mtable
}

func (a *Agent) runningMachines() []api.MachineInfo {
	m := a.machines()
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []api.MachineInfo
	for _, f := range m.byName {
		if f.Info.State == api.MachineRunning {
			out = append(out, f.Info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (a *Agent) AddMachine(name string, cfg api.MachineConfig) {
	m := a.machines()
	m.mu.Lock()
	m.byName[name] = &fakeMachine{
		Info: api.MachineInfo{Name: name, State: api.MachineStopped, UUID: newUUID(), Init: "systemd", DiskBytes: 64 << 20,
			Image: api.ImageRef{Distro: "ubuntu", Release: "resolute", Arch: "arm64", Variant: api.VariantDefault, Serial: StandinSerial}},
		Config: cfg, User: api.MachineUser{Name: cfg.User, UID: 501},
	}
	m.saveLocked()
	m.mu.Unlock()
}

func (a *Agent) RemoveMachine(name string) {
	m := a.machines()
	m.mu.Lock()
	delete(m.byName, name)
	m.saveLocked()
	m.mu.Unlock()
}

func (a *Agent) MachineNames() []string {
	m := a.machines()
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for name := range m.byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func newUUID() string {
	t := newToken()
	return t[0:8] + "-" + t[8:12] + "-4" + t[13:16] + "-8" + t[17:20] + "-" + t[20:32]
}

func (m *machineTable) takenLocked(name, self string) bool {
	for n := range m.byName {
		if n != self && relay.SameMachineName(n, name) {
			return true
		}
	}
	for n := range m.creating {
		if n != self && relay.SameMachineName(n, name) {
			return true
		}
	}
	return false
}

func (m *machineTable) saveLocked() {
	if m.file == "" {
		return
	}
	saved := struct {
		Machines []*fakeMachine  `json:"machines"`
		Kept     []api.ImageInfo `json:"kept"`
	}{Kept: m.kept}
	for _, fm := range m.byName {
		saved.Machines = append(saved.Machines, fm)
	}
	data, _ := json.Marshal(saved)
	tmp := m.file + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, m.file)
	}
}

func standinAddress6(cfg api.MachineConfig) string {
	a, err := netip.ParseAddr(cfg.Address)
	if err != nil || !a.Is4() || cfg.IsolateNetwork {
		return ""
	}
	return fmt.Sprintf("fd5e:1c2b:9a40:1::%x", a.As4()[3])
}

func (m *machineTable) stateLocked(fm *fakeMachine, state string, change func(*api.MachineInfo)) {
	fm.Info.State = state
	switch state {
	case api.MachineRunning:
		now := m.a.Clock()
		fm.Info.PID, fm.Info.Address, fm.Info.StartedMS, fm.Info.ReadyMS = 4000+len(fm.Info.Name), fm.Config.Address, now, now
		fm.Info.Address6 = standinAddress6(fm.Config)
		fm.Info.Error = ""
		fm.Info.Emulation, fm.Info.EmulationReason = "", ""
		if e := fm.Config.Emulation; fm.Info.Image.Arch == "amd64" && e != "" && e != "auto" {
			fm.Info.Emulation, fm.Info.EmulationReason = e, "explicit workload override"
		}
	case api.MachineStopped, api.MachineFailed, api.MachineCreating:
		fm.Info.Emulation, fm.Info.EmulationReason = "", ""
		fm.Info.PID, fm.Info.Address, fm.Info.StartedMS, fm.Info.ReadyMS = 0, "", 0, 0
		fm.Info.Address6 = ""
		if fm.ports != nil {
			fm.ports = nil
			m.portsLocked(fm)
		}
	}
	if state != api.MachineFailed {
		fm.Info.Error = ""
	}
	if change != nil {
		change(&fm.Info)
	}
	m.saveLocked()
	m.notifyLocked(api.MachineState{MachineInfo: fm.Info})
}

func (m *machineTable) notifyLocked(st api.MachineState) {
	m.a.mu.Lock()
	host := m.a.host
	m.a.mu.Unlock()
	if host != nil {
		host.Notify(api.NotifyMachineState, st)
	}
}

func machineConfig(cfg api.MachineConfig) error {
	if cfg.User == "" || cfg.MAC == "" || cfg.Address == "" {
		return fmt.Errorf("the machine's config needs user, mac and address: %+v", cfg)
	}
	if _, err := net.ParseMAC(cfg.MAC); err != nil {
		return err
	}
	if net.ParseIP(cfg.Address).To4() == nil || !strings.HasPrefix(cfg.Address, "198.19.0.") {
		return fmt.Errorf("the machine's address %q is not one of 198.19.0.0/24's", cfg.Address)
	}
	return nil
}

func (a *Agent) machineCall(ctx context.Context, c *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
	m := a.machines()
	if method == api.MethodMachineList {
		m.mu.Lock()
		defer m.mu.Unlock()
		l := api.MachineList{Machines: []api.MachineInfo{}, Images: append([]api.ImageInfo{}, m.kept...)}
		for _, fm := range m.byName {
			l.Machines = append(l.Machines, fm.Info)
		}
		sort.Slice(l.Machines, func(i, j int) bool { return l.Machines[i].Name < l.Machines[j].Name })
		return l, nil
	}
	if method == api.MethodOperationCancel {
		p, err := jsonrpc.Bind[api.OperationID](params)
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		op := m.ops[p.ID]
		m.mu.Unlock()
		if op != nil {
			op.cancel()
		}
		return api.Empty{}, nil
	}
	var p struct {
		Name    string            `json:"name"`
		NewName string            `json:"new_name"`
		GraceMS int               `json:"grace_ms"`
		Force   bool              `json:"force"`
		Config  api.MachineConfig `json:"config"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	if err := api.CheckMachineName(p.Name); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	if p.GraceMS < 0 {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "grace_ms must not be negative")
	}
	switch method {
	case api.MethodMachineCreate:
		cp, err := jsonrpc.Bind[api.MachineCreateParams](params)
		if err != nil {
			return nil, err
		}
		return m.create(ctx, c, cp)
	case api.MethodMachineImport:
		ip, err := jsonrpc.Bind[api.MachineImportParams](params)
		if err != nil {
			return nil, err
		}
		return m.importMachine(ctx, c, ip)
	case api.MethodMachineExport:
		m.mu.Lock()
		fm := m.byName[p.Name]
		var info api.MachineInfo
		if fm != nil {
			info = fm.Info
		}
		m.mu.Unlock()
		if fm == nil {
			return nil, jsonrpc.Errorf(api.CodeNoMachine, "no machine %q", p.Name)
		}
		return m.export(ctx, c, info)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	fm := m.byName[p.Name]
	if fm == nil {
		if m.creating[p.Name] {
			return nil, jsonrpc.Errorf(api.CodeFailed, "the machine %s is being made", p.Name)
		}
		return nil, jsonrpc.Errorf(api.CodeNoMachine, "no machine %q", p.Name)
	}
	switch method {
	case api.MethodMachineInfo:
		return fm.Info, nil
	case api.MethodMachineStart, api.MethodMachineRestart:
		if err := machineConfig(p.Config); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
		}
		fm.Config = p.Config
		if method == api.MethodMachineRestart && fm.Info.State == api.MachineRunning {
			m.stateLocked(fm, api.MachineStopping, nil)
			m.stateLocked(fm, api.MachineStopped, func(i *api.MachineInfo) { i.LastExit = api.ExitStopped })
		}
		if fm.Info.State != api.MachineRunning {
			m.stateLocked(fm, api.MachineStarting, nil)
			m.stateLocked(fm, api.MachineRunning, nil)
		}
		return fm.Info, nil
	case api.MethodMachineStop:
		if fm.Info.State == api.MachineRunning {
			m.stateLocked(fm, api.MachineStopping, nil)
			m.stateLocked(fm, api.MachineStopped, func(i *api.MachineInfo) { i.LastExit = api.ExitStopped })
			a.killSessions(p.Name)
		}
		return fm.Info, nil
	case api.MethodMachineDelete:
		if fm.Info.State == api.MachineRunning {
			m.stateLocked(fm, api.MachineStopping, nil)
			m.stateLocked(fm, api.MachineStopped, func(i *api.MachineInfo) { i.LastExit = api.ExitStopped })
			a.killSessions(p.Name)
		}
		delete(m.byName, p.Name)
		m.saveLocked()
		m.notifyLocked(api.MachineState{MachineInfo: fm.Info, Removed: true})
		return api.Empty{}, nil
	case api.MethodMachineClone, api.MethodMachineRename:
		if err := api.CheckMachineName(p.NewName); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
		}
		self := ""
		if method == api.MethodMachineRename {
			self = p.Name
		}
		if m.takenLocked(p.NewName, self) {
			return nil, jsonrpc.Errorf(api.CodeFailed, "a machine named %s exists", p.NewName)
		}
		if method == api.MethodMachineRename {
			delete(m.byName, p.Name)
			m.notifyLocked(api.MachineState{MachineInfo: fm.Info, Removed: true})
			fm.Info.Name = p.NewName
			m.byName[p.NewName] = fm
			m.saveLocked()
			m.notifyLocked(api.MachineState{MachineInfo: fm.Info})
			return fm.Info, nil
		}
		if err := machineConfig(p.Config); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
		}
		clone := &fakeMachine{Info: fm.Info, Config: p.Config, User: fm.User}
		clone.Info.Name, clone.Info.UUID, clone.Info.DiskBytes, clone.Info.LastExit = p.NewName, newUUID(), 0, ""
		m.byName[p.NewName] = clone
		m.stateLocked(clone, api.MachineStopped, nil)
		return clone.Info, nil
	}
	return nil, jsonrpc.Errorf(api.CodeNotImplemented, "%s is not implemented by the stand-in agent", method)
}

func (a *Agent) killSessions(machine string) {
	a.mu.Lock()
	var ss []*fakeSession
	for _, s := range a.sessions {
		if s.machine == machine {
			ss = append(ss, s)
		}
	}
	a.mu.Unlock()
	for _, s := range ss {
		select {
		case s.signal <- 9:
		default:
		}
	}
}

func (a *Agent) StopAll() {
	m := a.machines()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, fm := range m.byName {
		if fm.Info.State == api.MachineRunning {
			m.stateLocked(fm, api.MachineStopping, nil)
			m.stateLocked(fm, api.MachineStopped, func(i *api.MachineInfo) { i.LastExit = api.ExitStopped })
			a.killSessions(fm.Info.Name)
		}
	}
}

func (m *machineTable) startOp(c *jsonrpc.Conn, method string, stream func(net.Conn)) *fakeOp {
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.nextOp++
	op := &fakeOp{id: 100 + m.nextOp, method: method, conn: c, ctx: ctx, cancel: cancel, stream: stream}
	if stream != nil {
		op.token = newToken()
	}
	m.ops[op.id] = op
	m.mu.Unlock()
	go func() {
		select {
		case <-c.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	return op
}

func (m *machineTable) endOp(op *fakeOp) {
	m.mu.Lock()
	delete(m.ops, op.id)
	m.mu.Unlock()
	op.cancel()
}

func (op *fakeOp) progress(p api.OperationProgress) {
	p.ID, p.Method = op.id, op.method
	if p.State == "" {
		p.State = api.OperationRunning
	}
	op.conn.Notify(api.NotifyOperationProgress, p)
}

func (op *fakeOp) fail(phase string, err error) {
	if op.ctx.Err() != nil {
		op.progress(api.OperationProgress{State: api.OperationCancelled, Phase: phase})
		return
	}
	op.progress(api.OperationProgress{State: api.OperationFailed, Phase: phase, Error: err.Error()})
}

func (a *Agent) attachOpStream(token string, conn net.Conn) bool {
	m := a.machines()
	m.mu.Lock()
	var op *fakeOp
	for _, o := range m.ops {
		if o.token == token && o.stream != nil {
			op = o
		}
	}
	var fn func(net.Conn)
	if op != nil {
		fn, op.stream = op.stream, nil
	}
	m.mu.Unlock()
	if op == nil {
		return false
	}
	if relay.WriteOK(conn) != nil {
		op.cancel()
		return true
	}
	stop := context.AfterFunc(op.ctx, func() { conn.Close() })
	defer stop()
	fn(conn)
	return true
}

func (op *fakeOp) waitStream(attached <-chan struct{}) error {
	select {
	case <-attached:
		return nil
	case <-op.ctx.Done():
		return errors.New("cancelled")
	case <-time.After(attachBound):
		return errors.New("nobody attached the operation's stream within 10s")
	}
}

func (m *machineTable) create(ctx context.Context, c *jsonrpc.Conn, p api.MachineCreateParams) (any, error) {
	if err := machineConfig(p.Config); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	ref := p.Image.ImageRef
	if ref.Distro == "" || ref.Release == "" || ref.Arch == "" || ref.Serial == "" || (ref.Variant != api.VariantDefault && ref.Variant != api.VariantCloud) {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "the image is not named in full: %+v", ref)
	}
	if p.User.Name == "" || p.User.UID <= 0 {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "the user needs a name and a uid: %+v", p.User)
	}
	if len(p.UserData) > api.MaxUserDataBytes {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "user data is more than %d bytes", api.MaxUserDataBytes)
	}
	if p.UserData != "" && ref.Variant != api.VariantCloud {
		return nil, jsonrpc.Errorf(api.CodeFailed, "the %s variant has no cloud-init: user data needs the cloud variant", ref.Variant)
	}
	m.mu.Lock()
	if m.takenLocked(p.Name, "") {
		m.mu.Unlock()
		return nil, jsonrpc.Errorf(api.CodeFailed, "a machine named %s exists", p.Name)
	}
	kept := false
	for _, k := range m.kept {
		if k.ImageRef == ref {
			kept = true
		}
	}
	m.creating[p.Name] = true
	m.mu.Unlock()

	attached := make(chan struct{})
	var tarBytes int64
	var tarErr error
	var stream func(net.Conn)
	if !kept {
		stream = func(conn net.Conn) {
			defer close(attached)
			tarBytes, tarErr = readTar(conn)
		}
	}
	op := m.startOp(c, api.MethodMachineCreate, stream)
	jsonrpc.AfterReply(ctx, func() { go m.createRun(op, p, kept, attached, &tarBytes, &tarErr) })
	return api.Operation{ID: op.id, Token: op.token}, nil
}

func (m *machineTable) createRun(op *fakeOp, p api.MachineCreateParams, kept bool, attached chan struct{}, tarBytes *int64, tarErr *error) {
	defer m.endOp(op)
	failed := func(phase string, err error) {
		m.mu.Lock()
		delete(m.creating, p.Name)
		m.mu.Unlock()
		op.fail(phase, err)
	}
	if !kept {
		if err := op.waitStream(attached); err != nil {
			failed(api.PhaseImage, err)
			return
		}
		if *tarErr != nil || op.ctx.Err() != nil {
			failed(api.PhaseImage, fmt.Errorf("the image's stream: %v", *tarErr))
			return
		}
		op.progress(api.OperationProgress{Phase: api.PhaseImage, DoneBytes: *tarBytes})
		m.mu.Lock()
		out := m.kept[:0:0]
		for _, k := range m.kept {
			if k.Distro != p.Image.Distro || k.Release != p.Image.Release || k.Arch != p.Image.Arch || k.Variant != p.Image.Variant {
				out = append(out, k)
			}
		}
		m.kept = append(out, api.ImageInfo{ImageRef: p.Image.ImageRef, DiskBytes: *tarBytes})
		m.saveLocked()
		m.mu.Unlock()
	}
	fm := &fakeMachine{
		Info:   api.MachineInfo{Name: p.Name, State: api.MachineCreating, Image: p.Image.ImageRef, UUID: newUUID(), Init: "systemd", DiskBytes: 8 << 20},
		Config: p.Config, User: p.User,
	}
	if p.Image.Distro == "alpine" {
		fm.Info.Init = "busybox"
	}
	op.progress(api.OperationProgress{Phase: api.PhaseSetup})
	op.progress(api.OperationProgress{Phase: api.PhaseBoot})
	select {
	case <-time.After(m.a.CreateTime):
	case <-op.ctx.Done():
	}
	if op.ctx.Err() != nil {
		failed(api.PhaseBoot, op.ctx.Err())
		return
	}
	if m.a.CreateFail != "" {
		failed(api.PhaseBoot, errors.New(m.a.CreateFail))
		return
	}
	var ci *api.CloudInit
	if p.UserData != "" {
		op.progress(api.OperationProgress{Phase: api.PhaseCloudInit})
		ci = &api.CloudInit{Status: "done"}
		if strings.Contains(p.UserData, "false") {
			ci = &api.CloudInit{Status: "error", Errors: []string{"runcmd: exit status 1"}, OutputTail: []string{"+ false"}}
		}
	}
	m.mu.Lock()
	delete(m.creating, p.Name)
	m.byName[p.Name] = fm
	m.notifyLocked(api.MachineState{MachineInfo: fm.Info})
	m.stateLocked(fm, api.MachineStarting, nil)
	if hold := m.a.StartingTime; hold > 0 {
		m.mu.Unlock()
		select {
		case <-time.After(hold):
		case <-op.ctx.Done():
		}
		m.mu.Lock()
	}
	m.stateLocked(fm, api.MachineRunning, nil)
	info := fm.Info
	m.mu.Unlock()
	result, _ := json.Marshal(api.MachineCreated{Machine: info, CloudInit: ci})
	op.progress(api.OperationProgress{State: api.OperationDone, Phase: api.PhaseBoot, Result: result})
}

func readTar(r io.Reader) (int64, error) {
	cr := &countReader{r: r}
	tr := tar.NewReader(cr)
	for {
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return cr.n, err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return cr.n, err
		}
	}
	io.Copy(io.Discard, cr)
	return cr.n, nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

const archiveConfig = "_fev/v1/config.json"

func (m *machineTable) export(ctx context.Context, c *jsonrpc.Conn, info api.MachineInfo) (any, error) {
	attached := make(chan struct{})
	var written int64
	var werr error
	op := m.startOp(c, api.MethodMachineExport, func(conn net.Conn) {
		defer close(attached)
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		config, _ := json.Marshal(map[string]any{"name": info.Name, "image": info.Image, "uuid": info.UUID})
		at := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
		tw.WriteHeader(&tar.Header{Name: "_fev/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: at, Format: tar.FormatPAX})
		tw.WriteHeader(&tar.Header{Name: "_fev/v1/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: at, Format: tar.FormatPAX})
		tw.WriteHeader(&tar.Header{Name: archiveConfig, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(config)), ModTime: at, Format: tar.FormatPAX})
		tw.Write(config)
		tw.WriteHeader(&tar.Header{Name: "rootfs/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: at, Format: tar.FormatPAX})
		hostname := []byte(info.Name + "\n")
		tw.WriteHeader(&tar.Header{Name: "rootfs/etc/hostname", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(hostname)), ModTime: at, Format: tar.FormatPAX})
		tw.Write(hostname)
		filler := bytes.Repeat([]byte(info.Name+" "), 64<<10/(len(info.Name)+1))
		tw.WriteHeader(&tar.Header{Name: "rootfs/var/data", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(filler)), ModTime: at, Format: tar.FormatPAX})
		tw.Write(filler)
		tw.Close()
		written, werr = io.Copy(conn, &buf)
		if werr == nil {
			if cw, ok := conn.(interface{ CloseWrite() error }); ok {
				werr = cw.CloseWrite()
			}
		}
	})
	jsonrpc.AfterReply(ctx, func() {
		go func() {
			defer m.endOp(op)
			if err := op.waitStream(attached); err != nil {
				op.fail(api.PhaseArchive, err)
				return
			}
			if werr != nil || op.ctx.Err() != nil {
				op.fail(api.PhaseArchive, fmt.Errorf("the archive's stream: %v", werr))
				return
			}
			op.progress(api.OperationProgress{Phase: api.PhaseArchive, DoneBytes: written, TotalBytes: written})
			result, _ := json.Marshal(api.MachineExported{Bytes: written})
			op.progress(api.OperationProgress{State: api.OperationDone, Phase: api.PhaseArchive, DoneBytes: written, TotalBytes: written, Result: result})
		}()
	})
	return api.Operation{ID: op.id, Token: op.token}, nil
}

func (m *machineTable) importMachine(ctx context.Context, c *jsonrpc.Conn, p api.MachineImportParams) (any, error) {
	if err := machineConfig(p.Config); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	m.mu.Lock()
	if m.takenLocked(p.Name, "") {
		m.mu.Unlock()
		return nil, jsonrpc.Errorf(api.CodeFailed, "a machine named %s exists", p.Name)
	}
	m.creating[p.Name] = true
	m.mu.Unlock()
	attached := make(chan struct{})
	var config struct {
		Image api.ImageRef `json:"image"`
	}
	var read int64
	var rerr error
	op := m.startOp(c, api.MethodMachineImport, func(conn net.Conn) {
		defer close(attached)
		cr := &countReader{r: conn}
		tr := tar.NewReader(cr)
		h, err := tr.Next()
		for err == nil && h.Name != archiveConfig && strings.HasPrefix(h.Name, "_fev/") {
			h, err = tr.Next()
		}
		if err != nil || h.Name != archiveConfig {
			rerr = fmt.Errorf("the archive has no %s before its root", archiveConfig)
			return
		}
		if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&config); err != nil {
			rerr = fmt.Errorf("%s: %v", archiveConfig, err)
			return
		}
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				rerr = err
				return
			}
			if !strings.HasPrefix(h.Name, "rootfs/") {
				rerr = fmt.Errorf("the archive holds %s, outside rootfs/", h.Name)
				return
			}
			io.Copy(io.Discard, tr)
		}
		io.Copy(io.Discard, cr)
		read = cr.n
	})
	jsonrpc.AfterReply(ctx, func() {
		go func() {
			defer m.endOp(op)
			failed := func(err error) {
				m.mu.Lock()
				delete(m.creating, p.Name)
				m.mu.Unlock()
				op.fail(api.PhaseArchive, err)
			}
			if err := op.waitStream(attached); err != nil {
				failed(err)
				return
			}
			if rerr != nil || op.ctx.Err() != nil {
				failed(fmt.Errorf("the archive: %v", rerr))
				return
			}
			op.progress(api.OperationProgress{Phase: api.PhaseArchive, DoneBytes: read})
			op.progress(api.OperationProgress{Phase: api.PhaseSetup})
			m.mu.Lock()
			delete(m.creating, p.Name)
			fm := &fakeMachine{Info: api.MachineInfo{Name: p.Name, Image: config.Image, UUID: newUUID(), Init: "systemd", DiskBytes: read}, Config: p.Config, User: api.MachineUser{Name: p.Config.User, UID: 501}}
			m.byName[p.Name] = fm
			m.stateLocked(fm, api.MachineStopped, nil)
			info := fm.Info
			m.mu.Unlock()
			result, _ := json.Marshal(info)
			op.progress(api.OperationProgress{State: api.OperationDone, Phase: api.PhaseSetup, Result: result})
		}()
	})
	return api.Operation{ID: op.id, Token: op.token}, nil
}

var servedAhead = map[string]bool{
	api.MethodMachineCreate: true, api.MethodMachineDelete: true, api.MethodMachineClone: true,
	api.MethodMachineRename: true, api.MethodMachineExport: true, api.MethodMachineImport: true,
	api.MethodOperationCancel: true,
	api.MethodDebugStatus:     true, api.MethodDebugReset: true,
}
