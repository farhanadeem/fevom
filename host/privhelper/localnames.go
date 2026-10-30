package privhelper

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	api "fev/proto/privhelper"
)

func (s *Server) localNamesSet(ctx context.Context, p api.LocalNamesParams) (api.LocalNames, error) {
	if len(p.Names) > api.MaxLocalNames {
		return api.LocalNames{}, api.Refusal{Arg: "names", Value: fmt.Sprint(len(p.Names)), Reason: fmt.Sprintf("more than %d names", api.MaxLocalNames)}
	}
	own, err := s.sys.LocalHostName()
	if err != nil {
		return api.LocalNames{}, fmt.Errorf("reading the Mac's own name: %w", err)
	}
	if own != "" {
		own += ".local"
	}
	var wanted []string
	for i, n := range p.Names {
		c, err := api.CanonicalLocalName(n, own)
		if err != nil {
			r, _ := err.(api.Refusal)
			r.Arg = fmt.Sprintf("names[%d]", i)
			return api.LocalNames{}, r
		}
		if !slices.Contains(wanted, c) {
			wanted = append(wanted, c)
		}
	}
	lock, err := s.lockNames()
	if err != nil {
		return api.LocalNames{}, err
	}
	defer lock.Close()
	dir := s.path(ResolverDir)
	present, err := s.customEntries()
	if err != nil {
		return api.LocalNames{}, err
	}
	var adding, keeping []string
	for _, n := range wanted {
		if _, ok := present[n]; ok {
			keeping = append(keeping, n)
		} else {
			adding = append(adding, n)
		}
	}
	if len(adding) > 0 {
		newest := lock.added
		for _, t := range present {
			if t.After(newest) {
				newest = t
			}
		}
		if wait := time.Duration(api.LocalNamesInterval)*time.Millisecond - s.now().Sub(newest); wait > 0 {
			return api.LocalNames{}, &api.Error{Code: api.CodeTooSoon, Message: fmt.Sprintf("names were added %s ago; adding more waits %s", s.now().Sub(newest).Round(time.Millisecond), wait.Round(time.Millisecond))}
		}
		if err := lock.stamp(s.now()); err != nil {
			return api.LocalNames{}, err
		}
	}

	results := map[string]api.LocalName{}
	var removing []string
	for n := range present {
		if !slices.Contains(wanted, n) {
			removing = append(removing, n)
			results[n] = api.LocalName{Name: n, State: api.NameRemoved}
		}
	}

	var recheck []string
	if p.NetworkChanged {
		recheck = keeping
	} else {
		for _, n := range keeping {
			results[n] = api.LocalName{Name: n, State: api.NamePresent}
		}
	}
	if len(adding)+len(recheck) > 0 {
		store, err := s.sys.DNSResolvers()
		if err != nil {
			return api.LocalNames{}, fmt.Errorf("reading the Mac's DNS configuration: %w", err)
		}
		files, err := foreignResolvers(dir)
		if err != nil {
			return api.LocalNames{}, fmt.Errorf("reading %s: %w", dir, err)
		}
		resolvers := append(foreignStore(store), files...)
		var probeNew, probeAgain []string
		for _, n := range adding {
			if why := configured(resolvers, n); why != "" {
				results[n] = api.LocalName{Name: n, State: api.NameRefused, Reason: why}
			} else {
				probeNew = append(probeNew, n)
			}
		}
		for _, n := range recheck {
			if why := configured(resolvers, n); why != "" {
				results[n] = api.LocalName{Name: n, State: api.NameRemoved, Reason: why}
				removing = append(removing, n)
			} else {
				probeAgain = append(probeAgain, n)
			}
		}
		newAnswers, againAnswers, err := s.probe(ctx, probeNew, probeAgain)
		if err != nil {
			return api.LocalNames{}, err
		}
		for _, n := range probeNew {
			if a, ok := s.foreignAnswer(newAnswers[n]); ok {
				results[n] = api.LocalName{Name: n, State: api.NameRefused, Reason: answerReason(a), AnsweredBy: a.Addr}
				continue
			}
			if err := s.writeCustom(n); err != nil {
				return api.LocalNames{}, fmt.Errorf("writing the entry for %s: %w", n, err)
			}
			results[n] = api.LocalName{Name: n, State: api.NamePresent}
		}
		for _, n := range probeAgain {
			if a, ok := s.foreignAnswer(againAnswers[n]); ok {
				results[n] = api.LocalName{Name: n, State: api.NameRemoved, Reason: answerReason(a), AnsweredBy: a.Addr}
				removing = append(removing, n)
				continue
			}
			results[n] = api.LocalName{Name: n, State: api.NamePresent}
		}
	}
	for _, n := range removing {
		if err := s.removeCustom(n); err != nil {
			return api.LocalNames{}, fmt.Errorf("removing the entry for %s: %w", n, err)
		}
	}
	if err := s.ensureOwn(); err != nil {
		s.logf("fev.local's entry: %v", err)
	}
	out := api.LocalNames{Names: []api.LocalName{}}
	for _, r := range results {
		out.Names = append(out.Names, r)
	}
	sort.Slice(out.Names, func(i, j int) bool { return out.Names[i].Name < out.Names[j].Name })
	return out, nil
}

func (s *Server) probe(ctx context.Context, fresh, again []string) (map[string][]Answer, map[string][]Answer, error) {
	d := time.Duration(api.ProbeMS) * time.Millisecond
	var a, b map[string][]Answer
	var errA, errB error
	var wg sync.WaitGroup
	if len(fresh) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, errA = s.sys.Probe(ctx, fresh, []ProbeKind{ProbeSystem, ProbeZone, ProbeMulticast}, d)
		}()
	}
	if len(again) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, errB = s.sys.Probe(ctx, again, []ProbeKind{ProbeMulticast}, d)
		}()
	}
	wg.Wait()
	if err := errors.Join(errA, errB, ctx.Err()); err != nil {
		return nil, nil, fmt.Errorf("the probe: %w", err)
	}
	return a, b, nil
}

func (s *Server) foreignAnswer(as []Answer) (Answer, bool) {
	tunnels := map[string]bool{}
	if ifaces, err := s.sys.Interfaces(); err == nil {
		for _, i := range ifaces {
			if i.Has(s.plan.TunnelMac()) {
				tunnels[i.Name] = true
			}
		}
	}
	for _, a := range as {
		if a.Ifname != "" && tunnels[a.Ifname] {
			continue
		}
		if ad, err := netip.ParseAddr(a.Addr); err == nil && inside(s.bases, netip.PrefixFrom(ad, ad.BitLen())) {
			continue
		}
		return a, true
	}
	return Answer{}, false
}

func answerReason(a Answer) string {
	on := ""
	if a.Ifname != "" {
		on = " on " + a.Ifname
	}
	switch a.Kind {
	case ProbeZone:
		return fmt.Sprintf("a zone of the Mac's resolvers claims it (%s%s)", a.Type, on)
	case ProbeMulticast:
		return fmt.Sprintf("answered by mDNS%s", on)
	}
	return fmt.Sprintf("answered by the Mac's resolvers%s", on)
}

func configured(resolvers []Resolver, name string) string {
	for _, r := range resolvers {
		for _, list := range [][]string{r.Domains, r.Search} {
			for _, d := range list {
				d = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
				if d == "" || d == "local" || !api.Overlaps(name, d) {
					continue
				}
				rel := "the same name"
				switch {
				case strings.HasSuffix(name, "."+d):
					rel = "a domain above it"
				case strings.HasSuffix(d, "."+name):
					rel = "a domain below it"
				}
				return fmt.Sprintf("%s's resolver (%s) is configured for %s", d, r.Source, rel)
			}
		}
	}
	return ""
}
