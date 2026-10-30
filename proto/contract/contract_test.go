package contract

import (
	"strings"
	"testing"
	"testing/fstest"
)

type params struct {
	Name string `json:"name"`
	N    int    `json:"n,omitempty"`
}
type result struct {
	OK bool `json:"ok"`
}

var schema = map[string]Types{
	"a.call":   {params{}, result{}},
	"a.notify": {params{}, nil},
}

func load(t *testing.T, files map[string]string) []Fixture {
	t.Helper()
	m := fstest.MapFS{}
	for name, body := range files {
		m["fixtures/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	fx, err := Load(m, "fixtures")
	if err != nil {
		t.Fatal(err)
	}
	return fx
}

func TestVerifyAcceptsFixturesThatMatchTheSchema(t *testing.T) {
	fx := load(t, map[string]string{
		"a.call.json":       `{"method":"a.call","params":{"name":"x"},"result":{"ok":true}}`,
		"a.call.other.json": `{"method":"a.call","params":{"name":"y","n":2},"result":{"ok":false}}`,
		"a.notify.json":     `{"method":"a.notify","notification":true,"params":{"name":"z"}}`,
		"README":            "not a fixture",
	})
	if len(fx) != 3 || fx[0].Method != "a.call" || fx[2].Method != "a.notify" {
		t.Fatalf("loaded %+v", fx)
	}
	if errs := Verify(fx, schema); len(errs) != 0 {
		t.Fatalf("unexpected: %v", errs)
	}
}

func TestVerifyFindsEveryKindOfDrift(t *testing.T) {
	for want, files := range map[string]map[string]string{
		"a.notify: no fixture": {
			"a.call.json": `{"method":"a.call","params":{"name":"x"},"result":{"ok":true}}`,
		},
		"does not define": {
			"a.call.json":   `{"method":"a.call","params":{"name":"x"},"result":{"ok":true}}`,
			"a.notify.json": `{"method":"a.notify","notification":true,"params":{"name":"z"}}`,
			"b.call.json":   `{"method":"b.call","params":{},"result":{}}`,
		},
		"unknown field": {
			"a.call.json":   `{"method":"a.call","params":{"name":"x","extra":1},"result":{"ok":true}}`,
			"a.notify.json": `{"method":"a.notify","notification":true,"params":{"name":"z"}}`,
		},
		"notification flag": {
			"a.call.json":   `{"method":"a.call","params":{"name":"x"},"result":{"ok":true}}`,
			"a.notify.json": `{"method":"a.notify","params":{"name":"z"}}`,
		},
		"re-encodes": {
			"a.call.json":   `{"method":"a.call","params":{"name":"x","n":0},"result":{"ok":true}}`,
			"a.notify.json": `{"method":"a.notify","notification":true,"params":{"name":"z"}}`,
		},
	} {
		errs := Verify(load(t, files), schema)
		found := false
		for _, e := range errs {
			found = found || strings.Contains(e.Error(), want)
		}
		if !found {
			t.Errorf("want an error mentioning %q, got %v", want, errs)
		}
	}
}

func TestLoadRefusesAMisnamedOrMalformedFile(t *testing.T) {
	for name, body := range map[string]string{
		"a.call.json":  `{"method":"a.notify","notification":true,"params":{}}`,
		"broken.json":  `{"method":`,
		"a.extra.json": `{"method":"a.extra","params":{},"result":{},"comment":"no such field"}`,
	} {
		m := fstest.MapFS{"fixtures/" + name: &fstest.MapFile{Data: []byte(body)}}
		if _, err := Load(m, "fixtures"); err == nil {
			t.Errorf("%s was loaded", name)
		}
	}
}

func TestConformsIsStrict(t *testing.T) {
	if err := Conforms([]byte(`{"ok":true}`), result{}); err != nil {
		t.Fatal(err)
	}
	if err := Conforms([]byte(`{"ok":true,"more":1}`), result{}); err == nil {
		t.Fatal("an unknown field was accepted")
	}
	if err := Conforms([]byte(`{"ok":"yes"}`), result{}); err == nil {
		t.Fatal("a wrong type was accepted")
	}
}

func TestVerifyPendingHoldsWhatIsDecidedAndNotBuilt(t *testing.T) {
	pending := map[string]Types{"b.call": {params{}, result{}}}
	good := map[string]string{
		"b.call.json":       `{"method":"b.call","params":{"name":"x"},"result":{"ok":true}}`,
		"a.call.later.json": `{"method":"a.call","params":{"name":"y"},"result":{"ok":false}}`,
	}
	if errs := VerifyPending(load(t, good), pending, schema); len(errs) != 0 {
		t.Fatalf("unexpected: %v", errs)
	}
	for want, c := range map[string]struct {
		files   map[string]string
		pending map[string]Types
	}{
		"b.call: no fixture": {map[string]string{
			"a.call.later.json": good["a.call.later.json"],
		}, pending},
		"does not define": {map[string]string{
			"b.call.json": good["b.call.json"],
			"c.call.json": `{"method":"c.call","params":{},"result":{}}`,
		}, pending},
		"unknown field": {map[string]string{
			"b.call.json":       good["b.call.json"],
			"a.call.later.json": `{"method":"a.call","params":{"name":"y","since":5},"result":{"ok":false}}`,
		}, pending},
		"a.call: pending and built at once": {map[string]string{
			"b.call.json": good["b.call.json"],
			"a.call.json": good["a.call.later.json"],
		}, map[string]Types{"b.call": pending["b.call"], "a.call": schema["a.call"]}},
	} {
		errs := VerifyPending(load(t, c.files), c.pending, schema)
		found := false
		for _, e := range errs {
			found = found || strings.Contains(e.Error(), want)
		}
		if !found {
			t.Errorf("want an error mentioning %q, got %v", want, errs)
		}
	}
}
