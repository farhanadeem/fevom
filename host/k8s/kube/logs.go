package kube

import (
	"context"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"fev/host/k8s"
	api "fev/proto/daemon"
)

const defaultContainer = "kubectl.kubernetes.io/default-container"

func container(ctx context.Context, cs kubernetes.Interface, ns, pod, name string) (*corev1.Pod, string, error) {
	p, err := cs.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return nil, "", apiError(err)
	}
	all := append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...)
	all = append(all, ephemeral(p)...)
	if name != "" {
		for _, c := range all {
			if c.Name == name {
				return p, name, nil
			}
		}
		return nil, "", k8s.Errorf(api.CodeFailed, "container %q is not in pod %s/%s", name, ns, pod)
	}
	if d := p.Annotations[defaultContainer]; d != "" {
		for _, c := range p.Spec.Containers {
			if c.Name == d {
				return p, d, nil
			}
		}
	}
	if len(p.Spec.Containers) == 1 {
		return p, p.Spec.Containers[0].Name, nil
	}
	var names []string
	for _, c := range p.Spec.Containers {
		names = append(names, c.Name)
	}
	return nil, "", k8s.Errorf(api.CodeFailed, "pod %s/%s has %d containers (%s): name one", ns, pod, len(names), strings.Join(names, ", "))
}

func ephemeral(p *corev1.Pod) []corev1.Container {
	var out []corev1.Container
	for _, e := range p.Spec.EphemeralContainers {
		out = append(out, corev1.Container(e.EphemeralContainerCommon))
	}
	return out
}

func (c *Cluster) Logs(ctx context.Context, p api.K8sLogsParams, text func(string)) error {
	cs, _, err := c.clients()
	if err != nil {
		return err
	}
	_, name, err := container(ctx, cs, p.Namespace, p.Pod, p.Container)
	if err != nil {
		return err
	}
	opts := &corev1.PodLogOptions{Container: name, Follow: p.Follow, Previous: p.Previous, Timestamps: p.Timestamps}
	if p.TailLines > 0 {
		n := int64(p.TailLines)
		opts.TailLines = &n
	}
	rc, err := cs.CoreV1().Pods(p.Namespace).GetLogs(p.Pod, opts).Stream(ctx)
	if err != nil {
		return apiError(err)
	}
	defer rc.Close()
	return chunks(ctx, rc, text)
}

func chunks(ctx context.Context, r io.Reader, text func(string)) error {
	buf := make([]byte, 32<<10)
	held := 0
	for {
		n, err := r.Read(buf[held:])
		n += held
		cut := n
		for i := n - 1; i >= 0 && i >= n-utf8.UTFMax; i-- {
			if utf8.RuneStart(buf[i]) {
				if !utf8.FullRune(buf[i:n]) {
					cut = i
				}
				break
			}
		}
		if err != nil {
			cut = n
		}
		if cut > 0 {
			text(string(buf[:cut]))
		}
		held = copy(buf, buf[cut:n])
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return apiError(err)
		}
	}
}
