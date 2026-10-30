package kube

import (
	"log/slog"
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func ours(s *corev1.Service) bool {
	return s.Spec.Type == corev1.ServiceTypeLoadBalancer && s.Spec.LoadBalancerClass == nil
}

func allocate(all []*corev1.Service, lbr netip.Prefix, pending map[types.UID]netip.Addr, set func(*corev1.Service, netip.Addr) error, log *slog.Logger) map[string]netip.Addr {
	used := map[netip.Addr]bool{}
	lbOf := map[string]netip.Addr{}
	alive := map[types.UID]bool{}
	for _, s := range all {
		if !ours(s) {
			continue
		}
		alive[s.UID] = true
		for _, in := range s.Status.LoadBalancer.Ingress {
			if a, err := netip.ParseAddr(in.IP); err == nil && lbr.Contains(a) && !used[a] {
				used[a] = true
				lbOf[s.Namespace+"/"+s.Name] = a
				delete(pending, s.UID)
				break
			}
		}
	}
	for uid := range pending {
		if !alive[uid] {
			delete(pending, uid)
		}
	}
	for _, s := range all {
		id := s.Namespace + "/" + s.Name
		if !ours(s) || lbOf[id].IsValid() {
			continue
		}
		if a, ok := pending[s.UID]; ok && !used[a] {
			used[a] = true
			lbOf[id] = a
		}
	}
	for _, s := range all {
		id := s.Namespace + "/" + s.Name
		if !ours(s) || lbOf[id].IsValid() {
			continue
		}
		var pick netip.Addr
		for a := lbr.Addr().Next(); lbr.Contains(a.Next()); a = a.Next() {
			if !used[a] {
				pick = a
				break
			}
		}
		if !pick.IsValid() {
			log.Warn("k8s: the LoadBalancer range is full", "service", id)
			continue
		}
		if err := set(s, pick); err != nil {
			log.Warn("k8s: a LoadBalancer's status", "service", id, "err", err)
			continue
		}
		used[pick] = true
		lbOf[id] = pick
		if s.UID != "" {
			pending[s.UID] = pick
		}
	}
	return lbOf
}
