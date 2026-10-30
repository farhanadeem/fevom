package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"path"
	"slices"
	"strings"

	"fev/proto/relay"
)

const (
	MethodMachineCreate  = "machine.create"
	MethodMachineStart   = "machine.start"
	MethodMachineStop    = "machine.stop"
	MethodMachineRestart = "machine.restart"
	MethodMachineDelete  = "machine.delete"
	MethodMachineClone   = "machine.clone"
	MethodMachineRename  = "machine.rename"
	MethodMachineExport  = "machine.export"
	MethodMachineImport  = "machine.import"
	MethodMachineInfo    = "machine.info"
	MethodMachineList    = "machine.list"

	MethodOperationCancel = "operation.cancel"

	MethodTrustSet = "trust.set"

	MethodSSHAgentSet = "ssh_agent.set"

	MethodStorageCheck   = "storage.check"
	MethodStorageSalvage = "storage.salvage"
	MethodStorageUsage   = "storage.usage"

	NotifyMachineState      = "machine.state"
	NotifyOperationProgress = "operation.progress"
)

func CheckMachineName(name string) error { return relay.CheckMachineName(name) }

const (
	MachineCreating = "creating"
	MachineStopped  = "stopped"
	MachineStarting = "starting"
	MachineRunning  = "running"
	MachineStopping = "stopping"
	MachineFailed   = "failed"
)

const (
	ExitHalt    = "halt"
	ExitReboot  = "reboot"
	ExitStopped = "stopped"
)

const (
	VariantDefault = "default"
	VariantCloud   = "cloud"
)

type ImageRef struct {
	Distro  string `json:"distro"`
	Release string `json:"release"`
	Arch    string `json:"arch"`
	Variant string `json:"variant"`
	Serial  string `json:"serial"`
}

type ImageTemplate struct {
	Path       string            `json:"path"`
	When       []string          `json:"when"`
	CreateOnly bool              `json:"create_only,omitempty"`
	Template   string            `json:"template"`
	Properties map[string]string `json:"properties,omitempty"`
}

type MachineImage struct {
	ImageRef
	Templates []ImageTemplate `json:"templates,omitempty"`
}

type MachineUser struct {
	Name         string `json:"name"`
	UID          int    `json:"uid"`
	PasswordHash string `json:"password_hash,omitempty"`
}

type MachineConfig struct {
	Emulation string `json:"emulation,omitempty"`
	User      string `json:"user"`
	MAC       string `json:"mac"`
	Address   string `json:"address"`
	Share     bool   `json:"share,omitempty"`

	Isolated        bool           `json:"isolated,omitempty"`
	IsolateNetwork  bool           `json:"isolate_network,omitempty"`
	Mounts          []MachineMount `json:"mounts,omitempty"`
	ForwardSSHAgent bool           `json:"forward_ssh_agent,omitempty"`
}

const MaxUserDataBytes = 1 << 20

type MachineCreateParams struct {
	Name     string        `json:"name"`
	Image    MachineImage  `json:"image"`
	User     MachineUser   `json:"user"`
	Config   MachineConfig `json:"config"`
	UserData string        `json:"user_data,omitempty"`
}

type MachineNameParams struct {
	Name string `json:"name"`
}

type MachineStartParams struct {
	Name   string        `json:"name"`
	Config MachineConfig `json:"config"`
}

const DefaultMachineGraceMS = 10000

type MachineStopParams struct {
	Name    string `json:"name"`
	GraceMS int    `json:"grace_ms,omitempty"`
	Force   bool   `json:"force,omitempty"`
}

type MachineRestartParams struct {
	Name    string        `json:"name"`
	GraceMS int           `json:"grace_ms,omitempty"`
	Config  MachineConfig `json:"config"`
}

type MachineCloneParams struct {
	Name    string        `json:"name"`
	NewName string        `json:"new_name"`
	Config  MachineConfig `json:"config"`
}

type MachineRenameParams struct {
	Name    string `json:"name"`
	NewName string `json:"new_name"`
}

type MachineImportParams struct {
	Name   string        `json:"name"`
	Config MachineConfig `json:"config"`
}

type MachineInfo struct {
	Emulation       string   `json:"emulation,omitempty"`
	EmulationReason string   `json:"emulation_reason,omitempty"`
	Name            string   `json:"name"`
	State           string   `json:"state"`
	Image           ImageRef `json:"image"`
	UUID            string   `json:"uuid"`
	Init            string   `json:"init,omitempty"`
	Address         string   `json:"address,omitempty"`
	Address6        string   `json:"address6,omitempty"`
	PID             int      `json:"pid,omitempty"`
	StartedMS       float64  `json:"started_ms,omitempty"`
	ReadyMS         float64  `json:"ready_ms,omitempty"`
	DiskBytes       int64    `json:"disk_bytes"`
	LastExit        string   `json:"last_exit,omitempty"`
	Error           string   `json:"error,omitempty"`
}

type MachineState struct {
	MachineInfo
	Removed bool `json:"removed,omitempty"`
}

type ImageInfo struct {
	ImageRef
	DiskBytes int64 `json:"disk_bytes"`
}

type MachineList struct {
	Machines []MachineInfo `json:"machines"`
	Images   []ImageInfo   `json:"images"`
}

type Operation struct {
	ID    uint64 `json:"id"`
	Token string `json:"token,omitempty"`
}

type OperationID struct {
	ID uint64 `json:"id"`
}

const (
	OperationRunning   = "running"
	OperationDone      = "done"
	OperationFailed    = "failed"
	OperationCancelled = "cancelled"
)

const (
	PhaseImage     = "image"
	PhaseArchive   = "archive"
	PhaseSetup     = "setup"
	PhaseBoot      = "boot"
	PhaseOnline    = "online"
	PhaseCloudInit = "cloud-init"
	PhaseScrub     = "scrub"
	PhaseCopy      = "copy"
	PhaseRestore   = "restore"
)

type OperationProgress struct {
	ID         uint64          `json:"id"`
	Method     string          `json:"method"`
	State      string          `json:"state"`
	Phase      string          `json:"phase,omitempty"`
	DoneBytes  int64           `json:"done_bytes,omitempty"`
	TotalBytes int64           `json:"total_bytes,omitempty"`
	Error      string          `json:"error,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
}

type CloudInit struct {
	Status            string   `json:"status"`
	Errors            []string `json:"errors,omitempty"`
	OutputTail        []string `json:"output_tail,omitempty"`
	RecoverableErrors []string `json:"recoverable_errors,omitempty"`
}

type MachineCreated struct {
	Machine   MachineInfo `json:"machine"`
	CloudInit *CloudInit  `json:"cloud_init,omitempty"`
}

type MachineExported struct {
	Bytes int64 `json:"bytes"`
}

type SSHAgentParams struct {
	Path string `json:"path"`
}

type SSHAgentBound struct {
	Engine   bool     `json:"engine"`
	Machines []string `json:"machines"`
}

func CheckSSHAgentPath(p string) error {
	if p == "" {
		return nil
	}
	if !path.IsAbs(p) || path.Clean(p) != p || p == "/" || strings.ContainsRune(p, 0) {
		return fmt.Errorf("%q is not an absolute, clean path to a socket", p)
	}
	return nil
}

const MaxTrustBytes = 2 << 20

type TrustParams struct {
	Roots []string `json:"roots"`
}

type Trusted struct {
	Count  int    `json:"count"`
	Digest string `json:"digest"`
}

func TrustDER(roots []string) ([][]byte, error) {
	size := 0
	ders := make([][]byte, 0, len(roots))
	for i, r := range roots {
		size += len(r)
		if size > MaxTrustBytes {
			return nil, fmt.Errorf("the roots are more than %d bytes of PEM", MaxTrustBytes)
		}
		b, rest := pem.Decode([]byte(r))
		if b == nil || b.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
			return nil, fmt.Errorf("root %d is not one PEM block of a certificate", i)
		}
		ders = append(ders, b.Bytes)
	}
	return ders, nil
}

func TrustDigest(ders [][]byte) string {
	h := sha256.New()
	for _, d := range uniqueDER(ders) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(d)))
		h.Write(n[:])
		h.Write(d)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func uniqueDER(ders [][]byte) [][]byte {
	s := append([][]byte(nil), ders...)
	slices.SortFunc(s, bytes.Compare)
	return slices.CompactFunc(s, bytes.Equal)
}

func TrustCount(ders [][]byte) int { return len(uniqueDER(ders)) }

type StorageCheck struct {
	State         string  `json:"state"`
	Auto          bool    `json:"auto,omitempty"`
	ReadOnly      bool    `json:"read_only,omitempty"`
	StartedMS     float64 `json:"started_ms"`
	EndedMS       float64 `json:"ended_ms,omitempty"`
	ScrubbedBytes int64   `json:"scrubbed_bytes"`
	Corrected     uint64  `json:"corrected"`
	Uncorrectable uint64  `json:"uncorrectable"`
	Error         string  `json:"error,omitempty"`
}

type CheckParams struct {
	Follow bool `json:"follow,omitempty"`
}

const NoCheck = "no check has run since the data filesystem was mounted"

type SalvageParams struct {
	Dest     string   `json:"dest"`
	Volumes  []string `json:"volumes,omitempty"`
	Machines []string `json:"machines,omitempty"`
	Docker   bool     `json:"docker,omitempty"`
}

const (
	SalvageCopy    = "copy"
	SalvageRestore = "restore"
)

type Salvaged struct {
	Method string   `json:"method"`
	Files  int64    `json:"files"`
	Bytes  int64    `json:"bytes"`
	Errors []string `json:"errors,omitempty"`
	Lost   int64    `json:"lost,omitempty"`
}

type UsageParams struct {
	Engine bool `json:"engine,omitempty"`
}

type StorageUsage struct {
	UsedBytes   int64            `json:"used_bytes"`
	Machines    []MachineUsage   `json:"machines"`
	ImagesBytes int64            `json:"images_bytes"`
	EngineBytes int64            `json:"engine_bytes"`
	Engine      *EngineUsage     `json:"engine,omitempty"`
	DebugBytes  int64            `json:"debug_bytes,omitempty"`
	Kubernetes  *KubernetesUsage `json:"kubernetes,omitempty"`
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
