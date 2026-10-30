package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const (
	containerdName          = "/usr/bin/containerd"
	engineFevDir            = "/run/fev"
	containerdReal          = engineFevDir + "/containerd"
	containerdConfig        = engineFevDir + "/containerd.toml"
	dockerdContainerdConfig = "/run/docker/containerd/containerd.toml"
)

const (
	containerdSocket    = "/run/docker/containerd/containerd.sock"
	containerdBase      = engineFevDir + "/containerd-base.toml"
	containerdConfigCRI = engineFevDir + "/containerd-cri.toml"
)

const containerdBaseText = `# written by fev-init: the engine's containerd
version = 2
root = '/var/lib/docker/containerd/daemon'
state = '/run/docker/containerd/daemon'

[debug]
address = '/run/docker/containerd/containerd-debug.sock'

[grpc]
address = '` + containerdSocket + `'
max_recv_message_size = 16777216
max_send_message_size = 16777216
`

const containerdCommonText = `
[plugins.'io.containerd.gc.v1.scheduler']
pause_threshold = 0.0001
deletion_threshold = 1

[plugins.'io.containerd.snapshotter.v1.overlayfs']
mount_options = ['index=on', 'nfs_export=on']
`

const containerdConfigText = `# written by fev-init
version = 4
imports = ['` + containerdBase + `']
disabled_plugins = ['io.containerd.grpc.v1.cri', 'io.containerd.cri.v1.images', 'io.containerd.cri.v1.runtime', 'io.containerd.podsandbox.controller.v1.podsandbox']
` + containerdCommonText

const containerdCRIText = `# written by fev-init: the engine's containerd with the CRI plugins
version = 4
imports = ['` + containerdBase + `']
` + containerdCommonText + `
[plugins.'io.containerd.cri.v1.images']
snapshotter = 'overlayfs'

[plugins.'io.containerd.cri.v1.images'.pinned_images]
sandbox = 'docker.io/rancher/mirrored-pause:3.10.2'

[plugins.'io.containerd.cri.v1.images'.registry]
config_path = '` + k8sRunDir + `/certs.d'

[plugins.'io.containerd.cri.v1.runtime']
stats_collect_period = '10s'

[plugins.'io.containerd.cri.v1.runtime'.cni]
bin_dirs = ['/usr/lib/k3s/data/cni']
conf_dir = '/var/lib/rancher/k3s/agent/etc/cni/net.d'
`

func initPath() string {
	if path, err := os.Executable(); err == nil {
		return path
	}
	return "/init"
}

func isContainerdName(args []string) bool {
	return len(args) > 0 && filepath.Base(args[0]) == filepath.Base(containerdName)
}

func containerdArgs(args []string) []string {
	out := append([]string{containerdReal}, args[1:]...)
	for i := 1; i+1 < len(out); i++ {
		if out[i] == "--config" && out[i+1] == dockerdContainerdConfig {
			out[i+1] = containerdConfig
		}
	}
	return out
}

func execContainerd(args []string) error {
	argv := containerdArgs(args)
	if _, err := os.Stat(containerdConfig); err != nil {
		argv = append([]string{containerdReal}, args[1:]...)
	}
	return syscall.Exec(containerdReal, argv, os.Environ())
}

func prepareContainerd(run string) error {
	dir := filepath.Join(run, filepath.Base(engineFevDir))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.Base(containerdReal)), nil, 0o755); err != nil {
		return err
	}
	for name, text := range map[string]string{containerdBase: containerdBaseText, containerdConfig: containerdConfigText, containerdConfigCRI: containerdCRIText} {
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(name)), []byte(text), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func newEngineRun(run string, started func()) error {
	if err := os.RemoveAll(run); err != nil {
		return err
	}
	if err := os.MkdirAll(run, 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(run, "tmp"), 0o1777); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Join(run, "tmp"), 0o1777|os.ModeSticky); err != nil {
		return err
	}
	if err := prepareContainerd(run); err != nil {
		return fmt.Errorf("write containerd's configuration: %w", err)
	}
	started()
	return nil
}
