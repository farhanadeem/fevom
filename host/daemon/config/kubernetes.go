package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	api "fev/proto/daemon"
)

var together = []func(get func(string) any, asked map[string]any) error{
	clusterBesideDocker,
}

func init() {
	checks[api.KeyKubernetesImagesURL] = checkImagesURL
	checks[api.KeyKubernetesKubelet] = checkKubelet
}

func clusterBesideDocker(get func(string) any, asked map[string]any) error {
	on, _ := get(api.KeyKubernetesEnabled).(bool)
	docker, _ := get(api.KeyDockerEnabled).(bool)
	if !on || docker {
		return nil
	}
	if _, ok := asked[api.KeyKubernetesEnabled]; ok {
		return &Error{Key: api.KeyKubernetesEnabled, Reason: "is refused while docker.enabled is false: the cluster runs beside the Docker engine and its images are Docker's"}
	}
	return &Error{Key: api.KeyDockerEnabled, Reason: "is refused while kubernetes.enabled is true: the cluster runs beside the Docker engine; turn Kubernetes off first"}
}

func checkImagesURL(v any) (any, error) {
	s := strings.TrimSpace(v.(string))
	if s == "" {
		return "", nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, errors.New("is not a URL: " + s)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		if u.Hostname() == "" {
			return nil, errors.New("names no host")
		}
	case "file":
		if u.Host != "" && u.Host != "localhost" || !strings.HasPrefix(u.Path, "/") {
			return nil, errors.New("is a file URL of no absolute Mac path (file:///Users/...)")
		}
	case "":
		return nil, fmt.Errorf("needs a scheme: https://%s, or file:///PATH", s)
	default:
		return nil, fmt.Errorf("has the scheme %q; the archive's URL is https:// or file://", u.Scheme)
	}
	if u.Fragment != "" {
		return nil, errors.New("has a fragment")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	return u.String(), nil
}

func checkKubelet(v any) (any, error) {
	m := v.(map[string]any)
	var unknown, wrong []string
	for k, val := range m {
		if k == "apiVersion" || k == "kind" {
			return nil, fmt.Errorf("sets %s, which fev writes; give the fields alone, as {\"maxPods\": 50}", k)
		}
		want, ok := kubeletFields[k]
		if !ok {
			unknown = append(unknown, k)
			continue
		}
		if got := jsonKind(val); got != want {
			wrong = append(wrong, fmt.Sprintf("%s (a %s, not a %s)", k, want, got))
		}
	}
	sort.Strings(unknown)
	sort.Strings(wrong)
	switch {
	case len(unknown) > 0:
		return nil, fmt.Errorf("has %s, not a field of the kubelet's KubeletConfiguration (k8s.io/kubelet %s)", strings.Join(unknown, ", "), kubeletVersion)
	case len(wrong) > 0:
		return nil, fmt.Errorf("has %s", strings.Join(wrong, ", "))
	}
	return m, nil
}

func jsonKind(v any) string {
	switch v.(type) {
	case bool:
		return "bool"
	case float64, int, int64, json.Number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}
