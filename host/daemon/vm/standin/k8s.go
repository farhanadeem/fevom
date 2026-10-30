package standin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"fev/host/k8s"
	api "fev/proto/agent"
	"fev/proto/jsonrpc"
	"fev/proto/netplan"
)

const (
	standinK3sVersion     = "v1.37.1+k3s1"
	standinArchiveSize    = 73728162 + 176489629
	standinArchiveSHA     = "b3bd28d3a20144927b25ebb1d255a16ce55bb050b9af5e63abd1bf91c0838fe5"
	standinArchiveURL     = "https://github.com/k3s-io/k3s/releases/download/v1.37.1%2Bk3s1/k3s-airgap-images-arm64.tar.zst"
	standinCacheBytes     = 440213504
	standinK8sBytes       = 23789568
	standinK8sImagesBytes = 612368384
	standinOfflineError   = "Turning on Kubernetes the first time downloads it (250 MB) from github.com, which could not be reached: the stand-in is offline. Nothing was changed; try again when online, or set kubernetes.images_url."
	standinShortError     = "Turning on Kubernetes the first time needs 1.0 GiB of the data disk's budget for k3s, its images and its first start, and 0.4 GiB is left. Nothing was changed."
)

const StandinKubeconfig = "apiVersion: v1\nclusters:\n- cluster:\n    certificate-authority-data: c3RhbmQtaW4=\n    server: https://127.0.0.1:6443\n  name: default\ncontexts:\n- context:\n    cluster: default\n    user: default\n  name: default\ncurrent-context: default\nkind: Config\nusers:\n- name: default\n  user:\n    token: stand-in\n"

type K8sOptions struct {
	Wanted, Cached, Offline, Short bool
	NoCRI                          bool
	StartTime                      time.Duration
	KubeconfigLate                 time.Duration
	CountAsks                      bool
	Asks                           string
	FetchStep                      time.Duration
	ClientStart                    time.Duration
	ReadyAfter                     time.Duration
	Error                          string
	File, Socket                   string
	Plan                           netplan.Plan
}

type k8sState struct {
	mu       sync.Mutex
	o        K8sOptions
	saved    k8sSaved
	state    string
	err      string
	cfg      api.K8sConfig
	conns    map[*jsonrpc.Conn]bool
	ops      uint64
	fake     *fakeCluster
	cri      bool
	restarts int
	asks     int
}

type k8sSaved struct {
	Wanted    bool `json:"wanted"`
	Subvolume bool `json:"subvolume"`
	Cached    bool `json:"cached"`
}

func (a *Agent) SetK8s(o K8sOptions) error {
	k := &a.k8s
	k.mu.Lock()
	defer k.mu.Unlock()
	k.o = o
	k.conns = map[*jsonrpc.Conn]bool{}
	if data, err := os.ReadFile(o.File); o.File != "" && err == nil {
		json.Unmarshal(data, &k.saved)
	}
	if o.Wanted {
		k.saved.Wanted, k.saved.Subvolume = true, true
	}
	if o.Cached {
		k.saved.Cached = true
	}
	k.state = api.K8sOff
	k.fake = newFakeCluster(o.Plan)
	k.fake.startTime, k.fake.readyTime = o.ClientStart, o.ReadyAfter
	if k.saved.Wanted {
		if late := o.StartTime + o.KubeconfigLate; late > 0 {
			k.state = api.K8sStarting
			time.AfterFunc(late, func() {
				k.mu.Lock()
				defer k.mu.Unlock()
				if k.state == api.K8sStarting {
					k.run()
					k.changed()
				}
			})
		} else {
			k.run()
		}
	}
	k.save()
	if o.Socket == "" {
		return nil
	}
	return k.fake.serve(o.Socket, k.running)
}

func (k *k8sState) save() {
	if k.o.File == "" {
		return
	}
	data, _ := json.Marshal(k.saved)
	os.WriteFile(k.o.File, data, 0o600)
}

func (k *k8sState) run() {
	if k.o.Error != "" {
		k.state, k.err = api.K8sError, k.o.Error
		return
	}
	k.state, k.err = api.K8sRunning, ""
}

func (k *k8sState) running() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.state == api.K8sRunning
}

func (k *k8sState) status(withKubeconfig bool) api.K8sStatus {
	st := api.K8sStatus{Wanted: k.saved.Wanted, State: k.state, Subvolume: k.saved.Subvolume, Error: k.err,
		Archive: api.K8sArchive{Cached: k.saved.Cached, Size: standinArchiveSize, SHA256: standinArchiveSHA, URL: standinArchiveURL}}
	if k.saved.Cached {
		st.Archive.Bytes = standinCacheBytes
	}
	if k.saved.Subvolume {
		st.Bytes = standinK8sBytes
	}
	if k.state == api.K8sRunning || k.state == api.K8sError {
		st.CRI, st.Version = true, standinK3sVersion
	}
	if k.cri {
		st.CRI = true
	}
	if k.state == api.K8sRunning {
		st.PID = 2211
		if withKubeconfig {
			st.Kubeconfig = StandinKubeconfig
		}
	}
	return st
}

func (k *k8sState) changed() {
	st := k.status(false)
	for c := range k.conns {
		select {
		case <-c.Done():
			delete(k.conns, c)
		default:
			c.Notify(api.NotifyK8sState, st)
		}
	}
}

func (a *Agent) handleK8s(ctx context.Context, c *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
	k := &a.k8s
	k.mu.Lock()
	if k.conns == nil {
		k.conns = map[*jsonrpc.Conn]bool{}
		k.state = api.K8sOff
		k.fake = newFakeCluster(netplan.Default())
	}
	k.conns[c] = true
	k.mu.Unlock()
	switch method {
	case api.MethodK8sStatus:
		k.mu.Lock()
		defer k.mu.Unlock()
		if k.o.CountAsks {
			k.asks++
			os.WriteFile(k.o.Asks, []byte(strconv.Itoa(k.asks)+"\n"), 0o600)
		}
		return k.status(true), nil
	case api.MethodK8sStart:
		p, err := jsonrpc.Bind[api.K8sStartParams](params)
		if err != nil {
			return nil, err
		}
		return k.start(ctx, c, p), nil
	case api.MethodK8sStop:
		p, err := jsonrpc.Bind[api.K8sStopParams](params)
		if err != nil {
			return nil, err
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		k.saved.Wanted = false
		k.state, k.err = api.K8sOff, ""
		k.setCRI(p.KeepCRI && !k.o.NoCRI)
		k.save()
		k.changed()
		return k.status(false), nil
	case api.MethodK8sCRI:
		if k.o.NoCRI {
			return nil, jsonrpc.Errorf(api.CodeNotImplemented, "k8s.cri is not implemented by this stand-in (k8s-nocri)")
		}
		p, err := jsonrpc.Bind[api.K8sCRIParams](params)
		if err != nil {
			return nil, err
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		if !p.On && (k.state == api.K8sRunning || k.state == api.K8sStarting) {
			return nil, failed("the cluster runs: containerd keeps its CRI plugins until it is stopped")
		}
		k.setCRI(p.On)
		return k.status(false), nil
	case api.MethodK8sReset:
		p, err := jsonrpc.Bind[api.K8sResetParams](params)
		if err != nil {
			return nil, err
		}
		return k.reset(ctx, c, p), nil
	case api.MethodK8sConfigure:
		cfg, err := jsonrpc.Bind[api.K8sConfig](params)
		if err != nil {
			return nil, err
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		k.cfg = cfg
		return k.status(false), nil
	case api.MethodK8sImagePull:
		p, err := jsonrpc.Bind[api.K8sImagePullParams](params)
		if err != nil {
			return nil, err
		}
		if !k.running() {
			return nil, failed("the cluster is not running")
		}
		sum := sha256.Sum256([]byte(p.Ref))
		return api.K8sImagePulled{Ref: p.Ref, Digest: "sha256:" + hex.EncodeToString(sum[:])}, nil
	}
	return nil, jsonrpc.Errorf(api.CodeNotImplemented, "%s is not implemented by the stand-in agent", method)
}

func failed(text string) error { return jsonrpc.Errorf(api.CodeFailed, "%s", text) }

func (k *k8sState) setCRI(on bool) {
	if k.cri != on {
		k.restarts++
	}
	k.cri = on
}

func (a *Agent) K8sCRIRestarts() int {
	a.k8s.mu.Lock()
	defer a.k8s.mu.Unlock()
	return a.k8s.restarts
}

func (k *k8sState) start(ctx context.Context, c *jsonrpc.Conn, p api.K8sStartParams) api.Operation {
	k.mu.Lock()
	k.ops++
	id := 300 + k.ops
	k.cfg = p.Config
	cached, offline, short, delay, step := k.saved.Cached, k.o.Offline, k.o.Short, k.o.StartTime, k.o.FetchStep
	k.mu.Unlock()
	note := func(pr api.OperationProgress) {
		pr.ID, pr.Method = id, api.MethodK8sStart
		if pr.State == "" {
			pr.State = api.OperationRunning
		}
		c.Notify(api.NotifyOperationProgress, pr)
	}
	jsonrpc.AfterReply(ctx, func() {
		go func() {
			note(api.OperationProgress{Phase: api.K8sPhaseChecking})
			if !cached {
				if short {
					note(api.OperationProgress{State: api.OperationFailed, Phase: api.K8sPhaseChecking, Error: standinShortError})
					return
				}
				if offline {
					note(api.OperationProgress{State: api.OperationFailed, Phase: api.K8sPhaseFetching, Error: standinOfflineError})
					return
				}
				for _, done := range []int64{0, standinArchiveSize / 2, standinArchiveSize} {
					note(api.OperationProgress{Phase: api.K8sPhaseFetching, DoneBytes: done, TotalBytes: standinArchiveSize})
					time.Sleep(step)
				}
				for _, ph := range []string{api.K8sPhaseVerifying, api.K8sPhaseImporting} {
					note(api.OperationProgress{Phase: ph})
				}
			}
			note(api.OperationProgress{Phase: api.K8sPhaseStarting})
			k.mu.Lock()
			k.setCRI(true)
			k.state = api.K8sStarting
			k.saved.Wanted, k.saved.Subvolume, k.saved.Cached = true, true, true
			k.save()
			k.changed()
			k.mu.Unlock()
			time.Sleep(delay + k.o.KubeconfigLate)
			k.mu.Lock()
			k.run()
			st := k.status(true)
			k.changed()
			k.mu.Unlock()
			result, _ := json.Marshal(st)
			note(api.OperationProgress{State: api.OperationDone, Phase: api.K8sPhaseStarting, Result: result})
		}()
	})
	return api.Operation{ID: id}
}

func (k *k8sState) reset(ctx context.Context, c *jsonrpc.Conn, p api.K8sResetParams) api.Operation {
	k.mu.Lock()
	k.ops++
	id := 300 + k.ops
	k.mu.Unlock()
	jsonrpc.AfterReply(ctx, func() {
		go func() {
			c.Notify(api.NotifyOperationProgress, api.OperationProgress{ID: id, Method: api.MethodK8sReset, State: api.OperationRunning, Phase: api.K8sPhaseStopping})
			k.mu.Lock()
			freed := int64(0)
			if k.saved.Subvolume {
				freed += standinK8sBytes
			}
			if p.All && k.saved.Cached {
				freed += standinCacheBytes
				k.saved.Cached = false
			}
			k.saved.Wanted, k.saved.Subvolume = false, false
			k.state, k.err = api.K8sOff, ""
			k.setCRI(p.KeepCRI && !k.o.NoCRI)
			k.fake.reset()
			k.save()
			k.changed()
			kept := k.saved.Cached
			k.mu.Unlock()
			result, _ := json.Marshal(api.K8sResetResult{FreedBytes: freed, ArchiveKept: kept})
			c.Notify(api.NotifyOperationProgress, api.OperationProgress{ID: id, Method: api.MethodK8sReset, State: api.OperationDone, Phase: api.K8sPhaseDeleting, Result: result})
		}()
	})
	return api.Operation{ID: id}
}

func k8sOptions(set map[string]string, cmdline, vsockDir string) K8sOptions {
	o := K8sOptions{
		Wanted:    set["k8s"] == "on",
		Cached:    set["k8s-cached"] != "",
		Offline:   set["k8s-offline"] != "",
		Short:     set["k8s-short"] != "",
		NoCRI:     set["k8s-nocri"] != "",
		Error:     set["k8s-error"],
		Plan:      netplan.Default(),
		StartTime: 0,
	}
	if ms, err := strconv.Atoi(set["k8s-start-ms"]); err == nil {
		o.StartTime = time.Duration(ms) * time.Millisecond
	}
	if ms, err := strconv.Atoi(set["k8s-kubeconfig-ms"]); err == nil {
		o.KubeconfigLate = time.Duration(ms) * time.Millisecond
	}
	if ms, err := strconv.Atoi(set["k8s-fetch-ms"]); err == nil {
		o.FetchStep = time.Duration(ms) * time.Millisecond
	}
	if ms, err := strconv.Atoi(set["k8s-client-ms"]); err == nil {
		o.ClientStart = time.Duration(ms) * time.Millisecond
	}
	if ms, err := strconv.Atoi(set["k8s-ready-ms"]); err == nil {
		o.ReadyAfter = time.Duration(ms) * time.Millisecond
	}
	for _, w := range strings.Fields(cmdline) {
		switch {
		case w == "fev.k8s=1":
			o.Wanted = true
		case strings.HasPrefix(w, "fev.machnet="):
			if p, err := netplan.Parse(strings.TrimPrefix(w, "fev.machnet=")); err == nil {
				o.Plan = p
			}
		}
	}
	if vsockDir != "" {
		o.File = filepath.Join(vsockDir, "standin-k8s.json")
		o.Socket = K8sSocket(vsockDir)
		if set["k8s-count-asks"] != "" {
			o.CountAsks, o.Asks = true, filepath.Join(vsockDir, "standin-k8s-asks")
		}
	}
	return o
}

func K8sSocket(dir string) string { return filepath.Join(dir, "k8s.sock") }

var _ k8s.Backend = (*fakeCluster)(nil)
