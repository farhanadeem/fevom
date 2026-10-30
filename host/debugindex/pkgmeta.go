package debugindex

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/andybalholm/brotli"
)

type Meta struct {
	Attr        string
	Pname       string
	Name        string
	Version     string
	License     string
	Free        bool
	HasLicense  bool
	MainProgram string
	Priority    int
}

type pkgRaw struct {
	Pname   string          `json:"pname"`
	Name    string          `json:"name"`
	Version string          `json:"version"`
	Meta    json.RawMessage `json:"meta"`
}

type metaRaw struct {
	License     json.RawMessage `json:"license"`
	MainProgram string          `json:"mainProgram"`
	Priority    *int            `json:"priority"`
	Unfree      *bool           `json:"unfree"`
}

type PkgMetas struct {
	Version string
	ByAttr  map[string]Meta
	ByName  map[string]Meta
	ByPname map[string]Meta
	Count   int
}

func ParsePackages(r io.Reader) (*PkgMetas, error) {
	br := brotli.NewReader(r)
	var top struct {
		Version  string                     `json:"version"`
		Packages map[string]json.RawMessage `json:"packages"`
	}
	dec := json.NewDecoder(br)
	if err := dec.Decode(&top); err != nil {
		return nil, fmt.Errorf("packages.json: %w", err)
	}
	pm := &PkgMetas{
		Version: top.Version,
		ByAttr:  make(map[string]Meta, len(top.Packages)),
		ByName:  make(map[string]Meta, len(top.Packages)),
		ByPname: make(map[string]Meta, len(top.Packages)),
		Count:   len(top.Packages),
	}
	attrs := make([]string, 0, len(top.Packages))
	for a := range top.Packages {
		attrs = append(attrs, a)
	}
	sort.Slice(attrs, func(i, j int) bool {
		if len(attrs[i]) != len(attrs[j]) {
			return len(attrs[i]) < len(attrs[j])
		}
		return attrs[i] < attrs[j]
	})
	for _, attr := range attrs {
		var p pkgRaw
		if err := json.Unmarshal(top.Packages[attr], &p); err != nil {
			continue
		}
		m := Meta{Attr: attr, Pname: p.Pname, Name: p.Name, Version: p.Version, Priority: 5}
		if len(p.Meta) > 0 {
			var mr metaRaw
			if err := json.Unmarshal(p.Meta, &mr); err == nil {
				m.MainProgram = mr.MainProgram
				if mr.Priority != nil {
					m.Priority = *mr.Priority
				}
				disp, free, ok := parseLicense(mr.License)
				m.License, m.HasLicense = disp, ok
				switch {
				case mr.Unfree != nil:
					m.Free = !*mr.Unfree
				default:
					m.Free = free
				}
			}
		}
		pm.ByAttr[attr] = m
		if m.Name != "" {
			if _, ok := pm.ByName[m.Name]; !ok {
				pm.ByName[m.Name] = m
			}
		}
		if m.Pname != "" {
			if _, ok := pm.ByPname[m.Pname]; !ok {
				pm.ByPname[m.Pname] = m
			}
		}
	}
	return pm, nil
}

type oneLicense struct {
	SpdxID    string `json:"spdxId"`
	FullName  string `json:"fullName"`
	ShortName string `json:"shortName"`
	Free      *bool  `json:"free"`
}

func parseLicense(raw json.RawMessage) (display string, free bool, ok bool) {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return "", false, false
	}
	switch raw[0] {
	case '{':
		var comp struct {
			Licenses []json.RawMessage `json:"licenses"`
		}
		if json.Unmarshal(raw, &comp) == nil && len(comp.Licenses) > 0 {
			arr, _ := json.Marshal(comp.Licenses)
			return parseLicense(arr)
		}
		var exc struct {
			License   json.RawMessage `json:"license"`
			Exception json.RawMessage `json:"exception"`
		}
		if json.Unmarshal(raw, &exc) == nil && len(exc.License) > 0 {
			d, f, o := parseLicense(exc.License)
			if o {
				if ed, _, eo := parseLicense(exc.Exception); eo && ed != "" && d != "" {
					d = d + " with " + ed
				}
				return d, f, true
			}
		}
		var l oneLicense
		if json.Unmarshal(raw, &l) != nil {
			return "", false, false
		}
		if licenseDisplay(l) == "" && l.Free == nil {
			return "", false, false
		}
		return licenseDisplay(l), licenseFree(l), true
	case '[':
		var ls []json.RawMessage
		if json.Unmarshal(raw, &ls) != nil || len(ls) == 0 {
			return "", false, false
		}
		var names []string
		allFree := true
		any := false
		for _, e := range ls {
			d, f, o := parseLicense(e)
			if !o {
				continue
			}
			any = true
			if d != "" {
				names = append(names, d)
			}
			if !f {
				allFree = false
			}
		}
		if !any {
			return "", false, false
		}
		return strings.Join(names, ", "), allFree, true
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false, false
		}
		f := !strings.Contains(strings.ToLower(s), "unfree")
		return s, f, true
	}
	return "", false, false
}

func licenseDisplay(l oneLicense) string {
	switch {
	case l.FullName != "":
		return l.FullName
	case l.SpdxID != "":
		return l.SpdxID
	case l.ShortName != "":
		return l.ShortName
	}
	return ""
}

func licenseFree(l oneLicense) bool {
	if l.Free != nil {
		return *l.Free
	}
	return false
}
