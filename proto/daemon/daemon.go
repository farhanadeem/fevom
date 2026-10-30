package daemon

import "path/filepath"

const APIVersion = 1

func SocketPath(home string) string { return filepath.Join(home, "run", "fevd.sock") }

const (
	MethodHello    = "daemon.hello"
	MethodStatus   = "daemon.status"
	MethodShutdown = "daemon.shutdown"

	MethodVMStart   = "vm.start"
	MethodVMStop    = "vm.stop"
	MethodVMRestart = "vm.restart"

	MethodEventsSubscribe   = "events.subscribe"
	MethodEventsUnsubscribe = "events.unsubscribe"

	MethodConfigDescribe = "config.describe"
	MethodConfigList     = "config.list"
	MethodConfigGet      = "config.get"
	MethodConfigSet      = "config.set"
	MethodConfigReset    = "config.reset"

	MethodLogSubscribe   = "log.subscribe"
	MethodLogUnsubscribe = "log.unsubscribe"

	MethodBootTrace     = "boot.trace"
	MethodDockerStatus  = "docker.status"
	MethodDockerRestart = "docker.restart"
	MethodForwardsList  = "forwards.list"
	MethodDataReset     = "data.reset"

	MethodFilesStatus = "files.status"

	MethodStatsSubscribe   = "stats.subscribe"
	MethodStatsUnsubscribe = "stats.unsubscribe"
	MethodCaffeinateHold   = "caffeinate.hold"
	MethodForwardsRetry    = "forwards.retry"

	NotifyStatsSample = "stats.sample"

	NotifyPowerSleep = "power.sleep"
	NotifyPowerWake  = "power.wake"

	NotifyFilesChanged = "files.changed"

	NotifyStateChanged    = "state.changed"
	NotifyConfigChanged   = "config.changed"
	NotifyForwardsChanged = "forwards.changed"
	NotifyDockerEvent     = "docker.event"

	NotifyLogEntry      = "log.entry"
	NotifyLogBacklogEnd = "log.backlog_end"
)

const (
	CodeHelloRequired         = -32000
	CodeVersionMismatch       = -32001
	CodeFailed                = -32002
	CodeBuildMismatch         = -32003
	CodeUnknownKey            = -32004
	CodeInvalidValue          = -32005
	CodeShuttingDown          = -32006
	CodeNotImplemented        = -32007
	CodeBusy                  = -32009
	CodeDebugToolsUnavailable = -32010
)

type HelloParams struct {
	APIVersion   int    `json:"api_version"`
	Build        string `json:"build,omitempty"`
	Client       string `json:"client"`
	SSHAuthSock  string `json:"ssh_auth_sock,omitempty"`
	Terminfo     string `json:"terminfo,omitempty"`
	TerminfoDirs string `json:"terminfo_dirs,omitempty"`
}

type HelloResult struct {
	APIVersion int    `json:"api_version"`
	Build      string `json:"build"`
	PID        int    `json:"pid"`
	Home       string `json:"home"`
}

type BuildMismatch struct {
	DaemonBuild       string `json:"daemon_build"`
	ClientBuild       string `json:"client_build"`
	PID               int    `json:"pid,omitempty"`
	State             string `json:"state,omitempty"`
	RunningContainers *int   `json:"running_containers,omitempty"`
}

type Empty struct{}

const (
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateRunning  = "running"
	StateStopping = "stopping"
	StatePaused   = "paused"
)

const (
	PauseSleep = "sleep"
)

func PauseMarkPath(home string) string { return filepath.Join(home, "run", "paused") }

const PausedLine = `fev: paused for the Mac's sleep; this command waits for the wake ("fev start" resumes now)`

func StopMarkPath(home string) string { return filepath.Join(home, "stop-requested") }

const StoppedLine = `fev: stopped; run "fev start"`

const (
	PhaseVMM     = "vmm"
	PhaseKernel  = "kernel"
	PhaseInit    = "init"
	PhaseEngine  = "engine"
	PhaseBackoff = "backoff"
)

const (
	EngineStopped  = "stopped"
	EngineStarting = "starting"
	EngineReady    = "ready"
	EngineStopping = "stopping"
	EngineFailed   = "failed"
	EngineOff      = "off"
)

const EngineOffLine = `the Docker engine is off; run "fev config set docker.enabled true"`

type Status struct {
	Seq            uint64   `json:"seq"`
	State          string   `json:"state"`
	PauseReason    string   `json:"pause_reason,omitempty"`
	StopRequested  bool     `json:"stop_requested,omitempty"`
	Phase          string   `json:"phase,omitempty"`
	RetryInMS      int      `json:"retry_in_ms,omitempty"`
	Engine         string   `json:"engine"`
	BootID         string   `json:"boot_id,omitempty"`
	Since          string   `json:"since"`
	Fake           bool     `json:"fake,omitempty"`
	PendingRestart []string `json:"pending_restart"`
	Error          string   `json:"error,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
	LastExit       *Exit    `json:"last_exit,omitempty"`
	Windows        *Windows `json:"windows,omitempty"`

	Network *NetworkStatus `json:"network,omitempty"`
	Domains *DomainsStatus `json:"domains,omitempty"`
	HTTPS   *HTTPSStatus   `json:"https,omitempty"`
	Data    *DataStatus    `json:"data,omitempty"`

	View *FilesView `json:"view,omitempty"`

	Emulation *EmulationStatus `json:"emulation,omitempty"`

	Debug *DebugLine `json:"debug,omitempty"`

	K8s *K8sLine `json:"k8s,omitempty"`
}

type DebugLine struct {
	Exists   bool  `json:"exists"`
	Bytes    int64 `json:"bytes"`
	Packages int   `json:"packages"`
	Sessions int   `json:"sessions,omitempty"`
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
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
	SizeBytes uint64 `json:"size_bytes"`
	Reads     uint64 `json:"reads,omitempty"`
	Mappings  uint64 `json:"mappings,omitempty"`
	Live      uint64 `json:"live,omitempty"`
	Faults    uint64 `json:"faults,omitempty"`
}

type Exit struct {
	Code        int      `json:"code"`
	Signal      string   `json:"signal,omitempty"`
	At          string   `json:"at"`
	Expected    bool     `json:"expected"`
	VMMTail     []string `json:"vmm_tail,omitempty"`
	ConsoleTail []string `json:"console_tail,omitempty"`
}

type ShutdownParams struct {
	Force         bool   `json:"force,omitempty"`
	StopRequested bool   `json:"stop_requested,omitempty"`
	StopBeganNS   uint64 `json:"stop_began_ns,omitempty"`
}

type ShutdownResult struct {
	Superseded bool `json:"superseded,omitempty"`
}

type StartParams struct {
	NoWait bool `json:"no_wait,omitempty"`
}

type StopParams struct {
	Force       bool   `json:"force,omitempty"`
	TimeoutMS   int    `json:"timeout_ms,omitempty"`
	NoWait      bool   `json:"no_wait,omitempty"`
	StopBeganNS uint64 `json:"stop_began_ns,omitempty"`
}

const DefaultStopTimeoutMS = 15000

type Subscription struct {
	Subscription uint64 `json:"subscription"`
}

type EventsSubscribed struct {
	Subscription uint64 `json:"subscription"`
	Status       Status `json:"status"`
}

type StateChanged struct {
	Subscription uint64 `json:"subscription"`
	Status       Status `json:"status"`
}

const (
	KeyCPU               = "cpu"
	KeyMemoryMiB         = "memory_mib"
	KeyDockerSetContext  = "docker.set_context"
	KeyDockerEnabled     = "docker.enabled"
	KeyDockerNodeName    = "docker.node_name"
	KeyDataAllowBackup   = "data_allow_backup"
	KeyNetworkPublishLAN = "network.publish_lan"
	KeyFilesShare        = "files.share"

	KeyPowerPauseInSleep   = "power.pause_in_sleep"
	KeyStartAtLogin        = "start_at_login"
	KeyNetworkHostLoopback = "network.host_loopback"
	KeyFilesDAXWindowMiB   = "files.dax_window_mib"

	KeyMachinesForwardPorts     = "machines.forward_ports"
	KeyMachinesExposePortsToLAN = "machines.expose_ports_to_lan"

	KeySSHPort       = "ssh.port"
	KeySSHExposePort = "ssh.expose_port"
)

const (
	TypeInt    = "int"
	TypeBool   = "bool"
	TypeString = "string"
)

const (
	ApplyLive          = "live"
	ApplyEngineRestart = "engine_restart"
	ApplyVMRestart     = "vm_restart"
)

type ConfigKey struct {
	Key         string `json:"key"`
	Type        string `json:"type"`
	Default     any    `json:"default"`
	Apply       string `json:"apply"`
	Description string `json:"description"`
	Min         *int64 `json:"min,omitempty"`
	Max         *int64 `json:"max,omitempty"`
}

type ConfigKeys struct {
	Keys []ConfigKey `json:"keys"`
}

type ConfigValue struct {
	Value     any  `json:"value"`
	IsDefault bool `json:"is_default,omitempty"`
	Pending   bool `json:"pending,omitempty"`
}

type ConfigValues struct {
	Values map[string]ConfigValue `json:"values"`
}

type ConfigGetParams struct {
	Keys []string `json:"keys"`
}

type ConfigSetParams struct {
	Values map[string]any `json:"values"`
}

type ConfigResetParams struct {
	Keys []string `json:"keys,omitempty"`
}

type ConfigChange struct {
	Key     string `json:"key"`
	Value   any    `json:"value"`
	Apply   string `json:"apply"`
	Pending bool   `json:"pending,omitempty"`
}

type ConfigChanges struct {
	Changed []ConfigChange `json:"changed"`
}

type ConfigChanged struct {
	Subscription uint64 `json:"subscription"`
	ConfigChange
}

type ConfigError struct {
	Key    string `json:"key"`
	Reason string `json:"reason,omitempty"`
}

const (
	LogDaemon  = "daemon"
	LogVMM     = "vmm"
	LogConsole = "console"
	LogKernel  = "kernel"
	LogInit    = "init"
	LogEngine  = "engine"
)

const LogMachinePrefix = "machine:"

func MachineConsoleLog(name string) string { return LogMachinePrefix + name }
func MachineRuntimeLog(name string) string { return LogMachinePrefix + name + ":runtime" }

type LogSubscribeParams struct {
	Sources []string `json:"sources,omitempty"`
	Backlog int      `json:"backlog,omitempty"`
}

type LogEntry struct {
	Subscription uint64  `json:"subscription"`
	Source       string  `json:"source"`
	Time         string  `json:"time"`
	GuestMS      float64 `json:"guest_ms,omitempty"`
	Level        int     `json:"level"`
	Text         string  `json:"text"`
	Dropped      int     `json:"dropped,omitempty"`
}

type Trace struct {
	V             int     `json:"v"`
	BootID        string  `json:"boot_id"`
	Build         string  `json:"build"`
	Wall          string  `json:"wall"`
	Clock         string  `json:"clock"`
	Origin        string  `json:"origin"`
	OriginNS      uint64  `json:"origin_ns"`
	CPUs          int     `json:"cpus"`
	MemMiB        int     `json:"memory_mib"`
	GuestOffsetMS float64 `json:"guest_offset_ms"`
	GuestErrorMS  float64 `json:"guest_error_ms"`
	Marks         []Mark  `json:"marks"`
	Stages        []Stage `json:"stages"`
	TotalMS       float64 `json:"total_ms"`
	BudgetMS      float64 `json:"budget_ms"`
	OK            bool    `json:"ok"`
	Complete      bool    `json:"complete"`
}

type Mark struct {
	Name   string  `json:"name"`
	MS     float64 `json:"ms"`
	Approx bool    `json:"approx,omitempty"`
}

type Stage struct {
	Stage    int     `json:"stage"`
	From     string  `json:"from"`
	To       string  `json:"to"`
	MS       float64 `json:"ms"`
	BudgetMS float64 `json:"budget_ms"`
	Over     bool    `json:"over"`
}

type DockerStatus struct {
	Engine       string        `json:"engine"`
	Socket       string        `json:"socket"`
	Context      DockerContext `json:"context"`
	CompatSocket string        `json:"compat_socket"`
	CompatTarget string        `json:"compat_target,omitempty"`
	ConfigFile   string        `json:"config_file,omitempty"`
	ConfigState  string        `json:"config_state,omitempty"`
	ConfigError  string        `json:"config_error,omitempty"`
}

const (
	EngineConfigNone    = "none"
	EngineConfigApplied = "applied"
	EngineConfigRefused = "refused"
)

type FilesStatus struct {
	Share  bool       `json:"share"`
	Root   string     `json:"root,omitempty"`
	Mount  string     `json:"mount,omitempty"`
	Hidden []string   `json:"hidden,omitempty"`
	Events bool       `json:"events"`
	Watch  FilesWatch `json:"watch"`
	Denied []string   `json:"denied,omitempty"`
	View   *FilesView `json:"view,omitempty"`
}

type FilesWatch struct {
	Seq      uint64          `json:"seq"`
	Paths    []string        `json:"paths"`
	Rejected []FilesRejected `json:"rejected,omitempty"`
}

type FilesRejected struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type FilesChanged struct {
	Subscription uint64 `json:"subscription"`
	FilesStatus
}

type DockerEvent struct {
	Subscription uint64            `json:"subscription"`
	Type         string            `json:"type"`
	Action       string            `json:"action"`
	Detail       string            `json:"detail,omitempty"`
	ID           string            `json:"id"`
	Name         string            `json:"name,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
	Time         string            `json:"time"`
}

type DataResetResult struct {
	FreedBytes int64  `json:"freed_bytes"`
	Status     Status `json:"status"`
}

const (
	CompatLinked    = "linked"
	CompatAbsent    = "absent"
	CompatElsewhere = "elsewhere"
)

type DockerContext struct {
	Name     string `json:"name"`
	Exists   bool   `json:"exists"`
	Selected bool   `json:"selected"`
	Current  string `json:"current,omitempty"`
}

const (
	ForwardActive   = "active"
	ForwardConflict = "conflict"
	ForwardError    = "error"
)

const ForwardDocker = "docker"

const ForwardMachine = "machine"

type Forward struct {
	ID     string   `json:"id"`
	Source string   `json:"source"`
	Owner  string   `json:"owner,omitempty"`
	Proto  string   `json:"proto"`
	Mac    []string `json:"mac"`
	Guest  string   `json:"guest"`
	State  string   `json:"state"`
	Error  string   `json:"error,omitempty"`
}

type Forwards struct {
	Forwards []Forward `json:"forwards"`
}

type ForwardsChanged struct {
	Subscription uint64  `json:"subscription"`
	Forward      Forward `json:"forward"`
	Removed      bool    `json:"removed,omitempty"`
}

const (
	DefaultStatsIntervalMS = 2000
	MinStatsIntervalMS     = 250
)

type StatsSubscribeParams struct {
	IntervalMS int `json:"interval_ms,omitempty"`
}

type StatsSample struct {
	Subscription uint64      `json:"subscription"`
	Time         string      `json:"time"`
	State        string      `json:"state"`
	VMM          *VMMStats   `json:"vmm,omitempty"`
	Guest        *GuestStats `json:"guest,omitempty"`
}

type VMMStats struct {
	CPUMS          float64  `json:"cpu_ms"`
	GuestBytes     uint64   `json:"guest_bytes"`
	FootprintBytes uint64   `json:"footprint_bytes"`
	HeldBytes      uint64   `json:"held_bytes"`
	ReleasedBytes  uint64   `json:"released_bytes"`
	Windows        *Windows `json:"windows,omitempty"`
}

type GuestStats struct {
	MemTotalKB     int64        `json:"mem_total_kb"`
	MemAvailableKB int64        `json:"mem_available_kb"`
	CachedKB       int64        `json:"cached_kb"`
	Groups         []StatsGroup `json:"groups"`
}

const (
	StatsSystem    = "system"
	StatsService   = "service"
	StatsContainer = "container"
	StatsMachine   = "machine"
)

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

type CaffeinateParams struct {
	Reason  string `json:"reason,omitempty"`
	Display bool   `json:"display,omitempty"`
}

type Caffeinated struct {
	Holders int  `json:"holders"`
	Display bool `json:"display,omitempty"`
}

type ForwardsRetryParams struct {
	ID string `json:"id,omitempty"`
}

const PauseBoundMS = 2000

type PowerSleep struct {
	Subscription uint64  `json:"subscription"`
	Time         string  `json:"time"`
	Paused       bool    `json:"paused"`
	Changed      bool    `json:"changed"`
	PauseMS      float64 `json:"pause_ms,omitempty"`
	TimedOut     bool    `json:"timed_out,omitempty"`
}

const (
	WakeNotSleep   = "will_not_sleep"
	WakeUnfinished = "will_power_on"
	WakeCommand    = "command"
)

type PowerWake struct {
	Subscription  uint64  `json:"subscription"`
	Time          string  `json:"time"`
	Cause         string  `json:"cause,omitempty"`
	Resumed       bool    `json:"resumed"`
	PausedMS      float64 `json:"paused_ms,omitempty"`
	ClockStepped  bool    `json:"clock_stepped"`
	ClockOffsetMS float64 `json:"clock_offset_ms,omitempty"`
	Released      int     `json:"released,omitempty"`
}
