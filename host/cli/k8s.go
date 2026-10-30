package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"fev/host/daemon/home"
	"fev/host/k8s/kubeconfig"
	api "fev/proto/daemon"
)

func (a *app) k8sCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "k8s",
		Short: "Run a Kubernetes cluster beside the Docker engine",
		Long: `Run a single-node Kubernetes cluster (k3s) beside the Docker engine, off
until it is started. Its images are Docker's: an image built with "docker
build" runs in a pod with no push. Its services are reached from this Mac
by name (https://NAME.k8s.fev.local), at localhost:NODEPORT, and at their
pod and cluster addresses; kubectl uses the context "fev", which fev
adds to your kubeconfig while the cluster is on.

With no command, the same as "fev k8s status".`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error { return a.k8sStatus(cmd.Context()) },
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Say whether the cluster is on, and what it serves",
		Long: `Say whether the cluster is on: its node and version, the API's address on
this Mac, the kubeconfig and its context, the address ranges, every
service with its URLs and every ingress with its own, the kubelet settings
changed, and what the cluster costs while on. It starts nothing.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error { return a.k8sStatus(cmd.Context()) },
	}
	start := &cobra.Command{
		Use:   "start",
		Short: "Turn the cluster on",
		Long: `Turn the cluster on, and keep it on across fev's restarts until "fev
k8s stop". The first start downloads Kubernetes, k3s and its images
(about 240 MiB, once), from github.com, or from beside kubernetes.images_url;
a later one needs no network. It returns when the cluster runs.

The context "fev" is added to your kubeconfig ($KUBECONFIG's first file,
else ~/.kube/config); it becomes the current context only if the file had
none.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withDaemon(cmd.Context(), true, func(c *api.Client) error {
				before, _ := c.K8sStatus(cmd.Context())
				end, err := a.k8sFollow(cmd.Context(), c, "k8s start", func() (*api.Operation, error) { return c.K8sEnable(cmd.Context()) })
				if err != nil {
					return err
				}
				if err := ended("k8s start", end); err != nil {
					return err
				}
				st, err := c.K8sStatus(cmd.Context())
				if err != nil {
					return err
				}
				if a.json() || a.quiet {
					return a.emit(st, nil)
				}
				if st.State == api.K8sStateRunning {
					fmt.Fprintln(a.Stdout, "Kubernetes is running.")
				} else {
					fmt.Fprintf(a.Stdout, "Kubernetes is %s.\n", st.State)
				}
				if st.Kubeconfig.Present && (before == nil || !before.Kubeconfig.Present) {
					fmt.Fprintf(a.Stdout, "Added context %s to %s.\n", kubeconfig.Context, tildePath(st.Kubeconfig.Path))
				}
				if st.Kubeconfig.Present {
					fmt.Fprintf(a.Stdout, "kubectl --context %s get nodes\n", kubeconfig.Context)
				}
				a.k8sNotes(a.Stdout, st)
				if st.Cost != "" {
					fmt.Fprintln(a.Stdout, st.Cost+".")
				}
				return nil
			})
		},
	}
	stop := &cobra.Command{
		Use:   "stop",
		Short: "Turn the cluster off",
		Long: `Turn the cluster off: its pods are stopped with their grace periods, then
k3s and everything of the cluster's ends, and the context "fev" is taken
out of the kubeconfig. Docker's containers keep running. The cluster's
data is kept for the next "fev k8s start"; "fev k8s reset" deletes it.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.connect(cmd.Context())
			if err == errNotRunning {
				return a.withDaemon(cmd.Context(), false, func(c *api.Client) error {
					if _, err := c.ConfigSet(cmd.Context(), map[string]any{api.KeyKubernetesEnabled: false}); err != nil {
						return err
					}
					return a.k8sOff(cmd.Context(), c)
				})
			}
			if err != nil {
				return unreachable(err)
			}
			defer c.Close()
			end, err := a.k8sFollow(cmd.Context(), c, "k8s stop", func() (*api.Operation, error) { return c.K8sDisable(cmd.Context()) })
			if err != nil {
				return err
			}
			if err := ended("k8s stop", end); err != nil {
				return err
			}
			return a.k8sOff(cmd.Context(), c)
		},
	}
	var yes, all bool
	reset := &cobra.Command{
		Use:   "reset",
		Short: "Delete the cluster's data; Docker's is kept",
		Long: `Turn the cluster off and delete everything of it: its workloads, volumes,
images and settings in the cluster. Docker's images, containers and
volumes are untouched. The downloaded Kubernetes images are kept, so the
next start needs no network; --all deletes them too.

It asks for the word "reset" to be typed; --yes is for scripts.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				fmt.Fprintf(a.Stderr, "This deletes the Kubernetes cluster's workloads, volumes and images. Docker's are kept.\nType %q to go on: ", resetWord)
				line, _ := bufio.NewReader(a.Stdin).ReadString('\n')
				if strings.TrimSpace(line) != resetWord {
					return failf(ExitFailed, "not reset: %q was not typed", resetWord)
				}
			}
			return a.withDaemon(cmd.Context(), true, func(c *api.Client) error {
				end, err := a.k8sFollow(cmd.Context(), c, "k8s reset", func() (*api.Operation, error) {
					return c.K8sReset(cmd.Context(), api.K8sResetParams{All: all})
				})
				if err != nil {
					return err
				}
				if err := ended("k8s reset", end); err != nil {
					return err
				}
				var r api.K8sResetResult
				if len(end.Result) > 0 {
					if err := json.Unmarshal(end.Result, &r); err != nil {
						return fmt.Errorf("k8s reset: the result: %w", err)
					}
				}
				return a.emit(r, func(w io.Writer) {
					kv(w, "freed", bytesText(r.FreedBytes))
					if r.ArchiveKept {
						kv(w, "images", "the downloaded Kubernetes images are kept; the next start needs no network")
					} else {
						kv(w, "images", "deleted; the next start downloads them again")
					}
					kv(w, "kubernetes", "off")
				})
			})
		},
	}
	reset.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	reset.Flags().BoolVar(&all, "all", false, "also delete the downloaded Kubernetes images")
	cmd.AddCommand(status, start, stop, reset, a.kubeconfigCmd())
	return cmd
}

func (a *app) k8sOff(ctx context.Context, c *api.Client) error {
	st, err := c.K8sStatus(ctx)
	if err != nil {
		return err
	}
	return a.emit(st, func(w io.Writer) {
		fmt.Fprintln(w, "Kubernetes is off.")
		if st.Kubeconfig.Path != "" && !st.Kubeconfig.Present {
			fmt.Fprintf(w, "The context %s is not in %s any more.\n", kubeconfig.Context, tildePath(st.Kubeconfig.Path))
		}
	})
}

func (a *app) k8sStatus(ctx context.Context) error {
	c, err := a.connect(ctx)
	if err == errNotRunning {
		return failf(ExitNotRunning, "fev is not running; \"fev k8s start\" starts it with the cluster")
	}
	if err != nil {
		return unreachable(err)
	}
	defer c.Close()
	st, err := c.K8sStatus(ctx)
	if err != nil {
		return err
	}
	if a.quiet {
		_, err := fmt.Fprintln(a.Stdout, st.State)
		return err
	}
	return a.emit(st, func(w io.Writer) { a.k8sStatusText(w, st) })
}

func (a *app) k8sStatusText(w io.Writer, st *api.K8sStatus) {
	state := st.State
	if st.State == api.K8sStateRunning {
		if st.Ready {
			state += ", ready"
		} else {
			state += ", the node not ready yet"
		}
	}
	if st.Error != "" {
		state += ": " + st.Error
	}
	kv(w, "state", state)
	if !st.Enabled && st.State == api.K8sStateOff {
		if st.Archive.Cached {
			kv(w, "start", `"fev k8s start" (Kubernetes is downloaded)`)
		} else {
			kv(w, "start", fmt.Sprintf(`"fev k8s start" downloads Kubernetes (%s) once`, bytesText(st.Archive.Size)))
		}
		kv(w, "cost", st.Cost)
		return
	}
	if st.Node != "" || st.Version != "" {
		kv(w, "node", strings.TrimPrefix(strings.Join([]string{st.Node, st.Version}, ", "), ", "))
	}
	if st.API.Address != "" {
		kv(w, "api", "https://"+st.API.Address)
	}
	if st.Kubeconfig.Path != "" {
		k := tildePath(st.Kubeconfig.Path) + ", context " + st.Kubeconfig.Context
		switch {
		case st.Kubeconfig.Current:
			k += " (current)"
		case !st.Kubeconfig.Present:
			k += " (not there)"
		}
		kv(w, "kubeconfig", k)
	}
	kv(w, "ranges", fmt.Sprintf("load balancers %s, pods %s, services %s, dns %s",
		st.Ranges.LoadBalancers, st.Ranges.Pods, st.Ranges.Services, st.Ranges.DNS))
	if st.ClusterLocal.Answered {
		kv(w, "cluster.local", "answered on this Mac")
	} else if st.ClusterLocal.Note != "" {
		kv(w, "cluster.local", st.ClusterLocal.Note)
	}
	if len(st.Services) > 0 {
		fmt.Fprintln(w, "services:")
		var rows [][]string
		for _, s := range st.Services {
			addr := s.ClusterIP
			if s.LoadBalancer != "" {
				addr = s.LoadBalancer
			}
			rows = append(rows, []string{"  " + s.Namespace + "/" + s.Name, s.Type, addr, k8sPortsText(s.Ports), strings.Join(s.URLs, " ")})
		}
		table(w, []string{"  NAME", "TYPE", "ADDRESS", "PORTS", "URLS"}, rows)
	}
	if len(st.Ingresses) > 0 {
		fmt.Fprintln(w, "ingresses:")
		var rows [][]string
		for _, in := range st.Ingresses {
			rows = append(rows, []string{"  " + in.Namespace + "/" + in.Name, strings.Join(in.Hosts, ","), in.Address, strings.Join(in.URLs, " ")})
		}
		table(w, []string{"  NAME", "HOSTS", "ADDRESS", "URLS"}, rows)
	}
	if len(st.KubeletChanged) > 0 {
		kv(w, "kubelet", strings.Join(st.KubeletChanged, ", ")+" (kubernetes.kubelet)")
	}
	if st.Archive.Cached {
		kv(w, "images", fmt.Sprintf("downloaded, %s (kept by a reset; \"fev k8s reset --all\" deletes them)", bytesText(st.Archive.Bytes)))
	}
	a.k8sNotes(w, st)
	kv(w, "cost", st.Cost)
}

func (a *app) k8sNotes(w io.Writer, st *api.K8sStatus) {
	if st.API.Note != "" {
		kv(w, "note", st.API.Note)
	}
	for _, r := range st.RangeConflicts {
		kv(w, "note", r)
	}
	if len(st.PortConflicts) > 0 {
		kv(w, "note", "these ports are held by other programs on this Mac and not forwarded: "+strings.Join(st.PortConflicts, ", ")+` ("fev ports" says more)`)
	}
	if st.Kubeconfig.Present && st.Kubeconfig.Path != "" {
		if env := os.Getenv("KUBECONFIG"); env != "" && !namesFile(env, st.Kubeconfig.Path) {
			kv(w, "note", fmt.Sprintf("this shell's KUBECONFIG does not name %s, where the context is: add it, or use kubectl --kubeconfig %s",
				tildePath(st.Kubeconfig.Path), st.Kubeconfig.Path))
		}
	}
}

func namesFile(list, path string) bool {
	for _, p := range filepath.SplitList(list) {
		if p != "" && filepath.Clean(p) == filepath.Clean(path) {
			return true
		}
	}
	return false
}

func k8sPortsText(ports []api.K8sPort) string {
	var out []string
	for _, p := range ports {
		s := fmt.Sprintf("%d/%s", p.Port, strings.ToLower(p.Protocol))
		if p.NodePort != 0 {
			s = fmt.Sprintf("%d:%d/%s", p.Port, p.NodePort, strings.ToLower(p.Protocol))
		}
		out = append(out, s)
	}
	return strings.Join(out, ",")
}

func tildePath(path string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		if path == h {
			return "~"
		}
		if strings.HasPrefix(path, h+"/") {
			return "~" + path[len(h):]
		}
	}
	return path
}

var k8sPhaseText = map[string]string{
	api.K8sPhaseChecking:  "checking the space and the addresses",
	api.K8sPhaseFetching:  "downloading Kubernetes",
	api.K8sPhaseVerifying: "checking the download and unpacking k3s",
	api.K8sPhaseImporting: "importing the images",
	api.K8sPhaseStarting:  "starting the cluster",
	api.K8sPhaseReady:     "ready",
	api.K8sPhaseStopping:  "stopping the cluster",
	api.K8sPhaseDeleting:  "deleting the cluster's data",
}

func (a *app) k8sFollow(ctx context.Context, c *api.Client, what string, start func() (*api.Operation, error)) (api.Operation, error) {
	ops := &operations{early: map[string][]api.Operation{}}
	var finish func()
	if !a.quiet && !a.json() {
		tty := false
		if f, ok := a.Stderr.(*os.File); ok {
			tty = isTerminal(f)
		}
		phase, open := "", false
		var last time.Time
		ops.moved = func(op api.Operation) {
			if op.Phase == api.K8sPhaseFetching && op.TotalBytes > 0 {
				line := fmt.Sprintf("fev: %s: Downloading Kubernetes: %d of %d MB", what, op.DoneBytes/1e6, op.TotalBytes/1e6)
				switch {
				case tty && (time.Since(last) > 100*time.Millisecond || op.DoneBytes == op.TotalBytes):
					fmt.Fprintf(a.Stderr, "\r\033[K%s", line)
					open, last = true, time.Now()
				case !tty && phase != op.Phase:
					fmt.Fprintf(a.Stderr, "fev: %s: downloading Kubernetes (%d MB, once)\n", what, op.TotalBytes/1e6)
				}
				phase = op.Phase
				return
			}
			if op.Phase == "" || op.Phase == phase {
				return
			}
			if open {
				fmt.Fprintln(a.Stderr)
				open = false
			}
			phase = op.Phase
			words := k8sPhaseText[phase]
			if words == "" {
				words = strings.ReplaceAll(phase, "_", " ")
			}
			fmt.Fprintf(a.Stderr, "fev: %s: %s\n", what, words)
		}
		finish = func() {
			if open {
				fmt.Fprintln(a.Stderr)
			}
		}
	}
	_, unsubscribe, err := c.Subscribe(ctx, api.Events{Operation: ops.seen})
	if err != nil {
		return api.Operation{}, err
	}
	defer unsubscribe()
	op, err := start()
	if err != nil {
		return api.Operation{}, err
	}
	select {
	case end := <-ops.wait(op.ID):
		if finish != nil {
			ops.mu.Lock()
			finish()
			ops.mu.Unlock()
		}
		return end, nil
	case <-c.Done():
		return api.Operation{}, failf(ExitUnreachable, "%s: the daemon's connection ended before it did", what)
	case <-ctx.Done():
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c.OperationCancel(cctx, op.ID)
		return api.Operation{}, ctx.Err()
	}
}

func (a *app) kubeconfigCmd() *cobra.Command {
	var print, remove bool
	cmd := &cobra.Command{
		Use:   "kubeconfig",
		Short: "Say where the context fev is, print it, or remove it",
		Long: `Say which kubeconfig the context "fev" is in while the cluster is on.

--print prints a kubeconfig of that context alone (cluster-admin: keep it
to yourself), for a tool that takes a file. --remove takes the context out
of the kubeconfig now; the next start of the cluster adds it again. Neither
starts fev, and --remove works with fev stopped.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if print && remove {
				return usagef("k8s kubeconfig: --print or --remove, not both")
			}
			l, err := a.layout()
			if err != nil {
				return err
			}
			path, err := a.kubeconfigPath(cmd.Context(), l)
			if err != nil {
				return err
			}
			switch {
			case remove:
				r, err := kubeconfig.Remove(kubeconfig.NotePath(l.Home), path)
				if err != nil {
					return failf(ExitFailed, "k8s kubeconfig --remove: %v", err)
				}
				return a.emit(r, func(w io.Writer) {
					switch {
					case r.Path == "":
						fmt.Fprintln(w, "fev has put no context into a kubeconfig.")
					case r.Nothing:
						fmt.Fprintf(w, "%s has no context %s.\n", tildePath(r.Path), kubeconfig.Context)
					case r.Restored:
						fmt.Fprintf(w, "Removed context %s: %s is as it was before fev added it.\n", kubeconfig.Context, tildePath(r.Path))
					default:
						fmt.Fprintf(w, "Removed context %s from %s.\n", kubeconfig.Context, tildePath(r.Path))
					}
				})
			case print:
				if path == "" {
					return failf(ExitFailed, "k8s kubeconfig --print: fev has put no context into a kubeconfig; \"fev k8s start\" does")
				}
				b, err := kubeconfig.Extract(path)
				if err != nil {
					return failf(ExitFailed, "k8s kubeconfig --print: %v", err)
				}
				_, err = a.Stdout.Write(b)
				return err
			}
			present, current := false, false
			if path != "" {
				present, current = kubeconfig.Present(path)
			}
			k := api.K8sKubeconfig{Path: path, Context: kubeconfig.Context, Present: present, Current: current}
			return a.emit(k, func(w io.Writer) {
				if path == "" {
					fmt.Fprintln(w, "fev has put no context into a kubeconfig; \"fev k8s start\" does.")
					return
				}
				kv(w, "file", tildePath(path))
				state := "not there (the cluster is off)"
				if present {
					state = "there"
					if current {
						state += ", the current context"
					}
				}
				kv(w, "context "+kubeconfig.Context, state)
			})
		},
	}
	cmd.Flags().BoolVar(&print, "print", false, "print a kubeconfig of the context fev alone")
	cmd.Flags().BoolVar(&remove, "remove", false, "take the context fev out of the kubeconfig")
	return cmd
}

func (a *app) kubeconfigPath(ctx context.Context, l home.Layout) (string, error) {
	if c, err := a.connect(ctx); err == nil {
		defer c.Close()
		if st, err := c.K8sStatus(ctx); err == nil && st.Kubeconfig.Path != "" {
			return st.Kubeconfig.Path, nil
		}
	}
	n, err := kubeconfig.ReadNote(kubeconfig.NotePath(l.Home))
	if err != nil {
		return "", err
	}
	if n != nil {
		return n.Path, nil
	}
	return "", nil
}

func k8sLine(l *api.K8sLine) string {
	s := l.State
	if l.State == api.K8sStateRunning && l.Ready {
		s += ", ready"
	}
	if l.Version != "" {
		s += " (" + l.Version + ")"
	}
	if l.Error != "" {
		s += ": " + l.Error
	}
	return s
}
