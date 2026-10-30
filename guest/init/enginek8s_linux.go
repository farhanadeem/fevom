package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"fev/guest/internal/cri"
	"fev/guest/internal/timerslack"
)

const (
	k8sRunDir        = engineFevDir + "/k8s"
	k8sWant          = k8sRunDir + "/want"
	k8sAsk           = k8sRunDir + "/ask"
	k8sArgs          = k8sRunDir + "/args"
	k8sArchive       = k8sRunDir + "/archive"
	k8sSysctls       = k8sRunDir + "/sysctl.before"
	k8sImported      = "/var/lib/rancher/k3s/.fev-airgap-imported"
	k8sKubeconfigDir = "/etc/rancher/k3s"
	k3sUnpacked      = "/usr/lib/k3s/data"
	k8sStopLimit     = 60 * time.Second
	k3sStopWait      = 10 * time.Second
	k3sLoopEnds      = 5
	k3sLoopWindow    = time.Minute
)

var heldSysctls = []string{"kernel/panic", "kernel/panic_on_oops"}

var kubeletSysctls = []string{"vm/overcommit_memory", "vm/panic_on_oom", "kernel/panic", "kernel/panic_on_oops", "kernel/keys/root_maxkeys", "kernel/keys/root_maxbytes"}

type k8sStage struct {
	want                           string
	k3s, importer, cleaner, purger int
	k3sStopping, k3sHeld, k3sUp    bool
	restart                        bool
	dirty                          bool
	importFailed                   bool
	importAt                       time.Time
	importMS                       int64
	draining, drained              bool
	drainLimit                     time.Duration
	stopDrain                      bool
	reported                       string
	ends                           []time.Time
	failure                        string
	watch                          *os.File
	before                         map[string]string
}

func (st *engineStage) readWant() {
	k := &st.k8s
	k.reported = ""
	data, err := os.ReadFile(k8sWant)
	k.want = "off"
	if err == nil {
		k.want = strings.TrimSpace(string(data))
	}
	if k.want == "on" {
		k.importFailed = false
		k.failure = ""
		k.ends = nil
	}
	ask, err := os.ReadFile(k8sAsk)
	if err != nil {
		return
	}
	os.Remove(k8sAsk)
	word, arg, _ := strings.Cut(strings.TrimSpace(string(ask)), " ")
	switch word {
	case "drain":
		n, _ := strconv.Atoi(arg)
		k.drainLimit, k.stopDrain = time.Duration(n)*time.Second, true
		st.logf("k8s: asked to drain for the engine's stop (each pod at most %v)", k.drainLimit)
		if !k.dirty && k.k3s == 0 {
			k.drained = true
			fmt.Fprintf(st.report, "%s 0 0 0\n", reportK8sDrained)
		}
	case "restart":
		if k.k3s > 0 && !k.k3sStopping {
			k.restart = true
			st.stopK3s()
		}
	}
}

func (st *engineStage) wantCRI() bool {
	switch st.k8s.want {
	case "on", "cri", "off-cri", "reset-cri":
		return !st.stopping
	}
	return false
}

func (k *k8sStage) resetting() bool { return k.want == "reset" || k.want == "reset-cri" }

func (k *k8sStage) idle() bool {
	return k.k3s == 0 && k.importer == 0 && k.cleaner == 0 && k.purger == 0 && !k.draining
}

func (k *k8sStage) mayRestartContainerd() bool {
	return k.idle() && !(k.dirty && k.want != "on")
}

func (st *engineStage) stopK3s() {
	k := &st.k8s
	unix.Kill(k.k3s, unix.SIGTERM)
	k.k3sStopping = true
	st.after(k3sStopWait, fmt.Sprintf("k3s-kill %d", k.k3s))
}

func (st *engineStage) k8sStopping() {
	k := &st.k8s
	if k.k3s > 0 && !k.k3sStopping {
		st.stopK3s()
	}
	for _, p := range []int{k.importer, k.purger} {
		if p > 0 {
			unix.Kill(p, unix.SIGKILL)
		}
	}
}

func (st *engineStage) k8sReconcile() {
	k := &st.k8s
	if k.stopDrain {
		if k.k3s > 0 && !k.k3sStopping {
			st.stopK3s()
		}
		if k.importer > 0 {
			unix.Kill(k.importer, unix.SIGKILL)
		}
		if k.k3s == 0 && k.dirty && !k.draining && !k.drained {
			st.startDrain(k.drainLimit)
		}
		return
	}
	switch k.want {
	case "on":
		if st.ctr > 0 && st.ctrReady && st.ctrCRI && !st.ctrStopping && k.cleaner == 0 && k.purger == 0 && !k.draining {
			if st.needImport() {
				if k.importer == 0 {
					st.startImport()
				}
			} else if k.importer == 0 && k.k3s == 0 && !k.k3sHeld && !k.importFailed {
				st.startK3s()
			}
		}
	case "off", "reset", "cri", "off-cri", "reset-cri":
		if k.importer > 0 {
			unix.Kill(k.importer, unix.SIGKILL)
		}
		if k.k3s > 0 && !k.k3sStopping {
			st.stopK3s()
		}
		if k.k3s == 0 && k.importer == 0 && k.dirty && !k.draining && !k.drained {
			st.startDrain(k8sStopLimit)
		}
		if k.k3s == 0 && k.importer == 0 && k.dirty && k.drained && k.cleaner == 0 {
			k.cleaner = st.startShell(k8sCleanScript)
			if k.cleaner == 0 {
				k.dirty = false
			}
		}
		if k.resetting() && k.k3s == 0 && k.cleaner == 0 && !k.dirty && k.purger == 0 && st.ctr > 0 && st.ctrReady && !st.ctrStopping {
			k.purger = st.startShell(k8sPurgeScript)
		}
	}
}

func (st *engineStage) k8sEvent(word, arg string, pid int) {
	k := &st.k8s
	switch word {
	case "k3s-kill":
		if pid == k.k3s && k.k3sStopping {
			st.logf("k8s: k3s (pid %d) still running %v after SIGTERM; killing it", pid, k3sStopWait)
			unix.Kill(pid, unix.SIGKILL)
		}
	case "k3s-release":
		k.k3sHeld = false
	case "drained":
		k.draining, k.drained = false, true
		fmt.Fprintf(st.report, "%s %s\n", reportK8sDrained, arg)
		st.logf("k8s: pods stopped: %s", arg)
	case "kubeconfig":
		if k.k3s > 0 && !k.k3sUp {
			k.k3sUp = true
			st.logf("k8s: k3s wrote its kubeconfig")
		}
	case "sysctl":
		st.keepSysctl(arg)
	}
}

func (st *engineStage) k8sReaped(pid int, ws unix.WaitStatus) {
	k := &st.k8s
	switch pid {
	case k.k3s:
		planned := k.k3sStopping
		k.k3s, k.k3sStopping, k.k3sUp = 0, false, false
		st.closeWatch()
		st.logf("k8s: k3s ended (%v)", ws)
		if k.restart {
			k.restart = false
		} else if !planned && k.want == "on" && !st.stopping && !k.stopDrain {
			now := time.Now()
			kept := k.ends[:0]
			for _, t := range k.ends {
				if now.Sub(t) < k3sLoopWindow {
					kept = append(kept, t)
				}
			}
			k.ends = append(kept, now)
			if len(k.ends) >= k3sLoopEnds {
				k.importFailed = true
				k.failure = fmt.Sprintf("k3s ended %d times in %v (%v the last); the guest's log has its words", len(k.ends), k3sLoopWindow, ws)
				st.logf("k8s: %s", k.failure)
			} else {
				k.k3sHeld = true
				st.after(2*time.Second, "k3s-release")
			}
		}
	case k.importer:
		k.importer = 0
		if ws.Exited() && ws.ExitStatus() == 0 {
			k.importMS = time.Since(k.importAt).Milliseconds()
			_, sum := archiveFile()
			os.WriteFile(k8sImported, []byte(sum+"\n"), 0o644)
			st.logf("k8s: archive imported in %d ms", k.importMS)
		} else if k.want == "on" {
			k.importFailed = true
			k.failure = fmt.Sprintf("the airgap archive's import failed (%v); the guest's log has ctr's words", ws)
			st.logf("k8s: the import failed (%v)", ws)
		}
	case k.cleaner:
		k.cleaner, k.dirty, k.drained = 0, false, false
		st.logf("k8s: cluster cleaned (%v)", ws)
		reclaimLeaves()
	case k.purger:
		k.purger = 0
		st.logf("k8s: k8s.io purged (%v)", ws)
		after := "off"
		if k.want == "reset-cri" {
			after = "cri"
		}
		os.WriteFile(k8sWant, []byte(after+"\n"), 0o644)
		k.want = after
		k.reported = ""
		fmt.Fprintf(st.report, "%s purged 0 %v\n", reportK8s, st.ctrCRI)
	}
}

func archiveFile() (string, string) {
	data, err := os.ReadFile(k8sArchive)
	if err != nil {
		return "", ""
	}
	path, sum, _ := strings.Cut(strings.TrimSpace(string(data)), " ")
	return path, sum
}

func (st *engineStage) needImport() bool {
	if st.k8s.importFailed {
		return false
	}
	path, sum := archiveFile()
	if path == "" {
		return false
	}
	mark, err := os.ReadFile(k8sImported)
	return err != nil || strings.TrimSpace(string(mark)) != sum
}

func (st *engineStage) k3sExec() (string, []string) {
	env := st.envWith("")
	dir := k3sUnpacked + "/current"
	if d, err := filepath.EvalSymlinks(dir); err == nil {
		dir = d
	}
	for i, e := range env {
		if p, ok := strings.CutPrefix(e, "PATH="); ok {
			env[i] = "PATH=" + strings.Join([]string{k3sUnpacked + "/cni", dir + "/bin", p, dir + "/bin/aux"}, ":")
		}
	}
	return dir + "/bin/k3s", env
}

func k3sTool(name string) []string {
	return []string{k3sUnpacked + "/current/bin/" + name}
}

func k3sArgv(program string) []string {
	argv := []string{program}
	data, err := os.ReadFile(k8sArgs)
	if err != nil {
		return argv
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			argv = append(argv, line)
		}
	}
	return argv
}

func (st *engineStage) startK3s() {
	k := &st.k8s
	os.MkdirAll(k8sKubeconfigDir, 0o755)
	st.openWatch()
	program, env := st.k3sExec()
	pid, err := timerslack.StartWith(timerslack.Cluster, func() (int, error) { return st.spawn("k3s", k3sArgv(program), env) })
	if err != nil {
		st.closeWatch()
		st.logf("k8s: start k3s: %v", err)
		k.importFailed = true
		k.failure = fmt.Sprintf("k3s could not be started: %v", err)
		return
	}
	k.k3s, k.dirty, k.drained = pid, true, false
	st.logf("k8s: k3s started (pid %d, %s)", pid, program)
}

func (st *engineStage) startImport() {
	k := &st.k8s
	path, _ := archiveFile()
	argv := append(k3sTool("ctr"), "--address", containerdSocket, "-n", "k8s.io", "images", "import", "--label", "io.cri-containerd.pinned=pinned", path)
	pid, err := st.spawn("k3s", argv, st.envWith(""))
	if err != nil {
		st.logf("k8s: import: %v", err)
		k.importFailed = true
		k.failure = fmt.Sprintf("the airgap archive's import could not be started: %v", err)
		return
	}
	k.importer, k.importAt, k.dirty = pid, time.Now(), true
	st.logf("k8s: importing %s (pid %d)", path, pid)
}

func (st *engineStage) startShell(script string) int {
	pid, err := st.spawn("k3s", []string{"/bin/sh", "-c", script}, st.envWith(""))
	if err != nil {
		st.logf("k8s: %v", err)
		return 0
	}
	return pid
}

func (st *engineStage) startDrain(limit time.Duration) {
	k := &st.k8s
	k.draining = true
	st.logf("k8s: stopping the pods (each at most %v)", limit)
	go func() {
		c := cri.New(containerdSocket)
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), limit+20*time.Second)
		defer cancel()
		d := c.Drain(ctx, limit)
		line := fmt.Sprintf("%d %d %d", d.Containers, d.Sandboxes, d.Longest.Milliseconds())
		if len(d.Errors) > 0 {
			line += " " + strings.ReplaceAll(strings.Join(d.Errors, "; "), "\n", " ")
		}
		st.events <- "drained " + line
	}()
}

func (st *engineStage) openWatch() {
	k := &st.k8s
	st.closeWatch()
	if k.before == nil {
		k.before = readSysctls(k8sSysctls)
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		st.logf("k8s: inotify: %v", err)
		return
	}
	names := map[int]string{}
	for _, s := range heldSysctls {
		if _, ok := k.before[s]; !ok {
			continue
		}
		wd, err := unix.InotifyAddWatch(fd, "/proc/sys/"+s, unix.IN_MODIFY)
		if err != nil {
			st.logf("k8s: watch %s: %v", s, err)
			continue
		}
		names[wd] = "sysctl " + s
	}
	if wd, err := unix.InotifyAddWatch(fd, k8sKubeconfigDir, unix.IN_CLOSE_WRITE|unix.IN_MOVED_TO); err == nil {
		names[wd] = "kubeconfig"
	} else {
		st.logf("k8s: watch %s: %v", k8sKubeconfigDir, err)
	}
	f := os.NewFile(uintptr(fd), "k8s-watch")
	k.watch = f
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			if err != nil || n <= 0 {
				return
			}
			for off := 0; off+unix.SizeofInotifyEvent <= n; {
				ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
				name := ""
				if ev.Len > 0 {
					name = strings.TrimRight(string(buf[off+unix.SizeofInotifyEvent:off+unix.SizeofInotifyEvent+int(ev.Len)]), "\x00")
				}
				off += unix.SizeofInotifyEvent + int(ev.Len)
				what, ok := names[int(ev.Wd)]
				if !ok || (what == "kubeconfig" && name != "k3s.yaml") {
					continue
				}
				st.events <- what
			}
		}
	}()
}

func (st *engineStage) closeWatch() {
	k := &st.k8s
	if k.watch != nil {
		k.watch.Close()
	}
	k.watch = nil
}

func (st *engineStage) keepSysctl(name string) {
	want, ok := st.k8s.before[name]
	if !ok {
		return
	}
	got, err := os.ReadFile("/proc/sys/" + name)
	if err != nil || strings.TrimSpace(string(got)) == want {
		return
	}
	if err := os.WriteFile("/proc/sys/"+name, []byte(want+"\n"), 0); err != nil {
		st.logf("k8s: put %s back to %s: %v", name, want, err)
		return
	}
	fmt.Fprintf(st.report, "%s %s %s %s\n", reportK8sSysctl, name, strings.TrimSpace(string(got)), want)
	st.logf("k8s: %s put back from %s to %s", name, strings.TrimSpace(string(got)), want)
}

func readSysctls(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		if name, value, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			out[name] = value
		}
	}
	return out
}

func reclaimLeaves() {
	for _, leaf := range []string{"/sys/fs/cgroup/k3s", "/sys/fs/cgroup/kubepods"} {
		cur, err := os.ReadFile(leaf + "/memory.current")
		if err != nil {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(string(cur)), 10, 64); err == nil && n > 0 {
			reclaimLeaf(leaf, n)
		}
	}
	removeTree("/sys/fs/cgroup/kubepods")
}

func reclaimLeaf(leaf string, n int64) {
	fd, err := unix.Open(leaf+"/memory.reclaim", unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	defer unix.Close(fd)
	unix.Write(fd, []byte(strconv.FormatInt(n, 10)))
}

func removeTree(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() {
			removeTree(filepath.Join(dir, e.Name()))
		}
	}
	unix.Rmdir(dir)
}

func (st *engineStage) k8sState() string {
	k := &st.k8s
	switch {
	case k.importer > 0:
		return "importing"
	case k.k3s > 0 && (k.want != "on" || k.k3sStopping || k.stopDrain):
		return "stopping"
	case k.k3s > 0 && k.k3sUp:
		return "running"
	case k.k3s > 0:
		return "starting"
	case k.cleaner > 0 || k.purger > 0 || k.draining:
		return "stopping"
	case k.want == "on" && k.importFailed:
		return "error"
	case k.want == "on" && !k.stopDrain && !st.stopping:
		return "starting"
	case k.resetting(), k.dirty && !k.stopDrain:
		return "stopping"
	case k.want == "off" && st.ctrCRI:
		return "stopping"
	}
	return "off"
}

func (st *engineStage) ctrServing() bool {
	return st.ctr > 0 && (st.ctrReady || st.ctrWaited) && !st.ctrStopping
}

func (k *k8sStage) tell(st *engineStage) {
	if k.want == "" {
		return
	}
	line := fmt.Sprintf("%s %s %d %v", reportK8s, st.k8sState(), k.k3s, st.ctrCRI)
	if st.ctrServing() {
		line += " serving"
	}
	if k.importMS > 0 {
		line += fmt.Sprintf(" imported_ms=%d", k.importMS)
	}
	if st.k8sState() == "error" {
		why := k.failure
		if why == "" {
			why = "k3s could not be started (the guest's log has why)"
		}
		line += " error=" + why
	}
	if line == k.reported {
		return
	}
	k.reported = line
	fmt.Fprintln(st.report, line)
}

const k8sCleanScript = `
set -u
B=` + k3sUnpacked + `/current/bin CRICTL=` + k3sUnpacked + `/current/bin/crictl
$CRICTL --runtime-endpoint unix://` + containerdSocket + ` --timeout 20s rmp -a -f >/dev/null 2>&1
if [ -f /sys/fs/cgroup/kubepods/cgroup.kill ]; then
	echo 1 > /sys/fs/cgroup/kubepods/cgroup.kill
	i=0; while [ $i -lt 50 ] && grep -q "populated 1" /sys/fs/cgroup/kubepods/cgroup.events; do sleep 0.1; i=$((i+1)); done
fi
for n in /run/netns/cni-*; do [ -e "$n" ] || continue; umount "$n" 2>/dev/null; rm -f "$n"; done
awk '$2 ~ "^/var/lib/kubelet/|^/run/k3s/|^/var/lib/rancher/k3s/agent/containerd/" {print $2}' /proc/self/mounts | sort -r | while read -r m; do umount -l "$m" 2>/dev/null; done
for l in cni0 flannel.1 flannel-v6.1 flannel-wg kube-ipvs0; do ip link delete "$l" 2>/dev/null; done
for v in iptables ip6tables; do
	for t in filter nat mangle raw; do
		$v-save -t $t 2>/dev/null | awk '/^-A (INPUT|OUTPUT|FORWARD|PREROUTING|POSTROUTING) / && (/-j (KUBE-|FLANNEL-|CNI-)/ || /--comment "?(KUBE-ROUTER|kube-router)/)' | sed 's/^-A /-D /' | while read -r r; do eval "$v -t $t $r"; done
		for c in $($v-save -t $t 2>/dev/null | awk -F'[: ]' '/^:(KUBE-|FLANNEL-|CNI-)/ {print $2}'); do $v -t $t -F "$c" 2>/dev/null; done
		for c in $($v-save -t $t 2>/dev/null | awk -F'[: ]' '/^:(KUBE-|FLANNEL-|CNI-)/ {print $2}'); do $v -t $t -X "$c" 2>/dev/null; done
	done
done
# The tables kube-proxy and kube-router made that nothing else uses: gone
# when no rule is left in them (iptables makes one again when asked).
for f in ip ip6; do
	for t in mangle raw; do
		v=iptables; [ $f = ip6 ] && v=ip6tables
		if nft list table $f $t >/dev/null 2>&1 && ! $v-save -t $t 2>/dev/null | grep -q '^-A '; then nft delete table $f $t 2>/dev/null; fi
	done
done
if [ -x $B/ipset ]; then $B/ipset list -n 2>/dev/null | grep '^KUBE-' | while read -r s; do $B/ipset destroy "$s"; done; fi
rm -rf /var/lib/cni/networks/cbr0 /run/flannel
if [ -f ` + k8sSysctls + ` ]; then while read -r k v; do echo "$v" > /proc/sys/$k; done < ` + k8sSysctls + `; fi
exit 0
`

const k8sPurgeScript = `
set -u
CTR="` + k3sUnpacked + `/current/bin/ctr --address ` + containerdSocket + `"
C="$CTR -n k8s.io"
for c in $($C containers ls -q 2>/dev/null); do $C tasks kill -s 9 "$c" 2>/dev/null; $C tasks rm -f "$c" 2>/dev/null; $C containers rm "$c"; done
for s in $($C sandboxes ls 2>/dev/null | awk 'NR > 1 {print $1}'); do $C sandboxes rm -f "$s"; done
$C images ls -q 2>/dev/null | while read -r i; do $C images rm "$i" >/dev/null; done
for l in $($C leases ls -q 2>/dev/null); do $C leases rm "$l"; done
for pass in 1 2 3 4 5 6 7 8 9 10; do
	keys=$($C snapshots ls 2>/dev/null | awk 'NR > 1 {print $1}')
	[ -n "$keys" ] || break
	for k in $keys; do $C snapshots rm "$k" 2>/dev/null; done
done
$C content ls -q 2>/dev/null | while read -r d; do $C content rm "$d" >/dev/null 2>&1; done
$CTR namespaces rm k8s.io
exit 0
`
