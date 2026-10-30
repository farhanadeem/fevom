package storage

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	api "fev/proto/agent"
)

type RestoreConfig struct {
	Device, ToolDir string
	Start           func(path string, args []string, out *os.File) (wait func() error, kill func(), err error)
	Logf            func(format string, args ...any)
}

const restoreDir = ".fev-restore-"

func Restore(ctx context.Context, cfg RestoreConfig, r SalvageRequest) (api.Salvaged, error) {
	if err := r.check(); err != nil {
		return api.Salvaged{}, err
	}
	p, err := probePath(cfg.Device)
	if err != nil {
		return api.Salvaged{}, fmt.Errorf("salvage: read %s: %w", cfg.Device, err)
	}
	switch {
	case p.Kind == Btrfs && (p.PrimaryValid || p.MirrorValid):
	case p.Kind == Foreign:
		return api.Salvaged{}, fmt.Errorf("salvage: %s holds %s, not a fev data filesystem; there is nothing of fev's to salvage", cfg.Device, p.What)
	default:
		return api.Salvaged{}, fmt.Errorf("salvage: %s has no valid btrfs superblock in any of its copies, "+
			"and the tool that reads a filesystem without the kernel needs one; nothing was read or changed", cfg.Device)
	}
	tool := filepath.Join(cfg.ToolDir, "btrfs")
	if cfg.ToolDir == "" || !isFile(tool) {
		return api.Salvaged{}, fmt.Errorf("salvage: %v", ErrNoTool)
	}
	var moves []salvageItem
	if !r.all() {
		for _, v := range r.Volumes {
			moves = append(moves, salvageItem{src: filepath.Join(dockerVolumes, v, volumeData), dst: filepath.Join(r.Dest, SalvageVolumesDir, v)})
		}
		for _, m := range r.Machines {
			moves = append(moves, salvageItem{src: filepath.Join(MachinesSubvolume, m), dst: filepath.Join(r.Dest, SalvageMachinesDir, m)})
		}
		if r.Docker {
			moves = append(moves, salvageItem{src: "@docker", dst: filepath.Join(r.Dest, SalvageDockerDir)})
		}
		if err := prepareDest(r.Dest, moves); err != nil {
			return api.Salvaged{}, err
		}
	} else if err := prepareDest(r.Dest, []salvageItem{
		{dst: filepath.Join(r.Dest, SalvageVolumesDir)}, {dst: filepath.Join(r.Dest, SalvageMachinesDir)},
	}); err != nil {
		return api.Salvaged{}, err
	}
	var tag [6]byte
	rand.Read(tag[:])
	staging := filepath.Join(r.Dest, restoreDir+hex.EncodeToString(tag[:]))
	if err := os.Mkdir(staging, 0o700); err != nil {
		return api.Salvaged{}, destErr{err}
	}
	if r.Progress != nil {
		r.Progress(api.PhaseRestore, 0, 0)
	}
	args := []string{"restore", "-x", "-S", "-i", "--path-regex", restoreRegex(r), cfg.Device, staging}
	if cfg.Logf != nil {
		cfg.Logf("storage: salvage: nothing mounts; btrfs %s", strings.Join(args, " "))
	}
	out, errs, runErr := runTool(ctx, cfg, tool, args)
	if ctx.Err() != nil {
		os.RemoveAll(staging)
		return api.Salvaged{}, ctx.Err()
	}
	if r.all() {
		moves = nil
		if entries, err := os.ReadDir(filepath.Join(staging, dockerVolumes)); err == nil {
			for _, e := range entries {
				moves = append(moves, salvageItem{src: filepath.Join(dockerVolumes, e.Name(), volumeData), dst: filepath.Join(r.Dest, SalvageVolumesDir, e.Name())})
			}
		}
		if entries, err := os.ReadDir(filepath.Join(staging, MachinesSubvolume)); err == nil {
			for _, e := range entries {
				if api.CheckMachineName(e.Name()) == nil {
					moves = append(moves, salvageItem{src: filepath.Join(MachinesSubvolume, e.Name()), dst: filepath.Join(r.Dest, SalvageMachinesDir, e.Name())})
				}
			}
		}
	}
	res := api.Salvaged{Method: api.SalvageRestore}
	for _, mv := range moves {
		from := filepath.Join(staging, mv.src)
		if _, err := os.Lstat(from); err != nil {
			errs.add("%s: not found by the tool", "/"+mv.src)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(mv.dst), 0o755); err != nil {
			os.RemoveAll(staging)
			return api.Salvaged{}, destErr{err}
		}
		if err := os.Rename(from, mv.dst); err != nil {
			os.RemoveAll(staging)
			return api.Salvaged{}, destErr{err}
		}
		files, bytes := countTree(mv.dst)
		res.Files += files
		res.Bytes += bytes
	}
	os.RemoveAll(staging)
	res.Errors, res.Lost = errs.lines, errs.n
	if res.Files == 0 && runErr != nil {
		return res, fmt.Errorf("salvage: btrfs restore read nothing: %v: %s", runErr, lastLine(out))
	}
	return res, nil
}

func restoreRegex(r SalvageRequest) string {
	alt := func(names []string) string {
		q := make([]string, len(names))
		for i, n := range names {
			q[i] = regexp.QuoteMeta(n)
		}
		return "(" + strings.Join(q, "|") + ")"
	}
	var top []string
	switch {
	case r.all():
		top = append(top, `@docker(|/volumes(|/[^/]+(|/_data(|/.*))))`, `@machines(|/[a-zA-Z][^/]*(|/.*))`)
	default:
		var docker []string
		if r.Docker {
			docker = append(docker, `/.*`)
		} else if len(r.Volumes) > 0 {
			docker = append(docker, `/volumes(|/`+alt(r.Volumes)+`(|/_data(|/.*)))`)
		}
		if len(docker) > 0 {
			top = append(top, `@docker(|`+strings.Join(docker, "|")+`)`)
		}
		if len(r.Machines) > 0 {
			top = append(top, `@machines(|/`+alt(r.Machines)+`(|/.*))`)
		}
	}
	return `^/(|` + strings.Join(top, "|") + `)$`
}

func runTool(ctx context.Context, cfg RestoreConfig, tool string, args []string) (string, *errorList, error) {
	errs := &errorList{}
	pr, pw, err := os.Pipe()
	if err != nil {
		return "", errs, err
	}
	defer pr.Close()
	wait, kill, err := cfg.Start(tool, args, pw)
	pw.Close()
	if err != nil {
		return "", errs, err
	}
	stop := context.AfterFunc(ctx, kill)
	defer stop()
	var all strings.Builder
	var lost int64
	sc := bufio.NewScanner(io.LimitReader(pr, 64<<20))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if all.Len() < 1<<20 {
			all.WriteString(line + "\n")
		}
		if restoreTrouble(line) {
			errs.add("%s", strings.TrimSpace(line))
		}
		if restoreTrouble(line) && strings.Contains(line, "/") {
			lost++
		}
	}
	io.Copy(io.Discard, pr)
	errs.n = lost
	return all.String(), errs, wait()
}

func restoreTrouble(line string) bool {
	l := strings.ToLower(line)
	for _, w := range []string{"error", "could not", "couldn't", "cannot", "failed", "mismatch", "corrupt", "bad tree block"} {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}

func countTree(root string) (files, bytes int64) {
	filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		switch {
		case d.Type().IsRegular():
			files++
			if info, err := d.Info(); err == nil {
				bytes += info.Size()
			}
		case d.Type()&os.ModeSymlink != 0:
			files++
		}
		return nil
	})
	return files, bytes
}
