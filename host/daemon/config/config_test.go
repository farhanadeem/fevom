package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	api "fev/proto/daemon"
)

var mac = Host{CPUs: 10, MemMiB: 32768}

func load(t *testing.T, path string) (*Store, *[]string) {
	t.Helper()
	logged := &[]string{}
	s, err := Load(path, Keys(mac), func(format string, args ...any) { *logged = append(*logged, format) })
	if err != nil {
		t.Fatal(err)
	}
	return s, logged
}

func fresh(t *testing.T) (*Store, string) {
	path := filepath.Join(t.TempDir(), "config", "config.json")
	s, _ := load(t, path)
	return s, path
}

func TestTheRegistryDescribesItsKeys(t *testing.T) {
	s, _ := fresh(t)
	keys := s.Describe()
	var names []string
	for _, k := range keys {
		names = append(names, k.Key)
		if k.Description == "" || k.Type == "" || k.Apply == "" || k.Default == nil {
			t.Errorf("%s is not fully described: %+v", k.Key, k)
		}
	}
	if !reflect.DeepEqual(names, []string{api.KeyCPU, api.KeyMemoryMiB, api.KeyDockerSetContext, api.KeyDataAllowBackup, api.KeyNetworkPublishLAN, api.KeyFilesShare,
		api.KeyPowerPauseInSleep, api.KeyStartAtLogin, api.KeyNetworkHostLoopback, api.KeyMachinesForwardPorts, api.KeyMachinesExposePortsToLAN, api.KeySSHPort, api.KeySSHExposePort,
		api.KeyNetworkSubnet, api.KeyNetworkSubnet6, api.KeyNetworkDirectAccess, api.KeyNetworkHTTPS, api.KeyNetworkProxy, api.KeyNetworkProxyExclude, api.KeyNetworkProxyTransparent, api.KeyAdminEnabled, api.KeyFilesViewHidden,
		api.KeyEmulationX86, api.KeyEmulationEngine, api.KeyDockerEnabled, api.KeyDockerNodeName,
		api.KeyDebugCacheURL, api.KeyDebugIndexURL,
		api.KeyKubernetesEnabled, api.KeyKubernetesExposeLAN, api.KeyKubernetesKubelet, api.KeyKubernetesMetricsServer, api.KeyKubernetesImagesURL}) {
		t.Errorf("keys: %v", names)
	}
	cpu, mem := keys[0], keys[1]
	if cpu.Default != int64(10) || *cpu.Min != 1 || *cpu.Max != 10 || cpu.Apply != api.ApplyVMRestart {
		t.Errorf("cpu: %+v", cpu)
	}
	if mem.Default != int64(8192) || *mem.Min != 1024 || *mem.Max != 32768 || mem.Apply != api.ApplyVMRestart {
		t.Errorf("memory_mib: %+v", mem)
	}
	if keys[2].Default != true || keys[2].Apply != api.ApplyLive || keys[3].Default != false || keys[3].Apply != api.ApplyLive {
		t.Errorf("the two live keys: %+v %+v", keys[2], keys[3])
	}
	if keys[4].Default != true || keys[4].Apply != api.ApplyLive || keys[4].Type != api.TypeBool {
		t.Errorf("network.publish_lan: %+v", keys[4])
	}
	if keys[5].Default != true || keys[5].Apply != api.ApplyVMRestart || keys[5].Type != api.TypeBool {
		t.Errorf("files.share: %+v", keys[5])
	}
	for i, def := range map[int]bool{6: true, 7: false, 8: true, 9: true, 10: true} {
		if keys[i].Default != def || keys[i].Apply != api.ApplyLive || keys[i].Type != api.TypeBool {
			t.Errorf("%s: %+v", keys[i].Key, keys[i])
		}
	}
	got, _ := json.Marshal(api.ConfigKeys{Keys: keys})
	if want := describeFixture(t, len(keys)); string(got) != want {
		t.Errorf("describe encodes as\n%s\nwant\n%s", got, want)
	}
}

func describeFixture(t *testing.T, n int) string {
	t.Helper()
	fixtures, err := api.Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	for _, fx := range fixtures {
		if fx.Method != api.MethodConfigDescribe {
			continue
		}
		var want struct {
			Keys []json.RawMessage `json:"keys"`
		}
		if err := json.Unmarshal(fx.Result, &want); err != nil || len(want.Keys) != n {
			t.Fatalf("the fixture has %d keys, the registry %d (%v)", len(want.Keys), n, err)
		}
		var out bytes.Buffer
		if err := json.Compact(&out, []byte(`{"keys":[`+string(bytes.Join(rawSlice(want.Keys[:n]), []byte(",")))+`]}`)); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	t.Fatal("no config.describe fixture")
	return ""
}

func rawSlice(in []json.RawMessage) [][]byte {
	out := make([][]byte, len(in))
	for i, r := range in {
		out[i] = r
	}
	return out
}

func TestDefaultsAndBoundsFollowTheMac(t *testing.T) {
	for _, c := range []struct {
		host                     Host
		cpu, mem, memMin, memMax int64
	}{
		{Host{CPUs: 8, MemMiB: 8192}, 8, 4096, 1024, 8192},
		{Host{CPUs: 24, MemMiB: 196608}, 24, 8192, 1024, 64512},
		{Host{CPUs: 2, MemMiB: 512}, 2, 512, 512, 512},
	} {
		keys := Keys(c.host)
		if keys[0].Default != c.cpu || *keys[0].Max != c.cpu {
			t.Errorf("%+v: cpu %+v", c.host, keys[0])
		}
		if keys[1].Default != c.mem || *keys[1].Min != c.memMin || *keys[1].Max != c.memMax {
			t.Errorf("%+v: memory default %v, min %d, max %d", c.host, keys[1].Default, *keys[1].Min, *keys[1].Max)
		}
	}
	if h := ThisMac(); h.CPUs < 1 || h.MemMiB < 1024 {
		t.Errorf("this Mac: %+v", h)
	}
}

func TestValuesAreValidatedByTypeAndRange(t *testing.T) {
	s, _ := fresh(t)
	for _, c := range []struct {
		key    string
		value  any
		reason string
	}{
		{api.KeyCPU, float64(0), "at least 1"},
		{api.KeyCPU, float64(11), "at most 10 on this Mac"},
		{api.KeyCPU, 2.5, "whole number"},
		{api.KeyCPU, "4", "whole number"},
		{api.KeyCPU, true, "whole number"},
		{api.KeyCPU, nil, "whole number"},
		{api.KeyCPU, 1e300, "whole number"},
		{api.KeyMemoryMiB, float64(1023), "at least 1024"},
		{api.KeyMemoryMiB, float64(32769), "at most 32768"},
		{api.KeyDataAllowBackup, "true", "true or false"},
		{api.KeyDataAllowBackup, float64(1), "true or false"},
		{api.KeyDockerSetContext, nil, "true or false"},
		{api.KeyNetworkPublishLAN, "off", "true or false"},
	} {
		_, err := s.Set(map[string]any{c.key: c.value})
		e, ok := err.(*Error)
		if !ok || e.Key != c.key || e.Unknown || !strings.Contains(e.Reason, c.reason) {
			t.Errorf("set %s=%v: %v, want a refusal mentioning %q", c.key, c.value, err, c.reason)
		}
	}
	for _, c := range []struct {
		key   string
		value any
	}{{api.KeyCPU, float64(1)}, {api.KeyCPU, float64(10)}, {api.KeyMemoryMiB, float64(1024)}, {api.KeyMemoryMiB, float64(32768)}, {api.KeyDataAllowBackup, true}} {
		if _, err := s.Set(map[string]any{c.key: c.value}); err != nil {
			t.Errorf("set %s=%v at the edge of its range: %v", c.key, c.value, err)
		}
	}
	if _, err := s.Set(map[string]any{"rosetta": true}); !isUnknown(err, "rosetta") {
		t.Errorf("set of an unknown key: %v", err)
	}
	if _, err := s.Get("cpu", "rosetta"); !isUnknown(err, "rosetta") {
		t.Errorf("get of an unknown key: %v", err)
	}
	if _, err := s.Reset("rosetta"); !isUnknown(err, "rosetta") {
		t.Errorf("reset of an unknown key: %v", err)
	}
	_, err := s.Set(map[string]any{"rosetta": true})
	if !isUnknown(err, "rosetta") || !strings.Contains(err.Error(), "FEX") || !strings.Contains(err.Error(), "emulation.engine") {
		t.Errorf("rosetta is refused with %v; want the reason", err)
	}
	if _, err := s.Set(map[string]any{"rosette": true}); !isUnknown(err, "rosette") || strings.Contains(err.Error(), "FEX") {
		t.Errorf("another unknown key: %v", err)
	}
}

func isUnknown(err error, key string) bool {
	e, ok := err.(*Error)
	return ok && e.Unknown && e.Key == key
}

func TestASetWithOneBadValueChangesNothing(t *testing.T) {
	s, path := fresh(t)
	_, err := s.Set(map[string]any{api.KeyMemoryMiB: float64(4096), api.KeyDataAllowBackup: true, api.KeyCPU: float64(64)})
	if e, ok := err.(*Error); !ok || e.Key != api.KeyCPU {
		t.Fatalf("err: %v", err)
	}
	values, _ := s.Get()
	for name, v := range values {
		if !v.IsDefault {
			t.Errorf("%s was changed by a refused set: %+v", name, v)
		}
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a refused set wrote the file")
	}
}

func TestASetReportsWhatChangedAndIsOnDiskWhenItReturns(t *testing.T) {
	s, path := fresh(t)
	changed, err := s.Set(map[string]any{api.KeyMemoryMiB: float64(4096), api.KeyDataAllowBackup: true, api.KeyCPU: float64(10)})
	if err != nil {
		t.Fatal(err)
	}
	want := []api.ConfigChange{
		{Key: api.KeyDataAllowBackup, Value: true, Apply: api.ApplyLive},
		{Key: api.KeyMemoryMiB, Value: int64(4096), Apply: api.ApplyVMRestart},
	}
	if !reflect.DeepEqual(changed, want) {
		t.Errorf("changed: %+v", changed)
	}
	var f struct {
		Version int            `json:"version"`
		Values  map[string]any `json:"values"`
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &f) != nil {
		t.Fatalf("file: %s %v", data, err)
	}
	if f.Version != 1 || !reflect.DeepEqual(f.Values, map[string]any{"memory_mib": float64(4096), "data_allow_backup": true}) {
		t.Errorf("file: %s", data)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("%d files in the config directory", len(entries))
	}

	again, _ := load(t, path)
	values, _ := again.Get()
	if v := values[api.KeyMemoryMiB]; v.Value != int64(4096) || v.IsDefault {
		t.Errorf("memory_mib after a reload: %+v", v)
	}
	if v := values[api.KeyCPU]; v.Value != int64(10) || !v.IsDefault {
		t.Errorf("cpu after a reload: %+v", v)
	}
	if again.Int(api.KeyMemoryMiB) != 4096 || !again.Bool(api.KeyDataAllowBackup) || !again.Bool(api.KeyDockerSetContext) {
		t.Error("typed reads disagree with Get")
	}

	changed, _ = again.Set(map[string]any{api.KeyDataAllowBackup: false})
	if len(changed) != 1 || changed[0].Value != false {
		t.Errorf("changed: %+v", changed)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "data_allow_backup") {
		t.Errorf("a value equal to its default is still in the file: %s", data)
	}
	changed, _ = again.Reset()
	if len(changed) != 1 || changed[0].Key != api.KeyMemoryMiB || changed[0].Value != int64(8192) {
		t.Errorf("reset of everything changed %+v", changed)
	}
	if changed, _ := again.Reset(api.KeyMemoryMiB); len(changed) != 0 {
		t.Errorf("a reset of a key at its default changed %+v", changed)
	}
}

func TestAValueIsPendingOnlyWhileAVMRunsWithAnother(t *testing.T) {
	s, _ := fresh(t)
	changed, _ := s.Set(map[string]any{api.KeyMemoryMiB: float64(2048)})
	if changed[0].Pending || len(s.PendingRestart()) != 0 {
		t.Error("a change with no VM running is pending")
	}
	if cpus, mem := s.VMStarted(); cpus != 10 || mem != 2048 {
		t.Errorf("the VM is started with %d CPUs and %d MiB", cpus, mem)
	}
	changed, _ = s.Set(map[string]any{api.KeyMemoryMiB: float64(4096), api.KeyCPU: float64(4), api.KeyDataAllowBackup: true})
	for _, ch := range changed {
		if want := ch.Apply == api.ApplyVMRestart; ch.Pending != want {
			t.Errorf("%s: pending=%v", ch.Key, ch.Pending)
		}
	}
	if got := s.PendingRestart(); !reflect.DeepEqual(got, []string{api.KeyCPU, api.KeyMemoryMiB}) {
		t.Errorf("pending: %v", got)
	}
	values, _ := s.Get(api.KeyMemoryMiB, api.KeyDataAllowBackup)
	if !values[api.KeyMemoryMiB].Pending || values[api.KeyDataAllowBackup].Pending || len(values) != 2 {
		t.Errorf("get: %+v", values)
	}
	s.Set(map[string]any{api.KeyMemoryMiB: float64(2048)})
	if got := s.PendingRestart(); !reflect.DeepEqual(got, []string{api.KeyCPU}) {
		t.Errorf("pending after setting memory back: %v", got)
	}
	s.VMStopped()
	if got := s.PendingRestart(); len(got) != 0 {
		t.Errorf("pending with the VM stopped: %v", got)
	}
	if cpus, _ := s.VMStarted(); cpus != 4 || len(s.PendingRestart()) != 0 {
		t.Errorf("the next VM is started with %d CPUs, pending %v", cpus, s.PendingRestart())
	}
}

func TestAnUnknownKeyInTheFileIsKeptAndLogged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"version":1,"values":{"memory_mib":4096,"machine.dev.rosetta":{"on":true,"n":[1,2]}}}`), 0o644)
	s, logged := load(t, path)
	if len(*logged) != 1 || !strings.Contains((*logged)[0], "not known to this build") || len(s.Warnings()) != 0 {
		t.Errorf("logged %q, warnings %q", *logged, s.Warnings())
	}
	if _, err := s.Get("machine.dev.rosetta"); !isUnknown(err, "machine.dev.rosetta") {
		t.Errorf("an unknown key from the file is readable through the API: %v", err)
	}
	if _, err := s.Set(map[string]any{api.KeyCPU: float64(2)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var f struct {
		Values map[string]json.RawMessage `json:"values"`
	}
	json.Unmarshal(data, &f)
	var kept any
	json.Unmarshal(f.Values["machine.dev.rosetta"], &kept)
	if len(f.Values) != 1 || !reflect.DeepEqual(kept, map[string]any{"on": true, "n": []any{float64(1), float64(2)}}) {
		t.Errorf("after a set and a reset the file is %s", data)
	}
}

func TestACorruptFileIsSetAsideAndDefaultsAreUsed(t *testing.T) {
	for name, content := range map[string]string{
		"not JSON":        `{"version":1,"values":{"memory_mib":40`,
		"empty":           ``,
		"another version": `{"version":2,"values":{"memory_mib":4096}}`,
		"wrong shape":     `[1,2,3]`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			os.WriteFile(path, []byte(content), 0o644)
			s, logged := load(t, path)
			warnings := s.Warnings()
			if len(warnings) != 1 || !strings.Contains(warnings[0], "set aside as config.json.corrupt-") || !strings.Contains(warnings[0], "defaults are in use") || len(*logged) != 1 {
				t.Fatalf("warnings %q, logged %q", warnings, *logged)
			}
			aside, _ := filepath.Glob(path + ".corrupt-*")
			if len(aside) != 1 {
				t.Fatalf("set aside: %v", aside)
			}
			if data, _ := os.ReadFile(aside[0]); string(data) != content {
				t.Errorf("the file set aside holds %q", data)
			}
			if _, err := os.Stat(path); err == nil {
				t.Error("the corrupt file is still in place")
			}
			values, _ := s.Get()
			for key, v := range values {
				if !v.IsDefault {
					t.Errorf("%s is not at its default: %+v", key, v)
				}
			}
			if _, err := s.Set(map[string]any{api.KeyCPU: float64(3)}); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestAnInvalidValueInTheFileFallsBackToTheDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"version":1,"values":{"cpu":64,"memory_mib":2048,"data_allow_backup":"yes"}}`), 0o644)
	s, _ := load(t, path)
	if w := s.Warnings(); len(w) != 2 || !strings.Contains(strings.Join(w, "\n"), "the value of cpu is not valid (at most 10 on this Mac)") {
		t.Errorf("warnings: %q", w)
	}
	values, _ := s.Get()
	if v := values[api.KeyCPU]; v.Value != int64(10) || !v.IsDefault {
		t.Errorf("cpu: %+v", v)
	}
	if v := values[api.KeyMemoryMiB]; v.Value != int64(2048) {
		t.Errorf("a valid value beside invalid ones was lost: %+v", v)
	}
}
