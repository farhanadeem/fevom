package terminfo

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"fev/proto/agent"
)

const System = "/usr/share/terminfo"

type Env struct {
	Terminfo, TerminfoDirs, Home string
}

func (env Env) Dirs() []string {
	var dirs []string
	seen := map[string]bool{}
	add := func(d string) {
		if !filepath.IsAbs(d) {
			return
		}
		d = filepath.Clean(d)
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	add(env.Terminfo)
	if env.Home != "" {
		add(filepath.Join(env.Home, ".terminfo"))
	}
	if env.TerminfoDirs != "" {
		for _, d := range strings.Split(env.TerminfoDirs, ":") {
			if d == "" {
				d = System
			}
			add(d)
		}
	}
	add(System)
	return dirs
}

func Find(name string, dirs []string) []byte {
	if name == "dumb" || agent.CheckTerminfoName(name) != nil {
		return nil
	}
	for _, d := range dirs {
		for _, sub := range []string{name[:1], fmt.Sprintf("%02x", name[0])} {
			if b := read(filepath.Join(d, sub, name)); b != nil && agent.CheckTerminfo(name, b) == nil {
				return b
			}
		}
	}
	return nil
}

func read(path string) []byte {
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > agent.MaxTerminfo {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(f, agent.MaxTerminfo+1))
	if err != nil || len(b) > agent.MaxTerminfo {
		return nil
	}
	return b
}

func FromEnv(getenv func(string) string) []byte {
	home, _ := os.UserHomeDir()
	env := Env{Terminfo: getenv("TERMINFO"), TerminfoDirs: getenv("TERMINFO_DIRS"), Home: home}
	return Find(getenv("TERM"), env.Dirs())
}
