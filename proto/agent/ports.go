package agent

import "context"

const (
	MethodMachinePorts = "machine.ports"
	NotifyMachinePorts = "machine.ports_changed"
)

const (
	PortTCP = "tcp"
	PortUDP = "udp"
)

type MachinePort struct {
	Proto   string `json:"proto"`
	Address string `json:"address"`
	Port    uint16 `json:"port"`
}

type MachinePorts struct {
	Name    string        `json:"name"`
	Address string        `json:"address,omitempty"`
	Seq     uint64        `json:"seq"`
	Ports   []MachinePort `json:"ports"`
}

type MachinePortsList struct {
	Machines []MachinePorts `json:"machines"`
}

func (c *Client) MachinePorts(ctx context.Context) (*MachinePortsList, error) {
	var r MachinePortsList
	return &r, c.conn.Call(ctx, MethodMachinePorts, Empty{}, &r)
}

func (c *Client) OnMachinePorts(deliver func(MachinePorts)) {
	c.mu.Lock()
	c.ports = deliver
	c.mu.Unlock()
}

const (
	MethodEnginePorts = "engine.ports"
	NotifyEnginePorts = "engine.ports_changed"
)

type EnginePorts struct {
	Address string        `json:"address"`
	Seq     uint64        `json:"seq"`
	Ports   []MachinePort `json:"ports"`
}

func (c *Client) EnginePorts(ctx context.Context) (*EnginePorts, error) {
	var r EnginePorts
	return &r, c.conn.Call(ctx, MethodEnginePorts, Empty{}, &r)
}

func (c *Client) OnEnginePorts(deliver func(EnginePorts)) {
	c.mu.Lock()
	c.enginePorts = deliver
	c.mu.Unlock()
}
