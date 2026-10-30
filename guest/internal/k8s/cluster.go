package k8s

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

type Ranges struct {
	Pods, Services netip.Prefix
	DNS            netip.Addr
}

type Options struct {
	MetricsServer bool
}

const DataDir = "/var/lib/rancher/k3s"

func K3sArgs(r Ranges, containerd string, o Options) []string {
	args := []string{
		"server",
		"--data-dir", DataDir,
		"--container-runtime-endpoint", "unix://" + containerd,
		"--cluster-cidr", r.Pods.String(),
		"--service-cidr", r.Services.String(),
		"--cluster-dns", r.DNS.String(),
		"--flannel-backend", "host-gw",
		"--disable", "servicelb",
		"--disable", "local-storage",
		"--write-kubeconfig-mode", "600",
	}
	if !o.MetricsServer {
		args = append(args, "--disable", "metrics-server")
	}
	return args
}

const KubeletDir = DataDir + "/agent/etc/kubelet.conf.d"

const (
	FevDropin  = "10-fev.conf"
	UserDropin = "90-fev-user.conf"
)

var fevKubelet = map[string]any{
	"seccompDefault":              true,
	"failSwapOn":                  false,
	"imageGCHighThresholdPercent": 85,
	"imageGCLowThresholdPercent":  80,
}

func KubeletDropins(user json.RawMessage) (map[string][]byte, error) {
	out := map[string][]byte{}
	b, err := dropin(fevKubelet)
	if err != nil {
		return nil, err
	}
	out[FevDropin] = b
	if len(bytes.TrimSpace(user)) > 0 && string(bytes.TrimSpace(user)) != "null" {
		var m map[string]any
		if err := json.Unmarshal(user, &m); err != nil {
			return nil, fmt.Errorf("kubernetes.kubelet is not an object: %w", err)
		}
		if len(m) > 0 {
			for _, k := range []string{"apiVersion", "kind"} {
				delete(m, k)
			}
			if out[UserDropin], err = dropin(m); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func dropin(fields map[string]any) ([]byte, error) {
	m := map[string]any{"apiVersion": "kubelet.config.k8s.io/v1beta1", "kind": "KubeletConfiguration"}
	for k, v := range fields {
		m[k] = v
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func UserChanged(user json.RawMessage) []string {
	var m map[string]any
	if json.Unmarshal(user, &m) != nil {
		return nil
	}
	var out []string
	for k := range m {
		if _, ok := fevKubelet[k]; ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func RegistryHosts(registry string) map[string]string {
	fev := fmt.Sprintf("[host.%q]\n  capabilities = [\"pull\", \"resolve\"]\n", "http://"+registry)
	head := "# written by fev-init: fev's registry over Docker's images first\n"
	return map[string]string{
		"_default/hosts.toml":  head + fev,
		"docker.io/hosts.toml": head + "server = \"https://registry-1.docker.io\"\n\n" + fev,
	}
}

func NeverHosts(registry string) map[string]string {
	return map[string]string{
		"_default/hosts.toml": "# written by fev-init: fev's registry alone (k8s.image.pull)\n" +
			fmt.Sprintf("server = %q\n", "http://"+registry),
	}
}

func Words(args []string) string { return strings.Join(args, "\n") + "\n" }
