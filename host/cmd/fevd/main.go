package main

import (
	"encoding/json"
	"fev/host/k8s/kubeconfig"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"fev/host/bundle"
	"fev/host/daemon"
	"fev/host/daemon/boottrace"
	"fev/host/daemon/home"
	"fev/host/daemon/launch"
	"fev/host/daemon/vm"
	"fev/host/daemon/vm/standin"
	"fev/host/docker"
	"fev/host/docker/dockerctx"
	"fev/host/files"
	"fev/host/images"
	"fev/host/netstack"
	"fev/host/nfsview"
	"fev/host/power"
	"fev/proto/bridge"
	api "fev/proto/daemon"
	priv "fev/proto/privhelper"
	"fev/proto/version"
)

const defaultCmdline = "console=hvc0 quiet swiotlb=noforce"

type spaceOptions struct {
	margin   uint64
	file     string
	noBudget bool
}

var trustFile string

func viewOf(dir string, fakeVM, ownHome bool) *daemon.View {
	switch {
	case dir == "none":
		return nil
	case dir == "" && (fakeVM || ownHome):
		return nil
	case dir == "":
		user, err := os.UserHomeDir()
		if err != nil {
			fail("the view: %v", err)
		}
		dir = filepath.Join(user, "Fevom")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		fail("--view-dir: %v", err)
	}
	v := &daemon.View{Dir: abs}
	if self, err := os.Executable(); err == nil {
		v.Guard = func(dir, source string) (func(), error) { return nfsview.StartGuard([]string{self}, dir, source) }
	}
	return v
}

func modules(layout home.Layout, share *vm.Share, fakeVM, autostart, ownHome bool, noHostLoopback *atomic.Bool, space spaceOptions, imageServer string, sshAgent *daemon.SSHAgent, plan *daemon.Plan, network *daemon.Network, n names, view *daemon.View, emulation *daemon.Emulation) []daemon.Module {
	shared := files.New(files.Options{Share: share})
	var image *daemon.DataImage
	var trimmed, quiet func()
	if !fakeVM || space.file != "" {
		image = &daemon.DataImage{Path: layout.DataImage(), Margin: space.margin, NoBudget: space.noBudget}
		if fakeVM {
			image.SpaceFile = space.file
		}
		trimmed, quiet = image.Trimmed, image.Quiet
	}
	engine := docker.New(docker.Options{
		Trimmed:       trimmed,
		Quiet:         quiet,
		Autostart:     autostart,
		DockerConfig:  dockerConfig(fakeVM, ownHome),
		CompatLink:    os.Getenv("FEV_COMPAT_SOCKET"),
		Agent:         sshAgent.Path,
		Binds:         shared,
		Containers:    n.names.Feed(),
		LowPorts:      lowPorts(network),
		BeforeRestart: emulation.BeforeEngineRestart,
	})
	machines := &daemon.Machines{ImageServer: imageServer,
		Agent:        sshAgent,
		Notify:       notifyFile(os.Getenv("FEV_NOTIFY_FILE")),
		OnState:      n.names.Machine,
		OnGone:       n.names.MachinesGone,
		BeforeResume: func(boot string) { n.guest.WaitMacTold(boot, daemon.MacToldWait) },
	}
	if share != nil {
		machines.ShareRoot = share.Root
	}
	machines.DebugDockerConfig = debugDockerConfig(fakeVM, ownHome)
	machines.DebugDefaultHost = os.Getenv("DOCKER_HOST")
	machines.DebugOwnDockerHost = filepath.Join(layout.RunDir(), docker.SocketName)
	if d := bundle.Tools(bundle.Self()); d != "" {
		machines.DebugDockerCLI = filepath.Join(d, "docker")
	}
	if a := os.Getenv("FEV_DEBUG_AGENTS"); a != "" {
		machines.DebugAgentDir = a
	} else {
		machines.DebugAgentDir = bundle.DebugAgent(bundle.Self())
	}
	if trustFile != "" {
		machines.Trust = daemon.TrustFromFile(trustFile)
	}
	if view != nil {
		view.Isolated = machines.IsolatedNames
		machines.View = view.MachinePath
	}
	storageCalls := &daemon.StorageCalls{Machines: machines, Share: share}
	list := []daemon.Module{plan, engine, shared, sshAgent, emulation}
	if view != nil {
		list = append(list, view)
	}
	if image != nil {
		image.OnReset = machines.Forget
		list = append(list, image)
	}
	if !fakeVM {
		storageCalls.Image = layout.DataImage()
	}
	list = append(list, machines, storageCalls, &daemon.SSH{Machines: machines, OwnPortOnly: fakeVM || ownHome,
		Terminfo: os.Getenv("TERMINFO"), TerminfoDirs: os.Getenv("TERMINFO_DIRS")})
	list = append(list, n.guest)
	if network != nil {
		list = append(list, network)
	}
	list = append(list, n.names, &daemon.Domains{Source: n.names}, n.https)
	list = append(list, &daemon.K8s{Names: n.names, Network: network, Machines: machines, Forwards: engine.SetK8sForwards,
		ForwardList: engine.K8sForwards, Taken: plan.Taken, Fence: engine.Fence, Freed: engine.Freed,
		Kubeconfig: kubeconfigOf(layout, fakeVM, ownHome)})
	if !fakeVM {
		list = append(list, &daemon.Setting{Key: api.KeyNetworkHostLoopback, Apply: func(on bool) { noHostLoopback.Store(!on) }})
	}
	return list
}

func shareOf(layout home.Layout, root string, fakeVM, ownHome bool) (*vm.Share, string) {
	if root == "" {
		if fakeVM || ownHome {
			return nil, ""
		}
		root = "/"
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Sprintf("the Mac's files are not shared with the VM: %v", err)
	}
	share := &vm.Share{Root: abs, Deny: []string{layout.DataDir, layout.RunDir()}}
	if _, err := share.Arg(); err != nil {
		return nil, fmt.Sprintf("the Mac's files are not shared with the VM: %v", err)
	}
	return share, ""
}

func dockerConfig(fakeVM, ownHome bool) string {
	if dir := os.Getenv("FEV_DOCKER_CONFIG"); dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			return abs
		}
		return ""
	}
	if fakeVM || ownHome {
		return ""
	}
	dir, err := dockerctx.Dir()
	if err != nil {
		return ""
	}
	return dir
}

func kubeconfigOf(layout home.Layout, fakeVM, ownHome bool) string {
	if p := os.Getenv("FEV_K8S_KUBECONFIG"); p != "" {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return ""
	}
	if fakeVM || ownHome {
		return ""
	}
	if n, err := kubeconfig.ReadNote(kubeconfig.NotePath(layout.Home)); err == nil && n != nil && n.Path != "" {
		return n.Path
	}
	user, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return kubeconfig.DefaultPath(os.Getenv("KUBECONFIG"), user)
}

func debugDockerConfig(fakeVM, ownHome bool) string {
	if dir := os.Getenv("FEV_DEBUG_DOCKER_CONFIG"); dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			return abs
		}
		return ""
	}
	if fakeVM || ownHome {
		return ""
	}
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return dir
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".docker")
	}
	return ""
}

func loginItem(fakeVM, ownHome bool, homeFlag, dataDir string) *daemon.LoginItem {
	dir := os.Getenv("FEV_LAUNCH_AGENTS")
	if dir == "" {
		if fakeVM || ownHome {
			return nil
		}
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		dir = filepath.Join(userHome, "Library", "LaunchAgents")
	}
	self, err := os.Executable()
	if err != nil {
		return nil
	}
	item := &daemon.LoginItem{Dir: dir, Program: self}
	if homeFlag != "" {
		item.Args = append(item.Args, "--home", homeFlag)
	}
	if dataDir != "" {
		item.Args = append(item.Args, "--data-dir", dataDir)
	}
	if fakeVM {
		item.Args = append(item.Args, "--fake-vm")
	}
	return item
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fevd: "+format+"\n", args...)
	os.Exit(1)
}

func find(env string, candidates ...string) string {
	if path := os.Getenv(env); path != "" {
		return path
	}
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	for _, c := range candidates {
		path := filepath.Join(filepath.Dir(self), c)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

func main() {
	mainNS := boottrace.Now()
	if len(os.Args) > 1 && os.Args[1] == standin.Arg {
		os.Exit(standin.Main(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == nfsview.GuardArg {
		os.Exit(nfsview.GuardMain(os.Args[2:]))
	}

	homeFlag := flag.String("home", "", "directory for sockets, configuration, logs and the data image")
	dataDir := flag.String("data-dir", "", "directory for the data image")
	foreground := flag.Bool("foreground", false, "be the daemon, in this process")
	readyFD := flag.Int("ready-fd", -1, "descriptor to report readiness on (set by the launcher)")
	originNS := flag.Uint64("origin-ns", 0, "when the client that started this daemon itself started (set by the launcher)")
	fakeVM := flag.Bool("fake-vm", false, "run a stand-in for the VM; $FEV_FAKE_VM gives it instructions")
	noAutostart := flag.Bool("no-autostart", false, "do not start the VM until asked; the first docker command starts it")
	onDemand := flag.Bool("on-demand", false, "started by a docker command, not a fev one: start nothing if a stop was asked for meanwhile (set by the docker entry point)")
	login := flag.Bool("login", false, "started at login by the LaunchAgent that start_at_login writes")
	caDir := flag.String("ca-dir", "", "fev's CA from a key file in this directory, never the login keychain (tests and development tools)")
	dnsPort := flag.Int("dns-port", 0, "answer fev.local on 127.0.0.1 at this port from the start, whatever a helper says (tests)")
	fakeHelper := flag.Bool("fake-helper", false, "answer the admin helper's verbs with a stand-in on <home>/run/privhelper.sock that changes nothing on the Mac; $FEV_FAKE_HELPER gives it instructions (tests)")
	useHelper := flag.Bool("use-helper", false, "with a home of its own, use the admin helper `sudo fev setup --admin` installed, as the user's own daemon does")
	useVmnet := flag.Bool("vmnet", false, "with a home of its own, direct access through a vmnet interface of fevd's own when no admin helper is used, as the user's own daemon does (tests)")
	powerInject := flag.Bool("power-inject", false, "take sleep and wake from <home>/run/power.sock, not from the Mac (tests)")
	lowDiskMiB := flag.Uint64("low-disk-mib", 0, "what is left alone on the data image's volume, in MiB (default 5120); only for a daemon with a home of its own, for tests on a small scratch volume: the guest's cap on unwritten data follows it")
	noBudget := flag.Bool("no-space-budget", false, "send the guest no free-space budget; only for a daemon with a home of its own (tests: the baseline the budget is measured against)")
	spaceFile := flag.String("space-file", "", "with --fake-vm: set the stand-in's free-space budget from the two figures in this file, \"AVAILABLE IMAGE\" in bytes (tests)")
	flag.StringVar(&trustFile, "trust-file", "", "the Mac's trusted roots for machines from this PEM file, read at each VM's start in place of the keychains' trust settings (tests)")
	imageServer := flag.String("image-server", os.Getenv("FEV_IMAGE_SERVER"), "where machines' images come from (default $FEV_IMAGE_SERVER, else images.linuxcontainers.org; with --fake-vm none, the stand-in's own builds); \"none\" fetches nothing")
	shareRoot := flag.String("share-root", os.Getenv("FEV_SHARE_ROOT"), "the Mac directory shared with the VM while files.share is on (default $FEV_SHARE_ROOT; else / in the default home, nothing in another)")
	vmmPath := flag.String("vmm", "", "fevvm (default $FEV_VMM, else beside this program)")
	kernel := flag.String("kernel", "", "guest kernel Image (default $FEV_KERNEL)")
	initrd := flag.String("initrd", "", "guest init image (default $FEV_INITRD)")
	system := flag.String("system", "", "read-only guest system image (default $FEV_SYSTEM; none if there is none)")
	cmdline := flag.String("cmdline", "", "kernel command line (default $FEV_CMDLINE, else \""+defaultCmdline+"\")")
	logMax := flag.Int64("log-max-bytes", 0, "rotate each log at this size (default 10 MiB; tests use less)")
	tunnelMTU := flag.Int("tunnel-mtu", 0, "ask the admin helper for a tunnel of this MTU (1500 to 65535; default 16384); only for a daemon with a home of its own")
	showVersion := flag.Bool("version", false, "print the build identifier and exit")
	viewDir := flag.String("view-dir", "", "mount the view of the guest's files here (default ~/Fevom for the user's own daemon; a daemon with a home of its own, or with a stand-in VM, has none unless given one; \"none\" for none)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: fevd [--foreground] [--home DIR] [--data-dir DIR] [--fake-vm] [--no-autostart] [--on-demand] [--login] [--power-inject] [--low-disk-mib N] [--no-space-budget] [--space-file PATH] [--trust-file PATH] [--ca-dir DIR] [--dns-port N] [--fake-helper] [--use-helper] [--vmnet] [--image-server URL] [--share-root DIR] [--vmm PATH] [--kernel PATH] [--initrd PATH] [--system PATH]")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, "\n"+standin.Usage)
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Build)
		return
	}
	if flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}
	layout, err := home.Resolve(*homeFlag, *dataDir)
	if err != nil {
		fail("%v", err)
	}
	if err := layout.Validate(); err != nil {
		fail("%v", err)
	}

	if !*foreground {
		self, err := os.Executable()
		if err != nil {
			fail("%v", err)
		}
		if err := launch.Ensure(launch.Options{Layout: layout, Daemon: self, Args: os.Args[1:], OriginNS: mainNS}); err != nil {
			fail("%v", err)
		}
		return
	}

	ownHome := layout.Moved
	share, shareWarning := shareOf(layout, *shareRoot, *fakeVM, ownHome)
	var space spaceOptions
	if *lowDiskMiB != 0 {
		if !ownHome {
			fmt.Fprintln(os.Stderr, "fevd: --low-disk-mib is for a daemon with a home of its own (--home)")
			os.Exit(2)
		}
		space.margin = *lowDiskMiB << 20
	}
	minMTU, maxMTU := max(priv.MinMTU, netstack.MinTunnelMTU), min(priv.MaxMTU, netstack.MaxTunnelMTU)
	if *tunnelMTU != 0 && (!ownHome || *tunnelMTU < minMTU || *tunnelMTU > maxMTU) {
		fmt.Fprintf(os.Stderr, "fevd: --tunnel-mtu is for a daemon with a home of its own (--home), from %d to %d\n", minMTU, maxMTU)
		os.Exit(2)
	}
	if *noBudget {
		if !ownHome {
			fmt.Fprintln(os.Stderr, "fevd: --no-space-budget is for a daemon with a home of its own (--home)")
			os.Exit(2)
		}
		space.noBudget = true
	}
	if *spaceFile != "" {
		if !*fakeVM {
			fmt.Fprintln(os.Stderr, "fevd: --space-file is for a stand-in VM (--fake-vm)")
			os.Exit(2)
		}
		space.file = *spaceFile
	}
	if *fakeVM && *imageServer == "" {
		*imageServer = images.NoServer
	}
	noHostLoopback := &atomic.Bool{}
	plan, network := &daemon.Plan{}, &daemon.Network{}
	n := newNames(layout, *caDir, *dnsPort, *fakeVM, ownHome)
	emulation := &daemon.Emulation{}
	sshAgent := &daemon.SSHAgent{
		Initial:   os.Getenv("SSH_AUTH_SOCK"),
		GuestPath: func(p string) string { return engineAgentPath(p, share) },
	}
	var launcher vm.Launcher
	var kernelImage string
	if *fakeVM {
		self, err := os.Executable()
		if err != nil {
			fail("%v", err)
		}
		p := &vm.Process{Bin: self, Prefix: []string{standin.Arg}, Dir: layout.VMDir(), StandIn: true, Share: share, Control: true,
			BootWords: emulation.BootWords}
		for _, instruction := range strings.Fields(os.Getenv("FEV_FAKE_VM")) {
			p.Extra = append(p.Extra, "--standin", instruction)
		}
		launcher = p
	} else {
		if *cmdline == "" {
			*cmdline = os.Getenv("FEV_CMDLINE")
		}
		if *cmdline == "" {
			*cmdline = defaultCmdline
		}
		sshAgent.Pinned = strings.Contains(*cmdline, bridge.AgentPathParam+"=")
		if space.margin != 0 && !strings.Contains(*cmdline, "fev.dirty_bytes=") {
			*cmdline += fmt.Sprintf(" fev.dirty_bytes=%d", space.margin/4)
		}
		p := &vm.Process{
			Bin:            *vmmPath,
			Kernel:         *kernel,
			Initrd:         *initrd,
			SystemImage:    *system,
			Cmdline:        *cmdline,
			DataImage:      layout.DataImage(),
			Dir:            layout.VMDir(),
			Net:            true,
			Share:          share,
			Control:        os.Getenv("FEV_VMM_NO_CONTROL") == "",
			NoHostLoopback: noHostLoopback,
			BootWords: func() []string {
				return append(append(sshAgent.BootWords(), plan.BootWords()...), emulation.BootWords()...)
			},
			NetOptions: n.guest.Options,
			OnStack:    n.guest.Stack,
		}
		if p.Bin == "" {
			p.Bin = find("FEV_VMM", "fevvm", "../../vmm/target/release/fevvm")
		}
		if p.Kernel == "" {
			p.Kernel = find("FEV_KERNEL", "../Resources/guest/Image", "../../kernel/out/Image")
		}
		if p.Initrd == "" {
			p.Initrd = find("FEV_INITRD", "../Resources/guest/init.cpio", "../../guest/out/init.cpio")
		}
		if p.SystemImage == "" {
			p.SystemImage = find("FEV_SYSTEM", "../Resources/guest/system.img", "../../guest/out/system.img")
		}
		kernelImage = p.Kernel
		for what, path := range map[string]string{"VMM (--vmm)": p.Bin, "kernel (--kernel)": p.Kernel, "init image (--initrd)": p.Initrd} {
			if path == "" {
				fail("cannot find the %s; or run with --fake-vm", what)
			}
		}
		launcher = p
	}

	switch {
	case *fakeHelper:
		h, err := daemon.NewStandInHelper(os.Getenv("FEV_FAKE_HELPER"))
		if err != nil {
			fail("%v", err)
		}
		sock := filepath.Join(layout.RunDir(), "privhelper.sock")
		if err := os.MkdirAll(layout.RunDir(), 0o700); err != nil {
			fail("%v", err)
		}
		if err := h.Listen(sock); err != nil {
			fail("the stand-in helper: %v", err)
		}
		network.Helper = daemon.SocketHelper{Path: sock}
		plan.Recorded6 = h.Recorded6
		if len(h.NAT64) > 0 {
			network.NAT64 = h.NAT64Prefixes
		}
	case (!*fakeVM && !ownHome) || *useHelper || *useVmnet:
		if !*useHelper && ownHome {
			network.Helper = daemon.SocketHelper{Path: filepath.Join(layout.RunDir(), "no-helper.sock")}
		}
		mac := &macHolder{}
		network.StartMac = mac.start
		if *useHelper || !ownHome {
			plan.Recorded6 = installedRecorded6
		}
		n.guest.Mac = mac
	default:
		network = nil
	}
	if v := os.Getenv("FEV_FAKE_MAC_TAKEN"); v != "" {
		var taken []netip.Prefix
		for _, f := range strings.Fields(v) {
			pf, err := netip.ParsePrefix(f)
			if err != nil {
				fail("FEV_FAKE_MAC_TAKEN: %v", err)
			}
			taken = append(taken, pf)
		}
		plan.Taken = func() []netip.Prefix { return taken }
	} else if network != nil {
		plan.Taken = network.Taken
	}
	if network != nil {
		network.Tunnel = n.guest
		network.MTU = *tunnelMTU
		network.MacChanged = n.guest.MacChanged
		n.names.Network = network
		if (!*fakeVM && !ownHome) || *useVmnet {
			if p := newVmnetPath(n.names); p != nil {
				network.NoAdmin = p
				plan.NoAdminPath = true
				if !ownHome {
					plan.Recorded4 = installedRecorded4
				}
			}
		}
	}

	opts := daemon.Options{
		Layout: layout, Launcher: launcher, Autostart: !*noAutostart,
		Modules: modules(layout, share, *fakeVM, !*noAutostart, ownHome, noHostLoopback, space, *imageServer, sshAgent, plan, network, n, viewOf(*viewDir, *fakeVM, ownHome), emulation),
		MainNS:  mainNS, OriginNS: *originNS, LogMaxBytes: *logMax,
		LoginItem: loginItem(*fakeVM, ownHome, *homeFlag, *dataDir), Login: *login, OnDemand: *onDemand,
	}
	if kernelImage != "" {
		m, err := vm.ReadKernelManifest(kernelImage)
		switch {
		case err == nil:
			opts.KernelRelease = m.Release
		case !vm.NoManifest(err):
			fail("the kernel's manifest: %v", err)
		}
	}
	if v := os.Getenv("FEV_BOUNDS_MAX"); v != "" {
		most, err := time.ParseDuration(v)
		if err != nil || most <= 0 {
			fail("FEV_BOUNDS_MAX=%q is not a duration", v)
		}
		timing := daemon.DefaultTiming().Capped(most)
		opts.Timing = &timing
		docker.CapBounds(most)
	}
	if *powerInject {
		opts.PowerInject = filepath.Join(layout.RunDir(), "power.sock")
	} else if src, err := power.Listen(); err != nil {
		opts.Warnings = append(opts.Warnings, fmt.Sprintf("the Mac's sleep is not followed (%v): the VM is not paused for it and its clock is corrected only when fev is next used", err))
	} else {
		opts.Power = src
	}
	if shareWarning != "" {
		opts.Warnings = append(opts.Warnings, shareWarning)
	}
	if *readyFD >= 3 {
		opts.Ready = os.NewFile(uintptr(*readyFD), "ready")
	} else {
		opts.Echo = os.Stderr
	}
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)

	d, err := daemon.Start(opts)
	if err == daemon.ErrAlreadyRunning {
		if opts.Ready == nil {
			fmt.Fprintf(os.Stderr, "fevd: a daemon is already running in %s\n", layout.Home)
		}
		return
	}
	if err != nil {
		fail("%v", err)
	}
	go func() {
		<-signals
		go d.Shutdown(false)
		<-signals
		d.Shutdown(true)
	}()
	<-d.Done()
}

func notifyFile(path string) func(title, message string) error {
	if path == "" {
		return nil
	}
	return func(title, message string) error {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		b, _ := json.Marshal(map[string]string{"title": title, "message": message})
		_, err = f.Write(append(b, '\n'))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}
}

func engineAgentPath(p string, share *vm.Share) string {
	if share == nil || !filepath.IsAbs(p) {
		return ""
	}
	root, p := filepath.Clean(share.Root), filepath.Clean(p)
	if root == "/" {
		return p
	}
	if rel, ok := strings.CutPrefix(p, root+"/"); ok {
		return "/" + rel
	}
	return ""
}
