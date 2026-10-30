package storage

import (
	"errors"
	"fmt"
	"runtime"
	"time"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/internal/btrfs"
)

func AutoCheck(out Outcome) bool {
	return out.Rung >= RungSuperRecover && out.Rung <= RungBackupRoot
}

const (
	ioprioWhoProcess = 1
	ioprioIdle       = 3 << 13
)

func (v *Volume) StartCheck(auto bool) (api.StorageCheck, bool, <-chan struct{}) {
	v.checkMu.Lock()
	defer v.checkMu.Unlock()
	if v.check != nil && v.check.State == api.OperationRunning {
		return v.checkNowLocked(), false, v.checkDone
	}
	c := &api.StorageCheck{State: api.OperationRunning, Auto: auto, ReadOnly: v.out.ReadOnly, StartedMS: v.cfg.NowMS()}
	done := make(chan struct{})
	v.check, v.checkDone = c, done
	snapshot := *c
	go v.scrub(c, done)
	return snapshot, true, done
}

func (v *Volume) FollowCheck() (c api.StorageCheck, done <-chan struct{}, ok bool) {
	v.checkMu.Lock()
	defer v.checkMu.Unlock()
	if v.check == nil {
		return api.StorageCheck{}, nil, false
	}
	return v.checkNowLocked(), v.checkDone, true
}

func (v *Volume) scrub(c *api.StorageCheck, done chan struct{}) {
	runtime.LockOSThread()
	defer close(done)
	if _, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, 0, ioprioIdle); errno != 0 {
		v.cfg.Logf("storage: check: idle I/O priority not set (%v); checking at the normal one", errno)
	}
	var all, old unix.Sigset_t
	for i := range all.Val {
		all.Val[i] = ^all.Val[i]
	}
	masked := unix.PthreadSigmask(unix.SIG_SETMASK, &all, &old) == nil
	p, err := btrfs.Scrub(v.root, 1, c.ReadOnly)
	if masked {
		unix.PthreadSigmask(unix.SIG_SETMASK, &old, nil)
	}
	v.checkMu.Lock()
	fill(c, p)
	c.EndedMS = v.cfg.NowMS()
	switch {
	case err == nil:
		c.State = api.OperationDone
	case errors.Is(err, unix.ECANCELED):
		c.State, c.Error = api.OperationCancelled, "cancelled: the data filesystem was unmounted"
	default:
		c.State, c.Error = api.OperationFailed, fmt.Sprintf("scrub: %v", err)
	}
	result := *c
	v.checkMu.Unlock()
	v.cfg.Logf("storage: check %s in %.0f ms: %s", result.State, result.EndedMS-result.StartedMS, CheckSummary(result))
}

func fill(c *api.StorageCheck, p btrfs.ScrubProgress) {
	c.ScrubbedBytes = int64(p.DataBytesScrubbed + p.TreeBytesScrubbed)
	c.Corrected, c.Uncorrectable = p.CorrectedErrors, p.UncorrectableErrors
}

func (v *Volume) Check() *api.StorageCheck {
	v.checkMu.Lock()
	defer v.checkMu.Unlock()
	if v.check == nil {
		return nil
	}
	c := v.checkNowLocked()
	return &c
}

func (v *Volume) checkNowLocked() api.StorageCheck {
	c := *v.check
	if c.State == api.OperationRunning {
		if p, err := btrfs.ScrubStatus(v.root, 1); err == nil {
			fill(&c, p)
		}
	}
	return c
}

func CheckSummary(c api.StorageCheck) string {
	what := fmt.Sprintf("%d bytes verified (metadata, and data written before data in place)", c.ScrubbedBytes)
	switch {
	case c.Uncorrectable > 0:
		what += fmt.Sprintf(", %d sectors damaged with no good copy", c.Uncorrectable)
	case c.Corrected > 0 && c.ReadOnly:
		what += fmt.Sprintf(", %d sectors damaged that their second copy could repair (read-only: not repaired)", c.Corrected)
	case c.Corrected > 0:
		what += fmt.Sprintf(", %d sectors repaired from their second copy", c.Corrected)
	default:
		what += ", no errors"
	}
	if c.Error != "" {
		what += "; " + c.Error
	}
	return what
}

func (v *Volume) stopCheck() {
	v.checkMu.Lock()
	running := v.check != nil && v.check.State == api.OperationRunning
	done := v.checkDone
	v.checkMu.Unlock()
	if !running {
		return
	}
	if err := btrfs.CancelScrub(v.root); err != nil && !errors.Is(err, btrfs.ErrNoScrub) {
		v.cfg.Logf("storage: check: cancel: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		v.cfg.Logf("storage: check: still running 10 s after it was cancelled")
	}
}
