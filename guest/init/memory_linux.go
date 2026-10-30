package main

import (
	"os"
	"strconv"
	"strings"

	"fev/guest/agent"
	"fev/guest/internal/reclaim"
)

var engineTree = reclaim.Tree{Root: engineCgroup, Own: []string{engineSupervisorLeaf, "engine", "exec", "containerd", "k3s"}, Containers: "docker", System: cgroupRoot,
	Others: []string{viewCgroup}}

const engineSupervisorLeaf = "init.scope"

func memoryTree() reclaim.Tree {
	if reclaim.Exists(engineCgroup) {
		return engineTree
	}
	return reclaim.Tree{System: engineTree.System, Others: engineTree.Others}
}

func (s *system) IdleCache() (idle, own, live uint64, err error) {
	survey, err := memoryTree().Look()
	if err != nil {
		return 0, 0, 0, err
	}
	return survey.Idle(), survey.Own(), survey.Live, nil
}

func (s *system) MemoryPressure() (float64, error) {
	return reclaim.Pressure("/proc/pressure/memory")
}

func (s *system) ReclaimIdle(n uint64) error {
	survey, err := memoryTree().Look()
	if err != nil {
		return err
	}
	return survey.Reclaim(n)
}

func (s *system) CompactMemory() error {
	return os.WriteFile("/proc/sys/vm/compact_memory", []byte("1"), 0)
}

func (s *system) RunningCache() ([]agent.RunningCache, error) {
	if !reclaim.Exists(engineCgroup) {
		return nil, agent.ErrNotImplemented
	}
	survey, err := engineTree.Look()
	if err != nil {
		return nil, err
	}
	out := make([]agent.RunningCache, 0, len(survey.Running))
	for _, r := range survey.Running {
		out = append(out, agent.RunningCache{Dir: r.Dir, InactiveBytes: r.InactiveBytes, ActiveBytes: r.ActiveBytes,
			SlabBytes: r.SlabBytes, Refaults: r.Refaults, CPUUsec: r.CPUUsec})
	}
	return out, nil
}

func (s *system) TrimRunning(dir string, n uint64) error {
	return reclaim.Trim(dir, n)
}

const reportingOrder = "/sys/module/page_reporting/parameters/page_reporting_order"

func (s *system) SetReportingOrder(order int) (int, error) {
	data, err := os.ReadFile(reportingOrder)
	if err != nil {
		return 0, err
	}
	was, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, err
	}
	return was, os.WriteFile(reportingOrder, []byte(strconv.Itoa(order)), 0)
}

func (s *system) Unreportable() (uint64, error) {
	order, err := os.ReadFile(reportingOrder)
	if err != nil {
		return 0, err
	}
	below, err := strconv.Atoi(strings.TrimSpace(string(order)))
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile("/proc/buddyinfo")
	if err != nil {
		return 0, err
	}
	var n uint64
	page := uint64(os.Getpagesize())
	for _, line := range strings.Split(string(data), "\n") {
		_, counts, ok := strings.Cut(line, "zone")
		if !ok {
			continue
		}
		fields := strings.Fields(counts)
		for o := 0; o < below && o+1 < len(fields); o++ {
			c, err := strconv.ParseUint(fields[o+1], 10, 64)
			if err != nil {
				return 0, err
			}
			n += c << o * page
		}
	}
	return n, nil
}
