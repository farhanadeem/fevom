package machines

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"fev/proto/netplan"

	"github.com/vishvananda/netlink"
)

type Plug struct {
	Name, UUID      string
	MAC, Address    string
	User            string
	Root, Run       string
	NetNS           int
	Isolated, Agent bool
	IDBase          uint32
	IsolateNetwork  bool
	Address6Changed func()
}

type Plugged struct {
	MAC, Address string
	Address6     func() string
	Leased       <-chan struct{}
	Unplug       func()
}

type Network interface {
	Plug(p Plug) (Plugged, error)
}

const (
	LocalZone       = "fev.local"
	resolvedConfDir = "systemd/resolved.conf.d"
)

var ResolverAddress = netplan.Default().MachinesGateway().String()

var resolvedPrograms = []string{"/usr/lib/systemd/systemd-resolved", "/lib/systemd/systemd-resolved"}

func ResolvedDropIn(resolver string) string {
	return "# fev: machines' names (NAME." + LocalZone + ") from the machines' resolver.\n" +
		"[Resolve]\nDNS=" + resolver + "\nDomains=~" + LocalZone + "\n"
}

func writeResolvedDropIn(root, run, resolver string) error {
	found := false
	for _, p := range resolvedPrograms {
		if _, err := resolveIn(root, p); err == nil {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	dir := filepath.Join(run, resolvedConfDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if resolver == "" {
		resolver = ResolverAddress
	}
	return os.WriteFile(filepath.Join(dir, DropInName), []byte(ResolvedDropIn(resolver)), 0o644)
}

func HostLink(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "bm" + hex.EncodeToString(sum[:5])
}

type plugVeth struct{}

func (plugVeth) Plug(p Plug) (Plugged, error) {
	unplug, err := Veth(p.Name, p.MAC, p.NetNS, nil)
	if err != nil {
		return Plugged{}, err
	}
	return Plugged{MAC: p.MAC, Unplug: unplug}, nil
}

func Veth(name, mac string, netns int, master netlink.Link) (func(), error) {
	host := HostLink(name)
	if old, err := netlink.LinkByName(host); err == nil {
		netlink.LinkDel(old)
	}
	attrs := netlink.LinkAttrs{Name: host}
	if master != nil {
		attrs.MasterIndex = master.Attrs().Index
	}
	veth := &netlink.Veth{
		LinkAttrs:     attrs,
		PeerName:      "eth0",
		PeerNamespace: netlink.NsFd(netns),
	}
	if mac != "" {
		hw, err := net.ParseMAC(mac)
		if err != nil {
			return nil, fmt.Errorf("the machine's MAC %q: %w", mac, err)
		}
		veth.PeerHardwareAddr = hw
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return nil, fmt.Errorf("veth for %s: %w", name, err)
	}
	link, err := netlink.LinkByName(host)
	if err != nil {
		return nil, err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		netlink.LinkDel(link)
		return nil, err
	}
	return func() {
		if l, err := netlink.LinkByName(host); err == nil {
			netlink.LinkDel(l)
		}
	}, nil
}
