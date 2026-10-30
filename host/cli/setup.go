package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"fev/host/bundle"
	"fev/host/daemon/home"
	"fev/host/docker/dockerctx"
	"fev/host/docker/tools"
	api "fev/proto/daemon"
)

const ToolsEnv = "FEV_TOOLS"

func binDir(l home.Layout) string { return filepath.Join(l.Home, "bin") }

func toolsDir(l home.Layout) string {
	if dir := os.Getenv(ToolsEnv); dir != "" {
		return dir
	}
	return toolsFor(l, bundle.Self())
}

func toolsFor(l home.Layout, self string) string {
	if dir := bundle.Tools(self); dir != "" {
		return dir
	}
	return downloadedTools(l)
}

func downloadedTools(l home.Layout) string { return filepath.Join(l.Home, "tools") }

func setupState(l home.Layout) string { return filepath.Join(l.Home, "setup.json") }

var selfNames = []string{"fev", "fevctl", "docker", "docker-compose"}

var pathTools = []string{"docker-buildx", "docker-credential-osxkeychain", "kubectl"}

const (
	fenceOpen  = "# >>> fev setup >>>"
	fenceClose = "# <<< fev setup <<<"
)

type profileEdit struct {
	Shell   string   `json:"shell"`
	Path    string   `json:"path"`
	Created bool     `json:"created,omitempty"`
	Newline bool     `json:"newline,omitempty"`
	Dirs    []string `json:"dirs,omitempty"`
}

type setupRecord struct {
	Profiles     []profileEdit        `json:"profiles,omitempty"`
	DockerConfig string               `json:"docker_config,omitempty"`
	PluginDir    bool                 `json:"plugin_dir,omitempty"`
	DockerDir    bool                 `json:"docker_dir,omitempty"`
	CredsStore   bool                 `json:"creds_store,omitempty"`
	CredsFile    bool                 `json:"creds_file,omitempty"`
	CredsNote    *dockerctx.CredsNote `json:"creds_note,omitempty"`
	CredsUnnamed bool                 `json:"creds_unnamed,omitempty"`
	ContextUsed  bool                 `json:"context_used,omitempty"`
	SSH          *sshEdit             `json:"ssh,omitempty"`
}

func readSetup(l home.Layout) setupRecord {
	var r setupRecord
	if data, err := os.ReadFile(setupState(l)); err == nil {
		json.Unmarshal(data, &r)
	}
	return r
}

func writeSetup(l home.Layout, r setupRecord) error {
	data, _ := json.MarshalIndent(r, "", "  ")
	return replaceFile(setupState(l), append(data, '\n'), 0o644)
}

func replaceFile(path string, data []byte, mode fs.FileMode) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".fev.*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), mode)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

func shellPath(dir, userHome string) string {
	if rel, err := filepath.Rel(userHome, dir); err == nil && !strings.HasPrefix(rel, "..") {
		return "$HOME/" + rel
	}
	return dir
}

func block(shell, dir, userHome string) string {
	path := shellPath(dir, userHome)
	line := fmt.Sprintf(`export PATH="%s:$PATH"`, path)
	if shell == "fish" {
		line = fmt.Sprintf(`set -gx PATH "%s" $PATH`, path)
	}
	return fenceOpen + "\n" +
		"# fev's commands, and the docker that starts fev. \"fev setup --undo\" removes this.\n" +
		line + "\n" + completionLines(shell, filepath.Dir(dir), userHome) + fenceClose + "\n"
}

func profileFor(shell, userHome string) (string, error) {
	switch shell {
	case "zsh":
		dir := os.Getenv("ZDOTDIR")
		if dir == "" {
			dir = userHome
		}
		return filepath.Join(dir, ".zprofile"), nil
	case "bash":
		for _, name := range []string{".bash_profile", ".bash_login", ".profile"} {
			path := filepath.Join(userHome, name)
			if _, err := os.Stat(path); err == nil {
				return path, nil
			}
		}
		return filepath.Join(userHome, ".bash_profile"), nil
	case "fish":
		dir := os.Getenv("XDG_CONFIG_HOME")
		if dir == "" {
			dir = filepath.Join(userHome, ".config")
		}
		return filepath.Join(dir, "fish", "conf.d", "fev.fish"), nil
	}
	return "", usagef("setup: --shell is zsh, bash or fish, not %q", shell)
}

func addBlock(shell, path, text string) (edit profileEdit, changed bool, err error) {
	edit = profileEdit{Shell: shell, Path: path}
	old, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		edit.Created = true
		for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
			if _, err := os.Stat(dir); err == nil || dir == filepath.Dir(dir) {
				break
			}
			edit.Dirs = append([]string{dir}, edit.Dirs...)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return edit, false, err
		}
	case err != nil:
		return edit, false, err
	}
	if start := bytes.Index(old, []byte(fenceOpen)); start >= 0 {
		end := bytes.Index(old[start:], []byte(fenceClose+"\n"))
		if end < 0 || string(old[start:start+end+len(fenceClose)+1]) == text {
			return edit, false, nil
		}
		end += start + len(fenceClose) + 1
		updated := append(append(append([]byte{}, old[:start]...), text...), old[end:]...)
		return edit, true, replaceFile(path, updated, 0o644)
	}
	if len(old) > 0 && old[len(old)-1] != '\n' {
		edit.Newline = true
		old = append(old, '\n')
	}
	return edit, true, replaceFile(path, append(old, text...), 0o644)
}

func removeBlock(edit profileEdit) error {
	data, err := os.ReadFile(edit.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	start := bytes.Index(data, []byte(fenceOpen))
	if start < 0 {
		return nil
	}
	end := bytes.Index(data[start:], []byte(fenceClose+"\n"))
	if end < 0 {
		return fmt.Errorf("%s: the end of fev's block is missing; take it out by hand", edit.Path)
	}
	end += start + len(fenceClose) + 1
	if edit.Newline && start > 0 && data[start-1] == '\n' {
		start--
	}
	rest := append(append([]byte{}, data[:start]...), data[end:]...)
	if edit.Created && len(rest) == 0 {
		real := edit.Path
		if err := os.Remove(real); err != nil {
			return err
		}
		for i := len(edit.Dirs) - 1; i >= 0; i-- {
			if os.Remove(edit.Dirs[i]) != nil {
				break
			}
		}
		return nil
	}
	return replaceFile(edit.Path, rest, 0o644)
}

func link(dir, name, target string) (changed bool, err error) {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return false, err
	case info.Mode()&os.ModeSymlink == 0:
		return false, fmt.Errorf("%s is in the way and is not a link; move it and run setup again", path)
	default:
		if have, _ := os.Readlink(path); have == target {
			return false, nil
		}
	}
	tmp := path + ".fev.tmp"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}

func (a *app) setupCmd() *cobra.Command {
	var undo, noTools, noStart, removeContext bool
	var shells []string
	var cache string
	var admin adminOptions
	var adminFlag, https bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Put fev and its docker on PATH; --undo takes it all back",
		Long: `Make fev's commands, and a docker that starts fev by itself, the first
ones your shell finds:

  - links in ~/.fev/bin for fev, fevctl, docker and docker-compose;
  - the pinned Docker client tools: from Fevom.app's bundle when this fev
    is the app's (nothing downloaded), otherwise downloaded and checked
    into ~/.fev/tools; "docker compose" and "docker buildx" linked as
    CLI plugins, and docker-credential-osxkeychain on PATH, where docker
    finds it and keeps registry passwords in your keychain by itself
    (~/.docker/config.json is not changed);
  - one fenced block at the end of your shell's profile that puts
    ~/.fev/bin first on PATH;
  - one fenced Include at the top of ~/.ssh/config of fev's SSH entry,
    so that "ssh fev" reaches the Linux machines (taken out by you, it
    is not put back).

It makes the "fev" Docker context the current one, whatever was selected
before (remembered, so that --undo and uninstalling put it back): docker
commands go to fev. A context you select later is left as you chose it.
It then starts the daemon, without a VM.

--undo removes the links and the blocks: the profile and ~/.ssh/config are
then byte for byte what they were, and a credsStore "osxkeychain" a docker
login wrote meanwhile is taken out of ~/.docker/config.json (the helper it
names goes with fev). It leaves the admin helper and the Mac's trust in
fev's root: "sudo fev setup --admin --undo" and "fev setup --https
--undo" remove those. It starts nothing. With --remove-context (fev
stopped, as tools/uninstall.sh runs it) the Docker context selected before
fev's is current again and fev's is removed, as --undo alone does when
setup made it current. Typed at a terminal, when the tools
were downloaded, it then offers to remove them as well: they go if the
answer is "y", and stay otherwise, with no answer too. With stdin not a
terminal (a script) nothing is asked, and they stay. Running setup twice
changes nothing the second time.

sudo fev setup --admin installs fev's admin helper, the one part of
fev that runs as root: it lets fev route containers' and machines'
addresses from this Mac, answer *.fev.local names on it, and take ports
below 1024 on 127.0.0.1. It is not running between its calls, and is
checked by its version, so a rebuild of fev asks for no password. It
does only that, writes nothing into your home, and routes only inside the
address plan in your configuration (network.subnet). --docker-sock also
makes /var/run/docker.sock fev's; --replace-docker-sock replaces another
product's link there. "sudo fev setup --admin --undo" removes it all;
--dry-run, with no sudo, prints every path the install writes and every
command it runs.

fev setup --https asks this Mac to trust fev's own root for HTTPS (one
dialog, your own password; this account only); --https --undo removes the
trust. The root's key is in your login keychain, held by fev's key signer
and never exportable; if the key there was made by another key signer or an
older fev, which the keychain will not let this one use, --https replaces
it with a new key and root (the old root's trust removed: a second dialog).`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if adminFlag {
				if https || len(shells) > 0 || noTools || noStart {
					return usagef("setup --admin does only the admin helper; run the rest as yourself, without sudo")
				}
				admin.undo = undo
				return a.admin(admin)
			}
			if admin.dryRun || admin.dockerSock || admin.replaceDockerSock || admin.helper != "" {
				return usagef("setup: --dry-run, --docker-sock, --replace-docker-sock and --helper go with --admin")
			}
			if https {
				return a.httpsTrust(cmd.Context(), undo)
			}
			if os.Geteuid() == 0 {
				return usagef("setup puts fev on your PATH and runs as you, not under sudo; sudo is for setup --admin")
			}
			l, err := a.layout()
			if err != nil {
				return err
			}
			userHome, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			if len(shells) == 0 {
				shells = []string{filepath.Base(os.Getenv("SHELL"))}
				if _, err := profileFor(shells[0], userHome); err != nil {
					return usagef("setup: cannot tell the shell from SHELL=%q; name it with --shell zsh, bash or fish", os.Getenv("SHELL"))
				}
			}
			for _, shell := range shells {
				if _, err := profileFor(shell, userHome); err != nil {
					return err
				}
			}
			say := func(format string, args ...any) {
				if !a.quiet {
					fmt.Fprintf(a.Stdout, format+"\n", args...)
				}
			}
			if removeContext && !undo {
				return usagef("setup: --remove-context goes with --undo")
			}
			if undo {
				if removeContext {
					if err := removeDockerContext(l, say); err != nil {
						return failf(ExitFailed, "setup: the Docker context: %v", err)
					}
				}
				return a.undoSetup(l, say)
			}

			return a.setUp(cmd.Context(), l, userHome, shells, noTools, noStart, cache, say)
		},
	}
	cmd.Flags().BoolVar(&undo, "undo", false, "take back everything setup did")
	cmd.Flags().StringArrayVar(&shells, "shell", nil, "zsh, bash or fish; repeatable (default: the one $SHELL names)")
	cmd.Flags().BoolVar(&noTools, "no-tools", false, "do not download the Docker client tools; only the links and PATH")
	cmd.Flags().BoolVar(&noStart, "no-start", false, "do not start the daemon, which defines the fev Docker context")
	cmd.Flags().BoolVar(&removeContext, "remove-context", false, "with --undo: put back the Docker context selected before fev's and remove fev's (uninstalling; stop fev first)")
	cmd.Flags().MarkHidden("remove-context")
	cmd.Flags().StringVar(&cache, "tools-cache", "", "where downloads are kept (default <home>/cache/tools)")
	cmd.Flags().MarkHidden("tools-cache")
	cmd.Flags().BoolVar(&adminFlag, "admin", false, "install fev's admin helper (under sudo): direct access, names on this Mac, low ports")
	cmd.Flags().BoolVar(&admin.dryRun, "dry-run", false, "with --admin: print what the install writes and runs, changing nothing")
	cmd.Flags().BoolVar(&admin.dockerSock, "docker-sock", false, "with --admin: make /var/run/docker.sock fev's")
	cmd.Flags().BoolVar(&admin.replaceDockerSock, "replace-docker-sock", false, "with --admin: make /var/run/docker.sock fev's, replacing another product's link")
	cmd.Flags().StringVar(&admin.helper, "helper", "", "with --admin: the helper's binary (default: fev-privhelper beside fev)")
	cmd.Flags().MarkHidden("helper")
	cmd.Flags().BoolVar(&https, "https", false, "ask this Mac to trust fev's root for HTTPS (one dialog); with --undo, remove the trust")
	return cmd
}

func (a *app) setUp(ctx context.Context, l home.Layout, userHome string, shells []string, noTools, noStart bool, cache string, say func(string, ...any)) error {
	if !noTools && toolsDir(l) == downloadedTools(l) {
		if cache == "" {
			cache = filepath.Join(l.Home, "cache", "tools")
		}
		in := tools.Installer{Cache: cache, Bin: toolsDir(l), Fetch: curlFetch, Logf: func(format string, args ...any) {
			fmt.Fprintf(a.Stderr, format+"\n", args...)
		}}
		if _, err := in.Install(ctx); err != nil {
			return failf(ExitFailed, "setup: %v", err)
		}
	}
	record := readSetup(l)
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	bin := binDir(l)
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	for _, name := range selfNames {
		if changed, err := link(bin, name, self); err != nil {
			return failf(ExitFailed, "setup: %v", err)
		} else if changed {
			say("linked %s -> %s", filepath.Join(bin, name), self)
		}
	}
	for _, name := range pathTools {
		target := filepath.Join(toolsDir(l), name)
		if changed, err := link(bin, name, target); err != nil {
			return failf(ExitFailed, "setup: %v", err)
		} else if changed {
			say("linked %s -> %s", filepath.Join(bin, name), target)
		}
	}
	if dockerConfig, err := dockerctx.Dir(); err == nil {
		_, statErr := os.Stat(filepath.Join(dockerConfig, "cli-plugins"))
		_, dirErr := os.Stat(dockerConfig)
		left, err := tools.LinkPlugins(dockerConfig, toolsDir(l))
		if err != nil {
			return failf(ExitFailed, "setup: %v", err)
		}
		if record.DockerConfig == "" {
			record.DockerConfig = dockerConfig
			record.PluginDir = errors.Is(statErr, fs.ErrNotExist)
			record.DockerDir = errors.Is(dirErr, fs.ErrNotExist)
		}
		for _, path := range left {
			fmt.Fprintf(a.Stderr, "fev: left alone, it is not ours: %s\n", path)
		}
		if !record.CredsStore && !record.CredsUnnamed {
			if name, err := (dockerctx.Store{Dir: dockerConfig}).CredsStoreName(); err == nil && name == "" {
				record.CredsUnnamed = true
				if err := writeSetup(l, record); err != nil {
					return err
				}
			}
		}
	}
	if used, err := useDockerContext(l); err != nil {
		return failf(ExitFailed, "setup: the Docker context: %v", err)
	} else if used {
		say("the fev Docker context is the current one: docker commands go to fev")
		if !record.ContextUsed {
			record.ContextUsed = true
			if err := writeSetup(l, record); err != nil {
				return err
			}
		}
	}
	if changed, err := a.writeCompletions(l.Home); err != nil {
		return failf(ExitFailed, "setup: completions: %v", err)
	} else if changed {
		say("wrote shell completions for fev, docker and docker-compose in %s", completionsDir(l.Home))
	}
	for _, shell := range shells {
		path, _ := profileFor(shell, userHome)
		edit, changed, err := addBlock(shell, path, block(shell, bin, userHome))
		if err != nil {
			return failf(ExitFailed, "setup: %s: %v", path, err)
		}
		if !changed {
			continue
		}
		known := false
		for _, have := range record.Profiles {
			known = known || have.Path == edit.Path
		}
		if known {
			say("brought fev's block up to date in %s", path)
			continue
		}
		record.Profiles = append(record.Profiles, edit)
		if err := writeSetup(l, record); err != nil {
			return err
		}
		say("added %s to PATH in %s", shellPath(bin, userHome), path)
	}
	sshConfig := filepath.Join(userHome, ".ssh", "config")
	if edit, changed, err := addSSHInclude(sshConfig, sshEntryPath(l), record.SSH != nil); err != nil {
		return failf(ExitFailed, "setup: %s: %v", sshConfig, err)
	} else if changed {
		record.SSH = &edit
		say("included fev's SSH entry at the top of %s (ssh fev)", sshConfig)
	}
	if err := writeSetup(l, record); err != nil {
		return err
	}
	say("done; open a new terminal, or run: export PATH=\"%s:$PATH\"", shellPath(bin, userHome))
	if noStart {
		return nil
	}
	return a.withDaemon(ctx, false, func(c *api.Client) error {
		ds, err := c.DockerStatus(ctx)
		if err != nil {
			return err
		}
		switch {
		case ds.Context.Selected:
			say("docker commands go to fev (the Docker context %q is the current one)", ds.Context.Name)
		case ds.Context.Exists:
			fmt.Fprintf(a.Stderr, "fev: the Docker context %q is selected and stays selected; for fev use \"docker --context %s\" or \"docker context use %s\"\n",
				ds.Context.Current, ds.Context.Name, ds.Context.Name)
		}
		return nil
	})
}

func curlFetch(ctx context.Context, url string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/curl", "-fsSL", "--retry", "3", "--proto", "=https", "--tlsv1.2", url)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &curlBody{out, cmd, &stderr}, nil
}

type curlBody struct {
	io.ReadCloser
	cmd    *exec.Cmd
	stderr *bytes.Buffer
}

func (b *curlBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		if werr := b.cmd.Wait(); werr != nil {
			return n, fmt.Errorf("curl: %s", strings.TrimSpace(b.stderr.String()))
		}
	}
	return n, err
}

func (b *curlBody) Close() error {
	b.ReadCloser.Close()
	b.cmd.Wait()
	return nil
}

func useDockerContext(l home.Layout) (bool, error) {
	dir := os.Getenv("FEV_DOCKER_CONFIG")
	if dir == "" && l.Moved {
		return false, nil
	}
	if dir == "" {
		d, err := dockerctx.Dir()
		if err != nil {
			return false, err
		}
		dir = d
	}
	s := dockerctx.Store{Dir: dir, Socket: filepath.Join(l.RunDir(), "docker.sock"), StateFile: filepath.Join(filepath.Dir(l.ConfigFile()), "docker-context.json")}
	return true, s.Use()
}

func removeDockerContext(l home.Layout, say func(string, ...any)) error {
	dir := os.Getenv("FEV_DOCKER_CONFIG")
	if dir == "" && l.Moved {
		return nil
	}
	if dir == "" {
		d, err := dockerctx.Dir()
		if err != nil {
			return err
		}
		dir = d
	}
	s := dockerctx.Store{Dir: dir, StateFile: filepath.Join(filepath.Dir(l.ConfigFile()), "docker-context.json")}
	st, err := s.Status()
	if err != nil {
		return err
	}
	if !st.Exists {
		return nil
	}
	if err := s.Remove(); err != nil {
		return err
	}
	after, _ := s.Status()
	say("removed the Docker context %q from %s; the current one is %q", st.Name, dir, after.Current)
	return nil
}

func (a *app) undoSetup(l home.Layout, say func(string, ...any)) error {
	record := readSetup(l)
	var failed error
	if record.ContextUsed {
		if err := removeDockerContext(l, say); err != nil {
			fmt.Fprintf(a.Stderr, "fev: the Docker context: %v\n", err)
			failed = err
		}
		os.Remove(filepath.Dir(l.ConfigFile()))
	}
	for _, edit := range record.Profiles {
		if err := removeBlock(edit); err != nil {
			fmt.Fprintf(a.Stderr, "fev: %v\n", err)
			failed = err
			continue
		}
		say("removed fev's block from %s", edit.Path)
	}
	if record.SSH != nil {
		if err := removeSSHInclude(*record.SSH); err != nil {
			fmt.Fprintf(a.Stderr, "fev: %v\n", err)
			failed = err
		} else {
			say("removed fev's Include from %s", record.SSH.Path)
		}
	}
	if record.DockerConfig != "" {
		if record.CredsStore {
			store := dockerctx.Store{Dir: record.DockerConfig}
			unset := func() (bool, error) { return store.UnsetCredsStore(record.CredsFile) }
			if record.CredsNote != nil {
				unset = func() (bool, error) { return store.UnsetCredsStoreNoted(*record.CredsNote) }
			}
			if removed, err := unset(); err != nil {
				fmt.Fprintf(a.Stderr, "fev: %v\n", err)
				failed = err
			} else if removed {
				say("removed credsStore %q from %s", dockerctx.Keychain, filepath.Join(record.DockerConfig, "config.json"))
			}
		}
		if record.CredsUnnamed {
			store := dockerctx.Store{Dir: record.DockerConfig}
			if removed, err := store.UnsetCredsStore(false); err != nil {
				fmt.Fprintf(a.Stderr, "fev: %v\n", err)
				failed = err
			} else if removed {
				say("removed credsStore %q from %s: a docker login wrote it while fev's helper was on PATH, and the helper goes with fev (the passwords it kept stay in the keychain)", dockerctx.Keychain, filepath.Join(record.DockerConfig, "config.json"))
			}
		}
		if err := tools.UnlinkPlugins(record.DockerConfig, toolsDir(l)); err != nil {
			failed = err
		}
		if record.PluginDir {
			os.Remove(filepath.Join(record.DockerConfig, "cli-plugins"))
		}
		if record.DockerDir {
			os.Remove(record.DockerConfig)
		}
	}
	bin := binDir(l)
	for _, name := range append(append([]string{}, selfNames...), pathTools...) {
		path := filepath.Join(bin, name)
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			if os.Remove(path) == nil {
				say("removed %s", path)
			}
		}
	}
	os.Remove(bin)
	if _, err := os.Stat(completionsDir(l.Home)); err == nil && os.RemoveAll(completionsDir(l.Home)) == nil {
		say("removed %s", completionsDir(l.Home))
	}
	if failed != nil {
		return failf(ExitFailed, "setup --undo: not everything could be taken back; setup.json is kept")
	}
	os.Remove(setupState(l))
	a.offerTools(l, say)
	os.Remove(l.Home)
	return nil
}

func (a *app) offerTools(l home.Layout, say func(string, ...any)) {
	if os.Getenv(ToolsEnv) != "" || a.uninstalling {
		return
	}
	cache := filepath.Join(l.Home, "cache")
	var dirs []string
	var size int64
	for _, dir := range []string{downloadedTools(l), filepath.Join(cache, "tools")} {
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			continue
		}
		dirs = append(dirs, dir)
		filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if info, err := d.Info(); err == nil {
					size += info.Size()
				}
			}
			return nil
		})
	}
	if len(dirs) == 0 {
		return
	}
	where := strings.Join(dirs, " and ")
	if !isTerminal(a.Stdin) {
		say("kept %s (%s): the Docker client tools setup downloaded, which are asked about only at a terminal", where, bytesText(size))
		return
	}
	fmt.Fprintf(a.Stderr, "The Docker client tools that setup downloaded are still in %s (%s). Remove them too? [y/N] ", where, bytesText(size))
	line, _ := bufio.NewReader(a.Stdin).ReadString('\n')
	if !strings.HasSuffix(line, "\n") {
		fmt.Fprintln(a.Stderr)
	}
	if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
		say("kept %s", where)
		return
	}
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintf(a.Stderr, "fev: %v\n", err)
			continue
		}
		say("removed %s", dir)
	}
	os.Remove(cache)
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	return err == nil
}
