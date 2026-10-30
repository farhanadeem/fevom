package privhelper

import (
	"slices"
	"strings"
	"time"

	api "fev/proto/privhelper"
)

const ownName = "fev.local"

func (s *Server) storeForm() bool { return s.rec.ResolverForm == api.ResolverFormStore }

func (s *Server) customEntries() (map[string]time.Time, error) {
	if !s.storeForm() {
		return entries(s.path(ResolverDir))
	}
	es, err := s.store.Entries()
	if err != nil {
		return nil, err
	}
	out := map[string]time.Time{}
	for n, e := range es {
		if n != ownName {
			out[n] = e.Written
		}
	}
	return out, nil
}

func (s *Server) writeCustom(name string) error {
	if !s.storeForm() {
		return writeEntry(s.path(ResolverDir), name, s.rec.ResolverPort)
	}
	return s.store.Set(name, s.rec.ResolverPort, s.now())
}

func (s *Server) removeCustom(name string) error {
	if !s.storeForm() {
		return removeEntry(s.path(ResolverDir + "/" + name))
	}
	return s.store.Remove(name)
}

func (s *Server) ownState() string {
	st := resolverState(s.path(api.ResolverPath))
	if !s.storeForm() || st == api.StateForeign {
		return st
	}
	if rs, err := s.sys.DNSResolvers(); err == nil {
		for _, r := range rs {
			if ownStoreKey(r.Source) {
				continue
			}
			for _, d := range r.Domains {
				if strings.EqualFold(strings.TrimSuffix(d, "."), ownName) {
					return api.StateForeign
				}
			}
		}
	}
	es, err := s.store.Entries()
	if err != nil {
		return api.StateAbsent
	}
	if e, ok := es[ownName]; ok && e.Port == s.rec.ResolverPort {
		return api.StateFev
	}
	return api.StateAbsent
}

func (s *Server) ensureOwn() error {
	if !s.storeForm() || s.rec.NoResolver {
		return nil
	}
	if s.ownState() != api.StateAbsent {
		return nil
	}
	return s.store.Set(ownName, s.rec.ResolverPort, s.now())
}

func foreignStore(rs []Resolver) []Resolver {
	own := map[string]bool{}
	for _, r := range rs {
		if ownStoreKey(r.Source) {
			for _, d := range r.Domains {
				own[strings.ToLower(strings.TrimSuffix(d, "."))] = true
			}
		}
	}
	notOwn := func(l []string) []string {
		return slices.DeleteFunc(slices.Clone(l), func(d string) bool {
			return own[strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))]
		})
	}
	out := rs[:0:0]
	for _, r := range rs {
		if ownStoreKey(r.Source) {
			continue
		}
		if strings.HasSuffix(r.Source, ":/Network/Global/DNS") {
			r.Domains, r.Search = notOwn(r.Domains), notOwn(r.Search)
		}
		out = append(out, r)
	}
	return out
}
