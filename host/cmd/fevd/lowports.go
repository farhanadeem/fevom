package main

import (
	"fev/host/daemon"
	"fev/host/docker/forward"
)

func lowPorts(network *daemon.Network) forward.LowBinder {
	if network == nil {
		return nil
	}
	return network
}
