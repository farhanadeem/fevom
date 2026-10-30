package daemon

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"fev/host/daemon/vm"
	"fev/host/nfsview"
	"fev/proto/agent"
	"fev/proto/vmmctl"
)

type dirVM struct {
	dir, boot string
}

func (v dirVM) VsockPath(port uint32) string {
	return filepath.Join(v.dir, "vsock_"+strconv.FormatUint(uint64(port), 10)+".sock")
}
func (v dirVM) Agent() *agent.Client    { return nil }
func (v dirVM) BootID() string          { return v.boot }
func (v dirVM) Control() *vmmctl.Client { return nil }

func firstLine(t *testing.T, path string) <-chan string {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	got := make(chan string, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetReadDeadline(time.Now().Add(5 * time.Second))
				line, _ := bufio.NewReader(c).ReadString('\n')
				got <- line
			}()
		}
	}()
	return got
}

func TestTheViewsStreamsGoToNfsdsVsockPortWhenTheGuestHasOne(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "bv.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	vmx := dirVM{dir: dir, boot: "b2"}
	direct := firstLine(t, vmx.VsockPath(vm.PortView))
	relayed := firstLine(t, vmx.VsockPath(vm.PortRelay))
	v := &View{host: agentHost{vm: vmx}}
	o := viewOpener{v}

	open := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if c, err := o.Open(ctx, nfsview.Request); err == nil {
			c.Write([]byte("rpc\n"))
			c.Close()
		}
	}
	expect := func(what string, want <-chan string, line string) {
		t.Helper()
		select {
		case got := <-want:
			if got != line {
				t.Fatalf("%s: the first line was %q, want %q", what, got, line)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no connection", what)
		}
	}
	open()
	expect("no vsock port", relayed, "view nfs\n")
	v.vsockBoot, v.vsockPort = "b1", vm.PortView
	open()
	expect("another boot's port", relayed, "view nfs\n")
	v.vsockBoot = "b2"
	open()
	expect("this boot's port", direct, "rpc\n")
}
