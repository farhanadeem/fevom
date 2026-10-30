package images

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"fev/proto/agent"
)

const DefaultServer = "https://images.linuxcontainers.org"

const (
	maxIndex  = 4 << 20
	maxRootfs = 4 << 30
)

const (
	RootfsFile = "rootfs.tar.xz"
	IncusFile  = "incus.tar.xz"
	sumsFile   = "SHA256SUMS"
)

var ErrOlder = errors.New("older than the build fev already has")

type Source struct {
	Server  string
	Client  *http.Client
	TempDir string
	Now     func() time.Time

	v *verifier
}

const NoServer = "none"

var ErrNoServer = errors.New("this daemon has no image server")

func NewSource(server, tempDir string) (*Source, error) {
	kr, err := Keyring()
	if err != nil {
		return nil, err
	}
	if server == NoServer {
		s := &Source{Server: NoServer, TempDir: tempDir}
		s.v = newVerifier(kr, func() time.Time { return s.now() })
		return s, nil
	}
	if server == "" {
		server = DefaultServer
	}
	if !strings.HasPrefix(server, "https://") {
		return nil, fmt.Errorf("images: the server %q is not https", server)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = macProxy
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	s := &Source{Server: strings.TrimSuffix(server, "/"), TempDir: tempDir, Client: &http.Client{Transport: transport, CheckRedirect: httpsOnly}}
	s.v = newVerifier(kr, func() time.Time { return s.now() })
	return s, nil
}

func (s *Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func httpsOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("a redirect to %s, which is not https", req.URL.Redacted())
	}
	if len(via) >= 10 {
		return errors.New("more than 10 redirects")
	}
	return nil
}

func (s *Source) get(ctx context.Context, rel string) (*http.Response, error) {
	if s.Server == NoServer {
		return nil, ErrNoServer
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Server+rel, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", req.URL.Redacted(), resp.Status)
	}
	return resp, nil
}

func (s *Source) small(ctx context.Context, rel string, limit int64) ([]byte, error) {
	resp, err := s.get(ctx, rel)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", rel, limit)
	}
	return data, nil
}

const indexPath = "/meta/1.0/index-system"

func (s *Source) Index(ctx context.Context) ([]Entry, error) {
	if s.Server == NoServer {
		return nil, ErrNoServer
	}
	var err error
	for range 2 {
		var data, asc []byte
		if data, err = s.small(ctx, indexPath, maxIndex); err != nil {
			return nil, err
		}
		if asc, err = s.small(ctx, indexPath+".asc", maxSignature); err != nil {
			return nil, err
		}
		if _, err = s.v.check(data, asc); err == nil {
			return ParseIndex(data)
		}
	}
	return nil, fmt.Errorf("index-system: %w", err)
}

type Build struct {
	agent.ImageRef
	Signed      time.Time
	Templates   []agent.ImageTemplate
	RootfsBytes int64

	rootfs *os.File
}

func (b *Build) Close() error {
	if b.rootfs == nil {
		return nil
	}
	err := b.rootfs.Close()
	b.rootfs = nil
	return err
}

func (b *Build) WriteTar(ctx context.Context, w io.Writer) (int64, bool, error) {
	if b.rootfs == nil {
		return 0, false, errors.New("images: the build is closed")
	}
	return decompressXZ(ctx, b.rootfs, b.RootfsBytes, w)
}

type Progress func(file string, done, total int64)

func (s *Source) Fetch(ctx context.Context, e Entry, kept string, progress Progress) (*Build, error) {
	at, err := ParseSerial(e.Serial)
	if err != nil {
		return nil, err
	}
	if kept != "" {
		have, err := ParseSerial(kept)
		if err == nil && at.Before(have) {
			return nil, fmt.Errorf("%s %s %s: the server offers %s, %w (%s): refused, so that an old list cannot take a machine back to an old build", e.Distro, e.Release, e.Variant, e.Serial, ErrOlder, kept)
		}
	}
	what := fmt.Sprintf("%s %s %s %s", e.Distro, e.Release, e.Variant, e.Serial)
	sums, err := s.small(ctx, e.Path+sumsFile, maxSums)
	if err != nil {
		return nil, err
	}
	asc, err := s.small(ctx, e.Path+sumsFile+".asc", maxSignature)
	if err != nil {
		return nil, err
	}
	signed, err := s.v.check(sums, asc)
	if err != nil {
		return nil, fmt.Errorf("%s: %s.asc: %w", what, sumsFile, err)
	}
	if err := s.v.fresh(signed); err != nil {
		return nil, fmt.Errorf("%s: %s: %w", what, sumsFile, err)
	}
	digests, err := ParseSums(sums)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	for _, f := range []string{RootfsFile, IncusFile} {
		if digests[f] == nil {
			return nil, fmt.Errorf("%s: %s does not list %s", what, sumsFile, f)
		}
	}

	meta, err := s.small(ctx, e.Path+IncusFile, maxMetaXZ)
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(meta); !bytes.Equal(sum[:], digests[IncusFile]) {
		return nil, fmt.Errorf("%s: %s does not match %s: %x, signed %x", what, IncusFile, sumsFile, sum, digests[IncusFile])
	}
	templates, err := readTemplates(meta, e.ImageRef)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}

	f, size, err := s.download(ctx, e.Path+RootfsFile, digests[RootfsFile], progress)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return &Build{ImageRef: e.ImageRef, Signed: signed, Templates: templates, RootfsBytes: size, rootfs: f}, nil
}

const resumes = 5

func (s *Source) getFrom(ctx context.Context, rel string, off, total int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Server+rel, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	want := fmt.Sprintf("bytes %d-%d/%d", off, total-1, total)
	if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Range") != want {
		resp.Body.Close()
		return nil, fmt.Errorf("the server answered the rest's range with %s %q, not %q", resp.Status, resp.Header.Get("Content-Range"), want)
	}
	return resp.Body, nil
}

func (s *Source) download(ctx context.Context, rel string, want []byte, progress Progress) (*os.File, int64, error) {
	resp, err := s.get(ctx, rel)
	if err != nil {
		return nil, 0, err
	}
	body := resp.Body
	defer func() { body.Close() }()
	if resp.ContentLength > maxRootfs {
		return nil, 0, fmt.Errorf("%s: %d bytes, more than %d", rel, resp.ContentLength, int64(maxRootfs))
	}
	f, err := os.CreateTemp(s.TempDir, ".image-*")
	if err != nil {
		return nil, 0, err
	}
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return nil, 0, err
	}
	h := sha256.New()
	name := rel[strings.LastIndexByte(rel, '/')+1:]
	pw := &progressWriter{file: name, total: max(resp.ContentLength, 0), report: progress}
	w := io.MultiWriter(f, h, pw)
	n, err := io.Copy(w, io.LimitReader(body, maxRootfs+1))
	for left := resumes; err != nil && ctx.Err() == nil && left > 0 && resp.ContentLength > 0 && n > 0 && n < resp.ContentLength; left-- {
		cut := err
		body.Close()
		if body, err = s.getFrom(ctx, rel, n, resp.ContentLength); err != nil {
			body = io.NopCloser(strings.NewReader(""))
			err = fmt.Errorf("cut at %d bytes (%v); %w", n, cut, err)
			break
		}
		var more int64
		more, err = io.Copy(w, io.LimitReader(body, resp.ContentLength-n))
		n += more
		if err == nil && n < resp.ContentLength {
			err = io.ErrUnexpectedEOF
		}
		if more == 0 {
			break
		}
	}
	if err == nil && n > maxRootfs {
		err = fmt.Errorf("more than %d bytes", int64(maxRootfs))
	}
	if err == nil && resp.ContentLength >= 0 && n != resp.ContentLength {
		err = fmt.Errorf("%d bytes of %d", n, resp.ContentLength)
	}
	if err == nil {
		if sum := h.Sum(nil); !bytes.Equal(sum, want) {
			err = fmt.Errorf("does not match %s: %x, signed %x", sumsFile, sum, want)
		}
	}
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("%s: %w", name, err)
	}
	pw.end()
	return f, n, nil
}

type progressWriter struct {
	file        string
	done, total int64
	last        time.Time
	report      Progress
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.report != nil {
		if now := time.Now(); now.Sub(p.last) >= time.Second {
			p.last = now
			p.report(p.file, p.done, p.total)
		}
	}
	return len(b), nil
}

func (p *progressWriter) end() {
	if p.report != nil {
		p.report(p.file, p.done, p.total)
	}
}
