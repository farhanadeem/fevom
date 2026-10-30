package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/internal/binfmt"
	policy "fev/guest/internal/emulation"
	"fev/guest/internal/logbuf"
	"fev/guest/internal/netcfg"
	"fev/guest/internal/reaper"
	"fev/guest/internal/stage"
)

type emulation struct {
	log       *logbuf.Buffer
	path, dir string
	probeDir  string
	reaper    *reaper.Reaper
	engine    string
	profile   policy.Config
	probed    bool
	specs     []binfmt.Spec

	mu          sync.Mutex
	want        bool
	keeper      *binfmt.Keeper
	tableErr    error
	cacheName   int
	present     bool
	version     string
	versionRead bool
}

func newEmulation(log *logbuf.Buffer, cmdline string) *emulation {
	engine := netcfg.Arg(cmdline, api.EmulationEngineParam)
	if engine == "" {
		engine = api.EmulationEngineFEX
	}
	e := &emulation{log: log, path: api.EmulationPath, dir: binfmt.Dir,
		probeDir: filepath.Join(cgroupRoot, "fev.system", "emulation-probe"), engine: engine, want: netcfg.Arg(cmdline, api.EmulationCmdlineParam) != "0"}
	e.profile = policy.Config{Mode: engine, Reason: "Rosetta is not attached in fex mode"}
	e.specs = binfmt.Fevom(e.path)
	if engine == api.EmulationEngineRosetta || engine == api.EmulationEngineAuto {
		e.path = api.EmulationRosettaPath
		e.specs = binfmt.Mixed()
	}
	return e
}

func (e *emulation) start() error {
	if err := e.prepareTranslator(); err != nil {
		e.mu.Lock()
		e.tableErr = err
		want := e.want
		e.mu.Unlock()
		if !want {
			return stage.Skip("x86 emulation is off (%s=0); %v", api.EmulationCmdlineParam, err)
		}
		return err
	}

	if err := binfmt.Mount(e.dir); err != nil {
		e.mu.Lock()
		e.tableErr = err
		e.mu.Unlock()
		return err
	}
	_, statErr := os.Stat(e.path)
	k := &binfmt.Keeper{Table: binfmt.Table{Dir: e.dir}, Dir: e.dir, Specs: e.specs,
		Logf: func(format string, args ...any) { e.log.Logf(logbuf.Warning, format, args...) }}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.keeper, e.present = k, statErr == nil
	err := k.Start(e.want && e.present)
	if err == nil && e.want && e.present {
		e.measureLocked()
	}
	switch {
	case statErr != nil:
		return stage.Skip("the system image has no x86 translator at %s", e.path)
	case !e.want:
		return stage.Skip("x86 emulation is off (%s=0)", api.EmulationCmdlineParam)
	}
	return err
}

func (e *emulation) prepareTranslator() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch e.engine {
	case api.EmulationEngineFEX:
		return nil
	case api.EmulationEngineRosetta, api.EmulationEngineAuto:
		if err := os.MkdirAll(api.EmulationRosettaMount, 0755); err != nil {
			return err
		}
		if err := unix.Mount("rosetta", api.EmulationRosettaMount, "virtiofs", unix.MS_RDONLY|unix.MS_NODEV, ""); err != nil {
			if e.engine == api.EmulationEngineAuto {
				e.path, e.specs = api.EmulationPath, binfmt.Fevom(api.EmulationPath)
				e.profile.Reason = "Rosetta's dedicated share is unavailable; using FEX"
				e.log.Logf(logbuf.Warning, "emulation: %s: %v", e.profile.Reason, err)
				return nil
			}
			return fmt.Errorf("mount Rosetta's read-only share: %w", err)
		}
		if _, err := os.Stat(api.EmulationPath); err != nil {
			return fmt.Errorf("Rosetta mode still needs FEX for i386 at %s: %w", api.EmulationPath, err)
		}
		e.profile.Rosetta, e.profile.Reason = true, ""
		if fd, err := holdCacheName(api.EmulationRosettaCache); err != nil {
			e.log.Logf(logbuf.Warning, "emulation: Rosetta's cache name is not held: %v", err)
		} else {
			e.cacheName = fd
		}
		return nil
	default:
		return fmt.Errorf("unknown emulation engine %q", e.engine)
	}
}

func (e *emulation) set(x86 bool) (api.EmulationStatus, error) {
	e.mu.Lock()
	e.want = x86
	k, present := e.keeper, e.present
	e.mu.Unlock()
	if k != nil {
		if err := k.Set(x86 && present); err != nil && !x86 {
			return api.EmulationStatus{}, err
		}
	}
	if x86 && present {
		e.mu.Lock()
		e.measureLocked()
		e.mu.Unlock()
	}
	return e.status(), nil
}

func (e *emulation) measureLocked() {
	if e.probed || !e.profile.Rosetta {
		return
	}
	e.probed = true
	code := -1
	group, err := probeCgroup(e.probeDir)
	if err == nil {
		defer group.Close()
		defer os.Remove(e.probeDir)
		code, err = e.runProbe(group)
	}
	if code >= 64 && code <= 67 {
		e.profile.Faccessat2, e.profile.Seccomp = code&1 == 0, code&2 == 0
		e.profile.Reason = ""
		if code != 64 {
			e.profile.Reason = "installed Rosetta lacks "
			if code&1 != 0 {
				e.profile.Reason += "faccessat2/AT_EMPTY_PATH"
			}
			if code&3 == 3 {
				e.profile.Reason += " and "
			}
			if code&2 != 0 {
				e.profile.Reason += "x86 seccomp"
			}
		}
	} else {
		e.profile.Reason = fmt.Sprintf("Rosetta capability probe failed (exit %d): %v", code, err)
		if e.profile.Mode != policy.Rosetta {
			e.profile.Rosetta = false
		}
	}
	e.log.Logf(logbuf.Info, "emulation: Rosetta probe: available=%v faccessat2=%v seccomp=%v %s", e.profile.Rosetta, e.profile.Faccessat2, e.profile.Seccomp, e.profile.Reason)
}

func (e *emulation) runProbe(group *os.File) (int, error) {
	if e.reaper == nil {
		return -1, errors.New("no reaper")
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		return -1, err
	}
	defer null.Close()
	child, err := e.reaper.Start(policy.ProbePath, []string{policy.ProbePath}, &os.ProcAttr{
		Files: []*os.File{null, null, null},
		Sys:   &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(group.Fd())},
	})
	if err != nil {
		return -1, err
	}
	timer := time.AfterFunc(2*time.Second, func() { e.reaper.Signal(child, unix.SIGKILL, false) })
	st := child.Wait()
	timer.Stop()
	if st.Signaled() {
		return -1, fmt.Errorf("killed by signal %d", st.Signal())
	}
	return st.ExitStatus(), nil
}

func probeCgroup(dir string) (*os.File, error) {
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := policy.SetCgroup(dir, policy.Rosetta); err != nil {
		os.Remove(dir)
		return nil, err
	}
	f, err := os.Open(dir)
	if err != nil {
		os.Remove(dir)
	}
	return f, err
}

func (e *emulation) config() policy.Config { e.mu.Lock(); defer e.mu.Unlock(); return e.profile }

func (e *emulation) writeConfig(run, cgroup string) error {
	cfg := e.config()
	if cfg.Mode != policy.FEX {
		engine := policy.FEX
		if cfg.Rosetta {
			engine = policy.Rosetta
		}
		if err := policy.SetCgroup(cgroup, engine); err != nil {
			return err
		}
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if cfg.Mode != policy.FEX {
		if err := os.WriteFile(filepath.Join(run, "fev", "emulation.runtime"), []byte(cfg.Mode), 0644); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(run, "fev", "emulation.json"), b, 0644)
}

func containerEmulationChoices() []api.EmulationWorkload {
	var choices []api.EmulationWorkload
	for _, pattern := range []string{
		filepath.Join(engineRun, "docker/containerd/daemon/io.containerd.runtime.v2.task", "*", "*", policy.RecordName),
		filepath.Join(engineData, "buildkit/executor", "*", policy.RecordName),
	} {
		files, _ := filepath.Glob(pattern)
		for _, file := range files {
			b, err := os.ReadFile(file)
			if err != nil || len(b) > 8192 {
				continue
			}
			var choice api.EmulationWorkload
			if json.Unmarshal(b, &choice) == nil {
				choices = append(choices, choice)
			}
		}
	}
	return choices
}

func (e *emulation) status() api.EmulationStatus {
	e.mu.Lock()
	k, want, present, tableErr := e.keeper, e.want, e.present, e.tableErr
	e.mu.Unlock()
	st := api.EmulationStatus{X86: want, MemoryModel: binfmt.MemoryModel(), Entries: []api.EmulationEntry{}}
	cfg := e.config()
	if cfg.Mode == policy.Auto {
		st.Mode, st.Reason = cfg.Mode, cfg.Reason
	}
	if cfg.Mode == policy.Rosetta {
		st.Reason = cfg.Reason
	}
	st.Workloads = containerEmulationChoices()
	if present {
		st.Translator, st.Path = api.EmulationTranslator, e.path
		if e.path == api.EmulationRosettaPath {
			st.Translator = api.EmulationRosetta
		} else {
			st.Version = e.readVersion()
		}
	}
	var err error
	switch {
	case tableErr != nil:
		err = tableErr
	case k == nil:
		err = errors.New("the x86 translator's stage has not run")
	default:
		var restored int
		st.TableEnabled, st.Entries, st.Registered, restored, err = k.State()
		st.Restored = restored
	}
	if want && (!st.Registered || !st.TableEnabled) {
		switch {
		case tableErr != nil:
			st.Error = tableErr.Error()
		case !present && k != nil:
			st.Error = fmt.Sprintf("the system image has no x86 translator at %s", e.path)
		case err != nil:
			st.Error = err.Error()
		default:
			st.Error = binfmt.WhyNot(st.TableEnabled, st.Entries, e.specs)
		}
	} else if want {
		st.Error = binfmt.WhyNot(st.TableEnabled, st.Entries, e.specs)
	}
	return st
}

func (e *emulation) readVersion() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.versionRead {
		return e.version
	}
	e.versionRead = true
	if data, err := os.ReadFile(e.path + ".version"); err == nil {
		e.version = binfmt.Version(data)
		return e.version
	}
	f, err := os.Open(e.path)
	if err != nil {
		return ""
	}
	defer f.Close()
	e.version = unameVersion(f)
	return e.version
}

func unameVersion(r io.Reader) string {
	const keep = 64
	buf := make([]byte, 0, 1<<16+keep)
	chunk := make([]byte, 1<<16)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for at := 0; ; {
			i := bytes.Index(buf[at:], []byte("#FEX-"))
			if i < 0 {
				break
			}
			word := buf[at+i+1:]
			end := bytes.IndexAny(word, " \x00\n")
			if end < 0 || end > keep-8 {
				if len(word) < keep {
					break
				}
				at += i + 1
				continue
			}
			if bytes.HasPrefix(word[end:], []byte(" SMP")) {
				return string(word[:end])
			}
			at += i + 1
		}
		if err != nil {
			return ""
		}
		if len(buf) > keep {
			buf = append(buf[:0], buf[len(buf)-keep:]...)
		}
	}
}

func (s *system) EmulationStatus() api.EmulationStatus { return s.emu.status() }

func (s *system) EmulationSet(x86 bool) (api.EmulationStatus, error) { return s.emu.set(x86) }

func holdCacheName(name string) (int, error) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: "@" + name}); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("bind @%s: %w", name, err)
	}
	return fd, nil
}
