package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"fev/proto/agent"
	api "fev/proto/daemon"
)

const MachinePrefix = "machine."

const KeyUsername = "username"

func MachineKeys(macUser string) []api.ConfigKey {
	var noPort, maxPort int64 = 0, 65535
	return []api.ConfigKey{
		{Key: api.MachineKeyEmulation, Type: api.TypeString, Default: "auto", Apply: api.ApplyMachineRestart,
			Description: "The machine's x86-64 translator: auto follows emulation.engine; fex or rosetta forces one"},
		{Key: KeyUsername, Type: api.TypeString, Default: macUser, Apply: api.ApplyLive,
			Description: "The machine's default user, used by fev run, fev and ssh when no user is given"},
		{Key: api.MachineKeyIsolated, Type: api.TypeBool, Default: false, Apply: api.ApplyMachineRestart,
			Description: "Isolate the machine: its own user namespace, none of your Mac's files other than its mounts, and no mac command"},
		{Key: api.MachineKeyIsolateNetwork, Type: api.TypeBool, Default: false, Apply: api.ApplyMachineRestart,
			Description: "Limit an isolated machine to the internet: no other machines, containers, your Mac or its published ports"},
		{Key: api.MachineKeyMounts, Type: api.TypeList, Default: []string{}, Apply: api.ApplyMachineRestart,
			Description: "An isolated machine's Mac folders, each as SOURCE or SOURCE:DESTINATION. The destination defaults to the same path"},
		{Key: api.MachineKeyForwardSSHAgent, Type: api.TypeBool, Default: false, Apply: api.ApplyMachineRestart,
			Description: "Give an isolated machine your Mac's SSH agent"},
		{Key: api.MachineKeyHTTPPort, Type: api.TypeInt, Default: int64(0), Apply: api.ApplyMachineRestart, Min: &noPort, Max: &maxPort,
			Description: "The port NAME.fev.local serves over HTTP and HTTPS, if the machine serves plain HTTP. 0 detects it"},
		{Key: api.MachineKeyHTTPSPort, Type: api.TypeInt, Default: int64(0), Apply: api.ApplyMachineRestart, Min: &noPort, Max: &maxPort,
			Description: "The port https://NAME.fev.local uses if the machine serves TLS itself. 0 detects it"},
	}
}

func isScoped(key string) bool {
	_, short, ok := scopeOf(key, MachinePrefix)
	if !ok {
		return false
	}
	for _, t := range MachineKeys("") {
		if t.Key == short {
			return true
		}
	}
	return false
}

func CheckUsername(name string) error {
	if name == "" || len(name) > 32 {
		return fmt.Errorf("a user name is 1 to 32 characters: %q", name)
	}
	for i, c := range []byte(name) {
		switch {
		case c >= 'a' && c <= 'z', c == '_':
		case (c >= '0' && c <= '9') || c == '-':
			if i == 0 {
				return fmt.Errorf("a user name begins with a lower-case letter or an underscore: %q", name)
			}
		default:
			return fmt.Errorf("a user name is lower-case letters, digits, underscores and hyphens: %q", name)
		}
	}
	if name == "root" {
		return fmt.Errorf("root is not a machine's default user; name it in a command (fev -u root)")
	}
	return nil
}

func checkScoped(key string, v any) error {
	if strings.HasPrefix(key, MachinePrefix) && strings.HasSuffix(key, "."+api.MachineKeyEmulation) {
		if s, ok := v.(string); ok && (s == "auto" || s == "fex" || s == "rosetta") {
			return nil
		}
		return fmt.Errorf("emulation must be auto, fex or rosetta")
	}
	if strings.HasPrefix(key, MachinePrefix) && strings.HasSuffix(key, "."+KeyUsername) {
		if s, ok := v.(string); ok {
			return CheckUsername(s)
		}
	}
	if strings.HasPrefix(key, MachinePrefix) && strings.HasSuffix(key, "."+api.MachineKeyMounts) {
		list, _ := v.([]string)
		if len(list) > agent.MaxMachineMounts {
			return fmt.Errorf("at most %d mounts", agent.MaxMachineMounts)
		}
		var mounts []agent.MachineMount
		for _, m := range list {
			mm, err := agent.ParseMachineMount(m)
			if err != nil {
				return err
			}
			mounts = append(mounts, mm)
		}
		return agent.CheckIsolation(agent.MachineConfig{Isolated: true, Share: true, Mounts: mounts})
	}
	return nil
}

func scopedKey(prefix, name, key string) string { return prefix + name + "." + key }

func scopeOf(key, prefix string) (name, short string, ok bool) {
	rest, ok := strings.CutPrefix(key, prefix)
	if !ok {
		return "", "", false
	}
	i := strings.LastIndexByte(rest, '.')
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

func (s *Store) AddScope(prefix, name string, templates []api.ConfigKey, values map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := map[string]any{}
	for k, v := range s.set {
		next[k] = v
	}
	var added []api.ConfigKey
	for _, t := range templates {
		key := t
		key.Key = scopedKey(prefix, name, t.Key)
		if _, ok := s.byID[key.Key]; ok {
			return fmt.Errorf("%s exists", key.Key)
		}
		var v any
		if given, ok := values[t.Key]; ok {
			v = given
		} else if raw, ok := s.unknown[key.Key]; ok {
			json.Unmarshal(raw, &v)
		}
		if v != nil {
			checked, err := validate(key, v)
			if err == nil {
				err = checkScoped(key.Key, checked)
			}
			if err != nil {
				return &Error{Key: key.Key, Reason: err.Error()}
			}
			if !reflect.DeepEqual(checked, key.Default) {
				next[key.Key] = checked
			} else {
				delete(next, key.Key)
			}
		}
		added = append(added, key)
	}
	unknown := map[string]json.RawMessage{}
	for k, raw := range s.unknown {
		unknown[k] = raw
	}
	for _, key := range added {
		delete(unknown, key.Key)
	}
	saved := s.unknown
	s.unknown = unknown
	if err := s.save(next); err != nil {
		s.unknown = saved
		return err
	}
	s.set = next
	for _, key := range added {
		s.keys = append(s.keys, key)
		s.byID[key.Key] = key
	}
	return nil
}

func (s *Store) RemoveScope(prefix, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removeLocked(func(key string) bool {
		member, _, ok := scopeOf(key, prefix)
		return ok && member == name
	})
}

func (s *Store) PruneScope(prefix string, keep func(name string) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removeLocked(func(key string) bool {
		name, _, ok := scopeOf(key, prefix)
		return ok && !keep(name)
	})
}

func (s *Store) MachineValues(name string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{}
	for _, key := range s.keys {
		if member, short, ok := scopeOf(key.Key, MachinePrefix); ok && member == name && key.Apply == api.ApplyMachineRestart {
			out[short] = s.value(key)
		}
	}
	return out
}

func (s *Store) MachineStarted(name string, values map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetMachineLocked(name)
	for short, v := range values {
		if s.machineInEffect == nil {
			s.machineInEffect = map[string]any{}
		}
		s.machineInEffect[scopedKey(MachinePrefix, name, short)] = v
	}
}

func (s *Store) MachineStopped(name string) {
	s.mu.Lock()
	s.forgetMachineLocked(name)
	s.mu.Unlock()
}

func (s *Store) forgetMachineLocked(name string) {
	for k := range s.machineInEffect {
		if member, _, ok := scopeOf(k, MachinePrefix); ok && member == name {
			delete(s.machineInEffect, k)
		}
	}
}

func (s *Store) removeLocked(gone func(key string) bool) error {
	next := map[string]any{}
	for k, v := range s.set {
		if !gone(k) {
			next[k] = v
		}
	}
	unknown := map[string]json.RawMessage{}
	for k, raw := range s.unknown {
		if !gone(k) {
			unknown[k] = raw
		}
	}
	saved := s.unknown
	s.unknown = unknown
	if len(next) != len(s.set) || len(unknown) != len(saved) {
		if err := s.save(next); err != nil {
			s.unknown = saved
			return err
		}
	}
	s.set = next
	keys := s.keys[:0:0]
	for _, k := range s.keys {
		if gone(k.Key) {
			delete(s.byID, k.Key)
			continue
		}
		keys = append(keys, k)
	}
	s.keys = keys
	for k := range s.machineInEffect {
		if gone(k) {
			delete(s.machineInEffect, k)
		}
	}
	return nil
}

func (s *Store) RenameScope(prefix, from, to string) error {
	s.mu.Lock()
	var templates []api.ConfigKey
	values := map[string]any{}
	for _, k := range s.keys {
		if member, short, ok := scopeOf(k.Key, prefix); ok && member == from {
			t := k
			t.Key = short
			templates = append(templates, t)
			if v, ok := s.set[k.Key]; ok {
				values[short] = v
			}
		}
	}
	running := map[string]any{}
	for k, v := range s.machineInEffect {
		if member, short, ok := scopeOf(k, prefix); ok && member == from {
			running[scopedKey(prefix, to, short)] = v
		}
	}
	s.mu.Unlock()
	if err := s.AddScope(prefix, to, templates, values); err != nil {
		return err
	}
	if err := s.RemoveScope(prefix, from); err != nil {
		return err
	}
	if len(running) > 0 {
		s.mu.Lock()
		if s.machineInEffect == nil {
			s.machineInEffect = map[string]any{}
		}
		for k, v := range running {
			s.machineInEffect[k] = v
		}
		s.mu.Unlock()
	}
	return nil
}

func (s *Store) String(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.byID[name]
	if !ok {
		return ""
	}
	v, _ := s.value(key).(string)
	return v
}
