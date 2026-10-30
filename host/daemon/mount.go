package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fev/proto/agent"
	api "fev/proto/daemon"
)

const (
	WarnRecovered = "data.recovered"
	WarnRepaired  = "data.repaired"
	WarnCheck     = "data.check"
)

const mountTimeout = 30 * time.Second

const (
	repairPrefix = "BTRFS info (device "
	repairText   = "): read error corrected: ino 0 off "
)

func repairedDevice(line string) string {
	rest, ok := strings.CutPrefix(line, repairPrefix)
	if !ok {
		return ""
	}
	device, _, ok := strings.Cut(rest, repairText)
	if !ok || device == "" || strings.ContainsAny(device, " ()/") {
		return ""
	}
	return device
}

type KernelSink interface {
	KernelLine(boot, text string)
}

func (m *DataImage) KernelLine(boot, text string) {
	device := repairedDevice(text)
	if device == "" {
		return
	}
	m.rmu.Lock()
	if m.repairBoot != boot {
		m.repairBoot, m.repairs = boot, map[string]int{}
	}
	m.repairs[device]++
	warn := m.repairedLocked(boot)
	m.rmu.Unlock()
	if warn != "" {
		m.host.Log().Error("data: the guest's kernel repaired a metadata block of the data disk from its second copy", "boot", boot, "line", text)
		m.host.Warn(WarnRepaired, warn)
	}
}

func (m *DataImage) repairedLocked(boot string) string {
	if m.repairBoot != boot || m.mountBoot != boot || m.device == "" {
		return ""
	}
	n := m.repairs[m.device]
	if n == 0 {
		return ""
	}
	blocks := "a metadata block"
	if n > 1 {
		blocks = fmt.Sprintf("%d metadata blocks", n)
	}
	return "fev's data disk had " + blocks + " that failed its check, and the kernel rewrote it from the second copy the disk keeps (boot " + boot + "). " +
		"Nothing was lost: metadata is kept twice and checked on every read. File contents are kept once and are not checked, so this says nothing for or against them. " +
		"Once is a fault the disk format is there to absorb; if it comes back, suspect the Mac's disk, and keep copies of what the containers hold that you cannot make again. " +
		"`fev logs --files --source kernel` has the kernel's lines. This warning is withdrawn at the next start of the VM (`fev restart`)"
}

func (m *DataImage) mounted(boot string) {
	vm, ok := m.host.VM()
	if !ok || vm.BootID() != boot {
		return
	}
	a := vm.Agent()
	if a == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), mountTimeout)
	info, err := a.StorageInfo(ctx)
	cancel()
	log := m.host.Log()
	switch {
	case err == nil:
	case agent.IsNotImplemented(err):
		return
	case agent.IsNoDataFilesystem(err):
		m.dropRecovered()
		m.host.Warn(WarnRepaired, "")
		m.host.Warn(WarnCheck, "")
		return
	default:
		if _, running := m.host.VM(); running {
			log.Warn("data: the guest did not say how the data disk was mounted", "boot", boot, "err", err)
		}
		return
	}
	m.rmu.Lock()
	m.mountBoot, m.device = boot, filepath.Base(info.Device)
	repaired := m.repairedLocked(boot)
	m.rmu.Unlock()
	if repaired == "" {
		m.host.Warn(WarnRepaired, "")
	} else {
		log.Error("data: the guest's kernel repaired a metadata block of the data disk from its second copy", "boot", boot)
		m.host.Warn(WarnRepaired, repaired)
	}
	m.checked(a, boot, info.Check)
	text := RecoveredWarning(info, boot, m.Path)
	if text == "" {
		m.rmu.Lock()
		m.normalBoot = boot
		m.rmu.Unlock()
		if st := m.host.Status(); st.BootID == boot && st.Engine == api.EngineReady {
			m.sound(boot)
		}
		return
	}
	log.Error("data: the data disk did not mount normally", "boot", boot, "rung", info.MountRung, "read_only", info.ReadOnly, "steps", fmt.Sprintf("%+v", info.Steps))
	m.rmu.Lock()
	m.normalBoot, m.recovered = "", true
	m.rmu.Unlock()
	m.keepRecovered(text)
	m.host.Warn(WarnRecovered, text)
}

func (m *DataImage) checked(a *agent.Client, boot string, check *agent.StorageCheck) {
	if check == nil || check.State != agent.OperationRunning {
		m.host.Warn(WarnCheck, CheckWarning(check, boot))
		return
	}
	m.host.Warn(WarnCheck, "")
	ctx, cancel := context.WithTimeout(context.Background(), mountTimeout)
	defer cancel()
	op, err := a.StorageCheckFollow(ctx, func(p agent.OperationProgress) {
		if p.State == agent.OperationRunning {
			return
		}
		if vm, ok := m.host.VM(); !ok || vm.BootID() != boot {
			return
		}
		var done agent.StorageCheck
		if len(p.Result) == 0 || json.Unmarshal(p.Result, &done) != nil {
			m.host.Log().Warn("data: the data disk's check ended with no result", "boot", boot, "state", p.State, "err", p.Error)
			return
		}
		m.host.Warn(WarnCheck, CheckWarning(&done, boot))
	})
	switch {
	case err == nil:
		m.host.Log().Info("data: the data disk's check after the recovery is running; following it", "boot", boot, "operation", op.ID)
	case !agent.IsNotImplemented(err):
		m.host.Log().Warn("data: the data disk's check cannot be followed", "boot", boot, "err", err)
	}
}

func CheckWarning(check *agent.StorageCheck, boot string) string {
	if check == nil || check.State != agent.OperationDone || check.Uncorrectable == 0 {
		return ""
	}
	sectors := "a sector"
	if check.Uncorrectable > 1 {
		sectors = fmt.Sprintf("%d sectors", check.Uncorrectable)
	}
	how := "after the data disk needed a recovery to mount"
	if !check.Auto {
		how = "when asked"
	}
	return "fev's data disk has " + sectors + " that failed its checksum and had no good copy to repair it from (the check of the whole disk fev ran " + how + ", boot " + boot + "). " +
		"Whatever was stored there cannot be read: part of the filesystem's own records, or part of a file written before fev began writing file data in place. " +
		"Copy out what you need while the disk still mounts: `fev salvage DIR` copies the volumes and machines it can read into a folder on the Mac, and says what it could not. " +
		"Then `fev reset` deletes the disk and makes a new, empty one. " +
		"`fev logs --files --source init` has what the check said. This warning is withdrawn when the VM has next started (`fev restart`)"
}

func (m *DataImage) recoveredPath() string {
	return filepath.Join(filepath.Dir(m.host.Layout().ConfigFile()), "data-image.recovered")
}

func (m *DataImage) keepRecovered(text string) {
	path := m.recoveredPath()
	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		err = os.WriteFile(path, []byte(text+"\n"), 0o600)
	}
	if err != nil {
		m.host.Log().Warn("data: the recovery's warning cannot be kept; it will not outlive this daemon", "err", err)
	}
}

func (m *DataImage) dropRecovered() {
	m.rmu.Lock()
	m.recovered = false
	m.rmu.Unlock()
	os.Remove(m.recoveredPath())
	m.host.Warn(WarnRecovered, "")
}

func (m *DataImage) heldRecovered() {
	data, err := os.ReadFile(m.recoveredPath())
	text := strings.TrimSpace(string(data))
	if err != nil || text == "" {
		return
	}
	m.rmu.Lock()
	m.recovered = true
	m.rmu.Unlock()
	m.host.Warn(WarnRecovered, text)
}

func (m *DataImage) sound(boot string) {
	m.rmu.Lock()
	withdraw := m.recovered && m.normalBoot == boot
	m.rmu.Unlock()
	if withdraw {
		m.host.Log().Info("data: the data disk mounted normally and the engine is up on it; the recovery's warning is withdrawn", "boot", boot)
		m.dropRecovered()
	}
}

func RecoveredWarning(info *agent.StorageInfo, boot, image string) string {
	if info.MountRung <= 1 {
		return ""
	}
	name, lost := "", ""
	for _, s := range info.Steps {
		if s.Lost != "" {
			if lost != "" {
				lost += "; "
			}
			lost += s.Lost
		}
		if s.Rung == info.MountRung {
			name = s.Name
		}
	}
	step := fmt.Sprintf("step %d of 5", info.MountRung)
	if name != "" {
		step += ", " + name
	}
	if info.ReadOnly {
		unchanged := "Nothing on it was changed. "
		if lost != "" {
			unchanged = "Nothing on it was changed by this mount, but an earlier step of the recovery changed it: " + lost + ". "
		}
		keep := "`fev salvage DIR` copies its volumes and machines to the folder DIR on the Mac, and changes nothing on the disk. The disk is the file " + image + ": " +
			"to keep it whole for a later attempt, `fev stop` and copy that file. " +
			"`fev reset` then deletes it and makes a new, empty disk: every image, container and volume on it is lost. " +
			"`fev logs --files --source init` has what each step of the recovery said"
		if info.MountRung == 4 {
			return "fev's data disk is damaged and could not be repaired (boot " + boot + "). It is mounted read-only as of an older commit, to be copied from (" + step + "), and the Docker engine is not running on it. " +
				"It does not have the most recent commits, and a file rewritten in place since that commit has the commit's size with newer contents inside it, so a database copied from it needs its own check. " +
				unchanged + "Every start of the VM finds it the same way until it is reset. " + keep
		}
		return "fev's data disk is damaged and could not be repaired (boot " + boot + "). It is mounted read-only with its checks off, to be copied from (" + step + "), and the Docker engine is not running on it. " +
			unchanged + keep
	}
	what := "Nothing was given up to mount it."
	if lost != "" {
		what = "Data was lost: " + lost + "."
	}
	return "fev's data disk did not mount normally when the VM started (boot " + boot + ") and was recovered (" + step + "). " + what + " " +
		"The mount shows that the filesystem's own records are whole. It does not show that every file's contents are: file data is written in place and carries no checksum, " +
		"so check what matters (a database's own check, a rebuild of an image) before relying on it. " +
		"`fev logs --files --source init` has what each step of the recovery said. This warning is withdrawn when the VM has next started with a normal mount and the Docker engine has come up on it (`fev restart`). " +
		"If the engine does not come up, what it keeps on the disk did not survive the recovery: `fev reset` deletes the disk and makes a new, empty one, and every image, container and volume on it is lost"
}
