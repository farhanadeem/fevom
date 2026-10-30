package debugremote

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const contextsFile = "debug-contexts.json"

type contextsRecord struct {
	Told map[string]string `json:"told"`
}

func (o *Options) notice(info Info) {
	name := o.Ctx.Name
	if name == "" {
		name = "default"
	}
	if o.NoticeDir != "" && o.toldBefore(name) {
		return
	}
	o.errf("context %q is a Docker engine you can already reach; fev loads a labelled helper image and a %q volume, runs at least privilege (no --privileged, no host PID namespace), and removes everything else. `fev debug --reset -c %s` removes them.",
		name, StoreVolume, name)
	if o.NoticeDir != "" {
		o.recordTold(name)
	}
}

func (o *Options) recordPath() string { return filepath.Join(o.NoticeDir, contextsFile) }

func (o *Options) toldBefore(name string) bool {
	rec := o.readContexts()
	_, ok := rec.Told[name]
	return ok
}

func (o *Options) readContexts() contextsRecord {
	rec := contextsRecord{Told: map[string]string{}}
	data, err := os.ReadFile(o.recordPath())
	if err == nil {
		json.Unmarshal(data, &rec)
		if rec.Told == nil {
			rec.Told = map[string]string{}
		}
	}
	return rec
}

func (o *Options) recordTold(name string) {
	rec := o.readContexts()
	rec.Told[name] = time.Now().UTC().Format(time.RFC3339)
	data, _ := json.MarshalIndent(rec, "", "\t")
	if err := os.MkdirAll(o.NoticeDir, 0o700); err != nil {
		return
	}
	tmp := o.recordPath() + ".tmp"
	if os.WriteFile(tmp, append(data, '\n'), 0o600) == nil {
		os.Rename(tmp, o.recordPath())
	}
}

func (o *Options) forgetContext(name string) {
	if o.NoticeDir == "" {
		return
	}
	rec := o.readContexts()
	if _, ok := rec.Told[name]; !ok {
		return
	}
	delete(rec.Told, name)
	data, _ := json.MarshalIndent(rec, "", "\t")
	tmp := o.recordPath() + ".tmp"
	if os.WriteFile(tmp, append(data, '\n'), 0o600) == nil {
		os.Rename(tmp, o.recordPath())
	}
}

var _ = fmt.Sprintf
