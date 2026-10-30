package privhelper_test

import (
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"

	"fev/host/privhelper"
	"fev/host/privhelper/fakesys"
	api "fev/proto/privhelper"
)

func rejects(sys *fakesys.System) []string {
	tab, _ := sys.Routes()
	var out []string
	for _, r := range tab {
		if r.Flags&privhelper.RTFReject != 0 {
			out = append(out, r.Prefix.String()+"@"+map[bool]string{true: "lo0", false: "?"}[r.Index == 1])
		}
	}
	slices.Sort(out)
	return out
}

func TestTheInstallHoldsTheBaseOnTheMac(t *testing.T) {
	r := newInstallRig(t)
	sys := fakesys.New()
	o := r.opts()
	o.Sys = sys
	if _, err := privhelper.Install(o); err != nil {
		t.Fatal(err)
	}
	want := []string{"198.19.0.0/16@lo0", "fd5b:6e2a:91c0::/48@lo0"}
	if got := rejects(sys); !slices.Equal(got, want) {
		t.Errorf("after the install: %v, want %v", got, want)
	}
	if _, err := privhelper.Install(o); err != nil {
		t.Fatal(err)
	}
	if got := rejects(sys); !slices.Equal(got, want) {
		t.Errorf("after a second install: %v", got)
	}
	undo, err := privhelper.UndoWith(r.root, r.launchctl, sys)
	if err != nil {
		t.Fatal(err)
	}
	if got := rejects(sys); len(got) != 0 || !slices.Contains(undo.Removed, "the reject route for 198.19.0.0/16") {
		t.Errorf("after the uninstall: %v, %+v", got, undo)
	}

	r2 := newInstallRig(t)
	sys2 := fakesys.New()
	sys2.With(func(s *fakesys.System) {
		s.Table = append(s.Table, privhelper.Route{Prefix: netip.MustParsePrefix("198.19.0.0/16"), Index: 4, Flags: privhelper.RTFUp | privhelper.RTFStatic | privhelper.RTFGateway})
	})
	o2 := r2.opts()
	o2.Sys = sys2
	if _, err := privhelper.Install(o2); err != nil {
		t.Fatal(err)
	}
	if got := rejects(sys2); !slices.Equal(got, []string{"fd5b:6e2a:91c0::/48@lo0"}) {
		t.Errorf("beside another's route for the base: %v", got)
	}
	privhelper.UndoWith(r2.root, r2.launchctl, sys2)
	tab, _ := sys2.Routes()
	if !slices.ContainsFunc(tab, func(rt privhelper.Route) bool {
		return rt.Prefix.String() == "198.19.0.0/16" && rt.Index == 4
	}) {
		t.Error("the uninstall took another's route for the base")
	}
}

func TestTheBootPassAndRoutesSetHoldTheBase(t *testing.T) {
	r := newRig(t, nil)
	if err := r.srv.BootPass(); err != nil {
		t.Fatal(err)
	}
	if got := rejects(r.sys); !slices.Equal(got, []string{"198.19.0.0/16@lo0", "fd5b:6e2a:91c0::/48@lo0"}) {
		t.Errorf("after the boot pass: %v", got)
	}
	r.sys.With(func(s *fakesys.System) {
		s.Table = slices.DeleteFunc(s.Table, func(rt privhelper.Route) bool { return rt.Flags&privhelper.RTFReject != 0 })
	})
	tun := r.openTunnel()
	var res api.Routes
	if _, err := r.call(api.MethodRoutesSet, api.RoutesParams{Interface: tun, Prefixes: []string{"198.19.0.0/24", "198.19.64.0/18"}}, &res); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.routesOn(tun), " "); got != "198.19.0.0/24 198.19.64.0/18" {
		t.Errorf("the tunnel's routes: %s", got)
	}
	if got := rejects(r.sys); len(got) != 2 {
		t.Errorf("after routes.set: %v", got)
	}

	r2 := newRig(t, nil)
	r2.sys.With(func(s *fakesys.System) { s.Fail["reject.add"] = os.ErrPermission })
	tun2 := r2.openTunnel()
	if _, err := r2.call(api.MethodRoutesSet, api.RoutesParams{Interface: tun2, Prefixes: []string{"198.19.0.0/24"}}, &res); err != nil {
		t.Errorf("routes.set with the reject route failing: %v", err)
	}
}

func TestTheUninstallDeletesOnlyTheRejectRouteFevomMade(t *testing.T) {
	r := newInstallRig(t)
	sys := fakesys.New()
	o := r.opts()
	o.Sys = sys
	rep, err := privhelper.Install(o)
	if err != nil {
		t.Fatal(err)
	}
	want := []api.HeldRoute{
		{Prefix: "198.19.0.0/16", Interface: "lo0", Flags: privhelper.RTFUp | privhelper.RTFStatic | privhelper.RTFReject},
		{Prefix: "fd5b:6e2a:91c0::/48", Interface: "lo0", Flags: privhelper.RTFUp | privhelper.RTFStatic | privhelper.RTFReject},
	}
	if !slices.Equal(rep.Record.Held, want) {
		t.Errorf("the record's held routes: %+v, want %+v", rep.Record.Held, want)
	}
	sys.With(func(s *fakesys.System) {
		s.Table = slices.DeleteFunc(s.Table, func(rt privhelper.Route) bool { return rt.Flags&privhelper.RTFReject != 0 })
		s.Table = append(s.Table,
			privhelper.Route{Prefix: netip.MustParsePrefix("198.19.0.0/16"), Index: 1, Gateway: "127.0.0.1", Flags: privhelper.RTFUp | privhelper.RTFStatic | privhelper.RTFReject | privhelper.RTFGateway},
			privhelper.Route{Prefix: netip.MustParsePrefix("fd5b:6e2a:91c0::/48"), Index: 1, Gateway: "link#1", Flags: privhelper.RTFUp | privhelper.RTFStatic | privhelper.RTFBlackhole})
	})
	undo, err := privhelper.UndoWith(r.root, r.launchctl, sys)
	if err != nil {
		t.Fatal(err)
	}
	tab, _ := sys.Routes()
	for _, p := range []string{"198.19.0.0/16", "fd5b:6e2a:91c0::/48"} {
		if !slices.ContainsFunc(tab, func(rt privhelper.Route) bool { return rt.Prefix.String() == p }) {
			t.Errorf("the uninstall deleted another program's route for %s", p)
		}
		if !slices.ContainsFunc(undo.Left, func(s string) bool { return strings.Contains(s, p) && strings.Contains(s, "not the one fev made") }) {
			t.Errorf("another program's route for %s was not said: %+v", p, undo.Left)
		}
		if slices.Contains(undo.Removed, "the reject route for "+p) {
			t.Errorf("%s said removed: %+v", p, undo.Removed)
		}
	}

	r2 := newInstallRig(t)
	sys2 := fakesys.New()
	sys2.With(func(s *fakesys.System) {
		s.Table = append(s.Table, privhelper.Route{Prefix: netip.MustParsePrefix("198.19.0.0/16"), Index: 4, Gateway: "link#4", Flags: privhelper.RTFUp | privhelper.RTFStatic | privhelper.RTFReject})
	})
	o2 := r2.opts()
	o2.Sys = sys2
	if _, err := privhelper.Install(o2); err != nil {
		t.Fatal(err)
	}
	privhelper.UndoWith(r2.root, r2.launchctl, sys2)
	tab2, _ := sys2.Routes()
	if !slices.ContainsFunc(tab2, func(rt privhelper.Route) bool { return rt.Prefix.String() == "198.19.0.0/16" && rt.Index == 4 }) {
		t.Error("the uninstall deleted a reject route on en0 that was there before the install")
	}
	if slices.ContainsFunc(tab2, func(rt privhelper.Route) bool { return rt.Prefix.String() == "fd5b:6e2a:91c0::/48" }) {
		t.Error("fev's own route for the IPv6 base was left")
	}
}
