package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"fev/host/k8s"
	api "fev/proto/daemon"
)

const k8sZone = "k8s.fev.local"

const traefikID = "kube-system/traefik"

var leaseCheck = 12 * time.Second

type watcher struct {
	log   *slog.Logger
	cs    kubernetes.Interface
	cfg   k8s.Config
	reach func(k8s.Reach)
	now   func() time.Time

	mu   sync.Mutex
	lan  bool
	kick chan struct{}

	synced  time.Time
	beat0   time.Time
	live    bool
	last    []byte
	asked   map[types.UID]string
	pending map[types.UID]netip.Addr
	listers listers
}

type listers struct {
	svc   func(labels.Selector) ([]*corev1.Service, error)
	ing   func(labels.Selector) ([]*netv1.Ingress, error)
	nodes func(labels.Selector) ([]*corev1.Node, error)
	pods  func(labels.Selector) ([]*corev1.Pod, error)
}

func newWatcher(log *slog.Logger, cs kubernetes.Interface, cfg k8s.Config, reach func(k8s.Reach)) *watcher {
	return &watcher{log: log, cs: cs, cfg: cfg, reach: reach, now: time.Now, lan: cfg.LAN,
		kick: make(chan struct{}, 1), asked: map[types.UID]string{}, pending: map[types.UID]netip.Addr{}}
}

func (w *watcher) poke() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

func (w *watcher) setLAN(lan bool) {
	w.mu.Lock()
	w.lan = lan
	w.mu.Unlock()
	w.poke()
}

func (w *watcher) start(ctx context.Context) error {
	lbs, err := netip.ParsePrefix(w.cfg.Ranges.LoadBalancers)
	if err != nil {
		return fmt.Errorf("the LoadBalancer range %q: %w", w.cfg.Ranges.LoadBalancers, err)
	}
	f := informers.NewSharedInformerFactory(w.cs, 0)
	fp := informers.NewSharedInformerFactoryWithOptions(w.cs, 0, informers.WithTweakListOptions(func(o *metav1.ListOptions) {
		o.FieldSelector = "status.phase=Pending"
	}))
	svcs := f.Core().V1().Services()
	ings := f.Networking().V1().Ingresses()
	nodes := f.Core().V1().Nodes()
	pods := fp.Core().V1().Pods()
	h := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { w.poke() },
		UpdateFunc: func(any, any) { w.poke() },
		DeleteFunc: func(any) { w.poke() },
	}
	for _, i := range []cache.SharedIndexInformer{svcs.Informer(), ings.Informer(), nodes.Informer(), pods.Informer()} {
		i.AddEventHandler(h)
	}
	f.Start(ctx.Done())
	fp.Start(ctx.Done())
	shut := func() { f.Shutdown(); fp.Shutdown() }
	for _, fac := range []informers.SharedInformerFactory{f, fp} {
		for typ, ok := range fac.WaitForCacheSync(ctx.Done()) {
			if !ok {
				shut()
				return fmt.Errorf("the %v cache did not sync", typ)
			}
		}
	}
	w.listers = listers{svc: svcs.Lister().List, ing: ings.Lister().List, nodes: nodes.Lister().List, pods: pods.Lister().List}
	w.synced = w.now()
	w.log.Info("k8s: watching the cluster")
	w.poke()
	go func() {
		defer shut()
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.kick:
			}
			w.recompute(ctx, lbs)
		}
	}()
	return nil
}

func (w *watcher) recompute(ctx context.Context, lbr netip.Prefix) {
	all, _ := w.listers.svc(labels.Everything())
	ings, _ := w.listers.ing(labels.Everything())
	nodes, _ := w.listers.nodes(labels.Everything())
	pods, _ := w.listers.pods(labels.Everything())
	w.mu.Lock()
	lan := w.lan
	w.mu.Unlock()
	sortByAge(all)
	lbOf := allocate(all, lbr, w.pending, func(s *corev1.Service, a netip.Addr) error {
		cp := s.DeepCopy()
		mode := corev1.LoadBalancerIPModeVIP
		cp.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: a.String(), IPMode: &mode}}
		_, err := w.cs.CoreV1().Services(s.Namespace).UpdateStatus(ctx, cp, metav1.UpdateOptions{})
		if err == nil {
			w.log.Info("k8s: a LoadBalancer's address", "service", s.Namespace+"/"+s.Name, "address", a)
		}
		return err
	}, w.log)
	r := reachOf(all, ings, lbOf, lan)
	r.Node, r.Version, r.Ready = w.nodeOf(ctx, nodes)
	pulls := w.pullsOf(pods)
	b, _ := json.Marshal(r)
	if len(pulls) == 0 && string(b) == string(w.last) {
		return
	}
	w.last = b
	r.Pulls = pulls
	w.reach(r)
}

func sortByAge(all []*corev1.Service) {
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].CreationTimestamp.Equal(&all[j].CreationTimestamp) {
			return all[i].CreationTimestamp.Before(&all[j].CreationTimestamp)
		}
		return all[i].Namespace+"/"+all[i].Name < all[j].Namespace+"/"+all[j].Name
	})
}

func reachOf(all []*corev1.Service, ings []*netv1.Ingress, lbOf map[string]netip.Addr, lan bool) k8s.Reach {
	short := map[string]string{}
	for _, s := range all {
		id := s.Namespace + "/" + s.Name
		if !lbOf[id].IsValid() {
			continue
		}
		if cur, ok := short[s.Name]; !ok || s.Namespace == "default" && !strings.HasPrefix(cur, "default/") {
			short[s.Name] = id
		}
	}
	byAddr := map[netip.Addr]*k8s.Target{}
	var order []netip.Addr
	var services []api.K8sService
	for _, s := range all {
		id := s.Namespace + "/" + s.Name
		ks := api.K8sService{Namespace: s.Namespace, Name: s.Name, Type: string(s.Spec.Type)}
		if s.Spec.ClusterIP != corev1.ClusterIPNone {
			ks.ClusterIP = s.Spec.ClusterIP
		}
		var nodeURLs []string
		firstTCP := 0
		for _, p := range s.Spec.Ports {
			ks.Ports = append(ks.Ports, api.K8sPort{Name: p.Name, Protocol: protoOf(p.Protocol), Port: int(p.Port), NodePort: int(p.NodePort)})
			if isTCP(p.Protocol) {
				if firstTCP == 0 {
					firstTCP = int(p.Port)
				}
				if p.NodePort != 0 {
					nodeURLs = append(nodeURLs, fmt.Sprintf("http://localhost:%d", p.NodePort))
				}
			}
		}
		var urls []string
		if a := lbOf[id]; a.IsValid() {
			ks.LoadBalancer = a.String()
			t := &k8s.Target{ID: id, Address: a.String(), Names: []string{s.Name + "." + s.Namespace + "." + k8sZone}}
			main := t.Names[0]
			if short[s.Name] == id {
				t.Names = append(t.Names, s.Name+"."+k8sZone)
				main = s.Name + "." + k8sZone
			}
			webPorts(s, t)
			byAddr[a] = t
			order = append(order, a)
			switch {
			case t.Own443:
				urls = append(urls, "http://"+main)
			case t.Redirect80:
				urls = append(urls, "https://"+main)
			}
		}
		urls = append(urls, nodeURLs...)
		if ks.ClusterIP != "" && firstTCP != 0 {
			urls = append(urls, fmt.Sprintf("http://%s:%d", ks.ClusterIP, firstTCP))
		}
		ks.URLs = urls
		services = append(services, ks)
	}
	traefik := lbOf[traefikID]
	var ingresses []api.K8sIngress
	sort.SliceStable(ings, func(i, j int) bool {
		return ings[i].Namespace+"/"+ings[i].Name < ings[j].Namespace+"/"+ings[j].Name
	})
	for _, in := range ings {
		ki := api.K8sIngress{Namespace: in.Namespace, Name: in.Name}
		at := ingressAddress(in, byAddr, traefik)
		var tg *k8s.Target
		if at.IsValid() {
			ki.Address = at.String()
			tg = byAddr[at]
		}
		for _, r := range in.Spec.Rules {
			h := strings.ToLower(strings.TrimSuffix(r.Host, "."))
			if h == "" || slices.Contains(ki.Hosts, h) {
				continue
			}
			ki.Hosts = append(ki.Hosts, h)
			if tg == nil {
				continue
			}
			if h == k8sZone || strings.HasSuffix(h, "."+k8sZone) {
				if !slices.Contains(tg.Names, h) {
					tg.Names = append(tg.Names, h)
				}
				if !strings.HasPrefix(h, "*.") {
					scheme := "https://"
					if tg.Own443 {
						scheme = "http://"
					}
					ki.URLs = append(ki.URLs, scheme+h)
				}
			} else if !slices.Contains(tg.Hosts, h) {
				tg.Hosts = append(tg.Hosts, h)
			}
		}
		ingresses = append(ingresses, ki)
	}
	forwards := []k8s.Forward{{Proto: "tcp", Port: 6443}}
	seen := map[string]bool{"tcp/6443": true}
	for _, s := range all {
		for _, p := range s.Spec.Ports {
			if p.NodePort == 0 {
				continue
			}
			proto := protoOf(p.Protocol)
			if proto != "tcp" && proto != "udp" {
				continue
			}
			key := fmt.Sprintf("%s/%d", proto, p.NodePort)
			if seen[key] {
				continue
			}
			seen[key] = true
			forwards = append(forwards, k8s.Forward{Proto: proto, Port: int(p.NodePort), LAN: lan})
		}
	}
	r := k8s.Reach{Watching: true, Services: services, Ingresses: ingresses, Forwards: forwards}
	for _, a := range order {
		t := byAddr[a]
		sort.Strings(t.Names)
		sort.Strings(t.Hosts)
		r.Targets = append(r.Targets, *t)
	}
	return r
}

func ingressAddress(in *netv1.Ingress, byAddr map[netip.Addr]*k8s.Target, traefik netip.Addr) netip.Addr {
	for _, li := range in.Status.LoadBalancer.Ingress {
		if a, err := netip.ParseAddr(li.IP); err == nil && byAddr[a] != nil {
			return a
		}
	}
	class := ""
	if in.Spec.IngressClassName != nil {
		class = *in.Spec.IngressClassName
	} else if c := in.Annotations["kubernetes.io/ingress.class"]; c != "" {
		class = c
	}
	if traefik.IsValid() && byAddr[traefik] != nil && (class == "" || class == "traefik") {
		return traefik
	}
	return netip.Addr{}
}

func isTCP(p corev1.Protocol) bool { return p == corev1.ProtocolTCP || p == "" }

func protoOf(p corev1.Protocol) string {
	if p == "" {
		return "tcp"
	}
	return strings.ToLower(string(p))
}

func webPorts(s *corev1.Service, t *k8s.Target) {
	var first, named, has80, has443 bool
	var firstPort, namedPort int32
	for _, p := range s.Spec.Ports {
		if !isTCP(p.Protocol) {
			continue
		}
		switch {
		case p.Port == 80:
			has80 = true
		case p.Port == 443:
			has443 = true
		default:
			if (p.Name == "http" || p.Name == "web") && !named {
				named, namedPort = true, p.Port
			}
			if !first {
				first, firstPort = true, p.Port
			}
		}
	}
	if !has80 && !has443 && !first {
		t.Own443 = true
		return
	}
	t.Redirect80 = true
	switch {
	case has80:
		t.HTTPPort = 80
	case named:
		t.HTTPPort = int(namedPort)
	case first:
		t.HTTPPort = int(firstPort)
	default:
		t.HTTPSPort = 443
	}
	switch {
	case t.ID == traefikID:
		t.Pass443 = has443
	case has443:
		t.Own443 = true
	}
}

func (w *watcher) nodeOf(ctx context.Context, nodes []*corev1.Node) (name, version string, ready bool) {
	if len(nodes) == 0 {
		return "", "", false
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	n := nodes[0]
	name, version = n.Name, n.Status.NodeInfo.KubeletVersion
	var cond *corev1.NodeCondition
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == corev1.NodeReady {
			cond = &n.Status.Conditions[i]
		}
	}
	if cond == nil {
		return name, version, false
	}
	beat := cond.LastHeartbeatTime.Time
	if w.beat0.IsZero() {
		w.beat0 = beat
		if !w.live && !beat.Before(w.synced.Add(-15*time.Second)) {
			w.live = true
		}
		if !w.live {
			go func() {
				select {
				case <-ctx.Done():
					return
				case <-time.After(leaseCheck):
				}
				w.poke()
			}()
		}
	} else if !beat.Equal(w.beat0) {
		w.live = true
	}
	if !w.live && w.now().Sub(w.synced) >= leaseCheck {
		w.live = w.leaseRenewed(ctx, n.Name)
	}
	return name, version, w.live && cond.Status == corev1.ConditionTrue
}

func (w *watcher) leaseRenewed(ctx context.Context, node string) bool {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	l, err := w.cs.CoordinationV1().Leases(corev1.NamespaceNodeLease).Get(cctx, node, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return renewedAfter(l, w.synced)
}

func renewedAfter(l *coordv1.Lease, t time.Time) bool {
	return l.Spec.RenewTime != nil && l.Spec.RenewTime.After(t)
}

func (w *watcher) pullsOf(pods []*corev1.Pod) []string {
	seen := map[types.UID]bool{}
	refs := map[string]bool{}
	for _, p := range pods {
		seen[p.UID] = true
		if w.asked[p.UID] == p.ResourceVersion {
			continue
		}
		waiting := map[string]bool{}
		any := false
		for _, cs := range append(append([]corev1.ContainerStatus(nil), p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
			any = true
			if cs.State.Waiting != nil && cs.State.Waiting.Reason == "ErrImageNeverPull" {
				waiting[cs.Name] = true
			}
		}
		_, before := w.asked[p.UID]
		asked := false
		for _, c := range append(append([]corev1.Container(nil), p.Spec.InitContainers...), p.Spec.Containers...) {
			if c.ImagePullPolicy != corev1.PullNever || c.Image == "" {
				continue
			}
			if waiting[c.Name] || (!any && !before) {
				refs[c.Image] = true
				asked = true
			}
		}
		if asked || before {
			w.asked[p.UID] = p.ResourceVersion
		}
	}
	for uid := range w.asked {
		if !seen[uid] {
			delete(w.asked, uid)
		}
	}
	out := make([]string, 0, len(refs))
	for r := range refs {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}
