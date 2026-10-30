package machines

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	api "fev/proto/agent"

	"fev/guest/internal/logbuf"
)

const trustToolBound = 2 * time.Minute

const (
	stageTrustInstalled = 't'
	stageTrustFailed    = 'T'
)

func (rt *Runtime) trustFile() string { return filepath.Join(rt.o.RunDir, "trusted-roots.pem") }

func (rt *Runtime) TrustSet(ders [][]byte) (api.Trusted, error) {
	for i, d := range ders {
		if _, err := x509.ParseCertificate(d); err != nil {
			return api.Trusted{}, fmt.Errorf("root %d is not a certificate: %w", i, err)
		}
	}
	set := append([][]byte(nil), ders...)
	slices.SortFunc(set, bytes.Compare)
	set = slices.CompactFunc(set, bytes.Equal)
	var b bytes.Buffer
	for _, d := range set {
		pem.Encode(&b, &pem.Block{Type: "CERTIFICATE", Bytes: d})
	}
	if err := os.MkdirAll(rt.o.RunDir, 0o711); err != nil {
		return api.Trusted{}, err
	}
	tmp := rt.trustFile() + ".new"
	if err := os.WriteFile(tmp, b.Bytes(), 0o644); err != nil {
		return api.Trusted{}, err
	}
	if err := os.Rename(tmp, rt.trustFile()); err != nil {
		os.Remove(tmp)
		return api.Trusted{}, err
	}
	t := api.Trusted{Count: len(set), Digest: api.TrustDigest(set)}
	rt.mu.Lock()
	rt.trusted = &t
	rt.mu.Unlock()
	return t, nil
}

func (rt *Runtime) trustForStage() (string, *api.Trusted) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.trusted == nil {
		return "", nil
	}
	t := *rt.trusted
	return rt.trustFile(), &t
}

func (r *run) trustSaid(said string) {
	_, t := r.m.rt.trustForStage()
	switch {
	case strings.ContainsRune(said, stageTrustInstalled) && t != nil:
		r.m.runtimeLogf(logbuf.Info, "the Mac's trusted roots installed: %d (%.12s), the distribution's tool run", t.Count, t.Digest)
	case strings.ContainsRune(said, stageTrustFailed):
		r.m.runtimeLogf(logbuf.Warning, "the Mac's trusted roots were not installed; its console says why (tried again at the next start)")
	}
}

func readTrust(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 2*api.MaxTrustBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 2*api.MaxTrustBytes {
		return nil, errors.New("the set of roots is too large")
	}
	return parseTrustPEM(b), nil
}

func installTrust(root string, ders [][]byte) (ran bool, err error) {
	at := func(p string) string { return root + p }
	want := api.TrustDigest(ders)
	last := ""
	if b, err := os.ReadFile(at(TrustedFile)); err == nil {
		last = strings.TrimSpace(string(b))
	}
	if last == want {
		return false, nil
	}
	has := func(p string) bool { _, err := os.Stat(at(p)); return err == nil }
	tool, ok := chooseTrustTool(has)
	var present []string
	if entries, err := os.ReadDir(at(tool.dir)); err == nil {
		for _, e := range entries {
			present = append(present, e.Name())
		}
	}
	plan := planTrust(ders, tool.ext, present)
	if last == "" && len(plan.write) == 0 && len(plan.remove) == 0 {
		return false, nil
	}
	for _, name := range plan.remove {
		if err := os.Remove(at(filepath.Join(tool.dir, name))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	if len(plan.write) > 0 {
		if err := os.MkdirAll(at(tool.dir), 0o755); err != nil {
			return false, err
		}
	}
	for name, b := range plan.write {
		if err := replaceFile(at(filepath.Join(tool.dir, name)), b, 0o644); err != nil {
			return false, err
		}
	}
	if !ok {
		return false, fmt.Errorf("the machine has neither update-ca-certificates nor update-ca-trust; the roots are in %s for when it has", tool.dir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), trustToolBound)
	defer cancel()
	cmd := exec.CommandContext(ctx, at(tool.argv[0]), tool.argv[1:]...)
	cmd.Env = []string{"PATH=" + machinePath, "LANG=C"}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("%s: %w", tool.argv[0], err)
	}
	if err := os.MkdirAll(at(filepath.Dir(TrustedFile)), 0o755); err != nil {
		return true, err
	}
	return true, replaceFile(at(TrustedFile), []byte(want+"\n"), 0o644)
}

func replaceFile(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".fev-new"
	os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
