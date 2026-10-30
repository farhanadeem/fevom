package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"

	"fev/host/k8s"
	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

const FieldManager = "fev"

const restartedAt = "kubectl.kubernetes.io/restartedAt"

const watchSync = 30 * time.Second

type mgmt struct {
	dyn     dynamic.Interface
	mapper  meta.ResettableRESTMapper
	lbRange netip.Prefix
}

func newMgmt(rc *rest.Config, cs kubernetes.Interface, cfg k8s.Config) (*mgmt, error) {
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	m := &mgmt{dyn: dyn, mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(cs.Discovery()))}
	m.lbRange, _ = netip.ParsePrefix(cfg.Ranges.LoadBalancers)
	return m, nil
}

func NewWithClients(cs kubernetes.Interface, dyn dynamic.Interface, mapper meta.ResettableRESTMapper, rc *rest.Config, ranges api.K8sRanges) *Cluster {
	c := &Cluster{cs: cs, rest: rc}
	c.m = &mgmt{dyn: dyn, mapper: mapper}
	c.m.lbRange, _ = netip.ParsePrefix(ranges.LoadBalancers)
	return c
}

func (c *Cluster) clients() (kubernetes.Interface, *mgmt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cs == nil || c.m == nil {
		return nil, nil, k8s.Errorf(api.CodeFailed, "the cluster's client has not started")
	}
	return c.cs, c.m, nil
}

func apiError(err error) error {
	if err == nil {
		return nil
	}
	var e *jsonrpc.Error
	if errors.As(err, &e) {
		return err
	}
	code := api.CodeFailed
	if apierrors.IsConflict(err) {
		code = api.CodeConflict
	}
	return k8s.Errorf(code, "%s", oneLine(err.Error()))
}

func message(err error) string {
	var e *jsonrpc.Error
	if errors.As(err, &e) {
		return oneLine(e.Message)
	}
	return oneLine(err.Error())
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

var kindVersion = map[string]string{
	api.K8sKindNamespace:   "v1",
	api.K8sKindPod:         "v1",
	api.K8sKindService:     "v1",
	api.K8sKindDeployment:  "apps/v1",
	api.K8sKindStatefulSet: "apps/v1",
	api.K8sKindDaemonSet:   "apps/v1",
	api.K8sKindReplicaSet:  "apps/v1",
	api.K8sKindIngress:     "networking.k8s.io/v1",
}

func (m *mgmt) mapping(gvk schema.GroupVersionKind) (*meta.RESTMapping, error) {
	mp, err := m.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if meta.IsNoMatchError(err) {
		m.mapper.Reset()
		mp, err = m.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	}
	if err != nil {
		if meta.IsNoMatchError(err) {
			return nil, k8s.Errorf(api.CodeFailed, "the cluster has no kind %s in %s", gvk.Kind, gvk.GroupVersion())
		}
		return nil, apiError(err)
	}
	return mp, nil
}

func (m *mgmt) resource(ref api.K8sRef) (dynamic.ResourceInterface, string, error) {
	if ref.Kind == "" || ref.Name == "" {
		return nil, "", k8s.Errorf(api.CodeFailed, "name the object: its kind and its name")
	}
	av := ref.APIVersion
	if av == "" {
		av = kindVersion[ref.Kind]
	}
	if av == "" {
		return nil, "", k8s.Errorf(api.CodeFailed, "a %s needs its api_version", ref.Kind)
	}
	gv, err := schema.ParseGroupVersion(av)
	if err != nil {
		return nil, "", k8s.Errorf(api.CodeFailed, "api_version %q: %v", av, err)
	}
	mp, err := m.mapping(gv.WithKind(ref.Kind))
	if err != nil {
		return nil, "", err
	}
	if mp.Scope.Name() != meta.RESTScopeNameNamespace {
		return m.dyn.Resource(mp.Resource), "", nil
	}
	ns := ref.Namespace
	if ns == "" {
		ns = metav1.NamespaceDefault
	}
	return m.dyn.Resource(mp.Resource).Namespace(ns), ns, nil
}

func (c *Cluster) Watch(ctx context.Context, p api.K8sWatchParams, deliver func(api.K8sChanged)) ([]api.K8sObject, error) {
	cs, m, err := c.clients()
	if err != nil {
		return nil, err
	}
	kinds := p.Kinds
	if len(kinds) == 0 {
		kinds = []string{api.K8sKindNamespace, api.K8sKindDeployment, api.K8sKindPod, api.K8sKindService, api.K8sKindIngress}
	}
	inNS := informers.NewSharedInformerFactoryWithOptions(cs, 0, informers.WithNamespace(p.Namespace))
	all := informers.NewSharedInformerFactory(cs, 0)
	var (
		mu      sync.Mutex
		synced  bool
		initial []api.K8sObject
	)
	tell := func(change string, obj any) {
		if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = d.Obj
		}
		s, ok := summarize(obj, m.lbRange)
		if !ok {
			return
		}
		deliver(api.K8sChanged{Type: change, Object: s})
	}
	handler := cache.ResourceEventHandlerDetailedFuncs{
		AddFunc: func(obj any, isInInitialList bool) {
			mu.Lock()
			if isInInitialList && !synced {
				if s, ok := summarize(obj, m.lbRange); ok {
					initial = append(initial, s)
				}
				mu.Unlock()
				return
			}
			mu.Unlock()
			tell(api.K8sChangeAdded, obj)
		},
		UpdateFunc: func(old, obj any) {
			if o, ok := old.(metav1.Object); ok {
				if n, ok := obj.(metav1.Object); ok && o.GetResourceVersion() == n.GetResourceVersion() {
					return
				}
			}
			tell(api.K8sChangeModified, obj)
		},
		DeleteFunc: func(obj any) { tell(api.K8sChangeDeleted, obj) },
	}
	var regs []cache.InformerSynced
	seen := map[string]bool{}
	for _, kind := range kinds {
		if seen[kind] {
			continue
		}
		seen[kind] = true
		var inf cache.SharedIndexInformer
		switch kind {
		case api.K8sKindNamespace:
			inf = all.Core().V1().Namespaces().Informer()
		case api.K8sKindDeployment:
			inf = inNS.Apps().V1().Deployments().Informer()
		case api.K8sKindStatefulSet:
			inf = inNS.Apps().V1().StatefulSets().Informer()
		case api.K8sKindDaemonSet:
			inf = inNS.Apps().V1().DaemonSets().Informer()
		case api.K8sKindReplicaSet:
			inf = inNS.Apps().V1().ReplicaSets().Informer()
		case api.K8sKindPod:
			inf = inNS.Core().V1().Pods().Informer()
		case api.K8sKindService:
			inf = inNS.Core().V1().Services().Informer()
		case api.K8sKindIngress:
			inf = inNS.Networking().V1().Ingresses().Informer()
		default:
			return nil, k8s.Errorf(api.CodeFailed, "k8s.watch: kind %q is not one the view watches", kind)
		}
		reg, err := inf.AddEventHandler(handler)
		if err != nil {
			return nil, err
		}
		regs = append(regs, reg.HasSynced)
	}
	wctx, cancel := context.WithCancel(ctx)
	inNS.Start(wctx.Done())
	all.Start(wctx.Done())
	go func() {
		<-wctx.Done()
		inNS.Shutdown()
		all.Shutdown()
	}()
	sctx, scancel := context.WithTimeout(wctx, watchSync)
	ok := cache.WaitForCacheSync(sctx.Done(), regs...)
	scancel()
	if !ok {
		cancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, k8s.Errorf(api.CodeFailed, "the cluster did not list %s within %v", strings.Join(kinds, ", "), watchSync)
	}
	context.AfterFunc(ctx, cancel)
	mu.Lock()
	synced = true
	out := initial
	mu.Unlock()
	order := map[string]int{}
	for i, k := range kinds {
		if _, ok := order[k]; !ok {
			order[k] = i
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return order[a.Kind] < order[b.Kind]
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	if out == nil {
		out = []api.K8sObject{}
	}
	return out, nil
}

func clean(u *unstructured.Unstructured) {
	unstructured.RemoveNestedField(u.Object, "metadata", "managedFields")
}

func refOf(u *unstructured.Unstructured) api.K8sRef {
	return api.K8sRef{APIVersion: u.GetAPIVersion(), Kind: u.GetKind(), Namespace: u.GetNamespace(), Name: u.GetName()}
}

func (c *Cluster) Get(ctx context.Context, ref api.K8sRef) (api.K8sManifest, error) {
	_, m, err := c.clients()
	if err != nil {
		return api.K8sManifest{}, err
	}
	r, _, err := m.resource(ref)
	if err != nil {
		return api.K8sManifest{}, err
	}
	u, err := r.Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return api.K8sManifest{}, apiError(err)
	}
	clean(u)
	out, err := yaml.Marshal(u.Object)
	if err != nil {
		return api.K8sManifest{}, err
	}
	return api.K8sManifest{Ref: refOf(u), ResourceVersion: u.GetResourceVersion(), YAML: string(out)}, nil
}

func documents(d api.K8sDocument) ([]*unstructured.Unstructured, *api.K8sApplied) {
	dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(d.YAML), 4096)
	var out []*unstructured.Unstructured
	for n := 1; ; n++ {
		var obj map[string]any
		err := dec.Decode(&obj)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, &api.K8sApplied{Source: d.Source, Action: api.K8sActionFailed, Error: fmt.Sprintf("document %d: %s", n, oneLine(err.Error()))}
		}
		if len(obj) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: obj}
		if u.IsList() {
			items, err := u.ToList()
			if err != nil {
				return out, &api.K8sApplied{Source: d.Source, Action: api.K8sActionFailed, Error: fmt.Sprintf("document %d: %s", n, oneLine(err.Error()))}
			}
			for i := range items.Items {
				out = append(out, &items.Items[i])
			}
			continue
		}
		out = append(out, u)
	}
}

func (c *Cluster) Apply(ctx context.Context, p api.K8sApplyParams) (api.K8sApplyResult, error) {
	_, m, err := c.clients()
	if err != nil {
		return api.K8sApplyResult{}, err
	}
	defNS := p.Namespace
	if defNS == "" {
		defNS = metav1.NamespaceDefault
	}
	res := api.K8sApplyResult{Objects: []api.K8sApplied{}}
	for _, d := range p.Documents {
		objs, bad := documents(d)
		for _, u := range objs {
			res.Objects = append(res.Objects, m.applyOne(ctx, d.Source, u, defNS, p.DryRun))
		}
		if bad != nil {
			res.Objects = append(res.Objects, *bad)
		}
	}
	return res, nil
}

func (m *mgmt) applyOne(ctx context.Context, source string, u *unstructured.Unstructured, defNS string, dry bool) api.K8sApplied {
	a := api.K8sApplied{Source: source}
	fail := func(err error) api.K8sApplied {
		a.Action, a.Error = api.K8sActionFailed, message(err)
		return a
	}
	gvk := u.GroupVersionKind()
	if gvk.Kind == "" || u.GetAPIVersion() == "" {
		return fail(errors.New("a document needs its apiVersion and kind"))
	}
	if u.GetName() == "" {
		ref := api.K8sRef{APIVersion: u.GetAPIVersion(), Kind: gvk.Kind}
		a.Ref = &ref
		return fail(fmt.Errorf("a %s needs metadata.name to be applied", gvk.Kind))
	}
	mp, err := m.mapping(gvk)
	if err != nil {
		ref := refOf(u)
		a.Ref = &ref
		return fail(err)
	}
	var r dynamic.ResourceInterface = m.dyn.Resource(mp.Resource)
	if mp.Scope.Name() == meta.RESTScopeNameNamespace {
		if u.GetNamespace() == "" {
			u.SetNamespace(defNS)
		}
		r = m.dyn.Resource(mp.Resource).Namespace(u.GetNamespace())
	} else {
		u.SetNamespace("")
	}
	ref := refOf(u)
	a.Ref = &ref
	old, err := r.Get(ctx, u.GetName(), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		old = nil
	case err != nil:
		return fail(apiError(err))
	}
	opts := metav1.ApplyOptions{FieldManager: FieldManager, Force: false}
	if dry {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	if _, err := r.Apply(ctx, u.GetName(), u, opts); err != nil {
		return fail(apiError(err))
	}
	switch {
	case old == nil:
		a.Action = api.K8sActionCreated
	case contains(old.Object, u.Object):
		a.Action = api.K8sActionUnchanged
	default:
		a.Action = api.K8sActionConfigured
	}
	return a
}

func contains(have, want any) bool {
	switch w := want.(type) {
	case map[string]any:
		h, ok := have.(map[string]any)
		if !ok {
			return false
		}
		for k, wv := range w {
			hv, ok := h[k]
			if !ok || !contains(hv, wv) {
				return false
			}
		}
		return true
	case []any:
		h, ok := have.([]any)
		if !ok || len(h) != len(w) {
			return false
		}
		for i := range w {
			if !contains(h[i], w[i]) {
				return false
			}
		}
		return true
	case nil:
		return have == nil
	}
	if wn, ok := number(want); ok {
		hn, ok := number(have)
		return ok && hn == wn
	}
	return reflect.DeepEqual(have, want)
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case float64:
		return n, true
	case int32:
		return float64(n), true
	}
	return 0, false
}

func (c *Cluster) Update(ctx context.Context, p api.K8sUpdateParams) (api.K8sApplyResult, error) {
	_, m, err := c.clients()
	if err != nil {
		return api.K8sApplyResult{}, err
	}
	objs, bad := documents(api.K8sDocument{Source: "edit", YAML: p.YAML})
	if bad != nil || len(objs) != 1 {
		return api.K8sApplyResult{}, k8s.Errorf(api.CodeFailed, "the edited manifest is not one YAML document")
	}
	u := objs[0]
	r, ns, err := m.resource(p.Ref)
	if err != nil {
		return api.K8sApplyResult{}, err
	}
	if u.GetNamespace() == "" {
		u.SetNamespace(ns)
	}
	if u.GetKind() != p.Ref.Kind || u.GetName() != p.Ref.Name || u.GetNamespace() != ns {
		return api.K8sApplyResult{}, k8s.Errorf(api.CodeFailed, "the edited manifest names %s %s/%s, not the %s %s/%s it was read as",
			u.GetKind(), u.GetNamespace(), u.GetName(), p.Ref.Kind, ns, p.Ref.Name)
	}
	rv := u.GetResourceVersion()
	if rv == "" {
		return api.K8sApplyResult{}, k8s.Errorf(api.CodeFailed, "the edited manifest has no resourceVersion: edit what k8s.get returned, so that a change made since is not overwritten")
	}
	got, err := r.Update(ctx, u, metav1.UpdateOptions{FieldManager: FieldManager})
	if apierrors.IsConflict(err) {
		now := ""
		if cur, gerr := r.Get(ctx, p.Ref.Name, metav1.GetOptions{}); gerr == nil {
			now = ", now " + cur.GetResourceVersion()
		}
		return api.K8sApplyResult{}, k8s.Errorf(api.CodeConflict, "the %s %s/%s changed since it was read (resourceVersion %s%s); nothing was written",
			p.Ref.Kind, ns, p.Ref.Name, rv, now)
	}
	if err != nil {
		return api.K8sApplyResult{}, apiError(err)
	}
	ref := refOf(got)
	a := api.K8sApplied{Ref: &ref, Action: api.K8sActionConfigured}
	if got.GetResourceVersion() == rv {
		a.Action = api.K8sActionUnchanged
	}
	return api.K8sApplyResult{Objects: []api.K8sApplied{a}}, nil
}

func (c *Cluster) Scale(ctx context.Context, p api.K8sScaleParams) (api.K8sObject, error) {
	cs, m, err := c.clients()
	if err != nil {
		return api.K8sObject{}, err
	}
	if p.Replicas < 0 {
		return api.K8sObject{}, k8s.Errorf(api.CodeFailed, "replicas %d: at least 0", p.Replicas)
	}
	ns := p.Ref.Namespace
	if ns == "" {
		ns = metav1.NamespaceDefault
	}
	sc := &autoscalingv1.Scale{ObjectMeta: metav1.ObjectMeta{Name: p.Ref.Name, Namespace: ns}, Spec: autoscalingv1.ScaleSpec{Replicas: int32(p.Replicas)}}
	opts := metav1.UpdateOptions{FieldManager: FieldManager}
	apps := cs.AppsV1()
	switch p.Ref.Kind {
	case api.K8sKindDeployment:
		_, err = apps.Deployments(ns).UpdateScale(ctx, p.Ref.Name, sc, opts)
	case api.K8sKindStatefulSet:
		_, err = apps.StatefulSets(ns).UpdateScale(ctx, p.Ref.Name, sc, opts)
	case api.K8sKindReplicaSet:
		_, err = apps.ReplicaSets(ns).UpdateScale(ctx, p.Ref.Name, sc, opts)
	default:
		return api.K8sObject{}, k8s.Errorf(api.CodeFailed, "a %s has no scale: a Deployment, StatefulSet or ReplicaSet has", p.Ref.Kind)
	}
	if err != nil {
		return api.K8sObject{}, apiError(err)
	}
	return c.summaryOf(ctx, cs, m, p.Ref.Kind, ns, p.Ref.Name)
}

func (c *Cluster) Restart(ctx context.Context, ref api.K8sRef) (api.K8sObject, error) {
	cs, m, err := c.clients()
	if err != nil {
		return api.K8sObject{}, err
	}
	ns := ref.Namespace
	if ns == "" {
		ns = metav1.NamespaceDefault
	}
	patch := fmt.Appendf(nil, `{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`, restartedAt, time.Now().Format(time.RFC3339))
	opts := metav1.PatchOptions{FieldManager: FieldManager}
	apps := cs.AppsV1()
	switch ref.Kind {
	case api.K8sKindDeployment:
		_, err = apps.Deployments(ns).Patch(ctx, ref.Name, types.StrategicMergePatchType, patch, opts)
	case api.K8sKindStatefulSet:
		_, err = apps.StatefulSets(ns).Patch(ctx, ref.Name, types.StrategicMergePatchType, patch, opts)
	case api.K8sKindDaemonSet:
		_, err = apps.DaemonSets(ns).Patch(ctx, ref.Name, types.StrategicMergePatchType, patch, opts)
	default:
		return api.K8sObject{}, k8s.Errorf(api.CodeFailed, "a %s has no rollout to restart: a Deployment, StatefulSet or DaemonSet has", ref.Kind)
	}
	if err != nil {
		return api.K8sObject{}, apiError(err)
	}
	return c.summaryOf(ctx, cs, m, ref.Kind, ns, ref.Name)
}

func (c *Cluster) summaryOf(ctx context.Context, cs kubernetes.Interface, m *mgmt, kind, ns, name string) (api.K8sObject, error) {
	var obj any
	var err error
	g := metav1.GetOptions{}
	switch kind {
	case api.K8sKindDeployment:
		var o *appsv1.Deployment
		o, err = cs.AppsV1().Deployments(ns).Get(ctx, name, g)
		obj = o
	case api.K8sKindStatefulSet:
		var o *appsv1.StatefulSet
		o, err = cs.AppsV1().StatefulSets(ns).Get(ctx, name, g)
		obj = o
	case api.K8sKindDaemonSet:
		var o *appsv1.DaemonSet
		o, err = cs.AppsV1().DaemonSets(ns).Get(ctx, name, g)
		obj = o
	case api.K8sKindReplicaSet:
		var o *appsv1.ReplicaSet
		o, err = cs.AppsV1().ReplicaSets(ns).Get(ctx, name, g)
		obj = o
	case api.K8sKindPod:
		var o *corev1.Pod
		o, err = cs.CoreV1().Pods(ns).Get(ctx, name, g)
		obj = o
	case api.K8sKindService:
		var o *corev1.Service
		o, err = cs.CoreV1().Services(ns).Get(ctx, name, g)
		obj = o
	case api.K8sKindIngress:
		var o *netv1.Ingress
		o, err = cs.NetworkingV1().Ingresses(ns).Get(ctx, name, g)
		obj = o
	}
	if err != nil {
		return api.K8sObject{}, apiError(err)
	}
	s, _ := summarize(obj, m.lbRange)
	return s, nil
}

func (c *Cluster) Delete(ctx context.Context, ref api.K8sRef) error {
	_, m, err := c.clients()
	if err != nil {
		return err
	}
	r, _, err := m.resource(ref)
	if err != nil {
		return err
	}
	bg := metav1.DeletePropagationBackground
	return apiError(r.Delete(ctx, ref.Name, metav1.DeleteOptions{PropagationPolicy: &bg}))
}
