package daemon

import (
	"errors"
	"net/netip"
	"os"

	"fev/host/daemon/config"
	api "fev/proto/daemon"
	"fev/proto/netplan"
)

const WarnNoBase = "network.no_base"

type Plan struct {
	Taken       func() []netip.Prefix
	Fresh       func() bool
	Recorded6   func() string
	NoAdminPath bool
	Recorded4   func() string

	host Host
}

func (p *Plan) Name() string                   { return "address plan" }
func (p *Plan) Marks() []string                { return nil }
func (p *Plan) StateChanged(api.Status)        {}
func (p *Plan) ConfigChanged(api.ConfigChange) {}
func (p *Plan) Close()                         {}
func (p *Plan) Start(h Host) error             { p.host = h; return nil }
func (p *Plan) fresh() bool {
	if p.Fresh != nil {
		return p.Fresh()
	}
	_, err := os.Stat(p.host.Layout().DataImage())
	return errors.Is(err, os.ErrNotExist)
}

func (p *Plan) BeforeBoot() error {
	cfg := p.host.Config()
	values := map[string]any{}
	if cfg.String(api.KeyNetworkSubnet6) == "" {
		var p6 netip.Prefix
		if p.Recorded6 != nil {
			if rec, err := netplan.Parse6(p.Recorded6()); err == nil {
				p6 = rec.Prefix()
			}
		}
		if !p6.IsValid() {
			ula, err := netplan.NewULA(nil)
			if err != nil {
				return err
			}
			p6 = ula
		}
		values[api.KeyNetworkSubnet6] = p6.String()
	}
	if p.fresh() && cfg.IsDefault(api.KeyNetworkSubnet) && p.Taken != nil {
		taken := p.Taken()
		candidates := netplan.Candidates
		noAdmin := p.NoAdminPath && (!cfg.Bool(api.KeyAdminEnabled) || p.Recorded4 == nil || p.Recorded4() == "")
		if noAdmin {
			candidates = netplan.PrivateCandidates()
		}
		if plan, ok := netplan.Choose(candidates, taken); ok {
			p.host.Warn(WarnNoBase, "")
			switch {
			case noAdmin:
				p.host.Log().Info("address plan: no admin helper to use, so a private base is chosen for this data disk (the no-admin path's interface takes no other)",
					"chosen", plan.Base())
				values[api.KeyNetworkSubnet] = plan.Base().String()
			case plan.Base() != netplan.DefaultBase:
				p.host.Log().Info("address plan: the default meets a network of this Mac's; another base is chosen for this data disk",
					"default", netplan.DefaultBase, "chosen", plan.Base(), "overlapping", netplan.Overlapping(netplan.Default(), taken))
				values[api.KeyNetworkSubnet] = plan.Base().String()
			}
		} else {
			p.host.Warn(WarnNoBase, netplan.ErrNoBase.Error()+
				` ("fev config set `+api.KeyNetworkSubnet+` 10.X.0.0/16" with a /16 this Mac does not use, then "fev reset"`+
				`: containers and machines keep the addresses they are made with)`)
		}
	}
	if len(values) == 0 {
		return nil
	}
	_, err := setConfig(p.host, values)
	return err
}

func (p *Plan) BootWords() []string {
	cfg := p.host.Config()
	plan, err := netplan.Parse(cfg.String(api.KeyNetworkSubnet))
	if err != nil {
		plan = netplan.Default()
	}
	words := []string{plan.Param()}
	if p6, err := netplan.Parse6(cfg.String(api.KeyNetworkSubnet6)); err == nil {
		words = append(words, p6.Param())
	}
	return words
}

func PlanOf(cfg *config.Store) netplan.Plan {
	if plan, err := netplan.Parse(cfg.String(api.KeyNetworkSubnet)); err == nil {
		return plan
	}
	return netplan.Default()
}

func planInEffect(cfg *config.Store) netplan.Plan {
	if v, ok := cfg.InEffect(api.KeyNetworkSubnet); ok {
		if s, ok := v.(string); ok {
			if plan, err := netplan.Parse(s); err == nil {
				return plan
			}
		}
	}
	return PlanOf(cfg)
}

type configSetter interface {
	SetConfigStarting(values map[string]any) ([]api.ConfigChange, error)
}

func setConfig(h Host, values map[string]any) ([]api.ConfigChange, error) {
	if s, ok := h.(configSetter); ok {
		return s.SetConfigStarting(values)
	}
	return h.Config().SetStarting(values)
}
