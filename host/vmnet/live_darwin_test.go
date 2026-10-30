//go:build darwin && cgo

package vmnet

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/checksum"
)

func liveBase(t *testing.T) (netip.Prefix, Config) {
	if os.Getenv("FEV_VMNET_LIVE") != "1" {
		t.Skip("FEV_VMNET_LIVE=1 in an entitled test binary runs the real interface")
	}
	b := netip.MustParsePrefix("10.183.0.0/16")
	if v := os.Getenv("FEV_VMNET_BASE"); v != "" {
		b = netip.MustParsePrefix(v)
	}
	if b.Overlaps(netip.MustParsePrefix("198.19.0.0/16")) {
		t.Fatal("never 198.19.0.0/16 in a test")
	}
	a := b.Addr().As4()
	at := func(c, d byte) netip.Addr { return netip.AddrFrom4([4]byte{a[0], a[1], c, d}) }
	cfg := Config{Base: b, Mac: at(3, 1), Peer: at(3, 2), Prefixes: []netip.Prefix{
		netip.PrefixFrom(at(2, 0), 24), netip.PrefixFrom(at(4, 0), 22), netip.PrefixFrom(at(64, 0), 18)}}
	return b, cfg
}

func echo(f *os.File) {
	buf := make([]byte, 70000)
	for {
		n, err := f.Read(buf)
		if err != nil {
			return
		}
		p := buf[4:n]
		if len(p) < 28 || p[0]>>4 != 4 || p[9] != 1 {
			continue
		}
		ihl := int(p[0]&15) * 4
		if p[ihl] != 8 {
			continue
		}
		var s, d [4]byte
		copy(s[:], p[12:16])
		copy(d[:], p[16:20])
		copy(p[12:16], d[:])
		copy(p[16:20], s[:])
		p[10], p[11] = 0, 0
		binary.BigEndian.PutUint16(p[10:], ^checksum.Checksum(p[:ihl], 0))
		ic := p[ihl:]
		ic[0] = 0
		ic[2], ic[3] = 0, 0
		binary.BigEndian.PutUint16(ic[2:], ^checksum.Checksum(ic, 0))
		f.Write(buf[:n])
	}
}

func sh(name string, args ...string) string {
	out, _ := exec.Command(name, args...).CombinedOutput()
	return string(out)
}

func bridgeGone(name string, within time.Duration) (time.Duration, bool) {
	start := time.Now()
	for time.Since(start) < within {
		if !strings.Contains(" "+strings.TrimSpace(sh("/sbin/ifconfig", "-l"))+" ", " "+name+" ") {
			return time.Since(start), true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return within, false
}

func TestLiveTheMacReachesFevomsAddressesThroughTheBridge(t *testing.T) {
	base, cfg := liveBase(t)
	a := cfg.Peer.As4()
	target := netip.AddrFrom4([4]byte{a[0], a[1], 2, 7})
	named := netip.AddrFrom4([4]byte{a[0], a[1], 4, 9})
	t0 := time.Now()
	i, err := Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("started in %v: bridge %s, MTU %d, our link address %s", time.Since(t0).Round(time.Millisecond), i.Name(), i.MTU(), i.MAC())
	if i.Name() == "" || i.MTU() != DefaultMTU {
		i.Close()
		t.Fatalf("bridge %q MTU %d", i.Name(), i.MTU())
	}
	ifc := sh("/sbin/ifconfig", i.Name())
	t.Logf("ifconfig %s:\n%s", i.Name(), ifc)
	if !strings.Contains(ifc, "inet "+cfg.Mac.String()+" netmask 0xffff0000") || !strings.Contains(ifc, fmt.Sprintf("mtu %d", DefaultMTU)) {
		i.Close()
		t.Fatal("the bridge has not the Mac's address, the /16 and the MTU")
	}
	tun := i.Tunnel()
	go echo(tun)
	route := sh("/sbin/route", "-n", "get", target.String())
	t.Logf("route -n get %s:\n%s", target, route)
	if !strings.Contains(route, "interface: "+i.Name()) {
		t.Errorf("%s is not routed to %s", target, i.Name())
	}
	ping := sh("/sbin/ping", "-c", "3", "-t", "5", target.String())
	t.Logf("%s", ping)
	if !strings.Contains(ping, "3 packets received") {
		t.Errorf("ping %s: not 3 of 3", target)
	}
	name := fmt.Sprintf("bvlive-%d.local", os.Getpid())
	i.SetZones([]string{name})
	i.ServeMDNS(&Answerer{Lookup: func(n string) (netip.Addr, bool) {
		if n == name || strings.HasSuffix(n, "."+name) {
			return named, true
		}
		return netip.Addr{}, false
	}})
	got := ""
	for start := time.Now(); time.Since(start) < 20*time.Second; {
		ls := time.Now()
		out := sh("/usr/bin/dscacheutil", "-q", "host", "-a", "name", name)
		if strings.Contains(out, "ip_address: "+named.String()) {
			got = fmt.Sprintf("%s answered in %v, %v after the start", name, time.Since(ls).Round(time.Millisecond), time.Since(t0).Round(time.Millisecond))
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if got == "" {
		t.Errorf("%s not resolved by the Mac in 20 s", name)
	} else {
		t.Log(got)
		for k := range 3 {
			n := fmt.Sprintf("x%d.%s", k, name)
			ls := time.Now()
			out := sh("/usr/bin/dscacheutil", "-q", "host", "-a", "name", n)
			t.Logf("%s: %v (%s)", n, time.Since(ls).Round(time.Millisecond), strings.TrimSpace(strings.ReplaceAll(out, "\n", " ")))
		}
	}
	st := i.Stats()
	t.Logf("shim: %v", st)
	if st.ARPAnswered == 0 || st.ToTunnel < 3 || st.MDNSUp == 0 || st.MDNSDown == 0 {
		t.Errorf("stats %v", st)
	}
	bridge := i.Name()
	i.Close()
	tun.Close()
	d, ok := bridgeGone(bridge, 5*time.Second)
	t.Logf("closed: %s gone %v after (%v)", bridge, d.Round(time.Millisecond), ok)
	if !ok {
		t.Fatalf("%s still there 5 s after Close", bridge)
	}
	for _, l := range strings.Split(sh("/usr/sbin/netstat", "-rn", "-f", "inet"), "\n") {
		if strings.Contains(l, bridge) {
			t.Errorf("a route of the bridge left: %s", l)
		}
	}
	_ = base
	t1 := time.Now()
	j, err := Start(cfg)
	if err != nil {
		t.Fatalf("again: %v", err)
	}
	t.Logf("started again at once in %v: %s", time.Since(t1).Round(time.Millisecond), j.Name())
	j.Close()
}

func TestLiveAKilledHolderLeavesNothing(t *testing.T) {
	if os.Getenv("FEV_VMNET_CHILD") == "1" {
		_, cfg := liveBase(t)
		i, err := Start(cfg)
		if err != nil {
			fmt.Println("START FAILED", err)
			os.Exit(1)
		}
		fmt.Println("READY", i.Name())
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	_, cfg := liveBase(t)
	cmd := exec.Command(os.Args[0], "-test.run", "^TestLiveAKilledHolderLeavesNothing$")
	cmd.Env = append(os.Environ(), "FEV_VMNET_CHILD=1")
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, bridge := "", ""
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line = sc.Text()
		if strings.HasPrefix(line, "READY ") {
			bridge = strings.TrimPrefix(line, "READY ")
			break
		}
		if strings.HasPrefix(line, "START FAILED") {
			break
		}
	}
	if bridge == "" {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("the child did not start: %s", line)
	}
	t.Logf("child %d holds %s: %s", cmd.Process.Pid, bridge, strings.Fields(sh("/sbin/ifconfig", bridge))[0])
	k := time.Now()
	cmd.Process.Signal(syscall.SIGKILL)
	cmd.Wait()
	d, ok := bridgeGone(bridge, 5*time.Second)
	t.Logf("SIGKILL: %s gone %v after the kill (%v)", bridge, d.Round(time.Millisecond), ok)
	if !ok {
		t.Fatalf("%s left 5 s after the kill", bridge)
	}
	t1 := time.Now()
	j, err := Start(cfg)
	if err != nil {
		t.Fatalf("the base again after the kill (%v after it): %v", time.Since(k).Round(time.Millisecond), err)
	}
	t.Logf("the base started again %v after the kill, in %v: %s", time.Since(k).Round(time.Millisecond), time.Since(t1).Round(time.Millisecond), j.Name())
	j.Close()
}
