package kube

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "fev/proto/daemon"
)

func summarize(obj any, lbRange netip.Prefix) (api.K8sObject, bool) {
	switch o := obj.(type) {
	case *corev1.Namespace:
		s := header(api.K8sKindNamespace, &o.ObjectMeta)
		s.Phase = string(o.Status.Phase)
		return s, true
	case *appsv1.Deployment:
		s := header(api.K8sKindDeployment, &o.ObjectMeta)
		s.Workload = &api.K8sWorkload{Wanted: replicas(o.Spec.Replicas), Ready: int(o.Status.ReadyReplicas),
			Updated: int(o.Status.UpdatedReplicas), Available: int(o.Status.AvailableReplicas), Images: images(&o.Spec.Template.Spec)}
		return s, true
	case *appsv1.StatefulSet:
		s := header(api.K8sKindStatefulSet, &o.ObjectMeta)
		s.Workload = &api.K8sWorkload{Wanted: replicas(o.Spec.Replicas), Ready: int(o.Status.ReadyReplicas),
			Updated: int(o.Status.UpdatedReplicas), Available: int(o.Status.AvailableReplicas), Images: images(&o.Spec.Template.Spec)}
		return s, true
	case *appsv1.DaemonSet:
		s := header(api.K8sKindDaemonSet, &o.ObjectMeta)
		s.Workload = &api.K8sWorkload{Wanted: int(o.Status.DesiredNumberScheduled), Ready: int(o.Status.NumberReady),
			Updated: int(o.Status.UpdatedNumberScheduled), Available: int(o.Status.NumberAvailable), Images: images(&o.Spec.Template.Spec)}
		return s, true
	case *appsv1.ReplicaSet:
		s := header(api.K8sKindReplicaSet, &o.ObjectMeta)
		s.Workload = &api.K8sWorkload{Wanted: replicas(o.Spec.Replicas), Ready: int(o.Status.ReadyReplicas),
			Updated: int(o.Status.Replicas), Available: int(o.Status.AvailableReplicas), Images: images(&o.Spec.Template.Spec)}
		return s, true
	case *corev1.Pod:
		s := header(api.K8sKindPod, &o.ObjectMeta)
		s.Pod = podState(o)
		return s, true
	case *corev1.Service:
		s := header(api.K8sKindService, &o.ObjectMeta)
		s.Service = serviceState(o, lbRange)
		return s, true
	case *netv1.Ingress:
		s := header(api.K8sKindIngress, &o.ObjectMeta)
		s.Ingress = ingressState(o)
		return s, true
	}
	return api.K8sObject{}, false
}

func header(kind string, m *metav1.ObjectMeta) api.K8sObject {
	s := api.K8sObject{Kind: kind, Namespace: m.Namespace, Name: m.Name, UID: string(m.UID),
		ResourceVersion: m.ResourceVersion, Labels: m.Labels}
	if !m.CreationTimestamp.IsZero() {
		s.Created = m.CreationTimestamp.UTC().Format(time.RFC3339)
	}
	return s
}

func replicas(n *int32) int {
	if n == nil {
		return 1
	}
	return int(*n)
}

func images(spec *corev1.PodSpec) []string {
	var out []string
	for _, c := range spec.Containers {
		out = append(out, c.Image)
	}
	return out
}

func podState(p *corev1.Pod) *api.K8sPod {
	s := &api.K8sPod{Phase: string(p.Status.Phase), Node: p.Spec.NodeName, IP: p.Status.PodIP, Reason: p.Status.Reason}
	if s.Phase == "" {
		s.Phase = string(corev1.PodPending)
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			s.Ready = c.Status == corev1.ConditionTrue
		}
	}
	status := map[string]corev1.ContainerStatus{}
	for _, cs := range p.Status.InitContainerStatuses {
		status["init/"+cs.Name] = cs
	}
	for _, cs := range p.Status.ContainerStatuses {
		status[cs.Name] = cs
	}
	add := func(c corev1.Container, init bool) {
		key := c.Name
		if init {
			key = "init/" + c.Name
		}
		cs, seen := status[key]
		kc := api.K8sContainer{Name: c.Name, Image: c.Image, InitContainer: init, State: "waiting"}
		if seen {
			kc.Ready, kc.Restarts = cs.Ready, int(cs.RestartCount)
			switch {
			case cs.State.Running != nil:
				kc.State = "running"
			case cs.State.Terminated != nil:
				kc.State = "terminated"
			case cs.State.Waiting != nil && s.Reason == "" && cs.State.Waiting.Reason != "" && cs.State.Waiting.Reason != "ContainerCreating" && cs.State.Waiting.Reason != "PodInitializing":
				s.Reason = cs.State.Waiting.Reason
			}
		}
		s.Restarts += kc.Restarts
		s.Containers = append(s.Containers, kc)
	}
	for _, c := range p.Spec.InitContainers {
		add(c, true)
	}
	for _, c := range p.Spec.Containers {
		add(c, false)
	}
	if p.DeletionTimestamp != nil && s.Reason == "" {
		s.Reason = "Terminating"
	}
	return s
}

func serviceState(s *corev1.Service, lbRange netip.Prefix) *api.K8sService {
	ks := &api.K8sService{Namespace: s.Namespace, Name: s.Name, Type: string(s.Spec.Type), ClusterIP: s.Spec.ClusterIP}
	for _, li := range s.Status.LoadBalancer.Ingress {
		if a, err := netip.ParseAddr(li.IP); err == nil && lbRange.IsValid() && lbRange.Contains(a) {
			ks.LoadBalancer = a.String()
			break
		}
	}
	if ks.LoadBalancer != "" {
		ks.Names = []string{s.Name + "." + s.Namespace + "." + k8sZone, s.Name + "." + k8sZone}
		ks.URLs = append(ks.URLs, "https://"+s.Name+"."+k8sZone)
	}
	for _, p := range s.Spec.Ports {
		ks.Ports = append(ks.Ports, api.K8sPort{Name: p.Name, Protocol: string(p.Protocol), Port: int(p.Port), NodePort: int(p.NodePort)})
		if p.NodePort != 0 && (p.Protocol == corev1.ProtocolTCP || p.Protocol == "") {
			ks.URLs = append(ks.URLs, fmt.Sprintf("http://localhost:%d", p.NodePort))
		}
	}
	return ks
}

func ingressState(in *netv1.Ingress) *api.K8sIngress {
	ki := &api.K8sIngress{Namespace: in.Namespace, Name: in.Name}
	for _, li := range in.Status.LoadBalancer.Ingress {
		if li.IP != "" {
			ki.Address = li.IP
			break
		}
	}
	seen := map[string]bool{}
	for _, r := range in.Spec.Rules {
		h := strings.ToLower(r.Host)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		ki.Hosts = append(ki.Hosts, h)
		if strings.HasSuffix(h, "."+k8sZone) {
			ki.URLs = append(ki.URLs, "https://"+h)
		}
	}
	sort.Strings(ki.URLs)
	return ki
}
