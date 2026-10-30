package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"

	api "fev/proto/daemon"
	"fev/proto/netplan"
)

type NoAdminPath interface {
	Open(plan netplan.Plan, log *slog.Logger, stopped func()) (f *os.File, iface string, mtu int, err error)
	SetPrefixes(ps []netip.Prefix)
	Close()
}

var ErrHelperHeld = errors.New("the admin helper's reserved route holds fev's addresses")

func (n *Network) noAdminFor(reason string) bool {
	return n.NoAdmin != nil && reason == api.DirectAdminOff && n.cfg.Bool(api.KeyNetworkDirectAccess)
}

func (n *Network) startOpen() {
	n.mu.Lock()
	need := n.tun == nil && !n.opening && !n.closed
	if need {
		n.opening = true
	}
	n.mu.Unlock()
	if need {
		go n.open()
	}
}

func (n *Network) openNoAdmin(why string) {
	log := n.host.Log()
	n.mu.Lock()
	if n.closed || (n.tun != nil && n.via == api.ViaVmnet) {
		if n.tun != nil {
			n.noHelper = why
		}
		n.mu.Unlock()
		n.publish()
		return
	}
	n.mu.Unlock()
	plan := planInEffect(n.cfg)
	f, iface, mtu, err := n.NoAdmin.Open(plan, log, n.noAdminStopped)
	if err != nil {
		reason, failure := api.DirectFailed, "direct access with no admin helper did not start: "+err.Error()
		if errors.Is(err, ErrHelperHeld) {
			n.mu.Lock()
			helper := n.failure
			n.mu.Unlock()
			reason, failure = api.DirectHelperHeld, heldFailure(why, plan.Base(), helper)
		}
		n.mu.Lock()
		n.reason, n.failure, n.noHelper = reason, failure, why
		n.mu.Unlock()
		log.Warn("direct access: the no-admin path did not open", "why", why, "err", err)
		n.publish()
		return
	}
	n.mu.Lock()
	if n.closed || !n.cfg.Bool(api.KeyNetworkDirectAccess) || n.tun != nil {
		n.mu.Unlock()
		f.Close()
		n.NoAdmin.Close()
		return
	}
	n.tun, n.iface, n.reason, n.base, n.via, n.noHelper = f, iface, "", plan.Base().String(), api.ViaVmnet, why
	if why != api.DirectFailed {
		n.failure = ""
	}
	n.mu.Unlock()
	if n.Tunnel != nil {
		n.Tunnel.Attach(f, TunnelEnds{Interface: iface, Mac: plan.TunnelMac(), Peer: plan.TunnelPeer(), MTU: mtu})
	}
	if n.Tunnel6 != nil {
		n.Tunnel6(false)
	}
	log.Info("direct access: the no-admin path is open", "interface", iface, "mtu", mtu, "no_helper", why)
	n.reroute("the no-admin path opened")
}

func heldFailure(why string, base netip.Prefix, helper string) string {
	held := fmt.Sprintf("fev's admin helper holds fev's addresses (%s) while it is not in use, so the Mac cannot reach them", base)
	switch why {
	case api.DirectAdminOff:
		return held + `: run "fev config set admin.enabled true" to use it, or "sudo fev setup --admin --undo" to remove it`
	case api.DirectHelperOff:
		return held + `: switch it on (System Settings, General, Login Items) to use it, or run "sudo fev setup --admin --undo" to remove it`
	case api.DirectOutdated:
		return held + `: it is out of date: run "sudo fev setup --admin" to use it, or "sudo fev setup --admin --undo" to remove it`
	case api.DirectOtherUser:
		return held + `: it is another user's: "sudo fev setup --admin" installs it for you, "sudo fev setup --admin --undo" removes it`
	}
	if helper != "" {
		return helper + "; and its reserved route for fev's addresses (" + base.String() + ") keeps the path with no helper from them"
	}
	return held + `: run "sudo fev setup --admin" to install it again, or "sudo fev setup --admin --undo" to remove it`
}

func (n *Network) noAdminStopped() {
	n.mu.Lock()
	on := n.tun != nil && n.via == api.ViaVmnet
	n.mu.Unlock()
	if !on {
		return
	}
	n.host.Log().Warn("direct access: the no-admin path's interface stopped by itself")
	n.closeTunnel(api.DirectFailed)
	n.mu.Lock()
	n.failure = "vmnet stopped fev's network interface: direct access is back at the next start"
	n.mu.Unlock()
	n.publish()
}
