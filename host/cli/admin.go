package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"fev/host/bundle"
	api "fev/proto/daemon"
	"fev/proto/version"
)

type Admin interface {
	Install(o AdminInstall) (AdminReport, error)
	Undo(root string, launchctl func(args ...string) error) (AdminReport, error)
	Plan(home string, uid int) (base, base6 string, err error)
}

type AdminInstall struct {
	Root                          string
	OwnerUID                      int
	Source                        string
	UID                           int
	Home                          string
	Base, Base6                   string
	ResolverPort                  int
	DockerSock, DockerSockConsent bool
	Build                         string
	Launchctl                     func(args ...string) error
}

type AdminReport struct {
	Written, Removed, Left, Warnings []string
	Base6                            string
}

const adminResolverPort = 0

var helperNames = []string{"fev-privhelper", "privhelper"}

type adminOptions struct {
	undo, dryRun, dockerSock, replaceDockerSock bool
	helper                                      string
}

func findHelper() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	return helperNear(self)
}

func helperNear(self string) (string, error) {
	for _, name := range helperNames {
		if p := bundle.Peer(self, name); p != "" {
			return p, nil
		}
	}
	return "", fmt.Errorf("no %s beside %s; build it (host/build.sh) or name it with --helper", helperNames[0], self)
}

func adminUser(dryRun bool) (uid int, home string, err error) {
	s := os.Getenv("SUDO_UID")
	switch {
	case s != "":
		uid, err = strconv.Atoi(s)
		if err != nil || uid <= 0 {
			return 0, "", fmt.Errorf("SUDO_UID=%q is not a user's: run it as yourself, under sudo", s)
		}
	case dryRun && os.Geteuid() != 0:
		uid = os.Getuid()
	default:
		return 0, "", errors.New("setup --admin installs fev's root helper for the user who runs it: run it as yourself with sudo (sudo fev setup --admin), not as root")
	}
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return 0, "", fmt.Errorf("user %d: %w", uid, err)
	}
	if !filepath.IsAbs(u.HomeDir) {
		return 0, "", fmt.Errorf("user %d has no home directory", uid)
	}
	return uid, filepath.Clean(u.HomeDir), nil
}

func (a *app) admin(o adminOptions) error {
	if a.Admin == nil {
		return failf(ExitFailed, "this build of fev has no admin helper to install")
	}
	say := func(format string, args ...any) {
		if !a.quiet {
			fmt.Fprintf(a.Stdout, format+"\n", args...)
		}
	}
	if !o.dryRun && os.Geteuid() != 0 {
		verb := "setup --admin"
		if o.undo {
			verb += " --undo"
		}
		return failf(ExitFailed, "%s changes this Mac's system configuration and runs as root: sudo fev %s (--dry-run shows what it does, with no sudo)", verb, verb)
	}
	root, launchctl, noted := "", (func(...string) error)(nil), &[]string{}
	if o.dryRun {
		dir, err := os.MkdirTemp("", "fev-admin-dry-run.")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			dir = real
		}
		root = dir
		launchctl = func(args ...string) error {
			*noted = append(*noted, "launchctl "+strings.Join(unroot(root, args), " "))
			return nil
		}
	}
	if o.undo {
		if o.dryRun {
			say("dry run: nothing is installed under a scratch root, so there is nothing to remove; sudo fev setup --admin --undo removes what install.json lists:")
			say("  /Library/PrivilegedHelperTools/dev.fevom.privhelper, /Library/LaunchDaemons/dev.fevom.privhelper*.plist,")
			say("  fev's resolver entries (fev.local's and the custom .local names'), /var/run/docker.sock if it is fev's link,")
			say("  /Library/Application Support/dev.fevom/privhelper/install.json")
			return nil
		}
		r, err := a.Admin.Undo(root, launchctl)
		if err != nil {
			return failf(ExitFailed, "setup --admin --undo: %v", err)
		}
		for _, p := range r.Removed {
			say("removed %s", p)
		}
		for _, p := range r.Left {
			fmt.Fprintf(a.Stderr, "fev: left alone, it is not fev's or holds something else: %s\n", p)
		}
		if len(r.Removed) == 0 {
			say("no admin helper was installed")
		}
		say("direct access and names on this Mac end when fev's daemon next exits (fev stop)")
		return nil
	}

	uid, home, err := adminUser(o.dryRun)
	if err != nil {
		return failf(ExitFailed, "setup --admin: %v", err)
	}
	src := o.helper
	if src == "" {
		if src, err = findHelper(); err != nil {
			return failf(ExitFailed, "setup --admin: %v", err)
		}
	}
	base, base6, err := a.Admin.Plan(home, uid)
	if err != nil {
		return failf(ExitFailed, "setup --admin: the address plan in your configuration: %v", err)
	}
	in := AdminInstall{Root: root, OwnerUID: 0, Source: src, UID: uid, Home: home, Base: base, Base6: base6,
		ResolverPort: adminResolverPort, DockerSock: o.dockerSock || o.replaceDockerSock, DockerSockConsent: o.replaceDockerSock,
		Build: version.Build, Launchctl: launchctl}
	if o.dryRun {
		in.OwnerUID = os.Getuid()
	}
	r, err := a.Admin.Install(in)
	if err != nil {
		return failf(ExitFailed, "setup --admin: %v", err)
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(a.Stderr, "fev: %s\n", unrootText(root, w))
	}
	written := unroot(root, r.Written)
	sort.Strings(written)
	if r.Base6 != "" {
		base6 = r.Base6
	}
	if o.dryRun {
		say("dry run: sudo fev setup --admin would install fev's admin helper for user %d (%s), routing only inside %s%s:", uid, home, base, orNone(base6))
		for _, p := range written {
			say("  write %s", p)
		}
		for _, c := range *noted {
			say("  run %s", c)
		}
		return nil
	}
	for _, p := range written {
		say("wrote %s", p)
	}
	say("installed fev's admin helper for user %d, routing only inside %s%s", uid, base, orNone(base6))
	say("it is used from fev's next start (fev restart); nothing else asks for your password")
	return nil
}

func orNone(base6 string) string {
	if base6 == "" {
		return ""
	}
	return " and " + base6
}

func unroot(root string, paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = unrootText(root, p)
	}
	return out
}

func unrootText(root, s string) string {
	if root == "" {
		return s
	}
	return strings.ReplaceAll(s, root, "")
}

func (a *app) httpsTrust(ctx context.Context, undo bool) error {
	action := api.TrustInstall
	if undo {
		action = api.TrustRemove
	}
	if a.GraphicAccess != nil && !a.GraphicAccess() {
		return failf(ExitFailed, "fev setup --https asks for your password in a dialog on this Mac's own screen, which this session (over SSH) cannot show: run it in a Terminal on the Mac")
	}
	return a.withDaemon(ctx, false, func(c *api.Client) error {
		st, err := c.HTTPSTrust(ctx, action)
		if api.IsNotImplemented(err) {
			return failf(ExitFailed, "this build's daemon does not keep a root for HTTPS yet")
		}
		if err != nil {
			return err
		}
		return a.emit(st, func(w io.Writer) {
			httpsText(w, st)
			if !undo && st.Trust == api.TrustTrusted {
				kv(w, "firefox", "Firefox reads the Mac's trusted roots when it starts: quit and reopen it")
			}
		})
	})
}

func httpsText(w io.Writer, st *api.HTTPSStatus) {
	trust := map[string]string{
		api.TrustTrusted:   "trusted by this Mac for HTTPS (this account)",
		api.TrustUntrusted: "in your keychain, not trusted",
		api.TrustAbsent:    "none",
		api.TrustDeclined:  `not trusted: the dialog was cancelled ("fev setup --https" asks again)`,
	}[st.Trust]
	if trust == "" {
		trust = st.Trust
	}
	kv(w, "https root", trust)
	if st.Root != "" {
		kv(w, "sha-256", st.Root)
	}
	if st.Error != "" {
		kv(w, "error", st.Error)
	}
}
