package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func pinOf(data []byte, url string) Pin {
	s := sha256.Sum256(data)
	return Pin{Version: "v1.37.1+k3s1", URL: url, Size: int64(len(data)), SHA256: hex.EncodeToString(s[:])}
}

type archiveServer struct {
	data   []byte
	cut    int
	mu     sync.Mutex
	ranges []string
}

func (a *archiveServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.ranges = append(a.ranges, r.Header.Get("Range"))
	cut := a.cut
	a.cut = 0
	a.mu.Unlock()
	from := 0
	if rg := r.Header.Get("Range"); rg != "" {
		n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
		from = n
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(a.data)-1, len(a.data)))
		w.Header().Set("Content-Length", strconv.Itoa(len(a.data)-from))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(a.data)))
	}
	if cut > 0 {
		w.Write(a.data[from : from+cut])
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
		return
	}
	w.Write(a.data[from:])
}

func archiveBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func TestFetchResumesADroppedDownload(t *testing.T) {
	data := archiveBytes(3<<20 + 123)
	srv := &archiveServer{data: data, cut: 1 << 20}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	dir := t.TempDir()
	final := filepath.Join(dir, "a.tar.zst")
	var last int64
	f := &Fetcher{Client: ts.Client(), Every: 512 << 10}
	err := f.Fetch(context.Background(), ts.URL+"/a.tar.zst", pinOf(data, ts.URL+"/a.tar.zst"), final, func(have, total int64) { last = have })
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(final)
	if string(got) != string(data) {
		t.Fatal("the archive differs")
	}
	if last != int64(len(data)) {
		t.Errorf("progress ended at %d", last)
	}
	if len(srv.ranges) != 2 || srv.ranges[0] != "" || srv.ranges[1] != "bytes=1048576-" {
		t.Errorf("requests' ranges %q", srv.ranges)
	}
	if _, err := os.Stat(final + ".part"); !os.IsNotExist(err) {
		t.Errorf("part left: %v", err)
	}
}

func TestFetchResumesWhatAStopLeft(t *testing.T) {
	data := archiveBytes(2 << 20)
	srv := &archiveServer{data: data}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	dir := t.TempDir()
	final := filepath.Join(dir, "a.tar.zst")
	os.WriteFile(final+".part", data[:700_000], 0o644)
	f := &Fetcher{Client: ts.Client()}
	if err := f.Fetch(context.Background(), "", pinOf(data, ts.URL+"/a.tar.zst"), final, nil); err != nil {
		t.Fatal(err)
	}
	if len(srv.ranges) != 1 || srv.ranges[0] != "bytes=700000-" {
		t.Errorf("ranges %q", srv.ranges)
	}
	if got, _ := os.ReadFile(final); string(got) != string(data) {
		t.Fatal("the archive differs")
	}
}

func TestFetchRefusesAnArchiveThatIsNotThePins(t *testing.T) {
	data := archiveBytes(100_000)
	other := archiveBytes(100_000)
	other[5] ^= 1
	ts := httptest.NewServer(&archiveServer{data: other})
	defer ts.Close()
	dir := t.TempDir()
	final := filepath.Join(dir, "a.tar.zst")
	pin := pinOf(data, "https://github.com/x/a.tar.zst")
	err := (&Fetcher{Client: ts.Client()}).Fetch(context.Background(), ts.URL+"/mirror.tar.zst", pin, final, nil)
	var mm *ErrMismatch
	if !errors.As(err, &mm) || mm.Want != pin.SHA256 || mm.Got == pin.SHA256 || !strings.Contains(err.Error(), pin.SHA256) {
		t.Fatalf("err %v", err)
	}
	for _, p := range []string{final, final + ".part"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left: %v", p, err)
		}
	}
}

func TestFetchOfflineSaysSo(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	dir := t.TempDir()
	pin := Pin{URL: "http://" + addr + "/k3s-airgap-images-arm64.tar.zst", Size: 177_000_000, SHA256: strings.Repeat("0", 64)}
	err := (&Fetcher{}).Fetch(context.Background(), "", pin, filepath.Join(dir, "a"), nil)
	var off *ErrOffline
	if !errors.As(err, &off) || off.Host != "127.0.0.1" {
		t.Fatalf("err %v", err)
	}
	line := OfflineLine(pin, off)
	if !strings.Contains(line, "(177 MB) from 127.0.0.1, which could not be reached: ") || !strings.Contains(line, "Nothing was changed") {
		t.Errorf("line %q", line)
	}
	pin.URL = "https://no-such-host.invalid/a.tar.zst"
	err = (&Fetcher{}).Fetch(context.Background(), "", pin, filepath.Join(dir, "a"), nil)
	if !errors.As(err, &off) || !strings.Contains(err.Error(), "no-such-host.invalid") {
		t.Fatalf("err %v", err)
	}
}

func TestFetchFromAFileAndTheCache(t *testing.T) {
	data := archiveBytes(300_000)
	dir := t.TempDir()
	src := filepath.Join(dir, "mac", "airgap.tar.zst")
	os.MkdirAll(filepath.Dir(src), 0o755)
	os.WriteFile(src, data, 0o644)
	final := filepath.Join(dir, "cache.tar.zst")
	pin := pinOf(data, "https://github.com/k3s-io/k3s/releases/download/v1.37.1%2Bk3s1/k3s-airgap-images-arm64.tar.zst")
	if pin.File() != "k3s-airgap-images-arm64.tar.zst" {
		t.Errorf("file %q", pin.File())
	}
	f := &Fetcher{}
	if err := f.Fetch(context.Background(), "file://"+src, pin, final, nil); err != nil {
		t.Fatal(err)
	}
	os.Remove(src)
	var told int64
	if err := f.Fetch(context.Background(), "file://"+src, pin, final, func(h, _ int64) { told = h }); err != nil || told != pin.Size {
		t.Fatalf("cached: %v %d", err, told)
	}
	os.WriteFile(final, archiveBytes(10), 0o644)
	if err := f.Fetch(context.Background(), "file://"+src, pin, final, nil); err == nil {
		t.Fatal("a wrong cached archive was used")
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Errorf("wrong archive left: %v", err)
	}
}

func TestReadPin(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "archive.json")
	k3s := `,"k3s_url":"https://x/k3s-arm64","k3s_size":7,"k3s_sha256":"` + strings.Repeat("c", 64) + `"`
	whole := `{"version":"v1.37.1+k3s1","url":"https://x/y.tar.zst","size":10,"sha256":"` + strings.Repeat("a", 64) + `","payload":"` + strings.Repeat("b", 64) + `"`
	os.WriteFile(p, []byte(whole+k3s+`}`), 0o644)
	pin, err := ReadPin(p)
	if err != nil || pin.Payload != strings.Repeat("b", 64) || pin.File() != "y.tar.zst" || pin.Total() != 17 {
		t.Fatalf("%+v %v", pin, err)
	}
	if k := pin.K3s(); k.File() != "k3s-arm64" || k.Size != 7 || k.SHA256 != strings.Repeat("c", 64) || k.Version != pin.Version {
		t.Errorf("k3s's pin: %+v", k)
	}
	for _, bad := range []string{`{"url":"https://x/y"}`, whole + `}`} {
		os.WriteFile(p, []byte(bad), 0o644)
		if _, err := ReadPin(p); err == nil {
			t.Errorf("an incomplete pin was taken: %s", bad)
		}
	}
}

func TestK3sComesFromBesideTheArchive(t *testing.T) {
	pin := Pin{K3sURL: "https://github.com/k3s-io/k3s/releases/download/v1.37.1%2Bk3s1/k3s-arm64"}
	for in, want := range map[string]string{
		"": pin.K3sURL,
		"https://mirror.example/k3s/v1.37.1/k3s-airgap-images-arm64.tar.zst": "https://mirror.example/k3s/v1.37.1/k3s-arm64",
		"file:///Users/me/k3s/k3s-airgap-images-arm64.tar.zst":               "file:///Users/me/k3s/k3s-arm64",
	} {
		if got := pin.K3sSource(in); got != want {
			t.Errorf("%q: k3s from %q, want %q", in, got, want)
		}
	}
}
