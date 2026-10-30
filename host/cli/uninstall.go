package cli

import (
	"bufio"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"fev/host/bundle"
	"fev/host/daemon/home"
	"fev/host/k8s/kubeconfig"
	api "fev/proto/daemon"
)

type Keychain interface {
	State(der []byte) (string, error)
	Remove(der []byte) error
	DeleteKey(signer string) error
}

var macCommand = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

const appID = "dev.fevom.app"

type uninstallOptions struct {
	deleteData, dryRun, yes bool
	keepAdmin               bool
	fromApp                 bool
}

type uninstall struct {
	a         *app
	o         uninstallOptions
	l         home.Layout
	ctx       context.Context
	self, app string
	appsMade  bool
	user      string
	left      []string
}

func (a *app) uninstallCmd() *cobra.Command {
	var o uninstallOptions
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove Fevom and everything it set up",
		Long: `Remove Fevom from this Mac: start at login, the admin helper (your
administrator password), this Mac's trust in fev's HTTPS root (your
password), the Docker context, the commands on PATH and the SSH and
Kubernetes entries setup made, the app's login item and Finder sidebar
entry, and Fevom.app. Fevom is stopped first. What you had before
(your Docker context, your shell profile) is put back.

Your containers, images, volumes and Linux machines are kept, with fev's
settings, so that installing Fevom again finds them. --delete-data deletes
them as well, with the HTTPS root's key and the app's preferences.

It asks once before it starts (-y does not ask). --dry-run says what it
would do and changes nothing.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if os.Geteuid() == 0 {
				return usagef("uninstall runs as you, not under sudo: it asks for an administrator's password itself, for the admin helper")
			}
			u, err := a.newUninstall(cmd.Context(), o)
			if err != nil {
				return err
			}
			return u.run()
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.deleteData, "delete-data", false, "also delete every container, image, volume and machine, fev's settings and its HTTPS key")
	f.BoolVar(&o.dryRun, "dry-run", false, "say what would be removed; change nothing")
	f.BoolVarP(&o.yes, "yes", "y", false, "do not ask before starting")
	f.BoolVar(&o.keepAdmin, "keep-admin", false, "leave the admin helper and the HTTPS trust, and say how to remove them")
	f.BoolVar(&o.fromApp, "from-app", false, "run by Fevom.app")
	f.MarkHidden("keep-admin")
	f.MarkHidden("from-app")
	return cmd
}

func (a *app) newUninstall(ctx context.Context, o uninstallOptions) (*uninstall, error) {
	l, err := a.layout()
	if err != nil {
		return nil, err
	}
	user, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	u := &uninstall{a: a, o: o, l: l, ctx: ctx, user: user}
	if u.self, err = selfPath(); err != nil {
		return nil, err
	}
	record := filepath.Join(l.Home, "install.record")
	if b, err := os.ReadFile(record); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			k, v, _ := strings.Cut(line, " ")
			switch k {
			case "app":
				u.app = v
			case "apps_made":
				u.appsMade = v == "1"
			}
		}
	}
	if u.app == "" {
		if c := bundle.Contents(u.self); c != "" {
			u.app = filepath.Dir(c)
		}
	}
	if u.app != "" && !isFevomApp(u.app) {
		if _, err := os.Stat(u.app); err == nil {
			return nil, failf(ExitFailed, "uninstall: %s is not Fevom.app (its bundle identifier is not %s): nothing was removed", u.app, appID)
		}
	}
	return u, nil
}

func isFevomApp(path string) bool {
	id := plistValue(filepath.Join(path, "Contents", "Info.plist"), "CFBundleIdentifier")
	return id == appID || id == appID+".debug"
}

func (u *uninstall) say(format string, args ...any) {
	if !u.a.quiet {
		fmt.Fprintf(u.a.Stdout, format+"\n", args...)
	}
}

func (u *uninstall) do(what string, fn func() error) {
	if u.o.dryRun {
		u.say("Would %s", what)
		return
	}
	if err := fn(); err != nil {
		fmt.Fprintf(u.a.Stderr, "fev: could not %s: %v\n", what, err)
		u.left = append(u.left, what)
		return
	}
	verb, rest, _ := strings.Cut(what, " ")
	u.say("%s %s", capitalise(pastTense[verb]), rest)
}

var pastTense = map[string]string{"remove": "removed", "delete": "deleted", "stop": "stopped"}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (u *uninstall) run() error {
	adminRoot := os.Getenv("FEV_ADMIN_ROOT")
	admin := u.adminInstalled(adminRoot)
	der, trusted := u.trusted()
	agents := u.launchAgents()

	if u.o.dryRun {
		u.say("Dry run: nothing is changed.")
	} else if !u.o.yes && !u.o.fromApp {
		if !isTerminal(u.a.Stdin) {
			return usagef("uninstall asks before it starts, and this has no terminal to ask at: run it with -y")
		}
		u.describe(admin, trusted)
		fmt.Fprint(u.a.Stdout, "Uninstall Fevom? [y/N] ")
		answer, _ := bufio.NewReader(u.a.Stdin).ReadString('\n')
		switch strings.TrimSpace(answer) {
		case "y", "Y", "yes", "YES":
		default:
			u.say("Nothing was changed.")
			return nil
		}
	}

	u.stop(agents)

	switch {
	case !admin:
	case u.o.keepAdmin:
		u.say("Left the admin helper: sudo %s setup --admin --undo removes it", u.self)
		u.left = append(u.left, "the admin helper")
	case u.a.Admin == nil:
		u.say("Left the admin helper: this build of fev cannot remove it")
		u.left = append(u.left, "the admin helper")
	case adminRoot != "":
		u.do("remove the admin helper (under "+adminRoot+")", func() error {
			_, err := u.a.Admin.Undo(adminRoot, func(...string) error { return nil })
			return err
		})
	default:
		u.do("remove the admin helper", u.removeAdmin)
	}

	switch {
	case !trusted:
	case u.o.keepAdmin:
		u.say("Left this Mac's trust in fev's HTTPS root: %s setup --https --undo removes it", u.self)
		u.left = append(u.left, "the HTTPS trust")
	case os.Getenv("FEV_TRUST_SETTINGS") != "":
		u.do("remove this Mac's trust in fev's HTTPS root (the stand-in)", func() error { return os.Remove(os.Getenv("FEV_TRUST_SETTINGS")) })
	default:
		u.do("remove this Mac's trust in fev's HTTPS root", func() error { return u.a.Keychain.Remove(der) })
	}
	if u.o.deleteData && !u.l.Moved && u.a.Keychain != nil {
		signer := bundle.Peer(u.self, "fev-keysigner")
		if signer != "" {
			u.do("delete the HTTPS root's key from the login keychain", func() error { return u.a.Keychain.DeleteKey(signer) })
		} else {
			u.say("Left the HTTPS root's key in the login keychain (\"%s\"): no key signer beside %s; Keychain Access removes it", "dev.fevom.ca", u.self)
		}
	}

	if u.app != "" && !u.l.Moved {
		exe := filepath.Join(u.app, "Contents", "MacOS", "Fevom")
		if _, err := os.Stat(exe); err == nil {
			u.do("remove the app's login item and ~/Fevom from Finder's sidebar", func() error {
				out, err := macCommand(exe, "--uninstall-cleanup")
				if err != nil {
					return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
				}
				return nil
			})
		}
	}

	u.setup()

	if u.app != "" {
		if _, err := os.Stat(u.app); err == nil {
			u.do("remove "+u.app, func() error { return os.RemoveAll(u.app) })
		}
		if u.appsMade && !u.o.dryRun {
			if os.Remove(filepath.Dir(u.app)) == nil {
				u.say("Removed %s (empty, made by the install)", filepath.Dir(u.app))
			}
		}
	}
	if !u.l.Moved {
		lib := filepath.Join(u.user, "Library")
		for _, p := range []string{
			filepath.Join(lib, "Caches", appID),
			filepath.Join(lib, "HTTPStorages", appID),
			filepath.Join(lib, "WebKit", appID),
			filepath.Join(lib, "Saved Application State", appID+".savedState"),
		} {
			if _, err := os.Lstat(p); err == nil {
				u.do("remove "+tildePath(p), func() error { return os.RemoveAll(p) })
			}
		}
	}

	if u.o.deleteData {
		u.deleteData()
	} else {
		if !u.o.dryRun {
			os.Remove(filepath.Join(u.l.Home, "install.record"))
		}
		u.say("Kept your containers, images, volumes and machines and fev's settings (%s): a later install finds them; fev uninstall --delete-data deletes them", tildePath(u.l.Home))
	}

	if u.o.dryRun {
		return nil
	}
	if len(u.left) > 0 {
		return failf(ExitFailed, "uninstall: not removed: %s", strings.Join(u.left, "; "))
	}
	u.say("Fevom is uninstalled.")
	return nil
}

func (u *uninstall) describe(admin, trusted bool) {
	w := u.a.Stdout
	fmt.Fprintln(w, "This stops Fevom and removes:")
	if u.app != "" {
		fmt.Fprintf(w, "  %s\n", u.app)
	}
	if admin && !u.o.keepAdmin {
		fmt.Fprintln(w, "  the admin helper (asks for an administrator's password)")
	}
	if trusted && !u.o.keepAdmin {
		fmt.Fprintln(w, "  this Mac's trust in fev's HTTPS root (asks for your password)")
	}
	fmt.Fprintln(w, "  fev and docker on your PATH, the Docker context fev, and fev's SSH and Kubernetes entries")
	if u.o.deleteData {
		fmt.Fprintf(w, "  every container, image, volume and machine, fev's settings (%s), the app's preferences and the HTTPS root's key\n", tildePath(u.l.Home))
	} else {
		fmt.Fprintf(w, "Your containers, images, volumes and machines are kept (--delete-data deletes them).\n")
	}
}

func (u *uninstall) adminInstalled(root string) bool {
	if root == "" && u.l.Moved {
		return false
	}
	for _, p := range []string{"/Library/Application Support/dev.fevom/privhelper/install.json", "/Library/PrivilegedHelperTools/dev.fevom.privhelper"} {
		if _, err := os.Lstat(root + p); err == nil {
			return true
		}
	}
	return false
}

func (u *uninstall) trusted() (der []byte, ok bool) {
	if standIn := os.Getenv("FEV_TRUST_SETTINGS"); standIn != "" {
		b, err := os.ReadFile(standIn)
		return nil, err == nil && strings.Contains(string(b), ": fev development CA")
	}
	if u.l.Moved || u.a.Keychain == nil {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(u.l.HTTPSDir(), "ca.pem"))
	if err != nil {
		return nil, false
	}
	c, err := parsePEMCert(b)
	if err != nil {
		return nil, false
	}
	st, err := u.a.Keychain.State(c)
	return c, err == nil && st != api.TrustAbsent
}

func (u *uninstall) launchAgents() []string {
	dir := os.Getenv("FEV_LAUNCH_AGENTS")
	if dir == "" {
		if u.l.Moved {
			return nil
		}
		dir = filepath.Join(u.user, "Library", "LaunchAgents")
	}
	var found []string
	for _, label := range []string{"dev.fevom.fevd", "local.fev.fevd"} {
		if _, err := os.Stat(filepath.Join(dir, label+".plist")); err == nil {
			found = append(found, filepath.Join(dir, label+".plist"))
		}
	}
	return found
}

func (u *uninstall) stop(agents []string) {
	c, err := u.a.connect(u.ctx)
	running := err != errNotRunning
	if err == nil {
		if !u.o.dryRun {
			_, err := c.ConfigSet(u.ctx, map[string]any{"start_at_login": false})
			if err != nil {
				fmt.Fprintf(u.a.Stderr, "fev: start at login: %v\n", err)
			}
		}
		c.Close()
	}
	if !u.o.fromApp && !u.l.Moved && !u.o.dryRun {
		macCommand("/usr/bin/osascript", "-e", `if application id "`+appID+`" is running then tell application id "`+appID+`" to quit`)
	}
	if running {
		u.do("stop fev", func() error {
			stop := u.a.stopCmd()
			stop.SetContext(u.ctx)
			quiet := u.a.quiet
			u.a.quiet = true
			defer func() { u.a.quiet = quiet }()
			return stop.RunE(stop, nil)
		})
	}
	daemon := daemonNear(u.self)
	for _, plist := range agents {
		b, err := os.ReadFile(plist)
		if err != nil {
			continue
		}
		mine := (u.app != "" && strings.Contains(string(b), u.app+"/")) || (daemon != "" && strings.Contains(string(b), daemon))
		if !mine {
			u.say("Left %s: it starts a fevd that is not this install's", plist)
			continue
		}
		u.do("remove "+tildePath(plist), func() error {
			if err := os.Remove(plist); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		})
	}
}

func (u *uninstall) removeAdmin() error {
	argv := []string{u.self, "setup", "--admin", "--undo"}
	if f, ok := u.a.Stdin.(*os.File); ok && !u.o.fromApp && isTerminal(f) {
		fmt.Fprintln(u.a.Stdout, "Removing the admin helper needs an administrator's password:")
		cmd := exec.Command("/usr/bin/sudo", argv...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = f, u.a.Stdout, u.a.Stderr
		return cmd.Run()
	}
	if u.a.GraphicAccess != nil && !u.a.GraphicAccess() {
		return errors.New("an administrator's password is needed, and this session has neither a terminal nor the Mac's screen to ask on: run fev uninstall in a Terminal on the Mac")
	}
	quoted := make([]string, len(argv))
	for i, s := range argv {
		quoted[i] = "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	command := strings.Join(quoted, " ")
	script := `do shell script "` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(command) + `" with prompt "Fevom removes its admin helper." with administrator privileges`
	out, err := macCommand("/usr/bin/osascript", "-e", script)
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (u *uninstall) setup() {
	say := func(format string, args ...any) { u.say(capitalise(fmt.Sprintf(format, args...))) }
	if u.o.dryRun {
		u.say("Would put back the Docker context selected before fev's and take back fev setup's changes (%s)", tildePath(setupState(u.l)))
	} else {
		if err := removeDockerContext(u.l, say); err != nil {
			fmt.Fprintf(u.a.Stderr, "fev: the Docker context: %v\n", err)
			u.left = append(u.left, "the Docker context")
		}
		if _, err := os.Stat(setupState(u.l)); err == nil {
			u.a.uninstalling = true
			if err := u.a.undoSetup(u.l, say); err != nil {
				u.left = append(u.left, "setup's changes ("+tildePath(setupState(u.l))+")")
			}
		}
	}
	note := kubeconfig.NotePath(u.l.Home)
	if _, err := os.Stat(note); err != nil {
		return
	}
	path, err := u.a.kubeconfigPath(u.ctx, u.l)
	if err != nil {
		fmt.Fprintf(u.a.Stderr, "fev: the Kubernetes context: %v\n", err)
		u.left = append(u.left, "the Kubernetes context")
		return
	}
	u.do("remove the Kubernetes context "+kubeconfig.Context+" from "+tildePath(path), func() error {
		_, err := kubeconfig.Remove(note, path)
		return err
	})
}

func (u *uninstall) deleteData() {
	l := u.l
	if l.Placed {
		dir := l.DataDir
		if _, err := os.Stat(dir); err != nil {
			fmt.Fprintf(u.a.Stderr, "fev: fev's data image is in %s, which is not there (a disk not connected?): connect it and delete %s\n", dir, filepath.Join(dir, "data.img"))
			u.left = append(u.left, "the data image in "+dir)
		} else {
			u.do("delete the data image in "+dir, func() error {
				for _, name := range []string{"data.img", "swap.img"} {
					if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
						return err
					}
				}
				os.Remove(dir)
				return nil
			})
		}
	}
	if !l.Moved {
		data := filepath.Join(u.user, "Library", "Application Support", "dev.fevom")
		if _, err := os.Stat(data); err == nil {
			u.do("delete fev's data ("+tildePath(data)+")", func() error { return os.RemoveAll(data) })
		}
		view := filepath.Join(u.user, "Fevom")
		if _, err := os.Lstat(view); err == nil {
			u.do("remove ~/Fevom", func() error { return removeViewFolder(view) })
		}
	}
	if !safeToRemove(l.Home, u.user) {
		fmt.Fprintf(u.a.Stderr, "fev: left %s: it is not a directory of fev's own\n", l.Home)
		u.left = append(u.left, l.Home)
	} else if _, err := os.Stat(l.Home); err == nil {
		u.do("delete fev's home ("+tildePath(l.Home)+")", func() error { return os.RemoveAll(l.Home) })
	}
	if l.Moved {
		prefs := filepath.Join(u.user, "Library", "Preferences", appID+".plist")
		if _, err := os.Stat(prefs); err == nil {
			u.do("delete the app's preferences", func() error { return os.Remove(prefs) })
		}
	} else if _, err := macCommand("/usr/bin/defaults", "read", appID); err == nil {
		u.do("delete the app's preferences", func() error {
			_, err := macCommand("/usr/bin/defaults", "delete", appID)
			return err
		})
	}
}

func removeViewFolder(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() != "README.txt" {
			return fmt.Errorf("%s holds %s, which is not fev's: left", dir, e.Name())
		}
	}
	os.Chmod(dir, 0o755)
	if len(entries) > 0 {
		if err := os.Remove(filepath.Join(dir, "README.txt")); err != nil {
			return err
		}
	}
	return os.Remove(dir)
}

func safeToRemove(dir, user string) bool {
	dir = filepath.Clean(dir)
	return dir != "/" && dir != filepath.Clean(user) && !strings.HasPrefix(filepath.Clean(user)+"/", dir+"/") && len(dir) > len("/tmp")
}

func parsePEMCert(b []byte) ([]byte, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no certificate")
	}
	return block.Bytes, nil
}
