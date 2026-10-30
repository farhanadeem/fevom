package agent

import "encoding/json"

const (
	Port       = 1024
	HostPort   = 1025
	DockerPort = 2375
)

const APIVersion = 1

const (
	MethodHello          = "agent.hello"
	MethodPing           = "agent.ping"
	MethodInfo           = "system.info"
	MethodBootTrace      = "system.boot_trace"
	MethodShutdown       = "system.shutdown"
	MethodLogSubscribe   = "log.subscribe"
	MethodLogUnsubscribe = "log.unsubscribe"
	MethodFetch          = "diag.fetch"
	MethodPingHost       = "diag.ping"

	MethodEngineStatus      = "engine.status"
	MethodEngineStart       = "engine.start"
	MethodEngineStop        = "engine.stop"
	MethodEngineRestart     = "engine.restart"
	MethodEngineSubscribe   = "engine.subscribe"
	MethodEngineUnsubscribe = "engine.unsubscribe"

	MethodStorageInfo   = "storage.info"
	MethodStorageTrim   = "storage.trim"
	MethodStorageSync   = "storage.sync"
	MethodStorageBudget = "storage.budget"

	MethodExecStart  = "exec.start"
	MethodExecResize = "exec.resize"
	MethodExecSignal = "exec.signal"
	MethodExecClose  = "exec.close"

	MethodFileShareWatch  = "fileshare.watch"
	MethodFileShareBind   = "fileshare.bind"
	MethodEngineConfigure = "engine.configure"
	MethodStreamRefuse    = "stream.refuse"

	MethodTimeSet          = "time.set"
	MethodMemoryReclaim    = "memory.reclaim"
	MethodStatsSubscribe   = "stats.subscribe"
	MethodStatsUnsubscribe = "stats.unsubscribe"

	NotifyStatsSample    = "stats.sample"
	NotifyFileShareState = "fileshare.state"

	NotifyStreamRequest = "stream.request"

	NotifyLogEntry         = "log.entry"
	NotifyLogBacklogEnd    = "log.backlog_end"
	NotifyEngineState      = "engine.state"
	NotifyExecExit         = "exec.exit"
	NotifyShutdownProgress = "system.shutdown_progress"
)

const (
	CodeHelloRequired         = -32000
	CodeVersionMismatch       = -32001
	CodeFailed                = -32002
	CodeBuildMismatch         = -32003
	CodeNotImplemented        = -32007
	CodeNoMachine             = -32008
	CodeBusy                  = -32009
	CodeDebugToolsUnavailable = -32010
)

type HelloParams struct {
	APIVersion int    `json:"api_version"`
	Build      string `json:"build,omitempty"`
	Client     string `json:"client"`
}

type HelloResult struct {
	APIVersion   int    `json:"api_version"`
	AgentVersion string `json:"agent_version"`
	Kernel       string `json:"kernel"`
}

type BuildMismatch struct {
	AgentBuild  string `json:"agent_build"`
	ClientBuild string `json:"client_build"`
}

type Empty struct{}

type Info struct {
	Kernel            string      `json:"kernel"`
	UptimeMS          int64       `json:"uptime_ms"`
	CPUs              int         `json:"cpus"`
	MemTotalKB        int64       `json:"mem_total_kb"`
	MemAvailableKB    int64       `json:"mem_available_kb"`
	Interfaces        []Interface `json:"interfaces"`
	Mounts            []Mount     `json:"mounts"`
	CgroupControllers []string    `json:"cgroup_controllers"`
	FileShare         *FileShare  `json:"file_share,omitempty"`
	DirtyBytes        int64       `json:"dirty_bytes,omitempty"`
}

const (
	FeedConnected = "connected"
	FeedNone      = "none"
	FeedLost      = "lost"
	FeedMismatch  = "mismatch"
)

type FileShare struct {
	Tag           string   `json:"tag"`
	Target        string   `json:"target"`
	Device        bool     `json:"device"`
	Feed          string   `json:"feed"`
	FeedError     string   `json:"feed_error,omitempty"`
	Watching      int      `json:"watching"`
	Records       uint64   `json:"records"`
	Refused       uint64   `json:"refused"`
	Invalidations uint64   `json:"invalidations"`
	Unbound       []string `json:"unbound,omitempty"`
}

type FileShareState struct {
	FileShare
}

type SharePath struct {
	Target string
	Source string
}

var SharePaths = []SharePath{
	{"/Users", "/Users"},
	{"/Volumes", "/Volumes"},
	{"/private", "/private"},
	{"/Applications", "/Applications"},
	{"/Library", "/Library"},
	{"/System", "/System"},
	{"/opt/homebrew", "/opt/homebrew"},
	{"/usr/local", "/usr/local"},
	{"/tmp", "/private/tmp"},
	{"/var/folders", "/private/var/folders"},
}

const ShareTarget = "/mnt/mac"

var MachineSharePaths = []SharePath{
	{"/Users", "/Users"},
	{"/Volumes", "/Volumes"},
	{"/private", "/private"},
	{"/Applications", "/Applications"},
	{"/Library", "/Library"},
}

type BindTarget struct {
	Context string `json:"context"`
	Machine string `json:"machine,omitempty"`
}

type ShareBindParams struct {
	Targets []BindTarget `json:"targets,omitempty"`
}

type ShareBound struct {
	Bound    []string            `json:"bound"`
	Unbound  []string            `json:"unbound"`
	Machines []MachineShareBound `json:"machines,omitempty"`
}

type MachineShareBound struct {
	Machine string   `json:"machine"`
	Bound   []string `json:"bound"`
	Unbound []string `json:"unbound"`
}

type WatchParams struct {
	Paths []string `json:"paths"`
}

const (
	WatchOutside = "outside"
	WatchMissing = "missing"
	WatchFailed  = "failed"
)

type WatchResult struct {
	Watching int             `json:"watching"`
	Rejected []WatchRejected `json:"rejected,omitempty"`
}

type WatchRejected struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type EngineConfigureParams struct {
	Config json.RawMessage `json:"config"`
}

const (
	EngineConfigUnchanged = "unchanged"
	EngineConfigAccepted  = "accepted"
	EngineConfigRefused   = "refused"
)

type EngineConfigured struct {
	State         string `json:"state"`
	Error         string `json:"error,omitempty"`
	RestartNeeded bool   `json:"restart_needed,omitempty"`
}

const (
	StreamSSHAgent = "ssh-agent"
	StreamMac      = "mac"
)

const MaxStreamRequests = 64

const MaxStreams = 256

type StreamRequest struct {
	Token   string `json:"token"`
	Service string `json:"service"`
	Machine string `json:"machine,omitempty"`
	UID     *int   `json:"uid,omitempty"`
}

type StreamRefuseParams struct {
	Token  string `json:"token"`
	Reason string `json:"reason,omitempty"`
}

type Interface struct {
	Name      string   `json:"name"`
	Up        bool     `json:"up"`
	Addresses []string `json:"addresses"`
}

type Mount struct {
	Source string `json:"source"`
	Target string `json:"target"`
	FSType string `json:"fstype"`
}

type BootTraceParams struct {
	Wait bool `json:"wait,omitempty"`
}

type BootTrace struct {
	InitStartMS   float64 `json:"init_start_ms"`
	ReadyMS       float64 `json:"ready_ms"`
	NowMS         float64 `json:"now_ms"`
	EngineStartMS float64 `json:"engine_start_ms,omitempty"`
	EngineReadyMS float64 `json:"engine_ready_ms,omitempty"`
	Stages        []Stage `json:"stages"`
}

type Stage struct {
	Name    string  `json:"name"`
	StartMS float64 `json:"start_ms"`
	EndMS   float64 `json:"end_ms"`
	Error   string  `json:"error,omitempty"`
	Skipped string  `json:"skipped,omitempty"`
}

const DefaultShutdownDeadlineMS = 15000

const MaxShutdownDeadlineMS = 10 * 60 * 1000

type ShutdownParams struct {
	Reboot     bool `json:"reboot,omitempty"`
	DeadlineMS int  `json:"deadline_ms,omitempty"`
}

const (
	ShutdownBegin    = "begin"
	ShutdownEngine   = "engine"
	ShutdownSessions = "sessions"
	ShutdownSync     = "sync"
	ShutdownUnmount  = "unmount"
	ShutdownOff      = "off"
	ShutdownDeadline = "deadline"
)

type ShutdownProgress struct {
	Phase string `json:"phase"`
	Error string `json:"error,omitempty"`
}

const (
	LogKernel        = "kernel"
	LogInit          = "init"
	LogEngine        = "engine"
	LogMachinePrefix = "machine:"
)

func MachineConsoleLog(name string) string { return LogMachinePrefix + name }

func MachineRuntimeLog(name string) string { return LogMachinePrefix + name + ":runtime" }

type LogSubscribeParams struct {
	Sources []string `json:"sources,omitempty"`
	Backlog bool     `json:"backlog,omitempty"`
}

type Subscription struct {
	Subscription uint64 `json:"subscription"`
}

type LogEntry struct {
	Subscription uint64  `json:"subscription"`
	Source       string  `json:"source"`
	TimeMS       float64 `json:"time_ms"`
	Level        int     `json:"level"`
	Text         string  `json:"text"`
}

type FetchParams struct {
	URL       string `json:"url"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

type FetchResult struct {
	Status     int     `json:"status"`
	Bytes      int64   `json:"bytes"`
	TLS        string  `json:"tls,omitempty"`
	ElapsedMS  float64 `json:"elapsed_ms"`
	RemoteAddr string  `json:"remote_addr"`
}

type PingParams struct {
	Address   string `json:"address"`
	Count     int    `json:"count,omitempty"`
	TTL       int    `json:"ttl,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

type PingResult struct {
	Sent        int     `json:"sent"`
	Received    int     `json:"received"`
	From        string  `json:"from,omitempty"`
	TTLExceeded bool    `json:"ttl_exceeded,omitempty"`
	AvgMS       float64 `json:"avg_ms"`
}

const EngineCmdlineParam = "fev.engine"

const (
	EngineStopped  = "stopped"
	EngineStarting = "starting"
	EngineReady    = "ready"
	EngineStopping = "stopping"
	EngineFailed   = "failed"
)

type EngineStatus struct {
	State     string  `json:"state"`
	PID       int     `json:"pid,omitempty"`
	Starts    int     `json:"starts"`
	StartedMS float64 `json:"started_ms,omitempty"`
	ReadyMS   float64 `json:"ready_ms,omitempty"`
	LastExit  string  `json:"last_exit,omitempty"`
	Error     string  `json:"error,omitempty"`
}

type EngineStopParams struct {
	GraceMS int `json:"grace_ms,omitempty"`
}

const DefaultEngineGraceMS = 10000

type EngineState struct {
	Subscription uint64 `json:"subscription"`
	EngineStatus
}

type StorageInfo struct {
	Device    string        `json:"device"`
	FSType    string        `json:"fstype"`
	FSID      string        `json:"fsid"`
	Layout    int           `json:"layout"`
	SizeBytes int64         `json:"size_bytes"`
	UsedBytes int64         `json:"used_bytes"`
	MountRung int           `json:"mount_rung"`
	ReadOnly  bool          `json:"read_only"`
	Steps     []MountStep   `json:"steps,omitempty"`
	Format    string        `json:"format,omitempty"`
	Errors    StorageErrors `json:"errors"`
	Quota     *StorageQuota `json:"quota,omitempty"`
	Swap      []SwapDevice  `json:"swap,omitempty"`
	Check     *StorageCheck `json:"check,omitempty"`
}

const (
	SwapZram = "zram"
	SwapDisk = "disk"
)

type SwapDevice struct {
	Kind        string `json:"kind"`
	Device      string `json:"device"`
	Priority    int    `json:"priority"`
	SizeBytes   int64  `json:"size_bytes"`
	UsedBytes   int64  `json:"used_bytes"`
	Algorithm   string `json:"algorithm,omitempty"`
	StoredBytes int64  `json:"stored_bytes,omitempty"`
}

type MountStep struct {
	Rung    int    `json:"rung"`
	Name    string `json:"name"`
	Error   string `json:"error,omitempty"`
	Skipped string `json:"skipped,omitempty"`
	Lost    string `json:"lost,omitempty"`
}

const (
	FormatNew      = "new"
	FormatRepeated = "repeated"
	FormatFinished = "finished"
)

const NoDataFilesystem = "no data filesystem"

const DataReadOnly = "the data filesystem is mounted read-only"

type StorageQuota struct {
	Limited         bool  `json:"limited"`
	LimitBytes      int64 `json:"limit_bytes"`
	ReferencedBytes int64 `json:"referenced_bytes"`
	ReservedBytes   int64 `json:"reserved_bytes"`
}

type StorageErrors struct {
	Write      uint64 `json:"write"`
	Read       uint64 `json:"read"`
	Flush      uint64 `json:"flush"`
	Corruption uint64 `json:"corruption"`
	Generation uint64 `json:"generation"`
}

type TrimParams struct {
	MinLenBytes uint64 `json:"min_len_bytes,omitempty"`
}

type BudgetParams struct {
	AvailableBytes int64 `json:"available_bytes"`
	MarginBytes    int64 `json:"margin_bytes"`
	ImageBytes     int64 `json:"image_bytes"`
	Unlimited      bool  `json:"unlimited,omitempty"`
	ReclaimBytes   int64 `json:"reclaim_bytes,omitempty"`
}

type Budget struct {
	Limited         bool    `json:"limited"`
	LimitBytes      int64   `json:"limit_bytes"`
	BudgetBytes     int64   `json:"budget_bytes"`
	ReferencedBytes int64   `json:"referenced_bytes"`
	ReservedBytes   int64   `json:"reserved_bytes"`
	UsedBytes       int64   `json:"used_bytes"`
	DurationMS      float64 `json:"duration_ms"`
	Deferred        bool    `json:"deferred,omitempty"`
}

func ReclaimableBytes(imageBytes, usedBytes, reservedBytes int64) int64 {
	if reservedBytes < 0 {
		reservedBytes = 0
	}
	if n := imageBytes - usedBytes - reservedBytes; n > 0 {
		return n
	}
	return 0
}

func BudgetBytes(availableBytes, marginBytes, imageBytes, usedBytes, reservedBytes int64) int64 {
	short := usedBytes - imageBytes
	if reservedBytes < 0 {
		reservedBytes = 0
	}
	if short < -reservedBytes {
		short = -reservedBytes
	}
	have := availableBytes - short
	b := have - marginBytes
	if b < CleanupBytes && marginBytes > 0 && have-CleanupBytes >= marginBytes/2 {
		return CleanupBytes
	}
	if b < 0 {
		return 0
	}
	return b
}

const CleanupBytes = 32 << 20

const DirtyCapBytes = 5 << 30 / 4

type Synced struct {
	DurationMS float64 `json:"duration_ms"`
}

type TrimResult struct {
	TrimmedBytes uint64  `json:"trimmed_bytes"`
	DurationMS   float64 `json:"duration_ms"`
}

const (
	ExecInit    = "init"
	ExecEngine  = "engine"
	ExecMachine = "machine"
	ExecDebug   = "debug"
)

type ExecStartParams struct {
	Context  string   `json:"context"`
	Argv     []string `json:"argv"`
	Env      []string `json:"env,omitempty"`
	Dir      string   `json:"dir,omitempty"`
	TTY      bool     `json:"tty,omitempty"`
	StdinTTY bool     `json:"stdin_tty,omitempty"`
	Rows     int      `json:"rows,omitempty"`
	Cols     int      `json:"cols,omitempty"`

	Machine   string `json:"machine,omitempty"`
	User      string `json:"user,omitempty"`
	Login     bool   `json:"login,omitempty"`
	Shell     bool   `json:"shell,omitempty"`
	Subsystem string `json:"subsystem,omitempty"`
	Copy      string `json:"copy,omitempty"`
	Terminfo  []byte `json:"terminfo,omitempty"`

	Debug *DebugTarget `json:"debug,omitempty"`
}

const SubsystemSFTP = "sftp"

type ExecStarted struct {
	ID    uint64        `json:"id"`
	Token string        `json:"token"`
	Debug *DebugStarted `json:"debug,omitempty"`
}

type ExecResizeParams struct {
	ID   uint64 `json:"id"`
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
}

type ExecSignalParams struct {
	ID     uint64 `json:"id"`
	Signal int    `json:"signal"`
}

type ExecID struct {
	ID uint64 `json:"id"`
}

type ExecExit struct {
	ID       uint64 `json:"id"`
	ExitCode int    `json:"exit_code"`
	Signal   int    `json:"signal,omitempty"`
	Error    string `json:"error,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Note     string `json:"note,omitempty"`
}

const DefaultTimeThresholdMS = 100

type TimeSetParams struct {
	UnixNS      int64 `json:"unix_ns"`
	ThresholdMS int   `json:"threshold_ms,omitempty"`
}

type TimeSet struct {
	Stepped     bool    `json:"stepped"`
	OffsetMS    float64 `json:"offset_ms"`
	ThresholdMS int     `json:"threshold_ms"`
}

const DefaultReclaimFloorMiB = 128

const ReclaimSlackMiB = 64

type ReclaimParams struct {
	FloorMiB  int  `json:"floor_mib,omitempty"`
	NoCompact bool `json:"no_compact,omitempty"`
	Running   bool `json:"running,omitempty"`
}

const (
	TrimIdleCPUPercent = 1.0
	TrimFloorMiB       = 16
)

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
	Running         int     `json:"running,omitempty"`
	TrimmedBytes    uint64  `json:"trimmed_bytes,omitempty"`
	ColdBytes       uint64  `json:"cold_bytes,omitempty"`
}

const (
	DefaultStatsIntervalMS = 2000
	MinStatsIntervalMS     = 250
)

type StatsSubscribeParams struct {
	IntervalMS int `json:"interval_ms,omitempty"`
}

const (
	StatsSystem    = "system"
	StatsService   = "service"
	StatsContainer = "container"
	StatsMachine   = "machine"
)

type StatsSample struct {
	Subscription   uint64       `json:"subscription"`
	TimeMS         float64      `json:"time_ms"`
	MemTotalKB     int64        `json:"mem_total_kb"`
	MemAvailableKB int64        `json:"mem_available_kb"`
	CachedKB       int64        `json:"cached_kb"`
	Groups         []StatsGroup `json:"groups"`
}

type StatsGroup struct {
	Kind            string `json:"kind"`
	Name            string `json:"name"`
	ID              string `json:"id,omitempty"`
	CPUUsec         uint64 `json:"cpu_usec"`
	MemoryBytes     uint64 `json:"memory_bytes"`
	MemoryFileBytes uint64 `json:"memory_file_bytes"`
	DiskReadBytes   uint64 `json:"disk_read_bytes"`
	DiskWriteBytes  uint64 `json:"disk_write_bytes"`
	NetRxBytes      uint64 `json:"net_rx_bytes,omitempty"`
	NetTxBytes      uint64 `json:"net_tx_bytes,omitempty"`
	PIDs            int    `json:"pids"`
}
