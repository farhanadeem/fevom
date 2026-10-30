package btrfs

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	magic = 0x94

	pathNameMax   = 4087
	subvolNameMax = 4039
)

type volArgs struct {
	fd   int64
	name [pathNameMax + 1]byte
}

type volArgsV2 struct {
	fd      int64
	transid uint64
	flags   uint64
	unused  [4]uint64
	name    [subvolNameMax + 1]byte
}

type fsInfoArgs struct {
	maxID          uint64
	numDevices     uint64
	fsid           [16]byte
	nodesize       uint32
	sectorsize     uint32
	cloneAlignment uint32
	csumType       uint16
	csumSize       uint16
	flags          uint64
	generation     uint64
	metadataUUID   [16]byte
	reserved       [944]byte
}

type devInfoArgs struct {
	devid      uint64
	uuid       [16]byte
	bytesUsed  uint64
	totalBytes uint64
	fsid       [16]byte
	unused     [377]uint64
	path       [1024]byte
}

const devStatValues = 5

type devStatsArgs struct {
	devid   uint64
	nrItems uint64
	flags   uint64
	values  [devStatValues]uint64
	unused  [128 - 2 - devStatValues]uint64
}

type subvolWait struct {
	subvolid uint64
	mode     uint32
	count    uint32
}

type inoLookupArgs struct {
	treeid   uint64
	objectid uint64
	name     [4080]byte
}

type quotaCtlArgs struct {
	cmd    uint64
	status uint64
}

type qgroupAssignArgs struct {
	assign uint64
	src    uint64
	dst    uint64
}

type qgroupCreateArgs struct {
	create   uint64
	qgroupid uint64
}

type qgroupLimitArgs struct {
	qgroupid uint64
	flags    uint64
	maxRfer  uint64
	maxExcl  uint64
	rsvRfer  uint64
	rsvExcl  uint64
}

type fstrimRange struct {
	start, length, minlen uint64
}

const (
	iocWrite = 1
	iocRead  = 2
)

func ioc(dir, nr, size uintptr) uintptr {
	return dir<<30 | size<<16 | magic<<8 | nr
}

var (
	iocResize         = ioc(iocWrite, 3, unsafe.Sizeof(volArgs{}))
	iocSync           = ioc(0, 8, 0)
	iocInoLookup      = ioc(iocRead|iocWrite, 18, unsafe.Sizeof(inoLookupArgs{}))
	iocSubvolCreateV2 = ioc(iocWrite, 24, unsafe.Sizeof(volArgsV2{}))
	iocDevInfo        = ioc(iocRead|iocWrite, 30, unsafe.Sizeof(devInfoArgs{}))
	iocFSInfo         = ioc(iocRead, 31, unsafe.Sizeof(fsInfoArgs{}))
	iocQuotaCtl       = ioc(iocRead|iocWrite, 40, unsafe.Sizeof(quotaCtlArgs{}))
	iocQgroupAssign   = ioc(iocWrite, 41, unsafe.Sizeof(qgroupAssignArgs{}))
	iocQgroupCreate   = ioc(iocWrite, 42, unsafe.Sizeof(qgroupCreateArgs{}))
	iocQgroupLimit    = ioc(iocRead, 43, unsafe.Sizeof(qgroupLimitArgs{}))
	iocGetDevStats    = ioc(iocRead|iocWrite, 52, unsafe.Sizeof(devStatsArgs{}))
	iocSubvolSyncWait = ioc(iocWrite, 65, unsafe.Sizeof(subvolWait{}))
	iocFitrim         = (iocRead|iocWrite)<<30 | unsafe.Sizeof(fstrimRange{})<<16 | 'X'<<8 | 121
)

func ioctl(f *os.File, req uintptr, arg unsafe.Pointer) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno unix.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = unix.Syscall(unix.SYS_IOCTL, fd, req, uintptr(arg))
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}

func Sync(f *os.File) error {
	return ioctl(f, iocSync, nil)
}

func CreateSubvolume(dir *os.File, name string) error {
	if name == "" || len(name) > 255 {
		return fmt.Errorf("subvolume name %q: must be 1 to 255 bytes", name)
	}
	var args volArgsV2
	copy(args.name[:], name)
	return ioctl(dir, iocSubvolCreateV2, unsafe.Pointer(&args))
}

type FSInfo struct {
	FSID       [16]byte
	Devices    uint64
	NodeSize   uint32
	SectorSize uint32
	CsumType   uint16
	Generation uint64
}

func GetFSInfo(f *os.File) (FSInfo, error) {
	args := fsInfoArgs{flags: 1<<0 | 1<<1}
	if err := ioctl(f, iocFSInfo, unsafe.Pointer(&args)); err != nil {
		return FSInfo{}, err
	}
	return FSInfo{
		FSID: args.fsid, Devices: args.numDevices, NodeSize: args.nodesize,
		SectorSize: args.sectorsize, CsumType: args.csumType, Generation: args.generation,
	}, nil
}

type DevInfo struct {
	TotalBytes uint64
	BytesUsed  uint64
}

func GetDevInfo(f *os.File, devid uint64) (DevInfo, error) {
	args := devInfoArgs{devid: devid}
	if err := ioctl(f, iocDevInfo, unsafe.Pointer(&args)); err != nil {
		return DevInfo{}, err
	}
	return DevInfo{TotalBytes: args.totalBytes, BytesUsed: args.bytesUsed}, nil
}

type DevStats struct {
	Write, Read, Flush, Corruption, Generation uint64
}

func GetDevStats(f *os.File, devid uint64) (DevStats, error) {
	args := devStatsArgs{devid: devid, nrItems: devStatValues}
	if err := ioctl(f, iocGetDevStats, unsafe.Pointer(&args)); err != nil {
		return DevStats{}, err
	}
	v := args.values
	return DevStats{Write: v[0], Read: v[1], Flush: v[2], Corruption: v[3], Generation: v[4]}, nil
}

func GrowToDevice(f *os.File, devid uint64) error {
	args := volArgs{}
	copy(args.name[:], fmt.Sprintf("%d:max", devid))
	return ioctl(f, iocResize, unsafe.Pointer(&args))
}

func WaitDeletedSubvolumes(f *os.File) error {
	args := subvolWait{mode: 1}
	err := ioctl(f, iocSubvolSyncWait, unsafe.Pointer(&args))
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

func Trim(f *os.File, minLen uint64) (uint64, error) {
	r := fstrimRange{start: 0, length: ^uint64(0), minlen: minLen}
	if err := ioctl(f, iocFitrim, unsafe.Pointer(&r)); err != nil {
		return 0, err
	}
	return r.length, nil
}

func DiscardRange(dev *os.File, start, length uint64) error {
	r := [2]uint64{start, length}
	return ioctl(dev, unix.BLKDISCARD, unsafe.Pointer(&r))
}

func SubvolumeID(f *os.File) (uint64, error) {
	args := inoLookupArgs{objectid: 256}
	if err := ioctl(f, iocInoLookup, unsafe.Pointer(&args)); err != nil {
		return 0, err
	}
	return args.treeid, nil
}

func QgroupID(level, id uint64) uint64 { return level<<48 | id }

func EnableSimpleQuotas(f *os.File) error {
	args := quotaCtlArgs{cmd: 4}
	return ioctl(f, iocQuotaCtl, unsafe.Pointer(&args))
}

func DisableQuotas(f *os.File) error {
	args := quotaCtlArgs{cmd: 2}
	return ioctl(f, iocQuotaCtl, unsafe.Pointer(&args))
}

func CreateQgroup(f *os.File, id uint64) error {
	args := qgroupCreateArgs{create: 1, qgroupid: id}
	return ioctl(f, iocQgroupCreate, unsafe.Pointer(&args))
}

func AssignQgroup(f *os.File, child, parent uint64) error {
	args := qgroupAssignArgs{assign: 1, src: child, dst: parent}
	return ioctl(f, iocQgroupAssign, unsafe.Pointer(&args))
}

func RemoveQgroup(f *os.File, child, parent uint64) error {
	args := qgroupAssignArgs{assign: 0, src: child, dst: parent}
	return ioctl(f, iocQgroupAssign, unsafe.Pointer(&args))
}

const NoLimit = ^uint64(0)

func LimitQgroup(f *os.File, id, maxReferenced uint64) error {
	args := qgroupLimitArgs{qgroupid: id, flags: 1, maxRfer: maxReferenced}
	return ioctl(f, iocQgroupLimit, unsafe.Pointer(&args))
}

type ScrubProgress struct {
	DataExtentsScrubbed uint64
	TreeExtentsScrubbed uint64
	DataBytesScrubbed   uint64
	TreeBytesScrubbed   uint64
	ReadErrors          uint64
	CsumErrors          uint64
	VerifyErrors        uint64
	NoCsum              uint64
	CsumDiscards        uint64
	SuperErrors         uint64
	MallocErrors        uint64
	UncorrectableErrors uint64
	CorrectedErrors     uint64
	LastPhysical        uint64
	UnverifiedErrors    uint64
}

type scrubArgs struct {
	devid, start, end, flags uint64
	progress                 ScrubProgress
	unused                   [(1024 - 32 - unsafe.Sizeof(ScrubProgress{})) / 8]uint64
}

var (
	iocScrub         = ioc(iocRead|iocWrite, 27, unsafe.Sizeof(scrubArgs{}))
	iocScrubCancel   = ioc(0, 28, 0)
	iocScrubProgress = ioc(iocRead|iocWrite, 29, unsafe.Sizeof(scrubArgs{}))
)

const scrubReadOnly = 1

var ErrNoScrub = errors.New("no scrub is running")

func Scrub(f *os.File, devid uint64, readOnly bool) (ScrubProgress, error) {
	args := scrubArgs{devid: devid, end: ^uint64(0)}
	if readOnly {
		args.flags = scrubReadOnly
	}
	err := ioctl(f, iocScrub, unsafe.Pointer(&args))
	return args.progress, err
}

func ScrubStatus(f *os.File, devid uint64) (ScrubProgress, error) {
	args := scrubArgs{devid: devid}
	err := ioctl(f, iocScrubProgress, unsafe.Pointer(&args))
	if errors.Is(err, unix.ENOTCONN) {
		return ScrubProgress{}, ErrNoScrub
	}
	return args.progress, err
}

func CancelScrub(f *os.File) error {
	err := ioctl(f, iocScrubCancel, nil)
	if errors.Is(err, unix.ENOTCONN) {
		return ErrNoScrub
	}
	return err
}
