package machines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"
	"fev/proto/relay"

	"fev/guest/internal/logbuf"
	"fev/guest/storage"
)

type Progress func(phase string, done, total int64)

func (p Progress) say(phase string, done, total int64) {
	if p != nil {
		p(phase, done, total)
	}
}

func (rt *Runtime) storage() *storage.Machines {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.st
}

func (rt *Runtime) diskBytes(name string) int64 {
	st := rt.storage()
	if st == nil {
		return 0
	}
	n, err := st.Usage(name)
	if err != nil {
		return 0
	}
	return n
}

func (rt *Runtime) tidy() {
	names, err := Records(rt.o.DataDir)
	if err != nil {
		rt.o.Log.Logf(logbuf.Warning, "machines: the records: %v", err)
	}
	for _, name := range names {
		if st, err := os.Stat(filepath.Join(rt.o.DataDir, name)); err == nil && st.IsDir() {
			if err := rt.viewAdd(name); err != nil {
				rt.o.Log.Logf(logbuf.Warning, "machines: %v", err)
			}
			continue
		}
		rt.o.Log.Logf(logbuf.Warning, "machines: the record of %s has no files: an operation was stopped by force; removed", name)
		RemoveRecord(rt.o.DataDir, name)
	}
}

func (rt *Runtime) takenLocked(name string, self *machine) error {
	if err := api.CheckMachineName(name); err != nil {
		return err
	}
	selfName := ""
	if self != nil {
		selfName = self.name
	}
	if name != selfName {
		if _, err := os.Lstat(filepath.Join(rt.o.DataDir, name)); err == nil {
			return fmt.Errorf("%w: %s", storage.ErrMachineExists, name)
		}
	}
	entries, err := os.ReadDir(rt.o.DataDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() != selfName && relay.SameMachineName(e.Name(), name) {
			return fmt.Errorf("%w: %s (names that differ only in case are one name)", storage.ErrMachineExists, e.Name())
		}
	}
	for other, m := range rt.making {
		if m != self && relay.SameMachineName(other, name) {
			return fmt.Errorf("%w: %s (it is being made)", storage.ErrMachineExists, other)
		}
	}
	for other, m := range rt.runNames {
		if m != self && relay.SameMachineName(other, name) {
			return fmt.Errorf("a running machine was called %s when it started: the name is free once it stops", other)
		}
	}
	if rt.o.CgroupDir != "" {
		if st, err := os.Lstat(filepath.Join(rt.o.CgroupDir, name)); err == nil && !st.IsDir() {
			return fmt.Errorf("%q is not a machine's name here: it is one of the kernel's files in %s", name, rt.o.CgroupDir)
		}
	}
	return nil
}

func (rt *Runtime) reserve(name string, pending *Record) (*machine, error) {
	if err := rt.available(); err != nil {
		return nil, err
	}
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		return nil, errors.New("the guest is shutting down")
	}
	if err := rt.takenLocked(name, nil); err != nil {
		rt.mu.Unlock()
		return nil, err
	}
	m := &machine{rt: rt, name: name, state: api.MachineCreating, pending: pending}
	rt.byName[name] = m
	rt.making[name] = m
	rt.mu.Unlock()
	m.notify(false)
	return m, nil
}

func (m *machine) notify(removed bool) {
	if m.rt.o.Notify == nil {
		return
	}
	i := m.info(nil)
	if rec, err := ReadRecord(m.rt.o.DataDir, i.Name); err == nil && !removed {
		i = m.info(rec)
	}
	m.rt.o.Notify(api.MachineState{MachineInfo: i, Removed: removed})
}

func (rt *Runtime) forget(m *machine) {
	name := m.Name()
	rt.mu.Lock()
	if rt.byName[name] == m {
		delete(rt.byName, name)
	}
	if rt.making[name] == m {
		delete(rt.making, name)
	}
	rt.mu.Unlock()
	m.notify(true)
}

func (rt *Runtime) made(m *machine) {
	name := m.Name()
	if err := rt.viewAdd(name); err != nil {
		rt.o.Log.Logf(logbuf.Warning, "machines: %v", err)
	}
	m.mu.Lock()
	m.state, m.pending = api.MachineStopped, nil
	m.mu.Unlock()
	rt.mu.Lock()
	if rt.making[name] == m {
		delete(rt.making, name)
	}
	rt.mu.Unlock()
	m.notify(false)
}

func checkImageRef(ref api.ImageRef) error {
	for _, p := range []string{ref.Distro, ref.Release, ref.Arch, ref.Variant, ref.Serial} {
		if p == "" || p == "." || p == ".." || len(p) > 128 || p[0] == '.' || containsAny(p, "/\x00") {
			return fmt.Errorf("the image %s/%s/%s/%s/%s: each part must be a plain name", ref.Distro, ref.Release, ref.Arch, ref.Variant, ref.Serial)
		}
	}
	if ref.Arch != "arm64" && ref.Arch != "amd64" {
		return fmt.Errorf("the image is for %s; machines are arm64 or amd64", ref.Arch)
	}
	return nil
}

func containsAny(s, chars string) bool {
	for _, c := range chars {
		for _, d := range s {
			if c == d {
				return true
			}
		}
	}
	return false
}

type Creation struct {
	rt        *Runtime
	m         *machine
	create    *api.MachineCreateParams
	cfg       api.MachineConfig
	NeedImage bool
	ended     bool
}

func (rt *Runtime) BeginCreate(p api.MachineCreateParams) (*Creation, error) {
	if err := checkImageRef(p.Image.ImageRef); err != nil {
		return nil, err
	}
	if err := checkUserName(p.User.Name); err != nil {
		return nil, err
	}
	if p.User.UID <= 0 || p.User.UID >= 60000 {
		return nil, fmt.Errorf("uid %d is not one a machine's default user can have", p.User.UID)
	}
	if err := CheckUserData(p.Image.ImageRef, p.UserData); err != nil {
		return nil, err
	}
	m, err := rt.reserve(p.Name, &Record{Name: p.Name, Image: p.Image.ImageRef, User: p.User})
	if err != nil {
		return nil, err
	}
	c := &Creation{rt: rt, m: m, create: &p, cfg: p.Config}
	_, kept := ReadImageTemplates(rt.o.DataDir, p.Image.ImageRef)
	c.NeedImage = !kept || !rt.storage().HasImage(p.Image.ImageRef)
	return c, nil
}

func (rt *Runtime) BeginImport(name string, cfg api.MachineConfig) (*Creation, error) {
	m, err := rt.reserve(name, &Record{Name: name})
	if err != nil {
		return nil, err
	}
	return &Creation{rt: rt, m: m, cfg: cfg, NeedImage: true}, nil
}

func (c *Creation) Abandon() {
	if c.ended {
		return
	}
	c.ended = true
	c.rt.forget(c.m)
}

type countReader struct {
	ctx   context.Context
	r     io.Reader
	n     int64
	phase string
	p     Progress
}

func (c *countReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(b)
	c.n += int64(n)
	c.p.say(c.phase, c.n, 0)
	return n, err
}

type countWriter struct {
	ctx   context.Context
	w     io.Writer
	n     int64
	phase string
	p     Progress
}

func (c *countWriter) Write(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.w.Write(b)
	c.n += int64(n)
	c.p.say(c.phase, c.n, 0)
	return n, err
}

func templateConfig(p *api.MachineCreateParams) map[string]string {
	c := map[string]string{}
	if p.UserData != "" {
		c["cloud-init.user-data"] = p.UserData
	}
	return c
}

func (c *Creation) Create(ctx context.Context, image io.Reader, progress Progress) (api.MachineCreated, error) {
	rt, m, p := c.rt, c.m, c.create
	c.ended = true
	ref := p.Image.ImageRef
	st := rt.storage()
	fail := func(err error) (api.MachineCreated, error) {
		rt.forget(m)
		return api.MachineCreated{}, err
	}
	if c.NeedImage {
		if image == nil {
			return fail(errors.New("the guest does not have the image and none was sent"))
		}
		if err := rt.receiveImage(ctx, ref, p.Image.Templates, image, progress); err != nil {
			return fail(err)
		}
	}
	templates, ok := ReadImageTemplates(rt.o.DataDir, ref)
	if !ok {
		return fail(fmt.Errorf("the guest keeps the image %s %s but not its templates", ref.Distro, ref.Serial))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	progress.say(api.PhaseSetup, 0, 0)
	uuid, err := NewUUID()
	if err != nil {
		return fail(err)
	}
	rec := &Record{Name: p.Name, UUID: uuid, Image: ref, User: p.User, Setup: true}
	began := time.Now()
	var written []string
	err = st.CreateWith(p.Name, ref, func(root string) error {
		var err error
		written, err = applyTemplates(root, templates, TriggerCreate, TemplateContext{Name: p.Name, Arch: ref.Arch, Config: templateConfig(p)})
		if err != nil {
			return fmt.Errorf("the image's templates: %w", err)
		}
		if ref.Variant == api.VariantCloud {
			fd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			defer unix.Close(fd)
			if err := WriteCloudSeed(fd, CloudSeed{Name: p.Name, UUID: uuid, User: p.User.Name, UserData: p.UserData, Password: p.User.PasswordHash != ""}); err != nil {
				return fmt.Errorf("cloud-init's seed: %w", err)
			}
		}
		return WriteRecord(rt.o.DataDir, rec)
	})
	if err != nil {
		RemoveRecord(rt.o.DataDir, p.Name)
		return fail(err)
	}
	rt.o.Log.Logf(logbuf.Info, "machines: %s: made from %s %s %s %s in %d ms; templates: %v", p.Name, ref.Distro, ref.Release, ref.Variant, ref.Serial, time.Since(began).Milliseconds(), written)
	rt.made(m)
	if err := ctx.Err(); err != nil {
		rt.removeMade(p.Name)
		return api.MachineCreated{}, err
	}
	progress.say(api.PhaseBoot, 0, 0)
	info, err := rt.startCtx(ctx, p.Name, p.Config)
	if ctx.Err() != nil {
		rt.removeMade(p.Name)
		return api.MachineCreated{}, ctx.Err()
	}
	if err != nil {
		return api.MachineCreated{Machine: info}, err
	}
	out := api.MachineCreated{Machine: info}
	cloud, err := rt.FirstBoot(m.Name())
	out.CloudInit = cloud
	if err != nil {
		return out, err
	}
	out.Machine, _ = rt.Info(m.Name())
	return out, nil
}

func (rt *Runtime) receiveImage(ctx context.Context, ref api.ImageRef, templates []api.ImageTemplate, image io.Reader, progress Progress) error {
	st := rt.storage()
	progress.say(api.PhaseImage, 0, 0)
	began := time.Now()
	b, err := st.BeginImage(ref)
	if err != nil {
		return err
	}
	fd, err := unix.Open(b.Path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		b.Abort()
		return err
	}
	cr := &countReader{ctx: ctx, r: image, phase: api.PhaseImage, p: progress}
	n, warnings, err := Unpack(fd, cr)
	unix.Close(fd)
	if err == nil {
		_, err = io.Copy(io.Discard, cr)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		b.Abort()
		return fmt.Errorf("the image, after %d bytes: %w", cr.n, err)
	}
	for i, w := range warnings {
		if i == 5 {
			rt.o.Log.Logf(logbuf.Warning, "machines: the image %s %s: %d more such", ref.Distro, ref.Serial, len(warnings)-i)
			break
		}
		rt.o.Log.Logf(logbuf.Warning, "machines: the image %s %s: %s", ref.Distro, ref.Serial, w)
	}
	if err := WriteImageTemplates(rt.o.DataDir, ref, templates); err != nil {
		b.Abort()
		return fmt.Errorf("keep the image's templates: %w", err)
	}
	if err := b.Commit(); err != nil {
		return fmt.Errorf("put the image in the cache: %w", err)
	}
	PruneImageTemplates(rt.o.DataDir, ref)
	rt.o.Log.Logf(logbuf.Info, "machines: the image %s %s %s %s: %d entries, %d bytes, in %d ms", ref.Distro, ref.Release, ref.Variant, ref.Serial, n, cr.n, time.Since(began).Milliseconds())
	return nil
}

func (rt *Runtime) startCtx(ctx context.Context, name string, cfg api.MachineConfig) (api.MachineInfo, error) {
	type result struct {
		i   api.MachineInfo
		err error
	}
	done := make(chan result, 1)
	go func() {
		i, err := rt.Start(name, cfg)
		done <- result{i, err}
	}()
	select {
	case r := <-done:
		return r.i, r.err
	case <-ctx.Done():
		rt.Stop(name, 0, true)
		r := <-done
		return r.i, ctx.Err()
	}
}

func (rt *Runtime) removeMade(name string) {
	if err := rt.Delete(name, 0, true); err != nil && !errors.Is(err, ErrNoMachine) {
		rt.o.Log.Logf(logbuf.Error, "machines: %s: what a cancelled operation made could not be removed: %v", name, err)
	}
}

type archiveConfig struct {
	Version  int             `json:"version"`
	Name     string          `json:"name"`
	UUID     string          `json:"uuid"`
	Image    api.ImageRef    `json:"image"`
	User     api.MachineUser `json:"user"`
	Init     string          `json:"init,omitempty"`
	Setup    bool            `json:"setup,omitempty"`
	Online   bool            `json:"online,omitempty"`
	Exported string          `json:"exported"`
}

func (c *Creation) Import(ctx context.Context, archive io.Reader, progress Progress) (api.MachineInfo, error) {
	rt, m := c.rt, c.m
	c.ended = true
	name := m.Name()
	progress.say(api.PhaseArchive, 0, 0)
	uuid, err := NewUUID()
	if err != nil {
		rt.forget(m)
		return api.MachineInfo{}, err
	}
	cr := &countReader{ctx: ctx, r: archive, phase: api.PhaseArchive, p: progress}
	var rec *Record
	_, warnings, err := rt.storage().ImportWith(name, cr, Unpack, func(root string, config []byte) error {
		var a archiveConfig
		if err := json.Unmarshal(config, &a); err != nil {
			return fmt.Errorf("the archive's %s: %w", storage.ArchiveConfig, err)
		}
		if a.Version != 1 {
			return fmt.Errorf("the archive's %s is version %d; this fev reads version 1", storage.ArchiveConfig, a.Version)
		}
		if err := checkImageRef(a.Image); err != nil {
			return fmt.Errorf("the archive's image: %w", err)
		}
		if err := checkUserName(a.User.Name); err != nil {
			return fmt.Errorf("the archive's user: %w", err)
		}
		progress.say(api.PhaseSetup, cr.n, 0)
		said, keys, err := resetIdentity(root, identity{UUID: uuid, OldName: a.Name, NewName: name})
		if err != nil {
			return fmt.Errorf("its new identity: %w", err)
		}
		rt.o.Log.Logf(logbuf.Info, "machines: %s: imported from %s (%s); new identity in %v", name, a.Name, a.UUID, said)
		rec = &Record{Name: name, UUID: uuid, Image: a.Image, User: a.User, Setup: a.Setup, Online: a.Online, Keygen: keys > 0}
		return WriteRecord(rt.o.DataDir, rec)
	})
	if err == nil {
		err = ctx.Err()
		if err != nil {
			rt.storage().Delete(name)
		}
	}
	if err != nil {
		RemoveRecord(rt.o.DataDir, name)
		rt.forget(m)
		return api.MachineInfo{}, err
	}
	for i, w := range warnings {
		if i == 5 {
			break
		}
		rt.o.Log.Logf(logbuf.Warning, "machines: %s: %s", name, w)
	}
	rt.made(m)
	return m.info(rec), nil
}

func (m *machine) freezer() storage.Freezer {
	m.mu.Lock()
	r := m.cur
	m.mu.Unlock()
	if r == nil {
		return nil
	}
	return func() (func(), error) { return freeze(r.group) }
}

func (rt *Runtime) Export(ctx context.Context, name string, w io.Writer, progress Progress) (int64, error) {
	if err := rt.available(); err != nil {
		return 0, err
	}
	rec, err := ReadRecord(rt.o.DataDir, name)
	if err != nil {
		return 0, err
	}
	m := rt.get(name)
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.Name() != name {
		return 0, fmt.Errorf("%w: %s (renamed)", ErrNoMachine, name)
	}
	i := m.info(rec)
	config, _ := json.Marshal(archiveConfig{
		Version: 1, Name: name, UUID: rec.UUID, Image: rec.Image, User: rec.User, Init: i.Init,
		Setup: rec.Setup, Online: rec.Online, Exported: time.Now().UTC().Format(time.RFC3339),
	})
	progress.say(api.PhaseArchive, 0, 0)
	cw := &countWriter{ctx: ctx, w: w, phase: api.PhaseArchive, p: progress}
	n, err := rt.storage().Export(name, config, cw, m.freezer(), nil)
	if err == nil {
		err = ctx.Err()
	}
	return n, err
}

func (rt *Runtime) Clone(name, newName string) (api.MachineInfo, error) {
	if err := rt.available(); err != nil {
		return api.MachineInfo{}, err
	}
	src, err := ReadRecord(rt.o.DataDir, name)
	if err != nil {
		return api.MachineInfo{}, err
	}
	sm := rt.get(name)
	sm.opMu.Lock()
	defer sm.opMu.Unlock()
	if sm.Name() != name {
		return api.MachineInfo{}, fmt.Errorf("%w: %s (renamed)", ErrNoMachine, name)
	}
	uuid, err := NewUUID()
	if err != nil {
		return api.MachineInfo{}, err
	}
	rec := &Record{Name: newName, UUID: uuid, Image: src.Image, User: src.User, Setup: src.Setup, Online: src.Online, Keygen: src.Keygen}
	m, err := rt.reserve(newName, rec)
	if err != nil {
		return api.MachineInfo{}, err
	}
	err = rt.storage().Clone(name, newName, sm.freezer(), func(root string) error {
		said, keys, err := resetIdentity(root, identity{UUID: uuid, OldName: name, NewName: newName})
		if err != nil {
			return fmt.Errorf("its new identity: %w", err)
		}
		rec.Keygen = rec.Keygen || keys > 0
		rt.o.Log.Logf(logbuf.Info, "machines: %s: cloned from %s; new identity in %v", newName, name, said)
		return WriteRecord(rt.o.DataDir, rec)
	})
	if err != nil {
		RemoveRecord(rt.o.DataDir, newName)
		rt.forget(m)
		return api.MachineInfo{}, err
	}
	rt.made(m)
	return m.info(rec), nil
}

func (rt *Runtime) Delete(name string, grace time.Duration, force bool) error {
	if err := rt.available(); err != nil {
		return err
	}
	if _, err := ReadRecord(rt.o.DataDir, name); err != nil {
		return err
	}
	m := rt.get(name)
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	if m.name != name {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s (renamed)", ErrNoMachine, name)
	}
	if m.state == api.MachineCreating {
		m.mu.Unlock()
		return fmt.Errorf("%s is being made", name)
	}
	m.removing = true
	m.mu.Unlock()
	undo := func() {
		m.mu.Lock()
		m.removing = false
		m.mu.Unlock()
	}
	m.stop(grace, force)
	if err := rt.viewRemove(name); err != nil {
		undo()
		return err
	}
	held, err := rt.storage().DeleteHeld(name)
	if err != nil {
		rt.viewAdd(name)
		undo()
		return err
	}
	for _, h := range held {
		rt.o.Log.Logf(logbuf.Info, "machines: %s: deleted; still mounted by %s, which keeps its space until it ends", name, h)
	}
	if err := RemoveRecord(rt.o.DataDir, name); err != nil {
		rt.o.Log.Logf(logbuf.Error, "machines: %s: its record could not be removed: %v", name, err)
	}
	m.mu.Lock()
	clog := m.clog
	m.clog = nil
	m.mu.Unlock()
	if clog != nil {
		clog.endBoot()
	}
	for _, p := range []string{ConsoleFile(rt.o.DataDir, name), ConsoleFile(rt.o.DataDir, name) + ".1"} {
		os.Remove(p)
	}
	os.RemoveAll(filepath.Join(rt.o.RunDir, name))
	rt.o.Log.Logf(logbuf.Info, "machines: %s: deleted", name)
	rt.forget(m)
	return nil
}

type renamer interface {
	Rename(old, new string)
}

func (rt *Runtime) Rename(name, newName string) (api.MachineInfo, error) {
	if err := rt.available(); err != nil {
		return api.MachineInfo{}, err
	}
	if err := api.CheckMachineName(newName); err != nil {
		return api.MachineInfo{}, err
	}
	rec, err := ReadRecord(rt.o.DataDir, name)
	if err != nil {
		return api.MachineInfo{}, err
	}
	if newName == name {
		return rt.get(name).info(rec), nil
	}
	m := rt.get(name)
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	switch {
	case m.name != name:
		m.mu.Unlock()
		return api.MachineInfo{}, fmt.Errorf("%w: %s (renamed)", ErrNoMachine, name)
	case m.state == api.MachineCreating || m.removing:
		m.mu.Unlock()
		return api.MachineInfo{}, fmt.Errorf("%s is being made or deleted", name)
	}
	rt.mu.Lock()
	err = rt.takenLocked(newName, m)
	rt.mu.Unlock()
	m.mu.Unlock()
	if err != nil {
		return api.MachineInfo{}, err
	}
	nrec := *rec
	nrec.Name = newName
	if err := WriteRecord(rt.o.DataDir, &nrec); err != nil {
		return api.MachineInfo{}, err
	}
	if err := rt.storage().Rename(name, newName); err != nil {
		RemoveRecord(rt.o.DataDir, newName)
		return api.MachineInfo{}, err
	}
	if err := RemoveRecord(rt.o.DataDir, name); err != nil {
		rt.o.Log.Logf(logbuf.Error, "machines: %s: the old record could not be removed: %v", name, err)
	}
	said, err := renameHost(filepath.Join(rt.o.DataDir, newName), name, newName)
	if err != nil {
		rt.o.Log.Logf(logbuf.Warning, "machines: %s: its host name in its files: %v", newName, err)
	}
	m.mu.Lock()
	if m.clog != nil {
		m.clog.move(ConsoleFile(rt.o.DataDir, newName))
	} else {
		for _, sfx := range []string{"", ".1"} {
			os.Rename(ConsoleFile(rt.o.DataDir, name)+sfx, ConsoleFile(rt.o.DataDir, newName)+sfx)
		}
	}
	m.mu.Unlock()
	if err := rt.viewAdd(newName); err != nil {
		rt.o.Log.Logf(logbuf.Warning, "machines: %v", err)
	}
	if err := rt.viewRemove(name); err != nil {
		rt.o.Log.Logf(logbuf.Warning, "machines: %v", err)
	}
	old := m.info(rec)
	m.mu.Lock()
	r := m.cur
	rt.mu.Lock()
	if rt.byName[name] == m {
		delete(rt.byName, name)
	}
	rt.byName[newName] = m
	rt.mu.Unlock()
	m.name = newName
	if r != nil {
		r.rec.Name = newName
	}
	m.mu.Unlock()
	if r == nil {
		os.RemoveAll(filepath.Join(rt.o.RunDir, name))
	}
	if n, ok := rt.o.Network.(renamer); ok && r != nil {
		n.Rename(name, newName)
	}
	rt.o.Log.Logf(logbuf.Info, "machines: %s: renamed %s (running: %v); its files say the new name in %v", newName, name, r != nil, said)
	if rt.o.Notify != nil {
		rt.o.Notify(api.MachineState{MachineInfo: old, Removed: true})
	}
	m.notify(false)
	return m.info(&nrec), nil
}

func (c *consoleLog) move(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, sfx := range []string{".1", ""} {
		if err := os.Rename(c.path+sfx, path+sfx); err != nil && !errors.Is(err, os.ErrNotExist) {
			c.fail(err)
		}
	}
	c.path = path
}
