package daemon

const (
	MethodK8sStatus  = "k8s.status"
	MethodK8sEnable  = "k8s.enable"
	MethodK8sDisable = "k8s.disable"
	MethodK8sReset   = "k8s.reset"
	MethodK8sWatch   = "k8s.watch"
	MethodK8sUnwatch = "k8s.unwatch"
	MethodK8sGet     = "k8s.get"
	MethodK8sApply   = "k8s.apply"
	MethodK8sUpdate  = "k8s.update"
	MethodK8sScale   = "k8s.scale"
	MethodK8sRestart = "k8s.restart"
	MethodK8sDelete  = "k8s.delete"
	MethodK8sLogs    = "k8s.logs"
	MethodK8sExec    = "k8s.exec"

	NotifyK8sChanged = "k8s.changed"
	NotifyK8sLog     = "k8s.log"
	NotifyK8sLogEnd  = "k8s.log_end"
)

const CodeConflict = -32011

const (
	KeyKubernetesEnabled       = "kubernetes.enabled"
	KeyKubernetesExposeLAN     = "kubernetes.expose_services_to_lan"
	KeyKubernetesKubelet       = "kubernetes.kubelet"
	KeyKubernetesMetricsServer = "kubernetes.metrics_server"
	KeyKubernetesImagesURL     = "kubernetes.images_url"
)

const TypeObject = "object"

const (
	K8sStateOff      = "off"
	K8sStateStarting = "starting"
	K8sStateRunning  = "running"
	K8sStateStopping = "stopping"
	K8sStateError    = "error"
)

const (
	K8sPhaseChecking  = "checking"
	K8sPhaseFetching  = "fetching"
	K8sPhaseVerifying = "verifying"
	K8sPhaseImporting = "importing"
	K8sPhaseStarting  = "starting"
	K8sPhaseReady     = "ready"
	K8sPhaseStopping  = "stopping"
	K8sPhaseDeleting  = "deleting"
)

const (
	K8sKindNamespace   = "Namespace"
	K8sKindDeployment  = "Deployment"
	K8sKindStatefulSet = "StatefulSet"
	K8sKindDaemonSet   = "DaemonSet"
	K8sKindReplicaSet  = "ReplicaSet"
	K8sKindPod         = "Pod"
	K8sKindService     = "Service"
	K8sKindIngress     = "Ingress"
)

const (
	K8sActionCreated    = "created"
	K8sActionConfigured = "configured"
	K8sActionUnchanged  = "unchanged"
	K8sActionFailed     = "failed"
)

const (
	K8sChangeAdded    = "added"
	K8sChangeModified = "modified"
	K8sChangeDeleted  = "deleted"
)

type K8sStatus struct {
	Enabled        bool            `json:"enabled"`
	State          string          `json:"state"`
	Ready          bool            `json:"ready"`
	Node           string          `json:"node,omitempty"`
	Version        string          `json:"version,omitempty"`
	Error          string          `json:"error,omitempty"`
	Ranges         K8sRanges       `json:"ranges"`
	RangeConflicts []string        `json:"range_conflicts,omitempty"`
	API            K8sAPIForward   `json:"api"`
	Kubeconfig     K8sKubeconfig   `json:"kubeconfig"`
	ClusterLocal   K8sClusterLocal `json:"cluster_local"`
	Services       []K8sService    `json:"services,omitempty"`
	Ingresses      []K8sIngress    `json:"ingresses,omitempty"`
	KubeletChanged []string        `json:"kubelet_changed,omitempty"`
	PortConflicts  []string        `json:"port_conflicts,omitempty"`
	Archive        K8sArchive      `json:"archive"`
	Cost           string          `json:"cost"`
}

type K8sRanges struct {
	LoadBalancers string `json:"load_balancers"`
	Pods          string `json:"pods"`
	Services      string `json:"services"`
	DNS           string `json:"dns"`
}

type K8sAPIForward struct {
	Address string `json:"address,omitempty"`
	Note    string `json:"note,omitempty"`
}

type K8sKubeconfig struct {
	Path    string `json:"path"`
	Context string `json:"context"`
	Present bool   `json:"present"`
	Current bool   `json:"current,omitempty"`
}

type K8sClusterLocal struct {
	Answered bool   `json:"answered"`
	Note     string `json:"note,omitempty"`
}

type K8sArchive struct {
	Version string `json:"version"`
	Cached  bool   `json:"cached"`
	Bytes   int64  `json:"bytes,omitempty"`
	Size    int64  `json:"size"`
	URL     string `json:"url"`
}

type K8sService struct {
	Namespace    string    `json:"namespace"`
	Name         string    `json:"name"`
	Type         string    `json:"type"`
	ClusterIP    string    `json:"cluster_ip,omitempty"`
	LoadBalancer string    `json:"load_balancer,omitempty"`
	Ports        []K8sPort `json:"ports,omitempty"`
	Names        []string  `json:"names,omitempty"`
	URLs         []string  `json:"urls,omitempty"`
}

type K8sPort struct {
	Name     string `json:"name,omitempty"`
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
	NodePort int    `json:"node_port,omitempty"`
}

type K8sIngress struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Hosts     []string `json:"hosts,omitempty"`
	Address   string   `json:"address,omitempty"`
	URLs      []string `json:"urls,omitempty"`
}

type K8sLine struct {
	Enabled bool   `json:"enabled"`
	State   string `json:"state"`
	Ready   bool   `json:"ready"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

type K8sResetParams struct {
	All bool `json:"all,omitempty"`
}

type K8sWatchParams struct {
	Kinds     []string `json:"kinds,omitempty"`
	Namespace string   `json:"namespace,omitempty"`
}

type K8sWatching struct {
	Subscription uint64      `json:"subscription"`
	Objects      []K8sObject `json:"objects"`
}

type K8sChanged struct {
	Subscription uint64    `json:"subscription"`
	Type         string    `json:"type"`
	Object       K8sObject `json:"object"`
}

type K8sObject struct {
	Kind            string            `json:"kind"`
	Namespace       string            `json:"namespace,omitempty"`
	Name            string            `json:"name"`
	UID             string            `json:"uid"`
	Created         string            `json:"created"`
	ResourceVersion string            `json:"resource_version"`
	Labels          map[string]string `json:"labels,omitempty"`

	Workload *K8sWorkload `json:"workload,omitempty"`
	Pod      *K8sPod      `json:"pod,omitempty"`
	Service  *K8sService  `json:"service,omitempty"`
	Ingress  *K8sIngress  `json:"ingress,omitempty"`
	Phase    string       `json:"phase,omitempty"`
}

type K8sWorkload struct {
	Wanted    int      `json:"wanted"`
	Ready     int      `json:"ready"`
	Updated   int      `json:"updated"`
	Available int      `json:"available"`
	Images    []string `json:"images,omitempty"`
}

type K8sPod struct {
	Phase      string         `json:"phase"`
	Reason     string         `json:"reason,omitempty"`
	Ready      bool           `json:"ready"`
	Restarts   int            `json:"restarts"`
	Node       string         `json:"node,omitempty"`
	IP         string         `json:"ip,omitempty"`
	Containers []K8sContainer `json:"containers,omitempty"`
}

type K8sContainer struct {
	Name          string `json:"name"`
	Image         string `json:"image"`
	Ready         bool   `json:"ready"`
	Restarts      int    `json:"restarts"`
	State         string `json:"state"`
	InitContainer bool   `json:"init_container,omitempty"`
}

type K8sRef struct {
	APIVersion string `json:"api_version,omitempty"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
}

type K8sManifest struct {
	Ref             K8sRef `json:"ref"`
	ResourceVersion string `json:"resource_version"`
	YAML            string `json:"yaml"`
}

type K8sDocument struct {
	Source string `json:"source"`
	YAML   string `json:"yaml"`
}

type K8sApplyParams struct {
	Documents []K8sDocument `json:"documents"`
	Namespace string        `json:"namespace,omitempty"`
	DryRun    bool          `json:"dry_run,omitempty"`
}

type K8sApplyResult struct {
	Objects []K8sApplied `json:"objects"`
}

type K8sApplied struct {
	Source string  `json:"source,omitempty"`
	Ref    *K8sRef `json:"ref,omitempty"`
	Action string  `json:"action"`
	Error  string  `json:"error,omitempty"`
}

type K8sUpdateParams struct {
	Ref  K8sRef `json:"ref"`
	YAML string `json:"yaml"`
}

type K8sScaleParams struct {
	Ref      K8sRef `json:"ref"`
	Replicas int    `json:"replicas"`
}

type K8sLogsParams struct {
	Namespace  string `json:"namespace"`
	Pod        string `json:"pod"`
	Container  string `json:"container,omitempty"`
	Follow     bool   `json:"follow,omitempty"`
	Previous   bool   `json:"previous,omitempty"`
	TailLines  int    `json:"tail_lines,omitempty"`
	Timestamps bool   `json:"timestamps,omitempty"`
}

type K8sLog struct {
	Subscription uint64 `json:"subscription"`
	Text         string `json:"text"`
}

type K8sLogEnd struct {
	Subscription uint64 `json:"subscription"`
	Error        string `json:"error,omitempty"`
}

type K8sExecParams struct {
	Namespace string   `json:"namespace"`
	Pod       string   `json:"pod"`
	Container string   `json:"container,omitempty"`
	Argv      []string `json:"argv,omitempty"`
	TTY       bool     `json:"tty,omitempty"`
	Rows      int      `json:"rows,omitempty"`
	Cols      int      `json:"cols,omitempty"`
}

type K8sExecStarted struct {
	ID        uint64 `json:"id"`
	Token     string `json:"token"`
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
}

type K8sResetResult struct {
	FreedBytes  int64 `json:"freed_bytes"`
	ArchiveKept bool  `json:"archive_kept"`
}
