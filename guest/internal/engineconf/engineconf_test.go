package engineconf

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func parse(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

var base = Base{HostGateway: "198.18.0.254"}

func TestTheGeneratedFileCarriesFevomsKeys(t *testing.T) {
	data, err := Merge(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"hosts": []any{"unix:///run/docker.sock"}, "data-root": "/var/lib/docker", "exec-root": "/run/docker",
		"pidfile": "/run/docker.pid", "init-path": "/usr/bin/docker-init", "userland-proxy-path": "/usr/bin/docker-proxy",
		"host-gateway-ips": []any{"198.18.0.254"}, "storage-driver": "overlayfs",
		"exec-opts":            []any{"native.cgroupdriver=cgroupfs"},
		"default-ulimits":      map[string]any{"nofile": map[string]any{"Name": "nofile", "Soft": 1024.0, "Hard": 524288.0}},
		"allow-direct-routing": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("generated:\n%s", data)
	}
}

func TestTheUsersKeysAreMergedUnderFevoms(t *testing.T) {
	user := parse(t, `{
		"default-address-pools": [{"base": "10.200.0.0/16", "size": 24}],
		"ipv6": true, "fixed-cidr-v6": "fd00:1::/64",
		"features": {"containerd-snapshotter": false}, "storage-driver": "overlay2",
		"exec-opts": ["native.sysctl=1"],
		"default-ulimits": {"nofile": {"Name": "nofile", "Soft": 4096, "Hard": 4096}},
		"userns-remap": "default", "allow-direct-routing": false
	}`)
	data, err := Merge(base, user)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(data, &got)
	for key, want := range map[string]any{
		"storage-driver": "overlay2", "ipv6": true, "userns-remap": "default", "allow-direct-routing": false,
		"exec-opts":       []any{"native.cgroupdriver=cgroupfs", "native.sysctl=1"},
		"data-root":       "/var/lib/docker",
		"features":        map[string]any{"containerd-snapshotter": false},
		"default-ulimits": map[string]any{"nofile": map[string]any{"Name": "nofile", "Soft": 4096.0, "Hard": 4096.0}},
	} {
		if !reflect.DeepEqual(got[key], want) {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
	if got["default-address-pools"] == nil || got["fixed-cidr-v6"] != "fd00:1::/64" {
		t.Errorf("the user's keys are missing:\n%s", data)
	}
}

func TestReservedKeysAreRefusedByName(t *testing.T) {
	for key := range Reserved {
		_, err := Merge(base, map[string]json.RawMessage{"debug": json.RawMessage("true"), key: json.RawMessage(`"x"`)})
		var r *Refused
		if !errors.As(err, &r) || !strings.HasPrefix(r.Message, key+" is set by fev") {
			t.Errorf("%s: %v", key, err)
		}
	}
	if err := Check(parse(t, `{"data-root":"/somewhere/else"}`)); err == nil ||
		err.Error() != "data-root is set by fev and cannot be changed in docker.json" {
		t.Errorf("the message the contract's fixture shows: %v", err)
	}
	for _, bad := range []string{`{"exec-opts":["native.cgroupdriver=systemd"]}`, `{"exec-opts":"x"}`, `{"exec-opts":[1]}`} {
		if _, err := Merge(base, parse(t, bad)); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	if err := Check(parse(t, `{"pidfile":"p","hosts":[]}`)); err == nil || !strings.HasPrefix(err.Error(), "hosts ") {
		t.Errorf("two reserved keys: %v", err)
	}
	if err := Check(parse(t, `{"debug":true,"exec-opts":["native.sysctl=1"]}`)); err != nil {
		t.Errorf("a configuration with no reserved key: %v", err)
	}
}

func TestTheSuggestedStorageDriverFollowsTheImageStore(t *testing.T) {
	for config, want := range map[string]string{
		`{}`: "overlayfs",
		`{"features":{"containerd-snapshotter":true}}`:      "overlayfs",
		`{"features":{"buildkit":true}}`:                    "overlayfs",
		`{"features":{"containerd-snapshotter":false}}`:     "overlay2",
		`{"userns-remap":"default"}`:                        "overlay2",
		`{"userns-remap":"someone:somegroup"}`:              "overlay2",
		`{"userns-remap":""}`:                               "overlayfs",
		`{"userns-remap":"default","storage-driver":"vfs"}`: "vfs",
		`{"storage-driver":"overlay2"}`:                     "overlay2",
	} {
		data, err := Merge(base, parse(t, config))
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		json.Unmarshal(data, &got)
		if got["storage-driver"] != want {
			t.Errorf("%s: storage-driver %v, want %s", config, got["storage-driver"], want)
		}
	}
}

func TestCanonicalIgnoresOrderAndSpace(t *testing.T) {
	a := parse(t, `{"b": {"y": 1, "x": [1, 2]}, "a": true}`)
	b := parse(t, `{"a":true,"b":{"x":[1,2],"y":1}}`)
	if Canonical(a) != Canonical(b) || Canonical(a) != `{"a":true,"b":{"x":[1,2],"y":1}}` {
		t.Fatalf("%s and %s", Canonical(a), Canonical(b))
	}
	if Canonical(nil) != "{}" || Canonical(map[string]json.RawMessage{}) != "{}" {
		t.Fatal("no configuration is {}")
	}
	if Canonical(parse(t, `{"a":1}`)) == Canonical(parse(t, `{"a":2}`)) {
		t.Fatal("different values compare equal")
	}
	if got := Canonical(parse(t, `{"n":12345678901234567890}`)); got != `{"n":12345678901234567890}` {
		t.Fatal(got)
	}
}

func TestTheCgroupDriverIsRefusedHoweverItIsSpelt(t *testing.T) {
	for _, opt := range []string{
		"native.cgroupdriver=systemd",
		"Native.CgroupDriver=systemd",
		" native.cgroupdriver = systemd",
		"NATIVE.CGROUPDRIVER=cgroupfs",
		"\tnative.cgroupdriver=systemd",
	} {
		data, _ := json.Marshal(map[string]any{"exec-opts": []string{"isolation=default", opt}})
		user := parse(t, string(data))
		var refused *Refused
		if err := Check(user); !errors.As(err, &refused) || !strings.Contains(refused.Message, "cgroup driver") {
			t.Errorf("exec-opts %q: Check gave %v", opt, err)
		}
		if _, err := Merge(base, user); !errors.As(err, &refused) {
			t.Errorf("exec-opts %q: Merge gave %v", opt, err)
		}
	}
	if err := Check(parse(t, `{"exec-opts":["native.cgroupdrivers=x","isolation=default"]}`)); err != nil {
		t.Errorf("an unrelated option was refused: %v", err)
	}
}

func TestThePlansBridgeAndPoolsAreSuggested(t *testing.T) {
	plan := Base{HostGateway: "198.19.1.254", Bip: "198.19.2.1/24",
		Pools: []Pool{{Base: "198.19.64.0/18", Size: 24}, {Base: "198.19.128.0/18", Size: 24}}}
	data, err := Merge(plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(data, &got)
	wantPools := []any{map[string]any{"base": "198.19.64.0/18", "size": 24.0}, map[string]any{"base": "198.19.128.0/18", "size": 24.0}}
	if got["bip"] != "198.19.2.1/24" || !reflect.DeepEqual(got["default-address-pools"], wantPools) ||
		!reflect.DeepEqual(got["host-gateway-ips"], []any{"198.19.1.254"}) {
		t.Fatalf("generated:\n%s", data)
	}
	data, err = Merge(plan, parse(t, `{"bip": "10.1.0.1/24", "default-address-pools": [{"base": "10.200.0.0/16", "size": 24}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	json.Unmarshal(data, &got)
	if got["bip"] != "10.1.0.1/24" || !reflect.DeepEqual(got["default-address-pools"], []any{map[string]any{"base": "10.200.0.0/16", "size": 24.0}}) {
		t.Fatalf("the user's keys did not replace the plan's:\n%s", data)
	}
	data, _ = Merge(Base{HostGateway: "198.19.1.254"}, nil)
	got = nil
	json.Unmarshal(data, &got)
	if _, ok := got["bip"]; ok {
		t.Fatalf("bip without a plan:\n%s", data)
	}
	if _, ok := got["default-address-pools"]; ok {
		t.Fatalf("pools without a plan:\n%s", data)
	}
}

func TestTheRuntimeWrapperIsTheSuggestedDefault(t *testing.T) {
	b := Base{HostGateway: "198.19.1.254", Runtime: "/usr/bin/fev-runc"}
	data, err := Merge(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(data, &got)
	if got["default-runtime"] != RuntimeName ||
		!reflect.DeepEqual(got["runtimes"], map[string]any{RuntimeName: map[string]any{"path": "/usr/bin/fev-runc"}}) {
		t.Fatalf("generated:\n%s", data)
	}
	data, err = Merge(b, parse(t, `{"default-runtime": "runc", "runtimes": {"crun": {"path": "/usr/local/bin/crun"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	json.Unmarshal(data, &got)
	want := map[string]any{RuntimeName: map[string]any{"path": "/usr/bin/fev-runc"}, "crun": map[string]any{"path": "/usr/local/bin/crun"}}
	if got["default-runtime"] != "runc" || !reflect.DeepEqual(got["runtimes"], want) {
		t.Fatalf("the user's runtimes:\n%s", data)
	}
	var refused *Refused
	if _, err := Merge(b, parse(t, `{"runtimes": {"fev-runc": {"path": "/bin/true"}}}`)); !errors.As(err, &refused) ||
		!strings.Contains(err.Error(), RuntimeName) {
		t.Fatalf("a runtime of the wrapper's name: %v", err)
	}
	data, _ = Merge(Base{HostGateway: "198.19.1.254"}, nil)
	got = nil
	json.Unmarshal(data, &got)
	if _, ok := got["default-runtime"]; ok {
		t.Fatalf("a default runtime with no wrapper:\n%s", data)
	}
	if _, ok := got["runtimes"]; ok {
		t.Fatalf("runtimes with no wrapper:\n%s", data)
	}
}

func TestTheIPv6PoolIsSuggested(t *testing.T) {
	b := Base{HostGateway: "198.19.1.254", Pools: []Pool{{Base: "198.19.64.0/18", Size: 24}},
		Pool6: Pool{Base: "fd12:3456:789a:100::/56", Size: 64}}
	data, err := Merge(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(data, &got)
	want := []any{map[string]any{"base": "198.19.64.0/18", "size": 24.0}, map[string]any{"base": "fd12:3456:789a:100::/56", "size": 64.0}}
	if !reflect.DeepEqual(got["default-address-pools"], want) {
		t.Fatalf("generated:\n%s", data)
	}
	if _, ok := got["ipv6"]; ok {
		t.Fatalf("IPv6 turned on by default:\n%s", data)
	}
}
