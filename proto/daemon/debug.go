package daemon

const (
	MethodDebugStart  = "debug.start"
	MethodDebugStatus = "debug.status"
	MethodDebugReset  = "debug.reset"
)

const (
	DebugReasonToolsUnavailable = "debug.tools_unavailable"
	DebugReasonTargetGone       = "debug.target_gone"
)

const (
	KeyDebugCacheURL = "debug.cache_url"
	KeyDebugIndexURL = "debug.index_url"
)

type DebugStartParams struct {
	Target   string   `json:"target"`
	Context  string   `json:"context,omitempty"`
	Kind     string   `json:"kind,omitempty"`
	ReadOnly bool     `json:"read_only,omitempty"`
	Argv     []string `json:"argv,omitempty"`
	Env      []string `json:"env,omitempty"`
	Dir      string   `json:"dir,omitempty"`
	TTY      bool     `json:"tty,omitempty"`
	StdinTTY bool     `json:"stdin_tty,omitempty"`
	Rows     int      `json:"rows,omitempty"`
	Cols     int      `json:"cols,omitempty"`
	Terminfo []byte   `json:"terminfo,omitempty"`
}

type DebugStarted struct {
	ID       uint64   `json:"id"`
	Token    string   `json:"token"`
	Kind     string   `json:"kind"`
	TargetID string   `json:"target_id"`
	Name     string   `json:"name"`
	Image    string   `json:"image,omitempty"`
	ReadOnly bool     `json:"read_only"`
	Notes    []string `json:"notes,omitempty"`
}

type DebugStatus struct {
	Exists       bool           `json:"exists"`
	Bytes        int64          `json:"bytes"`
	Paths        int            `json:"paths"`
	Packages     []DebugPackage `json:"packages,omitempty"`
	DefaultSet   string         `json:"default_set,omitempty"`
	DefaultWhole bool           `json:"default_whole"`
	IndexDate    string         `json:"index_date,omitempty"`
	Sessions     int            `json:"sessions"`
}

type DebugPackage struct {
	Name      string   `json:"name"`
	Attr      string   `json:"attr,omitempty"`
	StorePath string   `json:"store_path"`
	Commands  []string `json:"commands,omitempty"`
	Default   bool     `json:"default,omitempty"`
}

type DebugResetParams struct {
	Force bool `json:"force,omitempty"`
}

type DebugReset struct {
	FreedBytes int64 `json:"freed_bytes"`
}
