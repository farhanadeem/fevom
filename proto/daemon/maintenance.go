package daemon

import "context"

const (
	MethodMemoryReclaim = "memory.reclaim"

	MethodStorageCheck = "storage.check"

	PhaseScrub = "scrub"
)

type ReclaimParams struct {
	FloorMiB int `json:"floor_mib,omitempty"`
}

const (
	ReclaimSkippedFloor    = "floor"
	ReclaimSkippedPressure = "pressure"
)

type Reclaimed struct {
	FileBeforeBytes uint64  `json:"file_before_bytes"`
	FileAfterBytes  uint64  `json:"file_after_bytes"`
	ReclaimedBytes  uint64  `json:"reclaimed_bytes"`
	FloorMiB        int     `json:"floor_mib"`
	Compacted       bool    `json:"compacted,omitempty"`
	Skipped         string  `json:"skipped,omitempty"`
	DurationMS      float64 `json:"duration_ms"`
}

type StorageCheckParams struct {
	Follow bool `json:"follow,omitempty"`
}

type StorageChecked struct {
	Auto          bool   `json:"auto,omitempty"`
	ReadOnly      bool   `json:"read_only,omitempty"`
	ScrubbedBytes int64  `json:"scrubbed_bytes"`
	Corrected     uint64 `json:"corrected"`
	Uncorrectable uint64 `json:"uncorrectable"`
}

func (c *Client) MemoryReclaim(ctx context.Context, p ReclaimParams) (*Reclaimed, error) {
	var r Reclaimed
	return &r, c.conn.Call(ctx, MethodMemoryReclaim, p, &r)
}

func (c *Client) StorageCheck(ctx context.Context, p StorageCheckParams) (*Operation, error) {
	var r Operation
	return &r, c.conn.Call(ctx, MethodStorageCheck, p, &r)
}
