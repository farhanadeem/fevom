package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"fev/host/storage"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

type DataImage struct {
	Path      string
	Margin    uint64
	NoBudget  bool
	SpaceFile string
	OnReset   func()
	host      Host

	bmu        sync.Mutex
	budgeting  bool
	again      string
	prev       api.Status
	lastBudget int64
	budgetBoot string
	failedBoot string
	budgets    uint64
	held       uint64
	spaceSeq   uint64
	watch      *lowDiskWatch

	rmu        sync.Mutex
	askedBoot  string
	mountBoot  string
	device     string
	repairBoot string
	repairs    map[string]int
	recovered  bool
	normalBoot string

	mu        sync.Mutex
	resetting bool

	free   func(path string) (uint64, error)
	blocks func(path string) (uint64, error)
}

const (
	WarnDiskLow     = "data.disk_low"
	WarnDataMissing = "data.image_missing"
	WarnSwap        = "data.swap"
)

const LowDiskBytes = 5 << 30

func volumeFree(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

func (m *DataImage) stampPath() string {
	return m.host.Layout().DataImageStamp()
}

func (m *DataImage) known() bool {
	data, err := os.ReadFile(m.stampPath())
	return err == nil && strings.TrimSpace(string(data)) == m.Path
}

func (m *DataImage) remember() {
	path := m.stampPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		err = os.WriteFile(path, []byte(m.Path+"\n"), 0o600)
		if err != nil {
			m.host.Log().Warn("cannot note that the data image was made; a missing image will not be reported", "err", err)
		}
	}
}

func (m *DataImage) forget() {
	os.Remove(m.stampPath())
	m.host.Warn(WarnDataMissing, "")
	m.dropRecovered()
	m.host.Warn(WarnRepaired, "")
	m.host.Warn(WarnCheck, "")
}

func (m *DataImage) look(running bool) {
	dir := filepath.Dir(m.Path)
	free := m.free
	if free == nil {
		free = volumeFree
	}
	if n, err := free(dir); err == nil {
		m.warnLow(n, nil)
	}
	if m.SpaceFile != "" {
		return
	}
	if _, err := os.Stat(m.Path); running && errors.Is(err, fs.ErrNotExist) {
		m.host.Warn(WarnDataMissing, "the data image "+m.Path+" was removed while the VM is running: everything in it is lost when the VM stops. "+
			"If it was moved, stop fev and put it back")
	}
}

func (m *DataImage) Trimmed() {
	m.look(true)
	m.Budget("trim")
}

func (m *DataImage) Name() string    { return "data-image" }
func (m *DataImage) Marks() []string { return nil }

func (m *DataImage) Start(h Host) error {
	m.host = h
	m.heldRecovered()
	if m.SpaceFile != "" {
		m.free, m.blocks = fakeFigures(m.SpaceFile)
	} else {
		h.Handle(api.MethodDataReset, m.reset)
	}
	watch, err := watchLowDisk(m.lowDisk)
	if err != nil {
		h.Log().Warn("data: the Mac's low-disk events cannot be followed", "err", err)
	}
	m.watch = watch
	return nil
}

func (m *DataImage) reset(ctx context.Context, _ json.RawMessage) (any, error) {
	m.mu.Lock()
	if m.resetting {
		m.mu.Unlock()
		return nil, jsonrpc.Errorf(api.CodeFailed, "a reset is already under way")
	}
	m.resetting = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.resetting = false
		m.mu.Unlock()
	}()
	wasUp := m.host.Status().State != api.StateStopped
	if err := m.host.StopVM(ctx); err != nil {
		return nil, err
	}
	freed, _ := storage.Allocated(m.Path)
	if err := storage.Reset(m.Path); err != nil {
		return nil, jsonrpc.Errorf(api.CodeFailed, "cannot remove the data image: %v", err)
	}
	m.forget()
	if m.OnReset != nil {
		m.OnReset()
	}
	m.host.Log().Info("the data image was reset: all Linux data is deleted", "path", m.Path, "freed_bytes", freed)
	m.mu.Lock()
	m.resetting = false
	m.mu.Unlock()
	if wasUp {
		if err := m.host.StartVM(); err != nil {
			return nil, err
		}
	}
	return api.DataResetResult{FreedBytes: freed, Status: m.host.Status()}, nil
}

func (m *DataImage) BeforeBoot() error {
	if m.SpaceFile != "" {
		m.look(false)
		return nil
	}
	m.mu.Lock()
	resetting := m.resetting
	m.mu.Unlock()
	if resetting {
		return errors.New("the data image is being reset")
	}
	if l := m.host.Layout(); l.Placed && filepath.Dir(m.Path) == l.DataDir {
		if _, err := os.Stat(l.DataDir); errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("the data directory %s is not there: is its disk connected? (fev data move put it there; \"fev data move %s\" moves it back once it is)",
				l.DataDir, l.DefaultDataDir)
		}
	}
	was := m.known()
	created, err := storage.Ensure(m.Path, storage.Options{AllowBackup: m.host.Config().Bool(api.KeyDataAllowBackup), Swap: m.swap})
	if created && err == nil {
		m.host.Log().Info("created the data image", "path", m.Path)
		if was {
			m.host.Log().Error("the data image was missing and a new, empty one was made", "path", m.Path)
			m.host.Warn(WarnDataMissing, "the data image "+m.Path+" was missing when the VM started, and a new, empty one was made: "+
				"the images, containers and volumes from before are not in it. If the old image was moved, stop fev and put it back in its place")
		}
		m.remember()
	} else if err == nil && !was {
		m.remember()
	}
	if err != nil {
		return m.explain(err)
	}
	m.look(false)
	return nil
}

func (m *DataImage) swap(err error) {
	if err == nil {
		m.host.Warn(WarnSwap, "")
		return
	}
	m.host.Log().Warn("the swap image cannot be made; the VM starts without it", "err", err)
	m.host.Warn(WarnSwap, "the swap image beside the data image could not be made ("+err.Error()+"): "+
		"the VM runs with compressed memory alone, and a container or machine that needs more memory than that is stopped sooner. "+
		"fev makes it again at the next start")
}

func (m *DataImage) explain(err error) error {
	if rest, ok := strings.CutPrefix(m.Path, "/Volumes/"); ok {
		volume := "/Volumes/" + strings.SplitN(rest, "/", 2)[0]
		if _, serr := os.Stat(volume); errors.Is(serr, fs.ErrNotExist) {
			return fmt.Errorf("the data image %s is on a volume that is not there (%s: is it mounted?): %w", m.Path, volume, err)
		}
	}
	return fmt.Errorf("the data image %s cannot be made ready: %w", m.Path, err)
}

func (m *DataImage) StateChanged(st api.Status) { m.follow(st) }

func (m *DataImage) ConfigChanged(ch api.ConfigChange) {
	allow, ok := ch.Value.(bool)
	if ch.Key != api.KeyDataAllowBackup || !ok {
		return
	}
	if _, err := os.Stat(m.Path); errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err := storage.SetBackupExcluded(m.Path, !allow); err != nil {
		m.host.Log().Error("cannot change the data image's backup attribute", "err", err)
	}
}

func (m *DataImage) Close() {
	if m.watch != nil {
		m.watch.stop()
	}
}
