package store

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"

	"fev/proto/debugstore"
)

//go:embed prefs.tsv
var prefsTSV []byte

type Index struct {
	rows  []Row
	keys  []string
	prefs map[string]string
	date  string
}

type Row struct {
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

func (ix *Index) Date() string { return ix.date }

func defaultPrefs() map[string]string { return parsePrefs(prefsTSV) }

func parsePrefs(b []byte) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) == 2 {
			m[f[0]] = f[1]
		}
	}
	return m
}

func (ix *Index) sortRows() {
	sort.Slice(ix.rows, func(i, j int) bool {
		if ix.rows[i].Command != ix.rows[j].Command {
			return ix.rows[i].Command < ix.rows[j].Command
		}
		return ix.rows[i].StorePath < ix.rows[j].StorePath
	})
	ix.keys = make([]string, len(ix.rows))
	for i, r := range ix.rows {
		ix.keys[i] = r.Command
	}
}

func (ix *Index) WriteTable(w io.Writer) error {
	var bw bytes.Buffer
	bw.WriteString("#fev-index-1\t" + ix.date + "\n")
	for _, r := range ix.rows {
		bw.WriteString(r.Command + "\t" + r.Attr + "\t" + r.Output + "\t" +
			b01(r.TopLevel) + "\t" + r.StorePath + "\t" + r.License + "\t" +
			b01(r.Free) + "\t" + b01(r.Main) + "\t" + strconv.Itoa(r.Priority) + "\n")
	}
	_, err := w.Write(bw.Bytes())
	return err
}

func b01(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func LoadTable(r io.Reader, prefs map[string]string, date string) (*Index, error) {
	if prefs == nil {
		prefs = defaultPrefs()
	}
	ix := &Index{prefs: prefs, date: date}
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#fev-index-1\t") {
			if h := strings.TrimPrefix(line, "#fev-index-1\t"); h != "" {
				ix.date = h
			}
			continue
		}
		if line[0] == '#' {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 9 {
			return nil, fmt.Errorf("index table: %q", line)
		}
		prio, _ := strconv.Atoi(f[8])
		ix.rows = append(ix.rows, Row{
			Command:   f[0],
			Attr:      f[1],
			Output:    f[2],
			TopLevel:  f[3] == "1",
			StorePath: f[4],
			License:   f[5],
			Free:      f[6] == "1",
			Main:      f[7] == "1",
			Priority:  prio,
		})
	}
	ix.sortRows()
	return ix, nil
}

func (ix *Index) candidates(name string) []Row {
	i := sort.SearchStrings(ix.keys, name)
	var out []Row
	for i < len(ix.keys) && ix.keys[i] == name {
		out = append(out, ix.rows[i])
		i++
	}
	return out
}

var penaltyPat = []string{"-env", "wrapped", "minimal", "freethreading", "-full", "static", "musl", "-classic", "-debug", "-test", "-tests"}

var boxes = map[string]bool{
	"busybox": true, "toybox": true, "ubase": true, "u-root": true,
	"u-root-cmds": true, "sbase": true, "uutils-coreutils": true,
	"uutils-procps": true, "cope": true,
}

func hasPenalty(s string) bool {
	s = strings.ToLower(s)
	for _, p := range penaltyPat {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

func stem(storePath string) string {
	name := nameOf(base(storePath))
	parts := strings.Split(name, "-")
	var out []string
	for _, q := range parts {
		if q != "" && q[0] >= '0' && q[0] <= '9' {
			break
		}
		out = append(out, q)
	}
	if len(out) == 0 {
		return name
	}
	return strings.Join(out, "-")
}

func (ix *Index) Pick(name string) (Row, int, bool) {
	c := ix.rankedCandidates(name)
	if len(c) == 0 {
		return Row{}, 0, false
	}
	return c[0], len(c), true
}

func (ix *Index) rankTupleFunc(name string) func(Row) struct_rank {
	want := ix.prefs[name]
	return func(r Row) struct_rank {
		st := stem(r.StorePath)
		sname := nameOf(base(r.StorePath))
		wantTier := 3
		if want != "" {
			switch {
			case r.Attr == want:
				wantTier = 0
			case st == want:
				wantTier = 1
			case strings.HasPrefix(sname, want):
				wantTier = 2
			default:
				wantTier = 3
			}
		}
		stemTier := 2
		switch {
		case st == name:
			stemTier = 0
		case strings.HasPrefix(st, name) || strings.HasPrefix(name, st):
			stemTier = 1
		}
		return struct_rank{
			a0: !r.TopLevel,
			a1: wantTier,
			a2: !r.Main,
			a3: stemTier,
			a4: boxes[st] || boxes[r.Attr],
			a5: hasPenalty(sname) || hasPenalty(r.Attr),
			a6: strings.Contains(r.Attr, "."),
			a7: r.Priority,
			a8: r.Output != "out",
			a9: len(r.Attr),
			aA: r.Attr,
		}
	}
}

func (ix *Index) rankedCandidates(name string) []Row {
	c := ix.candidates(name)
	if len(c) < 2 {
		return c
	}
	rank := ix.rankTupleFunc(name)
	sort.SliceStable(c, func(i, j int) bool {
		return rankLess(rank(c[i]), rank(c[j]))
	})
	return c
}

func rankLess(a, b struct_rank) bool {
	if a.a0 != b.a0 {
		return !a.a0 && b.a0
	}
	if a.a1 != b.a1 {
		return a.a1 < b.a1
	}
	if a.a2 != b.a2 {
		return !a.a2 && b.a2
	}
	if a.a3 != b.a3 {
		return a.a3 < b.a3
	}
	if a.a4 != b.a4 {
		return !a.a4 && b.a4
	}
	if a.a5 != b.a5 {
		return !a.a5 && b.a5
	}
	if a.a6 != b.a6 {
		return !a.a6 && b.a6
	}
	if a.a7 != b.a7 {
		return a.a7 < b.a7
	}
	if a.a8 != b.a8 {
		return !a.a8 && b.a8
	}
	if a.a9 != b.a9 {
		return a.a9 < b.a9
	}
	return a.aA < b.aA
}

type struct_rank struct {
	a0 bool
	a1 int
	a2 bool
	a3 int
	a4 bool
	a5 bool
	a6 bool
	a7 int
	a8 bool
	a9 int
	aA string
}

func (ix *Index) Lookup(command string) debugstore.Lookup {
	c := ix.rankedCandidates(command)
	var out debugstore.Lookup
	for i, r := range c {
		if i >= debugstore.MaxCandidates {
			break
		}
		out.Candidates = append(out.Candidates, debugstore.Candidate{
			Attr:      r.Attr,
			Name:      nameOf(base(r.StorePath)),
			Version:   versionOf(r.StorePath),
			StorePath: r.StorePath,
			License:   r.License,
			Unfree:    r.License != "" && !r.Free,
			Pick:      i == 0,
		})
	}
	return out
}

func versionOf(storePath string) string {
	name := nameOf(base(storePath))
	st := stem(storePath)
	v := strings.TrimPrefix(name, st)
	return strings.TrimPrefix(v, "-")
}

func (ix *Index) Resolve(name string) string {
	if r, _, ok := ix.Pick(name); ok {
		return r.StorePath
	}
	var best *Row
	for i := range ix.rows {
		r := &ix.rows[i]
		if r.Attr != name {
			continue
		}
		if best == nil || better(r, best) {
			best = r
		}
	}
	if best != nil {
		return best.StorePath
	}
	return ""
}

func better(a, b *Row) bool {
	if a.TopLevel != b.TopLevel {
		return a.TopLevel
	}
	if (a.Output == "out") != (b.Output == "out") {
		return a.Output == "out"
	}
	return a.StorePath < b.StorePath
}

func (ix *Index) Search(text string, limit int) debugstore.SearchResult {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	text = strings.ToLower(text)
	seen := map[string]bool{}
	var out debugstore.SearchResult
	for _, r := range ix.rows {
		if !strings.Contains(strings.ToLower(r.Command), text) && !strings.Contains(strings.ToLower(r.Attr), text) {
			continue
		}
		k := r.Command + "\x00" + r.Attr
		if seen[k] {
			continue
		}
		seen[k] = true
		out.Matches = append(out.Matches, debugstore.Match{
			Command: r.Command,
			Attr:    r.Attr,
			Name:    nameOf(base(r.StorePath)),
			License: r.License,
			Unfree:  r.License != "" && !r.Free,
		})
		if len(out.Matches) >= limit {
			break
		}
	}
	return out
}

func (s *Store) indexSource() string { return s.indexURL }

func (s *Store) index(ctx context.Context) (*Index, error) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	if s.idx != nil {
		return s.idx, nil
	}
	if s.indexFile != "" {
		ix, err := loadIndexFile(s.indexFile)
		if err != nil {
			return nil, err
		}
		s.idx = ix
		return ix, nil
	}
	if url := s.indexSource(); url != "" {
		if ix, err := s.cachedOverride(); err == nil {
			s.idx = ix
			return ix, nil
		}
		ix, err := s.fetchIndex(ctx, url)
		if err == nil {
			s.idx = ix
			return ix, nil
		}
	}
	ix, err := shippedIndex()
	if err != nil {
		return nil, err
	}
	s.idx = ix
	return ix, nil
}

func (s *Store) cachedIndex(ctx context.Context) (*Index, error) {
	s.idxMu.Lock()
	if s.idx != nil {
		ix := s.idx
		s.idxMu.Unlock()
		return ix, nil
	}
	s.idxMu.Unlock()
	if ix, err := s.cachedOverride(); err == nil {
		return ix, nil
	}
	return shippedIndex()
}

var (
	shippedOnce sync.Once
	shippedIx   *Index
	shippedErr  error
)

func shippedIndex() (*Index, error) {
	shippedOnce.Do(func() {
		if len(fevIndexTableZstd) == 0 {
			shippedErr = fmt.Errorf("index: no shipped command index for this architecture")
			return
		}
		zr, err := zstd.NewReader(bytes.NewReader(fevIndexTableZstd), zstd.WithDecoderConcurrency(1))
		if err != nil {
			shippedErr = err
			return
		}
		defer zr.Close()
		shippedIx, shippedErr = LoadTable(zr, defaultPrefs(), "")
	})
	return shippedIx, shippedErr
}

func (s *Store) cachedOverride() (*Index, error) {
	dir := s.dir(subIndex)
	b, err := os.ReadFile(filepath.Join(dir, "table.tsv"))
	if err != nil {
		return nil, err
	}
	src, _ := os.ReadFile(filepath.Join(dir, "url"))
	if strings.TrimSpace(string(src)) != s.indexSource() {
		return nil, fmt.Errorf("index: cached override is from another url")
	}
	return LoadTable(bytes.NewReader(b), defaultPrefs(), "")
}

func (s *Store) fetchIndex(ctx context.Context, url string) (*Index, error) {
	resp, err := s.get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	body := raw
	if len(raw) >= 4 && raw[0] == 0x28 && raw[1] == 0xb5 && raw[2] == 0x2f && raw[3] == 0xfd {
		zr, zerr := zstd.NewReader(bytes.NewReader(raw), zstd.WithDecoderConcurrency(1))
		if zerr != nil {
			return nil, zerr
		}
		defer zr.Close()
		if body, err = io.ReadAll(zr); err != nil {
			return nil, err
		}
	}
	ix, err := LoadTable(bytes.NewReader(body), defaultPrefs(), "")
	if err != nil {
		return nil, err
	}
	dir := s.dir(subIndex)
	os.MkdirAll(dir, 0o755)
	if err := writeFileAtomic(filepath.Join(dir, "table.tsv"), body); err == nil {
		writeFileAtomic(filepath.Join(dir, "url"), []byte(url))
	}
	return ix, nil
}

func (s *Store) SetIndexURL(u string) { s.indexURL = u }

func (s *Store) SetIndexFile(p string) { s.indexFile = p }

func loadIndexFile(p string) (*Index, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if len(b) >= 4 && b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd {
		zr, zerr := zstd.NewReader(bytes.NewReader(b), zstd.WithDecoderConcurrency(1))
		if zerr != nil {
			return nil, zerr
		}
		defer zr.Close()
		if b, err = io.ReadAll(zr); err != nil {
			return nil, err
		}
	}
	return LoadTable(bytes.NewReader(b), defaultPrefs(), "")
}

func (s *Store) SetIndex(ix *Index) {
	s.idxMu.Lock()
	s.idx = ix
	s.idxMu.Unlock()
}

func (s *Store) Lookup(ctx context.Context, command string) (*debugstore.Lookup, error) {
	ix, err := s.index(ctx)
	if err != nil {
		return nil, err
	}
	l := ix.Lookup(command)
	return &l, nil
}

func (s *Store) Search(ctx context.Context, text string, limit int) (*debugstore.SearchResult, error) {
	ix, err := s.index(ctx)
	if err != nil {
		return nil, err
	}
	r := ix.Search(text, limit)
	return &r, nil
}

func (s *Store) IndexDate() string {
	ix, err := s.cachedIndex(context.Background())
	if err != nil {
		return ""
	}
	return ix.Date()
}
