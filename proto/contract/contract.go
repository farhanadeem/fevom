package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"reflect"
	"sort"
	"strings"
)

type Fixture struct {
	Method       string          `json:"method"`
	Notification bool            `json:"notification,omitempty"`
	Params       json.RawMessage `json:"params"`
	Result       json.RawMessage `json:"result,omitempty"`
}

type Types = struct{ Params, Result any }

func Load(files fs.FS, dir string) ([]Fixture, error) {
	entries, err := fs.ReadDir(files, dir)
	if err != nil {
		return nil, err
	}
	var out []Fixture
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := fs.ReadFile(files, dir+"/"+e.Name())
		if err != nil {
			return nil, err
		}
		var f Fixture
		if err := StrictDecode(data, &f); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if !strings.HasPrefix(e.Name(), f.Method+".") {
			return nil, fmt.Errorf("%s: holds a fixture for %s; name the file after its method", e.Name(), f.Method)
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Method < out[j].Method })
	return out, nil
}

func StrictDecode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func Conforms(data []byte, zero any) error {
	return StrictDecode(data, reflect.New(reflect.TypeOf(zero)).Interface())
}

func Verify(fixtures []Fixture, schema map[string]Types) []error {
	var errs []error
	seen := map[string]bool{}
	for _, f := range fixtures {
		seen[f.Method] = true
		types, ok := schema[f.Method]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: fixture for a method the schema does not define", f.Method))
			continue
		}
		if err := roundTrip(f.Params, types.Params); err != nil {
			errs = append(errs, fmt.Errorf("%s params: %w", f.Method, err))
		}
		if f.Notification != (types.Result == nil) {
			errs = append(errs, fmt.Errorf("%s: notification flag disagrees with the schema", f.Method))
		}
		if types.Result != nil {
			if err := roundTrip(f.Result, types.Result); err != nil {
				errs = append(errs, fmt.Errorf("%s result: %w", f.Method, err))
			}
		}
	}
	var missing []string
	for method := range schema {
		if !seen[method] {
			missing = append(missing, method)
		}
	}
	sort.Strings(missing)
	for _, method := range missing {
		errs = append(errs, fmt.Errorf("%s: no fixture", method))
	}
	return errs
}

func roundTrip(data json.RawMessage, zero any) error {
	v := reflect.New(reflect.TypeOf(zero))
	if err := StrictDecode(data, v.Interface()); err != nil {
		return err
	}
	again, err := json.Marshal(v.Elem().Interface())
	if err != nil {
		return err
	}
	var a, b any
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	if err := json.Unmarshal(again, &b); err != nil {
		return err
	}
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("fixture %s re-encodes as %s", data, again)
	}
	return nil
}

func VerifyPending(fixtures []Fixture, pending, built map[string]Types) []error {
	var errs []error
	all := map[string]Types{}
	var twice []string
	for method, types := range pending {
		all[method] = types
		if _, ok := built[method]; ok {
			twice = append(twice, method)
		}
	}
	sort.Strings(twice)
	for _, method := range twice {
		errs = append(errs, fmt.Errorf("%s: pending and built at once", method))
	}
	for _, f := range fixtures {
		if types, ok := built[f.Method]; ok {
			all[f.Method] = types
		}
	}
	return append(errs, Verify(fixtures, all)...)
}
