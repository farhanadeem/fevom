package privhelper_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"fev/host/privhelper"
	"fev/host/privhelper/fakesys"
	api "fev/proto/privhelper"
)

type installRig struct {
	root, home, bin string
	calls           []string
}

func newInstallRig(t *testing.T) *installRig {
	t.Helper()
	dir := t.TempDir()
	r := &installRig{root: filepath.Join(dir, "root"), home: filepath.Join(dir, "home"), bin: filepath.Join(dir, "fev-privhelper")}
	for _, d := range []string{r.root, r.home, filepath.Join(r.root, "Library"), filepath.Join(r.root, "etc"), filepath.Join(r.root, "var/run")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.Chmod(r.root, 0o755)
	if err := os.WriteFile(r.bin, []byte("#!helper\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *installRig) launchctl(args ...string) error {
	r.calls = append(r.calls, strings.Join(args, " "))
	return nil
}

func (r *installRig) opts() privhelper.InstallOptions {
	return privhelper.InstallOptions{
		Root: r.root, Source: r.bin, Resolver: true, OwnerUID: os.Getuid(), Launchctl: r.launchctl,
		Record: api.InstallRecord{UID: os.Getuid(), Home: r.home, Base: "198.19.0.0/16", Base6: "fd5b:6e2a:91c0::/48", ResolverPort: 15353, Build: "test"},
	}
}

func tree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		fi, _ := os.Lstat(p)
		out = append(out, strings.TrimPrefix(p, root)+" "+fi.Mode().String())
		return nil
	})
	return out
}

func standInEntry(t *testing.T, root, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, privhelper.StoreStandIn, name+".json"))
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s's stand-in entry: %v", name, err)
	}
	return m
}

func wantStoreEntry(t *testing.T, root, name string, port int) {
	t.Helper()
	m := standInEntry(t, root, name)
	if m == nil {
		t.Errorf("no store entry for %s", name)
		return
	}
	got := fmt.Sprint(m["key"], m["SupplementalMatchDomains"], m["SupplementalMatchDomainsNoSearch"], m["ServerAddresses"], m["ServerPort"])
	want := fmt.Sprint(api.StoreKey(name), []any{name}, wantNoSearch(name), []any{"127.0.0.1"}, float64(port))
	if got != want {
		t.Errorf("%s's store entry: %s, want %s", name, got, want)
	}
}

func TestInstallAndUndoLeaveTheTreeAsItWas(t *testing.T) {
	r := newInstallRig(t)
	before := tree(t, r.root)
	rep, err := privhelper.Install(r.opts())
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{
		"/Library/Application Support", "/Library/Application Support/dev.fevom", "/Library/Application Support/dev.fevom/privhelper",
		"/Library/LaunchDaemons", "/Library/LaunchDaemons/dev.fevom.privhelper.plist",
		"/Library/PrivilegedHelperTools", "/Library/PrivilegedHelperTools/dev.fevom.privhelper",
		api.RecordPath, "/var/db", privhelper.StoreStandIn,
	}
	slices.Sort(wantPaths)
	if !slices.Equal(rep.Record.Paths, wantPaths) {
		t.Errorf("recorded paths:\n%v\nwant\n%v", rep.Record.Paths, wantPaths)
	}
	if !slices.Equal(rep.Store, []string{api.StoreKey("fev.local")}) {
		t.Errorf("the store keys set: %v", rep.Store)
	}
	fi, err := os.Stat(filepath.Join(r.root, api.InstallPath))
	if err != nil || fi.Mode().Perm() != 0o555 {
		t.Errorf("the binary: %v %v", fi, err)
	}
	plist, _ := os.ReadFile(filepath.Join(r.root, api.PlistPath))
	for _, want := range []string{"<string>dev.fevom.privhelper</string>", "<string>" + api.InstallPath + "</string>", "<string>serve</string>",
		"<key>Wait</key>\n\t\t<false/>", "<key>RunAtLoad</key>\n\t<true/>", "<string>" + api.SocketPath + "</string>",
		"<key>SockPathOwner</key>\n\t\t\t<integer>" + itoa(os.Getuid()) + "</integer>",
		"<key>SockPathMode</key>\n\t\t\t<integer>384</integer>"} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("the job lacks %q:\n%s", want, plist)
		}
	}
	if strings.Contains(string(plist), "KeepAlive") {
		t.Error("the socket's job has KeepAlive")
	}
	if out, err := exec.Command("/usr/bin/plutil", "-lint", filepath.Join(r.root, api.PlistPath)).CombinedOutput(); err != nil {
		t.Errorf("plutil: %v %s", err, out)
	}
	if _, err := os.Lstat(filepath.Join(r.root, api.BootPlistPath)); err == nil {
		t.Error("a second job for the boot pass was written")
	}
	if _, err := os.Lstat(filepath.Join(r.root, api.ResolverPath)); err == nil {
		t.Error("the store form wrote a resolver file")
	}
	wantStoreEntry(t, r.root, "fev.local", 15353)
	var rec api.InstallRecord
	b, _ := os.ReadFile(filepath.Join(r.root, api.RecordPath))
	if err := json.Unmarshal(b, &rec); err != nil || rec.UID != os.Getuid() || rec.Home != r.home || rec.ProtocolVersion != api.ProtocolVersion || rec.InstalledAt == "" ||
		rec.ResolverForm != api.ResolverFormStore || rec.NoResolver {
		t.Errorf("the record: %+v %v", rec, err)
	}
	wantCalls := []string{"bootout system/dev.fevom.privhelper", "bootout system/dev.fevom.privhelper.boot",
		"bootstrap system " + filepath.Join(r.root, api.PlistPath)}
	if !slices.Equal(r.calls, wantCalls) {
		t.Errorf("launchctl: %v", r.calls)
	}
	installed := tree(t, r.root)
	if _, err := privhelper.Install(r.opts()); err != nil {
		t.Fatalf("again: %v", err)
	}
	if again := tree(t, r.root); !slices.Equal(again, installed) {
		t.Errorf("a second install changed the tree:\n%v\n%v", installed, again)
	}

	privhelper.DirStore{Dir: filepath.Join(r.root, privhelper.StoreStandIn)}.Set("app.local", 15353, time.Now())
	os.MkdirAll(filepath.Join(r.root, "etc/resolver"), 0o755)
	os.WriteFile(filepath.Join(r.root, "etc/resolver/vpn.local"), []byte("nameserver 10.8.0.1\n"), 0o644)
	os.WriteFile(filepath.Join(r.root, privhelper.LockPath), []byte("2026-10-16T12:00:00Z\n"), 0o644)
	r.calls = nil
	undo, err := privhelper.Undo(r.root, r.launchctl)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.calls, wantCalls[:2]) {
		t.Errorf("launchctl at the uninstall: %v", r.calls)
	}
	if !slices.Contains(undo.Removed, api.StoreKey("app.local")) || !slices.Contains(undo.Removed, api.StoreKey("fev.local")) || len(undo.Left) != 0 {
		t.Errorf("uninstall: %+v", undo)
	}
	if b, _ := os.ReadFile(filepath.Join(r.root, "etc/resolver/vpn.local")); string(b) != "nameserver 10.8.0.1\n" {
		t.Error("someone's resolver file went with the uninstall")
	}
	os.Remove(filepath.Join(r.root, "etc/resolver/vpn.local"))
	os.Remove(filepath.Join(r.root, "etc/resolver"))
	if after := tree(t, r.root); !slices.Equal(after, before) {
		t.Errorf("the tree after the uninstall:\n%v\nbefore:\n%v", after, before)
	}
}

func TestTheFileFormInstallAndUndo(t *testing.T) {
	r := newInstallRig(t)
	before := tree(t, r.root)
	o := r.opts()
	o.Record.ResolverForm = api.ResolverFormFile
	o.BootJob = true
	rep, err := privhelper.Install(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{api.ResolverPath, "/etc/resolver", api.BootPlistPath} {
		if !slices.Contains(rep.Record.Paths, p) {
			t.Errorf("%s not recorded: %v", p, rep.Record.Paths)
		}
	}
	if len(rep.Store) != 0 || standInEntry(t, r.root, "fev.local") != nil {
		t.Errorf("the file form set a store entry: %v", rep.Store)
	}
	resolver, _ := os.ReadFile(filepath.Join(r.root, api.ResolverPath))
	if string(resolver) != api.ResolverHeader+"\nnameserver 127.0.0.1\nport 15353\n" {
		t.Errorf("the resolver file: %q", resolver)
	}
	if out, err := exec.Command("/usr/bin/plutil", "-lint", filepath.Join(r.root, api.PlistPath), filepath.Join(r.root, api.BootPlistPath)).CombinedOutput(); err != nil {
		t.Errorf("plutil: %v %s", err, out)
	}
	os.WriteFile(filepath.Join(r.root, "etc/resolver/app.local"), privhelper.ResolverFileText(15353), 0o644)
	os.WriteFile(filepath.Join(r.root, "etc/resolver/vpn.local"), []byte("nameserver 10.8.0.1\n"), 0o644)
	undo, err := privhelper.Undo(r.root, r.launchctl)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(undo.Removed, "/etc/resolver/app.local") || !slices.Contains(undo.Removed, api.ResolverPath) || !slices.Contains(undo.Left, "/etc/resolver") {
		t.Errorf("uninstall: %+v", undo)
	}
	os.Remove(filepath.Join(r.root, "etc/resolver/vpn.local"))
	os.Remove(filepath.Join(r.root, "etc/resolver"))
	if after := tree(t, r.root); !slices.Equal(after, before) {
		t.Errorf("the tree after the uninstall:\n%v\nbefore:\n%v", after, before)
	}
}

func TestAnInstallTakesAwayTheOtherFormsEntries(t *testing.T) {
	r := newInstallRig(t)
	o := r.opts()
	o.Record.ResolverForm = api.ResolverFormFile
	if _, err := privhelper.Install(o); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(r.root, "etc/resolver/app.local"), privhelper.ResolverFileText(15353), 0o644)
	os.WriteFile(filepath.Join(r.root, "etc/resolver/vpn.local"), []byte("nameserver 10.8.0.1\n"), 0o644)
	if _, err := privhelper.Install(r.opts()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{api.ResolverPath, "/etc/resolver/app.local"} {
		if _, err := os.Lstat(filepath.Join(r.root, p)); err == nil {
			t.Errorf("the store form's install left fev's %s", p)
		}
	}
	if _, err := os.Lstat(filepath.Join(r.root, "etc/resolver/vpn.local")); err != nil {
		t.Error("someone's file went")
	}
	wantStoreEntry(t, r.root, "fev.local", 15353)
	privhelper.DirStore{Dir: filepath.Join(r.root, privhelper.StoreStandIn)}.Set("app.local", 15353, time.Now())
	if _, err := privhelper.Install(o); err != nil {
		t.Fatal(err)
	}
	if standInEntry(t, r.root, "fev.local") != nil || standInEntry(t, r.root, "app.local") != nil {
		t.Error("the file form's install left fev's store entries")
	}
	if _, err := os.Lstat(filepath.Join(r.root, api.ResolverPath)); err != nil {
		t.Errorf("no resolver file: %v", err)
	}
}

func TestNoResolverIsRecorded(t *testing.T) {
	r := newInstallRig(t)
	o := r.opts()
	o.Resolver = false
	rep, err := privhelper.Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Record.NoResolver || len(rep.Store) != 0 || standInEntry(t, r.root, "fev.local") != nil {
		t.Errorf("an install with no resolver: %+v", rep)
	}
	srv, err := privhelper.New(privhelper.Config{Record: rep.Record, Root: r.root, Sys: fakesys.New()})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.BootPass(); err != nil || standInEntry(t, r.root, "fev.local") != nil {
		t.Errorf("the boot pass after --no-resolver: %v", err)
	}
}

func TestInstallMakesTheLinkWhenAskedAndUndoRemovesOnlyFevoms(t *testing.T) {
	r := newInstallRig(t)
	o := r.opts()
	o.Record.DockerSockLink = true
	rep, err := privhelper.Install(o)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(r.root, api.DockerSockPath)
	if to, _ := os.Readlink(link); to != filepath.Join(r.home, ".fev/run/docker.sock") || !slices.Contains(rep.Record.Paths, api.DockerSockPath) {
		t.Errorf("the link: %q %v", to, rep.Record.Paths)
	}
	if _, err := privhelper.Undo(r.root, r.launchctl); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); err == nil {
		t.Error("the link stayed")
	}

	r2 := newInstallRig(t)
	other := "/Users/x/.otherproduct/run/docker.sock"
	os.Symlink(other, filepath.Join(r2.root, api.DockerSockPath))
	o2 := r2.opts()
	o2.Record.DockerSockLink = true
	rep, err = privhelper.Install(o2)
	if err != nil || len(rep.Warnings) != 1 {
		t.Fatalf("install over another's link: %+v %v", rep, err)
	}
	privhelper.Undo(r2.root, r2.launchctl)
	if to, _ := os.Readlink(filepath.Join(r2.root, api.DockerSockPath)); to != other {
		t.Errorf("another product's link became %q", to)
	}
}

func TestInstallRefusesWritableDirectoriesAndOthersFiles(t *testing.T) {
	r := newInstallRig(t)
	os.MkdirAll(filepath.Join(r.root, "Library/LaunchDaemons"), 0o755)
	os.Chmod(filepath.Join(r.root, "Library/LaunchDaemons"), 0o775)
	if _, err := privhelper.Install(r.opts()); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Errorf("a group-writable LaunchDaemons: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.root, api.InstallPath)); err == nil {
		t.Error("a refused install wrote the binary")
	}

	r = newInstallRig(t)
	o := r.opts()
	o.OwnerUID = 0
	if _, err := privhelper.Install(o); err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Errorf("a tree not root's: %v", err)
	}

	r = newInstallRig(t)
	for _, edit := range []func(*privhelper.InstallOptions){
		func(o *privhelper.InstallOptions) { o.Record.UID = 0 },
		func(o *privhelper.InstallOptions) { o.Record.Home = "/" },
		func(o *privhelper.InstallOptions) { o.Record.Home = filepath.Join(r.home, "missing") },
		func(o *privhelper.InstallOptions) { o.Source = r.home },
		func(o *privhelper.InstallOptions) { o.Record.Base = "8.8.0.0/16" },
	} {
		o := r.opts()
		edit(&o)
		if _, err := privhelper.Install(o); err == nil {
			t.Errorf("%+v was installed", o.Record)
		}
	}
	os.MkdirAll(filepath.Join(r.root, "etc/resolver"), 0o755)
	os.WriteFile(filepath.Join(r.root, api.ResolverPath), []byte("nameserver 10.0.0.53\n"), 0o644)
	if _, err := privhelper.Install(r.opts()); err == nil || !strings.Contains(err.Error(), "not fev's") {
		t.Errorf("someone's fev.local file: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(r.root, api.ResolverPath)); string(b) != "nameserver 10.0.0.53\n" {
		t.Error("someone's fev.local file was changed")
	}
}

func TestTheRootChainOfTheMacsOwnDirectories(t *testing.T) {
	for _, d := range []string{"/Library/LaunchDaemons", "/Library/PrivilegedHelperTools", "/private/etc"} {
		if _, err := os.Stat(d); err != nil {
			continue
		}
		if err := privhelper.CheckRootChain("", d, 0); err != nil {
			t.Errorf("%s: %v", d, err)
		}
	}
	if err := privhelper.CheckRootChain("", t.TempDir(), 0); err == nil {
		t.Error("a temporary directory passed")
	}
	if err := privhelper.CheckRootChain("", "/tmp", 0); err == nil {
		t.Error("/tmp passed")
	}
}

func TestTheInstallReadsThePlanFromTheConfiguration(t *testing.T) {
	home := t.TempDir()
	base, base6, err := privhelper.ConfiguredPlan(home, os.Getuid())
	if err != nil || base != "198.19.0.0/16" || base6 != "" {
		t.Errorf("no file: %q %q %v", base, base6, err)
	}
	cfg := filepath.Join(home, ".fev/config/config.json")
	os.MkdirAll(filepath.Dir(cfg), 0o755)
	os.WriteFile(cfg, []byte(`{"version":1,"values":{"network.subnet":"10.234.0.0/16","network.subnet6":"fd5b:6e2a:91c0::/48","cpu":4}}`), 0o644)
	base, base6, err = privhelper.ConfiguredPlan(home, os.Getuid())
	if err != nil || base != "10.234.0.0/16" || base6 != "fd5b:6e2a:91c0::/48" {
		t.Errorf("set: %q %q %v", base, base6, err)
	}
	if _, _, err := privhelper.ConfiguredPlan(home, os.Getuid()+1); err == nil {
		t.Error("a file of another uid's was read")
	}
	for _, bad := range []string{`{"values":{"network.subnet":"8.8.0.0/16"}}`, `{"values":{"network.subnet":7}}`, `not json`} {
		os.WriteFile(cfg, []byte(bad), 0o644)
		if _, _, err := privhelper.ConfiguredPlan(home, os.Getuid()); err == nil {
			t.Errorf("%s was read", bad)
		}
	}
	os.Remove(cfg)
	os.Symlink("/etc/hosts", cfg)
	if _, _, err := privhelper.ConfiguredPlan(home, os.Getuid()); err == nil {
		t.Error("a link was followed")
	}
}

func TestUndoRemovesTheSocketLaunchdLeft(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "bv.")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	os.MkdirAll(filepath.Join(root, "var/run"), 0o755)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(root, api.SocketPath), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close()
	rep, err := privhelper.Undo(root, func(...string) error { return nil })
	if err != nil || !slices.Contains(rep.Removed, api.SocketPath) {
		t.Errorf("undo: %+v %v", rep, err)
	}
	if _, err := os.Lstat(filepath.Join(root, api.SocketPath)); err == nil {
		t.Error("the socket stayed")
	}
	os.WriteFile(filepath.Join(root, api.SocketPath), []byte("x"), 0o644)
	privhelper.Undo(root, func(...string) error { return nil })
	if _, err := os.Lstat(filepath.Join(root, api.SocketPath)); err != nil {
		t.Error("a file that is not a socket was removed")
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestTheResolverPortIsChosenAtInstallAndKept(t *testing.T) {
	r := newInstallRig(t)
	o := r.opts()
	o.Record.ResolverPort = 0
	var asked []int
	o.PortFree = func(p int) bool { asked = append(asked, p); return p != api.DefaultResolverPort }
	rep, err := privhelper.Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Record.ResolverPort != 15354 || !slices.Equal(asked, []int{15353, 15354}) {
		t.Errorf("chose %d after asking %v", rep.Record.ResolverPort, asked)
	}
	wantStoreEntry(t, r.root, "fev.local", 15354)
	srv, err := privhelper.New(privhelper.Config{Record: rep.Record, Root: r.root, Sys: fakesys.New()})
	if err != nil {
		t.Fatal(err)
	}
	if h, _, err := srv.Call(context.Background(), api.MethodHello, nil); err != nil || h.(api.HelloResult).ResolverPort != 15354 {
		t.Errorf("hello: %+v %v", h, err)
	}

	asked = nil
	o.PortFree = func(p int) bool { asked = append(asked, p); return false }
	if rep, err = privhelper.Install(o); err != nil || rep.Record.ResolverPort != 15354 || len(asked) != 0 {
		t.Errorf("a reinstall: port %d, asked %v, %v", rep.Record.ResolverPort, asked, err)
	}
	o.Record.ResolverPort = 20053
	if rep, err = privhelper.Install(o); err != nil || rep.Record.ResolverPort != 20053 {
		t.Errorf("a port named: %d %v", rep.Record.ResolverPort, err)
	}

	o.Record.ResolverForm = api.ResolverFormTunnel
	if _, err := privhelper.Install(o); err == nil || !strings.Contains(err.Error(), "not built") {
		t.Errorf("the tunnel form: %v", err)
	}

	if _, err := privhelper.ChooseResolverPort(0, func(int) bool { return false }); err == nil || !strings.Contains(err.Error(), "--resolver-port") {
		t.Errorf("every port held: %v", err)
	}
}

func TestTheResolverPortCheckSeesAWildcardHolder(t *testing.T) {
	l, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if p := l.Addr().(*net.TCPAddr).Port; privhelper.ResolverPortFree(p) {
		t.Errorf("port %d is free beside a wildcard TCP listener", p)
	}
	var u net.PacketConn
	var p int
	for try := 0; u == nil; try++ {
		tl, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		p = tl.Addr().(*net.TCPAddr).Port
		u, err = net.ListenPacket("udp4", fmt.Sprintf("127.0.0.1:%d", p))
		tl.Close()
		if err != nil && try == 20 {
			t.Fatalf("no port free on both TCP and UDP in 20 tries: %v", err)
		}
	}
	if privhelper.ResolverPortFree(p) {
		t.Errorf("port %d is free beside a UDP socket", p)
	}
	u.Close()
	if !privhelper.ResolverPortFree(p) {
		t.Errorf("port %d is held after its socket closed", p)
	}
}

func TestTheInstallRecordsAnIPv6Base(t *testing.T) {
	r := newInstallRig(t)
	o := r.opts()
	o.Record.Base6 = ""
	rep, err := privhelper.Install(o)
	if err != nil {
		t.Fatal(err)
	}
	p, err := netip.ParsePrefix(rep.Record.Base6)
	if err != nil || p.Bits() != 48 || !netip.MustParsePrefix("fd00::/8").Contains(p.Addr()) {
		t.Fatalf("the record's IPv6 base %q: %v", rep.Record.Base6, err)
	}
	again, err := privhelper.Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if again.Record.Base6 != rep.Record.Base6 {
		t.Errorf("a reinstall made another /48: %s, then %s", rep.Record.Base6, again.Record.Base6)
	}
	o.Record.Base6 = "fd5b:6e2a:91c0::/48"
	if own, err := privhelper.Install(o); err != nil || own.Record.Base6 != "fd5b:6e2a:91c0::/48" {
		t.Errorf("the configuration's /48: %q %v", own.Record.Base6, err)
	}
}
