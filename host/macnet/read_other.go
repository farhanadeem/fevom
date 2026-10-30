//go:build !darwin

package macnet

import "errors"

func readMac() (Snapshot, error) {
	return Snapshot{}, errors.New("macnet: reading the network needs macOS")
}
