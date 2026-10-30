package daemon

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/vmmctl"
)

const WarnWritesHeld = "data.writes_held"

const WarnBudgetRefused = "data.budget_refused"

const budgetTimeout = 30 * time.Second

type SpaceSink interface {
	Space(sp vmmctl.Space)
}

func imageBlocks(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	if st.Blocks < 0 {
		return 0, nil
	}
	return uint64(st.Blocks) * 512, nil
}

func (m *DataImage) margin() uint64 {
	if m.Margin != 0 {
		return m.Margin
	}
	return LowDiskBytes
}

func (m *DataImage) figures() (available, image uint64, err error) {
	free, blocks := m.free, m.blocks
	if free == nil {
		free = volumeFree
	}
	if blocks == nil {
		blocks = imageBlocks
	}
	if available, err = free(filepath.Dir(m.Path)); err != nil {
		return 0, 0, err
	}
	if image, err = blocks(m.Path); err != nil {
		return 0, 0, err
	}
	return available, image, nil
}

func (m *DataImage) Budget(why string) {
	if m.NoBudget {
		return
	}
	m.bmu.Lock()
	if m.budgeting {
		m.again = why
		m.bmu.Unlock()
		return
	}
	m.budgeting = true
	m.bmu.Unlock()
	go func() {
		for {
			m.budgetOnce(why)
			m.bmu.Lock()
			why, m.again = m.again, ""
			if why == "" {
				m.budgeting = false
				m.bmu.Unlock()
				return
			}
			m.bmu.Unlock()
		}
	}()
}

func (m *DataImage) budgetIdle() bool {
	m.bmu.Lock()
	defer m.bmu.Unlock()
	return !m.budgeting
}

func (m *DataImage) budgetOnce(why string) {
	log := m.host.Log()
	vm, ok := m.host.VM()
	if !ok {
		return
	}
	switch m.host.Status().State {
	case api.StatePaused:
		log.Debug("data: no budget while the VM is paused; one is set at the wake", "why", why)
		return
	case api.StateStopping, api.StateStopped:
		return
	}
	a := vm.Agent()
	if a == nil {
		return
	}
	available, image, err := m.figures()
	if err != nil {
		log.Warn("data: the Mac's free space cannot be read; the guest keeps the budget it has", "why", why, "err", err)
		return
	}
	m.bmu.Lock()
	holding := m.held != 0
	m.bmu.Unlock()
	params := agent.BudgetParams{AvailableBytes: clampInt64(available), MarginBytes: clampInt64(m.margin()), ImageBytes: clampInt64(image)}
	if why != "trim" && !holding {
		params.ReclaimBytes = clampInt64(m.margin() / reclaimShare)
	}
	ctx, cancel := context.WithTimeout(context.Background(), budgetTimeout)
	r, err := a.StorageBudget(ctx, params)
	cancel()
	if err == nil && r.Deferred {
		available, image, r, err = m.reclaimFirst(a, why, params, r)
	}
	if err != nil {
		switch {
		case m.host.Status().State == api.StatePaused:
			log.Info("data: the budget was not set before the VM was paused; one is set at the wake", "why", why, "err", err)
		case agent.IsNotImplemented(err):
		case agent.IsNoDataFilesystem(err), agent.IsDataReadOnly(err):
			log.Info("data: no budget: the guest has no data filesystem that can be written", "why", why, "err", agentReason(err))
			m.host.Warn(WarnBudgetRefused, "")
		default:
			m.bmu.Lock()
			said := m.failedBoot == vm.BootID()
			m.failedBoot = vm.BootID()
			m.bmu.Unlock()
			if _, running := m.host.VM(); running {
				if !said {
					log.Warn("data: the guest did not take a free-space budget; containers keep the limit they had", "why", why, "err", err)
				}
				m.host.Warn(WarnBudgetRefused, "fev could not tell its VM how much free space the Mac has ("+agentReason(err)+"). "+
					"Containers may see no limit and the data disk's whole size, or a limit that is out of date; if the Mac fills, their writes wait for room. "+
					"This happens when the quota group fev keeps on its data disk was removed or disabled from a privileged container: "+
					"restarting fev sets the limit again if the group is there")
			}
		}
		return
	}
	m.host.Warn(WarnBudgetRefused, "")
	held, told := uint64(0), false
	m.bmu.Lock()
	said := m.spaceSeq
	m.bmu.Unlock()
	if ctl := vm.Control(); ctl != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		br, err := ctl.Budget(ctx, vmmctl.Baseline{AvailableBytes: available, ImageBytes: image, MarginBytes: m.margin()})
		cancel()
		if err == nil {
			held, told = br.HeldRequests, true
		} else if _, running := m.host.VM(); running {
			log.Warn("data: the VMM was not told the budget's figures; its check measures from the last it has", "why", why, "err", err)
		}
	}
	m.bmu.Lock()
	moved := r.BudgetBytes - m.lastBudget
	first := m.budgetBoot != vm.BootID()
	big := first || moved >= 64<<20 || moved <= -(64<<20)
	if big {
		m.lastBudget, m.budgetBoot = r.BudgetBytes, vm.BootID()
	}
	m.budgets++
	m.bmu.Unlock()
	level := log.Debug
	if big {
		level = log.Info
	}
	level("data: free-space budget set", "why", why, "budget_bytes", r.BudgetBytes, "limit_bytes", r.LimitBytes,
		"mac_available_bytes", available, "margin_bytes", m.margin(), "image_bytes", image, "used_bytes", r.UsedBytes,
		"held_requests", held, "ms", math.Round(r.DurationMS*100)/100)
	m.warnLow(available, &r.BudgetBytes)
	budget := &api.DataBudget{ReferencedBytes: r.ReferencedBytes, BudgetBytes: r.BudgetBytes, LimitBytes: r.LimitBytes,
		Limited: r.Limited, At: time.Now().UTC().Format(time.RFC3339)}
	if !r.Limited {
		budget.LimitBytes = 0
	}
	boot := vm.BootID()
	updateStatus(m.host, func(st *api.Status) {
		if st.BootID == boot {
			st.Data = &api.DataStatus{Budget: budget}
		}
	})
	if told {
		m.bmu.Lock()
		current := m.spaceSeq == said
		m.bmu.Unlock()
		if current {
			m.noteHeld(held, available)
		}
	}
}

const reclaimShare = 8

const trimTimeout = 60 * time.Second

func (m *DataImage) reclaimFirst(a *agent.Client, why string, params agent.BudgetParams, deferred *agent.Budget) (available, image uint64, r *agent.Budget, err error) {
	log := m.host.Log()
	ctx, cancel := context.WithTimeout(context.Background(), trimTimeout)
	t, terr := a.StorageTrim(ctx, agent.TrimParams{})
	cancel()
	if terr != nil {
		log.Warn("data: the trim before a free-space budget failed; the budget is set without it", "why", why, "err", terr)
	}
	available, image = uint64(params.AvailableBytes), uint64(params.ImageBytes)
	if a2, i2, ferr := m.figures(); ferr == nil {
		if terr == nil {
			log.Info("data: the image held more than the guest's filesystem uses; trimmed before the budget", "why", why,
				"image_bytes_before", image, "image_bytes", i2, "used_bytes", deferred.UsedBytes, "reserved_bytes", deferred.ReservedBytes,
				"mac_available_bytes", a2, "ms", math.Round(t.DurationMS))
		}
		available, image = a2, i2
	}
	params.AvailableBytes, params.ImageBytes, params.ReclaimBytes = clampInt64(available), clampInt64(image), 0
	ctx, cancel = context.WithTimeout(context.Background(), budgetTimeout)
	r, err = a.StorageBudget(ctx, params)
	cancel()
	return available, image, r, err
}

func agentReason(err error) string {
	s := err.Error()
	if i := strings.Index(s, " (code "); i > 0 {
		s = s[:i]
	}
	return s
}

func clampInt64(n uint64) int64 {
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}

func (m *DataImage) warnLow(available uint64, budget *int64) {
	if available >= m.margin() {
		m.host.Warn(WarnDiskLow, "")
		return
	}
	gib := func(n uint64) float64 { return float64(n) / (1 << 30) }
	text := fmt.Sprintf("the Mac's disk is nearly full: %.1f GiB free on the volume that holds fev's data (%s), and fev leaves %.1f GiB of it alone. ",
		gib(available), filepath.Dir(m.Path), gib(m.margin()))
	if budget != nil && *budget > 0 {
		text += fmt.Sprintf("Containers can write %.1f GiB more; past that a write ", gib(uint64(*budget)))
		text += `fails with "No space left on device". Nothing is lost and nothing goes read-only: reads, deletes and changes to existing data go on. ` +
			"Free space on the Mac, or remove images and volumes (docker system prune)"
	} else {
		text += `Containers are told their disk is full: a write that needs new space fails with "No space left on device", ` +
			"and no container can be started or removed. Nothing is lost and nothing goes read-only: reads and deletes go on. " +
			"Free space on the Mac, or remove a volume or an image that nothing uses (docker volume rm, docker rmi): " +
			"docker system prune works again once one has been removed"
	}
	m.host.Warn(WarnDiskLow, text)
}

func (m *DataImage) noteHeld(held, available uint64) {
	m.bmu.Lock()
	was := m.held
	m.held = held
	m.bmu.Unlock()
	switch {
	case held != 0:
		m.host.Warn(WarnWritesHeld, fmt.Sprintf("the Mac's disk is full (%d MiB free on the volume that holds fev's data, %s): "+
			"writes to fev's data disk are waiting for room, and containers that write are stalled, not failed. "+
			"They go on by themselves when there is space: free some on the Mac", available>>20, filepath.Dir(m.Path)))
		if was == 0 {
			m.host.Log().Error("data: the Mac has no room for the data image; the VMM holds the guest's writes until it has", "held_requests", held, "mac_available_bytes", available)
		}
	case was != 0:
		m.host.Warn(WarnWritesHeld, "")
		m.host.Log().Info("data: the writes that waited for room on the Mac are done", "mac_available_bytes", available)
	default:
		m.host.Warn(WarnWritesHeld, "")
	}
}

func (m *DataImage) Space(sp vmmctl.Space) {
	m.bmu.Lock()
	m.spaceSeq++
	m.bmu.Unlock()
	m.noteHeld(sp.HeldRequests, sp.AvailableBytes)
	m.Budget("vmm")
}

func (m *DataImage) Quiet() { m.Budget("quiet") }

func (m *DataImage) lowDisk() {
	_, running := m.host.VM()
	m.look(running)
	m.Budget("low-disk")
}

func (m *DataImage) follow(st api.Status) {
	m.bmu.Lock()
	prev := m.prev
	m.prev = st
	m.bmu.Unlock()
	if st.BootID != "" && (st.State == api.StateRunning || (st.State == api.StateStarting && st.Phase == api.PhaseEngine)) {
		m.rmu.Lock()
		ask := m.askedBoot != st.BootID
		m.askedBoot = st.BootID
		m.rmu.Unlock()
		if ask {
			go m.mounted(st.BootID)
		}
		if st.Engine == api.EngineReady {
			m.sound(st.BootID)
		}
	}
	switch {
	case st.State == api.StateStarting && st.Phase == api.PhaseInit && (prev.State != api.StateStarting || prev.Phase != api.PhaseInit || prev.BootID != st.BootID):
		m.Budget("boot")
	case st.State == api.StateRunning && prev.State == api.StatePaused:
		m.Budget("wake")
	case st.State == api.StateStopped || (st.State == api.StateStarting && prev.State != api.StateStarting):
		m.bmu.Lock()
		held := m.held
		m.held = 0
		m.bmu.Unlock()
		if held != 0 {
			m.host.Warn(WarnWritesHeld, "")
		}
		m.host.Warn(WarnBudgetRefused, "")
	}
}

func fakeFigures(path string) (free func(string) (uint64, error), blocks func(string) (uint64, error)) {
	read := func(i int) (uint64, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		var v [2]uint64
		if _, err := fmt.Sscan(string(data), &v[0], &v[1]); err != nil {
			return 0, fmt.Errorf("%s: %w", path, err)
		}
		return v[i], nil
	}
	return func(string) (uint64, error) { return read(0) }, func(string) (uint64, error) { return read(1) }
}
