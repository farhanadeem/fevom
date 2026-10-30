package store

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) (*Store, *[]string, *sync.Mutex) {
	t.Helper()
	s, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var logs []string
	s.warn = func(m string) { mu.Lock(); logs = append(logs, m); mu.Unlock() }
	s.backoff = func(int) time.Duration { return 0 }
	return s, &logs, &mu
}

func TestGetRetriesTransientThenSucceeds(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	s, logs, lmu := testStore(t)
	resp, err := s.get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("get after two transient 503s: %v", err)
	}
	resp.Body.Close()
	if hits != 3 {
		t.Fatalf("server saw %d requests, want 3 (2 retried)", hits)
	}
	lmu.Lock()
	n := len(*logs)
	lmu.Unlock()
	if n != 2 {
		t.Fatalf("want 2 honest retry log lines, got %d: %v", n, *logs)
	}
}

func TestGetGivesUpAfterBoundOnPersistentFailure(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	s, _, _ := testStore(t)
	_, err := s.get(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("a persistent 502 should give up with an error")
	}
	if hits != fetchAttempts {
		t.Fatalf("server saw %d requests, want %d (the bound)", hits, fetchAttempts)
	}
	if got := err.Error(); !contains(got, "giving up") {
		t.Fatalf("error should say it gave up: %q", got)
	}
}

func TestGetDoesNotRetryPermanent(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	s, logs, lmu := testStore(t)
	if _, err := s.get(context.Background(), srv.URL); err == nil {
		t.Fatal("a 404 should return an error")
	}
	if hits != 1 {
		t.Fatalf("a 404 was retried (%d requests); permanent failures must not retry", hits)
	}
	lmu.Lock()
	n := len(*logs)
	lmu.Unlock()
	if n != 0 {
		t.Fatalf("a permanent failure logged %d retry lines, want 0", n)
	}
}

func TestGetStopsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	s, _, _ := testStore(t)
	s.backoff = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := s.get(ctx, srv.URL); err == nil {
		t.Fatal("a cancelled context should stop the retries")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
