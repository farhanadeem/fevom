package webproxy

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"testing"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"
)

func TestTheListerReadsAnotherNamespace(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ready := make(chan string)
	release := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			ready <- "error " + err.Error()
			return
		}
		lo := func(port int, addr [4]byte) {
			fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
			unix.Bind(fd, &unix.SockaddrInet4{Port: port, Addr: addr})
			unix.Listen(fd, 8)
		}
		lo(3000, [4]byte{})
		lo(2019, [4]byte{127, 0, 0, 1})
		ready <- fmt.Sprintf("/proc/%d/task/%d/ns/net", os.Getpid(), unix.Gettid())
		<-release
	}()
	path := <-ready
	defer close(release)
	if len(path) > 6 && path[:6] == "error " {
		t.Skip(path)
	}
	own, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	unix.Bind(own, &unix.SockaddrInet4{Port: 0})
	unix.Listen(own, 1)
	defer unix.Close(own)

	ns := &Namespaces{}
	ns.Apply(Control{Machines: map[string]string{"m": path}})
	ls, err := NSLister{NS: ns}.Listening(api.DomainTarget{Kind: api.TargetMachine, ID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range ls {
		got = append(got, fmt.Sprintf("%s:%d", l.Addr, l.Port))
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"0.0.0.0:3000", "127.0.0.1:2019"}) {
		t.Errorf("listed %q", got)
	}
	if c := candidates(ls); !slices.Equal(c, []int{3000}) {
		t.Errorf("candidates %v", c)
	}
	ns.Apply(Control{NetNSRoot: "/"})
	if p, _ := ns.Path(api.DomainTarget{Kind: api.TargetContainer, Name: "c", NetNS: path[1:]}); p != path {
		t.Errorf("container path %q", p)
	}
}
