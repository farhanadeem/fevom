package docker

import (
	"fmt"
	"strings"
	"testing"

	"fev/proto/agent"
)

func TestEngineWantsAreTheGuestsLoopbackForThisMacAlone(t *testing.T) {
	got := EngineWants(agent.EnginePorts{Address: "127.0.0.1", Seq: 3, Ports: []agent.MachinePort{
		{Proto: agent.PortTCP, Address: "0.0.0.0", Port: 8080},
		{Proto: agent.PortTCP, Address: "127.0.0.1", Port: 8080},
		{Proto: agent.PortTCP, Address: "::", Port: 9000},
		{Proto: agent.PortUDP, Address: "0.0.0.0", Port: 5353},
		{Proto: agent.PortTCP, Address: "0.0.0.0", Port: 0},
		{Proto: "sctp", Address: "0.0.0.0", Port: 7000},
		{Proto: agent.PortTCP, Address: "198.19.1.2", Port: 7001},
		{Proto: agent.PortTCP, Address: "::1", Port: 7002},
		{Proto: agent.PortTCP, Address: "::ffff:127.0.0.1", Port: 7003},
	}})
	var rows []string
	for _, w := range got {
		rows = append(rows, fmt.Sprintf("%s/%d->%s lan=%v", w.Proto, w.Port, w.Guest, w.LAN))
	}
	want := "udp/5353->127.0.0.1:5353 lan=false tcp/7003->127.0.0.1:7003 lan=false tcp/8080->127.0.0.1:8080 lan=false tcp/9000->127.0.0.1:9000 lan=false"
	if g := strings.Join(rows, " "); g != want {
		t.Fatalf("EngineWants:\n got %s\nwant %s", g, want)
	}
}
