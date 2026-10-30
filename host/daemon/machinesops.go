package daemon

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"

	"fev/host/daemon/config"
	"fev/host/docker/vsock"
	"fev/host/images"
	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
	"fev/proto/relay"
)

const archiveConfig = "_fev/v1/config.json"

const maxArchiveConfig = 1 << 20

const guestCancelBound = 30 * time.Second

func openStream(ctx context.Context, vm VM, token string) (*net.UnixConn, error) {
	return vsock.Relay{Path: vm.VsockPath(relay.Port)}.Open(ctx, relay.Request{Kind: relay.Stream, Target: token})
}

func opStream(vm VM) images.OpenStream {
	return func(ctx context.Context, token string) (images.Stream, error) { return openStream(ctx, vm, token) }
}

type guestOp struct {
	op    *Operation
	final chan agent.OperationProgress
}

func newGuestOp(op *Operation) *guestOp {
	return &guestOp{op: op, final: make(chan agent.OperationProgress, 1)}
}

func (g *guestOp) progress(p agent.OperationProgress) {
	if p.State == agent.OperationRunning {
		g.op.Progress(p.Phase, p.DoneBytes, p.TotalBytes)
		return
	}
	select {
	case g.final <- p:
	default:
	}
}

func (g *guestOp) wait(client *agent.Client, id uint64) (agent.OperationProgress, error) {
	select {
	case p := <-g.final:
		return p, nil
	case <-client.Done():
		return agent.OperationProgress{}, errors.New("the VM stopped before the guest finished")
	case <-g.op.Context().Done():
	}
	ctx, cancel := context.WithTimeout(context.Background(), guestCancelBound)
	defer cancel()
	client.OperationCancel(ctx, id)
	select {
	case p := <-g.final:
		return p, nil
	case <-client.Done():
		return agent.OperationProgress{State: agent.OperationCancelled}, nil
	case <-ctx.Done():
		return agent.OperationProgress{}, errors.New("the guest did not say how its cancelled operation ended")
	}
}

func (m *Machines) reserve(name string, image api.Image) (*machineEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.takenLocked(name, ""); err != nil {
		return nil, err
	}
	addr, mac, err := m.allocLocked()
	if err != nil {
		return nil, jsonrpc.Errorf(api.CodeFailed, "%v", err)
	}
	e := &machineEntry{Name: name, Image: image, UID: m.UID, MAC: mac, Address: addr, Created: stamp(time.Now())}
	setStateLocked(e, api.MachineCreating)
	m.entries[name] = e
	m.busy[name]++
	m.changedLocked(e, false)
	return e, nil
}

func (m *Machines) settle(e *machineEntry, made *agent.MachineInfo, user string, keys ...map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy[e.Name]--; m.busy[e.Name] <= 0 {
		delete(m.busy, e.Name)
	}
	if m.entries[e.Name] != e {
		return
	}
	if made == nil {
		delete(m.entries, e.Name)
		m.host.PublishMachine(api.MachinesChanged{Machine: m.view(e), Removed: true})
		return
	}
	values := map[string]any{}
	if user != "" {
		values[config.KeyUsername] = user
	}
	for _, k := range keys {
		for key, v := range k {
			values[key] = v
		}
	}
	m.cfg.RemoveScope(config.MachinePrefix, e.Name)
	if err := m.cfg.AddScope(config.MachinePrefix, e.Name, config.MachineKeys(m.MacUser), values); err != nil {
		m.log.Warn("machines: a new machine's settings cannot be made", "machine", e.Name, "err", err)
	}
	e.startValues = m.cfg.MachineValues(e.Name)
	e.Image = api.Image(made.Image)
	m.applyLocked(e, *made, false)
	if made.State == api.MachineCreating {
		setStateLocked(e, api.MachineStopped)
	}
	if m.def == "" {
		m.def = e.Name
	}
	m.saveLocked()
	m.changedLocked(e, false)
}

func (m *Machines) create(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.MachinesCreateParams](params)
	if err != nil {
		return nil, err
	}
	if err := relay.CheckMachineName(p.Name); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	if p.Distro == "" {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "a machine is made from a distribution: distro is required")
	}
	if _, err := images.Architecture(p.Arch); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	if len(p.UserData) > api.MaxUserDataBytes {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "cloud-init user data is %d bytes; at most %d", len(p.UserData), api.MaxUserDataBytes)
	}
	user := p.User
	if user == "" {
		user = m.MacUser
	}
	if err := config.CheckUsername(user); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "the machine's user: %v", err)
	}
	img, err := images.Resolve(p.Distro, p.Release, p.Arch, p.UserData != "")
	if err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	distro, release := p.Distro, p.Release
	iso, isoKeys, err := m.createIsolation(p)
	if err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
	}
	if err := m.amd64Refusal(img.Arch); err != nil {
		return nil, err
	}
	if p.Emulation != "" && p.Emulation != "auto" && img.Arch != "amd64" {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "emulation %s is for amd64 machines; %s is %s, which runs natively", p.Emulation, p.Name, img.Arch)
	}
	e, err := m.reserve(p.Name, api.Image{Distro: img.Distro, Release: img.Release, Arch: img.Arch, Variant: img.Variant})
	if err != nil {
		return nil, err
	}
	return m.ops.Start(ctx, api.MethodMachinesCreate, p.Name, api.PhaseVerify, func(op *Operation) {
		settled := false
		defer func() {
			if !settled {
				m.settle(e, nil, "")
			}
		}()
		octx := op.Context()
		client, err := m.guest(octx, true)
		if err != nil {
			op.Fail(err)
			return
		}
		m.mu.Lock()
		vm := m.vm
		m.mu.Unlock()
		g := newGuestOp(op)
		cfg := m.configFor(p.Name)
		cfg.User = user
		cfg.Isolated, cfg.IsolateNetwork, cfg.ForwardSSHAgent, cfg.Mounts = iso.Isolated, iso.IsolateNetwork, iso.ForwardSSHAgent, iso.Mounts
		if iso.Emulation != "auto" {
			cfg.Emulation = iso.Emulation
		}
		m.startedWith(p.Name, cfg)
		gp := agent.MachineCreateParams{Name: p.Name, User: agent.MachineUser{Name: user, UID: m.UID, PasswordHash: p.PasswordHash}, Config: cfg, UserData: p.UserData}
		fetched := func(file string, done, total int64) { op.Progress(api.PhaseDownload, done, total) }
		started := time.Now()
		gop, _, sent, err := m.source.Create(octx, client, opStream(vm), distro, release, img.Arch, gp, fetched, g.progress)
		if err != nil {
			m.log.Error("machines: a create failed", "machine", p.Name, "err", err)
			op.Fail(err)
			return
		}
		end, err := g.wait(client, gop.ID)
		if err != nil {
			op.Fail(err)
			return
		}
		switch end.State {
		case agent.OperationDone:
		case agent.OperationCancelled:
			op.Cancelled()
			return
		default:
			op.Fail(errors.New(end.Error))
			return
		}
		var r agent.MachineCreated
		if err := json.Unmarshal(end.Result, &r); err != nil || r.Machine.Name != p.Name {
			op.Fail(fmt.Errorf("the guest's create ended with a result that is not this machine's: %s", clip(string(end.Result), 400)))
			return
		}
		m.log.Info("machines: made", "machine", p.Name, "image", fmt.Sprintf("%s %s %s %s", r.Machine.Image.Distro, r.Machine.Image.Release, r.Machine.Image.Variant, r.Machine.Image.Serial),
			"tar_bytes", sent.TarBytes, "parallel", sent.Parallel, "took", time.Since(started).Round(time.Millisecond))
		m.settle(e, &r.Machine, user, isoKeys)
		settled = true
		m.mu.Lock()
		view := m.view(e)
		m.mu.Unlock()
		result := api.MachineCreated{Machine: view}
		if r.CloudInit != nil {
			result.CloudInit = &api.CloudInit{Status: r.CloudInit.Status, Errors: r.CloudInit.Errors, OutputTail: r.CloudInit.OutputTail, RecoverableErrors: r.CloudInit.RecoverableErrors}
		}
		op.Done(result)
	}), nil
}

func (m *Machines) createIsolation(p api.MachinesCreateParams) (agent.MachineConfig, map[string]any, error) {
	share, _ := m.cfg.InEffect(api.KeyFilesShare)
	c := agent.MachineConfig{Isolated: p.Isolated, IsolateNetwork: p.IsolateNetwork, ForwardSSHAgent: p.ForwardSSHAgent, Share: share == true}
	if p.Emulation != "" && p.Emulation != "auto" && p.Emulation != "fex" && p.Emulation != "rosetta" {
		return c, nil, fmt.Errorf("emulation must be auto, fex or rosetta")
	}
	c.Emulation = p.Emulation
	var mounts []agent.MachineMount
	for _, v := range p.Mounts {
		mm, err := agent.ParseMachineMount(v)
		if err != nil {
			return c, nil, err
		}
		mounts = append(mounts, mm)
	}
	c.Mounts = mounts
	if err := agent.CheckIsolation(c); err != nil {
		return c, nil, err
	}
	if !p.Isolated {
		if p.Emulation != "" {
			return agent.MachineConfig{Emulation: p.Emulation}, map[string]any{api.MachineKeyEmulation: p.Emulation}, nil
		}
		return agent.MachineConfig{}, nil, nil
	}
	for _, mm := range mounts {
		if _, ok := sharePath(m.ShareRoot, mm.Source); !ok {
			return c, nil, fmt.Errorf("%s is outside what the VM is given of the Mac", mm.Source)
		}
	}
	c.Mounts = m.mounts(p.Mounts)
	keys := map[string]any{
		api.MachineKeyIsolated:        true,
		api.MachineKeyIsolateNetwork:  p.IsolateNetwork,
		api.MachineKeyForwardSSHAgent: p.ForwardSSHAgent,
	}
	if p.Emulation != "" {
		keys[api.MachineKeyEmulation] = p.Emulation
	}
	if len(p.Mounts) > 0 {
		keys[api.MachineKeyMounts] = append([]string(nil), p.Mounts...)
	}
	return c, keys, nil
}

func macPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%q is not an absolute path", clip(path, 200))
	}
	return filepath.Clean(path), nil
}

type countWriter struct {
	w  io.Writer
	n  int64
	on func(int64)
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	c.on(c.n)
	return n, err
}

type countReader struct {
	r  io.Reader
	n  atomic.Int64
	on atomic.Pointer[func(int64)]
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	total := c.n.Add(int64(n))
	if on := c.on.Load(); on != nil {
		(*on)(total)
	}
	return n, err
}

func (m *Machines) export(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.MachinesExportParams](params)
	if err != nil {
		return nil, err
	}
	if _, err := m.entry(p.Name); err != nil {
		return nil, err
	}
	path, err := macPath(p.Path)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(filepath.Dir(path)); err != nil || !st.IsDir() {
		return nil, jsonrpc.Errorf(api.CodeFailed, "%s is not a folder on this Mac", filepath.Dir(path))
	}
	if _, err := os.Lstat(path); err == nil && !p.Replace {
		return nil, jsonrpc.Errorf(api.CodeFailed, "%s exists; it is replaced only when asked", path)
	}
	return m.ops.Start(ctx, api.MethodMachinesExport, p.Name, agent.PhaseArchive, func(op *Operation) {
		octx := op.Context()
		client, err := m.guest(octx, true)
		if err != nil {
			op.Fail(err)
			return
		}
		m.mu.Lock()
		vm := m.vm
		m.mu.Unlock()
		tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".fev-*")
		if err != nil {
			op.Fail(err)
			return
		}
		kept := false
		defer func() {
			tmp.Close()
			if !kept {
				os.Remove(tmp.Name())
			}
		}()
		g := newGuestOp(op)
		gop, err := client.MachineExport(octx, p.Name, g.progress)
		if err != nil {
			op.Fail(guestError(err))
			return
		}
		plain, err := m.receiveArchive(octx, vm, gop, tmp, op)
		if err != nil {
			ctx, cancel := context.WithTimeout(context.Background(), guestCancelBound)
			client.OperationCancel(ctx, gop.ID)
			cancel()
			op.Fail(fmt.Errorf("the archive: %w", err))
			return
		}
		end, err := g.wait(client, gop.ID)
		switch {
		case err != nil:
			op.Fail(err)
			return
		case end.State == agent.OperationCancelled:
			op.Cancelled()
			return
		case end.State != agent.OperationDone:
			op.Fail(errors.New(end.Error))
			return
		}
		var r agent.MachineExported
		if json.Unmarshal(end.Result, &r) == nil && r.Bytes != plain {
			op.Fail(fmt.Errorf("the guest wrote %d bytes of the archive and %d arrived", r.Bytes, plain))
			return
		}
		if err := tmp.Sync(); err != nil {
			op.Fail(err)
			return
		}
		st, err := tmp.Stat()
		if err != nil {
			op.Fail(err)
			return
		}
		if p.Replace {
			err = os.Rename(tmp.Name(), path)
		} else {
			err = unix.RenameatxNp(unix.AT_FDCWD, tmp.Name(), unix.AT_FDCWD, path, unix.RENAME_EXCL)
		}
		if err != nil {
			op.Fail(fmt.Errorf("%s: %w", path, err))
			return
		}
		kept = true
		m.log.Info("machines: exported", "machine", p.Name, "path", path, "bytes", st.Size(), "tar_bytes", plain)
		op.Progress(agent.PhaseArchive, plain, plain)
		op.Done(api.MachineExported{Path: path, Bytes: st.Size()})
	}), nil
}

func (m *Machines) receiveArchive(ctx context.Context, vm VM, gop *agent.Operation, f *os.File, op *Operation) (int64, error) {
	if gop.Token == "" {
		return 0, errors.New("the guest gave no stream for the archive")
	}
	stream, err := openStream(ctx, vm, gop.Token)
	if err != nil {
		return 0, err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	zw, err := zstd.NewWriter(f)
	if err != nil {
		return 0, err
	}
	cw := &countWriter{w: zw, on: func(n int64) { op.Progress(agent.PhaseArchive, n, 0) }}
	if _, err := io.Copy(cw, stream); err != nil {
		zw.Close()
		return cw.n, err
	}
	if err := zw.Close(); err != nil {
		return cw.n, err
	}
	return cw.n, ctx.Err()
}

func readArchiveConfig(zr io.Reader, what string) (map[string]json.RawMessage, io.Reader, error) {
	var head bytes.Buffer
	tr := tar.NewReader(io.TeeReader(io.LimitReader(zr, 2*maxArchiveConfig), &head))
	notOne := fmt.Errorf("this is not %s archive: it has no %s before its root", what, archiveConfig)
	var h *tar.Header
	for {
		var err error
		if h, err = tr.Next(); err != nil {
			return nil, nil, notOne
		}
		name := strings.TrimPrefix(path.Clean("/"+h.Name), "/")
		if name == archiveConfig {
			break
		}
		if name != "_fev" && !strings.HasPrefix(name, "_fev/") {
			return nil, nil, notOne
		}
	}
	if h.Size > maxArchiveConfig {
		return nil, nil, fmt.Errorf("%s is %d bytes, more than %d", archiveConfig, h.Size, maxArchiveConfig)
	}
	var cfg map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(tr, maxArchiveConfig)).Decode(&cfg); err != nil {
		return nil, nil, fmt.Errorf("%s: %v", archiveConfig, err)
	}
	return cfg, io.MultiReader(&head, zr), nil
}

func (m *Machines) importMachine(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := jsonrpc.Bind[api.MachinesImportParams](params)
	if err != nil {
		return nil, err
	}
	path, err := macPath(p.Path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, jsonrpc.Errorf(api.CodeFailed, "%v", err)
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, jsonrpc.Errorf(api.CodeFailed, "%s is not a file", path)
	}
	read := &countReader{r: f}
	zr, err := zstd.NewReader(read)
	if err != nil {
		f.Close()
		return nil, jsonrpc.Errorf(api.CodeFailed, "%s: %v", path, err)
	}
	fail := func(format string, args ...any) (any, error) {
		zr.Close()
		f.Close()
		return nil, jsonrpc.Errorf(api.CodeFailed, format, args...)
	}
	cfg, body, err := readArchiveConfig(zr, "a machine's")
	if err != nil {
		return fail("%s: %v", path, err)
	}
	if kind := string(cfg["kind"]); kind != "" && kind != "null" {
		return fail("%s: this is not a machine's archive but a Docker volume's: fev docker volume import takes it", path)
	}
	var was struct {
		Name  string    `json:"name"`
		Image api.Image `json:"image"`
	}
	raw, _ := json.Marshal(cfg)
	json.Unmarshal(raw, &was)
	name := p.Name
	if name == "" {
		name = was.Name
	}
	if err := relay.CheckMachineName(name); err != nil {
		zr.Close()
		f.Close()
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "the machine's name: %v", err)
	}
	e, err := m.reserve(name, was.Image)
	if err != nil {
		zr.Close()
		f.Close()
		return nil, err
	}
	return m.ops.Start(ctx, api.MethodMachinesImport, name, agent.PhaseArchive, func(op *Operation) {
		defer f.Close()
		defer zr.Close()
		settled := false
		defer func() {
			if !settled {
				m.settle(e, nil, "")
			}
		}()
		octx := op.Context()
		client, err := m.guest(octx, true)
		if err != nil {
			op.Fail(err)
			return
		}
		m.mu.Lock()
		vm := m.vm
		m.mu.Unlock()
		g := newGuestOp(op)
		total := st.Size()
		var sent atomic.Int64
		on := func(n int64) {
			sent.Store(n)
			op.Progress(agent.PhaseArchive, n, total)
		}
		read.on.Store(&on)
		gop, err := client.MachineImport(octx, agent.MachineImportParams{Name: name, Config: m.startConfig(name)}, func(p agent.OperationProgress) {
			switch {
			case p.State != agent.OperationRunning:
				g.progress(p)
			case p.Phase != agent.PhaseArchive:
				op.Progress(p.Phase, sent.Load(), total)
			}
		})
		if err != nil {
			op.Fail(guestError(err))
			return
		}
		if err := sendArchive(octx, vm, gop, body); err != nil {
			ctx, cancel := context.WithTimeout(context.Background(), guestCancelBound)
			client.OperationCancel(ctx, gop.ID)
			cancel()
			op.Fail(fmt.Errorf("the archive: %w", err))
			return
		}
		end, err := g.wait(client, gop.ID)
		switch {
		case err != nil:
			op.Fail(err)
			return
		case end.State == agent.OperationCancelled:
			op.Cancelled()
			return
		case end.State != agent.OperationDone:
			op.Fail(errors.New(end.Error))
			return
		}
		var info agent.MachineInfo
		if err := json.Unmarshal(end.Result, &info); err != nil || info.Name != name {
			op.Fail(fmt.Errorf("the guest's import ended with a result that is not this machine's: %s", clip(string(end.Result), 400)))
			return
		}
		m.settle(e, &info, "")
		settled = true
		m.mu.Lock()
		view := m.view(e)
		m.mu.Unlock()
		m.log.Info("machines: imported", "machine", name, "path", path)
		op.Done(view)
	}), nil
}

func sendArchive(ctx context.Context, vm VM, gop *agent.Operation, body io.Reader) error {
	if gop.Token == "" {
		return errors.New("the guest gave no stream for the archive")
	}
	stream, err := openStream(ctx, vm, gop.Token)
	if err != nil {
		return err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	if _, err := io.Copy(stream, body); err != nil {
		return err
	}
	if err := stream.CloseWrite(); err != nil {
		return err
	}
	return ctx.Err()
}

func (m *Machines) amd64Refusal(arch string) error {
	if arch != images.AMD64 {
		return nil
	}
	if on, ok := m.cfg.InEffect(api.KeyEmulationX86); ok && on == false {
		return jsonrpc.Errorf(api.CodeFailed, "an amd64 machine runs under x86 emulation, which is off (%s): turn it on with fev config set %s true", api.KeyEmulationX86, api.KeyEmulationX86)
	}
	return nil
}
