package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"

	"fev/host/privhelper"
	"fev/proto/netplan"
	api "fev/proto/privhelper"
	"fev/proto/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	mode, args := os.Args[1], os.Args[2:]
	var err error
	switch mode {
	case "serve":
		err = serve()
	case "boot":
		err = boot()
	case "foreground", "--foreground":
		err = foreground(args)
	case "install":
		err = install(args)
	case "uninstall":
		err = uninstall(args)
	case "version":
		fmt.Printf("fev-privhelper %s, protocol %d, security level %d\n", version.Build, api.ProtocolVersion, api.SecurityLevel)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fev-privhelper %s: %v\n", mode, err)
		if mode == "serve" || mode == "boot" {
			privhelper.SystemLog("%s: %v", mode, err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: fev-privhelper serve | boot | foreground --socket PATH --allow-uid UID [...] | install [...] | uninstall | version")
	os.Exit(2)
}

func installedRecord() (api.InstallRecord, error) {
	f, err := os.OpenFile(api.RecordPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return api.InstallRecord{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return api.InstallRecord{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || fi.Mode().Perm()&0o022 != 0 {
		return api.InstallRecord{}, fmt.Errorf("%s is not root's alone", api.RecordPath)
	}
	var r api.InstallRecord
	if err := json.NewDecoder(f).Decode(&r); err != nil {
		return api.InstallRecord{}, fmt.Errorf("%s: %w", api.RecordPath, err)
	}
	return r, nil
}

func installedServer() (*privhelper.Server, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("not root: launchd runs this")
	}
	r, err := installedRecord()
	if err != nil {
		return nil, err
	}
	return privhelper.New(privhelper.Config{Record: r, Sys: privhelper.Darwin{}, Logf: privhelper.SystemLog})
}

func serve() error {
	srv, err := installedServer()
	if err != nil {
		return err
	}
	if _, err := unix.Getpeername(0); err != nil {
		privhelper.SystemLog("serve with no connection on descriptor 0 (%v): the boot pass", err)
		return srv.BootPass()
	}
	f := os.NewFile(0, "connection")
	c, err := net.FileConn(f)
	f.Close()
	if err != nil {
		return err
	}
	if null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		for _, fd := range []int{0, 1, 2} {
			unix.Dup2(int(null.Fd()), fd)
		}
		null.Close()
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		c.Close()
		return fmt.Errorf("descriptor 0 is a %T", c)
	}
	srv.ServeConn(uc)
	return nil
}

func boot() error {
	srv, err := installedServer()
	if err != nil {
		return err
	}
	return srv.BootPass()
}

func foreground(args []string) error {
	fs := flag.NewFlagSet("foreground", flag.ExitOnError)
	socket := fs.String("socket", "", "the socket to listen on (a short path: /tmp/bv.X/priv.sock)")
	allow := fs.Int("allow-uid", -1, "the uid let in besides root")
	rec := recordFlags(fs, api.DefaultResolverPort)
	root := fs.String("root", "", "a scratch directory standing for / for the paths the verbs write")
	fs.Parse(args)
	if *socket == "" || *allow <= 0 {
		return errors.New("--socket and --allow-uid (a user's) are needed")
	}
	r, err := rec.record(*allow)
	if err != nil {
		return err
	}
	srv, err := privhelper.New(privhelper.Config{Record: r, AllowUID: *allow, Root: *root, Sys: privhelper.Darwin{}, Logf: stderrLog})
	if err != nil {
		return err
	}
	return listen(*socket, *allow, srv)
}

func stderrLog(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "fev-privhelper: "+format+"\n", a...)
}

func listen(path string, uid int, srv *privhelper.Server) error {
	os.Remove(path)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	l.SetUnlinkOnClose(true)
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(path, uid, -1); err != nil {
			return err
		}
	}
	stderrLog("listening on %s for uid %d and root (pid %d, euid %d)", path, uid, os.Getpid(), os.Geteuid())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		l.Close()
	}()
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go srv.ServeConn(c)
	}
}

type recFlags struct {
	home, base, base6, form *string
	port                    *int
}

func recordFlags(fs *flag.FlagSet, port int) recFlags {
	return recFlags{
		home:  fs.String("home", "", "the user's home (the user's own by default)"),
		base:  fs.String("base", "", "the plan's IPv4 /16 (network.subnet; the user's configuration's, else "+netplan.DefaultBase.String()+")"),
		base6: fs.String("base6", "", "the plan's ULA /48 (network.subnet6; the user's configuration's, else none)"),
		port:  fs.Int("resolver-port", port, portHelp(port)),
		form:  fs.String("resolver-form", api.ResolverFormStore, "the resolver entries' form: store (supplemental resolvers in the System Configuration store, the default; a stand-in directory under --root) or file (/etc/resolver, which leaves fev.local and shop.local themselves to mDNS)"),
	}
}

func portHelp(port int) string {
	h := "the loopback port fevd answers fev.local on"
	if port == 0 {
		h += fmt.Sprintf(" (0: the one an earlier install recorded, else %d unless something holds it, else the next free)", api.DefaultResolverPort)
	}
	return h
}

func (f recFlags) record(uid int) (api.InstallRecord, error) {
	home := *f.home
	if home == "" {
		u, err := user.LookupId(strconv.Itoa(uid))
		if err != nil {
			return api.InstallRecord{}, fmt.Errorf("the home of uid %d: %w", uid, err)
		}
		home = u.HomeDir
	}
	base, base6, err := privhelper.ConfiguredPlan(home, uid)
	if err != nil {
		return api.InstallRecord{}, err
	}
	if *f.base != "" {
		base = *f.base
	}
	if *f.base6 != "" {
		base6 = *f.base6
	}
	return api.InstallRecord{UID: uid, Home: filepath.Clean(home), Base: base, Base6: base6, ResolverPort: *f.port, ResolverForm: *f.form, Build: version.Build}, nil
}

func install(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	rec := recordFlags(fs, 0)
	link := fs.Bool("docker-sock", false, "make /var/run/docker.sock fev's link, and again at each start of the Mac")
	replace := fs.Bool("replace-docker-sock", false, "with --docker-sock: replace a link to another product's socket")
	noResolver := fs.Bool("no-resolver", false, "make no fev.local resolver entry")
	bootJob := fs.Bool("boot-job", false, "also write a second job for the boot pass")
	root := fs.String("root", "", "install into this scratch directory standing for /, as the user, launchctl's calls printed (the dry run)")
	fs.Parse(args)
	uid, owner, launchctl, err := installer(*root)
	if err != nil {
		return err
	}
	r, err := rec.record(uid)
	if err != nil {
		return err
	}
	r.DockerSockLink = *link
	r.DockerSockConsent = *link && *replace
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	rep, err := privhelper.Install(privhelper.InstallOptions{Root: *root, Source: self, Record: r, Resolver: !*noResolver, BootJob: *bootJob, OwnerUID: owner, Launchctl: launchctl})
	if err != nil {
		return err
	}
	fmt.Printf("installed the helper for uid %d (%s): base %s", rep.Record.UID, rep.Record.Home, rep.Record.Base)
	if rep.Record.Base6 != "" {
		fmt.Printf(" and %s", rep.Record.Base6)
	}
	fmt.Printf(", fev.local on 127.0.0.1:%d\n", rep.Record.ResolverPort)
	for _, p := range rep.Record.Paths {
		fmt.Println("  wrote", p)
	}
	for _, k := range rep.Store {
		fmt.Println("  set", k)
	}
	for _, w := range rep.Warnings {
		fmt.Println("  warning:", w)
	}
	return nil
}

func installer(root string) (uid, owner int, launchctl func(...string) error, err error) {
	if root != "" {
		if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
			return 0, 0, nil, fmt.Errorf("--root %s is not a directory", root)
		}
		return os.Getuid(), os.Getuid(), printLaunchctl, nil
	}
	if os.Geteuid() != 0 {
		return 0, 0, nil, errors.New("run it with sudo")
	}
	uid, err = strconv.Atoi(os.Getenv("SUDO_UID"))
	if err != nil || uid <= 0 {
		return 0, 0, nil, errors.New("run it with sudo from the account fev is for (SUDO_UID is missing or root's)")
	}
	return uid, 0, nil, nil
}

func printLaunchctl(args ...string) error {
	fmt.Println("  launchctl", args)
	return nil
}

func uninstall(args []string) error {
	fset := flag.NewFlagSet("uninstall", flag.ExitOnError)
	root := fset.String("root", "", "uninstall from this scratch directory standing for /, launchctl's calls printed")
	fset.Parse(args)
	var launchctl func(...string) error
	if *root != "" {
		launchctl = printLaunchctl
	}
	rep, err := privhelper.Undo(*root, launchctl)
	if err != nil {
		return err
	}
	for _, p := range rep.Removed {
		fmt.Println("  removed", p)
	}
	for _, p := range rep.Left {
		fmt.Println("  left", p, "(not fev's, or not empty)")
	}
	fmt.Println("direct access and names on the Mac end when fevd next exits")
	return nil
}
