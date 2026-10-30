package main

import (
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed k8s-local-storage.yaml
var k8sLocalStorage []byte

var k8sSkipped = []string{"gateway-api-crd.yaml"}

func k8sManifests() error {
	return writeK8sManifests(filepath.Join(k8sData, "k3s", "server", "manifests"))
}

func writeK8sManifests(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range k8sSkipped {
		if err := os.WriteFile(filepath.Join(dir, name+".skip"), nil, 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, "fev-local-storage.yaml"), k8sLocalStorage, 0o644)
}
