package k8s

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
)

var testRanges = Ranges{Pods: netip.MustParsePrefix("198.19.16.0/20"), Services: netip.MustParsePrefix("198.19.32.0/20"), DNS: netip.MustParseAddr("198.19.32.10")}

func TestK3sArgs(t *testing.T) {
	a := K3sArgs(testRanges, "/run/docker/containerd/containerd.sock", Options{})
	line := strings.Join(a, " ")
	for _, w := range []string{"server --data-dir /var/lib/rancher/k3s", "--container-runtime-endpoint unix:///run/docker/containerd/containerd.sock",
		"--cluster-cidr 198.19.16.0/20", "--service-cidr 198.19.32.0/20", "--cluster-dns 198.19.32.10", "--flannel-backend host-gw",
		"--disable servicelb", "--disable metrics-server", "--write-kubeconfig-mode 600"} {
		if !strings.Contains(line, w) {
			t.Errorf("%q lacks %q", line, w)
		}
	}
	if strings.Contains(line, "kubelet-arg") {
		t.Errorf("a kubelet flag: %q", line)
	}
	on := strings.Join(K3sArgs(testRanges, "/s", Options{MetricsServer: true}), " ")
	if strings.Contains(on, "metrics-server") {
		t.Errorf("metrics-server disabled when asked for: %q", on)
	}
}

func TestKubeletDropins(t *testing.T) {
	d, err := KubeletDropins(nil)
	if err != nil || len(d) != 1 {
		t.Fatalf("%v %v", d, err)
	}
	var m map[string]any
	json.Unmarshal(d[FevDropin], &m)
	if m["seccompDefault"] != true || m["failSwapOn"] != false || m["kind"] != "KubeletConfiguration" || m["apiVersion"] != "kubelet.config.k8s.io/v1beta1" {
		t.Errorf("fev's %v", m)
	}
	user := json.RawMessage(`{"maxPods": 50, "seccompDefault": false, "kind": "Other"}`)
	d, err = KubeletDropins(user)
	if err != nil || len(d) != 2 {
		t.Fatalf("%v %v", d, err)
	}
	m = nil
	json.Unmarshal(d[UserDropin], &m)
	if m["maxPods"] != float64(50) || m["seccompDefault"] != false || m["kind"] != "KubeletConfiguration" {
		t.Errorf("user's %v", m)
	}
	if got := UserChanged(user); !reflect.DeepEqual(got, []string{"seccompDefault"}) {
		t.Errorf("changed %v", got)
	}
	if !slices.IsSorted([]string{FevDropin, UserDropin}) || !(strings.Compare("00-k3s-defaults.conf", FevDropin) < 0) {
		t.Error("the drop-ins are not read in fev's order")
	}
	if _, err := KubeletDropins(json.RawMessage(`[1]`)); err == nil {
		t.Error("an array was taken")
	}
}

func TestRegistryHosts(t *testing.T) {
	h := RegistryHosts("127.0.0.1:6449")
	if !strings.Contains(h["_default/hosts.toml"], `[host."http://127.0.0.1:6449"]`) || strings.Contains(h["_default/hosts.toml"], "server") {
		t.Errorf("_default %q", h["_default/hosts.toml"])
	}
	d := h["docker.io/hosts.toml"]
	if !strings.Contains(d, `server = "https://registry-1.docker.io"`) || !strings.Contains(d, `capabilities = ["pull", "resolve"]`) {
		t.Errorf("docker.io %q", d)
	}
	n := NeverHosts("127.0.0.1:6449")
	if len(n) != 1 || !strings.Contains(n["_default/hosts.toml"], `server = "http://127.0.0.1:6449"`) || strings.Contains(n["_default/hosts.toml"], "docker.io") {
		t.Errorf("never %v", n)
	}
}

func TestK3sOwnLocalStorageIsOff(t *testing.T) {
	line := strings.Join(K3sArgs(Ranges{}, "/run/c.sock", Options{}), " ")
	if !strings.Contains(line, "--disable local-storage") {
		t.Errorf("%q lacks --disable local-storage", line)
	}
}
