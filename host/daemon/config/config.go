package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"fev/proto/agent"
	api "fev/proto/daemon"
	"fev/proto/debugstore"
	"fev/proto/netplan"
)

const FileVersion = 1

const maxGuestMiB = 64512

type Host struct {
	CPUs   int
	MemMiB int64
}

func ThisMac() Host {
	h := Host{CPUs: runtime.NumCPU(), MemMiB: 8192}
	if bytes, err := unix.SysctlUint64("hw.memsize"); err == nil {
		h.MemMiB = int64(bytes >> 20)
	}
	return h
}

func Keys(h Host) []api.ConfigKey {
	bound := func(v int64) *int64 { return &v }
	memDefault := min(int64(8192), h.MemMiB/2)
	memMax := min(h.MemMiB, int64(maxGuestMiB))
	memMin := int64(1024)
	memMin = min(memMin, memMax)
	memDefault = max(memDefault, memMin)
	return []api.ConfigKey{
		{Key: api.KeyCPU, Type: api.TypeInt, Default: int64(h.CPUs), Apply: api.ApplyVMRestart,
			Description: "The most CPU cores the VM can use", Min: bound(1), Max: bound(int64(h.CPUs))},
		{Key: api.KeyMemoryMiB, Type: api.TypeInt, Default: memDefault, Apply: api.ApplyVMRestart,
			Description: "The most memory the VM can use, in MiB. Memory it isn't using goes back to your Mac", Min: bound(memMin), Max: bound(memMax)},
		{Key: api.KeyDockerSetContext, Type: api.TypeBool, Default: true, Apply: api.ApplyLive,
			Description: "Create the fev Docker context and select it, so docker commands use Fevom"},
		{Key: api.KeyDataAllowBackup, Type: api.TypeBool, Default: false, Apply: api.ApplyLive,
			Description: "Include Fevom's data in Time Machine backups. It is one large disk image, which can make backups much larger"},
		{Key: api.KeyNetworkPublishLAN, Type: api.TypeBool, Default: true, Apply: api.ApplyLive,
			Description: "Let other devices on your network reach published ports. Ports published on 127.0.0.1 or ::1 stay on this Mac"},
		{Key: api.KeyFilesShare, Type: api.TypeBool, Default: true, Apply: api.ApplyVMRestart,
			Description: "Share your Mac's files with the VM, so containers can bind-mount Mac folders"},
		{Key: api.KeyPowerPauseInSleep, Type: api.TypeBool, Default: true, Apply: api.ApplyLive,
			Description: "Pause the VM while your Mac sleeps. Containers and machines carry on where they were when it wakes"},
		{Key: api.KeyStartAtLogin, Type: api.TypeBool, Default: false, Apply: api.ApplyLive,
			Description: "Start Fevom in the background when you log in. The VM starts only when something needs it"},
		{Key: api.KeyNetworkHostLoopback, Type: api.TypeBool, Default: true, Apply: api.ApplyLive,
			Description: "Let containers reach servers on your Mac's localhost at host.docker.internal"},
		{Key: api.KeyMachinesForwardPorts, Type: api.TypeBool, Default: true, Apply: api.ApplyLive,
			Description: "Forward the ports machines listen on to localhost on your Mac"},
		{Key: api.KeyMachinesExposePortsToLAN, Type: api.TypeBool, Default: true, Apply: api.ApplyLive,
			Description: "Let other devices on your network reach machines' ports that listen on all addresses (0.0.0.0 or ::)"},
		{Key: api.KeySSHPort, Type: api.TypeInt, Default: int64(api.DefaultSSHPort), Apply: api.ApplyLive,
			Description: "The port of Fevom's SSH server, used by ssh fev", Min: bound(1), Max: bound(65535)},
		{Key: api.KeySSHExposePort, Type: api.TypeBool, Default: false, Apply: api.ApplyLive,
			Description: "Let other devices on your network reach Fevom's SSH server. When off, only this Mac can connect"},
		{Key: api.KeyNetworkSubnet, Type: api.TypeString, Default: netplan.DefaultBase.String(), Apply: api.ApplyVMRestart,
			Description: "The IPv4 /16 Fevom takes its addresses from, chosen at the first start away from your Mac's networks. Changing it renumbers every container and machine"},
		{Key: api.KeyNetworkSubnet6, Type: api.TypeString, Default: "", Apply: api.ApplyVMRestart,
			Description: "The IPv6 /48 (a unique local range) Fevom takes its addresses from, made at the first start"},
		{Key: api.KeyNetworkDirectAccess, Type: api.TypeBool, Default: true, Apply: api.ApplyLive,
			Description: "Reach containers and machines at their IP addresses from your Mac. Needs the admin helper (sudo fev setup --admin)"},
		{Key: api.KeyNetworkHTTPS, Type: api.TypeBool, Default: true, Apply: api.ApplyLive,
			Description: "Serve fev.local names over HTTPS, with certificates from Fevom's own root certificate, which your Mac trusts after fev setup --https"},
		{Key: api.KeyNetworkProxy, Type: api.TypeString, Default: ProxyAuto, Apply: api.ApplyLive,
			Description: "The proxy for image pulls and for containers' and machines' connections: auto follows your Mac's settings, none uses no proxy, or enter a proxy URL"},
		{Key: api.KeyNetworkProxyExclude, Type: api.TypeList, Default: []string{}, Apply: api.ApplyLive,
			Description: "Hosts, domains and IP ranges (CIDR) reached without the proxy, in addition to your Mac's own exceptions"},
		{Key: api.KeyNetworkProxyTransparent, Type: api.TypeBool, Default: true, Apply: api.ApplyLive,
			Description: "Send containers' and machines' TCP connections through the proxy, without proxy settings inside them"},
		{Key: api.KeyAdminEnabled, Type: api.TypeBool, Default: true, Apply: api.ApplyLive,
			Description: "Use the admin helper for direct IP access, fev.local names on your Mac and ports below 1024. Install it with sudo fev setup --admin"},
		{Key: api.KeyFilesViewHidden, Type: api.TypeBool, Default: false, Apply: api.ApplyLive,
			Description: "Hide ~/Fevom from Finder's sidebar and the desktop. The folder stays at ~/Fevom"},
		{Key: api.KeyEmulationX86, Type: api.TypeBool, Default: true, Apply: api.ApplyEngineRestart,
			Description: "Run x86 Linux software (linux/amd64 images, amd64 machines and 32-bit programs) through translation in the VM"},
		{Key: api.KeyEmulationEngine, Type: api.TypeString, Default: agent.EmulationEngineFEX, Apply: api.ApplyVMRestart,
			Description: "The x86-64 translator: auto uses Rosetta, and FEX for software known to fail under Rosetta; fex or rosetta forces one. 32-bit programs always use FEX. A machine's emulation setting or a container's dev.fevom.emulation label overrides it"},
		{Key: api.KeyDockerEnabled, Type: api.TypeBool, Default: true, Apply: api.ApplyLive,
			Description: "Run the Docker engine. When off, docker commands are refused and Linux machines keep running"},
		{Key: api.KeyDockerNodeName, Type: api.TypeString, Default: agent.DefaultNodeName, Apply: api.ApplyVMRestart,
			Description: "The Docker engine's host name, shown by docker info and to a swarm's nodes"},
		{Key: api.KeyDebugCacheURL, Type: api.TypeString, Default: debugstore.CacheURL, Apply: api.ApplyLive,
			Description: "The binary cache the Debug Shell's tools are downloaded from. A mirror must serve the cache's own signatures. Applies at the next download"},
		{Key: api.KeyDebugIndexURL, Type: api.TypeString, Default: "", Apply: api.ApplyLive,
			Description: "Where the Debug Shell's command index is downloaded from. Empty uses the index that ships with Fevom. Applies at the next download"},
		{Key: api.KeyKubernetesEnabled, Type: api.TypeBool, Default: false, Apply: api.ApplyLive,
			Description: "Run a single-node Kubernetes cluster beside Docker that can run your Docker images. The first start downloads about 250 MB. While on, it uses about a tenth of a CPU core and up to 2 GiB of memory"},
		{Key: api.KeyKubernetesExposeLAN, Type: api.TypeBool, Default: false, Apply: api.ApplyLive,
			Description: "Let other devices on your network reach the cluster's NodePort services. When off, only this Mac can"},
		{Key: api.KeyKubernetesKubelet, Type: api.TypeObject, Default: map[string]any{}, Apply: api.ApplyLive,
			Description: "Fields of the cluster's kubelet configuration (KubeletConfiguration) as a JSON object. Applying it restarts k3s; workloads keep running"},
		{Key: api.KeyKubernetesMetricsServer, Type: api.TypeBool, Default: false, Apply: api.ApplyLive,
			Description: "Run metrics-server in the cluster, for kubectl top"},
		{Key: api.KeyKubernetesImagesURL, Type: api.TypeString, Default: "", Apply: api.ApplyLive,
			Description: "Where the cluster's image archive is downloaded from at the first start. Empty uses k3s's release. Its SHA-256 is checked either way"},
	}
}

type Error struct {
	Key     string
	Reason  string
	Unknown bool
}

var declined = map[string]string{
	"rosetta": "use emulation.engine rosetta to select Rosetta or emulation.engine fex to select FEX; emulation.x86 turns x86 programs off and on",
}

func (e *Error) Error() string {
	if e.Unknown {
		if why, ok := declined[e.Key]; ok {
			return "no such configuration key: " + e.Key + ". " + why
		}
		return "no such configuration key: " + e.Key
	}
	return e.Key + ": " + e.Reason
}

type file struct {
	Version int                        `json:"version"`
	Values  map[string]json.RawMessage `json:"values"`
}

type Store struct {
	path string
	keys []api.ConfigKey
	byID map[string]api.ConfigKey

	mu              sync.Mutex
	set             map[string]any
	unknown         map[string]json.RawMessage
	inEffect        map[string]any
	machineInEffect map[string]any
	warnings        []string
}

func Load(path string, keys []api.ConfigKey, logf func(format string, args ...any)) (*Store, error) {
	s := &Store{path: path, keys: keys, byID: map[string]api.ConfigKey{}, set: map[string]any{}, unknown: map[string]json.RawMessage{}}
	for _, k := range keys {
		s.byID[k.Key] = k
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil || f.Version != FileVersion {
		aside := fmt.Sprintf("%s.corrupt-%s", path, time.Now().UTC().Format("20060102T150405"))
		why := fmt.Sprintf("version %d where %d was expected", f.Version, FileVersion)
		if err != nil {
			why = err.Error()
		}
		if rerr := os.Rename(path, aside); rerr != nil {
			return nil, fmt.Errorf("%s cannot be read (%s) and cannot be set aside: %w", path, why, rerr)
		}
		w := fmt.Sprintf("%s could not be read and was set aside as %s; defaults are in use", filepath.Base(path), filepath.Base(aside))
		s.warnings = append(s.warnings, w)
		logf("%s (%s)", w, why)
		return s, nil
	}
	for name, raw := range f.Values {
		key, ok := s.byID[name]
		if !ok {
			s.unknown[name] = raw
			if !isScoped(name) {
				logf("%s: key %q is not known to this build; it is kept as it is", filepath.Base(path), name)
			}
			continue
		}
		var v any
		err := json.Unmarshal(raw, &v)
		if err == nil {
			v, err = validate(key, v)
		}
		if err != nil {
			w := fmt.Sprintf("%s: the value of %s is not valid (%v); the default is in use", filepath.Base(path), name, err)
			s.warnings = append(s.warnings, w)
			logf("%s", w)
			continue
		}
		if !reflect.DeepEqual(v, key.Default) {
			s.set[name] = v
		}
	}
	return s, nil
}

func validate(key api.ConfigKey, v any) (any, error) {
	v, err := validateType(key, v)
	if err != nil {
		return nil, err
	}
	if check := checks[key.Key]; check != nil {
		return check(v)
	}
	return v, nil
}

func validateType(key api.ConfigKey, v any) (any, error) {
	switch key.Type {
	case api.TypeBool:
		if b, ok := v.(bool); ok {
			return b, nil
		}
		return nil, errors.New("must be true or false")
	case api.TypeString:
		if s, ok := v.(string); ok {
			return s, nil
		}
		return nil, errors.New("must be a string")
	case api.TypeList:
		return listOf(v)
	case api.TypeObject:
		if m, ok := v.(map[string]any); ok {
			return m, nil
		}
		return nil, errors.New("must be a JSON object")
	case api.TypeInt:
		var n int64
		switch x := v.(type) {
		case float64:
			if x != math.Trunc(x) || math.Abs(x) > 1<<53 {
				return nil, errors.New("must be a whole number")
			}
			n = int64(x)
		case int64:
			n = x
		case int:
			n = int64(x)
		default:
			return nil, errors.New("must be a whole number")
		}
		if key.Min != nil && n < *key.Min {
			return nil, fmt.Errorf("at least %d", *key.Min)
		}
		if key.Max != nil && n > *key.Max {
			return nil, fmt.Errorf("at most %d on this Mac", *key.Max)
		}
		return n, nil
	}
	return nil, fmt.Errorf("key has unknown type %q", key.Type)
}

func (s *Store) Warnings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.warnings...)
}

func (s *Store) Describe() []api.ConfigKey { return append([]api.ConfigKey(nil), s.keys...) }

func (s *Store) value(key api.ConfigKey) any {
	if v, ok := s.set[key.Key]; ok {
		return v
	}
	return key.Default
}

func (s *Store) pending(key api.ConfigKey) bool {
	if key.Apply == api.ApplyMachineRestart {
		v, ok := s.machineInEffect[key.Key]
		return ok && !reflect.DeepEqual(v, s.value(key))
	}
	if (key.Apply != api.ApplyVMRestart && key.Apply != api.ApplyEngineRestart) || s.inEffect == nil {
		return false
	}
	return !reflect.DeepEqual(s.inEffect[key.Key], s.value(key))
}

func (s *Store) Get(names ...string) (map[string]api.ConfigValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.lookup(names)
	if err != nil {
		return nil, err
	}
	out := map[string]api.ConfigValue{}
	for _, key := range keys {
		_, isSet := s.set[key.Key]
		out[key.Key] = api.ConfigValue{Value: Redact(key.Key, s.value(key)), IsDefault: !isSet, Pending: s.pending(key)}
	}
	return out, nil
}

func (s *Store) lookup(names []string) ([]api.ConfigKey, error) {
	if len(names) == 0 {
		return s.keys, nil
	}
	keys := make([]api.ConfigKey, 0, len(names))
	for _, name := range names {
		key, ok := s.byID[name]
		if !ok {
			return nil, &Error{Key: name, Unknown: true}
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func (s *Store) Int(name string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value(s.byID[name]).(int64)
}

func (s *Store) Bool(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value(s.byID[name]).(bool)
}

func (s *Store) Object(name string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{}
	for k, v := range s.value(s.byID[name]).(map[string]any) {
		out[k] = v
	}
	return out
}

func (s *Store) Set(values map[string]any) ([]api.ConfigChange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	checked := map[string]any{}
	for name, v := range values {
		key, ok := s.byID[name]
		if !ok {
			return nil, &Error{Key: name, Unknown: true}
		}
		v, err := validate(key, v)
		if err == nil {
			err = checkScoped(name, v)
		}
		if err != nil {
			return nil, &Error{Key: name, Reason: err.Error()}
		}
		if cur := s.value(key); !reflect.DeepEqual(v, cur) && reflect.DeepEqual(v, Redact(name, cur)) {
			v = cur
		}
		checked[name] = v
	}
	return s.apply(checked)
}

func (s *Store) Add(name string, values []string) ([]api.ConfigChange, error) {
	return s.editList(name, values, true)
}

func (s *Store) Remove(name string, values []string) ([]api.ConfigChange, error) {
	return s.editList(name, values, false)
}

func (s *Store) editList(name string, values []string, add bool) ([]api.ConfigChange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.byID[name]
	if !ok {
		return nil, &Error{Key: name, Unknown: true}
	}
	if key.Type != api.TypeList {
		verb := "add"
		if !add {
			verb = "remove"
		}
		return nil, &Error{Key: name, Reason: fmt.Sprintf("is a %s, not a list; config %s changes only a list, and config set this key", key.Type, verb)}
	}
	given := make([]any, len(values))
	for i, v := range values {
		given[i] = v
	}
	checked, err := validate(key, given)
	if err == nil {
		err = checkScoped(name, checked)
	}
	if err != nil {
		return nil, &Error{Key: name, Reason: err.Error()}
	}
	cur := s.value(key).([]string)
	next := append([]string{}, cur...)
	for _, v := range checked.([]string) {
		at := slices.Index(next, v)
		switch {
		case add && at < 0:
			next = append(next, v)
		case !add && at >= 0:
			next = slices.Delete(next, at, at+1)
		}
	}
	if add && len(next) != len(cur) {
		if _, err := validate(key, toAny(next)); err != nil {
			return nil, &Error{Key: name, Reason: err.Error()}
		}
	}
	return s.apply(map[string]any{name: next})
}

func toAny(list []string) []any {
	out := make([]any, len(list))
	for i, v := range list {
		out[i] = v
	}
	return out
}

func (s *Store) List(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.byID[name]
	if !ok {
		return nil
	}
	v, _ := s.value(key).([]string)
	return append([]string{}, v...)
}

func (s *Store) Reset(names ...string) ([]api.ConfigChange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.lookup(names)
	if err != nil {
		return nil, err
	}
	values := map[string]any{}
	for _, key := range keys {
		values[key.Key] = key.Default
	}
	return s.apply(values)
}

func (s *Store) apply(values map[string]any) ([]api.ConfigChange, error) {
	next := map[string]any{}
	for name, v := range s.set {
		next[name] = v
	}
	changed := []api.ConfigChange{}
	for name, v := range values {
		key := s.byID[name]
		if reflect.DeepEqual(s.value(key), v) {
			continue
		}
		if reflect.DeepEqual(v, key.Default) {
			delete(next, name)
		} else {
			next[name] = v
		}
		changed = append(changed, api.ConfigChange{Key: name, Value: Redact(name, v), Apply: key.Apply})
	}
	if len(changed) == 0 {
		return changed, nil
	}
	get := func(name string) any {
		if v, ok := next[name]; ok {
			return v
		}
		if k, ok := s.byID[name]; ok {
			return k.Default
		}
		return nil
	}
	for _, rule := range together {
		if err := rule(get, values); err != nil {
			return nil, err
		}
	}
	if err := s.save(next); err != nil {
		return nil, fmt.Errorf("cannot write %s: %w", s.path, err)
	}
	s.set = next
	sort.Slice(changed, func(i, j int) bool { return changed[i].Key < changed[j].Key })
	for i := range changed {
		changed[i].Pending = s.pending(s.byID[changed[i].Key])
	}
	return changed, nil
}

func (s *Store) save(set map[string]any) error {
	f := file{Version: FileVersion, Values: map[string]json.RawMessage{}}
	for name, raw := range s.unknown {
		f.Values[name] = raw
	}
	for name, v := range set {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		f.Values[name] = raw
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(data, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), s.path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

func (s *Store) VMStarted() (cpus, memMiB int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inEffect = map[string]any{}
	for _, key := range s.keys {
		if key.Apply == api.ApplyVMRestart || key.Apply == api.ApplyEngineRestart {
			s.inEffect[key.Key] = s.value(key)
		}
	}
	return int(s.inEffect[api.KeyCPU].(int64)), int(s.inEffect[api.KeyMemoryMiB].(int64))
}

func (s *Store) InEffect(name string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.inEffect[name]
	return v, ok
}

func (s *Store) SetInEffect(name string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key, ok := s.byID[name]; ok && key.Apply == api.ApplyEngineRestart && s.inEffect != nil {
		s.inEffect[name] = v
	}
}

func (s *Store) SetStarting(values map[string]any) ([]api.ConfigChange, error) {
	changed, err := s.Set(values)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inEffect != nil {
		for name := range values {
			if key := s.byID[name]; key.Apply == api.ApplyVMRestart {
				s.inEffect[name] = s.value(key)
			}
		}
	}
	for i := range changed {
		changed[i].Pending = s.pending(s.byID[changed[i].Key])
	}
	return changed, nil
}

func (s *Store) IsDefault(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, set := s.set[name]
	_, ok := s.byID[name]
	return ok && !set
}

func (s *Store) VMStopped() {
	s.mu.Lock()
	s.inEffect = nil
	s.machineInEffect = nil
	s.mu.Unlock()
}

func (s *Store) PendingRestart() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := []string{}
	for _, key := range s.keys {
		if key.Apply == api.ApplyVMRestart && s.pending(key) {
			pending = append(pending, key.Key)
		}
	}
	sort.Strings(pending)
	return pending
}
