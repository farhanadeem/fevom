package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
	"golang.org/x/sys/unix"

	"fev/proto/debugstore"
)

type Store struct {
	root      string
	cacheURL  string
	indexURL  string
	indexFile string
	pinsDir   string
	key       key
	http      *http.Client
	j         int

	warn    func(string)
	backoff func(attempt int) time.Duration

	mu    sync.Mutex
	paths map[string]struct{}

	idxMu sync.Mutex
	idx   *Index
}

const (
	subStore    = "store"
	subDB       = "db"
	subProfiles = "profiles"
	subIndex    = "index"
	subHomes    = "homes"
	subPins     = "pins"
	subTmp      = "tmp"
	defaultProf = "default"

	defaultSetFile = "default-set"
	defaultIDFile  = "default-set.id"
)

func Open(root, cacheURL string) (*Store, error) {
	if cacheURL == "" {
		cacheURL = debugstore.CacheURL
	}
	k, err := parseKey(debugstore.CacheKey)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{subStore, subDB, filepath.Join(subProfiles, defaultProf), subIndex, subHomes, subPins, subTmp} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 16
	tr.ForceAttemptHTTP2 = true
	return &Store{
		root:     root,
		cacheURL: strings.TrimRight(cacheURL, "/"),
		pinsDir:  filepath.Join(root, subPins),
		key:      k,
		http:     &http.Client{Transport: tr, Timeout: 10 * time.Minute},
		j:        16,
		warn:     func(s string) { fmt.Fprintln(os.Stderr, s) },
		backoff:  defaultBackoff,
		paths:    map[string]struct{}{},
	}, nil
}

const fetchAttempts = 5

func defaultBackoff(attempt int) time.Duration {
	d := 200 * time.Millisecond
	for i := 1; i < attempt; i++ {
		d *= 2
	}
	return d
}

func (s *Store) warnf(format string, a ...any) {
	if s.warn != nil {
		s.warn(fmt.Sprintf(format, a...))
	}
}

func transientStatus(code int) bool {
	switch code {
	case 429, 500, 502, 503, 504:
		return true
	}
	return false
}

func transientNetErr(err error) bool {
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func (s *Store) SetPinsDir(dir string) { s.pinsDir = dir }

func (s *Store) lockRoot() (func(), error) {
	s.mu.Lock()
	f, err := os.OpenFile(filepath.Join(s.root, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_CLOEXEC, 0o644)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		s.mu.Unlock()
		return nil, fmt.Errorf("the store's lock: %w", err)
	}
	return func() {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
		s.mu.Unlock()
	}, nil
}

func (s *Store) clearTmp() {
	tmp := s.dir(subTmp)
	ents, _ := os.ReadDir(tmp)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			removeStoreTree(filepath.Join(tmp, e.Name()))
		}
	}
}

func (s *Store) dir(sub string) string     { return filepath.Join(s.root, sub) }
func (s *Store) storePath(b string) string { return filepath.Join(s.root, subStore, b) }
func (s *Store) dbPath(h string) string    { return filepath.Join(s.root, subDB, h+".narinfo") }
func (s *Store) profile() string           { return filepath.Join(s.root, subProfiles, defaultProf) }

func (s *Store) Root() string { return s.root }

func (s *Store) get(ctx context.Context, u string) (*http.Response, error) {
	var last error
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := s.http.Do(req)
		switch {
		case err == nil && resp.StatusCode == 200:
			return resp, nil
		case err == nil:
			resp.Body.Close()
			last = fmt.Errorf("GET %s: %s", u, resp.Status)
			if !transientStatus(resp.StatusCode) {
				return nil, last
			}
		default:
			last = err
			if !transientNetErr(err) {
				return nil, err
			}
		}
		if attempt >= fetchAttempts {
			return nil, fmt.Errorf("GET %s: giving up after %d attempts: %w", u, fetchAttempts, last)
		}
		d := s.backoff(attempt)
		s.warnf("fev: debug tool cache: %s (attempt %d/%d), retrying in %s", shortErr(last), attempt, fetchAttempts, d)
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

func (s *Store) narinfo(ctx context.Context, h string) (*narinfo, error) {
	resp, err := s.get(ctx, s.cacheURL+"/"+h+".narinfo")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	n, err := parseNarinfo(string(b))
	if err != nil {
		return nil, fmt.Errorf("%s.narinfo: %w", h, err)
	}
	if hashOf(base(n.StorePath)) != h {
		return nil, fmt.Errorf("%s.narinfo names %s", h, n.StorePath)
	}
	if err := n.verify(s.key); err != nil {
		return nil, err
	}
	return n, nil
}

type counter struct {
	r io.Reader
	n *atomic.Int64
}

func (c counter) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	c.n.Add(int64(k))
	return k, err
}

func (s *Store) fetch(ctx context.Context, n *narinfo, wire *atomic.Int64) (unpackStats, error) {
	var st unpackStats
	if n.NarSize > MaxNarBytes {
		return st, fmt.Errorf("%s: NAR of %d bytes over the limit", n.StorePath, n.NarSize)
	}
	b := base(n.StorePath)
	final := s.storePath(b)
	tmpName := ".tmp-" + b
	tmpFull := filepath.Join(s.dir(subTmp), tmpName)
	if err := removeStoreTree(tmpFull); err != nil {
		return st, err
	}
	tmpFd, err := unix.Open(s.dir(subTmp), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return st, err
	}
	defer unix.Close(tmpFd)
	resp, err := s.get(ctx, s.cacheURL+"/"+n.URL)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	var body io.Reader = counter{resp.Body, wire}
	switch n.Compression {
	case "zstd":
		d, err := zstd.NewReader(body, zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxWindow(maxDecoderWindow), zstd.WithDecoderMaxMemory(maxDecoderMemory))
		if err != nil {
			return st, err
		}
		defer d.Close()
		body = d
	case "xz":
		d, err := xz.ReaderConfig{DictCap: maxDecoderWindow}.NewReader(body)
		if err != nil {
			return st, err
		}
		body = d
	case "none", "":
	default:
		return st, fmt.Errorf("%s: compression %q", n.StorePath, n.Compression)
	}
	lim := &sizeLimit{r: body, left: n.NarSize + 1}
	h := sha256.New()
	tee := io.TeeReader(lim, h)
	st, err = unpackNAR(tee, tmpFd, tmpName, n.NarSize)
	if err == nil {
		var one [1]byte
		if k, _ := io.ReadFull(tee, one[:]); k > 0 {
			err = fmt.Errorf("the NAR is longer than its NarSize (%d bytes)", n.NarSize)
		}
	}
	if err != nil {
		if lim.over {
			err = fmt.Errorf("the NAR is longer than its NarSize (%d bytes)", n.NarSize)
		}
		removeStoreTree(tmpFull)
		return st, fmt.Errorf("%s: %w", n.StorePath, err)
	}
	if got := "sha256:" + nixBase32(h.Sum(nil)); got != n.NarHash {
		removeStoreTree(tmpFull)
		return st, fmt.Errorf("%s: NAR hash %s, narinfo says %s", n.StorePath, got, n.NarHash)
	}
	if err := os.Rename(tmpFull, final); err != nil {
		return st, err
	}
	sealTree(final)
	return st, nil
}

const (
	maxDecoderWindow = 64 << 20
	maxDecoderMemory = 256 << 20
)

type sizeLimit struct {
	r    io.Reader
	left int64
	over bool
}

func (l *sizeLimit) Read(p []byte) (int, error) {
	if l.left <= 0 {
		l.over = true
		return 0, io.EOF
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	k, err := l.r.Read(p)
	l.left -= int64(k)
	if l.left <= 0 && k > 0 {
		l.over = true
	}
	return k, err
}

var epochTime = time.Unix(1, 0)

func sealTree(p string) {
	var dirs []string
	filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, q)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Chtimes(dirs[i], epochTime, epochTime)
		os.Chmod(dirs[i], 0o555)
	}
}

var removeHook func(string)

func removeStoreTree(p string) error {
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if removeHook != nil {
		removeHook(p)
	}
	filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(q, 0o755)
		}
		return nil
	})
	return os.RemoveAll(p)
}

func (s *Store) Install(ctx context.Context, names []string, progress func(debugstore.Progress)) (*debugstore.Installed, error) {
	unlock, err := s.lockRoot()
	if err != nil {
		return nil, err
	}
	defer unlock()
	s.clearTmp()
	roots, err := s.resolveNames(ctx, names)
	if err != nil {
		return nil, err
	}
	roots, err = s.fetchClosure(ctx, roots, progress)
	if err != nil {
		return nil, err
	}
	if err := s.profileAdd(roots); err != nil {
		return nil, err
	}
	return s.installed(roots), nil
}

func (s *Store) installed(roots []string) *debugstore.Installed {
	out := &debugstore.Installed{}
	seen := map[string]bool{}
	dflt := s.defaultRoots()
	for _, r := range roots {
		pkg := s.packageOf(r, dflt[StoreDir+"/"+r])
		out.Installed = append(out.Installed, pkg)
		for _, c := range pkg.Commands {
			if !seen[c] {
				seen[c] = true
				out.Commands = append(out.Commands, c)
			}
		}
	}
	sort.Strings(out.Commands)
	return out
}

func (s *Store) ResolveName(ctx context.Context, name string) (string, error) {
	roots, err := s.resolveNames(ctx, []string{name})
	if err != nil {
		return "", err
	}
	return StoreDir + "/" + roots[0], nil
}

func (s *Store) resolveNames(ctx context.Context, names []string) ([]string, error) {
	var roots []string
	for _, name := range names {
		if strings.HasPrefix(name, StoreDir+"/") || looksLikeBase(name) {
			b := base(name)
			if !validBase(b) {
				return nil, fmt.Errorf("%q is not a store path", name)
			}
			roots = append(roots, b)
			continue
		}
		idx, err := s.index(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", name, err)
		}
		sp := idx.Resolve(name)
		if sp == "" || !strings.HasPrefix(sp, StoreDir+"/") || !validBase(base(sp)) {
			return nil, fmt.Errorf("no package provides %q", name)
		}
		roots = append(roots, base(sp))
	}
	return roots, nil
}

func looksLikeBase(s string) bool {
	if len(s) < 34 || s[32] != '-' {
		return false
	}
	for i := 0; i < 32; i++ {
		if !strings.ContainsRune(b32, rune(s[i])) {
			return false
		}
	}
	return true
}

func (s *Store) fetchClosure(ctx context.Context, roots []string, progress func(debugstore.Progress)) ([]string, error) {
	var (
		mu       sync.Mutex
		seen     = map[string]bool{}
		infos    = map[string]*narinfo{}
		named    = map[string]string{}
		firstErr error
		wg       sync.WaitGroup
		infoSem  = make(chan struct{}, s.j)
		narSem   = make(chan struct{}, s.j)
		wire     atomic.Int64
		totalNar atomic.Int64
		pathsTot atomic.Int64
		pathsDon atomic.Int64
	)
	report := func() {
		if progress == nil {
			return
		}
		progress(debugstore.Progress{
			Phase:      debugstore.PhaseFetch,
			DoneBytes:  wire.Load(),
			TotalBytes: totalNar.Load(),
			PathsDone:  int(pathsDon.Load()),
			PathsTotal: int(pathsTot.Load()),
		})
	}
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	var visit func(h string)
	visit = func(h string) {
		mu.Lock()
		if seen[h] {
			mu.Unlock()
			return
		}
		seen[h] = true
		if len(seen) > MaxClosurePaths {
			if firstErr == nil {
				firstErr = fmt.Errorf("closure over %d paths", MaxClosurePaths)
			}
			mu.Unlock()
			return
		}
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n := s.registered(h); n != nil {
				if _, err := os.Lstat(s.storePath(base(n.StorePath))); err == nil {
					mu.Lock()
					named[h] = base(n.StorePath)
					mu.Unlock()
					return
				}
			}
			infoSem <- struct{}{}
			n, err := s.narinfo(ctx, h)
			<-infoSem
			if err != nil {
				fail(err)
				return
			}
			mu.Lock()
			infos[h] = n
			named[h] = base(n.StorePath)
			mu.Unlock()
			pathsTot.Add(1)
			totalNar.Add(n.NarSize)
			for _, r := range n.References {
				if rh := hashOf(r); rh != h {
					visit(rh)
				}
			}
			if err := s.moveOut(base(n.StorePath)); err != nil {
				fail(err)
				return
			}
			narSem <- struct{}{}
			for try := 1; ; try++ {
				_, err = s.fetch(ctx, n, &wire)
				if err == nil || try >= fetchAttempts || strings.Contains(err.Error(), "NAR hash") || strings.Contains(err.Error(), "NarSize") {
					break
				}
				s.warnf("fev: debug tool cache: %s (attempt %d/%d), retrying", shortErr(err), try, fetchAttempts)
			}
			<-narSem
			if err != nil {
				fail(err)
				return
			}
			pathsDon.Add(1)
			report()
		}()
	}
	for _, r := range roots {
		visit(hashOf(r))
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	for h, n := range infos {
		tmp := s.dbPath(h) + ".tmp"
		if err := os.WriteFile(tmp, []byte(n.raw), 0o644); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, s.dbPath(h)); err != nil {
			return nil, err
		}
	}
	out := make([]string, len(roots))
	for i, r := range roots {
		out[i] = named[hashOf(r)]
	}
	return out, nil
}

func (s *Store) moveOut(b string) error {
	p := s.storePath(b)
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	gone := filepath.Join(s.dir(subTmp), ".tmp-gone-"+b+"-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	os.Chmod(p, 0o755)
	if err := os.Rename(p, gone); err != nil {
		return err
	}
	return removeStoreTree(gone)
}

func (s *Store) registered(h string) *narinfo {
	b, err := os.ReadFile(s.dbPath(h))
	if err != nil {
		return nil
	}
	n, err := parseNarinfo(string(b))
	if err != nil {
		return nil
	}
	return n
}

func (s *Store) packageOf(b string, dflt bool) debugstore.Package {
	p := debugstore.Package{StorePath: StoreDir + "/" + b, Name: nameOf(b), Default: dflt}
	if !validBase(b) {
		return p
	}
	ents, _ := os.ReadDir(filepath.Join(s.storePath(b), "bin"))
	for _, e := range ents {
		p.Commands = append(p.Commands, e.Name())
	}
	sort.Strings(p.Commands)
	return p
}

func nameOf(b string) string {
	if i := strings.IndexByte(b, '-'); i > 0 && i+1 < len(b) {
		return b[i+1:]
	}
	return b
}

func pnameOf(name string) string {
	for i := 0; i+1 < len(name); i++ {
		if name[i] == '-' && name[i+1] >= '0' && name[i+1] <= '9' {
			return name[:i]
		}
	}
	return name
}

func (s *Store) profileAdd(roots []string) error {
	var list []string
	have := map[string]bool{}
	for _, r := range roots {
		sp := StoreDir + "/" + r
		if !have[sp] {
			have[sp] = true
			list = append(list, sp)
		}
	}
	for _, sp := range s.manifest() {
		if !have[sp] {
			have[sp] = true
			list = append(list, sp)
		}
	}
	if err := s.writeManifest(list); err != nil {
		return err
	}
	return s.relink(list)
}

func (s *Store) writeManifest(list []string) error {
	return writeFileAtomic(filepath.Join(s.profile(), "manifest"), []byte(strings.Join(list, "\n")+"\n"))
}

func writeFileAtomic(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (s *Store) manifest() []string {
	b, err := os.ReadFile(filepath.Join(s.profile(), "manifest"))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Fields(string(b)) {
		if strings.HasPrefix(l, StoreDir+"/") && validBase(base(l)) {
			out = append(out, l)
		}
	}
	return out
}

func (s *Store) defaultRoots() map[string]bool {
	m := map[string]bool{}
	b, err := os.ReadFile(filepath.Join(s.profile(), defaultSetFile))
	if err != nil {
		return m
	}
	for _, l := range strings.Fields(string(b)) {
		m[l] = true
	}
	return m
}

func (s *Store) relink(list []string) error {
	prof := s.profile()
	for _, sub := range []string{"bin", "share"} {
		nb := filepath.Join(prof, "."+sub+".new")
		removeStoreTree(nb)
		if err := os.Mkdir(nb, 0o755); err != nil {
			return err
		}
		var srcs []string
		for _, sp := range list {
			srcs = append(srcs, filepath.Join(sp, sub))
		}
		if sub == "bin" {
			for _, sp := range list {
				srcs = append(srcs, filepath.Join(sp, "sbin"))
			}
			if err := s.linkFlat(nb, srcs); err != nil {
				return err
			}
		} else if err := s.linkMerged(nb, srcs, 0); err != nil {
			return err
		}
		old := filepath.Join(prof, "."+sub+".old")
		removeStoreTree(old)
		os.Rename(filepath.Join(prof, sub), old)
		if err := os.Rename(nb, filepath.Join(prof, sub)); err != nil {
			return err
		}
		if err := removeStoreTree(old); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) real(storePath string) string {
	rest := strings.TrimPrefix(storePath, StoreDir+"/")
	return filepath.Join(s.root, subStore, rest)
}

func (s *Store) linkFlat(dst string, srcs []string) error {
	for _, src := range srcs {
		ents, _ := os.ReadDir(s.real(src))
		for _, e := range ents {
			l := filepath.Join(dst, e.Name())
			if _, err := os.Lstat(l); err == nil {
				continue
			}
			if err := os.Symlink(filepath.Join(src, e.Name()), l); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) linkMerged(dst string, srcs []string, depth int) error {
	byName := map[string][]string{}
	var order []string
	for _, src := range srcs {
		ents, _ := os.ReadDir(s.real(src))
		for _, e := range ents {
			if _, ok := byName[e.Name()]; !ok {
				order = append(order, e.Name())
			}
			byName[e.Name()] = append(byName[e.Name()], filepath.Join(src, e.Name()))
		}
	}
	for _, name := range order {
		cands := byName[name]
		var dirs []string
		for _, c := range cands {
			if fi, err := os.Stat(s.real(c)); err == nil && fi.IsDir() {
				dirs = append(dirs, c)
			}
		}
		if len(dirs) > 1 && depth < 16 && dirs[0] == cands[0] {
			d := filepath.Join(dst, name)
			if err := os.Mkdir(d, 0o755); err != nil {
				return err
			}
			if err := s.linkMerged(d, dirs, depth+1); err != nil {
				return err
			}
			continue
		}
		if err := os.Symlink(cands[0], filepath.Join(dst, name)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) LinkCABundle(etc string) error {
	for _, sp := range s.manifest() {
		rel := "etc/ssl/certs/ca-bundle.crt"
		if fi, err := os.Stat(filepath.Join(s.real(sp), rel)); err == nil && fi.Mode().IsRegular() {
			if err := os.MkdirAll(filepath.Join(etc, "ssl"), 0o755); err != nil {
				return err
			}
			l := filepath.Join(etc, "ssl", "ca-bundle.crt")
			os.Remove(l)
			return os.Symlink(sp+"/"+rel, l)
		}
	}
	return errors.New("no package of the profile has a CA bundle (cacert)")
}

func (s *Store) List() (*debugstore.ListResult, error) {
	out := &debugstore.ListResult{}
	dflt := s.defaultRoots()
	for _, sp := range s.manifest() {
		out.Packages = append(out.Packages, s.packageOf(base(sp), dflt[sp]))
	}
	return out, nil
}

func (s *Store) Remove(names []string) (*debugstore.Removed, error) {
	unlock, err := s.lockRoot()
	if err != nil {
		return nil, err
	}
	defer unlock()
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wantHash := map[string]bool{}
	if ix, err := s.cachedIndex(ctx); err == nil {
		for _, n := range names {
			if sp := ix.Resolve(n); sp != "" {
				wantHash[hashOf(base(sp))] = true
			}
		}
	}
	var kept, dropped []string
	for _, sp := range s.manifest() {
		b := base(sp)
		match := want[sp] || want[b] || want[nameOf(b)] || want[pnameOf(nameOf(b))] || wantHash[hashOf(b)]
		if !match {
			for _, c := range s.packageOf(b, false).Commands {
				if want[c] {
					match = true
					break
				}
			}
		}
		if match {
			dropped = append(dropped, sp)
			continue
		}
		kept = append(kept, sp)
	}
	if err := s.writeManifest(kept); err != nil {
		return nil, err
	}
	if err := s.relink(kept); err != nil {
		return nil, err
	}
	out, err := s.gcLocked()
	if err != nil {
		return nil, err
	}
	out.Removed = dropped
	return out, nil
}

func (s *Store) GC() (*debugstore.Removed, error) {
	unlock, err := s.lockRoot()
	if err != nil {
		return nil, err
	}
	defer unlock()
	s.clearTmp()
	return s.gcLocked()
}

func (s *Store) gcLocked() (*debugstore.Removed, error) {
	refs := map[string][]string{}
	sizes := map[string]int64{}
	paths := map[string]string{}
	ents, err := os.ReadDir(s.dir(subDB))
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".narinfo") {
			continue
		}
		n := s.registered(strings.TrimSuffix(e.Name(), ".narinfo"))
		if n == nil {
			continue
		}
		h := hashOf(base(n.StorePath))
		paths[h] = base(n.StorePath)
		sizes[h] = n.NarSize
		for _, r := range n.References {
			refs[h] = append(refs[h], hashOf(r))
		}
	}
	live := map[string]bool{}
	var mark func(h string)
	mark = func(h string) {
		if live[h] {
			return
		}
		live[h] = true
		for _, r := range refs[h] {
			mark(r)
		}
	}
	for _, sp := range s.manifest() {
		mark(hashOf(base(sp)))
	}
	pents, _ := os.ReadDir(s.pinsDir)
	for _, e := range pents {
		b, err := os.ReadFile(filepath.Join(s.pinsDir, e.Name()))
		if err != nil {
			continue
		}
		for _, l := range strings.Fields(string(b)) {
			mark(hashOf(base(l)))
		}
	}
	var dead []string
	for h := range paths {
		if !live[h] {
			dead = append(dead, h)
		}
	}
	sort.Strings(dead)
	out := &debugstore.Removed{}
	for _, h := range dead {
		if err := s.moveOut(paths[h]); err != nil {
			return out, err
		}
		if err := os.Remove(s.dbPath(h)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return out, err
		}
		out.FreedBytes += sizes[h]
	}
	return out, nil
}

func (s *Store) Reset() (int64, error) {
	unlock, err := s.lockRoot()
	if err != nil {
		return 0, err
	}
	defer unlock()
	before, _ := s.Bytes()
	gone := filepath.Join(s.dir(subTmp), ".tmp-reset-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	os.Chmod(s.dir(subStore), 0o755)
	if err := os.Rename(s.dir(subStore), gone); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	for _, d := range []string{subDB, subProfiles, subIndex, subHomes, subPins, subTmp} {
		if err := removeStoreTree(s.dir(d)); err != nil {
			return 0, err
		}
	}
	for _, d := range []string{subStore, subDB, filepath.Join(subProfiles, defaultProf), subIndex, subHomes, subPins, subTmp} {
		if err := os.MkdirAll(s.dir(d), 0o755); err != nil {
			return 0, err
		}
	}
	return before, nil
}

func (s *Store) Bytes() (int64, int) {
	var total int64
	n := 0
	ents, _ := os.ReadDir(s.dir(subDB))
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".narinfo") {
			continue
		}
		ni := s.registered(strings.TrimSuffix(e.Name(), ".narinfo"))
		if ni == nil {
			continue
		}
		total += ni.NarSize
		n++
	}
	return total, n
}
