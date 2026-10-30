package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestContainerdIsGivenInitsConfigurationInPlaceOfDockerds(t *testing.T) {
	for _, c := range []struct {
		name     string
		in, want []string
	}{
		{"as dockerd starts it",
			[]string{"/usr/bin/containerd", "--config", dockerdContainerdConfig},
			[]string{containerdReal, "--config", containerdConfig}},
		{"with more flags around it",
			[]string{"containerd", "--log-level", "debug", "--config", dockerdContainerdConfig, "--address", "/x"},
			[]string{containerdReal, "--log-level", "debug", "--config", containerdConfig, "--address", "/x"}},
		{"another file is not init's to replace",
			[]string{"containerd", "--config", "/etc/containerd/config.toml"},
			[]string{containerdReal, "--config", "/etc/containerd/config.toml"}},
		{"a subcommand",
			[]string{"containerd", "--version"},
			[]string{containerdReal, "--version"}},
		{"a flag with no value",
			[]string{"containerd", "--config"},
			[]string{containerdReal, "--config"}},
		{"no arguments",
			[]string{"containerd"},
			[]string{containerdReal}},
	} {
		if got := containerdArgs(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestOnlyTheNameContainerdIsNotInit(t *testing.T) {
	for args, want := range map[string]bool{
		"/usr/bin/containerd --config x": true,
		"containerd":                     true,
		"/init":                          false,
		"/sbin/fev-init":                 false,
		"/usr/bin/containerd-shim-runc-v2 -namespace moby": false,
		"": false,
	} {
		if got := isContainerdName(strings.Fields(args)); got != want {
			t.Errorf("%q: %v, want %v", args, got, want)
		}
	}
}

func TestContainerdsConfigurationImportsDockerdsAndSetsTheCollector(t *testing.T) {
	run := t.TempDir()
	if err := prepareContainerd(run); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(run, "fev", "containerd")); err != nil || st.Size() != 0 {
		t.Errorf("the mount point of the real containerd: %v, %v", st, err)
	}
	base, err := os.ReadFile(filepath.Join(run, "fev", "containerd-base.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"root = '/var/lib/docker/containerd/daemon'\n", "state = '/run/docker/containerd/daemon'\n", "address = '/run/docker/containerd/containerd.sock'\n"} {
		if !strings.Contains(string(base), want) {
			t.Errorf("the base lacks %q:\n%s", want, base)
		}
	}
	common := []string{
		"version = 4\n",
		"imports = ['/run/fev/containerd-base.toml']\n",
		"[plugins.'io.containerd.gc.v1.scheduler']\npause_threshold = 0.0001\ndeletion_threshold = 1\n",
		"[plugins.'io.containerd.snapshotter.v1.overlayfs']\nmount_options = ['index=on', 'nfs_export=on']\n",
	}
	for file, more := range map[string][]string{
		"containerd.toml": {"disabled_plugins = ['io.containerd.grpc.v1.cri', 'io.containerd.cri.v1.images', 'io.containerd.cri.v1.runtime', 'io.containerd.podsandbox.controller.v1.podsandbox']\n"},
		"containerd-cri.toml": {"sandbox = 'docker.io/rancher/mirrored-pause:3.10.2'\n", "config_path = '/run/fev/k8s/certs.d'\n",
			"bin_dirs = ['/usr/lib/k3s/data/cni']\n", "[plugins.'io.containerd.cri.v1.runtime']\nstats_collect_period = '10s'\n"},
	} {
		data, err := os.ReadFile(filepath.Join(run, "fev", file))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range append(append([]string{}, common...), more...) {
			if !strings.Contains(string(data), want) {
				t.Errorf("%s lacks %q:\n%s", file, want, data)
			}
		}
		if file == "containerd-cri.toml" && strings.Contains(string(data), "disabled_plugins") {
			t.Errorf("%s disables plugins", file)
		}
	}
	if err := prepareContainerd(run); err == nil {
		t.Error("a second prepare in the same directory did not fail")
	}
}

func TestTheEnginesRunIsMadeWithFevomsRootInIt(t *testing.T) {
	run := filepath.Join(t.TempDir(), "run")
	writeCA := func() {
		ca := filepath.Join(run, "fev", "https", "ca.pem")
		if err := os.MkdirAll(filepath.Dir(ca), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ca, []byte("root"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for start := 1; start <= 2; start++ {
		if err := newEngineRun(run, writeCA); err != nil {
			t.Fatalf("start %d: %v", start, err)
		}
		for _, f := range []string{"fev/containerd.toml", "fev/containerd", "fev/https/ca.pem"} {
			if _, err := os.Stat(filepath.Join(run, f)); err != nil {
				t.Errorf("start %d: %v", start, err)
			}
		}
		if st, err := os.Stat(filepath.Join(run, "tmp")); err != nil || st.Mode()&os.ModeSticky == 0 || st.Mode().Perm() != 0o777 {
			t.Errorf("start %d: the engine's /tmp: %v %v", start, st, err)
		}
	}
}
