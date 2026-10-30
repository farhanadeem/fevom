package nfsview

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	api "fev/proto/agent"
)

type Objects struct {
	tree        *Tree
	ops         Ops
	images      Lazy
	removed     func([]string)
	logf        func(string, ...any)
	engineRoots string

	ended chan struct{}

	mu       sync.Mutex
	closed   bool
	isolated map[string]bool
	machines map[string]MachineSpec
	wantM    map[string]MachineSpec
	volumes  map[string]VolumeSpec
	images_  map[string]ImageSpec
	conts    map[string]*shown
	released map[string]bool
}

type Ops interface {
	MakeDir(path string) error
	RemoveDir(path string) error
	Bind(src, dst string) error
	Move(from, to string) error
	Release(path string, whole bool) error
	Flush() error
	CloneFrom(nsPath string, pid int, clones []Clone) ([]error, error)
}

type Lazy interface {
	Prepare(path string, img ImageSpec) error
	Release(path string) error
	Made(path string) bool
	Stop()
}

type Clone struct {
	Src, Dst string
	Overlay  bool
	Dir      bool
}

const EngineRoots = "/var/lib/docker/rootfs/overlayfs"

type MachineSpec struct {
	UUID     string
	Name     string
	Root     string
	UID, GID uint32
}

type VolumeSpec struct{ Name, Data string }

type ImageSpec struct {
	ID, Name string
	DiffIDs  []string
}

func (i ImageSpec) Key() string { return i.ID + "\x00" + i.Name }

type shown struct {
	id, name string
	path     string
	binds    []string
}

type ObjectsOptions struct {
	Images      Lazy
	Removed     func([]string)
	Logf        func(string, ...any)
	EngineRoots string
	Isolated    []string
}

func NewObjects(tree *Tree, ops Ops, o ObjectsOptions) *Objects {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Removed == nil {
		o.Removed = func([]string) {}
	}
	if o.EngineRoots == "" {
		o.EngineRoots = EngineRoots
	}
	ob := &Objects{
		tree: tree, ops: ops, images: o.Images, removed: o.Removed, logf: o.Logf, engineRoots: o.EngineRoots,
		isolated: map[string]bool{}, machines: map[string]MachineSpec{}, wantM: map[string]MachineSpec{},
		volumes: map[string]VolumeSpec{}, images_: map[string]ImageSpec{}, conts: map[string]*shown{},
		released: map[string]bool{}, ended: make(chan struct{}),
	}
	for _, n := range o.Isolated {
		ob.isolated[n] = true
	}
	return ob
}

var ErrNotMounted = errors.New("nfsview: not a mount point")

var ErrNotOverlay = errors.New("nfsview: not an overlay")

var ErrClosed = errors.New("nfsview: the view is stopping")

func (o *Objects) rel(p string) string {
	return strings.TrimPrefix(strings.TrimPrefix(p, o.tree.Root()), "/")
}

func (o *Objects) show(obj Object, mount func(string) error) (gone []string, flush bool, err error) {
	_, had := o.tree.Lookup(obj.Kind, obj.Key)
	ch := o.tree.Set(obj)
	if ch.Refused != "" {
		if ch.OldPath != "" {
			o.releasePlace(obj.Kind, ch.OldPath)
			gone = append(gone, ch.OldPath)
		}
		return gone, ch.Flush || ch.OldPath != "", fmt.Errorf("nfsview: %v %q not shown: %s", obj.Kind, obj.Name, ch.Refused)
	}
	switch {
	case had && ch.OldPath != "" && ch.OldPath != ch.Path:
		if err := o.ops.MakeDir(ch.Path); err != nil {
			return gone, true, err
		}
		if err := o.ops.Move(ch.OldPath, ch.Path); err != nil {
			o.tree.Remove(obj.Kind, obj.Key)
			o.releasePlace(obj.Kind, ch.OldPath)
			o.ops.RemoveDir(ch.Path)
			return append(gone, ch.OldPath), true, err
		}
		o.ops.RemoveDir(ch.OldPath)
		gone = append(gone, ch.OldPath)
		return gone, true, nil
	case had:
		return nil, ch.Flush, nil
	}
	if err := o.ops.MakeDir(ch.Path); err != nil {
		o.tree.Remove(obj.Kind, obj.Key)
		return nil, ch.Flush, err
	}
	if err := mount(ch.Path); err != nil {
		o.tree.Remove(obj.Kind, obj.Key)
		o.ops.RemoveDir(ch.Path)
		return nil, true, err
	}
	return nil, ch.Flush, nil
}

func (o *Objects) releasePlace(kind Kind, p string) {
	var err error
	switch kind {
	case KindImage:
		if o.images != nil {
			err = o.images.Release(p)
		}
	case KindContainer:
		err = o.ops.Release(p, true)
	default:
		err = o.ops.Release(p, false)
	}
	if err != nil {
		o.logf("release %s: %v", o.rel(p), err)
	}
	if err := o.ops.RemoveDir(p); err != nil {
		o.logf("remove %s: %v", o.rel(p), err)
	}
}

func (o *Objects) hide(kind Kind, key string) []string {
	p, ok := o.tree.Lookup(kind, key)
	if !ok {
		return nil
	}
	var binds []Placed
	if kind == KindContainer {
		for _, b := range o.tree.Objects() {
			if b.Kind == KindBind && strings.HasPrefix(b.Key, key+":") {
				binds = append(binds, b)
			}
		}
	}
	o.tree.Remove(kind, key)
	sort.Slice(binds, func(i, j int) bool { return binds[i].Path > binds[j].Path })
	var gone []string
	for _, b := range binds {
		if err := o.ops.Release(b.Path, false); err != nil {
			o.logf("release %s: %v", o.rel(b.Path), err)
		}
		gone = append(gone, b.Path)
	}
	o.releasePlace(kind, p.Path)
	return append(gone, p.Path)
}

func (o *Objects) done(flush bool, gone []string) {
	if flush || len(gone) > 0 {
		if err := o.ops.Flush(); err != nil {
			o.logf("flush: %v", err)
		} else {
			o.tree.Table().Flushed()
		}
	}
	if len(gone) > 0 {
		rel := make([]string, len(gone))
		for i, g := range gone {
			rel[i] = o.rel(g)
		}
		o.removed(rel)
	}
}

func (o *Objects) SetIsolated(name string, isolated bool) {
	o.mu.Lock()
	if o.isolated[name] == isolated {
		o.mu.Unlock()
		return
	}
	if isolated {
		o.isolated[name] = true
	} else {
		delete(o.isolated, name)
	}
	want := make([]MachineSpec, 0, len(o.wantM))
	for _, m := range o.wantM {
		want = append(want, m)
	}
	o.mu.Unlock()
	o.SyncMachines(want)
}

func (o *Objects) Isolated(name string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.isolated[name]
}

func (o *Objects) SyncMachines(specs []MachineSpec) {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	for _, m := range specs {
		if prev, ok := o.wantM[m.UUID]; ok && prev.Name != m.Name && o.isolated[prev.Name] {
			delete(o.isolated, prev.Name)
			o.isolated[m.Name] = true
		}
	}
	o.wantM = map[string]MachineSpec{}
	want := map[string]MachineSpec{}
	for _, m := range specs {
		o.wantM[m.UUID] = m
		if !o.isolated[m.Name] {
			want[m.UUID] = m
		}
	}
	var gone []string
	flush := false
	for id := range o.machines {
		if _, ok := want[id]; !ok {
			gone = append(gone, o.hide(KindMachine, id)...)
			delete(o.machines, id)
		}
	}
	for _, id := range sortedKeys(want) {
		m := want[id]
		g, f, err := o.show(Object{Kind: KindMachine, Key: id, Name: m.Name, UID: m.UID, GID: m.GID},
			func(p string) error { return o.ops.Bind(m.Root, p) })
		gone, flush = append(gone, g...), flush || f
		if err != nil {
			o.logf("machine %s: %v", m.Name, err)
			delete(o.machines, id)
			continue
		}
		o.machines[id] = m
	}
	o.mu.Unlock()
	o.done(flush, gone)
}

func (o *Objects) SyncVolumes(specs []VolumeSpec) {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	want := map[string]VolumeSpec{}
	for _, v := range specs {
		want[v.Name] = v
	}
	var gone []string
	flush := false
	for name, v := range o.volumes {
		if w, ok := want[name]; !ok || w.Data != v.Data {
			gone = append(gone, o.hide(KindVolume, name)...)
			delete(o.volumes, name)
		}
	}
	for _, name := range sortedKeys(want) {
		v := want[name]
		if _, ok := o.volumes[name]; ok {
			continue
		}
		g, f, err := o.show(Object{Kind: KindVolume, Key: name, Name: name},
			func(p string) error { return o.ops.Bind(v.Data, p) })
		gone, flush = append(gone, g...), flush || f
		if err != nil {
			o.logf("volume %s: %v", name, err)
			continue
		}
		o.volumes[name] = v
	}
	o.mu.Unlock()
	o.done(flush, gone)
}

func (o *Objects) SyncImages(specs []ImageSpec) {
	if o.images == nil {
		return
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	want := map[string]ImageSpec{}
	for _, i := range specs {
		want[i.Key()] = i
	}
	var gone []string
	flush := false
	for k := range o.images_ {
		if _, ok := want[k]; !ok {
			gone = append(gone, o.hide(KindImage, k)...)
			delete(o.images_, k)
		}
	}
	for _, k := range sortedKeys(want) {
		img := want[k]
		if _, ok := o.images_[k]; ok {
			continue
		}
		g, f, err := o.show(Object{Kind: KindImage, Key: k, Name: img.Name, ReadOnly: true},
			func(p string) error { return o.images.Prepare(p, img) })
		gone, flush = append(gone, g...), flush || f
		if err != nil {
			o.logf("image %s: %v", img.Name, err)
			continue
		}
		o.images_[k] = img
	}
	o.mu.Unlock()
	o.done(flush, gone)
}

func (o *Objects) Attach(nsPath, id, name string) error {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return ErrClosed
	}
	delete(o.released, id)
	_, err := o.attachLocked(nsPath, id, name)
	return err
}

func (o *Objects) AttachFound(nsPath, id, name string) (bool, error) {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return false, ErrClosed
	}
	if o.released[id] {
		o.mu.Unlock()
		return false, nil
	}
	if _, ok := o.conts[id]; ok {
		o.mu.Unlock()
		return true, nil
	}
	return o.attachLocked(nsPath, id, name)
}

func (o *Objects) attachLocked(nsPath, id, name string) (bool, error) {
	var gone []string
	if _, ok := o.conts[id]; ok {
		gone = o.hide(KindContainer, id)
		delete(o.conts, id)
	}
	src := path.Join(o.engineRoots, id)
	var cerr error
	g, flush, err := o.show(Object{Kind: KindContainer, Key: id, Name: name}, func(p string) error {
		errs, err := o.ops.CloneFrom(nsPath, 0, []Clone{{Src: src, Dst: p, Overlay: true}})
		if err != nil {
			return err
		}
		cerr = errs[0]
		return errs[0]
	})
	gone = append(gone, g...)
	if err == nil {
		p, _ := o.tree.Lookup(KindContainer, id)
		o.conts[id] = &shown{id: id, name: name, path: p.Path}
	}
	o.mu.Unlock()
	o.done(flush, gone)
	if errors.Is(cerr, ErrNotMounted) {
		return false, nil
	}
	return err == nil, err
}

func (o *Objects) Rename(id, name string) {
	o.mu.Lock()
	c, ok := o.conts[id]
	if o.closed || !ok || c.name == name {
		o.mu.Unlock()
		return
	}
	g, flush, err := o.show(Object{Kind: KindContainer, Key: id, Name: name}, nil)
	if err == nil {
		c.name = name
		if p, ok := o.tree.Lookup(KindContainer, id); ok {
			c.path = p.Path
		}
	} else {
		delete(o.conts, id)
		o.logf("container %s: %v", name, err)
	}
	o.mu.Unlock()
	o.done(flush, g)
}

func (o *Objects) Binds(nsPath, id string, pid int, dests []string) error {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return ErrClosed
	}
	c, ok := o.conts[id]
	if !ok || len(dests) == 0 {
		o.mu.Unlock()
		return nil
	}
	var clones []Clone
	var at []string
	for _, d := range dests {
		if containsString(c.binds, d) {
			continue
		}
		ch := o.tree.SetBind(id, d, 0, 0)
		if ch.Refused != "" {
			continue
		}
		clones = append(clones, Clone{Src: d, Dst: ch.Path, Dir: true})
		at = append(at, d)
	}
	var errsAll []error
	flush := false
	if len(clones) > 0 {
		errs, err := o.ops.CloneFrom(nsPath, pid, clones)
		if err != nil {
			errs = make([]error, len(clones))
			for i := range errs {
				errs[i] = err
			}
		}
		for i, e := range errs {
			if e != nil {
				o.tree.Remove(KindBind, BindKey(id, at[i]))
				errsAll = append(errsAll, fmt.Errorf("%s: %w", at[i], e))
				continue
			}
			c.binds = append(c.binds, at[i])
			flush = true
		}
	}
	o.mu.Unlock()
	o.done(flush, nil)
	return errors.Join(errsAll...)
}

func (o *Objects) Release(id string) {
	o.mu.Lock()
	o.released[id] = true
	if _, ok := o.conts[id]; !ok {
		o.mu.Unlock()
		return
	}
	gone := o.hide(KindContainer, id)
	delete(o.conts, id)
	o.mu.Unlock()
	o.done(true, gone)
}

func (o *Objects) ReleaseContainers() {
	o.mu.Lock()
	o.released = map[string]bool{}
	var gone []string
	for _, id := range sortedKeys(o.conts) {
		gone = append(gone, o.hide(KindContainer, id)...)
		delete(o.conts, id)
	}
	o.mu.Unlock()
	o.done(len(gone) > 0, gone)
}

func (o *Objects) Forget(id string) {
	o.mu.Lock()
	delete(o.released, id)
	o.mu.Unlock()
}

func (o *Objects) Containers() map[string]string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := map[string]string{}
	for id, c := range o.conts {
		out[id] = c.name
	}
	return out
}

func (o *Objects) List() []api.ViewObject {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []api.ViewObject
	for _, p := range o.tree.Objects() {
		v := api.ViewObject{Path: o.rel(p.Path), FSID: p.FSID, UID: p.UID, GID: p.GID, ReadOnly: p.ReadOnly, Name: p.Name, Mounted: true}
		switch p.Kind {
		case KindMachine:
			v.Kind = api.ViewMachine
		case KindVolume:
			v.Kind = api.ViewVolume
		case KindContainer:
			v.Kind = api.ViewContainer
		case KindImage:
			v.Kind = api.ViewImage
			v.Mounted = o.images != nil && o.images.Made(p.Path)
		case KindBind:
			v.Kind = api.ViewBind
		}
		out = append(out, v)
	}
	return out
}

func (o *Objects) Close() {
	o.mu.Lock()
	if !o.closed {
		o.closed = true
		close(o.ended)
	}
	o.mu.Unlock()
	if o.images != nil {
		o.images.Stop()
	}
}

func (o *Objects) Ended() <-chan struct{} { return o.ended }

func (o *Objects) AfterStop() {
	if c, ok := o.images.(interface{ Close() }); ok {
		c.Close()
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
