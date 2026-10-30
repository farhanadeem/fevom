package macnet

import (
	"net/netip"

	priv "fev/proto/privhelper"
)

func HeldIn(path string, owner int) []Held {
	rec, ok := priv.ReadRecord(path, owner)
	if !ok {
		return nil
	}
	var out []Held
	for _, h := range priv.HeldOf(rec) {
		p, err := netip.ParsePrefix(h.Prefix)
		if err != nil || h.Flags&0x8 == 0 || h.Flags&0x1000 != 0 {
			continue
		}
		out = append(out, Held{Prefix: p, Interface: h.Interface})
	}
	return out
}
