package attach

type DebugEntry struct {
	Kind string `json:"kind"`

	StoreTreeFd   int `json:"store_tree_fd"`
	ProfileTreeFd int `json:"profile_tree_fd"`
	EtcTreeFd     int `json:"etc_tree_fd"`
	GalleyTreeFd  int `json:"galley_tree_fd"`
	SocketTreeFd  int `json:"socket_tree_fd"`
	HomeTreeFd    int `json:"home_tree_fd"`

	MountFd       int `json:"mount_fd"`
	NetFd         int `json:"net_fd"`
	IPCFd         int `json:"ipc_fd"`
	UTSFd         int `json:"uts_fd"`
	CgroupNSFd    int `json:"cgroup_ns_fd"`
	PIDFd         int `json:"pid_fd"`
	UserFd        int `json:"user_fd"`
	CgroupProcsFd int `json:"cgroup_procs_fd"`

	TargetPidNS uint64 `json:"target_pidns,omitempty"`
	TargetNSPid int    `json:"target_nspid,omitempty"`
	TargetStart uint64 `json:"target_start,omitempty"`
	Prefix      []Insn `json:"prefix,omitempty"`
	DumpLock    string `json:"dump_lock,omitempty"`

	Lowers           []string `json:"lowers,omitempty"`
	Upper            string   `json:"upper,omitempty"`
	Work             string   `json:"work,omitempty"`
	DevCgroupProcsFd int      `json:"dev_cgroup_procs_fd"`
	DevPtsFd         int      `json:"dev_pts_fd"`

	Env  []string `json:"env"`
	Dir  string   `json:"dir"`
	Home string   `json:"home"`

	StageFd int  `json:"stage_fd"`
	Spec    Spec `json:"spec"`

	Name string `json:"name"`
}

const (
	DebugKindRunning = "running"
	DebugKindStopped = "stopped"
	DebugKindImage   = "image"
)

const (
	DebugRequestContainer = "container"
	DebugRequestImage     = "image"
)
