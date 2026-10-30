package doctor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"fev/host/daemon/home"
	api "fev/proto/daemon"
)

type Severity string

const (
	Problem Severity = "problem"
	Warning Severity = "warning"
	Info    Severity = "info"
)

const (
	FixSockets = "remove-stale-sockets"
	FixSetup   = "setup"
	FixContext = "select-context"
	FixCheck   = "check-data-disk"
)

type Finding struct {
	ID       string   `json:"id"`
	Severity Severity `json:"severity"`
	Text     string   `json:"text"`
	Advice   string   `json:"advice,omitempty"`
	Fix      string   `json:"fix,omitempty"`
	Fixed    bool     `json:"fixed,omitempty"`
	FixError string   `json:"fix_error,omitempty"`
}

type Report struct {
	Home     string    `json:"home"`
	Running  bool      `json:"running"`
	Findings []Finding `json:"findings"`
}

func (r Report) Problems() int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity == Problem && !f.Fixed {
			n++
		}
	}
	return n
}

type Env struct {
	Layout     home.Layout
	UserHome   string
	Getenv     func(string) string
	Self       string
	Tools      string
	ToolNames  []string
	LinkNames  []string
	SetUp      bool
	DockerSock string
	Status     *api.Status
	Answered   bool
}

func (e Env) getenv(name string) string {
	if e.Getenv == nil {
		return os.Getenv(name)
	}
	return e.Getenv(name)
}

func (e Env) binDir() string     { return filepath.Join(e.Layout.Home, "bin") }
func (e Env) dockerSock() string { return filepath.Join(e.Layout.RunDir(), "docker.sock") }

func (e Env) dockerConfig() string {
	if dir := e.getenv("DOCKER_CONFIG"); dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			return abs
		}
		return dir
	}
	return filepath.Join(e.UserHome, ".docker")
}

func Check(e Env) Report {
	r := Report{Home: e.Layout.Home, Running: e.Answered, Findings: []Finding{}}
	for _, check := range []func(Env) []Finding{
		checkSockets, checkLinks, checkTools, checkPath, checkDockerEnv, checkContexts, checkSystemSocket, checkDaemon,
	} {
		r.Findings = append(r.Findings, check(e)...)
	}
	return r
}

func daemonAlive(l home.Layout) bool {
	if _, err := os.Stat(l.DaemonLock()); err != nil {
		return false
	}
	f, held, err := home.Lock(l.DaemonLock(), false)
	if err != nil {
		return false
	}
	if f != nil {
		f.Close()
	}
	return held
}

func accepts(path string) bool {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func staleSockets(l home.Layout) []string {
	if daemonAlive(l) {
		return nil
	}
	var stale []string
	filepath.WalkDir(l.RunDir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&fs.ModeSocket != 0 && !accepts(path) {
			stale = append(stale, path)
		}
		return nil
	})
	sort.Strings(stale)
	return stale
}

func checkSockets(e Env) []Finding {
	var out []Finding
	if !e.Answered && daemonAlive(e.Layout) {
		out = append(out, Finding{ID: "daemon.unresponsive", Severity: Problem,
			Text:   "a fev daemon holds " + e.Layout.DaemonLock() + " but does not answer on " + e.Layout.Socket(),
			Advice: "`fev stop --force` ends it; the next fev or docker command starts a new one. `fev report` before that keeps its logs for a bug report"})
	}
	if stale := staleSockets(e.Layout); len(stale) > 0 {
		out = append(out, Finding{ID: "daemon.stale_sockets", Severity: Warning,
			Text:   fmt.Sprintf("%d socket(s) left by a daemon that is gone: %s", len(stale), strings.Join(stale, ", ")),
			Advice: "harmless to fev, which replaces them at its next start; a program that dials one directly is refused until then",
			Fix:    FixSockets})
	}
	return out
}

func RemoveStaleSockets(l home.Layout) ([]string, error) {
	var removed []string
	for _, path := range staleSockets(l) {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, err
		}
		removed = append(removed, path)
	}
	return removed, nil
}

func checkLinks(e Env) []Finding {
	if !e.SetUp {
		return nil
	}
	var missing, elsewhere []string
	for _, name := range e.LinkNames {
		path := filepath.Join(e.binDir(), name)
		target, err := filepath.EvalSymlinks(path)
		switch {
		case err != nil:
			missing = append(missing, name)
		case e.Self != "" && target != e.Self:
			elsewhere = append(elsewhere, path+" -> "+target)
		}
	}
	var out []Finding
	if len(missing) > 0 {
		out = append(out, Finding{ID: "setup.links_missing", Severity: Problem,
			Text: "fev's links are missing from " + e.binDir() + ": " + strings.Join(missing, ", "),
			Fix:  FixSetup})
	}
	if len(elsewhere) > 0 {
		out = append(out, Finding{ID: "setup.links_elsewhere", Severity: Warning,
			Text:   "fev's links point at another fev than this one (" + e.Self + "): " + strings.Join(elsewhere, "; "),
			Advice: "a fev moved or installed again since setup ran; setup links them to this one",
			Fix:    FixSetup})
	}
	return out
}

func checkTools(e Env) []Finding {
	if !e.SetUp || e.Tools == "" {
		return nil
	}
	var missing []string
	for _, name := range e.ToolNames {
		info, err := os.Stat(filepath.Join(e.Tools, name))
		if err != nil || info.Mode()&0o111 == 0 {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []Finding{{ID: "setup.tools_missing", Severity: Problem,
		Text: "Client tools are missing from " + e.Tools + ": " + strings.Join(missing, ", "),
		Fix:  FixSetup}}
}

func whose(path string) string {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		real = path
	}
	switch {
	case strings.Contains(real, "/Docker.app/"):
		return "Docker Desktop"
	case strings.HasPrefix(real, "/opt/homebrew/") || strings.HasPrefix(real, "/usr/local/Cellar/") || strings.HasPrefix(real, "/usr/local/Homebrew/"):
		return "Homebrew"
	case strings.HasPrefix(real, "/nix/"):
		return "Nix"
	case strings.HasPrefix(real, "/opt/local/"):
		return "MacPorts"
	}
	return "another Docker install"
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

func checkPath(e Env) []Finding {
	dirs := filepath.SplitList(e.getenv("PATH"))
	bin := e.binDir()
	at := -1
	for i, dir := range dirs {
		if dir != "" && samePath(dir, bin) {
			at = i
			break
		}
	}
	var out []Finding
	if at < 0 {
		if !e.SetUp {
			return []Finding{{ID: "path.not_set_up", Severity: Warning,
				Text:   "fev is not set up in this home: " + bin + " is not on PATH, so docker here is not fev's",
				Advice: "`fev setup` puts it first on PATH (one fenced block in your shell's profile; `fev setup --undo` takes it out)"}}
		}
		return []Finding{{ID: "path.missing", Severity: Problem,
			Text:   bin + " is not on PATH in this shell, so docker here is not fev's",
			Advice: "a shell opened before `fev setup` ran has the old PATH: open a new terminal. If a new one has it too, your shell's profile does not read fev's block, or replaces PATH after it",
			Fix:    FixSetup}}
	}
	for _, name := range []string{"docker", "docker-compose"} {
		for _, dir := range dirs[:at] {
			if dir == "" {
				continue
			}
			path := filepath.Join(dir, name)
			if !executable(path) || samePath(path, filepath.Join(bin, name)) {
				continue
			}
			out = append(out, Finding{ID: "path.shadowed", Severity: Problem,
				Text: fmt.Sprintf("%s (%s) comes before fev's on PATH: `%s` runs it, not fev's, which starts fev by itself and follows fev's Docker context",
					path, whose(path), name),
				Advice: fmt.Sprintf("put %s first on PATH: something in your shell's startup files adds %s ahead of it after fev's block (a line in ~/.zshrc or ~/.bashrc, or the other install's own block). Move fev's block below it, or remove that line", bin, dir)})
			break
		}
	}
	return out
}

func (e Env) isFevomHost(host string) bool {
	path, ok := strings.CutPrefix(host, "unix://")
	return ok && path != "" && samePath(path, e.dockerSock())
}

func checkDockerEnv(e Env) []Finding {
	var out []Finding
	if host := e.getenv("DOCKER_HOST"); host != "" && !e.isFevomHost(host) {
		out = append(out, Finding{ID: "docker.host_env", Severity: Warning,
			Text: "DOCKER_HOST is set to " + host + " in this shell: docker commands go there, not to fev",
			Advice: "unset it (`unset DOCKER_HOST`) and remove the line that sets it from your shell's startup files" +
				setters(e, "DOCKER_HOST") + "; with it unset, the fev Docker context decides"})
	}
	if name := e.getenv("DOCKER_CONTEXT"); name != "" && name != contextName {
		out = append(out, Finding{ID: "docker.context_env", Severity: Warning,
			Text:   "DOCKER_CONTEXT is set to " + name + " in this shell: docker commands use that context, not fev's",
			Advice: "unset it (`unset DOCKER_CONTEXT`) and remove the line that sets it" + setters(e, "DOCKER_CONTEXT")})
	}
	return out
}

func setters(e Env, variable string) string {
	var found []string
	for _, name := range []string{".zshenv", ".zprofile", ".zshrc", ".bash_profile", ".bashrc", ".profile", ".config/fish/config.fish"} {
		path := filepath.Join(e.UserHome, name)
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), variable) {
			found = append(found, path)
		}
	}
	if len(found) == 0 {
		return ""
	}
	return " (" + variable + " appears in " + strings.Join(found, ", ") + ")"
}

const contextName = "fev"

type contextMeta struct {
	Name      string `json:"Name"`
	Endpoints map[string]struct {
		Host string `json:"Host"`
	} `json:"Endpoints"`
}

func contextsOf(dir string) map[string]contextMeta {
	out := map[string]contextMeta{}
	entries, _ := os.ReadDir(filepath.Join(dir, "contexts", "meta"))
	for _, ent := range entries {
		data, err := os.ReadFile(filepath.Join(dir, "contexts", "meta", ent.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var m contextMeta
		if json.Unmarshal(data, &m) != nil || m.Name == "" {
			continue
		}
		sum := sha256.Sum256([]byte(m.Name))
		if hex.EncodeToString(sum[:]) != ent.Name() {
			continue
		}
		out[m.Name] = m
	}
	return out
}

func currentContext(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return "default", nil
	}
	if err != nil {
		return "", err
	}
	var cfg struct {
		CurrentContext string `json:"currentContext"`
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return "default", nil
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", err
	}
	if cfg.CurrentContext == "" {
		return "default", nil
	}
	return cfg.CurrentContext, nil
}

func socketThere(host string) (path string, unix, there bool) {
	path, unix = strings.CutPrefix(host, "unix://")
	if !unix {
		return "", false, false
	}
	_, err := os.Stat(path)
	return path, true, err == nil
}

func checkContexts(e Env) []Finding {
	dir := e.dockerConfig()
	var out []Finding
	current, err := currentContext(dir)
	if err != nil {
		return []Finding{{ID: "docker.config_unreadable", Severity: Problem,
			Text:   filepath.Join(dir, "config.json") + " cannot be read as docker reads it: " + err.Error(),
			Advice: "docker refuses every command until it is valid JSON again; fix or move it"}}
	}
	contexts := contextsOf(dir)
	ours, defined := contexts[contextName]
	if defined && !e.isFevomHost(ours.Endpoints["docker"].Host) {
		out = append(out, Finding{ID: "docker.context_outdated", Severity: Problem,
			Text: fmt.Sprintf("the fev Docker context points at %s, not at this home's %s", ours.Endpoints["docker"].Host, e.dockerSock()),
			Fix:  FixContext})
	}
	switch {
	case current == contextName && !defined:
		out = append(out, Finding{ID: "docker.context_missing", Severity: Problem,
			Text: "the current Docker context is fev's, and it is not defined in " + dir + ": docker refuses every command",
			Fix:  FixContext})
	case current == contextName, e.getenv("DOCKER_HOST") != "" || e.getenv("DOCKER_CONTEXT") != "":
	case current == "default":
		if !e.SetUp && !defined {
			break
		}
		out = append(out, Finding{ID: "docker.context_default", Severity: Warning,
			Text: "the current Docker context is the default one (/var/run/docker.sock), not fev's: docker commands reach fev only if that socket is fev's",
			Fix:  FixContext})
	default:
		other, there := contexts[current]
		text := "the current Docker context is " + current + ", not fev's: docker commands go to " + other.Endpoints["docker"].Host
		sev := Warning
		if !there {
			text = "the current Docker context is " + current + ", which is not defined: docker refuses every command"
			sev = Problem
		} else if path, unix, ok := socketThere(other.Endpoints["docker"].Host); unix && !ok {
			text += ", whose socket " + path + " is not there: docker commands fail"
			sev = Problem
		}
		out = append(out, Finding{ID: "docker.context_other", Severity: sev, Text: text,
			Advice: "`docker context use fev` by hand; --fix does the same, and `fev config set docker.set_context false` puts the other back",
			Fix:    FixContext})
	}
	var gone []string
	for name, m := range contexts {
		if name == contextName || name == current {
			continue
		}
		if path, unix, ok := socketThere(m.Endpoints["docker"].Host); unix && !ok {
			gone = append(gone, name+" ("+path+")")
		}
	}
	if len(gone) > 0 {
		sort.Strings(gone)
		out = append(out, Finding{ID: "docker.contexts_stale", Severity: Info,
			Text:   "Docker contexts whose socket is not there: " + strings.Join(gone, ", "),
			Advice: "their engine is not running, or was removed; `docker context rm NAME` removes one you no longer use"})
	}
	return out
}

func checkSystemSocket(e Env) []Finding {
	path := e.DockerSock
	if path == "" {
		path = "/var/run/docker.sock"
	}
	target, err := os.Readlink(path)
	if err != nil {
		return nil
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	if samePath(target, e.dockerSock()) {
		return nil
	}
	if _, err := os.Stat(target); err != nil {
		return []Finding{{ID: "docker.system_socket_stale", Severity: Warning,
			Text:   path + " is a link to " + target + ", which is not there: tools that use " + path + " directly fail",
			Advice: "`sudo fev setup --admin --replace-docker-sock` makes it fev's"}}
	}
	return []Finding{{ID: "docker.system_socket_other", Severity: Info,
		Text:   path + " is a link to " + target + " (" + whose(target) + "): tools that use it directly reach that engine, not fev",
		Advice: "`sudo fev setup --admin --replace-docker-sock` makes it fev's"}}
}

const (
	RecoveredPrefix = "fev's data disk did not mount normally"
	DamagedPrefix   = "fev's data disk is damaged and could not be repaired"
)

func IsRecovered(warning string) bool {
	return strings.HasPrefix(warning, RecoveredPrefix) || strings.HasPrefix(warning, DamagedPrefix)
}

func checkDaemon(e Env) []Finding {
	st := e.Status
	if st == nil {
		return nil
	}
	var out []Finding
	if st.Engine == api.EngineFailed {
		text := "the Docker engine failed to start"
		if st.Error != "" {
			text += ": " + st.Error
		}
		out = append(out, Finding{ID: "daemon.engine_failed", Severity: Problem, Text: text,
			Advice: "`fev logs --files --source engine` has its lines; the warnings below say what the daemon knows of why"})
	}
	if st.State == api.StateStopped && st.Error != "" && st.Engine != api.EngineFailed {
		out = append(out, Finding{ID: "daemon.vm_failed", Severity: Problem, Text: "the VM is stopped: " + st.Error,
			Advice: "`fev logs --files` has the daemon's, the VM's and the guest's lines"})
	}
	for _, w := range st.Warnings {
		f := Finding{ID: "daemon.warning", Severity: Warning, Text: w}
		switch {
		case strings.HasPrefix(w, RecoveredPrefix):
			f.ID, f.Severity = "data.recovered", Problem
			if st.State == api.StateRunning || st.State == api.StateStarting && st.Phase == api.PhaseEngine {
				f.Fix = FixCheck
				f.Advice = "--fix checks every block of the data disk against its checksum now (storage.check) and says what it found"
			}
		case strings.HasPrefix(w, DamagedPrefix):
			f.ID, f.Severity = "data.damaged", Problem
			f.Advice = "nothing repairs it in place: `fev salvage DIR` copies what can be read off it, then `fev reset` makes a new, empty disk"
		}
		out = append(out, f)
	}
	return out
}
