package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	api "fev/proto/daemon"
	"fev/proto/jsonrpc"
)

type fakeAdmin struct {
	installs []AdminInstall
	undos    []string
	planOf   []string
	err      error
}

var helperPaths = []string{
	"/Library/Application Support/dev.fevom/privhelper/install.json",
	"/Library/LaunchDaemons/dev.fevom.privhelper.plist",
	"/Library/PrivilegedHelperTools/dev.fevom.privhelper",
	"/etc/resolver/fev.local",
}

func (f *fakeAdmin) Install(o AdminInstall) (AdminReport, error) {
	f.installs = append(f.installs, o)
	if f.err != nil {
		return AdminReport{}, f.err
	}
	var r AdminReport
	for _, p := range helperPaths {
		full := filepath.Join(o.Root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, nil, 0o644)
		r.Written = append(r.Written, full)
	}
	if o.Launchctl != nil {
		o.Launchctl("bootstrap", "system", filepath.Join(o.Root, "/Library/LaunchDaemons/dev.fevom.privhelper.plist"))
	}
	if o.DockerSock && !o.DockerSockConsent {
		r.Warnings = append(r.Warnings, filepath.Join(o.Root, "/var/run/docker.sock")+": a link of another product's; --replace-docker-sock replaces it")
	}
	return r, nil
}

func (f *fakeAdmin) Undo(root string, launchctl func(...string) error) (AdminReport, error) {
	f.undos = append(f.undos, root)
	return AdminReport{Removed: helperPaths, Left: []string{"/var/run/docker.sock"}}, f.err
}

func (f *fakeAdmin) Plan(home string, uid int) (string, string, error) {
	f.planOf = append(f.planOf, home+" "+strconv.Itoa(uid))
	return "198.19.0.0/16", "fd5b:6e2a:91c0::/48", nil
}

func fevAdmin(t *testing.T, home string, admin Admin, argv ...string) result {
	t.Helper()
	var out, errw lockedBuffer
	code := Main(Env{
		Name: "fev", Args: append([]string{"--home", home}, argv...),
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errw, Admin: admin,
	})
	return result{out.String(), errw.String(), code}
}

func TestSetupAdminInstallsOnlyUnderSudoAndDryRunsWithout(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("run as root: the refusals are a user's")
	}
	home := scratchHome(t)
	helper := filepath.Join(t.TempDir(), "fev-privhelper")
	os.WriteFile(helper, []byte("helper"), 0o755)
	before := snapshot(t, home)
	userHome, _ := os.UserHomeDir()
	userBefore := snapshot(t, userHome)

	a := &fakeAdmin{}
	for _, argv := range [][]string{{"setup", "--admin"}, {"setup", "--admin", "--undo"}} {
		r := fevAdmin(t, home, a, append(argv, "--helper", helper)...)
		if r.code != ExitFailed || !strings.Contains(r.err, "runs as root: sudo fev "+strings.Join(argv, " ")) {
			t.Errorf("%v as a user: exit %d, %q", argv, r.code, r.err)
		}
	}
	if len(a.installs)+len(a.undos) != 0 {
		t.Errorf("a user's setup --admin reached the installer: %+v %v", a.installs, a.undos)
	}

	t.Setenv("SUDO_UID", "")
	os.Unsetenv("SUDO_UID")
	r := fevAdmin(t, home, a, "setup", "--admin", "--dry-run", "--helper", helper, "--docker-sock")
	if r.code != 0 || len(a.installs) != 1 {
		t.Fatalf("dry run: exit %d, %q %q", r.code, r.out, r.err)
	}
	in := a.installs[0]
	me, _ := user.Current()
	if in.Root == "" || in.OwnerUID != os.Getuid() || in.UID != os.Getuid() || in.Home != filepath.Clean(me.HomeDir) || in.Source != helper ||
		in.Base != "198.19.0.0/16" || in.Base6 != "fd5b:6e2a:91c0::/48" || in.ResolverPort != 0 || !in.DockerSock || in.DockerSockConsent || in.Build == "" {
		t.Errorf("the dry run asked for %+v", in)
	}
	if _, err := os.Stat(in.Root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dry run's scratch root is left: %v", err)
	}
	for _, want := range append(helperPaths, "run launchctl bootstrap system /Library/LaunchDaemons/dev.fevom.privhelper.plist", "routing only inside 198.19.0.0/16 and fd5b:6e2a:91c0::/48") {
		if !strings.Contains(r.out, want) {
			t.Errorf("the dry run does not say %q:\n%s", want, r.out)
		}
	}
	if strings.Contains(r.out+r.err, in.Root) {
		t.Errorf("the dry run names its scratch root:\n%s%s", r.out, r.err)
	}
	if !strings.Contains(r.err, "fev: /var/run/docker.sock: a link of another product's") {
		t.Errorf("the installer's warning: %q", r.err)
	}
	if len(a.planOf) != 1 || a.planOf[0] != filepath.Clean(me.HomeDir)+" "+strconv.Itoa(os.Getuid()) {
		t.Errorf("the plan was read for %v", a.planOf)
	}
	r = fevAdmin(t, home, a, "setup", "--admin", "--undo", "--dry-run")
	if r.code != 0 || len(a.undos) != 0 || !strings.Contains(r.out, "install.json") {
		t.Errorf("undo's dry run: %d %q, undos %v", r.code, r.out, a.undos)
	}
	for _, bad := range []string{"0", "me"} {
		t.Setenv("SUDO_UID", bad)
		if r := fevAdmin(t, home, a, "setup", "--admin", "--dry-run", "--helper", helper); r.code != ExitFailed || !strings.Contains(r.err, "is not a user's") {
			t.Errorf("SUDO_UID=%s: %d %q", bad, r.code, r.err)
		}
	}
	os.Unsetenv("SUDO_UID")

	for _, argv := range [][]string{
		{"setup", "--admin", "--https"}, {"setup", "--admin", "--shell", "zsh"}, {"setup", "--admin", "--no-tools"},
		{"setup", "--dry-run"}, {"setup", "--docker-sock"}, {"setup", "--replace-docker-sock"},
	} {
		if r := fevAdmin(t, home, a, argv...); r.code != ExitUsage {
			t.Errorf("%v: exit %d %q", argv, r.code, r.err)
		}
	}
	if r := fevAdmin(t, home, nil, "setup", "--admin", "--dry-run"); r.code != ExitFailed || !strings.Contains(r.err, "no admin helper") {
		t.Errorf("no installer: %d %q", r.code, r.err)
	}
	if got := snapshot(t, home); got != before {
		t.Errorf("setup --admin wrote into fev's home:\n%s", got)
	}
	if got := snapshot(t, userHome); got != userBefore {
		t.Errorf("setup --admin wrote into the user's home")
	}
}

func TestSetupHTTPSAsksTheDaemonForTheTrust(t *testing.T) {
	f := newFake(t)
	f.fixture(api.MethodHTTPSTrust, "https.trust")
	r := f.run("setup", "--https").want(t, 0)
	if got := f.params(api.MethodHTTPSTrust); got != `{"action":"install"}` {
		t.Errorf("https.trust params %s", got)
	}
	if !strings.Contains(r.out, "https root: trusted by this Mac for HTTPS") || !strings.Contains(r.out, "sha-256: ") {
		t.Errorf("setup --https printed %q", r.out)
	}
	if !strings.Contains(r.out, "Firefox reads the Mac's trusted roots when it starts: quit and reopen it") {
		t.Errorf("setup --https did not say what Firefox needs: %q", r.out)
	}
	f2 := newFake(t)
	f2.fixture(api.MethodHTTPSTrust, "https.trust.declined")
	if r := f2.run("setup", "--https", "--undo").want(t, 0); strings.Contains(r.out, "Firefox") {
		t.Errorf("--undo spoke of Firefox: %q", r.out)
	}
	if got := f2.params(api.MethodHTTPSTrust); got != `{"action":"remove"}` {
		t.Errorf("https.trust params %s", got)
	}
	f4 := newFake(t)
	f4.fixture(api.MethodHTTPSTrust, "https.trust.declined")
	if r := f4.run("setup", "--https").want(t, 0); !strings.Contains(r.out, "the dialog was cancelled") || strings.Contains(r.out, "Firefox") {
		t.Errorf("declined: %q", r.out)
	}
	f3 := newFake(t)
	f3.on(api.MethodHTTPSTrust, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return nil, jsonrpc.Errorf(api.CodeNotImplemented, "https.trust is not implemented in this build of the daemon")
	})
	if r := f3.run("setup", "--https"); r.code != ExitFailed || !strings.Contains(r.err, "does not keep a root for HTTPS yet") {
		t.Errorf("not implemented: %d %q", r.code, r.err)
	}
}

func TestSetupHTTPSFailsPlainlyWithNoWindowServer(t *testing.T) {
	for _, argv := range [][]string{{"setup", "--https"}, {"setup", "--https", "--undo"}} {
		f := newFake(t)
		f.fixture(api.MethodHTTPSTrust, "https.trust")
		var out, errw lockedBuffer
		code := Main(Env{
			Name: "fev", Args: append([]string{"--home", f.home}, argv...),
			Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errw,
			GraphicAccess: func() bool { return false },
		})
		if code != ExitFailed || !strings.Contains(errw.String(), "Terminal on the Mac") {
			t.Errorf("%v with no window server: %d %q %q", argv, code, out.String(), errw.String())
		}
		for _, c := range f.made() {
			if c.Method == api.MethodHTTPSTrust {
				t.Errorf("%v with no window server asked the daemon: %s", argv, c.Params)
			}
		}
	}
	f := newFake(t)
	f.on(api.MethodHTTPSTrust, func(*jsonrpc.Conn, json.RawMessage) (any, error) {
		return nil, jsonrpc.Errorf(api.CodeFailed, "the trust dialog needs the Mac's own login session (not SSH)")
	})
	if r := f.run("setup", "--https"); r.code != ExitFailed || !strings.Contains(r.err, "the Mac's own login session (not SSH)") {
		t.Errorf("the daemon's refusal: %d %q", r.code, r.err)
	}
}
