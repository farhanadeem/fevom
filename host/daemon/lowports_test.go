package daemon

import (
	"context"
	"errors"
	"net"
	"slices"
	"testing"

	api "fev/proto/daemon"
)

func TestALowPortIsAskedOfTheHelperUnlessAdminFeaturesAreOff(t *testing.T) {
	f, n, h, _, _ := direct(t, "")
	for _, family := range []int{4, 6} {
		file, err := n.BindLow("tcp", family, 80)
		if err != nil {
			t.Fatalf("family %d: %v", family, err)
		}
		l, err := net.FileListener(file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		at := l.Addr().(*net.TCPAddr)
		l.Close()
		if !at.IP.IsLoopback() {
			t.Errorf("family %d: bound at %v, not loopback", family, at)
		}
	}
	if _, err := n.BindLow("udp", 4, 1024); err == nil {
		t.Error("port 1024 was bound through the helper")
	}
	if got, want := h.Bound(), []string{"tcp4:80", "tcp6:80"}; !slices.Equal(got, want) {
		t.Errorf("the helper was asked %v, want %v", got, want)
	}

	if _, err := f.client().ConfigSet(context.Background(), map[string]any{api.KeyAdminEnabled: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.BindLow("tcp", 4, 80); !errors.Is(err, errAdminOff) {
		t.Errorf("with admin features off: %v", err)
	}
	if got := h.Bound(); len(got) != 2 {
		t.Errorf("with admin features off the helper was asked: %v", got)
	}
}
