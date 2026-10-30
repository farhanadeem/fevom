package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Descriptor struct {
	MediaType, Digest string
	Size              int64
}

type Info struct {
	Size   int64
	Labels map[string]string
}

var ErrNotFound = errors.New("not found")

type Store interface {
	Image(ctx context.Context, name string) (Descriptor, error)
	Info(ctx context.Context, digest string) (Info, error)
	Open(digest string) (*os.File, error)
}

type Server struct {
	Store Store
	Logf  func(format string, args ...any)
}

const SourceLabel = "containerd.io/distribution.source."

var (
	pathRE = regexp.MustCompile(`^/v2/(.+)/(manifests|blobs)/([^/]+)$`)
	nameRE = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)
	tagRE  = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
	digRE  = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	hostRE = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]*[a-zA-Z0-9])?(:[0-9]{1,5})?$`)
)

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		apiError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "fev's registry is read-only")
		return
	}
	if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
		return
	}
	m := pathRE.FindStringSubmatch(r.URL.Path)
	if m == nil || !nameRE.MatchString(m[1]) {
		apiError(w, http.StatusNotFound, "NAME_UNKNOWN", "not one of Docker's images")
		return
	}
	name, kind, ref := m[1], m[2], m[3]
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if kind == "blobs" {
		if !digRE.MatchString(ref) {
			apiError(w, http.StatusNotFound, "BLOB_UNKNOWN", "not a digest")
			return
		}
		s.serveBlob(ctx, w, r, ref, "application/octet-stream")
		return
	}
	if digRE.MatchString(ref) {
		s.serveBlob(ctx, w, r, ref, "")
		return
	}
	ns := r.URL.Query().Get("ns")
	if !tagRE.MatchString(ref) || !hostRE.MatchString(ns) {
		s.logf("registry: %s %s: no registry named (ns %q): 404", r.Method, r.URL.Path, ns)
		apiError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "no registry named")
		return
	}
	full := ns + "/" + name + ":" + ref
	d, err := s.Store.Image(ctx, full)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			s.logf("registry: %s: %v", full, err)
		} else {
			s.logf("registry: %s %s: not one of Docker's images (404)", r.Method, full)
		}
		apiError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "not one of Docker's images")
		return
	}
	info, err := s.Store.Info(ctx, d.Digest)
	if err != nil {
		apiError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "not in Docker's store")
		return
	}
	if pulledFrom(info.Labels, ns, name) {
		s.logf("registry: %s is Docker's pull from %s: left to it (404)", full, ns)
		apiError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "Docker pulled this tag from its registry: ask the registry")
		return
	}
	s.logf("registry: %s %s -> %s (Docker's)", r.Method, full, d.Digest)
	s.serve(w, r, d.Digest, d.MediaType, info.Size)
}

func pulledFrom(labels map[string]string, registry, name string) bool {
	host := registry
	if h, _, err := net.SplitHostPort(registry); err == nil {
		host = h
	}
	v, ok := labels[SourceLabel+host]
	if !ok {
		return false
	}
	for _, repo := range strings.Split(v, ",") {
		if repo == name {
			return true
		}
	}
	return false
}

func (s *Server) serveBlob(ctx context.Context, w http.ResponseWriter, r *http.Request, digest, mediaType string) {
	info, err := s.Store.Info(ctx, digest)
	if err != nil {
		code := "BLOB_UNKNOWN"
		if mediaType == "" {
			code = "MANIFEST_UNKNOWN"
		}
		apiError(w, http.StatusNotFound, code, "not in Docker's store")
		return
	}
	s.serve(w, r, digest, mediaType, info.Size)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, digest, mediaType string, size int64) {
	f, err := s.Store.Open(digest)
	if err != nil {
		apiError(w, http.StatusNotFound, "BLOB_UNKNOWN", "not in Docker's store")
		return
	}
	defer f.Close()
	if mediaType == "" {
		mediaType = sniffMediaType(f)
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Etag", `"`+digest+`"`)
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		return
	}
	http.ServeContent(w, r, "", time.Time{}, f)
}

func sniffMediaType(f *os.File) string {
	var m struct {
		MediaType string `json:"mediaType"`
	}
	dec := json.NewDecoder(f)
	if dec.Decode(&m) == nil && m.MediaType != "" {
		f.Seek(0, 0)
		return m.MediaType
	}
	f.Seek(0, 0)
	return "application/vnd.oci.image.manifest.v1+json"
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"errors":[{"code":%q,"message":%q}]}`+"\n", code, msg)
}

type Checked struct {
	net.Listener
	Check func(net.Conn) (bool, error)
	Logf  func(format string, args ...any)

	refused int
}

func (l *Checked) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ok, err := l.Check(c)
		if ok {
			return c, nil
		}
		l.refused++
		if l.Logf != nil && (l.refused == 1 || l.refused%100 == 0) {
			l.Logf("registry: refused %s, not containerd's (%d refused; %v)", c.RemoteAddr(), l.refused, err)
		}
		c.Close()
	}
}
