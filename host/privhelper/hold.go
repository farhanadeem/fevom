package privhelper

import (
	"errors"
	"fmt"
	"net/netip"
	"syscall"

	api "fev/proto/privhelper"
)

const heldInterface = "lo0"

func recordedBases(r api.InstallRecord) []netip.Prefix {
	var out []netip.Prefix
	for _, b := range []string{r.Base, r.Base6} {
		if p, err := netip.ParsePrefix(b); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out
}

func recordedHeld(r api.InstallRecord) []api.HeldRoute { return api.HeldOf(r) }

func heldPrefix(h api.HeldRoute) (netip.Prefix, bool) {
	p, err := netip.ParsePrefix(h.Prefix)
	if err != nil || p.Masked() != p {
		return netip.Prefix{}, false
	}
	return p, true
}

func IsHeld(rt Route, h api.HeldRoute, ifIndex func(string) (int, bool)) bool {
	p, ok := heldPrefix(h)
	if !ok || rt.Prefix != p {
		return false
	}
	idx, ok := ifIndex(h.Interface)
	if !ok || rt.Index != idx || rt.Gateway != fmt.Sprintf("link#%d", idx) {
		return false
	}
	return rt.Flags&api.HeldFlagsMask == h.Flags&api.HeldFlagsMask
}

func indexOf(sys System) func(string) (int, bool) {
	ifs, err := sys.Interfaces()
	return func(name string) (int, bool) {
		if err != nil {
			return 0, false
		}
		for _, i := range ifs {
			if i.Name == name {
				return i.Index, true
			}
		}
		return 0, false
	}
}

func holdBases(sys System, held []api.HeldRoute) error {
	if sys == nil || len(held) == 0 {
		return nil
	}
	table, err := sys.Routes()
	if err != nil {
		return err
	}
	var errs []error
	for _, h := range held {
		b, ok := heldPrefix(h)
		if !ok || h.Interface != heldInterface {
			continue
		}
		have := false
		for _, r := range table {
			if r.Prefix == b && r.Flags&RTFIfScope == 0 {
				have = true
				break
			}
		}
		if have {
			continue
		}
		if err := sys.AddRejectRoute(b); err != nil && !errors.Is(err, syscall.EEXIST) {
			errs = append(errs, fmt.Errorf("the reject route for %s: %w", b, err))
		}
	}
	return errors.Join(errs...)
}

func releaseBases(sys System, held []api.HeldRoute) (removed []netip.Prefix, left []string, err error) {
	if sys == nil || len(held) == 0 {
		return nil, nil, nil
	}
	table, err := sys.Routes()
	if err != nil {
		return nil, nil, err
	}
	ifIndex := indexOf(sys)
	var errs []error
	for _, h := range held {
		b, ok := heldPrefix(h)
		if !ok {
			continue
		}
		for _, r := range table {
			if r.Prefix != b || r.Flags&RTFIfScope != 0 {
				continue
			}
			if !IsHeld(r, h, ifIndex) || h.Interface != heldInterface {
				left = append(left, fmt.Sprintf("a route for %s that is not the one fev made (gateway %s, flags %#x): another program's", b, orNoGateway(r.Gateway), r.Flags))
				break
			}
			if err := sys.DeleteRejectRoute(b); err != nil && !errors.Is(err, syscall.ESRCH) {
				errs = append(errs, fmt.Errorf("the reject route for %s: %w", b, err))
			} else {
				removed = append(removed, b)
			}
			break
		}
	}
	return removed, left, errors.Join(errs...)
}

func orNoGateway(g string) string {
	if g == "" {
		return "none"
	}
	return g
}

func sysFor(root string, given System) System {
	if given != nil {
		return given
	}
	if root == "" {
		return Darwin{}
	}
	return nil
}
