package report

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"fev/host/daemon/doctor"
	"fev/host/daemon/home"
	api "fev/proto/daemon"
	"fev/proto/version"
)

type Options struct {
	Layout      home.Layout
	Client      *api.Client
	Doctor      doctor.Report
	Now         time.Time
	CallTimeout time.Duration
}

type Written struct {
	Path    string   `json:"path"`
	Bytes   int64    `json:"bytes"`
	Files   []string `json:"files"`
	Skipped []string `json:"skipped,omitempty"`
}

func DefaultName(now time.Time) string {
	return "fev-report-" + now.Format("20060102-150405") + ".zip"
}

func Write(ctx context.Context, path string, o Options) (*Written, error) {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = 10 * time.Second
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("%s exists; name another file", path)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fev-report-*.zip")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	w := &writer{zip: zip.NewWriter(tmp), now: o.Now}
	w.build(ctx, o)
	if err := w.zip.Close(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return &Written{Path: path, Bytes: info.Size(), Files: w.files, Skipped: w.skipped}, nil
}

type writer struct {
	zip     *zip.Writer
	now     time.Time
	files   []string
	skipped []string
}

func (w *writer) add(name string, data []byte) {
	h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: w.now}
	h.SetMode(0o600)
	f, err := w.zip.CreateHeader(h)
	if err == nil {
		_, err = f.Write(data)
	}
	if err != nil {
		w.skip(name, err)
		return
	}
	w.files = append(w.files, name)
}

func (w *writer) skip(name string, err error) {
	w.skipped = append(w.skipped, name+": "+err.Error())
}

func (w *writer) addJSON(name string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		w.skip(name, err)
		return
	}
	w.add(name, append(data, '\n'))
}

func (w *writer) addFile(name, path string) {
	src, err := os.Open(path)
	if err != nil {
		w.skip(name, err)
		return
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		w.skip(name, err)
		return
	}
	h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: info.ModTime()}
	h.SetMode(0o600)
	dst, err := w.zip.CreateHeader(h)
	if err == nil {
		_, err = io.Copy(dst, src)
	}
	if err != nil {
		w.skip(name, err)
		return
	}
	w.files = append(w.files, name)
}

func (w *writer) build(ctx context.Context, o Options) {
	l := o.Layout
	w.add("summary.txt", summary(l, o.Now))
	w.addJSON("doctor.json", o.Doctor)
	if o.Client != nil {
		w.daemon(ctx, o)
	}
	w.add("home.txt", listing(l))
	if data, err := os.ReadFile(l.ConfigFile()); err == nil {
		w.add("config/config.json", Redact(data))
	} else if !os.IsNotExist(err) {
		w.skip("config/config.json", err)
	}
	if _, err := os.Stat(filepath.Join(l.Home, "setup.json")); err == nil {
		w.addFile("setup.json", filepath.Join(l.Home, "setup.json"))
	}
	entries, err := os.ReadDir(l.LogDir())
	if err != nil && !os.IsNotExist(err) {
		w.skip("log/", err)
	}
	for _, ent := range entries {
		if ent.Type().IsRegular() {
			w.addFile("log/"+ent.Name(), filepath.Join(l.LogDir(), ent.Name()))
		}
	}
	w.add("README.txt", w.readme(o))
}

func (w *writer) daemon(ctx context.Context, o Options) {
	c := o.Client
	call := func(name string, fn func(context.Context) (any, error)) {
		cctx, cancel := context.WithTimeout(ctx, o.CallTimeout)
		defer cancel()
		v, err := fn(cctx)
		if err != nil {
			w.skip(name, err)
			return
		}
		w.addJSON(name, v)
	}
	call("daemon/status.json", func(ctx context.Context) (any, error) { return c.Status(ctx) })
	call("daemon/docker.status.json", func(ctx context.Context) (any, error) { return c.DockerStatus(ctx) })
	call("daemon/files.status.json", func(ctx context.Context) (any, error) { return c.FilesStatus(ctx) })
	call("daemon/forwards.list.json", func(ctx context.Context) (any, error) { return c.Forwards(ctx) })
	call("daemon/machines.list.json", func(ctx context.Context) (any, error) { return c.Machines(ctx) })
	call("daemon/boot.trace.json", func(ctx context.Context) (any, error) { return c.BootTrace(ctx) })
	call("daemon/config.list.json", func(ctx context.Context) (any, error) {
		values, err := c.ConfigList(ctx)
		if err != nil {
			return nil, err
		}
		data, _ := json.Marshal(values)
		var back any
		json.Unmarshal(Redact(data), &back)
		return back, nil
	})
	call("daemon/storage.usage.json", func(ctx context.Context) (any, error) { return c.StorageUsage(ctx, api.UsageParams{}) })
	call("daemon/stats.sample.json", func(ctx context.Context) (any, error) { return oneSample(ctx, c) })
}

func oneSample(ctx context.Context, c *api.Client) (*api.StatsSample, error) {
	got := make(chan api.StatsSample, 1)
	stop, err := c.SubscribeStats(ctx, 1000, func(s api.StatsSample) {
		select {
		case got <- s:
		default:
		}
	})
	if err != nil {
		return nil, err
	}
	defer stop()
	select {
	case s := <-got:
		return &s, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("no stats sample: %w", ctx.Err())
	}
}

func summary(l home.Layout, now time.Time) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "fev diagnostic report, %s\n\n", now.Format(time.RFC3339))
	fmt.Fprintf(&b, "build: %s\n", version.Build)
	fmt.Fprintf(&b, "home: %s\n", l.Home)
	fmt.Fprintf(&b, "data: %s\n", l.DataDir)
	if out, err := exec.Command("/usr/bin/sw_vers").Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			k, v, _ := strings.Cut(line, ":")
			fmt.Fprintf(&b, "macos %s: %s\n", strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v))
		}
	}
	if model, err := unix.Sysctl("hw.model"); err == nil {
		fmt.Fprintf(&b, "model: %s\n", model)
	}
	if mem, err := unix.SysctlUint64("hw.memsize"); err == nil {
		fmt.Fprintf(&b, "memory: %d MiB\n", mem>>20)
	}
	if n, err := unix.SysctlUint32("hw.ncpu"); err == nil {
		fmt.Fprintf(&b, "processors: %d\n", n)
	}
	fmt.Fprintf(&b, "arch: %s\n", runtime.GOARCH)
	for _, dir := range []string{l.Home, l.DataDir} {
		var st unix.Statfs_t
		if unix.Statfs(dir, &st) == nil {
			fmt.Fprintf(&b, "free on %s's volume: %d MiB of %d MiB\n", dir, st.Bavail*uint64(st.Bsize)>>20, st.Blocks*uint64(st.Bsize)>>20)
		}
	}
	return b.Bytes()
}

var leftOut = map[string]string{
	"data":  "the data image and what is beside it",
	"https": "fev's CA and keys",
	"ssh":   "fev's SSH keys and configuration",
}

func listing(l home.Layout) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s\n", l.Home)
	filepath.WalkDir(l.Home, func(path string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(l.Home, path)
		if rel == "." {
			return nil
		}
		if err != nil {
			fmt.Fprintf(&b, "%s: %v\n", rel, err)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		line := fmt.Sprintf("%s %12d %s %s", info.Mode(), info.Size(), info.ModTime().Format(time.RFC3339), rel)
		if d.Type()&fs.ModeSymlink != 0 {
			if target, err := os.Readlink(path); err == nil {
				line += " -> " + target
			}
		}
		b.WriteString(line + "\n")
		if why, ok := leftOut[rel]; ok && d.IsDir() {
			fmt.Fprintf(&b, "  (not listed: %s)\n", why)
			return filepath.SkipDir
		}
		return nil
	})
	if l.DataDir != "" && !strings.HasPrefix(l.DataDir, l.Home+"/") {
		if info, err := os.Stat(l.DataImage()); err == nil {
			var st unix.Stat_t
			allocated := int64(-1)
			if unix.Stat(l.DataImage(), &st) == nil {
				allocated = st.Blocks * 512
			}
			fmt.Fprintf(&b, "\n%s: %d bytes long, %d allocated\n", l.DataImage(), info.Size(), allocated)
		}
	}
	return b.Bytes()
}

var userinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s"]+@`)

func Redact(data []byte) []byte {
	return userinfo.ReplaceAllFunc(data, func(m []byte) []byte {
		scheme := m[:bytes.Index(m, []byte("://"))+3]
		return append(append([]byte{}, scheme...), "REDACTED@"...)
	})
}

func (w *writer) readme(o Options) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, `fev's diagnostic report, made %s by "fev report".

It was written on this Mac and sent nowhere. Read it before you attach it to
a bug report: the logs name containers, machines, images and Mac paths.

Left out: the data image and everything under data/, fev's CA and keys
(https/), its SSH keys (ssh/), the run directory's sockets and files, and
your environment. Credentials in URLs in the configuration (a proxy's
user:password@) are replaced by REDACTED.

`, o.Now.Format(time.RFC3339))
	if o.Client == nil {
		b.WriteString("No daemon answered when it was made: daemon/ is not here.\n\n")
	}
	files := append([]string{}, w.files...)
	sort.Strings(files)
	b.WriteString("In it:\n")
	for _, f := range files {
		fmt.Fprintf(&b, "  %s\n", f)
	}
	if len(w.skipped) > 0 {
		b.WriteString("\nCould not be included:\n")
		for _, s := range w.skipped {
			fmt.Fprintf(&b, "  %s\n", s)
		}
	}
	return b.Bytes()
}
