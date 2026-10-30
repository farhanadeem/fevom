package fw

import (
	"strings"
)

type Tables map[string]*Table

type Table struct {
	Chains map[string]bool
	Rules  map[string]bool
	Order  map[string][]string
}

func ParseSave(out string) Tables {
	t := Tables{}
	var cur *Table
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "*"):
			cur = &Table{Chains: map[string]bool{}, Rules: map[string]bool{}, Order: map[string][]string{}}
			t[line[1:]] = cur
		case cur == nil:
		case strings.HasPrefix(line, ":"):
			if f := strings.Fields(line[1:]); len(f) > 0 {
				cur.Chains[f[0]] = true
			}
		case strings.HasPrefix(line, "-A "):
			cur.Rules[line] = true
			if f := strings.Fields(line); len(f) > 1 {
				cur.Order[f[1]] = append(cur.Order[f[1]], line)
			}
		}
	}
	return t
}

func (t Tables) Has(table, rule string) bool {
	tb := t[table]
	return tb != nil && tb.Rules[rule]
}

func (t Tables) HasChain(table, chain string) bool {
	tb := t[table]
	return tb != nil && tb.Chains[chain]
}

func (t Tables) First(table, chain string) string {
	tb := t[table]
	if tb == nil || len(tb.Order[chain]) == 0 {
		return ""
	}
	return tb.Order[chain][0]
}

func (t Tables) InternalBridges() []string {
	tb := t["filter"]
	if tb == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, rule := range tb.Order[InternalChain] {
		f := strings.Fields(rule)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "-o" && strings.HasSuffix(rule, "-j DROP") && !seen[f[i+1]] {
				seen[f[i+1]] = true
				out = append(out, f[i+1])
			}
		}
	}
	return out
}

const (
	UserChain     = "DOCKER-USER"
	InternalChain = "DOCKER-INTERNAL"
)

func declare(b *strings.Builder, now Tables, table, chain string) {
	if !now.HasChain(table, chain) {
		b.WriteString(":" + chain + " - [0:0]\n")
	}
}
