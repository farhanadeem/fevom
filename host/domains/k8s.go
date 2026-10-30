package domains

import (
	"net/netip"
	"sort"
	"strings"

	"github.com/miekg/dns"

	"fev/proto/agent"
)

const K8sZone = "k8s." + Zone

const ClusterZone = "cluster.local"

type K8sService struct {
	ID                          string
	Address                     netip.Addr
	Names                       []string
	HTTPPort, HTTPSPort         int
	Redirect80, Own443, Pass443 bool
	Hosts                       []string
}

func (r *Registry) SetK8s(svcs []K8sService) {
	r.mu.Lock()
	next := map[string]*target{}
	for _, s := range svcs {
		s := s
		t := &target{kind: agent.TargetK8s, id: s.ID, name: s.ID, addr: s.Address, k: &s}
		if old, ok := r.k8s[s.ID]; ok {
			t.order = old.order
		} else {
			r.order++
			t.order = r.order
		}
		next[s.ID] = t
	}
	changed := len(next) != len(r.k8s)
	for id, t := range next {
		old, ok := r.k8s[id]
		if !ok || !sameK8s(old.k, t.k) {
			changed = true
		}
	}
	if changed {
		r.k8s = next
		r.rebuild()
	}
	r.mu.Unlock()
	if changed {
		r.changed()
	}
}

func sameK8s(a, b *K8sService) bool {
	return a.ID == b.ID && a.Address == b.Address && strings.Join(a.Names, ",") == strings.Join(b.Names, ",") &&
		a.HTTPPort == b.HTTPPort && a.HTTPSPort == b.HTTPSPort && a.Redirect80 == b.Redirect80 &&
		a.Own443 == b.Own443 && a.Pass443 == b.Pass443 && strings.Join(a.Hosts, ",") == strings.Join(b.Hosts, ",")
}

func k8sWants(s *K8sService) []string {
	var out []string
	for _, n := range s.Names {
		n = canonical(n)
		if under(n, K8sZone) && n != K8sZone && checkName(n, true) == nil {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func (r *Registry) SetForward(zone string, fn func(*dns.Msg) (*dns.Msg, error)) {
	r.mu.Lock()
	if r.forward == nil {
		r.forward = map[string]func(*dns.Msg) (*dns.Msg, error){}
	}
	if fn == nil {
		delete(r.forward, zone)
	} else {
		r.forward[zone] = fn
	}
	r.mu.Unlock()
}

func (r *Registry) forwardOf(name string) func(*dns.Msg) (*dns.Msg, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for zone, fn := range r.forward {
		if under(name, zone) {
			return fn
		}
	}
	return nil
}

func (r *Registry) Forward(req *dns.Msg) (*dns.Msg, bool) {
	if req == nil || len(req.Question) != 1 {
		return nil, false
	}
	fn := r.forwardOf(canonical(req.Question[0].Name))
	if fn == nil {
		return nil, false
	}
	resp, err := fn(req)
	if err != nil || resp == nil {
		resp = new(dns.Msg)
		resp.SetRcode(req, dns.RcodeServerFailure)
	}
	resp.Id = req.Id
	return resp, true
}

func (r *Registry) SetExtraLocal(names []string) {
	r.mu.Lock()
	if strings.Join(r.extraLocal, ",") == strings.Join(names, ",") {
		r.mu.Unlock()
		return
	}
	r.extraLocal = append([]string(nil), names...)
	r.gen++
	r.mu.Unlock()
	r.changed()
}

func (r *Registry) ForwardA(name string) (addr netip.Addr, found, forwarded bool) {
	name = canonical(name)
	if r.forwardOf(name) == nil {
		return netip.Addr{}, false, false
	}
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(name), dns.TypeA)
	resp, ok := r.Forward(q)
	if !ok || resp.Rcode != dns.RcodeSuccess {
		return netip.Addr{}, false, true
	}
	for _, rr := range resp.Answer {
		if a, ok := rr.(*dns.A); ok {
			if ip, ok := netip.AddrFromSlice(a.A.To4()); ok {
				return ip, true, true
			}
		}
	}
	return netip.Addr{}, false, true
}
