//go:build !darwin || !cgo

package macnet

import (
	"errors"
	"log/slog"
)

func Start(*slog.Logger) (*Watcher, error) {
	return nil, errors.New("macnet: watching the network needs macOS and cgo")
}
