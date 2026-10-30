package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/debug/attach"
	"fev/guest/internal/copytar"
	"fev/guest/internal/isolate"
	"fev/guest/internal/thp"
)

const DebugExtraBaseFd = entrySpecFd + 1

const EntryName = "fev-session-entry"

const EntryUserName = "fev-session-entry-user"

func IsEntry(args []string) bool {
	return len(args) > 0 && args[0] == EntryName || len(args) == 2 && args[1] == EntryUserName
}

const FileLimit = 1 << 20

const (
	entrySpecFd   = 3
	entryMountFd  = 4
	entryCgroupFd = 5
	entryUserFd   = 6
	entryStageFd  = 7
	entrySelfFd   = 8
)

type Cred struct {
	UID    uint32   `json:"uid"`
	GID    uint32   `json:"gid"`
	Groups []uint32 `json:"groups"`
}

type entrySpec struct {
	Path       string             `json:"path,omitempty"`
	Argv       []string           `json:"argv"`
	Env        []string           `json:"env"`
	Dir        string             `json:"dir"`
	Home       string             `json:"home,omitempty"`
	Enter      bool               `json:"enter,omitempty"`
	EnterUser  bool               `json:"enter_user,omitempty"`
	Isolate    bool               `json:"isolate,omitempty"`
	Cred       *Cred              `json:"cred,omitempty"`
	Copy       string             `json:"copy,omitempty"`
	RuntimeDir string             `json:"runtime_dir,omitempty"`
	Name       string             `json:"name"`
	Debug      *attach.DebugEntry `json:"debug,omitempty"`
}

func EntryMain() {
	runtime.LockOSThread()
	var spec entrySpec
	f := os.NewFile(entrySpecFd, "spec")
	b, err := io.ReadAll(f)
	f.Close()
	if err == nil {
		err = json.Unmarshal(b, &spec)
	}
	if err != nil {
		entryFail(126, "fev", "the session's spec: %v", err)
	}
	name := spec.Name
	if name == "" {
		name = "fev"
	}
	if spec.Debug != nil {
		attach.Run(*spec.Debug)
		entryFail(126, name, "the debug attach returned")
	}
	if spec.Enter {
		if err := thp.Restore(); err != nil {
			fmt.Fprintf(os.Stderr, "%s: the session runs without transparent huge pages: %v\n", name, err)
		}
	}
	if spec.EnterUser {
		lim := unix.Rlimit{Cur: FileLimit, Max: FileLimit}
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
			fmt.Fprintf(os.Stderr, "%s: the descriptor limit stays as it was: %v\n", name, err)
		}
		if spec.Isolate {
			if err := isolate.Install(); err != nil {
				entryFail(126, name, "%v", err)
			}
		}
		enterUser(name, spec)
	}
	if spec.Enter {
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			entryFail(126, name, "unshare: %v", err)
		}
		if err := unix.Setns(entryMountFd, unix.CLONE_NEWNS); err != nil {
			entryFail(126, name, "enter the machine's mount namespace: %v", err)
		}
		if err := unix.Setns(entryCgroupFd, unix.CLONE_NEWCGROUP); err != nil {
			entryFail(126, name, "enter the machine's cgroup namespace: %v", err)
		}
	}
	userStage := len(os.Args) == 2 && os.Args[1] == EntryUserName
	if spec.Copy != "" && !userStage {
		entryFail(126, name, "a copy is made only in an isolated machine's user stage")
	}
	unix.CloseRange(3, ^uint(0), unix.CLOSE_RANGE_CLOEXEC)
	lim := unix.Rlimit{Cur: FileLimit, Max: FileLimit}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		fmt.Fprintf(os.Stderr, "%s: the descriptor limit stays as it was: %v\n", name, err)
	}
	if c := spec.Cred; c != nil && spec.Copy != "" {
		if err := allThreadsCred(c); err != nil {
			entryFail(126, name, "%v", err)
		}
	} else if c != nil {
		groups := c.Groups
		if len(groups) == 0 {
			groups = []uint32{c.GID}
		}
		if _, _, e := unix.RawSyscall(unix.SYS_SETGROUPS, uintptr(len(groups)), uintptr(unsafe.Pointer(&groups[0])), 0); e != 0 {
			entryFail(126, name, "setgroups: %v", e)
		}
		if _, _, e := unix.RawSyscall(unix.SYS_SETRESGID, uintptr(c.GID), uintptr(c.GID), uintptr(c.GID)); e != 0 {
			entryFail(126, name, "setresgid: %v", e)
		}
		if _, _, e := unix.RawSyscall(unix.SYS_SETRESUID, uintptr(c.UID), uintptr(c.UID), uintptr(c.UID)); e != 0 {
			entryFail(126, name, "setresuid: %v", e)
		}
	}
	env := spec.Env
	if spec.RuntimeDir != "" {
		env = withRuntimeDir(env, spec.RuntimeDir)
	}
	dir := spec.Dir
	if dir == "" {
		dir = spec.Home
	}
	if dir == "" {
		dir = "/"
	}
	if err := unix.Chdir(dir); err != nil {
		if spec.Home == "" || dir == spec.Home {
			entryFail(1, name, "cd %s: %v", dir, err)
		}
		fmt.Fprintf(os.Stderr, "%s: %s is not in this machine (%v); starting in %s\n", name, dir, err, spec.Home)
		if err := unix.Chdir(spec.Home); err != nil {
			unix.Chdir("/")
		}
	}
	if spec.Copy != "" {
		unix.Close(entrySelfFd)
		os.Exit(runCopy(name, spec.Copy, spec.Argv))
	}
	path := spec.Path
	if path == "" {
		path, err = findProgram(spec.Argv[0], env)
		if err != nil {
			code := 127
			if errors.Is(err, unix.EACCES) {
				code = 126
			}
			entryFail(code, name, "%v", err)
		}
	}
	err = syscall.Exec(path, spec.Argv, env)
	code := 126
	if errors.Is(err, unix.ENOENT) {
		code = 127
	}
	entryFail(code, name, "%s: %v", spec.Argv[0], err)
}

func enterUser(name string, spec entrySpec) {
	spec.EnterUser, spec.Enter, spec.Isolate = false, false, false
	b, _ := json.Marshal(spec)
	mfd, err := unix.MemfdCreate("fev-session-spec", 0)
	if err != nil {
		entryFail(126, name, "the user stage's spec: %v", err)
	}
	if _, err := unix.Write(mfd, b); err != nil {
		entryFail(126, name, "the user stage's spec: %v", err)
	}
	if _, err := unix.Seek(mfd, 0, io.SeekStart); err != nil {
		entryFail(126, name, "the user stage's spec: %v", err)
	}
	if mfd != entrySpecFd {
		if err := unix.Dup3(mfd, entrySpecFd, 0); err != nil {
			entryFail(126, name, "the user stage's spec: %v", err)
		}
		unix.Close(mfd)
	}
	for _, fd := range []int{entryMountFd, entryCgroupFd, entryUserFd, entrySelfFd} {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
			entryFail(126, name, "descriptor %d: %v", fd, err)
		}
	}
	if _, err := unix.FcntlInt(uintptr(entryStageFd), unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
		entryFail(126, name, "descriptor %d: %v", entryStageFd, err)
	}
	argv := []string{"fev-isostage", EntryUserName}
	err = unix.Exec(fmt.Sprintf("/proc/self/fd/%d", entryStageFd), argv, []string{})
	entryFail(126, name, "join the machine's user namespace: %v", err)
}

func allThreadsCred(c *Cred) error {
	groups := []int{int(c.GID)}
	if len(c.Groups) > 0 {
		groups = groups[:0]
		for _, g := range c.Groups {
			groups = append(groups, int(g))
		}
	}
	if err := syscall.Setgroups(groups); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setresgid(int(c.GID), int(c.GID), int(c.GID)); err != nil {
		return fmt.Errorf("setresgid: %w", err)
	}
	if err := syscall.Setresuid(int(c.UID), int(c.UID), int(c.UID)); err != nil {
		return fmt.Errorf("setresuid: %w", err)
	}
	return nil
}

func runCopy(name, dir string, argv []string) int {
	var err error
	warn := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, name+": "+format+"\n", args...)
	}
	switch dir {
	case api.CopyIn:
		if err = copytar.Unpack(os.Stdin, argv[0], argv[1:], warn); err == nil {
			io.Copy(io.Discard, os.Stdin)
		}
	case api.CopyOut:
		err = copytar.Pack(os.Stdout, argv, warn)
	default:
		err = fmt.Errorf("no copy %q", dir)
	}
	if err != nil {
		warn("%v", err)
		return 1
	}
	return 0
}

func entryFail(code int, name, format string, args ...any) {
	fmt.Fprintf(os.Stderr, name+": "+format+"\n", args...)
	os.Exit(code)
}

func findProgram(file string, env []string) (string, error) {
	if strings.Contains(file, "/") {
		return file, nil
	}
	path := defaultPath
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			path = v
		}
	}
	denied := ""
	for _, dir := range strings.Split(path, ":") {
		if dir == "" {
			dir = "."
		}
		candidate := dir + "/" + file
		st, err := os.Stat(candidate)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		if unix.Access(candidate, unix.X_OK) == nil {
			return candidate, nil
		}
		if denied == "" {
			denied = candidate
		}
	}
	if denied != "" {
		return "", fmt.Errorf("%s: %w", denied, unix.EACCES)
	}
	return "", fmt.Errorf("%s: command not found", file)
}

func withRuntimeDir(env []string, dir string) []string {
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return env
	}
	has := func(name string) bool {
		for _, e := range env {
			if strings.HasPrefix(e, name+"=") {
				return true
			}
		}
		return false
	}
	if !has("XDG_RUNTIME_DIR") {
		env = append(env, "XDG_RUNTIME_DIR="+dir)
	}
	if _, err := os.Stat(dir + "/bus"); err == nil && !has("DBUS_SESSION_BUS_ADDRESS") {
		env = append(env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+dir+"/bus")
	}
	return env
}

type Proc struct {
	Path                      string
	Argv                      []string
	Env                       []string
	Dir                       string
	Cred                      *Cred
	MountNS, CgroupNS, UserNS *os.File
	Stage                     string
	Isolate                   bool
	Files                     [3]*os.File
	Sys                       *syscall.SysProcAttr
}

func (p Proc) Attr(self string) (path string, argv []string, attr *os.ProcAttr, release func(), err error) {
	if p.MountNS == nil || p.CgroupNS == nil || p.UserNS == nil {
		return "", nil, nil, nil, errors.New("a process in a user namespace needs its mount, cgroup and user namespaces")
	}
	spec := entrySpec{Path: p.Path, Argv: p.Argv, Env: p.Env, Dir: p.Dir, Enter: true, EnterUser: true, Isolate: p.Isolate, Cred: p.Cred, Name: "fev"}
	if spec.Env == nil {
		spec.Env = []string{}
	}
	if spec.Dir == "" {
		spec.Dir = "/"
	}
	b, _ := json.Marshal(spec)
	mfd, err := unix.MemfdCreate("fev-proc-spec", unix.MFD_CLOEXEC)
	if err != nil {
		return "", nil, nil, nil, err
	}
	specF := os.NewFile(uintptr(mfd), "spec")
	if _, err := specF.Write(b); err != nil {
		specF.Close()
		return "", nil, nil, nil, err
	}
	if _, err := specF.Seek(0, io.SeekStart); err != nil {
		specF.Close()
		return "", nil, nil, nil, err
	}
	stage, err := os.Open(p.Stage)
	if err != nil {
		specF.Close()
		return "", nil, nil, nil, err
	}
	selfF, err := os.Open(self)
	if err != nil {
		specF.Close()
		stage.Close()
		return "", nil, nil, nil, err
	}
	files := []*os.File{p.Files[0], p.Files[1], p.Files[2], specF, p.MountNS, p.CgroupNS, p.UserNS, stage, selfF}
	attr = &os.ProcAttr{Dir: "/", Env: []string{}, Files: files, Sys: p.Sys}
	release = func() { specF.Close(); stage.Close(); selfF.Close() }
	return self, []string{EntryName}, attr, release, nil
}
