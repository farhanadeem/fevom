package standin

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"fev/host/k8s"
	api "fev/proto/daemon"
	"fev/proto/netplan"
)

type objKey struct{ kind, ns, name string }

type fakeObj struct {
	o     api.K8sObject
	doc   map[string]any
	owner string
}

type fakeWatch struct {
	p       api.K8sWatchParams
	deliver func(api.K8sChanged)
}

type fakeCluster struct {
	plan netplan.Plan

	mu        sync.Mutex
	objs      map[objKey]*fakeObj
	rv        int
	uid       int
	pods      int
	watches   map[int]*fakeWatch
	nextW     int
	reach     func(k8s.Reach)
	lan       bool
	on        func() bool
	startTime time.Duration
	started   bool
	readyTime time.Duration
	readyAt   time.Time
}

var fakeEpoch = time.Date(2026, 10, 23, 21, 0, 0, 0, time.UTC)

func newFakeCluster(plan netplan.Plan) *fakeCluster {
	f := &fakeCluster{plan: plan, watches: map[int]*fakeWatch{}}
	f.reset()
	return f
}

func (f *fakeCluster) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objs = map[objKey]*fakeObj{}
	for _, ns := range []string{"default", "kube-system"} {
		f.put(&fakeObj{o: api.K8sObject{Kind: api.K8sKindNamespace, Name: ns, Phase: "Active"}})
	}
	f.put(&fakeObj{o: api.K8sObject{Kind: api.K8sKindService, Namespace: "default", Name: "kubernetes",
		Service: &api.K8sService{Namespace: "default", Name: "kubernetes", Type: "ClusterIP", ClusterIP: f.svcAddr(1), Ports: []api.K8sPort{{Name: "https", Protocol: "TCP", Port: 443}}}}})
	f.put(&fakeObj{o: api.K8sObject{Kind: api.K8sKindService, Namespace: "kube-system", Name: "kube-dns",
		Service: &api.K8sService{Namespace: "kube-system", Name: "kube-dns", Type: "ClusterIP", ClusterIP: f.plan.KubeDNS().String(),
			Ports: []api.K8sPort{{Name: "dns", Protocol: "UDP", Port: 53}, {Name: "dns-tcp", Protocol: "TCP", Port: 53}}}}})
	f.deployment("kube-system", "coredns", "rancher/mirrored-coredns-coredns:1.12.3", 1)
	f.deployment("kube-system", "traefik", "rancher/mirrored-library-traefik:3.5.1", 1)
	f.service("kube-system", "traefik", "LoadBalancer", []api.K8sPort{{Name: "web", Protocol: "TCP", Port: 80, NodePort: 31080}, {Name: "websecure", Protocol: "TCP", Port: 443, NodePort: 31443}})
	f.deployment("default", "web", "web:dev", 2)
	f.service("default", "web", "LoadBalancer", []api.K8sPort{{Name: "http", Protocol: "TCP", Port: 80, NodePort: 30080}})
	f.put(&fakeObj{o: api.K8sObject{Kind: api.K8sKindIngress, Namespace: "default", Name: "shop",
		Ingress: &api.K8sIngress{Namespace: "default", Name: "shop", Hosts: []string{"shop.k8s.fev.local"}}}})
	f.refresh()
}

func (f *fakeCluster) svcAddr(n int) string {
	a := f.plan.KubeServices().Addr()
	for i := 0; i < n; i++ {
		a = a.Next()
	}
	return a.String()
}

func (f *fakeCluster) put(x *fakeObj) {
	key := objKey{x.o.Kind, x.o.Namespace, x.o.Name}
	old := f.objs[key]
	f.rv++
	x.o.ResourceVersion = strconv.Itoa(1000 + f.rv)
	change := api.K8sChangeModified
	if old == nil {
		f.uid++
		x.o.UID = fmt.Sprintf("5b1d3c2e-0000-4000-8000-%012d", f.uid)
		x.o.Created = fakeEpoch.Add(time.Duration(f.uid) * time.Second).Format(time.RFC3339)
		change = api.K8sChangeAdded
	} else {
		x.o.UID, x.o.Created = old.o.UID, old.o.Created
		if x.owner == "" {
			x.owner = old.owner
		}
	}
	f.objs[key] = x
	f.tell(change, x.o)
}

func (f *fakeCluster) remove(key objKey) {
	x := f.objs[key]
	if x == nil {
		return
	}
	delete(f.objs, key)
	f.tell(api.K8sChangeDeleted, x.o)
}

func (f *fakeCluster) tell(change string, o api.K8sObject) {
	for _, w := range f.watches {
		if matches(w.p, o) {
			w.deliver(api.K8sChanged{Type: change, Object: o})
		}
	}
}

var defaultKinds = []string{api.K8sKindNamespace, api.K8sKindDeployment, api.K8sKindPod, api.K8sKindService, api.K8sKindIngress}

func matches(p api.K8sWatchParams, o api.K8sObject) bool {
	kinds := p.Kinds
	if len(kinds) == 0 {
		kinds = defaultKinds
	}
	found := false
	for _, k := range kinds {
		found = found || k == o.Kind
	}
	return found && (p.Namespace == "" || o.Kind == api.K8sKindNamespace || o.Namespace == p.Namespace)
}

func (f *fakeCluster) deployment(ns, name, image string, replicas int) {
	f.put(&fakeObj{o: api.K8sObject{Kind: api.K8sKindDeployment, Namespace: ns, Name: name, Labels: map[string]string{"app": name},
		Workload: &api.K8sWorkload{Wanted: replicas, Ready: replicas, Updated: replicas, Available: replicas, Images: []string{image}}}})
	f.podsOf(ns, name, image, replicas, false)
}

func (f *fakeCluster) podsOf(ns, name, image string, replicas int, fresh bool) {
	var mine []objKey
	for k, x := range f.objs {
		if k.kind == api.K8sKindPod && x.owner == ns+"/"+name {
			mine = append(mine, k)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].name < mine[j].name })
	if fresh {
		for _, k := range mine {
			f.remove(k)
		}
		mine = nil
	}
	for len(mine) > replicas {
		f.remove(mine[len(mine)-1])
		mine = mine[:len(mine)-1]
	}
	for len(mine) < replicas {
		f.pods++
		pod := fmt.Sprintf("%s-7c9d8b6f5-%05d", name, f.pods)
		ip := f.plan.KubePods().Addr()
		for i := 0; i < 1+f.pods%250; i++ {
			ip = ip.Next()
		}
		f.put(&fakeObj{owner: ns + "/" + name, o: api.K8sObject{Kind: api.K8sKindPod, Namespace: ns, Name: pod, Labels: map[string]string{"app": name},
			Pod: &api.K8sPod{Phase: "Running", Ready: true, Node: "fev", IP: ip.String(),
				Containers: []api.K8sContainer{{Name: name, Image: image, Ready: true, State: "running"}}}}})
		mine = append(mine, objKey{api.K8sKindPod, ns, pod})
	}
}

func (f *fakeCluster) service(ns, name, typ string, ports []api.K8sPort) {
	s := &api.K8sService{Namespace: ns, Name: name, Type: typ, Ports: ports}
	key := objKey{api.K8sKindService, ns, name}
	if old := f.objs[key]; old != nil && old.o.Service != nil {
		s.ClusterIP, s.LoadBalancer = old.o.Service.ClusterIP, old.o.Service.LoadBalancer
	}
	if s.ClusterIP == "" {
		s.ClusterIP = f.svcAddr(100 + f.uid)
	}
	if typ == "LoadBalancer" && s.LoadBalancer == "" {
		used := map[string]bool{}
		for _, x := range f.objs {
			if x.o.Service != nil {
				used[x.o.Service.LoadBalancer] = true
			}
		}
		lb := f.plan.KubeLoadBalancers()
		for a := lb.Addr().Next(); lb.Contains(a.Next()); a = a.Next() {
			if !used[a.String()] {
				s.LoadBalancer = a.String()
				break
			}
		}
	}
	if typ != "LoadBalancer" {
		s.LoadBalancer = ""
	}
	f.put(&fakeObj{o: api.K8sObject{Kind: api.K8sKindService, Namespace: ns, Name: name, Service: s}})
}

func (f *fakeCluster) refresh() {
	var r k8s.Reach
	r.Watching, r.Ready, r.Node, r.Version = true, !time.Now().Before(f.readyAt), "fev", standinK3sVersion
	r.Forwards = []k8s.Forward{{Proto: "tcp", Port: 6443}}
	var traefik string
	keys := f.sorted(api.K8sKindService)
	for _, k := range keys {
		x := f.objs[k]
		s := x.o.Service
		s.Names, s.URLs = nil, nil
		if s.LoadBalancer != "" {
			s.Names = []string{s.Name + "." + s.Namespace + ".k8s.fev.local", s.Name + ".k8s.fev.local"}
			s.URLs = append(s.URLs, "https://"+s.Name+".k8s.fev.local")
			if k.ns == "kube-system" && k.name == "traefik" {
				traefik = s.LoadBalancer
			}
		}
		for _, p := range s.Ports {
			if p.NodePort != 0 {
				s.URLs = append(s.URLs, fmt.Sprintf("http://localhost:%d", p.NodePort))
				r.Forwards = append(r.Forwards, k8s.Forward{Proto: strings.ToLower(p.Protocol), Port: p.NodePort, LAN: f.lan})
			}
		}
		if s.ClusterIP != "" && s.Type != "ClusterIP" {
			s.URLs = append(s.URLs, "http://"+s.ClusterIP)
		}
	}
	targets := map[string]*k8s.Target{}
	var order []string
	for _, k := range keys {
		s := f.objs[k].o.Service
		if s.LoadBalancer == "" {
			continue
		}
		t := &k8s.Target{ID: k.ns + "/" + k.name, Address: s.LoadBalancer, Names: append([]string(nil), s.Names...), Redirect80: true}
		has443 := false
		for _, p := range s.Ports {
			switch {
			case p.Port == 80:
				t.HTTPPort = 80
			case p.Port == 443:
				has443 = true
			}
		}
		if t.HTTPPort == 0 {
			for _, p := range s.Ports {
				if p.Port != 443 {
					t.HTTPPort = p.Port
					break
				}
			}
		}
		if t.HTTPPort == 0 && has443 {
			t.HTTPSPort = 443
		}
		if has443 {
			t.Own443 = t.ID != "kube-system/traefik"
			t.Pass443 = !t.Own443
		}
		targets[s.LoadBalancer] = t
		order = append(order, s.LoadBalancer)
		r.Services = append(r.Services, *s)
	}
	for _, k := range keys {
		if s := f.objs[k].o.Service; s.LoadBalancer == "" {
			r.Services = append(r.Services, *s)
		}
	}
	for _, k := range f.sorted(api.K8sKindIngress) {
		in := f.objs[k].o.Ingress
		in.Address, in.URLs = traefik, nil
		for _, h := range in.Hosts {
			if strings.HasSuffix(h, ".k8s.fev.local") && traefik != "" {
				in.URLs = append(in.URLs, "https://"+h)
				targets[traefik].Names = append(targets[traefik].Names, h)
			}
		}
		r.Ingresses = append(r.Ingresses, *in)
	}
	for _, a := range order {
		r.Targets = append(r.Targets, *targets[a])
	}
	if f.reach != nil {
		f.reach(r)
	}
}

func (f *fakeCluster) sorted(kind string) []objKey {
	var keys []objKey
	for k := range f.objs {
		if k.kind == kind {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].ns+"/"+keys[i].name < keys[j].ns+"/"+keys[j].name
	})
	return keys
}

func (f *fakeCluster) serve(path string, on func() bool) error {
	f.mu.Lock()
	f.on = on
	f.mu.Unlock()
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	go (&k8s.Server{B: f}).Serve(l)
	return nil
}

func (f *fakeCluster) check() error {
	f.mu.Lock()
	on, started := f.on, f.started
	f.mu.Unlock()
	if on != nil && !on() {
		return k8s.Errorf(api.CodeFailed, "the stand-in's cluster is not running")
	}
	if !started {
		return k8s.Errorf(api.CodeFailed, "the cluster's client has not started")
	}
	return nil
}

func (f *fakeCluster) Start(ctx context.Context, cfg k8s.Config, reach func(k8s.Reach)) error {
	f.mu.Lock()
	on, wait := f.on, f.startTime
	f.mu.Unlock()
	if on != nil && !on() {
		return k8s.Errorf(api.CodeFailed, "the stand-in's cluster is not running")
	}
	if cfg.Kubeconfig != StandinKubeconfig {
		return k8s.Errorf(api.CodeFailed, "cluster.start: not the stand-in cluster's kubeconfig")
	}
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	f.reach, f.lan, f.started = reach, cfg.LAN, true
	f.readyAt = time.Now().Add(f.readyTime)
	if f.readyTime > 0 {
		time.AfterFunc(f.readyTime, func() {
			f.mu.Lock()
			if f.reach != nil {
				f.refresh()
			}
			f.mu.Unlock()
		})
	}
	f.refresh()
	f.mu.Unlock()
	context.AfterFunc(ctx, func() {
		f.mu.Lock()
		f.reach, f.started = nil, false
		f.mu.Unlock()
	})
	return nil
}

func (f *fakeCluster) SetLAN(lan bool) {
	f.mu.Lock()
	f.lan = lan
	f.refresh()
	f.mu.Unlock()
}

func (f *fakeCluster) Close() {}

func (f *fakeCluster) Watch(ctx context.Context, p api.K8sWatchParams, deliver func(api.K8sChanged)) ([]api.K8sObject, error) {
	if err := f.check(); err != nil {
		return nil, err
	}
	for _, k := range p.Kinds {
		if _, ok := kindVersion[k]; !ok {
			return nil, k8s.Errorf(api.CodeFailed, "k8s.watch: kind %q is not one fev watches", k)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextW++
	id := f.nextW
	f.watches[id] = &fakeWatch{p: p, deliver: deliver}
	context.AfterFunc(ctx, func() {
		f.mu.Lock()
		delete(f.watches, id)
		f.mu.Unlock()
	})
	var out []api.K8sObject
	var keys []objKey
	for k, x := range f.objs {
		if matches(p, x.o) {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		return a.kind+"/"+a.ns+"/"+a.name < b.kind+"/"+b.ns+"/"+b.name
	})
	for _, k := range keys {
		out = append(out, f.objs[k].o)
	}
	return out, nil
}

var kindVersion = map[string]string{
	api.K8sKindNamespace: "v1", api.K8sKindPod: "v1", api.K8sKindService: "v1",
	api.K8sKindDeployment: "apps/v1", api.K8sKindStatefulSet: "apps/v1", api.K8sKindDaemonSet: "apps/v1",
	api.K8sKindReplicaSet: "apps/v1", api.K8sKindIngress: "networking.k8s.io/v1",
}

func (f *fakeCluster) find(ref api.K8sRef) (*fakeObj, objKey, error) {
	ns := ref.Namespace
	if ref.Kind != api.K8sKindNamespace && ns == "" {
		ns = "default"
	}
	key := objKey{ref.Kind, ns, ref.Name}
	x := f.objs[key]
	if x == nil {
		return nil, key, k8s.Errorf(api.CodeFailed, "%s %q not found in %s", strings.ToLower(ref.Kind), ref.Name, ns)
	}
	return x, key, nil
}

func manifest(x *fakeObj) string {
	doc := map[string]any{}
	for k, v := range x.doc {
		doc[k] = v
	}
	if len(doc) == 0 {
		doc["apiVersion"], doc["kind"] = kindVersion[x.o.Kind], x.o.Kind
		switch {
		case x.o.Workload != nil:
			doc["spec"] = map[string]any{"replicas": x.o.Workload.Wanted, "selector": map[string]any{"matchLabels": map[string]any{"app": x.o.Name}},
				"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": x.o.Name}},
					"spec": map[string]any{"containers": []any{map[string]any{"name": x.o.Name, "image": x.o.Workload.Images[0]}}}}}
		case x.o.Service != nil:
			var ports []any
			for _, p := range x.o.Service.Ports {
				ports = append(ports, map[string]any{"name": p.Name, "port": p.Port, "protocol": p.Protocol})
			}
			doc["spec"] = map[string]any{"type": x.o.Service.Type, "ports": ports}
		}
	}
	meta := map[string]any{"name": x.o.Name}
	if old, ok := doc["metadata"].(map[string]any); ok {
		for k, v := range old {
			meta[k] = v
		}
	}
	if x.o.Namespace != "" {
		meta["namespace"] = x.o.Namespace
	}
	meta["resourceVersion"] = x.o.ResourceVersion
	meta["uid"] = x.o.UID
	delete(meta, "managedFields")
	doc["metadata"] = meta
	out, _ := yaml.Marshal(doc)
	return string(out)
}

func (f *fakeCluster) Get(ctx context.Context, ref api.K8sRef) (api.K8sManifest, error) {
	if err := f.check(); err != nil {
		return api.K8sManifest{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	x, _, err := f.find(ref)
	if err != nil {
		return api.K8sManifest{}, err
	}
	r := api.K8sRef{APIVersion: kindVersion[x.o.Kind], Kind: x.o.Kind, Namespace: x.o.Namespace, Name: x.o.Name}
	return api.K8sManifest{Ref: r, ResourceVersion: x.o.ResourceVersion, YAML: manifest(x)}, nil
}

func parseDocs(source, text string) ([]map[string]any, []api.K8sApplied) {
	var docs []map[string]any
	var bad []api.K8sApplied
	dec := yaml.NewDecoder(strings.NewReader(text))
	for n := 1; ; n++ {
		var doc map[string]any
		err := dec.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			bad = append(bad, api.K8sApplied{Source: source, Action: api.K8sActionFailed, Error: fmt.Sprintf("%s: document %d: %s", source, n, oneLine(err.Error()))})
			break
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return docs, bad
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

func str(m map[string]any, path ...string) string {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[p]
	}
	s, _ := cur.(string)
	return s
}

func num(m map[string]any, path ...string) (int, bool) {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return 0, false
		}
		cur = mm[p]
	}
	n, ok := cur.(int)
	return n, ok
}

func (f *fakeCluster) applyDoc(source string, doc map[string]any, defNS string, dry bool) api.K8sApplied {
	kind, name := str(doc, "kind"), str(doc, "metadata", "name")
	ns := str(doc, "metadata", "namespace")
	if kind != api.K8sKindNamespace && ns == "" {
		ns = defNS
	}
	ref := &api.K8sRef{APIVersion: str(doc, "apiVersion"), Kind: kind, Namespace: ns, Name: name}
	if kind == api.K8sKindNamespace {
		ref.Namespace = ""
	}
	if kind == "" || name == "" || ref.APIVersion == "" {
		return api.K8sApplied{Source: source, Ref: ref, Action: api.K8sActionFailed, Error: "a document needs apiVersion, kind and metadata.name"}
	}
	if ref.Namespace != "" && f.objs[objKey{api.K8sKindNamespace, "", ref.Namespace}] == nil {
		return api.K8sApplied{Source: source, Ref: ref, Action: api.K8sActionFailed, Error: fmt.Sprintf("namespaces %q not found", ref.Namespace)}
	}
	key := objKey{kind, ref.Namespace, name}
	old := f.objs[key]
	clean := map[string]any{}
	for k, v := range doc {
		clean[k] = v
	}
	if meta, ok := clean["metadata"].(map[string]any); ok {
		m := map[string]any{}
		for k, v := range meta {
			if k != "resourceVersion" && k != "uid" {
				m[k] = v
			}
		}
		clean["metadata"] = m
	}
	action := api.K8sActionCreated
	if old != nil {
		if reflect.DeepEqual(old.doc, clean) {
			return api.K8sApplied{Source: source, Ref: ref, Action: api.K8sActionUnchanged}
		}
		action = api.K8sActionConfigured
	}
	if dry {
		return api.K8sApplied{Source: source, Ref: ref, Action: action}
	}
	switch kind {
	case api.K8sKindDeployment:
		replicas, ok := num(doc, "spec", "replicas")
		if !ok {
			replicas = 1
		}
		image := "busybox"
		if spec, ok := doc["spec"].(map[string]any); ok {
			if c := containersOf(spec); len(c) > 0 {
				if im := str(c[0], "image"); im != "" {
					image = im
				}
			}
		}
		f.deployment(ref.Namespace, name, image, replicas)
	case api.K8sKindService:
		typ := str(doc, "spec", "type")
		if typ == "" {
			typ = "ClusterIP"
		}
		var ports []api.K8sPort
		if spec, ok := doc["spec"].(map[string]any); ok {
			list, _ := spec["ports"].([]any)
			for i, p := range list {
				pm, _ := p.(map[string]any)
				port, _ := num(pm, "port")
				proto := str(pm, "protocol")
				if proto == "" {
					proto = "TCP"
				}
				kp := api.K8sPort{Name: str(pm, "name"), Protocol: proto, Port: port}
				if typ == "NodePort" || typ == "LoadBalancer" {
					kp.NodePort = 30100 + f.uid + i
				}
				ports = append(ports, kp)
			}
		}
		f.service(ref.Namespace, name, typ, ports)
	case api.K8sKindNamespace:
		f.put(&fakeObj{o: api.K8sObject{Kind: kind, Name: name, Phase: "Active"}})
	case api.K8sKindIngress:
		var hosts []string
		if spec, ok := doc["spec"].(map[string]any); ok {
			rules, _ := spec["rules"].([]any)
			for _, r := range rules {
				if h := str(r.(map[string]any), "host"); h != "" {
					hosts = append(hosts, strings.ToLower(h))
				}
			}
		}
		f.put(&fakeObj{o: api.K8sObject{Kind: kind, Namespace: ref.Namespace, Name: name, Ingress: &api.K8sIngress{Namespace: ref.Namespace, Name: name, Hosts: hosts}}})
	default:
		f.put(&fakeObj{o: api.K8sObject{Kind: kind, Namespace: ref.Namespace, Name: name}})
	}
	f.objs[key].doc = clean
	f.refresh()
	return api.K8sApplied{Source: source, Ref: ref, Action: action}
}

func containersOf(spec map[string]any) []map[string]any {
	t, _ := spec["template"].(map[string]any)
	ts, _ := t["spec"].(map[string]any)
	list, _ := ts["containers"].([]any)
	var out []map[string]any
	for _, c := range list {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeCluster) Apply(ctx context.Context, p api.K8sApplyParams) (api.K8sApplyResult, error) {
	if err := f.check(); err != nil {
		return api.K8sApplyResult{}, err
	}
	ns := p.Namespace
	if ns == "" {
		ns = "default"
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r := api.K8sApplyResult{Objects: []api.K8sApplied{}}
	for _, d := range p.Documents {
		docs, bad := parseDocs(d.Source, d.YAML)
		for _, doc := range docs {
			r.Objects = append(r.Objects, f.applyDoc(d.Source, doc, ns, p.DryRun))
		}
		r.Objects = append(r.Objects, bad...)
	}
	return r, nil
}

func (f *fakeCluster) Update(ctx context.Context, p api.K8sUpdateParams) (api.K8sApplyResult, error) {
	if err := f.check(); err != nil {
		return api.K8sApplyResult{}, err
	}
	docs, bad := parseDocs("edit", p.YAML)
	if len(bad) > 0 || len(docs) != 1 {
		return api.K8sApplyResult{}, k8s.Errorf(api.CodeFailed, "the edited manifest is not one YAML document")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	x, _, err := f.find(p.Ref)
	if err != nil {
		return api.K8sApplyResult{}, err
	}
	rv := str(docs[0], "metadata", "resourceVersion")
	if rv != x.o.ResourceVersion {
		return api.K8sApplyResult{}, k8s.Errorf(api.CodeConflict, "the %s %s/%s changed since it was read (resourceVersion %s, now %s); nothing was written",
			p.Ref.Kind, x.o.Namespace, x.o.Name, rv, x.o.ResourceVersion)
	}
	if str(docs[0], "kind") != x.o.Kind || str(docs[0], "metadata", "name") != x.o.Name {
		return api.K8sApplyResult{}, k8s.Errorf(api.CodeFailed, "the edited manifest names another object")
	}
	a := f.applyDoc("", docs[0], x.o.Namespace, false)
	if a.Action == api.K8sActionUnchanged {
		a.Action = api.K8sActionConfigured
	}
	a.Source = ""
	return api.K8sApplyResult{Objects: []api.K8sApplied{a}}, nil
}

func (f *fakeCluster) Scale(ctx context.Context, p api.K8sScaleParams) (api.K8sObject, error) {
	if err := f.check(); err != nil {
		return api.K8sObject{}, err
	}
	if p.Replicas < 0 {
		return api.K8sObject{}, k8s.Errorf(api.CodeFailed, "replicas %d: at least 0", p.Replicas)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	x, _, err := f.find(p.Ref)
	if err != nil {
		return api.K8sObject{}, err
	}
	if x.o.Workload == nil || x.o.Kind == api.K8sKindDaemonSet {
		return api.K8sObject{}, k8s.Errorf(api.CodeFailed, "a %s has no scale", x.o.Kind)
	}
	w := *x.o.Workload
	w.Wanted, w.Ready, w.Updated, w.Available = p.Replicas, p.Replicas, p.Replicas, p.Replicas
	nx := &fakeObj{o: x.o, doc: x.doc}
	nx.o.Workload = &w
	if nx.doc != nil {
		if spec, ok := nx.doc["spec"].(map[string]any); ok {
			spec["replicas"] = p.Replicas
		}
	}
	f.put(nx)
	f.podsOf(x.o.Namespace, x.o.Name, w.Images[0], p.Replicas, false)
	return nx.o, nil
}

func (f *fakeCluster) Restart(ctx context.Context, ref api.K8sRef) (api.K8sObject, error) {
	if err := f.check(); err != nil {
		return api.K8sObject{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	x, _, err := f.find(ref)
	if err != nil {
		return api.K8sObject{}, err
	}
	if x.o.Workload == nil || x.o.Kind == api.K8sKindReplicaSet {
		return api.K8sObject{}, k8s.Errorf(api.CodeFailed, "a %s has no rollout to restart", x.o.Kind)
	}
	nx := &fakeObj{o: x.o, doc: x.doc}
	f.put(nx)
	f.podsOf(x.o.Namespace, x.o.Name, x.o.Workload.Images[0], x.o.Workload.Wanted, true)
	return nx.o, nil
}

func (f *fakeCluster) Delete(ctx context.Context, ref api.K8sRef) error {
	if err := f.check(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	x, key, err := f.find(ref)
	if err != nil {
		return err
	}
	if x.o.Kind == api.K8sKindDeployment {
		f.podsOf(x.o.Namespace, x.o.Name, "", 0, false)
	}
	if x.o.Kind == api.K8sKindNamespace {
		for k := range f.objs {
			if k.ns == x.o.Name {
				f.remove(k)
			}
		}
	}
	f.remove(key)
	if x.owner != "" {
		if d := f.objs[objKey{api.K8sKindDeployment, x.o.Namespace, strings.TrimPrefix(x.owner, x.o.Namespace+"/")}]; d != nil {
			f.podsOf(d.o.Namespace, d.o.Name, d.o.Workload.Images[0], d.o.Workload.Wanted, false)
		}
	}
	f.refresh()
	return nil
}

func (f *fakeCluster) pod(ns, name, container string) (*fakeObj, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	x, _, err := f.find(api.K8sRef{Kind: api.K8sKindPod, Namespace: ns, Name: name})
	if err != nil {
		return nil, "", err
	}
	cs := x.o.Pod.Containers
	if container == "" {
		if len(cs) != 1 {
			return nil, "", k8s.Errorf(api.CodeFailed, "pod %s has %d containers: name one", name, len(cs))
		}
		return x, cs[0].Name, nil
	}
	for _, c := range cs {
		if c.Name == container {
			return x, container, nil
		}
	}
	return nil, "", k8s.Errorf(api.CodeFailed, "container %q is not in pod %s", container, name)
}

func (f *fakeCluster) Logs(ctx context.Context, p api.K8sLogsParams, text func(string)) error {
	if err := f.check(); err != nil {
		return err
	}
	x, container, err := f.pod(p.Namespace, p.Pod, p.Container)
	if err != nil {
		return err
	}
	if p.Previous {
		restarts := 0
		for _, c := range x.o.Pod.Containers {
			if c.Name == container {
				restarts = c.Restarts
			}
		}
		if restarts == 0 {
			return fmt.Errorf("previous terminated container %q in pod %q not found", container, p.Pod)
		}
	}
	lines := []string{"listening on :8080", "GET / 200 1.2ms", "GET /healthz 200 0.3ms"}
	if p.TailLines > 0 && p.TailLines < len(lines) {
		lines = lines[len(lines)-p.TailLines:]
	}
	var b strings.Builder
	for i, l := range lines {
		if p.Timestamps {
			fmt.Fprintf(&b, "%s ", fakeEpoch.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano))
		}
		fmt.Fprintf(&b, "%s %s/%s: %s\n", container, p.Pod, container, l)
	}
	text(b.String())
	if p.Follow {
		<-ctx.Done()
	}
	return nil
}

func (f *fakeCluster) ExecTarget(ctx context.Context, p api.K8sExecParams) (string, error) {
	if err := f.check(); err != nil {
		return "", err
	}
	_, container, err := f.pod(p.Namespace, p.Pod, p.Container)
	return container, err
}

func (f *fakeCluster) Exec(ctx context.Context, p api.K8sExecParams, stream io.ReadWriter, resize <-chan [2]int) (int, error) {
	run := func(argv []string, w io.Writer) int {
		switch {
		case len(argv) == 0:
			return 0
		case argv[0] == "echo":
			fmt.Fprintln(w, strings.Join(argv[1:], " "))
		case argv[0] == "hostname":
			fmt.Fprintln(w, p.Pod)
		case argv[0] == "true":
		case argv[0] == "size":
			select {
			case sz := <-resize:
				fmt.Fprintf(w, "%d %d\n", sz[0], sz[1])
			default:
				fmt.Fprintln(w, "no size")
			}
		case argv[0] == "exit":
			n := 0
			if len(argv) > 1 {
				n, _ = strconv.Atoi(argv[1])
			}
			return n
		default:
			fmt.Fprintf(w, "sh: %s: not found\n", argv[0])
			return 127
		}
		return 0
	}
	if len(p.Argv) > 0 {
		return run(p.Argv, stream), nil
	}
	fmt.Fprint(stream, "/ # ")
	sc := bufio.NewScanner(stream)
	for sc.Scan() {
		argv := strings.Fields(sc.Text())
		if len(argv) > 0 && argv[0] == "exit" {
			return run(argv, stream), nil
		}
		run(argv, stream)
		fmt.Fprint(stream, "/ # ")
	}
	return 0, nil
}
