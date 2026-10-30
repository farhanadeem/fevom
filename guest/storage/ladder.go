package storage

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

var MkfsOptions = []string{
	"-q", "-K", "-L", "fev-data",
	"-n", "16384", "-s", "4096", "--csum", "crc32c",
	"-m", "dup", "-d", "single",
	"-O", "extref,skinny-metadata,no-holes,free-space-tree,block-group-tree,squota",
}

const (
	MountOptions    = "discard=async,nodatasum,nodatacow"
	MountOptionsCow = "discard=async"
)

var ErrNoTool = errors.New("the btrfs tool is not available")

type Device interface {
	Mount(options string, readOnly bool) error
	Unmount() error
	Tool(args ...string) (string, error)
	Probe() (Probe, error)
}

type Step struct {
	Rung    int
	Name    string
	Lossy   bool
	Err     string
	Skipped string
	Lost    string
}

const (
	LostLog  = "the fsync log could not be replayed and was dropped: writes acknowledged since the last commit (up to 30 s before the stop) are gone"
	ViewRoot = "it shows the filesystem as of an older commit, without the most recent commits and the fsync log; " +
		"a file rewritten in place since then has that commit's size with newer contents, so a database copied from it needs its own check"
)

const BackupRootOptions = "rescue=usebackuproot,rescue=nologreplay"

type Outcome struct {
	Rung     int
	ReadOnly bool
	Steps    []Step
}

const (
	RungNormal       = 1
	RungSuperRecover = 2
	RungZeroLog      = 3
	RungBackupRoot   = 4
	RungSalvage      = 5
)

func Climb(d Device, first Probe, mountOptions string, logf func(string, ...any)) (Outcome, error) {
	var out Outcome
	done := func(rung int, name string, lost string, readOnly bool) (Outcome, error) {
		out.Steps = append(out.Steps, Step{Rung: rung, Name: name, Lossy: lost != "", Lost: lost})
		out.Rung, out.ReadOnly = rung, readOnly
		return out, nil
	}
	failed := func(rung int, name string, lossy bool, err error) {
		out.Steps = append(out.Steps, Step{Rung: rung, Name: name, Lossy: lossy, Err: err.Error()})
		logf("storage: rung %d (%s) failed: %v", rung, name, err)
	}
	skipped := func(rung int, name string, lossy bool, why string) {
		out.Steps = append(out.Steps, Step{Rung: rung, Name: name, Lossy: lossy, Skipped: why})
		logf("storage: rung %d (%s) not tried: %s", rung, name, why)
	}

	err := d.Mount(mountOptions, false)
	if err == nil {
		return done(RungNormal, "mount", "", false)
	}
	failed(RungNormal, "mount", false, err)
	if notTheDisk(err) {
		return out, fmt.Errorf("the data filesystem was not mounted and was left as it is: %v, "+
			"which is not a sign of damage to it, so no repair was tried", err)
	}

	const recoverName = "btrfs rescue super-recover"
	switch {
	case first.PrimaryValid:
		skipped(RungSuperRecover, recoverName, false, "the first superblock is valid")
	case !first.MirrorValid:
		skipped(RungSuperRecover, recoverName, false, "no superblock copy is valid")
	default:
		output, err := d.Tool("rescue", "super-recover", "-y")
		if errors.Is(err, ErrNoTool) {
			skipped(RungSuperRecover, recoverName, false, err.Error())
			break
		}
		var exit interface{ ExitStatus() int }
		if errors.As(err, &exit) && exit.ExitStatus() == 2 {
			err = nil
		}
		if err != nil {
			failed(RungSuperRecover, recoverName, false, fmt.Errorf("%v: %s", err, lastLine(output)))
			break
		}
		logf("storage: rung %d: rewrote the first superblock from a mirror copy", RungSuperRecover)
		if err := d.Mount(mountOptions, false); err != nil {
			failed(RungSuperRecover, recoverName, false, err)
			break
		}
		return done(RungSuperRecover, recoverName, "", false)
	}

	const zeroName = "btrfs rescue zero-log"
	now, err := d.Probe()
	switch {
	case err != nil:
		skipped(RungZeroLog, zeroName, true, fmt.Sprintf("cannot read the superblock: %v", err))
	case !now.PrimaryValid || now.LogRoot == 0:
		skipped(RungZeroLog, zeroName, true, "the filesystem has no fsync log to replay")
	default:
		if err := d.Mount("rescue=nologreplay", true); err != nil {
			skipped(RungZeroLog, zeroName, true, fmt.Sprintf("it does not mount without the log either: %v", err))
			break
		}
		if err := d.Unmount(); err != nil {
			failed(RungZeroLog, zeroName, true, fmt.Errorf("unmount after the trial mount: %w", err))
			return out, ladderError(out)
		}
		output, err := d.Tool("rescue", "zero-log")
		if errors.Is(err, ErrNoTool) {
			skipped(RungZeroLog, zeroName, true, err.Error())
			break
		}
		if err != nil {
			failed(RungZeroLog, zeroName, true, fmt.Errorf("%v: %s", err, lastLine(output)))
			break
		}
		logf("storage: rung %d: DATA MAY BE LOST: %s", RungZeroLog, LostLog)
		if err := d.Mount(mountOptions, false); err != nil {
			failed(RungZeroLog, zeroName, true, err)
			out.Steps[len(out.Steps)-1].Lost = LostLog
			break
		}
		return done(RungZeroLog, zeroName, LostLog, false)
	}

	const backupName = "read-only rescue=usebackuproot"
	if err := d.Mount(BackupRootOptions, true); err != nil {
		failed(RungBackupRoot, backupName, false, err)
	} else {
		logf("storage: rung %d: mounted READ-ONLY from an older tree root, to be copied from: %s. "+
			"Nothing on the disk was changed, and the engine is not started on it", RungBackupRoot, ViewRoot)
		return done(RungBackupRoot, backupName, "", true)
	}

	const salvageName = "read-only rescue=all"
	if err := d.Mount("rescue=all", true); err != nil {
		failed(RungSalvage, salvageName, false, err)
		return out, ladderError(out)
	}
	logf("storage: rung %d: mounted READ-ONLY for salvage with checks off; nothing was repaired. "+
		"Copy out what is needed, then reset", RungSalvage)
	return done(RungSalvage, salvageName, "", true)
}

func notTheDisk(err error) bool {
	for _, errno := range []unix.Errno{unix.ENOMEM, unix.EBUSY, unix.ENODEV, unix.EPERM, unix.EINTR, unix.EAGAIN} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

func ladderError(out Outcome) error {
	var parts []string
	for _, s := range out.Steps {
		switch {
		case s.Err != "":
			parts = append(parts, fmt.Sprintf("rung %d (%s): %s", s.Rung, s.Name, s.Err))
		case s.Skipped != "":
			parts = append(parts, fmt.Sprintf("rung %d (%s) not tried: %s", s.Rung, s.Name, s.Skipped))
		}
	}
	return fmt.Errorf("the data filesystem cannot be mounted and was left as it is; %s. %s", strings.Join(parts, "; "), ResetRemedy)
}

const ResetRemedy = "`fev reset` deletes this disk and makes a new, empty one: every image, container and volume on it is lost"

const KeepRemedy = "`fev salvage DIR` cannot read it either: every tool that reads a btrfs filesystem starts from a superblock, and none is left. " +
	"To keep what is in it for a later attempt, `fev stop`, then copy the image file (the daemon's data directory, data.img) before anything else."

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
