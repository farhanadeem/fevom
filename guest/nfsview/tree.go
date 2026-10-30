package nfsview

import (
	"fmt"
	"hash/crc32"
	"path"
	"sort"
	"strings"
	"sync"
)

type Kind int

const (
	KindContainer Kind = iota + 1
	KindImage
	KindVolume
	KindMachine
	KindBind
)

func (k Kind) String() string {
	switch k {
	case KindContainer:
		return "container"
	case KindImage:
		return "image"
	case KindVolume:
		return "volume"
	case KindMachine:
		return "machine"
	case KindBind:
		return "bind"
	}
	return fmt.Sprintf("kind %d", int(k))
}

const (
	DirDocker     = "docker"
	DirContainers = "docker/containers"
	DirImages     = "docker/images"
	DirVolumes    = "docker/volumes"
)

const README = `fev

This folder shows the files of fev's Linux machines and Docker objects:
each machine at ~/Fevom/<machine>, Docker's containers, images and volumes
under ~/Fevom/docker. Nothing here takes space on your Mac: the files live
in fev's Linux VM, and this folder is empty while fev is stopped.

Running containers and their volumes, machines and volumes are writable;
images are read-only. To hide this folder from Finder's sidebar and the
desktop, keeping the path, set files.view_hidden (fev config set
files.view_hidden true).

[fev is RUNNING. Files are available.]
`

type File struct {
	Name string
	Dir  bool
	Mode uint32
	Data string
}

func RootFiles() []File {
	return []File{
		{Name: "README.txt", Mode: 0o444, Data: README},
		{Name: ".metadata_never_index", Mode: 0o444},
		{Name: ".metadata_never_index_unless_rootfs", Mode: 0o444},
		{Name: ".metadata-never-index", Mode: 0o444},
		{Name: ".fseventsd", Dir: true, Mode: 0o555},
		{Name: DirDocker, Dir: true, Mode: 0o755},
		{Name: DirContainers, Dir: true, Mode: 0o755},
		{Name: DirImages, Dir: true, Mode: 0o755},
		{Name: DirVolumes, Dir: true, Mode: 0o755},
	}
}

type Object struct {
	Kind     Kind
	Key      string
	Name     string
	UID, GID uint32
	ReadOnly bool
}

type Placed struct {
	Object
	Path string
	FSID uint32
}

type Change struct {
	Path    string
	OldPath string
	FSID    uint32
	Flush   bool
	Refused string
}

type Owner struct{ UID, GID uint32 }

type Tree struct {
	root  string
	owner Owner
	table *Table

	mu      sync.Mutex
	objects map[objKey]Placed
	byPath  map[string]objKey
}

type objKey struct {
	kind Kind
	key  string
}

const (
	flagsCommon = FlagInsecurePort | FlagNoSubtreeChk | FlagFSID
	FlagsRoot   = flagsCommon | FlagReadOnly | FlagRootSquash | FlagCrossMount
	FlagsObject = flagsCommon | FlagAllSquash | FlagAsync | FlagCrossMount
)

func NewTree(root string, owner Owner) *Tree {
	t := &Tree{
		root:    path.Clean(root),
		owner:   owner,
		table:   NewTable(),
		objects: map[objKey]Placed{},
		byPath:  map[string]objKey{},
	}
	t.table.Set(t.root, Export{FSID: 0, Flags: FlagsRoot, AnonUID: owner.UID, AnonGID: owner.GID})
	return t
}

func (t *Tree) Root() string { return t.root }

func (t *Tree) Owner() Owner { return t.owner }

func (t *Tree) Table() *Table { return t.table }

func FSIDOf(kind Kind, key string, n int) uint32 {
	k := key
	if n > 0 {
		k = fmt.Sprintf("%s#%d", key, n)
	}
	return uint32(kind)<<28 | crc32.ChecksumIEEE([]byte(k))&0x0fffffff
}

func BindKey(containerID, dest string) string { return containerID + ":" + dest }

var reserved = func() map[string]bool {
	m := map[string]bool{}
	for _, f := range RootFiles() {
		if !strings.Contains(f.Name, "/") {
			m[strings.ToLower(f.Name)] = true
		}
	}
	return m
}()

const maxName = 255

func Sanitize(name string) (string, error) {
	n := strings.ReplaceAll(strings.ReplaceAll(name, "\x00", ""), "/", "_")
	if n == "" || n == "." || n == ".." {
		return "", fmt.Errorf("%q cannot be a directory's name", name)
	}
	if len(n) > maxName {
		h := fmt.Sprintf("~%08x", crc32.ChecksumIEEE([]byte(name)))
		cut := maxName - len(h)
		for cut > 0 && n[cut]&0xc0 == 0x80 {
			cut--
		}
		n = n[:cut] + h
	}
	return n, nil
}

func (t *Tree) dirOf(k Kind) string {
	switch k {
	case KindContainer:
		return path.Join(t.root, DirContainers)
	case KindImage:
		return path.Join(t.root, DirImages)
	case KindVolume:
		return path.Join(t.root, DirVolumes)
	case KindMachine:
		return t.root
	}
	return ""
}

func (t *Tree) Set(o Object) Change {
	if o.Kind < KindContainer || o.Kind > KindMachine {
		return Change{Refused: fmt.Sprintf("%v is not shown by Set", o.Kind)}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	k := objKey{o.Kind, o.Key}
	old, had := t.objects[k]
	name, err := Sanitize(o.Name)
	if err == nil && o.Kind == KindMachine && reserved[strings.ToLower(name)] {
		err = fmt.Errorf("a machine named %q has the name of one of the view's own files", o.Name)
	}
	if err != nil {
		ch := Change{Refused: err.Error()}
		if had {
			ch.OldPath = old.Path
			ch.Flush = t.drop(k, old)
		}
		return ch
	}
	p := path.Join(t.dirOf(o.Kind), name)
	if had && old.Path == p {
		p = old.Path
	} else if other, taken := t.byPath[p]; taken && other != k {
		p = path.Join(t.dirOf(o.Kind), fmt.Sprintf("%s~%08x", name, crc32.ChecksumIEEE([]byte(o.Key))))
	}
	return t.place(k, o, p, old, had)
}

func (t *Tree) SetBind(containerID, dest string, uid, gid uint32) Change {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.objects[objKey{KindContainer, containerID}]
	if !ok {
		return Change{Refused: "no container " + containerID}
	}
	d := path.Clean("/" + dest)
	if d == "/" {
		return Change{Refused: "a bind at the container's root"}
	}
	key := BindKey(containerID, d)
	k := objKey{KindBind, key}
	old, had := t.objects[k]
	o := Object{Kind: KindBind, Key: key, Name: d, UID: uid, GID: gid}
	return t.place(k, o, c.Path+d, old, had)
}

func (t *Tree) place(k objKey, o Object, p string, old Placed, had bool) Change {
	var flush bool
	if had && old.Path != p {
		flush = t.table.Remove(old.Path)
		delete(t.byPath, old.Path)
	}
	flags := FlagsObject
	if o.ReadOnly {
		flags = flagsCommon | FlagReadOnly | FlagAllSquash | FlagCrossMount
	}
	fsid := old.FSID
	var f, ok bool
	for n := 0; ; n++ {
		if !had || n > 0 {
			fsid = FSIDOf(o.Kind, o.Key, n)
		}
		f, ok = t.table.Set(p, Export{FSID: fsid, Flags: flags, AnonUID: o.UID, AnonGID: o.GID})
		if ok {
			break
		}
	}
	t.objects[k] = Placed{Object: o, Path: p, FSID: fsid}
	t.byPath[p] = k
	ch := Change{Path: p, FSID: fsid, Flush: flush || f}
	if had && old.Path != p {
		ch.OldPath = old.Path
	}
	return ch
}

func (t *Tree) Remove(kind Kind, key string) Change {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := objKey{kind, key}
	old, had := t.objects[k]
	if !had {
		return Change{}
	}
	ch := Change{OldPath: old.Path, FSID: old.FSID}
	ch.Flush = t.drop(k, old)
	if kind == KindContainer {
		for bk, b := range t.objects {
			if bk.kind == KindBind && strings.HasPrefix(bk.key, key+":") {
				ch.Flush = t.drop(bk, b) || ch.Flush
			}
		}
	}
	return ch
}

func (t *Tree) drop(k objKey, p Placed) bool {
	delete(t.objects, k)
	delete(t.byPath, p.Path)
	return t.table.Remove(p.Path)
}

func (t *Tree) Lookup(kind Kind, key string) (Placed, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.objects[objKey{kind, key}]
	return p, ok
}

func (t *Tree) Objects() []Placed {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Placed, 0, len(t.objects))
	for _, p := range t.objects {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
