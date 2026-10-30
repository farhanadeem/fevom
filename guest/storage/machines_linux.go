package storage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/internal/btrfs"
)

const MachinesSubvolume = "@machines"

const (
	ImagesDir  = ".images"
	scratchDir = ".scratch"
)

type Freezer func() (thaw func(), err error)

type Machines struct {
	v   *Volume
	dir string
	mu  sync.Mutex
}

func (v *Volume) Machines() (*Machines, error) {
	switch {
	case v.out.ReadOnly:
		return nil, errors.New(api.DataReadOnly)
	case v.ctl == nil:
		return nil, fmt.Errorf("the data filesystem is at layout %d and has no quota groups", v.layout)
	}
	return &Machines{v: v, dir: filepath.Join(v.cfg.Target, MachinesSubvolume)}, nil
}

func (m *Machines) Path(name string) string { return filepath.Join(m.dir, name) }

func (m *Machines) ImagePath(ref api.ImageRef) string {
	return filepath.Join(m.dir, ImagesDir, ref.Distro, ref.Release, ref.Arch, ref.Variant, ref.Serial)
}

func checkRef(ref api.ImageRef) error {
	for _, p := range []string{ref.Distro, ref.Release, ref.Arch, ref.Variant, ref.Serial} {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, "/\x00") || strings.HasPrefix(p, ".") || len(p) > 128 {
			return fmt.Errorf("image %s/%s/%s/%s/%s: each part must be a plain name", ref.Distro, ref.Release, ref.Arch, ref.Variant, ref.Serial)
		}
	}
	return nil
}

func checkName(name string) error {
	if err := api.CheckMachineName(name); err != nil {
		return fmt.Errorf("%w (%v)", errInvalid, err)
	}
	return nil
}

var errInvalid = errors.New("invalid name")

var (
	ErrMachineExists = errors.New("a machine of that name exists")
	ErrNoMachine     = errors.New("no such machine")
	ErrNoImage       = errors.New("the guest does not have that image build")
)

func machineGroup(id uint64) uint64 { return btrfs.QgroupID(1, id) }

func subvolumeID(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	id, err := btrfs.SubvolumeID(f)
	if err != nil {
		return 0, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return 0, err
	}
	if st.Ino != 256 {
		return 0, fmt.Errorf("%s is not a subvolume", path)
	}
	return id, nil
}

func randomName(prefix string) string {
	var b [8]byte
	rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

func openDir(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|unix.O_DIRECTORY, 0)
}

func (m *Machines) scratch() (string, error) {
	d := filepath.Join(m.dir, scratchDir)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

func (m *Machines) HasImage(ref api.ImageRef) bool {
	if checkRef(ref) != nil {
		return false
	}
	if _, err := subvolumeID(m.ImagePath(ref)); err != nil {
		return false
	}
	return m.readOnly(m.ImagePath(ref)) == nil
}

func (m *Machines) readOnly(path string) error {
	f, err := openDir(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if ro, err := btrfs.ReadOnly(f); err == nil && ro {
		return nil
	}
	if err := btrfs.SetReadOnly(f, true); err != nil {
		return fmt.Errorf("make the image read-only: %w", err)
	}
	return nil
}

type ImageBuild struct {
	m    *Machines
	ref  api.ImageRef
	Path string
	done bool
}

func (m *Machines) BeginImage(ref api.ImageRef) (*ImageBuild, error) {
	if err := checkRef(ref); err != nil {
		return nil, err
	}
	d, err := m.scratch()
	if err != nil {
		return nil, err
	}
	dir, err := openDir(d)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	name := randomName("image-")
	if err := btrfs.CreateSubvolumeIn(dir, name, nil); err != nil {
		return nil, fmt.Errorf("make the image's subvolume: %w", err)
	}
	return &ImageBuild{m: m, ref: ref, Path: filepath.Join(d, name)}, nil
}

func (b *ImageBuild) Commit() error {
	if b.done {
		return errors.New("the image build was committed or abandoned already")
	}
	b.done = true
	m := b.m
	m.mu.Lock()
	defer m.mu.Unlock()
	final := m.ImagePath(b.ref)
	if _, err := subvolumeID(final); err == nil {
		m.deleteTree(b.Path)
		return m.readOnly(final)
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		m.deleteTree(b.Path)
		return err
	}
	if err := os.Rename(b.Path, final); err != nil {
		m.deleteTree(b.Path)
		return fmt.Errorf("put the image in the cache: %w", err)
	}
	if err := m.readOnly(final); err != nil {
		m.deleteTree(final)
		return err
	}
	entries, _ := os.ReadDir(filepath.Dir(final))
	for _, e := range entries {
		if e.Name() != b.ref.Serial && e.IsDir() && e.Name() < b.ref.Serial {
			old := filepath.Join(filepath.Dir(final), e.Name())
			if id, err := subvolumeID(old); err == nil {
				if err := m.retire(id); err != nil {
					m.v.cfg.Logf("storage: the older image build %s is not recorded as retired, so what machines still share of it is no part's in the disk's usage: %v", e.Name(), err)
				}
			}
			if err := m.deleteTree(old); err != nil {
				m.v.cfg.Logf("storage: the older image build %s could not be removed: %v", e.Name(), err)
			}
		}
	}
	return nil
}

const retiredFile = ".retired"

func (m *Machines) retiredPath() string { return filepath.Join(m.dir, ImagesDir, retiredFile) }

func retiredBuilds(path string) []uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var ids []uint64
	for _, f := range strings.Fields(string(b)) {
		if id, err := strconv.ParseUint(f, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func (m *Machines) retire(id uint64) error {
	var keep []string
	for _, x := range append(retiredBuilds(m.retiredPath()), id) {
		if x == id || m.hasGroup(btrfs.QgroupID(0, x)) {
			if s := strconv.FormatUint(x, 10); !slices.Contains(keep, s) {
				keep = append(keep, s)
			}
		}
	}
	tmp := m.retiredPath() + ".new"
	if err := os.WriteFile(tmp, []byte(strings.Join(keep, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	f, err := os.Open(tmp)
	if err == nil {
		err = f.Sync()
		f.Close()
	}
	if err == nil {
		err = os.Rename(tmp, m.retiredPath())
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

func (m *Machines) hasGroup(g uint64) bool {
	_, err := m.v.readSysfs(fmt.Sprintf("qgroups/%d_%d/referenced", btrfs.QgroupLevel(g), btrfs.QgroupNumber(g)))
	return err == nil
}

func (b *ImageBuild) Abort() {
	if b.done {
		return
	}
	b.done = true
	b.m.mu.Lock()
	defer b.m.mu.Unlock()
	b.m.deleteTree(b.Path)
}

func (m *Machines) Images() ([]api.ImageInfo, error) {
	var out []api.ImageInfo
	root := filepath.Join(m.dir, ImagesDir)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(root, path)
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) < 5 || !d.IsDir() {
			return nil
		}
		ref := api.ImageRef{Distro: parts[0], Release: parts[1], Arch: parts[2], Variant: parts[3], Serial: parts[4]}
		info := api.ImageInfo{ImageRef: ref}
		if id, err := subvolumeID(path); err == nil {
			info.DiskBytes = m.groupBytes(btrfs.QgroupID(0, id))
		}
		out = append(out, info)
		return fs.SkipDir
	})
	return out, err
}

func (m *Machines) Create(name string, ref api.ImageRef) error {
	return m.create(name, ref, nil)
}

func (m *Machines) CreateWith(name string, ref api.ImageRef, prepare func(root string) error) error {
	return m.create(name, ref, prepare)
}

func (m *Machines) create(name string, ref api.ImageRef, prepare func(string) error) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := checkRef(ref); err != nil {
		return err
	}
	src, err := openDir(m.ImagePath(ref))
	if err != nil {
		return fmt.Errorf("%w: %s/%s/%s/%s/%s", ErrNoImage, ref.Distro, ref.Release, ref.Arch, ref.Variant, ref.Serial)
	}
	defer src.Close()
	return m.place(name, prepare, func(dir *os.File, tmp string) error {
		return btrfs.Snapshot(src, dir, tmp, false, nil)
	})
}

func (m *Machines) place(name string, prepare func(string) error, make func(dir *os.File, tmp string) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := os.Lstat(m.Path(name)); err == nil {
		return fmt.Errorf("%w: %s", ErrMachineExists, name)
	}
	d, err := m.scratch()
	if err != nil {
		return err
	}
	dir, err := openDir(d)
	if err != nil {
		return err
	}
	defer dir.Close()
	tmp := randomName("machine-")
	path := filepath.Join(d, tmp)
	if err := make(dir, tmp); err != nil {
		return fmt.Errorf("make the machine's subvolume: %w", err)
	}
	fail := func(err error) error {
		m.deleteTree(path)
		return err
	}
	id, err := subvolumeID(path)
	if err != nil {
		return fail(err)
	}
	if err := m.joinGroup(id); err != nil {
		return fail(err)
	}
	if prepare != nil {
		if err := prepare(path); err != nil {
			return fail(err)
		}
	}
	if err := os.Rename(path, m.Path(name)); err != nil {
		return fail(fmt.Errorf("put the machine in place: %w", err))
	}
	said, err := m.checkMembership()
	for _, line := range said {
		m.v.cfg.Logf("storage: %s", line)
	}
	if err != nil {
		m.v.cfg.Logf("storage: the machines' quota groups were not checked: %v", err)
	}
	return nil
}

func (m *Machines) joinGroup(id uint64) error {
	g := machineGroup(id)
	if err := btrfs.CreateQgroup(m.v.ctl, g); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("make the machine's quota group 1/%d: %w", id, err)
	}
	if err := btrfs.AssignQgroup(m.v.ctl, btrfs.QgroupID(0, id), g); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("put subvolume %d into its quota group: %w", id, err)
	}
	return nil
}

func (m *Machines) Clone(src, dst string, freeze Freezer, prepare func(root string) error) error {
	if err := checkName(src); err != nil {
		return err
	}
	if err := checkName(dst); err != nil {
		return err
	}
	if _, err := subvolumeID(m.Path(src)); err != nil {
		return fmt.Errorf("%w: %s", ErrNoMachine, src)
	}
	s, err := openDir(m.Path(src))
	if err != nil {
		return err
	}
	defer s.Close()
	return m.place(dst, prepare, func(dir *os.File, tmp string) error {
		return frozen(freeze, func() error { return btrfs.Snapshot(s, dir, tmp, false, nil) })
	})
}

func frozen(freeze Freezer, fn func() error) error {
	if freeze == nil {
		return fn()
	}
	thaw, err := freeze()
	if err != nil {
		return fmt.Errorf("freeze the machine for its snapshot: %w", err)
	}
	defer thaw()
	return fn()
}

func (m *Machines) Rename(name, newName string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := checkName(newName); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := subvolumeID(m.Path(name)); err != nil {
		return fmt.Errorf("%w: %s", ErrNoMachine, name)
	}
	if _, err := os.Lstat(m.Path(newName)); err == nil {
		return fmt.Errorf("%w: %s", ErrMachineExists, newName)
	}
	return os.Rename(m.Path(name), m.Path(newName))
}

func (m *Machines) Delete(name string) error {
	_, err := m.remove(name, false)
	return err
}

func (m *Machines) DeleteHeld(name string) ([]Holder, error) {
	return m.remove(name, true)
}

func (m *Machines) remove(name string, held bool) ([]Holder, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	path := m.Path(name)
	id, err := subvolumeID(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNoMachine, name)
	}
	hs, err := m.release(name, path, held)
	if err != nil {
		return nil, err
	}
	return hs, m.removeSubvolume(path, id)
}

func (m *Machines) removeSubvolume(path string, id uint64) error {
	g := machineGroup(id)
	rels, err := btrfs.QgroupRelations(m.v.root)
	if err != nil {
		return fmt.Errorf("read the quota groups: %w", err)
	}
	for _, r := range rels {
		if r.Parent == g {
			if err := btrfs.RemoveQgroup(m.v.ctl, r.Child, g); err != nil && !errors.Is(err, unix.ENOENT) {
				return fmt.Errorf("take %d/%d out of the machine's group: %w", btrfs.QgroupLevel(r.Child), btrfs.QgroupNumber(r.Child), err)
			}
		}
	}
	if err := btrfs.DestroyQgroup(m.v.ctl, g); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove the machine's group 1/%d: %w", id, err)
	}
	if err := m.deleteTree(path); err != nil {
		return err
	}
	if err := m.v.Sync(); err != nil {
		return err
	}
	if err := btrfs.WaitDeletedSubvolumes(m.v.root); err != nil {
		return err
	}
	m.destroyEmptyGroups()
	return nil
}

func (m *Machines) deleteTree(path string) error {
	id, err := subvolumeID(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	refs, err := btrfs.Subvolumes(m.v.root)
	if err != nil {
		return fmt.Errorf("list the subvolumes: %w", err)
	}
	under := descendants(refs, id)
	depth := func(x uint64) int { return depthOf(refs, x) }
	sort.Slice(under, func(i, j int) bool { return depth(under[i]) > depth(under[j]) })
	for _, x := range under {
		if err := btrfs.DeleteSubvolumeByID(m.v.root, x); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("delete subvolume %d inside %s: %w", x, filepath.Base(path), err)
		}
	}
	dir, err := openDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := btrfs.DeleteSubvolume(dir, filepath.Base(path)); err != nil {
		return fmt.Errorf("delete subvolume %s: %w", filepath.Base(path), err)
	}
	return nil
}

func descendants(refs []btrfs.SubvolumeRef, id uint64) []uint64 {
	var out []uint64
	frontier := []uint64{id}
	for len(frontier) > 0 {
		var next []uint64
		for _, r := range refs {
			if slices.Contains(frontier, r.Parent) {
				out = append(out, r.ID)
				next = append(next, r.ID)
			}
		}
		frontier = next
	}
	return out
}

func depthOf(refs []btrfs.SubvolumeRef, id uint64) int {
	d := 0
	for {
		found := false
		for _, r := range refs {
			if r.ID == id {
				id, found = r.Parent, true
				d++
				break
			}
		}
		if !found || d > 1<<16 {
			return d
		}
	}
}

func (m *Machines) groupBytes(g uint64) int64 {
	n, err := m.v.readSysfsUint(fmt.Sprintf("qgroups/%d_%d/referenced", btrfs.QgroupLevel(g), btrfs.QgroupNumber(g)))
	if err != nil {
		return 0
	}
	return int64(n)
}

func (m *Machines) Usage(name string) (int64, error) {
	if err := checkName(name); err != nil {
		return 0, err
	}
	id, err := subvolumeID(m.Path(name))
	if err != nil {
		return 0, fmt.Errorf("%w: %s", ErrNoMachine, name)
	}
	return m.groupBytes(machineGroup(id)), nil
}

func (m *Machines) CheckMembership() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.checkMembership()
}

func (m *Machines) ClearScratch() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var said []string
	entries, err := os.ReadDir(filepath.Join(m.dir, scratchDir))
	if err != nil {
		return nil
	}
	for _, e := range entries {
		p := filepath.Join(m.dir, scratchDir, e.Name())
		if err := m.deleteTree(p); err != nil {
			said = append(said, fmt.Sprintf("left in the scratch directory and not removed: %s (%v)", e.Name(), err))
		} else {
			said = append(said, fmt.Sprintf("removed what an operation stopped by force left: %s", e.Name()))
		}
	}
	return said
}

func (m *Machines) checkMembership() ([]string, error) {
	var said []string
	say := func(format string, args ...any) {
		said = append(said, fmt.Sprintf(format, args...))
	}
	refs, err := btrfs.Subvolumes(m.v.root)
	if err != nil {
		return said, fmt.Errorf("list the subvolumes: %w", err)
	}
	rels, err := btrfs.QgroupRelations(m.v.root)
	if err != nil {
		return said, fmt.Errorf("read the quota groups: %w", err)
	}
	member := map[[2]uint64]bool{}
	for _, r := range rels {
		member[[2]uint64{r.Child, r.Parent}] = true
	}
	ctlID, _ := btrfs.SubvolumeID(m.v.ctl)
	machinesID, err := subvolumeID(m.dir)
	if err != nil {
		return said, err
	}
	machineOf := map[uint64]uint64{}
	machineName := map[uint64]string{}
	for _, r := range refs {
		if r.Parent == machinesID && r.DirID == 256 && api.CheckMachineName(r.Name) == nil {
			machineName[r.ID] = r.Name
			machineOf[r.ID] = r.ID
			for _, x := range descendants(refs, r.ID) {
				machineOf[x] = r.ID
			}
		}
	}
	assign := func(child, parent uint64) {
		if member[[2]uint64{child, parent}] {
			return
		}
		err := btrfs.AssignQgroup(m.v.ctl, child, parent)
		switch {
		case err == nil:
			say("subvolume %d was not in group %d/%d: put back", btrfs.QgroupNumber(child), btrfs.QgroupLevel(parent), btrfs.QgroupNumber(parent))
		case errors.Is(err, unix.EEXIST):
		default:
			say("subvolume %d could not be put back into group %d/%d: %v", btrfs.QgroupNumber(child), btrfs.QgroupLevel(parent), btrfs.QgroupNumber(parent), err)
		}
	}
	assign(btrfs.QgroupID(0, 5), quotaGroup)
	for _, r := range refs {
		if r.ID == ctlID {
			continue
		}
		assign(btrfs.QgroupID(0, r.ID), quotaGroup)
	}
	for id, name := range machineName {
		g := machineGroup(id)
		if err := btrfs.CreateQgroup(m.v.ctl, g); err == nil {
			say("machine %s had no quota group: made 1/%d", name, id)
		} else if !errors.Is(err, unix.EEXIST) {
			say("machine %s's quota group 1/%d could not be made: %v", name, id, err)
			continue
		}
	}
	for x, mid := range machineOf {
		assign(btrfs.QgroupID(0, x), machineGroup(mid))
	}
	for _, r := range rels {
		if btrfs.QgroupLevel(r.Child) != 0 || btrfs.QgroupLevel(r.Parent) != 1 || r.Parent == quotaGroup {
			continue
		}
		owner := btrfs.QgroupNumber(r.Parent)
		if _, ok := machineName[owner]; !ok {
			continue
		}
		if machineOf[btrfs.QgroupNumber(r.Child)] == owner {
			continue
		}
		if err := btrfs.RemoveQgroup(m.v.ctl, r.Child, r.Parent); err != nil && !errors.Is(err, unix.ENOENT) {
			say("subvolume %d is in machine %s's group and could not be taken out: %v", btrfs.QgroupNumber(r.Child), machineName[owner], err)
		} else {
			say("subvolume %d is not under machine %s and was in its group: taken out", btrfs.QgroupNumber(r.Child), machineName[owner])
		}
	}
	if n := m.destroyEmptyGroups(); n > 0 {
		say("removed %d groups of subvolumes that are gone and own nothing", n)
	}
	return said, nil
}

func (m *Machines) MachineGroups() (map[string]uint64, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, err
	}
	out := map[string]uint64{}
	for _, e := range entries {
		if api.CheckMachineName(e.Name()) != nil || !e.IsDir() {
			continue
		}
		if id, err := subvolumeID(m.Path(e.Name())); err == nil {
			out[e.Name()] = id
		}
	}
	return out, nil
}

func Mounted(target string, logf func(string, ...any)) (*Volume, error) {
	v := &Volume{cfg: Config{Target: target, Logf: logf, NowMS: func() float64 { return 0 }}, fstype: "btrfs"}
	if err := v.openRoot(); err != nil {
		return nil, err
	}
	if fsi, err := btrfs.GetFSInfo(v.root); err == nil {
		v.sysfs = "/sys/fs/btrfs/" + formatFSID(fsi.FSID)
	}
	s, err := ReadStamp(target)
	if err != nil {
		v.root.Close()
		return nil, err
	}
	v.layout = s.Layout
	if f, err := os.Open(filepath.Join(target, ControlSubvolume)); err == nil {
		v.ctl = f
	}
	return v, nil
}

func (m *Machines) DestroyEmptyGroups() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.destroyEmptyGroups()
}

func (m *Machines) destroyEmptyGroups() int {
	refs, err := btrfs.Subvolumes(m.v.root)
	if err != nil {
		return 0
	}
	linked := map[uint64]bool{5: true}
	for _, r := range refs {
		linked[r.ID] = true
	}
	entries, err := os.ReadDir(filepath.Join(m.v.sysfs, "qgroups"))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), "0_")
		if !ok {
			continue
		}
		var id uint64
		if _, err := fmt.Sscan(rest, &id); err != nil || linked[id] {
			continue
		}
		if m.groupBytes(btrfs.QgroupID(0, id)) != 0 {
			continue
		}
		if btrfs.DestroyQgroup(m.v.ctl, btrfs.QgroupID(0, id)) == nil {
			n++
		}
	}
	return n
}
