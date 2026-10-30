package netstack

import (
	"net"
	"testing"
	"time"
)

type deadlineRecorder struct {
	net.PacketConn
	set []time.Time
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error {
	d.set = append(d.set, t)
	return nil
}

func TestTheDNSSocketSetsNoTimerWhileWaitingForAQuestion(t *testing.T) {
	rec := &deadlineRecorder{}
	c := quietPacketConn{rec}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	c.SetReadDeadline(time.Time{})
	if len(rec.set) != 0 {
		t.Fatalf("a future deadline reached the socket: %v", rec.set)
	}
	past := time.Unix(1, 0)
	c.SetReadDeadline(past)
	if len(rec.set) != 1 || !rec.set[0].Equal(past) {
		t.Fatalf("the unblocking deadline did not reach the socket: %v", rec.set)
	}
}
