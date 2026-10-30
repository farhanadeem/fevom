package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

type Pin struct {
	Version   string `json:"version"`
	URL       string `json:"url"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	Payload   string `json:"payload,omitempty"`
	K3sURL    string `json:"k3s_url"`
	K3sSize   int64  `json:"k3s_size"`
	K3sSHA256 string `json:"k3s_sha256"`
}

func (p Pin) K3s() Pin {
	return Pin{Version: p.Version, URL: p.K3sURL, Size: p.K3sSize, SHA256: p.K3sSHA256}
}

func (p Pin) Total() int64 { return p.Size + p.K3sSize }

func (p Pin) K3sSource(archiveURL string) string {
	if archiveURL == "" {
		return p.K3sURL
	}
	u, err := url.Parse(archiveURL)
	if err != nil {
		return archiveURL
	}
	u.Path = path.Join(path.Dir(u.Path), p.K3s().File())
	u.RawPath = ""
	return u.String()
}

func (p Pin) File() string {
	if u, err := url.Parse(p.URL); err == nil && u.Path != "" {
		return filepath.Base(u.Path)
	}
	return "k3s-airgap-images.tar.zst"
}

func ReadPin(path string) (Pin, error) {
	var p Pin
	data, err := os.ReadFile(path)
	if err != nil {
		return p, fmt.Errorf("the system image's Kubernetes pin: %w", err)
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("the system image's Kubernetes pin %s: %w", path, err)
	}
	if p.URL == "" || p.Size <= 0 || len(p.SHA256) != 64 || p.K3sURL == "" || p.K3sSize <= 0 || len(p.K3sSHA256) != 64 || p.Payload == "" {
		return p, fmt.Errorf("the system image's Kubernetes pin %s is incomplete", path)
	}
	return p, nil
}

type ErrOffline struct {
	Host   string
	Reason error
}

func (e *ErrOffline) Error() string {
	return fmt.Sprintf("%s could not be reached: %v", e.Host, e.Reason)
}

func (e *ErrOffline) Unwrap() error { return e.Reason }

type ErrMismatch struct{ File, Got, Want string }

func (e *ErrMismatch) Error() string {
	return fmt.Sprintf("%s's SHA-256 is %s, not the pin's %s; it was deleted", e.File, e.Got, e.Want)
}

type Progress func(have, total int64)

type Fetcher struct {
	Client *http.Client
	Files  func(path string) string
	Every  int64
	Logf   func(format string, args ...any)
}

func (f *Fetcher) Fetch(ctx context.Context, src string, pin Pin, final string, progress Progress) error {
	if progress == nil {
		progress = func(int64, int64) {}
	}
	if src == "" {
		src = pin.URL
	}
	u, err := url.Parse(src)
	if err != nil {
		return fmt.Errorf("%s's URL %q: %w", pin.File(), src, err)
	}
	if ok, err := f.check(final, pin); err != nil {
		return err
	} else if ok {
		progress(pin.Size, pin.Size)
		return nil
	}
	switch u.Scheme {
	case "file":
		return f.copyFile(ctx, u.Path, pin, final, progress)
	case "https", "http":
		return f.download(ctx, u, pin, final, progress)
	}
	return fmt.Errorf("%s's URL %q is neither https nor file", pin.File(), src)
}

func (f *Fetcher) check(final string, pin Pin) (bool, error) {
	st, err := os.Stat(final)
	if err != nil {
		return false, nil
	}
	if st.Size() == pin.Size {
		if sum, err := fileSum(final); err == nil && sum == pin.SHA256 {
			return true, nil
		}
	}
	return false, os.Remove(final)
}

func (f *Fetcher) copyFile(ctx context.Context, path string, pin Pin, final string, progress Progress) error {
	if f.Files != nil {
		path = f.Files(path)
	}
	in, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%s at %s: %w", pin.File(), path, err)
	}
	defer in.Close()
	part := final + ".part"
	out, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = f.copy(ctx, io.MultiWriter(out, h), in, 0, pin.Size, progress)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return err
	}
	return finish(part, final, h, pin)
}

func (f *Fetcher) download(ctx context.Context, u *url.URL, pin Pin, final string, progress Progress) error {
	part := final + ".part"
	h := sha256.New()
	var have int64
	if st, err := os.Stat(part); err == nil && st.Size() <= pin.Size {
		in, err := os.Open(part)
		if err != nil {
			return err
		}
		have, err = io.Copy(h, in)
		in.Close()
		if err != nil {
			return err
		}
		if have > 0 && f.Logf != nil {
			f.Logf("resuming the archive's fetch at %d of %d bytes", have, pin.Size)
		}
	} else if err == nil {
		os.Remove(part)
	}
	for have < pin.Size {
		n, err := f.get(ctx, u, part, have, h, pin.Size, progress)
		have += n
		if err == nil {
			break
		}
		var off *ErrOffline
		if errors.As(err, &off) || ctx.Err() != nil || n == 0 {
			return err
		}
	}
	return finish(part, final, h, pin)
}

func (f *Fetcher) get(ctx context.Context, u *url.URL, part string, have int64, h hash.Hash, total int64, progress Progress) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, err
	}
	if have > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
	}
	resp, err := f.client().Do(req)
	if err != nil {
		if offline(err) {
			return 0, &ErrOffline{Host: u.Hostname(), Reason: reason(err)}
		}
		return 0, fmt.Errorf("fetch %s: %w", u.Host, err)
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	switch {
	case have > 0 && resp.StatusCode == http.StatusPartialContent:
	case resp.StatusCode == http.StatusOK:
		if have > 0 {
			h.Reset()
			have = 0
		}
		flags |= os.O_TRUNC
	default:
		return 0, fmt.Errorf("fetch %s: %s", u.Host, resp.Status)
	}
	out, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := f.copy(ctx, io.MultiWriter(out, h), resp.Body, have, total, progress)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return n, err
}

func (f *Fetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return http.DefaultClient
}

func (f *Fetcher) copy(ctx context.Context, out io.Writer, in io.Reader, have, total int64, progress Progress) (int64, error) {
	every := f.Every
	if every <= 0 {
		every = 4 << 20
	}
	buf := make([]byte, 256<<10)
	var n, told int64
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		m, err := in.Read(buf)
		if m > 0 {
			if _, werr := out.Write(buf[:m]); werr != nil {
				return n, werr
			}
			n += int64(m)
			if n-told >= every {
				progress(have+n, total)
				told = n
			}
			if have+n > total {
				return n, fmt.Errorf("the download is longer than the pin's %d bytes", total)
			}
		}
		if err == io.EOF {
			progress(have+n, total)
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

func finish(part, final string, h hash.Hash, pin Pin) error {
	got := hex.EncodeToString(h.Sum(nil))
	if got != pin.SHA256 {
		os.Remove(part)
		return &ErrMismatch{File: pin.File(), Got: got, Want: pin.SHA256}
	}
	if st, err := os.Stat(part); err != nil || st.Size() != pin.Size {
		os.Remove(part)
		return fmt.Errorf("%s is not the pin's %d bytes", pin.File(), pin.Size)
	}
	return os.Rename(part, final)
}

func fileSum(path string) (string, error) {
	in, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer in.Close()
	h := sha256.New()
	if _, err := io.Copy(h, in); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func offline(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	return false
}

func reason(err error) error {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.IsNotFound {
			return fmt.Errorf("the name %s did not resolve", dns.Name)
		}
		return fmt.Errorf("looking up %s: %s", dns.Name, dns.Err)
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		msg := op.Err.Error()
		if i := strings.LastIndex(msg, ": "); i >= 0 {
			msg = msg[i+2:]
		}
		return errors.New(msg)
	}
	return err
}

func OfflineLine(pin Pin, err *ErrOffline) string {
	return fmt.Sprintf("Turning on Kubernetes the first time downloads it (%d MB) from %s, which could not be reached: %v. Nothing was changed; try again when online, or set kubernetes.images_url.",
		(pin.Total()+500_000)/1_000_000, err.Host, err.Reason)
}
