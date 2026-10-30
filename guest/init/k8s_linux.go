package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/internal/k8s"
	"fev/guest/internal/logbuf"
	"fev/guest/internal/netcfg"
	"fev/guest/internal/reaper"
	"fev/guest/internal/registry"
	"fev/guest/storage"
)

const (
	k8sData         = "/data/" + storage.K8sSubvolume
	k8sCache        = "/data/" + storage.K8sCacheSubvolume
	k8sPinFile      = systemMount + "/k8s/archive.json"
	k8sImagesDir    = "/usr/lib/k3s-images"
	k8sStopWait     = k8sStopLimit + 60*time.Second
	k8sStartWait    = 3 * time.Minute
	k8sReadyWait    = 2 * time.Minute
	k8sStartRoom    = 1 << 30
	k8sPayloadRoom  = 300 << 20
	registryAddr    = "127.0.0.1:6449"
	registryProgram = systemMount + "/bin/fev-registry"
)

type k8sBind struct {
	source, target string
	ro             bool
}

var k8sDataBinds = []struct{ sub, target string }{
	{"k3s", "/var/lib/rancher/k3s"},
	{"storage", "/var/lib/rancher/k3s/storage"},
	{"kubelet", "/var/lib/kubelet"},
	{"pods", "/var/log/pods"},
	{"etc", "/etc/rancher"},
	{"cni", "/var/lib/cni"},
}

type k8sGuest struct {
	s *system

	mu      sync.Mutex
	wanted  bool
	cfg     api.K8sConfig
	sup     supK8s
	changed chan struct{}
	said    int
	purges  int
	drained chan string
	busy    string
	sysctls string
	reg     *k8sRegistry
	criMade []string
}

type supK8s struct {
	state      string
	pid        int
	cri        bool
	importedMS int64
	err        string
	serving    bool
}

var k8sOnce sync.Once

func (s *system) cluster() *k8sGuest {
	k8sOnce.Do(func() {
		k := &k8sGuest{s: s, changed: make(chan struct{}), sup: supK8s{state: api.K8sOff}}
		var b strings.Builder
		for _, name := range kubeletSysctls {
			if v, err := os.ReadFile("/proc/sys/" + name); err == nil {
				fmt.Fprintf(&b, "%s %s\n", name, strings.TrimSpace(string(v)))
			}
		}
		k.sysctls = b.String()
		k.reg = &k8sRegistry{s: s}
		if netcfg.Arg(s.cmdline, api.K8sCmdlineParam) == "1" {
			if why := k.cacheMissing(); why != "" {
				s.log.Logf(logbuf.Warning, "k8s: the boot line asks for the cluster, but %s; the daemon's k8s.start fetches it", why)
			} else {
				k.wanted = true
			}
		}
		s.k8s = k
	})
	return s.k8s
}

func (k *k8sGuest) pin() (k8s.Pin, error) { return k8s.ReadPin(k8sPinFile) }

func archivePath(p k8s.Pin) string { return filepath.Join(k8sCache, "images", p.File()) }

var k8sCacheDirs = []string{"images", "k3s/data", "download"}

func k3sDownload(p k8s.Pin) string { return filepath.Join(k8sCache, "download", p.K3s().File()) }

func (k *k8sGuest) payloadReady(p k8s.Pin) bool {
	mark, err := os.ReadFile(filepath.Join(k8sCache, "k3s", "data", p.Payload, ".fev-verified"))
	return err == nil && strings.TrimSpace(string(mark)) == p.Payload
}

func (k *k8sGuest) cacheMissing() string {
	p, err := k.pin()
	if err != nil {
		return err.Error()
	}
	if st, err := os.Stat(archivePath(p)); err != nil || st.Size() != p.Size {
		return "the airgap archive is not in the cache"
	}
	if !k.payloadReady(p) {
		return "k3s is not in the cache"
	}
	if _, err := os.Stat(k8sData); err != nil {
		return "the cluster has no subvolume"
	}
	return ""
}

func (k *k8sGuest) binds() []k8sBind {
	var out []k8sBind
	for _, b := range k8sDataBinds {
		out = append(out, k8sBind{source: filepath.Join(k8sData, b.sub), target: b.target})
	}
	out = append(out, k8sBind{source: filepath.Join(k8sCache, "images"), target: k8sImagesDir, ro: true},
		k8sBind{source: filepath.Join(k8sCache, "k3s", "data"), target: k3sUnpacked, ro: true})
	return out
}

func (k *k8sGuest) wantedNow() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.wanted
}

func k8sRunFile(name string) string {
	return filepath.Join(engineRun, filepath.Base(engineFevDir), "k8s", name)
}

func (k *k8sGuest) prepareRun() error {
	if err := os.MkdirAll(k8sRunFile(""), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(k8sRunFile("sysctl.before"), []byte(k.sysctls), 0o644); err != nil {
		return err
	}
	for dir, files := range map[string]map[string]string{"certs.d": k8s.RegistryHosts(registryAddr), "never.d": k8s.NeverHosts(registryAddr)} {
		for name, text := range files {
			p := filepath.Join(k8sRunFile(dir), name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
				return err
			}
		}
	}
	want := "off"
	if k.wantedNow() {
		if err := k.writeStartFiles(); err != nil {
			return err
		}
		want = "on"
	}
	return os.WriteFile(k8sRunFile("want"), []byte(want+"\n"), 0o644)
}

func (k *k8sGuest) writeStartFiles() error {
	p := k.s.plan
	k.mu.Lock()
	cfg := k.cfg
	k.mu.Unlock()
	args := k8s.K3sArgs(k8s.Ranges{Pods: p.KubePods(), Services: p.KubeServices(), DNS: p.KubeDNS()}, containerdSocket, k8s.Options{MetricsServer: cfg.MetricsServer})
	if err := os.WriteFile(k8sRunFile("args"), []byte(k8s.Words(args)), 0o644); err != nil {
		return err
	}
	if pin, err := k.pin(); err == nil {
		line := filepath.Join(k8sImagesDir, pin.File()) + " " + pin.SHA256 + "\n"
		if err := os.WriteFile(k8sRunFile("archive"), []byte(line), 0o644); err != nil {
			return err
		}
	}
	if err := k8sManifests(); err != nil {
		return err
	}
	return k.writeDropins(cfg)
}

func (k *k8sGuest) writeDropins(cfg api.K8sConfig) error {
	files, err := k8s.KubeletDropins(cfg.Kubelet)
	if err != nil {
		return err
	}
	dir := filepath.Join(k8sData, "k3s", strings.TrimPrefix(k8s.KubeletDir, k8s.DataDir+"/"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{k8s.FevDropin, k8s.UserDropin} {
		text, ok := files[name]
		if !ok {
			os.Remove(filepath.Join(dir, name))
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), text, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (k *k8sGuest) signal() error {
	e := k.s.engine
	e.mu.Lock()
	run := e.cur
	e.mu.Unlock()
	if run == nil {
		return errors.New("the engine is not running")
	}
	if !k.s.reaper.Signal(run.child, unix.SIGUSR2, false) {
		return errors.New("the engine's supervisor has ended")
	}
	return nil
}

func (k *k8sGuest) reported(word, rest string) {
	switch word {
	case reportK8s:
		f := strings.Fields(rest)
		if len(f) < 3 {
			return
		}
		s := supK8s{state: f[0]}
		s.pid, _ = strconv.Atoi(f[1])
		s.cri = f[2] == "true"
		for i, w := range f[3:] {
			if w == "serving" {
				s.serving = true
			}
			if v, ok := strings.CutPrefix(w, "imported_ms="); ok {
				s.importedMS, _ = strconv.ParseInt(v, 10, 64)
			}
			if v, ok := strings.CutPrefix(w, "error="); ok {
				s.err = strings.Join(append([]string{v}, f[3+i+1:]...), " ")
				break
			}
		}
		k.mu.Lock()
		prev := k.sup
		k.sup = s
		k.said++
		if s.state == "purged" {
			k.purges++
		}
		close(k.changed)
		k.changed = make(chan struct{})
		k.mu.Unlock()
		if prev.state != s.state {
			k.s.log.Logf(logbuf.Info, "k8s: %s", s.state)
		}
		k.notify()
	case reportK8sDrained:
		k.s.log.Logf(logbuf.Info, "k8s: pods stopped with their grace (containers, sandboxes, longest ms: %s)", rest)
		k.mu.Lock()
		ch := k.drained
		k.drained = nil
		k.mu.Unlock()
		if ch != nil {
			ch <- rest
		}
	case reportK8sSysctl:
		k.s.log.Logf(logbuf.Info, "k8s: the kubelet's %s put back", rest)
	}
}

func (k *k8sGuest) notify() {
	st := k.status(false)
	if link := k.s.host.Load(); link != nil {
		link.Notify(api.NotifyK8sState, st)
	}
}

func (k *k8sGuest) wait(ctx context.Context, d time.Duration, states ...string) (supK8s, error) {
	return k.waitAfter(ctx, -1, d, states...)
}

func (k *k8sGuest) mark() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.said
}

func (k *k8sGuest) waitAfter(ctx context.Context, mark int, d time.Duration, states ...string) (supK8s, error) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		k.mu.Lock()
		s, ch, said := k.sup, k.changed, k.said
		k.mu.Unlock()
		for _, w := range states {
			if s.state == w && said > mark {
				return s, nil
			}
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return s, ctx.Err()
		case <-timer.C:
			return s, fmt.Errorf("the cluster is still %s after %v", s.state, d)
		}
	}
}

func (k *k8sGuest) waitServing(d time.Duration) error {
	timer := time.NewTimer(max(d, 0))
	defer timer.Stop()
	for {
		k.mu.Lock()
		s, ch := k.sup, k.changed
		k.mu.Unlock()
		if s.serving {
			return nil
		}
		select {
		case <-ch:
		case <-timer.C:
			return errors.New("containerd does not serve again (the cluster is " + s.state + ")")
		}
	}
}

func (k *k8sGuest) waitPurged(from int, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		k.mu.Lock()
		n, ch, st := k.purges, k.changed, k.sup.state
		k.mu.Unlock()
		if n > from {
			return nil
		}
		select {
		case <-ch:
		case <-timer.C:
			return fmt.Errorf("k8s.io was not emptied in %v (the cluster is %s)", d, st)
		}
	}
}

func (k *k8sGuest) begin(what string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.busy != "" {
		return fmt.Errorf("the cluster is busy: %s is under way", k.busy)
	}
	k.busy = what
	return nil
}

func (k *k8sGuest) end() {
	k.mu.Lock()
	k.busy = ""
	k.mu.Unlock()
}

func (s *system) K8sStart(ctx context.Context, p api.K8sStartParams, progress func(string, int64, int64)) (api.K8sStatus, error) {
	k := s.cluster()
	if err := k.begin(api.MethodK8sStart); err != nil {
		return k.status(false), err
	}
	defer k.end()
	if s.engine.sup.Status().State != api.EngineReady {
		return k.status(false), errors.New("the Docker engine is not running")
	}
	progress(api.K8sPhaseChecking, 0, 0)
	pin, err := k.pin()
	if err != nil {
		return k.status(false), err
	}
	vol, err := s.volume()
	if err != nil {
		return k.status(false), err
	}
	if err := k.room(pin); err != nil {
		return k.status(false), err
	}
	cache, _, err := vol.EnsureK8sCacheSubvolume()
	if err != nil {
		return k.status(false), err
	}
	for _, d := range k8sCacheDirs {
		if err := os.MkdirAll(filepath.Join(cache, d), 0o755); err != nil {
			return k.status(false), err
		}
	}
	type part struct {
		pin       k8s.Pin
		src, dest string
		need      bool
	}
	final := archivePath(pin)
	arch, archErr := os.Stat(final)
	parts := []part{
		{pin.K3s(), pin.K3sSource(p.ArchiveURL), filepath.Join(cache, "download", pin.K3s().File()), !k.payloadReady(pin)},
		{pin, p.ArchiveURL, filepath.Join(cache, "images", pin.File()), archErr != nil || arch.Size() != pin.Size},
	}
	var total, done int64
	for _, pt := range parts {
		if pt.need {
			total += pt.pin.Size
		}
	}
	f := &k8s.Fetcher{Client: &http.Client{Timeout: 30 * time.Minute}, Files: s.macPath,
		Logf: func(format string, args ...any) { s.log.Logf(logbuf.Info, "k8s: "+format, args...) }}
	for _, pt := range parts {
		if !pt.need {
			continue
		}
		began := time.Now()
		err := f.Fetch(ctx, pt.src, pt.pin, pt.dest, func(have, _ int64) { progress(api.K8sPhaseFetching, done+have, total) })
		var off *k8s.ErrOffline
		if errors.As(err, &off) {
			return k.status(false), errors.New(k8s.OfflineLine(pin, off))
		}
		if err != nil {
			return k.status(false), err
		}
		done += pt.pin.Size
		s.log.Logf(logbuf.Info, "k8s: %s fetched and verified in %d ms", pt.pin.File(), time.Since(began).Milliseconds())
	}
	if total > 0 {
		progress(api.K8sPhaseVerifying, total, total)
	}
	removeOthers(filepath.Join(cache, "images"), pin.File())
	if !k.payloadReady(pin) {
		if err := k.extract(cache, pin); err != nil {
			return k.status(false), err
		}
	}
	if _, _, err := vol.EnsureK8sSubvolume(); err != nil {
		return k.status(false), err
	}
	if err := k8sNested(); err != nil {
		return k.status(false), err
	}
	k.mu.Lock()
	k.cfg = p.Config
	k.wanted = true
	k.mu.Unlock()
	if err := k.writeStartFiles(); err != nil {
		return k.status(false), err
	}
	if err := s.engine.k8sBindRun(true); err != nil {
		return k.status(false), err
	}
	k.reg.arm()
	if err := os.WriteFile(k8sRunFile("want"), []byte("on\n"), 0o644); err != nil {
		return k.status(false), err
	}
	s.log.Logf(logbuf.Info, "k8s: start")
	mark := k.mark()
	if err := k.signal(); err != nil {
		return k.status(false), err
	}
	st, err := k.awaitRunning(ctx, mark, progress)
	if err == nil {
		k.mu.Lock()
		k.criMade = nil
		k.mu.Unlock()
	}
	return st, err
}

func (k *k8sGuest) awaitRunning(ctx context.Context, mark int, progress func(string, int64, int64)) (api.K8sStatus, error) {
	deadline := time.Now().Add(k8sStartWait)
	for {
		s, err := k.waitAfter(ctx, mark, time.Until(deadline), api.K8sRunning, api.K8sError, "importing", api.K8sOff)
		if err != nil {
			return k.status(false), err
		}
		switch s.state {
		case "importing":
			progress(api.K8sPhaseImporting, 0, 0)
			if _, err := k.wait(ctx, time.Until(deadline), api.K8sRunning, api.K8sError, api.K8sStarting, api.K8sOff); err != nil {
				return k.status(false), err
			}
			progress(api.K8sPhaseStarting, 0, 0)
			continue
		case api.K8sError:
			return k.status(false), errors.New(s.err)
		case api.K8sOff:
			return k.status(false), errors.New("the cluster was turned off while it started")
		}
		break
	}
	progress(api.K8sPhaseStarting, 0, 0)
	if err := k.apiReady(ctx); err != nil {
		return k.status(false), err
	}
	return k.status(true), nil
}

func (k *k8sGuest) apiReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, k8sReadyWait)
	defer cancel()
	var last error
	for {
		c, err := kubeClient()
		if err == nil {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://127.0.0.1:6443/readyz", nil)
			resp, err2 := c.Do(req)
			if err2 == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					c.CloseIdleConnections()
					return nil
				}
				err2 = fmt.Errorf("readyz: %s", resp.Status)
			}
			c.CloseIdleConnections()
			last = err2
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the cluster's API did not answer in %v: %v", k8sReadyWait, last)
		case <-time.After(250 * time.Millisecond):
		}
		k.mu.Lock()
		st := k.sup.state
		k.mu.Unlock()
		if st != api.K8sRunning && st != api.K8sStarting {
			return fmt.Errorf("the cluster is %s", st)
		}
	}
}

func kubeClient() (*http.Client, error) {
	data, err := os.ReadFile(filepath.Join(k8sData, "etc", "k3s", "k3s.yaml"))
	if err != nil {
		return nil, err
	}
	field := func(name string) ([]byte, error) {
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), name+":"); ok {
				return base64.StdEncoding.DecodeString(strings.TrimSpace(v))
			}
		}
		return nil, fmt.Errorf("k3s.yaml has no %s", name)
	}
	ca, err := field("certificate-authority-data")
	if err != nil {
		return nil, err
	}
	cert, err := field("client-certificate-data")
	if err != nil {
		return nil, err
	}
	key, err := field("client-key-data")
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{pair}}}}, nil
}

func (k *k8sGuest) room(pin k8s.Pin) error {
	var need int64
	if st, err := os.Stat(archivePath(pin)); err != nil || st.Size() != pin.Size {
		need += pin.Size
	}
	if !k.payloadReady(pin) {
		need += k8sPayloadRoom
		if st, err := os.Stat(k3sDownload(pin)); err != nil || st.Size() != pin.K3sSize {
			need += pin.K3sSize
		}
	}
	if _, err := os.Stat(filepath.Join(k8sData, "k3s", filepath.Base(k8sImported))); err != nil {
		need += k8sStartRoom
	}
	var st unix.Statfs_t
	if err := unix.Statfs(engineData, &st); err != nil {
		return err
	}
	free := int64(st.Bavail) * st.Bsize
	if free < need {
		return fmt.Errorf("Turning on Kubernetes needs %.1f GB free for k3s, its images and its first start, and fev has %.1f GB (the Mac's free space less fev's margin). Nothing was changed.", float64(need)/1e9, float64(free)/1e9)
	}
	return nil
}

func (k *k8sGuest) extract(cache string, pin k8s.Pin) error {
	began := time.Now()
	bin := filepath.Join(cache, "download", pin.K3s().File())
	out, err := k.s.reaper.Run(5*time.Minute, registryProgram, "unpack", "-hash", pin.Payload, "-check=false", bin, filepath.Join(cache, "k3s"), filepath.Dir(k3sUnpacked))
	if err != nil {
		return fmt.Errorf("extract k3s's payload: %v: %s", err, lastLines(out, 3))
	}
	k.s.log.Logf(logbuf.Info, "k8s: k3s's payload extracted in %d ms: %s", time.Since(began).Milliseconds(), lastLines(out, 1))
	data := filepath.Join(cache, "k3s", "data")
	go func() {
		out, err := k.s.reaper.Run(5*time.Minute, registryProgram, "check", filepath.Join(cache, "k3s"), pin.Payload)
		if err != nil {
			k.s.log.Logf(logbuf.Error, "k8s: k3s's payload failed its check; the next enable extracts it again: %v: %s", err, lastLines(out, 2))
			return
		}
		k.s.log.Logf(logbuf.Info, "k8s: k3s's payload %s", lastLines(out, 1))
		os.Remove(bin)
	}()
	time.AfterFunc(payloadDropAfter, func() { dropPageCache(data, k.s.log) })
	return nil
}

const payloadDropAfter = 30 * time.Second

func dropPageCache(dir string, log *logbuf.Buffer) {
	var files, bytes int64
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil
		}
		defer unix.Close(fd)
		unix.SyncFileRange(fd, 0, 0, unix.SYNC_FILE_RANGE_WAIT_BEFORE|unix.SYNC_FILE_RANGE_WRITE|unix.SYNC_FILE_RANGE_WAIT_AFTER)
		if unix.Fadvise(fd, 0, 0, unix.FADV_DONTNEED) == nil {
			files++
			if st, err := d.Info(); err == nil {
				bytes += st.Size()
			}
		}
		return nil
	})
	log.Logf(logbuf.Info, "k8s: the payload's page cache given back (%d files, %d MB)", files, bytes/1e6)
}

func removeOthers(dir, keep string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.Name() != keep {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

func (s *system) K8sStop(p api.K8sStopParams) (api.K8sStatus, error) {
	k := s.cluster()
	if err := k.begin(api.MethodK8sStop); err != nil {
		return k.status(false), err
	}
	defer k.end()
	want := "off"
	if p.KeepCRI {
		want = "off-cri"
	}
	return k.down(want, api.K8sOff)
}

func (k *k8sGuest) down(want, until string) (api.K8sStatus, error) {
	k.mu.Lock()
	k.wanted = false
	k.mu.Unlock()
	e := k.s.engine
	if e.sup.Status().State != api.EngineReady {
		k.reg.disarm()
		return k.status(false), nil
	}
	if err := os.WriteFile(k8sRunFile("want"), []byte(want+"\n"), 0o644); err != nil {
		return k.status(false), err
	}
	mark := k.mark()
	k.mu.Lock()
	purges := k.purges
	k.mu.Unlock()
	if err := k.signal(); err != nil {
		return k.status(false), err
	}
	began := time.Now()
	if want == "reset" || want == "reset-cri" {
		if err := k.waitPurged(purges, k8sStopWait); err != nil {
			return k.status(false), err
		}
		mark = k.mark() - 1
	}
	if _, err := k.waitAfter(context.Background(), mark, k8sStopWait, until); err != nil {
		return k.status(false), err
	}
	if err := k.waitServing(time.Until(began.Add(k8sStopWait))); err != nil {
		return k.status(false), err
	}
	k.reg.disarm()
	if err := e.k8sBindRun(false); err != nil {
		return k.status(false), err
	}
	k.s.log.Logf(logbuf.Info, "k8s: %s in %d ms", want, time.Since(began).Milliseconds())
	return k.status(false), nil
}

func (s *system) K8sReset(ctx context.Context, p api.K8sResetParams, progress func(string, int64, int64)) (api.K8sResetResult, error) {
	k := s.cluster()
	if err := k.begin(api.MethodK8sReset); err != nil {
		return api.K8sResetResult{}, err
	}
	defer k.end()
	vol, err := s.volume()
	if err != nil {
		return api.K8sResetResult{}, err
	}
	freed := vol.K8sBytes()
	progress(api.K8sPhaseStopping, 0, 0)
	want := "reset"
	if p.KeepCRI {
		want = "reset-cri"
	}
	if _, err := k.down(want, api.K8sOff); err != nil {
		return api.K8sResetResult{}, err
	}
	progress(api.K8sPhaseDeleting, 0, 0)
	if err := vol.RemoveK8sSubvolume(); err != nil {
		return api.K8sResetResult{}, err
	}
	r := api.K8sResetResult{FreedBytes: freed, ArchiveKept: true}
	if p.All {
		cached := vol.K8sCacheBytes()
		if err := vol.RemoveK8sCacheSubvolume(); err != nil {
			return r, err
		}
		r.FreedBytes += cached
		r.ArchiveKept = false
	}
	s.log.Logf(logbuf.Info, "k8s: reset (all %v, cri kept %v, %d bytes freed)", p.All, p.KeepCRI, r.FreedBytes)
	k.notify()
	return r, nil
}

func (s *system) K8sConfigure(cfg api.K8sConfig) (api.K8sStatus, error) {
	k := s.cluster()
	k.mu.Lock()
	k.cfg = cfg
	wanted := k.wanted
	k.mu.Unlock()
	if !wanted || s.engine.sup.Status().State != api.EngineReady {
		return k.status(false), nil
	}
	if err := k.writeStartFiles(); err != nil {
		return k.status(false), err
	}
	if err := os.WriteFile(k8sRunFile("ask"), []byte("restart\n"), 0o644); err != nil {
		return k.status(false), err
	}
	s.log.Logf(logbuf.Info, "k8s: the kubelet's settings changed; k3s starts again")
	return k.status(false), k.signal()
}

func (s *system) K8sStatus() api.K8sStatus { return s.cluster().status(true) }

func (k *k8sGuest) status(withKubeconfig bool) api.K8sStatus {
	k.mu.Lock()
	sup, wanted := k.sup, k.wanted
	k.mu.Unlock()
	st := api.K8sStatus{Wanted: wanted, State: sup.state, PID: sup.pid, CRI: sup.cri, ImportedMS: sup.importedMS}
	switch sup.state {
	case "importing":
		st.State = api.K8sStarting
	case "purged", "":
		st.State = api.K8sOff
	case api.K8sError:
		st.Error = sup.err
	}
	if pin, err := k.pin(); err == nil {
		st.Version = pin.Version
		st.Archive = api.K8sArchive{Size: pin.Total(), SHA256: pin.SHA256, URL: pin.URL}
		if f, err := os.Stat(archivePath(pin)); err == nil && f.Size() == pin.Size && k.payloadReady(pin) {
			st.Archive.Cached = true
		}
	}
	if _, err := os.Stat(k8sData); err == nil {
		st.Subvolume = true
	}
	if vol, err := k.s.volume(); err == nil {
		st.Bytes = vol.K8sBytes()
		st.Archive.Bytes = vol.K8sCacheBytes()
	}
	if withKubeconfig && st.State == api.K8sRunning {
		if data, err := os.ReadFile(filepath.Join(k8sData, "etc", "k3s", "k3s.yaml")); err == nil {
			st.Kubeconfig = string(data)
		}
	}
	return st
}

func (k *k8sGuest) relayTarget(a netip.Addr) bool {
	k.mu.Lock()
	on := k.sup.state == api.K8sRunning || k.sup.state == api.K8sStarting
	k.mu.Unlock()
	return on && (k.s.plan.KubeServices().Contains(a) || k.s.plan.KubePods().Contains(a))
}

func (k *k8sGuest) drainForStop(limit time.Duration) <-chan string {
	k.mu.Lock()
	running := k.sup.state != api.K8sOff && k.sup.state != "" && k.sup.state != "purged"
	if !running {
		k.mu.Unlock()
		return nil
	}
	ch := make(chan string, 1)
	k.drained = ch
	k.mu.Unlock()
	secs := int(limit / time.Second)
	if err := os.WriteFile(k8sRunFile("ask"), []byte(fmt.Sprintf("drain %d\n", secs)), 0o644); err != nil {
		return nil
	}
	if err := k.signal(); err != nil {
		return nil
	}
	return ch
}

func (s *system) K8sImagePull(ctx context.Context, ref string) (api.K8sImagePulled, error) {
	k := s.cluster()
	k.mu.Lock()
	st := k.sup.state
	k.mu.Unlock()
	if st != api.K8sRunning {
		return api.K8sImagePulled{}, fmt.Errorf("the cluster is %s", st)
	}
	full := normalizeRef(ref)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	argv := append(k3sTool("ctr"), "--address", containerdSocket, "-n", "k8s.io", "images", "pull",
		"--hosts-dir", filepath.Join(k8sRunDir, "never.d"), full)
	out, err := s.engineRun(ctx, argv)
	if err != nil {
		return api.K8sImagePulled{}, fmt.Errorf("pull %s through fev's registry: %v: %s", full, err, lastLines(out, 3))
	}
	store := registry.NewContainerd(engineRun+"/docker/containerd/containerd.sock", "k8s.io", "")
	d, err := store.Image(ctx, full)
	if err != nil {
		return api.K8sImagePulled{}, fmt.Errorf("pull %s: not in k8s.io after the pull: %v", full, err)
	}
	s.log.Logf(logbuf.Info, "k8s: %s pulled from Docker's images (%s)", full, d.Digest)
	return api.K8sImagePulled{Ref: full, Digest: d.Digest}, nil
}

func normalizeRef(ref string) string {
	name, digest, _ := strings.Cut(ref, "@")
	first, _, hasSlash := strings.Cut(name, "/")
	if !hasSlash || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		if !hasSlash {
			name = "library/" + name
		}
		name = "docker.io/" + name
	}
	if digest != "" {
		return name + "@" + digest
	}
	last := name[strings.LastIndex(name, "/")+1:]
	if !strings.Contains(last, ":") {
		name += ":latest"
	}
	return name
}

func lastLines(b []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

func (s *system) engineRun(ctx context.Context, argv []string) ([]byte, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		r.Close()
		w.Close()
		return nil, err
	}
	defer null.Close()
	var child *reaper.Child
	err = s.engine.In(func(cgroup int) error {
		var err error
		child, err = s.reaper.Start(argv[0], argv, &os.ProcAttr{
			Dir: "/", Env: []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"},
			Files: []*os.File{null, w, w},
			Sys:   &syscall.SysProcAttr{Setsid: true, UseCgroupFD: true, CgroupFD: cgroup},
		})
		return err
	})
	w.Close()
	if err != nil {
		r.Close()
		return nil, err
	}
	done := make(chan []byte, 1)
	go func() {
		var b bytes.Buffer
		b.ReadFrom(r)
		r.Close()
		done <- b.Bytes()
	}()
	exited := make(chan unix.WaitStatus, 1)
	go func() { exited <- child.Wait() }()
	select {
	case ws := <-exited:
		out := <-done
		if ws.Signaled() {
			return out, fmt.Errorf("%s: killed by signal %d", filepath.Base(argv[0]), ws.Signal())
		}
		if ws.ExitStatus() != 0 {
			return out, fmt.Errorf("%s: exit %d", filepath.Base(argv[0]), ws.ExitStatus())
		}
		return out, nil
	case <-ctx.Done():
		s.reaper.Signal(child, unix.SIGKILL, false)
		<-exited
		return <-done, ctx.Err()
	}
}

func (e *engine) k8sBindRun(on bool) error {
	e.mu.Lock()
	run := e.cur
	e.mu.Unlock()
	if run == nil {
		return errors.New("the engine is not running")
	}
	if run.k8sBound.Load() == on {
		return nil
	}
	binds := e.sys.cluster().binds()
	var fds []int
	defer func() {
		for _, fd := range fds {
			unix.Close(fd)
		}
	}()
	if on {
		for _, b := range binds {
			fd, err := unix.OpenTree(unix.AT_FDCWD, b.source, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
			if err != nil {
				return fmt.Errorf("clone %s: %w", b.source, err)
			}
			fds = append(fds, fd)
		}
	}
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		run.ns.RLock()
		defer run.ns.RUnlock()
		if run.mnt == nil {
			result <- errors.New("the engine has stopped")
			return
		}
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			result <- err
			return
		}
		if err := unix.Setns(int(run.mnt.Fd()), unix.CLONE_NEWNS); err != nil {
			result <- err
			return
		}
		if !on {
			for _, t := range k8sUnbindTargets() {
				if err := unix.Unmount(t, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
					result <- fmt.Errorf("unmount %s: %w", t, err)
					return
				}
			}
			result <- nil
			return
		}
		for i, b := range binds {
			if err := os.MkdirAll(b.target, 0o755); err != nil {
				result <- err
				return
			}
			if err := unix.MoveMount(fds[i], "", unix.AT_FDCWD, b.target, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
				result <- fmt.Errorf("mount %s on %s: %w", b.source, b.target, err)
				return
			}
			flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT)
			if b.ro {
				flags |= unix.MS_RDONLY
			}
			if err := unix.Mount("", b.target, "", flags, ""); err != nil {
				result <- fmt.Errorf("remount %s: %w", b.target, err)
				return
			}
			if err := unix.Mount("", b.target, "", unix.MS_SHARED, ""); err != nil {
				result <- fmt.Errorf("share %s: %w", b.target, err)
				return
			}
		}
		result <- nil
	}()
	err := <-result
	if err == nil {
		run.k8sBound.Store(on)
	}
	return err
}

func k8sUnbindTargets() []string {
	out := []string{k3sUnpacked, k8sImagesDir}
	for i := len(k8sDataBinds) - 1; i >= 0; i-- {
		out = append(out, k8sDataBinds[i].target)
	}
	return out
}

func k8sNested() error {
	for _, b := range k8sDataBinds {
		if err := os.MkdirAll(filepath.Join(k8sData, b.sub), 0o755); err != nil {
			return err
		}
		for _, outer := range k8sDataBinds {
			if rel, ok := strings.CutPrefix(b.target, outer.target+"/"); ok {
				if err := os.MkdirAll(filepath.Join(k8sData, outer.sub, rel), 0o755); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (k *k8sGuest) k8sStartSteps() []k8sBind {
	if !k.wantedNow() {
		return nil
	}
	if err := k8sNested(); err != nil {
		k.s.log.Logf(logbuf.Warning, "k8s: the cluster's parts: %v", err)
	}
	return k.binds()
}

func (s *system) macPath(path string) string {
	for _, b := range api.SharePaths {
		if rest, ok := strings.CutPrefix(path, b.Target); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
			return shareMount + b.Source + rest
		}
	}
	return shareMount + path
}

func k8sStatusJSON(st api.K8sStatus) string {
	st.Kubeconfig = ""
	b, _ := json.Marshal(st)
	return string(b)
}
