package nfsview

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == GuardArg {
		os.Exit(GuardMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func TestTheGuardRemovesTheViewTheDaemonLeft(t *testing.T) {
	v := newView(t)
	v.mount(t, true)
	stop, err := StartGuard([]string{os.Args[0]}, v.dir, Source(v.server.Endpoint()))
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if _, ok, _ := Find(v.dir); ok {
		t.Fatal("the view is still mounted after the daemon's end")
	}
	checkStopped(t, v.dir)

	v.mount(t, true)
	stop, err = StartGuard([]string{os.Args[0]}, v.dir, Source(Endpoint{Socket: "/tmp/bv.other/view.sock"}))
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if _, ok, _ := Find(v.dir); !ok {
		t.Fatal("the guard removed a view that was not its daemon's")
	}
	ForceUnmount(v.dir)
	stop, err = StartGuard([]string{os.Args[0]}, v.dir, Source(v.server.Endpoint()))
	if err != nil {
		t.Fatal(err)
	}
	stop()
}
