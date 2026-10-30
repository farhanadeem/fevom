package domains

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
)

type addresses struct {
	first, last netip.Addr
	skip        netip.Addr
	path        string

	byKey    map[string]netip.Addr
	inUse    map[string]bool
	released map[string]uint64
	tick     uint64
	next     netip.Addr
}

type addressFile struct {
	Version int            `json:"version"`
	Names   []addressEntry `json:"names"`
}

type addressEntry struct {
	Key      string `json:"key"`
	Address  string `json:"address"`
	Released uint64 `json:"released,omitempty"`
}

func newAddresses(first, last, skip netip.Addr, path string) *addresses {
	a := &addresses{first: first, last: last, skip: skip, path: path, byKey: map[string]netip.Addr{}, inUse: map[string]bool{}, released: map[string]uint64{}, next: first}
	a.load()
	return a
}

func (a *addresses) load() {
	if a.path == "" {
		return
	}
	b, err := os.ReadFile(a.path)
	if err != nil {
		return
	}
	var f addressFile
	if json.Unmarshal(b, &f) != nil || f.Version != 1 {
		return
	}
	byAddr := map[netip.Addr]bool{}
	for _, e := range f.Names {
		ip, err := netip.ParseAddr(e.Address)
		if err != nil || !a.inRange(ip) || byAddr[ip] || e.Key == "" {
			continue
		}
		byAddr[ip] = true
		a.byKey[e.Key] = ip
		r := e.Released
		if r > a.tick {
			a.tick = r
		}
		a.released[e.Key] = r
		if ip.Compare(a.next) >= 0 {
			a.next = a.after(ip)
		}
	}
}

func (a *addresses) inRange(ip netip.Addr) bool {
	return ip.Compare(a.first) >= 0 && ip.Compare(a.last) <= 0 && ip != a.skip
}

func (a *addresses) after(ip netip.Addr) netip.Addr {
	n := ip.Next()
	if n == a.skip {
		n = n.Next()
	}
	return n
}

func (a *addresses) take(key string) (netip.Addr, bool) {
	if ip, ok := a.byKey[key]; ok {
		if !a.inUse[key] {
			a.inUse[key] = true
			delete(a.released, key)
			a.save()
		}
		return ip, true
	}
	if a.next.IsValid() && a.next.Compare(a.last) <= 0 {
		ip := a.next
		a.next = a.after(ip)
		a.byKey[key], a.inUse[key] = ip, true
		a.save()
		return ip, true
	}
	oldest, at := "", uint64(0)
	for k, r := range a.released {
		if oldest == "" || r < at || r == at && k < oldest {
			oldest, at = k, r
		}
	}
	if oldest == "" {
		return netip.Addr{}, false
	}
	ip := a.byKey[oldest]
	delete(a.byKey, oldest)
	delete(a.released, oldest)
	a.byKey[key], a.inUse[key] = ip, true
	a.save()
	return ip, true
}

func (a *addresses) release(key string) {
	if !a.inUse[key] {
		return
	}
	delete(a.inUse, key)
	a.tick++
	a.released[key] = a.tick
	a.save()
}

func (a *addresses) save() {
	if a.path == "" {
		return
	}
	f := addressFile{Version: 1}
	for k, ip := range a.byKey {
		e := addressEntry{Key: k, Address: ip.String()}
		if !a.inUse[k] {
			e.Released = a.released[k]
		}
		f.Names = append(f.Names, e)
	}
	sort.Slice(f.Names, func(i, j int) bool {
		if f.Names[i].Released != f.Names[j].Released {
			return f.Names[i].Released < f.Names[j].Released
		}
		return f.Names[i].Key < f.Names[j].Key
	})
	b, _ := json.MarshalIndent(f, "", "\t")
	tmp := a.path + ".new"
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return
	}
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return
	}
	os.Rename(tmp, a.path)
}

func (a *addresses) String() string {
	return fmt.Sprintf("%d names, %d in use", len(a.byKey), len(a.inUse))
}
