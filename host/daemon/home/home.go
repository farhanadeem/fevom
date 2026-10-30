package home

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"

	api "fev/proto/daemon"
)

const MaxSocketPath = 103

const Env = "FEV_HOME"

type Layout struct {
	Home           string
	DataDir        string
	DefaultDataDir string
	Placed         bool
	Moved          bool
}

func Resolve(homeFlag, dataDirFlag string) (Layout, error) {
	var l Layout
	moved := true
	switch {
	case homeFlag != "":
		l.Home = homeFlag
	case os.Getenv(Env) != "":
		l.Home = os.Getenv(Env)
	default:
		user, err := os.UserHomeDir()
		if err != nil {
			return l, fmt.Errorf("cannot find the home directory: %w", err)
		}
		l.Home = filepath.Join(user, ".fev")
		l.DataDir = filepath.Join(user, "Library", "Application Support", "dev.fevom", "data")
		moved = false
	}
	abs, err := filepath.Abs(l.Home)
	if err != nil {
		return l, err
	}
	l.Home = abs
	if moved {
		for _, user := range ownHomes() {
			if abs == filepath.Join(user, ".fev") {
				l.DataDir = filepath.Join(user, "Library", "Application Support", "dev.fevom", "data")
				moved = false
				break
			}
		}
	}
	l.Moved = moved
	if moved {
		l.DataDir = filepath.Join(l.Home, "data")
	}
	l.DefaultDataDir = l.DataDir
	if dataDirFlag != "" {
		if l.DataDir, err = filepath.Abs(dataDirFlag); err != nil {
			return l, err
		}
		return l, nil
	}
	placed, err := l.ReadPlacement()
	if err != nil {
		return l, err
	}
	if placed != "" {
		l.DataDir, l.Placed = placed, true
	}
	return l, nil
}

func (l Layout) PlacementFile() string { return filepath.Join(l.Home, "config", "data.json") }

type placement struct {
	Dir string `json:"dir"`
}

func (l Layout) ReadPlacement() (string, error) {
	data, err := os.ReadFile(l.PlacementFile())
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var p placement
	if err := json.Unmarshal(data, &p); err != nil {
		return "", fmt.Errorf("%s: %w", l.PlacementFile(), err)
	}
	if p.Dir != "" && !filepath.IsAbs(p.Dir) {
		return "", fmt.Errorf("%s: the data directory %q is not an absolute path", l.PlacementFile(), p.Dir)
	}
	return filepath.Clean(p.Dir), nil
}

func (l Layout) WritePlacement(dir string) error {
	if filepath.Clean(dir) == filepath.Clean(l.DefaultDataDir) {
		err := os.Remove(l.PlacementFile())
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	data, err := json.Marshal(placement{Dir: dir})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.PlacementFile()), 0o755); err != nil {
		return err
	}
	tmp := l.PlacementFile() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.PlacementFile())
}

func (l Layout) DataImageStamp() string {
	return filepath.Join(l.Home, "config", "data-image.made")
}

func (l Layout) RunDir() string { return filepath.Join(l.Home, "run") }

func (l Layout) VMDir() string      { return filepath.Join(l.RunDir(), "vm") }
func (l Layout) Socket() string     { return api.SocketPath(l.Home) }
func (l Layout) StartLock() string  { return filepath.Join(l.RunDir(), "start.lock") }
func (l Layout) DaemonLock() string { return filepath.Join(l.RunDir(), "fevd.lock") }
func (l Layout) InfoFile() string   { return filepath.Join(l.RunDir(), "fevd.json") }
func (l Layout) ConfigFile() string { return filepath.Join(l.Home, "config", "config.json") }
func (l Layout) LogDir() string     { return filepath.Join(l.Home, "log") }

func (l Layout) MachinesFile() string { return filepath.Join(l.Home, "config", "machines.json") }

func (l Layout) ImagesDir() string { return filepath.Join(l.RunDir(), "images") }

func (l Layout) HTTPSDir() string { return filepath.Join(l.Home, "https") }

func (l Layout) NamesFile() string { return filepath.Join(l.Home, "config", "names.json") }
func (l Layout) BootTraceLog() string {
	return filepath.Join(l.LogDir(), "boot-trace.jsonl")
}
func (l Layout) DataImage() string { return filepath.Join(l.DataDir, "data.img") }

const longestSocket = "vsock_4294967295.sock"

func (l Layout) Validate() error {
	longest := filepath.Join(l.VMDir(), longestSocket)
	if len(longest) > MaxSocketPath {
		return fmt.Errorf("home %s is too long for unix sockets: %s is %d bytes and macOS allows %d; use a shorter --home, such as /tmp/bh.XXXXXX",
			l.Home, longest, len(longest), MaxSocketPath)
	}
	return nil
}

func (l Layout) Create() error {
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{
		{l.Home, 0o755},
		{l.RunDir(), 0o700},
		{l.VMDir(), 0o700},
		{filepath.Dir(l.ConfigFile()), 0o755},
		{l.LogDir(), 0o700},
		{l.ImagesDir(), 0o700},
	} {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return err
		}
		if d.mode == 0o700 {
			if err := os.Chmod(d.path, d.mode); err != nil {
				return err
			}
		}
	}
	return nil
}

type Info struct {
	PID        int    `json:"pid"`
	Build      string `json:"build"`
	APIVersion int    `json:"api_version"`
	VMMPID     int    `json:"vmm_pid"`
	Started    string `json:"started"`
}

func (l Layout) WriteInfo(info Info) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	tmp := l.InfoFile() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.InfoFile())
}

func (l Layout) ReadInfo() (Info, error) {
	var info Info
	data, err := os.ReadFile(l.InfoFile())
	if err != nil {
		return info, err
	}
	return info, json.Unmarshal(data, &info)
}

func accountHome() string {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return ""
	}
	return filepath.Clean(u.HomeDir)
}

func ownHomes() []string {
	var dirs []string
	if h, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Clean(h))
	}
	if a := accountHome(); a != "" && (len(dirs) == 0 || dirs[0] != a) {
		dirs = append(dirs, a)
	}
	return dirs
}
