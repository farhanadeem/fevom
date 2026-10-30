package privhelper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"fev/proto/netplan"
	api "fev/proto/privhelper"
)

const BootLabel = api.Label + ".boot"

type InstallOptions struct {
	Root      string
	Source    string
	Record    api.InstallRecord
	Resolver  bool
	Store     Store
	Sys       System
	BootJob   bool
	OwnerUID  int
	Launchctl func(args ...string) error
	Now       func() time.Time
	PortFree  func(port int) bool
}

const resolverPortTries = 64

func ChooseResolverPort(old int, free func(int) bool) (int, error) {
	if old >= 1024 && old <= 65535 {
		return old, nil
	}
	if free == nil {
		free = ResolverPortFree
	}
	for p := api.DefaultResolverPort; p < api.DefaultResolverPort+resolverPortTries; p++ {
		if free(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("127.0.0.1's ports %d to %d are all held: name one with --resolver-port", api.DefaultResolverPort, api.DefaultResolverPort+resolverPortTries-1)
}

func ResolverPortFree(port int) bool {
	for _, typ := range []int{syscall.SOCK_STREAM, syscall.SOCK_DGRAM} {
		fd, err := syscall.Socket(syscall.AF_INET, typ, 0)
		if err != nil {
			return false
		}
		err = syscall.Bind(fd, &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}})
		syscall.Close(fd)
		if err != nil {
			return false
		}
	}
	return true
}

type InstallReport struct {
	Record   api.InstallRecord
	Warnings []string
	Store    []string
}

func Install(o InstallOptions) (InstallReport, error) {
	if o.Root == "" && os.Geteuid() != 0 {
		return InstallReport{}, errors.New("the install runs as root: sudo")
	}
	r := o.Record
	if r.ResolverForm == "" {
		r.ResolverForm = api.ResolverFormStore
	}
	r.NoResolver = !o.Resolver
	store := o.Store
	if store == nil {
		store = storeFor(o.Root)
	}
	at := func(p string) string { return rooted(o.Root, p) }
	var old api.InstallRecord
	if b, err := os.ReadFile(at(api.RecordPath)); err == nil {
		json.Unmarshal(b, &old)
	}
	if r.ResolverPort == 0 {
		p, err := ChooseResolverPort(old.ResolverPort, o.PortFree)
		if err != nil {
			return InstallReport{}, err
		}
		r.ResolverPort = p
	}
	if r.Base6 == "" {
		if p6, err := netplan.Parse6(old.Base6); err == nil {
			r.Base6 = p6.Prefix().String()
		} else {
			ula, err := netplan.NewULA(nil)
			if err != nil {
				return InstallReport{}, err
			}
			r.Base6 = ula.String()
		}
	}
	if _, _, _, err := checkRecord(r); err != nil {
		return InstallReport{}, err
	}
	if err := checkHome(r.Home, r.UID); err != nil {
		return InstallReport{}, err
	}
	launchctl := o.Launchctl
	if launchctl == nil {
		launchctl = runLaunchctl
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	written := slices.Clone(old.Paths)
	note := func(p string) {
		if !slices.Contains(written, p) {
			written = append(written, p)
		}
	}
	for _, dir := range []string{filepath.Dir(api.InstallPath), filepath.Dir(api.PlistPath), filepath.Dir(api.RecordPath)} {
		made, err := mkdirs(o.Root, dir, o.Root == "")
		if err != nil {
			return InstallReport{}, err
		}
		for _, m := range made {
			note(m)
		}
		if err := CheckRootChain(o.Root, dir, o.OwnerUID); err != nil {
			return InstallReport{}, err
		}
	}
	bin, err := readRegular(o.Source)
	if err != nil {
		return InstallReport{}, fmt.Errorf("the helper to install: %w", err)
	}
	launchctl("bootout", "system/"+api.Label)
	launchctl("bootout", "system/"+BootLabel)

	if err := writeOwned(at(api.InstallPath), bin, 0o555, o.Root == ""); err != nil {
		return InstallReport{}, err
	}
	note(api.InstallPath)
	if err := writeOwned(at(api.PlistPath), LaunchdPlist(r.UID), 0o644, o.Root == ""); err != nil {
		return InstallReport{}, err
	}
	note(api.PlistPath)
	if o.BootJob {
		if err := writeOwned(at(api.BootPlistPath), BootPlist(), 0o644, o.Root == ""); err != nil {
			return InstallReport{}, err
		}
		note(api.BootPlistPath)
	}
	var warnings, keys []string
	if o.Resolver {
		path := at(api.ResolverPath)
		if _, err := os.Lstat(path); err == nil {
			if ours, err := isFev(path); err != nil || !ours {
				return InstallReport{}, fmt.Errorf("%s is there and is not fev's; it is left alone and fev.local is not resolved on the Mac until it goes", api.ResolverPath)
			}
		}
	}
	if err := dropOtherForm(o.Root, r.ResolverForm, store); err != nil {
		return InstallReport{}, err
	}
	if o.Resolver && r.ResolverForm == api.ResolverFormStore {
		if _, ok := store.(DirStore); ok {
			made, err := mkdirs(o.Root, StoreStandIn, false)
			if err != nil {
				return InstallReport{}, err
			}
			for _, m := range made {
				note(m)
			}
		}
		if err := store.Set(ownName, r.ResolverPort, now()); err != nil {
			return InstallReport{}, fmt.Errorf("fev.local's resolver entry: %w", err)
		}
		keys = append(keys, api.StoreKey(ownName))
	} else if o.Resolver {
		made, err := mkdirs(o.Root, ResolverDir, o.Root == "")
		if err != nil {
			return InstallReport{}, err
		}
		for _, m := range made {
			note(m)
		}
		if err := writeOwned(at(api.ResolverPath), ResolverFileText(r.ResolverPort), 0o644, o.Root == ""); err != nil {
			return InstallReport{}, err
		}
		note(api.ResolverPath)
	}
	if r.DockerSockLink {
		st, err := LinkDockerSock(at(api.DockerSockPath), dockerSockTarget(r.Home), r.DockerSockConsent)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", api.DockerSockPath, err))
		} else if st.State == api.StateFev {
			note(api.DockerSockPath)
		}
	}
	r.Held = api.HeldFor(recordedBases(r))
	r.ProtocolVersion = api.ProtocolVersion
	r.SecurityLevel = api.SecurityLevel
	r.InstalledAt = now().UTC().Format(time.RFC3339)
	note(api.RecordPath)
	sort.Strings(written)
	r.Paths = written
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return InstallReport{}, err
	}
	if err := writeOwned(at(api.RecordPath), append(b, '\n'), 0o644, o.Root == ""); err != nil {
		return InstallReport{}, err
	}
	if err := holdBases(sysFor(o.Root, o.Sys), r.Held); err != nil {
		warnings = append(warnings, fmt.Sprintf("the base is not held on the Mac while fev is down: %v", err))
	}
	if err := launchctl("bootstrap", "system", at(api.PlistPath)); err != nil {
		return InstallReport{}, fmt.Errorf("launchctl bootstrap %s: %w", api.PlistPath, err)
	}
	if o.BootJob {
		if err := launchctl("bootstrap", "system", at(api.BootPlistPath)); err != nil {
			return InstallReport{}, fmt.Errorf("launchctl bootstrap %s: %w", api.BootPlistPath, err)
		}
	}
	return InstallReport{Record: r, Warnings: warnings, Store: keys}, nil
}

func dropOtherForm(root, form string, store Store) error {
	at := func(p string) string { return rooted(root, p) }
	if form == api.ResolverFormStore {
		present, err := entries(at(ResolverDir))
		if err != nil {
			return err
		}
		for n := range present {
			if err := removeEntry(at(ResolverDir + "/" + n)); err != nil {
				return err
			}
		}
		if resolverState(at(api.ResolverPath)) == api.StateFev {
			return os.Remove(at(api.ResolverPath))
		}
		return nil
	}
	es, err := store.Entries()
	if err != nil {
		return err
	}
	for n := range es {
		if err := store.Remove(n); err != nil {
			return err
		}
	}
	return nil
}

type UndoReport struct {
	Removed []string
	Left    []string
}

func Undo(root string, launchctl func(args ...string) error) (UndoReport, error) {
	return UndoWith(root, launchctl, nil)
}

func UndoWith(root string, launchctl func(args ...string) error, sys System) (UndoReport, error) {
	if root == "" && os.Geteuid() != 0 {
		return UndoReport{}, errors.New("the uninstall runs as root: sudo")
	}
	if launchctl == nil {
		launchctl = runLaunchctl
	}
	at := func(p string) string { return rooted(root, p) }
	var rec api.InstallRecord
	if b, err := os.ReadFile(at(api.RecordPath)); err == nil {
		json.Unmarshal(b, &rec)
	}
	launchctl("bootout", "system/"+api.Label)
	launchctl("bootout", "system/"+BootLabel)
	var rep UndoReport
	if fi, err := os.Lstat(at(api.SocketPath)); err == nil && fi.Mode()&fs.ModeSocket != 0 {
		if err := os.Remove(at(api.SocketPath)); err == nil {
			rep.Removed = append(rep.Removed, api.SocketPath)
		}
	}
	if present, err := entries(at(ResolverDir)); err == nil {
		for n := range present {
			p := ResolverDir + "/" + n
			if err := removeEntry(at(p)); err == nil {
				rep.Removed = append(rep.Removed, p)
			}
		}
	}
	removed, left, err := releaseBases(sysFor(root, sys), recordedHeld(rec))
	if err != nil {
		rep.Left = append(rep.Left, "the reject route for fev's base ("+err.Error()+")")
	}
	for _, b := range removed {
		rep.Removed = append(rep.Removed, "the reject route for "+b.String())
	}
	rep.Left = append(rep.Left, left...)
	store := storeFor(root)
	if es, err := store.Entries(); err == nil {
		for n := range es {
			if err := store.Remove(n); err == nil {
				rep.Removed = append(rep.Removed, api.StoreKey(n))
			} else {
				rep.Left = append(rep.Left, api.StoreKey(n))
			}
		}
	} else {
		rep.Left = append(rep.Left, "fev's resolver entries in the System Configuration store ("+err.Error()+")")
	}
	switch resolverState(at(api.ResolverPath)) {
	case api.StateFev:
		if err := os.Remove(at(api.ResolverPath)); err == nil {
			rep.Removed = append(rep.Removed, api.ResolverPath)
		}
	case api.StateForeign:
		rep.Left = append(rep.Left, api.ResolverPath)
	}
	if rec.Home != "" {
		before, err := DockerSockState(at(api.DockerSockPath), dockerSockTarget(rec.Home))
		if err == nil && before.State == api.StateFev {
			if st, err := UnlinkDockerSock(at(api.DockerSockPath), dockerSockTarget(rec.Home)); err == nil && st.State == api.StateAbsent {
				rep.Removed = append(rep.Removed, api.DockerSockPath)
			}
		}
	}
	for _, p := range []string{api.InstallPath, api.PlistPath, api.BootPlistPath, LockPath, api.RecordPath} {
		err := os.Remove(at(p))
		switch {
		case err == nil:
			rep.Removed = append(rep.Removed, p)
		case !errors.Is(err, fs.ErrNotExist):
			rep.Left = append(rep.Left, p)
		}
	}
	var dirs []string
	for _, p := range rec.Paths {
		if fi, err := os.Lstat(at(p)); err == nil && fi.IsDir() {
			dirs = append(dirs, p)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.Count(dirs[i], "/") > strings.Count(dirs[j], "/") })
	for _, d := range dirs {
		if err := os.Remove(at(d)); err == nil {
			rep.Removed = append(rep.Removed, d)
		} else {
			rep.Left = append(rep.Left, d)
		}
	}
	sort.Strings(rep.Removed)
	sort.Strings(rep.Left)
	return rep, nil
}

func rooted(root, p string) string {
	if root == "" {
		return p
	}
	return filepath.Join(root, p)
}

func runLaunchctl(args ...string) error {
	out, err := exec.Command("/bin/launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func mkdirs(root, dir string, chown bool) ([]string, error) {
	var missing []string
	for d := dir; d != "/" && d != "."; d = filepath.Dir(d) {
		if _, err := os.Lstat(rooted(root, d)); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		missing = append([]string{d}, missing...)
	}
	for _, d := range missing {
		if err := os.Mkdir(rooted(root, d), 0o755); err != nil {
			return nil, err
		}
		if chown {
			if err := os.Lchown(rooted(root, d), 0, 0); err != nil {
				return nil, err
			}
		}
	}
	return missing, nil
}

func CheckRootChain(root, dir string, uid int) error {
	top := rooted(root, "/")
	paths := []string{rooted(root, dir)}
	if real, err := filepath.EvalSymlinks(paths[0]); err == nil && real != paths[0] {
		paths = append(paths, real)
	}
	realTop, _ := filepath.EvalSymlinks(top)
	for _, start := range paths {
		for p := start; ; p = filepath.Dir(p) {
			fi, err := os.Lstat(p)
			if err != nil {
				return err
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				return fmt.Errorf("%s: no owner", p)
			}
			if int(st.Uid) != uid {
				return fmt.Errorf("%s is owned by uid %d, not %d: the helper is installed only where no other user can write", p, st.Uid, uid)
			}
			if fi.Mode()&fs.ModeSymlink == 0 && fi.Mode().Perm()&0o022 != 0 {
				return fmt.Errorf("%s is writable by its group or by others (%v): the helper is installed only where no other user can write", p, fi.Mode().Perm())
			}
			if p == top || p == realTop || p == "/" || p == filepath.Dir(p) {
				break
			}
		}
	}
	return nil
}

func checkHome(home string, uid int) error {
	fi, err := os.Stat(home)
	if err != nil {
		return fmt.Errorf("the user's home: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != uid {
		return fmt.Errorf("%s is not a directory of uid %d's", home, uid)
	}
	return nil
}

func readRegular(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return io.ReadAll(f)
}

func writeOwned(path string, b []byte, mode fs.FileMode, chown bool) error {
	if err := writeAtomic(path, b, mode); err != nil {
		return err
	}
	if chown {
		if err := os.Lchown(path, 0, 0); err != nil {
			return err
		}
	}
	return nil
}

func ConfiguredPlan(home string, uid int) (base, base6 string, err error) {
	base = netplan.DefaultBase.String()
	path := filepath.Join(home, ".fev", "config", "config.json")
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return base, "", nil
	}
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", "", err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != uid || !fi.Mode().IsRegular() {
		return "", "", fmt.Errorf("%s is not a file of uid %d's", path, uid)
	}
	var file struct {
		Values map[string]json.RawMessage `json:"values"`
	}
	if err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&file); err != nil {
		return "", "", fmt.Errorf("%s is not fev's configuration", path)
	}
	read := func(key string) (string, error) {
		raw, ok := file.Values[key]
		if !ok {
			return "", nil
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("%s in %s is not a string", key, path)
		}
		return s, nil
	}
	if s, err := read("network.subnet"); err != nil {
		return "", "", err
	} else if s != "" {
		p, err := netplan.Parse(s)
		if err != nil {
			return "", "", fmt.Errorf("network.subnet: %w", err)
		}
		base = p.Base().String()
	}
	if s, err := read("network.subnet6"); err != nil {
		return "", "", err
	} else if s != "" {
		p, err := netplan.Parse6(s)
		if err != nil {
			return "", "", fmt.Errorf("network.subnet6: %w", err)
		}
		base6 = p.Prefix().String()
	}
	return base, base6, nil
}

const AppBundleID = "dev.fevom.app"

func LaunchdPlist(uid int) []byte {
	return fmt.Appendf(nil, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>AssociatedBundleIdentifiers</key>
	<array>
		<string>%s</string>
	</array>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>serve</string>
	</array>
	<key>inetdCompatibility</key>
	<dict>
		<key>Wait</key>
		<false/>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>Sockets</key>
	<dict>
		<key>Listener</key>
		<dict>
			<key>SockPathName</key>
			<string>%s</string>
			<key>SockPathOwner</key>
			<integer>%d</integer>
			<key>SockPathMode</key>
			<integer>384</integer>
		</dict>
	</dict>
</dict>
</plist>
`, api.Label, AppBundleID, api.InstallPath, api.SocketPath, uid)
}

func BootPlist() []byte {
	return fmt.Appendf(nil, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>AssociatedBundleIdentifiers</key>
	<array>
		<string>%s</string>
	</array>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>boot</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>LaunchOnlyOnce</key>
	<true/>
</dict>
</plist>
`, BootLabel, AppBundleID, api.InstallPath)
}
