package daemon

import (
	"encoding/json"
	"path/filepath"
)

const (
	MethodMachinesList       = "machines.list"
	MethodMachinesInfo       = "machines.info"
	MethodMachinesCreate     = "machines.create"
	MethodMachinesStart      = "machines.start"
	MethodMachinesStop       = "machines.stop"
	MethodMachinesRestart    = "machines.restart"
	MethodMachinesDelete     = "machines.delete"
	MethodMachinesClone      = "machines.clone"
	MethodMachinesRename     = "machines.rename"
	MethodMachinesExport     = "machines.export"
	MethodMachinesImport     = "machines.import"
	MethodMachinesSetDefault = "machines.set_default"

	MethodSessionStart  = "session.start"
	MethodSessionResize = "session.resize"
	MethodSessionSignal = "session.signal"
	MethodSessionClose  = "session.close"

	MethodSSHInfo = "ssh.info"

	MethodOperationGet    = "operation.get"
	MethodOperationCancel = "operation.cancel"

	MethodStorageUsage   = "storage.usage"
	MethodStorageSalvage = "storage.salvage"

	NotifyMachinesChanged   = "machines.changed"
	NotifyOperationProgress = "operation.progress"
	NotifySessionExit       = "session.exit"
)

const CodeNoMachine = -32008

func SessionSocketPath(home string) string { return filepath.Join(home, "run", "session.sock") }

const (
	MachineCreating = "creating"
	MachineStopped  = "stopped"
	MachineStarting = "starting"
	MachineRunning  = "running"
	MachineStopping = "stopping"
	MachineFailed   = "failed"
)

type Image struct {
	Distro  string `json:"distro"`
	Release string `json:"release"`
	Arch    string `json:"arch"`
	Variant string `json:"variant"`
	Serial  string `json:"serial"`
}

type Machine struct {
	Emulation       string `json:"emulation,omitempty"`
	EmulationReason string `json:"emulation_reason,omitempty"`
	Name            string `json:"name"`
	State           string `json:"state"`
	Image           Image  `json:"image"`
	User            string `json:"user"`
	Default         bool   `json:"default,omitempty"`
	Address         string `json:"address"`
	Address6        string `json:"address6,omitempty"`
	DiskBytes       int64  `json:"disk_size"`
	Since           string `json:"since"`
	LastExit        string `json:"last_exit,omitempty"`
	Error           string `json:"error,omitempty"`
	Isolated        bool   `json:"isolated,omitempty"`
	IsolateNetwork  bool   `json:"isolate_network,omitempty"`
}

type Machines struct {
	Machines []Machine `json:"machines"`
	Default  string    `json:"default,omitempty"`
}

type MachineName struct {
	Name string `json:"name"`
}

const MaxUserDataBytes = 1 << 20

type MachinesCreateParams struct {
	Emulation       string   `json:"emulation,omitempty"`
	Name            string   `json:"name"`
	Distro          string   `json:"distro"`
	Release         string   `json:"release,omitempty"`
	Arch            string   `json:"arch,omitempty"`
	User            string   `json:"user,omitempty"`
	PasswordHash    string   `json:"password_hash,omitempty"`
	UserData        string   `json:"user_data,omitempty"`
	Isolated        bool     `json:"isolated,omitempty"`
	IsolateNetwork  bool     `json:"isolate_network,omitempty"`
	Mounts          []string `json:"mounts,omitempty"`
	ForwardSSHAgent bool     `json:"forward_ssh_agent,omitempty"`
}

type MachineCreated struct {
	Machine   Machine    `json:"machine"`
	CloudInit *CloudInit `json:"cloud_init,omitempty"`
}

type CloudInit struct {
	Status            string   `json:"status"`
	Errors            []string `json:"errors,omitempty"`
	OutputTail        []string `json:"output_tail,omitempty"`
	RecoverableErrors []string `json:"recoverable_errors,omitempty"`
}

type MachinesStopParams struct {
	Name    string `json:"name"`
	GraceMS int    `json:"grace_ms,omitempty"`
	Force   bool   `json:"force,omitempty"`
}

type MachinesRenameParams struct {
	Name    string `json:"name"`
	NewName string `json:"new_name"`
}

type MachinesExportParams struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Replace bool   `json:"replace,omitempty"`
}

type MachineExported struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

type MachinesImportParams struct {
	Path string `json:"path"`
	Name string `json:"name,omitempty"`
}

const (
	OperationRunning   = "running"
	OperationDone      = "done"
	OperationFailed    = "failed"
	OperationCancelled = "cancelled"
)

const (
	PhaseVerify   = "verify"
	PhaseDownload = "download"
)

type Operation struct {
	ID         string          `json:"id"`
	Method     string          `json:"method"`
	Target     string          `json:"target"`
	State      string          `json:"state"`
	Phase      string          `json:"phase,omitempty"`
	DoneBytes  int64           `json:"done_bytes,omitempty"`
	TotalBytes int64           `json:"total_bytes,omitempty"`
	Note       string          `json:"note,omitempty"`
	Started    string          `json:"started"`
	Ended      string          `json:"ended,omitempty"`
	Error      string          `json:"error,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
}

type OperationID struct {
	ID string `json:"id"`
}

type OperationProgress struct {
	Subscription uint64    `json:"subscription"`
	Operation    Operation `json:"operation"`
}

type MachinesChanged struct {
	Subscription uint64  `json:"subscription"`
	Machine      Machine `json:"machine"`
	Removed      bool    `json:"removed,omitempty"`
}

type SessionStartParams struct {
	Machine  string   `json:"machine,omitempty"`
	User     string   `json:"user,omitempty"`
	Argv     []string `json:"argv"`
	Env      []string `json:"env,omitempty"`
	Dir      string   `json:"dir,omitempty"`
	TTY      bool     `json:"tty,omitempty"`
	StdinTTY bool     `json:"stdin_tty,omitempty"`
	Rows     int      `json:"rows,omitempty"`
	Cols     int      `json:"cols,omitempty"`
	Login    bool     `json:"login,omitempty"`
	Shell    bool     `json:"shell,omitempty"`
	Terminfo []byte   `json:"terminfo,omitempty"`
	Copy     string   `json:"copy,omitempty"`
}

type SessionStarted struct {
	ID      uint64 `json:"id"`
	Token   string `json:"token"`
	Machine string `json:"machine"`
	User    string `json:"user"`
}

type SessionResizeParams struct {
	ID   uint64 `json:"id"`
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
}

type SessionSignalParams struct {
	ID     uint64 `json:"id"`
	Signal int    `json:"signal"`
}

type SessionID struct {
	ID uint64 `json:"id"`
}

type SessionExit struct {
	ID          uint64 `json:"id"`
	ExitCode    int    `json:"exit_code"`
	Signal      int    `json:"signal,omitempty"`
	Error       string `json:"error,omitempty"`
	StreamEnded bool   `json:"stream_ended,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Note        string `json:"note,omitempty"`
}

func SSHSocketPath(home string) string { return filepath.Join(home, "run", "ssh.sock") }

const DefaultSSHPort = 32122

type SSHInfo struct {
	Host                string `json:"host"`
	Port                int    `json:"port"`
	Listening           bool   `json:"listening"`
	ExposePort          bool   `json:"expose_port,omitempty"`
	Alias               string `json:"alias"`
	User                string `json:"user"`
	IdentityFile        string `json:"identity_file"`
	KnownHostsFile      string `json:"known_hosts_file"`
	ConfigFile          string `json:"config_file"`
	Included            bool   `json:"included"`
	ProxyCommand        string `json:"proxy_command"`
	Socket              string `json:"socket"`
	HostKey             string `json:"host_key"`
	AuthorizedKeys      string `json:"authorized_keys"`
	AuthorizedKeysError string `json:"authorized_keys_error,omitempty"`
}

type UsageParams struct {
	Engine bool `json:"engine,omitempty"`
}

type StorageUsage struct {
	ImageFileBytes int64            `json:"image_file_bytes"`
	UsedBytes      int64            `json:"used_bytes"`
	Machines       []MachineUsage   `json:"machines"`
	ImagesBytes    int64            `json:"images_bytes"`
	EngineBytes    int64            `json:"engine_bytes"`
	Engine         *EngineUsage     `json:"engine,omitempty"`
	DebugBytes     int64            `json:"debug_bytes,omitempty"`
	Kubernetes     *KubernetesUsage `json:"kubernetes,omitempty"`
}

type KubernetesUsage struct {
	Bytes         int64 `json:"bytes"`
	ClusterBytes  int64 `json:"cluster_bytes"`
	CacheBytes    int64 `json:"cache_bytes"`
	ImagesBytes   int64 `json:"images_bytes,omitempty"`
	ImagesCounted bool  `json:"images_counted,omitempty"`
}

type MachineUsage struct {
	Name      string `json:"name"`
	DiskBytes int64  `json:"disk_bytes"`
}

type EngineUsage struct {
	ImagesBytes     int64 `json:"images_bytes"`
	ContainersBytes int64 `json:"containers_bytes"`
	VolumesBytes    int64 `json:"volumes_bytes"`
	BuildCacheBytes int64 `json:"build_cache_bytes"`
}

type SalvageParams struct {
	Dest     string   `json:"dest"`
	Volumes  []string `json:"volumes,omitempty"`
	Machines []string `json:"machines,omitempty"`
	Docker   bool     `json:"docker,omitempty"`
}

type Salvaged struct {
	Method string   `json:"method"`
	Files  int64    `json:"files"`
	Bytes  int64    `json:"bytes"`
	Errors []string `json:"errors,omitempty"`
	Lost   int64    `json:"lost,omitempty"`
}
