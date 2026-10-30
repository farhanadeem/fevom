package agent

import (
	"embed"

	"fev/proto/contract"
)

//go:embed fixtures/pending
var pendingFiles embed.FS

var Pending = map[string]contract.Types{}

func PendingFixtures() ([]Fixture, error) { return contract.Load(pendingFiles, "fixtures/pending") }
