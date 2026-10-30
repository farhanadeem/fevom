package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"

	"fev/guest/internal/engineconf"
	"fev/guest/internal/logbuf"
	"fev/guest/internal/reaper"
)

const (
	engineConfigKept = "/data/@system/docker.json"
	engineConfigGood = "/data/@system/docker.good.json"
	engineConfigName = "daemon.json"
	engineValidate   = "/run/fev/engine/validate"
	validateWait     = 20 * time.Second
)

func (e *engine) keptConfig() map[string]json.RawMessage { return e.readConfig(engineConfigKept) }

func (e *engine) readConfig(path string) map[string]json.RawMessage {
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			e.sys.log.Logf(logbuf.Warning, "engine: cannot read the kept configuration: %v", err)
		}
		return nil
	}
	var user map[string]json.RawMessage
	if err := json.Unmarshal(data, &user); err != nil {
		e.sys.log.Logf(logbuf.Warning, "engine: the kept configuration is not valid and is ignored: %v", err)
		return nil
	}
	if err := engineconf.Check(user); err != nil {
		e.sys.log.Logf(logbuf.Warning, "engine: the kept configuration is ignored: %v", err)
		return nil
	}
	return user
}

func (e *engine) writeConfig() (string, error) {
	user := e.keptConfig()
	e.mu.Lock()
	bad := e.badConfig
	e.mu.Unlock()
	if bad != "" && engineconf.Canonical(user) == bad {
		user = e.readConfig(engineConfigGood)
		e.sys.log.Logf(logbuf.Warning, "engine: starting on the last configuration it was ready with, not the one kept")
	}
	data, err := engineconf.Merge(e.sys.engineBase(), user)
	if err != nil {
		return "", err
	}
	return engineconf.Canonical(user), os.WriteFile(filepath.Join(engineRun, engineConfigName), data, 0o644)
}

func (e *engine) ended(spawnedWith string, ready bool) {
	good := engineconf.Canonical(e.readConfig(engineConfigGood))
	switch {
	case ready && spawnedWith != good:
		if err := keepConfig(engineConfigGood, spawnedWith); err != nil {
			e.sys.log.Logf(logbuf.Warning, "engine: cannot note the configuration it is ready with: %v", err)
		}
	case !ready && spawnedWith != good:
		e.mu.Lock()
		e.badConfig, e.badWhy = spawnedWith, e.lastFailure
		why := e.badWhy
		e.mu.Unlock()
		e.sys.log.Logf(logbuf.Error, "engine: dockerd did not start with the configuration it was given (%s); the next start uses the last one it was ready with", why)
	}
}

func (e *engine) configure(user map[string]json.RawMessage) (api.EngineConfigured, error) {
	e.cfgMu.Lock()
	defer e.cfgMu.Unlock()
	if _, err := e.sys.volume(); err != nil {
		return api.EngineConfigured{}, err
	}
	want := engineconf.Canonical(user)
	e.mu.Lock()
	bad, why := e.badConfig, e.badWhy
	e.mu.Unlock()
	if bad != "" && want == bad {
		if why == "" {
			e.mu.Lock()
			why = e.lastFailure
			e.mu.Unlock()
		}
		if why == "" {
			why = "dockerd exited before it was ready"
		}
		return api.EngineConfigured{State: api.EngineConfigRefused, Error: "the engine did not start with it: " + why}, nil
	}
	restart := func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		switch e.sup.Status().State {
		case api.EngineStarting, api.EngineReady:
			return e.spawnedWith != want
		}
		return false
	}
	if want == engineconf.Canonical(e.keptConfig()) {
		return api.EngineConfigured{State: api.EngineConfigUnchanged, RestartNeeded: restart()}, nil
	}
	merged, err := engineconf.Merge(e.sys.engineBase(), user)
	var refused *engineconf.Refused
	switch {
	case errors.As(err, &refused):
		return api.EngineConfigured{State: api.EngineConfigRefused, Error: refused.Message}, nil
	case err != nil:
		return api.EngineConfigured{}, err
	}
	message, err := e.validate(merged)
	if err != nil {
		return api.EngineConfigured{}, fmt.Errorf("the configuration could not be checked: %w", err)
	}
	if message != "" {
		return api.EngineConfigured{State: api.EngineConfigRefused, Error: message}, nil
	}
	if err := keepConfig(engineConfigKept, want); err != nil {
		return api.EngineConfigured{}, fmt.Errorf("the configuration could not be kept: %w", err)
	}
	return api.EngineConfigured{State: api.EngineConfigAccepted, RestartNeeded: restart()}, nil
}

func keepConfig(path, canonical string) error {
	if canonical == "{}" {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(canonical + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func (e *engine) validate(config []byte) (string, error) {
	root, run := engineValidate+"/root", engineValidate+"/run"
	if err := os.RemoveAll(engineValidate); err != nil {
		return "", err
	}
	defer os.RemoveAll(engineValidate)
	for _, dir := range []string{root, run} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(run, engineConfigName), config, 0o644); err != nil {
		return "", err
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		return "", err
	}
	defer null.Close()
	pr, pw, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer pr.Close()

	type started struct {
		child *reaper.Child
		err   error
	}
	spawned := make(chan started, 1)
	go func() {
		runtime.LockOSThread()
		fail := func(what string, err error) { spawned <- started{err: fmt.Errorf("%s: %w", what, err)} }
		if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
			fail("unshare mount namespace", err)
			return
		}
		if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
			fail("make / private", err)
			return
		}
		if err := unix.Mount(engineImage, root, "", unix.MS_BIND, ""); err != nil {
			fail("bind the engine's image", err)
			return
		}
		if err := unix.Mount(run, root+"/run", "", unix.MS_BIND, ""); err != nil {
			fail("bind the candidate", err)
			return
		}
		child, err := e.sys.reaper.Start(engineArgs[0], []string{engineArgs[0], "--validate", "--config-file", "/run/" + engineConfigName}, &os.ProcAttr{
			Dir:   "/",
			Env:   e.sys.engineEnvironment(),
			Files: []*os.File{null, pw, pw},
			Sys:   &syscall.SysProcAttr{Chroot: root, Setsid: true},
		})
		if err != nil {
			fail("start dockerd --validate", err)
			return
		}
		spawned <- started{child: child}
	}()
	s := <-spawned
	pw.Close()
	if s.err != nil {
		return "", s.err
	}
	timer := time.AfterFunc(validateWait, func() { e.sys.reaper.Signal(s.child, unix.SIGKILL, false) })
	var out bytes.Buffer
	out.ReadFrom(pr)
	status := s.child.Wait()
	timer.Stop()
	text := strings.TrimSpace(out.String())
	switch {
	case status.Signaled():
		return "", fmt.Errorf("dockerd --validate was killed by signal %d: %s", status.Signal(), clipText(text, 400))
	case status.ExitStatus() == 0:
		return "", nil
	}
	if _, after, ok := strings.Cut(text, engineConfigName+": "); ok {
		text = after
	}
	if text == "" {
		text = fmt.Sprintf("dockerd --validate exited with status %d", status.ExitStatus())
	}
	return clipText(text, 2000), nil
}

func clipText(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
