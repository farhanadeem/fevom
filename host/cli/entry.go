package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fev/host/daemon/home"
	"fev/host/daemon/launch"
	api "fev/proto/daemon"
)

var execve = syscall.Exec

var ensure = launch.Ensure

const ExitEntry = 125

const EntryMarkEnv = "FEV_DOCKER_ENTRY"

func IsEntryName(name string) bool { return name == "docker" || name == "docker-compose" }

var dockerFlags = map[string]bool{
	"--config": true, "-c": true, "--context": true, "-H": true, "--host": true,
	"-l": true, "--log-level": true, "--tlscacert": true, "--tlscert": true, "--tlskey": true,
}
var composeFlags = map[string]bool{
	"-f": true, "--file": true, "-p": true, "--project-name": true, "--profile": true,
	"--env-file": true, "--project-directory": true, "--parallel": true, "--progress": true, "--ansi": true,
}

type globals struct {
	host, context, config string
	rest                  []string
}

func parseGlobals(name string, argv []string) globals {
	var g globals
	set := func(flag, value string) {
		switch flag {
		case "-H", "--host":
			if g.host == "" {
				g.host = value
			}
		case "-c", "--context":
			g.context = value
		case "--config":
			g.config = value
		}
	}
	takes := func(flag string) bool {
		return dockerFlags[flag] || (name == "docker-compose" && composeFlags[flag])
	}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" || !strings.HasPrefix(arg, "-") || arg == "-" {
			g.rest = argv[i:]
			break
		}
		if strings.HasPrefix(arg, "--") {
			if flag, value, ok := strings.Cut(arg, "="); ok {
				set(flag, value)
			} else if takes(arg) && i+1 < len(argv) {
				i++
				set(arg, argv[i])
			}
			continue
		}
		for j := 1; j < len(arg); j++ {
			flag := "-" + arg[j:j+1]
			if !takes(flag) {
				if j+1 < len(arg) && arg[j+1] == '=' {
					break
				}
				continue
			}
			if value := strings.TrimPrefix(arg[j+1:], "="); value != "" || j+1 < len(arg) {
				set(flag, value)
			} else if i+1 < len(argv) {
				i++
				set(flag, argv[i])
			}
			break
		}
	}
	return g
}

const defaultHost = "unix:///var/run/docker.sock"

func headed(g globals, userHome string) string {
	if g.host != "" {
		return g.host
	}
	name := g.context
	if name == "" {
		if host := os.Getenv("DOCKER_HOST"); host != "" {
			return host
		}
		name = os.Getenv("DOCKER_CONTEXT")
	}
	dir := g.config
	if dir == "" {
		dir = os.Getenv("DOCKER_CONFIG")
	}
	if dir == "" {
		dir = filepath.Join(userHome, ".docker")
	}
	if name == "" {
		data, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err == nil {
			var cfg struct {
				CurrentContext string `json:"currentContext"`
			}
			if json.Unmarshal(data, &cfg) != nil {
				return ""
			}
			name = cfg.CurrentContext
		}
	}
	if name == "" || name == "default" {
		if host := os.Getenv("DOCKER_HOST"); host != "" {
			return host
		}
		return defaultHost
	}
	sum := sha256.Sum256([]byte(name))
	data, err := os.ReadFile(filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json"))
	if err != nil {
		return ""
	}
	var meta struct {
		Endpoints map[string]struct {
			Host string `json:"Host"`
		} `json:"Endpoints"`
	}
	if json.Unmarshal(data, &meta) != nil {
		return ""
	}
	return meta.Endpoints["docker"].Host
}

func resolved(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	if target, err := os.Readlink(path); err == nil {
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		return filepath.Join(dir, filepath.Base(path))
	}
	return filepath.Clean(path)
}

func isFevom(host, socket string) bool {
	path, ok := strings.CutPrefix(host, "unix://")
	if !ok || path == "" {
		return false
	}
	return resolved(path) == resolved(socket)
}

func otherDocker(name string) string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return ""
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 || os.SameFile(info, selfInfo) {
			continue
		}
		return path
	}
	return ""
}

func Entry(e Env) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(e.Stderr, "fev: "+format+"\n", args...)
		return ExitEntry
	}
	if os.Getenv(EntryMarkEnv) == strconv.Itoa(os.Getpid()) {
		return fail("the docker entry point ran itself; is %s a link to fev? Run \"fev setup\" again", filepath.Join("tools", e.Name))
	}
	l, err := home.Resolve("", "")
	if err != nil {
		return fail("%v", err)
	}
	userHome, _ := os.UserHomeDir()
	socket := filepath.Join(l.RunDir(), "docker.sock")
	pinned := filepath.Join(toolsDir(l), e.Name)

	g := parseGlobals(e.Name, e.Args)
	if !isFevom(headed(g, userHome), socket) {
		real := otherDocker(e.Name)
		if real == "" {
			real = pinned
		}
		return become(e, real, fail)
	}
	if completing(g.rest) {
		return complete(e, l, pinned, fail)
	}
	if _, err := os.Stat(api.StopMarkPath(l.Home)); err == nil {
		fmt.Fprintln(e.Stderr, api.StoppedLine)
		return ExitFailed
	}
	opts := launch.Options{Layout: l, Client: e.Name, OriginNS: e.OriginNS, Daemon: daemonBeside(), Args: []string{"--on-demand"}}
	opts.Args = append(opts.Args, strings.Fields(os.Getenv(DaemonArgsEnv))...)
	if err := ensure(opts); err != nil {
		if _, serr := os.Stat(api.StopMarkPath(l.Home)); serr == nil {
			fmt.Fprintln(e.Stderr, api.StoppedLine)
			return ExitFailed
		}
		return fail("cannot start: %v", err)
	}
	if _, err := os.Stat(api.StopMarkPath(l.Home)); err == nil {
		fmt.Fprintln(e.Stderr, api.StoppedLine)
		return ExitFailed
	}
	if _, err := os.Stat(api.PauseMarkPath(l.Home)); err == nil {
		fmt.Fprintln(e.Stderr, api.PausedLine)
	}
	if publishes(e.Name, g.rest) {
		if before, ok := conflicts(l, e.Name); ok {
			return runAndReport(e, l, pinned, before, fail)
		}
	}
	return become(e, pinned, fail)
}

func publishes(name string, rest []string) bool {
	if name == "docker-compose" {
		return len(rest) > 0 && rest[0] == "up"
	}
	if len(rest) > 1 && rest[0] == "container" {
		rest = rest[1:]
	}
	switch {
	case len(rest) == 0:
		return false
	case rest[0] == "run", rest[0] == "start":
		return true
	case rest[0] == "compose":
		sub := parseGlobals("docker-compose", rest[1:]).rest
		return len(sub) > 0 && sub[0] == "up"
	}
	return false
}

const conflictBound = 2 * time.Second

func conflicts(l home.Layout, name string) (map[string]api.Forward, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), conflictBound)
	defer cancel()
	conn, err := net.Dial("unix", l.Socket())
	if err != nil {
		return nil, false
	}
	client, _, err := api.NewClient(ctx, conn, name, helloOptions()...)
	if err != nil {
		return nil, false
	}
	defer client.Close()
	forwards, err := client.Forwards(ctx)
	if err != nil {
		return nil, false
	}
	out := map[string]api.Forward{}
	for _, f := range forwards {
		if f.State == api.ForwardConflict {
			out[f.ID] = f
		}
	}
	return out, true
}

func bindsMacPath(name string, rest []string) bool {
	if name != "docker" {
		return false
	}
	if len(rest) > 1 && rest[0] == "container" {
		rest = rest[1:]
	}
	if len(rest) == 0 || (rest[0] != "run" && rest[0] != "create") {
		return false
	}
	isPath := func(volume string) bool {
		source, _, found := strings.Cut(volume, ":")
		return found && (strings.HasPrefix(source, "/") || strings.HasPrefix(source, ".") || strings.HasPrefix(source, "~"))
	}
	isBind := func(mount string) bool {
		for _, field := range strings.Split(mount, ",") {
			if field == "type=bind" {
				return true
			}
		}
		return false
	}
	afterFlag := false
	for i := 1; i < len(rest); i++ {
		arg := rest[i]
		switch {
		case arg == "--":
			return false
		case arg == "-v" || arg == "--volume":
			if i+1 < len(rest) && isPath(rest[i+1]) {
				return true
			}
			afterFlag = true
		case strings.HasPrefix(arg, "--volume="):
			if isPath(strings.TrimPrefix(arg, "--volume=")) {
				return true
			}
			afterFlag = false
		case arg == "--mount":
			if i+1 < len(rest) && isBind(rest[i+1]) {
				return true
			}
			afterFlag = true
		case strings.HasPrefix(arg, "--mount="):
			if isBind(strings.TrimPrefix(arg, "--mount=")) {
				return true
			}
			afterFlag = false
		case strings.HasPrefix(arg, "-v") && !strings.HasPrefix(arg, "--") && len(arg) > 2:
			if isPath(arg[2:]) {
				return true
			}
			afterFlag = false
		case strings.HasPrefix(arg, "-"):
			afterFlag = !strings.Contains(arg, "=")
		case afterFlag:
			afterFlag = false
		default:
			return false
		}
	}
	return false
}

func sharingOff(l home.Layout, name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), conflictBound)
	defer cancel()
	conn, err := net.Dial("unix", l.Socket())
	if err != nil {
		return false
	}
	client, _, err := api.NewClient(ctx, conn, name, helloOptions()...)
	if err != nil {
		return false
	}
	defer client.Close()
	st, err := client.FilesStatus(ctx)
	return err == nil && !st.Share
}

func runAndReport(e Env, l home.Layout, real string, before map[string]api.Forward, fail func(string, ...any) int) int {
	if _, err := os.Stat(real); err != nil {
		return fail("%s is not installed (%s); run \"fev setup\"", e.Name, real)
	}
	child := exec.Command(real, e.Args...)
	child.Args[0] = e.Name
	child.Env = markedEnv()
	child.Stdin, child.Stdout, child.Stderr = e.Stdin, e.Stdout, e.Stderr
	err := runChild(child)
	if err != nil && bindsMacPath(e.Name, parseGlobals(e.Name, e.Args).rest) && sharingOff(l, e.Name) {
		fmt.Fprintf(e.Stderr, "fev: file sharing is off (%s is false), so a container cannot bind-mount a Mac folder; \"fev config set %s true\" and \"fev restart\" turn it on\n",
			api.KeyFilesShare, api.KeyFilesShare)
	}
	if after, ok := conflicts(l, e.Name); ok {
		ids := make([]string, 0, len(after))
		for id := range after {
			if _, old := before[id]; !old {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Fprintf(e.Stderr, "fev: %s\n", conflictLine(after[id]))
		}
	}
	var ex *exit
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, &ex) && ex.silent:
		return ex.code
	}
	return fail("%v", err)
}

func markedEnv() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, EntryMarkEnv+"=") {
			env = append(env, kv)
		}
	}
	return append(env, EntryMarkEnv+"="+strconv.Itoa(os.Getpid()))
}

func completing(rest []string) bool {
	if len(rest) == 0 {
		return false
	}
	switch rest[0] {
	case "__complete", "__completeNoDesc", "completion":
		return true
	}
	return false
}

func complete(e Env, l home.Layout, pinned string, fail func(string, ...any) int) int {
	up := false
	if conn, err := net.Dial("unix", l.Socket()); err == nil {
		conn.Close()
		info, err := l.ReadInfo()
		up = err == nil && info.VMMPID != 0
	}
	if _, err := os.Stat(api.StopMarkPath(l.Home)); err == nil {
		up = false
	}
	if up {
		return become(e, pinned, fail)
	}
	if _, err := os.Stat(pinned); err != nil {
		return fail("%s is not installed (%s); run \"fev setup\"", e.Name, pinned)
	}
	var env []string
	for _, kv := range markedEnv() {
		if !strings.HasPrefix(kv, "DOCKER_HOST=") && !strings.HasPrefix(kv, "DOCKER_CONTEXT=") {
			env = append(env, kv)
		}
	}
	env = append(env, "DOCKER_HOST=unix://"+filepath.Join(l.RunDir(), "no-engine-while-fev-is-stopped.sock"))
	err := execve(pinned, append([]string{e.Name}, e.Args...), env)
	return fail("cannot run %s: %v", pinned, err)
}

func become(e Env, real string, fail func(string, ...any) int) int {
	info, err := os.Stat(real)
	if err != nil {
		return fail("%s is not installed (%s); run \"fev setup\"", e.Name, real)
	}
	if self, err := os.Executable(); err == nil {
		if selfInfo, err := os.Stat(self); err == nil && os.SameFile(info, selfInfo) {
			return fail("%s is fev itself, not the real %s; run \"fev setup\" again", real, e.Name)
		}
	}
	err = execve(real, append([]string{e.Name}, e.Args...), markedEnv())
	return fail("cannot run %s: %v", real, err)
}
