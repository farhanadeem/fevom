package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fev/host/docker/proxy"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"fev/host/daemon"
	"fev/proto/agent"
	api "fev/proto/daemon"
)

const ConfigName = "docker.json"

const maxConfig = 1 << 20

var configureTimeout = 30 * time.Second

var guestCallTimeout = time.Minute

func CapBounds(d time.Duration) {
	configureTimeout = min(configureTimeout, d)
	guestCallTimeout = min(guestCallTimeout, d)
	proxy.WaitedAfter = min(proxy.WaitedAfter, d)
}

func (m *Module) ConfigFile() string {
	return filepath.Join(filepath.Dir(m.host.Layout().ConfigFile()), ConfigName)
}

func readConfig(path string) (config json.RawMessage, present bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return json.RawMessage("{}"), false, nil
	}
	if err != nil {
		return nil, true, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, true, err
	}
	if st.Size() > maxConfig {
		return nil, true, fmt.Errorf("%s is %d bytes; an engine configuration is at most %d", ConfigName, st.Size(), maxConfig)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfig+1))
	if err != nil {
		return nil, true, err
	}
	if len(data) > maxConfig {
		return nil, true, fmt.Errorf("%s is more than %d bytes", ConfigName, maxConfig)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return json.RawMessage("{}"), true, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, true, fmt.Errorf("%s is not valid JSON: %v", ConfigName, err)
	}
	if object == nil {
		return nil, true, fmt.Errorf("%s must hold a JSON object", ConfigName)
	}
	return data, len(object) > 0, nil
}

func (m *Module) configure(ctx context.Context, vm daemon.VM) (restart bool) {
	state, problem := api.EngineConfigNone, ""
	defer func() {
		m.mu.Lock()
		changed := m.configState != state || m.configError != problem
		m.configState, m.configError = state, problem
		m.mu.Unlock()
		if changed && problem != "" {
			m.log.Warn("docker: "+ConfigName+" was not applied; the engine keeps the last configuration that passed", "reason", problem)
		}
		text := ""
		if state == api.EngineConfigRefused {
			text = ConfigName + " was not applied (" + clipText(problem, 300) + "); the engine keeps the last configuration that passed. Mend " + m.ConfigFile() + " and run \"fev restart docker\""
		}
		m.host.Warn(WarnConfig, text)
	}()
	config, present, err := readConfig(m.ConfigFile())
	if err != nil {
		state, problem = api.EngineConfigRefused, err.Error()
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, configureTimeout)
	defer cancel()
	result, err := m.configureCall(ctx, vm, config)
	if err != nil {
		if present {
			state, problem = api.EngineConfigRefused, "the guest could not check it: "+err.Error()
		}
		return false
	}
	switch result.State {
	case agent.EngineConfigRefused:
		state, problem = api.EngineConfigRefused, clip(result.Error, 2000)
		if problem == "" {
			problem = "the engine refused it"
		}
		return false
	case agent.EngineConfigAccepted, agent.EngineConfigUnchanged:
		if present {
			state = api.EngineConfigApplied
		}
		return result.RestartNeeded
	}
	state, problem = api.EngineConfigRefused, "the guest answered engine.configure with state "+clip(result.State, 40)
	return false
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
