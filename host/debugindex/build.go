package debugindex

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"
)

type TableRow struct {
	Command   string
	Attr      string
	Output    string
	TopLevel  bool
	StorePath string
	License   string
	Free      bool
	Main      bool
	Priority  int
}

type Stats struct {
	RawRows     int
	Rows        int
	Commands    int
	WithLicense int
	Unfree      int
	FromSbin    int
	FromLibexec int
	MetaByName  int
	MetaByPname int
	MetaByAttr  int
	MetaMissed  int
}

func Build(raw []RawRow, pm *PkgMetas) ([]TableRow, Stats) {
	var st Stats
	st.RawRows = len(raw)
	seen := map[string]bool{}
	var out []TableRow
	for _, r := range raw {
		m, src := joinMeta(r, pm)
		switch src {
		case "name":
			st.MetaByName++
		case "pname":
			st.MetaByPname++
		case "attr":
			st.MetaByAttr++
		default:
			st.MetaMissed++
		}
		row := TableRow{
			Command:   r.Command,
			Attr:      r.Attr,
			Output:    r.Output,
			TopLevel:  r.TopLevel,
			StorePath: r.StorePath,
			License:   sanitize(m.License),
			Free:      m.Free,
			Main:      m.MainProgram != "" && r.Command == m.MainProgram,
			Priority:  m.Priority,
		}
		if m.Priority == 0 {
			row.Priority = 5
		}
		key := row.Command + "\x00" + row.Attr + "\x00" + row.StorePath
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, row)
		if r.Where == "sbin" {
			st.FromSbin++
		} else if r.Where == "libexec" {
			st.FromLibexec++
		}
		if m.HasLicense {
			st.WithLicense++
		}
		if m.HasLicense && !m.Free {
			st.Unfree++
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Command != out[j].Command {
			return out[i].Command < out[j].Command
		}
		return out[i].StorePath < out[j].StorePath
	})
	st.Rows = len(out)
	cmds := map[string]bool{}
	for _, r := range out {
		cmds[r.Command] = true
	}
	st.Commands = len(cmds)
	return out, st
}

func joinMeta(r RawRow, pm *PkgMetas) (Meta, string) {
	name := nameOf(base(r.StorePath))
	noOut := name
	if r.Output != "" && r.Output != "out" {
		noOut = strings.TrimSuffix(name, "-"+r.Output)
	}
	st := stem(r.StorePath)
	type cand struct {
		m   Meta
		src string
		ok  bool
	}
	cands := []cand{
		{pm.ByName[noOut], "name", hasKey(pm.ByName, noOut)},
		{pm.ByName[name], "name", hasKey(pm.ByName, name)},
		{pm.ByPname[st], "pname", hasKey(pm.ByPname, st)},
	}
	if i := strings.IndexByte(st, '-'); i > 0 {
		cands = append(cands, cand{pm.ByPname[st[i+1:]], "pname", hasKey(pm.ByPname, st[i+1:])})
	}
	cands = append(cands, cand{pm.ByAttr[r.Attr], "attr", hasKey(pm.ByAttr, r.Attr)})
	var first *cand
	for i := range cands {
		if !cands[i].ok {
			continue
		}
		if first == nil {
			first = &cands[i]
		}
		if cands[i].m.HasLicense {
			return cands[i].m, cands[i].src
		}
	}
	if first != nil {
		return first.m, first.src
	}
	return Meta{Priority: 5}, ""
}

func hasKey(m map[string]Meta, k string) bool {
	_, ok := m[k]
	return ok
}

func WriteTable(w io.Writer, rows []TableRow, header string) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "#fev-index-1\t%s\n", header)
	for _, r := range rows {
		fmt.Fprintf(bw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n",
			r.Command, r.Attr, r.Output, b01(r.TopLevel), r.StorePath,
			r.License, b01(r.Free), b01(r.Main), r.Priority)
	}
	return bw.Flush()
}

func b01(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}

func base(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func nameOf(b string) string {
	if len(b) > 33 && b[32] == '-' {
		return b[33:]
	}
	return b
}

func stem(storePath string) string {
	name := nameOf(base(storePath))
	parts := strings.Split(name, "-")
	var keep []string
	for _, q := range parts {
		if q != "" && q[0] >= '0' && q[0] <= '9' {
			break
		}
		keep = append(keep, q)
	}
	if len(keep) == 0 {
		return name
	}
	return strings.Join(keep, "-")
}
