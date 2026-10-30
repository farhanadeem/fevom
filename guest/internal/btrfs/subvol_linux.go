package btrfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	subvolReadOnly       = 1 << 1
	subvolQgroupInherit  = 1 << 2
	subvolSpecByID       = 1 << 4
	subvolFlagReadOnlyRO = 1 << 1
)

type qgroupInheritHead struct {
	flags         uint64
	numQgroups    uint64
	numRefCopies  uint64
	numExclCopies uint64
	lim           [5]uint64
}

type searchKey struct {
	treeID                    uint64
	minObjectID, maxObjectID  uint64
	minOffset, maxOffset      uint64
	minTransID, maxTransID    uint64
	minType, maxType          uint32
	nrItems                   uint32
	unused                    uint32
	unused1, unused2, unused3 uint64
	unused4                   uint64
}

type searchArgs struct {
	key searchKey
	buf [4096 - unsafe.Sizeof(searchKey{})]byte
}

type searchHeader struct {
	transid  uint64
	objectid uint64
	offset   uint64
	typ      uint32
	len      uint32
}

var (
	iocTreeSearch     = ioc(iocRead|iocWrite, 17, unsafe.Sizeof(searchArgs{}))
	iocSnapCreateV2   = ioc(iocWrite, 23, unsafe.Sizeof(volArgsV2{}))
	iocSubvolGetFlags = ioc(iocRead, 25, 8)
	iocSubvolSetFlags = ioc(iocWrite, 26, 8)
	iocSnapDestroyV2  = ioc(iocWrite, 63, unsafe.Sizeof(volArgsV2{}))
)

const (
	rootTreeObjectID  = 1
	quotaTreeObjectID = 8
	rootRefKey        = 156
	qgroupRelationKey = 246
)

func inheritList(groups []uint64) []byte {
	head := int(unsafe.Sizeof(qgroupInheritHead{}))
	b := make([]byte, head+8*len(groups))
	binary.LittleEndian.PutUint64(b[8:], uint64(len(groups)))
	for i, g := range groups {
		binary.LittleEndian.PutUint64(b[head+8*i:], g)
	}
	return b
}

func subvolCall(dir *os.File, req uintptr, name string, flags uint64, fd int64, groups []uint64) error {
	if name == "" || len(name) > 255 {
		return fmt.Errorf("subvolume name %q: must be 1 to 255 bytes", name)
	}
	var args volArgsV2
	args.fd = fd
	args.flags = flags
	copy(args.name[:], name)
	var inherit []byte
	if len(groups) > 0 {
		inherit = inheritList(groups)
		args.flags |= subvolQgroupInherit
		args.unused[0] = uint64(len(inherit))
		args.unused[1] = uint64(uintptr(unsafe.Pointer(&inherit[0])))
	}
	err := ioctl(dir, req, unsafe.Pointer(&args))
	_ = inherit
	return err
}

func CreateSubvolumeIn(dir *os.File, name string, groups []uint64) error {
	return subvolCall(dir, iocSubvolCreateV2, name, 0, 0, groups)
}

func Snapshot(src, dir *os.File, name string, readOnly bool, groups []uint64) error {
	var flags uint64
	if readOnly {
		flags = subvolReadOnly
	}
	return subvolCall(dir, iocSnapCreateV2, name, flags, int64(src.Fd()), groups)
}

func DeleteSubvolume(dir *os.File, name string) error {
	return subvolCall(dir, iocSnapDestroyV2, name, 0, 0, nil)
}

func DeleteSubvolumeByID(dir *os.File, id uint64) error {
	var args volArgsV2
	args.flags = subvolSpecByID
	binary.LittleEndian.PutUint64(args.name[:8], id)
	return ioctl(dir, iocSnapDestroyV2, unsafe.Pointer(&args))
}

func ReadOnly(f *os.File) (bool, error) {
	var flags uint64
	if err := ioctl(f, iocSubvolGetFlags, unsafe.Pointer(&flags)); err != nil {
		return false, err
	}
	return flags&subvolFlagReadOnlyRO != 0, nil
}

func SetReadOnly(f *os.File, readOnly bool) error {
	var flags uint64
	if err := ioctl(f, iocSubvolGetFlags, unsafe.Pointer(&flags)); err != nil {
		return err
	}
	if readOnly {
		flags |= subvolFlagReadOnlyRO
	} else {
		flags &^= subvolFlagReadOnlyRO
	}
	return ioctl(f, iocSubvolSetFlags, unsafe.Pointer(&flags))
}

func DestroyQgroup(f *os.File, id uint64) error {
	args := qgroupCreateArgs{create: 0, qgroupid: id}
	return ioctl(f, iocQgroupCreate, unsafe.Pointer(&args))
}

type treeItem struct {
	objectid, offset uint64
	typ              uint32
	data             []byte
}

func search(f *os.File, tree uint64, typ uint32, minObj, maxObj uint64) ([]treeItem, error) {
	var items []treeItem
	key := searchKey{
		treeID: tree, minObjectID: minObj, maxObjectID: maxObj,
		maxOffset: ^uint64(0), maxTransID: ^uint64(0),
		minType: typ, maxType: typ,
	}
	for {
		args := searchArgs{key: key}
		args.key.nrItems = 4096
		if err := ioctl(f, iocTreeSearch, unsafe.Pointer(&args)); err != nil {
			return nil, err
		}
		n := args.key.nrItems
		if n == 0 {
			return items, nil
		}
		off := 0
		var last searchHeader
		for i := uint32(0); i < n; i++ {
			if off+int(unsafe.Sizeof(searchHeader{})) > len(args.buf) {
				return nil, errors.New("tree search: a result runs past the buffer")
			}
			h := *(*searchHeader)(unsafe.Pointer(&args.buf[off]))
			off += int(unsafe.Sizeof(searchHeader{}))
			if off+int(h.len) > len(args.buf) {
				return nil, errors.New("tree search: an item runs past the buffer")
			}
			if h.typ == typ {
				items = append(items, treeItem{
					objectid: h.objectid, offset: h.offset, typ: h.typ,
					data: append([]byte(nil), args.buf[off:off+int(h.len)]...),
				})
			}
			off += int(h.len)
			last = h
		}
		switch {
		case last.offset < ^uint64(0):
			key.minObjectID, key.minOffset = last.objectid, last.offset+1
		case last.objectid < maxObj:
			key.minObjectID, key.minOffset = last.objectid+1, 0
		default:
			return items, nil
		}
		if key.minObjectID > maxObj {
			return items, nil
		}
	}
}

type SubvolumeRef struct {
	ID, Parent, DirID uint64
	Name              string
}

func Subvolumes(f *os.File) ([]SubvolumeRef, error) {
	items, err := search(f, rootTreeObjectID, rootRefKey, 5, ^uint64(0)-256)
	if err != nil {
		return nil, err
	}
	var refs []SubvolumeRef
	for _, it := range items {
		if len(it.data) < 18 {
			continue
		}
		nameLen := int(binary.LittleEndian.Uint16(it.data[16:]))
		if 18+nameLen > len(it.data) {
			continue
		}
		refs = append(refs, SubvolumeRef{
			ID: it.offset, Parent: it.objectid,
			DirID: binary.LittleEndian.Uint64(it.data[0:]),
			Name:  string(it.data[18 : 18+nameLen]),
		})
	}
	return refs, nil
}

type QgroupRelation struct{ Child, Parent uint64 }

func QgroupRelations(f *os.File) ([]QgroupRelation, error) {
	items, err := search(f, quotaTreeObjectID, qgroupRelationKey, 0, ^uint64(0))
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rels []QgroupRelation
	for _, it := range items {
		if it.objectid>>48 < it.offset>>48 {
			rels = append(rels, QgroupRelation{Child: it.objectid, Parent: it.offset})
		}
	}
	return rels, nil
}

func QgroupLevel(id uint64) uint64  { return id >> 48 }
func QgroupNumber(id uint64) uint64 { return id & (1<<48 - 1) }
