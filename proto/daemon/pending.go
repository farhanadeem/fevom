package daemon

import (
	"embed"

	"fev/proto/contract"
)

//go:embed fixtures/pending/*.json
var pendingFiles embed.FS

var Pending = map[string]contract.Types{}

var PendingKeys = []string{
	KeyFilesDAXWindowMiB,
}

func PendingFixtures() ([]Fixture, error) { return contract.Load(pendingFiles, "fixtures/pending") }
