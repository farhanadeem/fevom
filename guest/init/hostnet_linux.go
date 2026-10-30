package main

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"fev/guest/internal/fw"
	"fev/guest/internal/hostlo"
	"fev/guest/internal/logbuf"
)

func (s *system) startHostNet() error {
	source := s.plan.HostLoSource()
	if err := s.hostNetPrepare(source); err != nil {
		s.log.Logf(logbuf.Warning, "init: host-network containers do not reach the Mac's loopback: %v", err)
		return err
	}
	p, err := hostlo.Attach(filepath.Join(cgroupRoot, "fev.system"), source)
	if err != nil {
		s.log.Logf(logbuf.Warning, "init: host-network containers do not reach the Mac's loopback: %v", err)
		return err
	}
	s.hostlo = p
	return nil
}

func (s *system) hostNetPrepare(source netip.Addr) error {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("lo: %w", err)
	}
	a := &netlink.Addr{IPNet: &net.IPNet{IP: net.IP(source.AsSlice()), Mask: net.CIDRMask(32, 32)}, Scope: unix.RT_SCOPE_HOST}
	if err := netlink.AddrReplace(lo, a); err != nil {
		return fmt.Errorf("add %s to lo: %w", a.IPNet, err)
	}
	uplink := s.direct.uplinkName()
	if uplink == "" {
		s.log.Logf(logbuf.Info, "init: no uplink: host-network containers reach only the guest's loopback")
		return nil
	}
	tool := fw.Tool{Root: engineImage, Reaper: s.reaper}
	if err := tool.Nft(hostlo.Rules(uplink, s.plan.Guest().Addr(), source, s.plan.Host())); err != nil {
		return fmt.Errorf("the late nat chain: %w", err)
	}
	key := "/proc/sys/net/ipv4/conf/" + uplink + "/route_localnet"
	if err := os.WriteFile(key, []byte("1"), 0); err != nil {
		return fmt.Errorf("set %s: %w", key, err)
	}
	return nil
}
