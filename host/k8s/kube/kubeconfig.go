package kube

import "fev/host/k8s/kubeconfig"

const ContextName = kubeconfig.Context

func MergeContext(path, raw, apiAddress string) error {
	if apiAddress == "" {
		apiAddress = APIAddress
	}
	_, err := kubeconfig.Merge(path, "", raw, apiAddress)
	return err
}

func RemoveContext(path string) error {
	_, err := kubeconfig.Remove("", path)
	return err
}
