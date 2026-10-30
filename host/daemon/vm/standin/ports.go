package standin

import (
	"fmt"
	"sort"

	api "fev/proto/agent"
)

func (a *Agent) machinePorts() api.MachinePortsList {
	m := a.machines()
	m.mu.Lock()
	defer m.mu.Unlock()
	l := api.MachinePortsList{Machines: []api.MachinePorts{}}
	for _, fm := range m.byName {
		if fm.Info.State == api.MachineRunning {
			l.Machines = append(l.Machines, api.MachinePorts{Name: fm.Info.Name, Address: fm.Info.Address, Seq: max(m.portSeq, 1), Ports: append([]api.MachinePort{}, fm.ports...)})
		}
	}
	sort.Slice(l.Machines, func(i, j int) bool { return l.Machines[i].Name < l.Machines[j].Name })
	return l
}

func (a *Agent) SetMachinePorts(name string, ports []api.MachinePort) error {
	m := a.machines()
	m.mu.Lock()
	defer m.mu.Unlock()
	fm := m.byName[name]
	if fm == nil || fm.Info.State != api.MachineRunning {
		return fmt.Errorf("%s is not a running machine", name)
	}
	fm.ports = append([]api.MachinePort{}, ports...)
	m.portsLocked(fm)
	return nil
}

func (m *machineTable) portsLocked(fm *fakeMachine) {
	m.portSeq = max(m.portSeq, 1) + 1
	mp := api.MachinePorts{Name: fm.Info.Name, Seq: m.portSeq, Ports: append([]api.MachinePort{}, fm.ports...)}
	if fm.Info.State == api.MachineRunning {
		mp.Address = fm.Info.Address
	}
	m.a.mu.Lock()
	host := m.a.host
	m.a.mu.Unlock()
	if host != nil {
		host.Notify(api.NotifyMachinePorts, mp)
	}
}
