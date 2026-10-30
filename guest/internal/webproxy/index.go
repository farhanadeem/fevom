package webproxy

import (
	"html/template"
	"net/http"
	"sort"
	"strings"

	api "fev/proto/agent"
)

var indexPage = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>fev</title>
<style>
:root{color-scheme:light dark;--fg:#1d1d1f;--muted:#6e6e73;--bg:#fff;--line:#e5e5ea;--link:#0a64d8}
@media (prefers-color-scheme:dark){:root{--fg:#f5f5f7;--muted:#a1a1a6;--bg:#1c1c1e;--line:#38383a;--link:#4ea1ff}}
body{font:15px/1.45 -apple-system,system-ui,sans-serif;margin:0 auto;max-width:860px;padding:24px 16px;color:var(--fg);background:var(--bg)}
h1{font-size:22px;margin:0 0 4px}p{color:var(--muted);margin:0 0 20px}
h2{font-size:13px;text-transform:uppercase;letter-spacing:.04em;color:var(--muted);margin:24px 0 8px}
ul{list-style:none;padding:0;margin:0}li{padding:10px 0;border-top:1px solid var(--line);overflow-wrap:anywhere}
b{font-weight:600}a{color:var(--link);text-decoration:none;margin-right:12px}a:hover{text-decoration:underline}
</style></head><body>
<h1>fev</h1><p>{{len .Containers}} containers and {{len .Machines}} machines with names.</p>
{{if .Containers}}<h2>Containers</h2><ul>{{range .Containers}}<li><b>{{.Name}}</b><br>{{range .Links}}<a href="{{.}}">{{.}}</a>{{end}}</li>{{end}}</ul>{{end}}
{{if .Machines}}<h2>Machines</h2><ul>{{range .Machines}}<li><b>{{.Name}}</b><br>{{range .Links}}<a href="{{.}}">{{.}}</a>{{end}}</li>{{end}}</ul>{{end}}
</body></html>
`))

type indexRow struct {
	Name  string
	Links []string
}

func (p *Proxy) index(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	set := p.set
	p.mu.Unlock()
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	var data struct{ Containers, Machines []indexRow }
	for _, t := range set.Targets {
		row := indexRow{Name: t.Name}
		for _, n := range t.Names {
			if !strings.HasPrefix(n, "*.") {
				row.Links = append(row.Links, scheme+"://"+n+"/")
			}
		}
		if t.Kind == api.TargetMachine {
			data.Machines = append(data.Machines, row)
		} else {
			data.Containers = append(data.Containers, row)
		}
	}
	for _, rows := range [][]indexRow{data.Containers, data.Machines} {
		sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	indexPage.Execute(w, data)
}
