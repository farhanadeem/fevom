package vmmctl

const APIVersion = 1

const Flag = "--control-fd"

const (
	MethodPause  = "pause"
	MethodResume = "resume"
	MethodStats  = "stats"
	MethodStop   = "stop"
	MethodKill   = "kill"

	MethodBudget = "budget"

	NotifyReset = "reset"
	NotifySpace = "space"
)

const (
	CodeFailed   = -32002
	CodeStopping = -32006
)

const (
	StateRunning  = "running"
	StatePaused   = "paused"
	StateStopping = "stopping"
)

type Empty struct{}

type PauseResult struct {
	State      string `json:"state"`
	Changed    bool   `json:"changed"`
	VCPUs      int    `json:"vcpus"`
	TookNS     uint64 `json:"took_ns"`
	PausedAtNS uint64 `json:"paused_at_ns"`
}

type ResumeResult struct {
	State         string `json:"state"`
	Changed       bool   `json:"changed"`
	VCPUs         int    `json:"vcpus"`
	TookNS        uint64 `json:"took_ns"`
	OffsetNS      uint64 `json:"offset_ns"`
	TotalOffsetNS uint64 `json:"total_offset_ns"`
}

type Stats struct {
	APIVersion    int                          `json:"api_version"`
	Build         string                       `json:"build"`
	State         string                       `json:"state"`
	AtNS          uint64                       `json:"at_ns"`
	Pauses        uint64                       `json:"pauses"`
	TotalOffsetNS uint64                       `json:"total_offset_ns"`
	PausedAtNS    uint64                       `json:"paused_at_ns,omitempty"`
	Resets        uint64                       `json:"resets"`
	VCPUs         []VCPU                       `json:"vcpus"`
	Memory        Memory                       `json:"memory"`
	Devices       map[string]map[string]uint64 `json:"devices,omitempty"`
	Windows       *Windows                     `json:"windows,omitempty"`
}

type VCPU struct {
	CPUNS uint64 `json:"cpu_ns"`
	Exits uint64 `json:"exits"`
}

type Memory struct {
	GuestBytes      uint64 `json:"guest_bytes"`
	FootprintBytes  uint64 `json:"footprint_bytes"`
	ResidentBytes   uint64 `json:"resident_bytes"`
	CompressedBytes uint64 `json:"compressed_bytes"`
	ReleasedBytes   uint64 `json:"released_bytes"`
	ReleasedRanges  uint64 `json:"released_ranges"`
}

type Windows struct {
	DataDisk  *Window `json:"data_disk,omitempty"`
	FileShare *Window `json:"file_share,omitempty"`
}

const (
	WindowOn     = "on"
	WindowOff    = "off"
	WindowFailed = "failed"
)

type Window struct {
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	SizeBytes  uint64 `json:"size_bytes"`
	GuestAddr  uint64 `json:"guest_addr,omitempty"`
	Reads      uint64 `json:"reads,omitempty"`
	Mappings   uint64 `json:"mappings,omitempty"`
	Unmappings uint64 `json:"unmappings,omitempty"`
	Live       uint64 `json:"live,omitempty"`
	Faults     uint64 `json:"faults,omitempty"`
}

type Reset struct {
	Count uint64 `json:"count"`
	AtNS  uint64 `json:"at_ns"`
}

type Baseline struct {
	AvailableBytes uint64 `json:"available_bytes"`
	ImageBytes     uint64 `json:"image_bytes"`
	MarginBytes    uint64 `json:"margin_bytes"`
}

type BudgetResult struct {
	HeldRequests uint64 `json:"held_requests"`
}

type Space struct {
	AvailableBytes uint64 `json:"available_bytes"`
	ImageBytes     uint64 `json:"image_bytes"`
	HeldRequests   uint64 `json:"held_requests"`
	AtNS           uint64 `json:"at_ns"`
}
