package engine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"fev/host/docker/fakeguest"
)

var bg = context.Background()

func serve(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	c, _ := serveUntil(t, handler)
	return c
}

func serveUntil(t *testing.T, handler http.Handler) (*Client, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "be.")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "vsock_2375.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(l)
	c := NewClient(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	})
	t.Cleanup(func() {
		c.Close()
		server.Close()
		os.RemoveAll(dir)
	})
	return c, func() { server.Close() }
}

func TestEventsRecordedFromAnEngineAreNormalised(t *testing.T) {
	file, err := os.Open("testdata/events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var got []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		e, err := Normalise(scanner.Bytes())
		if err != nil {
			t.Fatalf("%s: %v", scanner.Text(), err)
		}
		if len(e.ID) != 64 || e.Time.IsZero() || e.Name == "" {
			t.Errorf("%+v", e)
		}
		got = append(got, fmt.Sprintf("%s %s %s %s", e.Type, e.Action, e.ID[:4], e.Name))
	}
	want := []string{
		"container create 5cec hardcore_roentgen",
		"container attach 5cec hardcore_roentgen",
		"network connect fa74 bridge",
		"container start a03c heuristic_chebyshev",
		"network disconnect fa74 bridge",
		"container die a03c heuristic_chebyshev",
		"container destroy a03c heuristic_chebyshev",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	e, _ := Normalise([]byte(`{"Type":"network","Action":"disconnect","Actor":{"ID":"fa74","Attributes":{"container":"a03c","name":"bridge","type":"bridge"}},"time":1791515131,"timeNano":1791515131833143823}`))
	if e.Attributes["container"] != "a03c" || e.Time.UnixNano() != 1791515131833143823 {
		t.Fatalf("%+v", e)
	}
}

func TestVolumeImageAndDetailedEventsAreNormalised(t *testing.T) {
	cases := []struct {
		line string
		want Event
	}{
		{`{"Type":"volume","Action":"create","Actor":{"ID":"pgdata","Attributes":{"driver":"local"}},"scope":"local","time":1791515131,"timeNano":1791515131000000001}`,
			Event{Type: TypeVolume, Action: "create", ID: "pgdata", Attributes: map[string]string{"driver": "local"}, Time: time.Unix(0, 1791515131000000001)}},
		{`{"Type":"volume","Action":"destroy","Actor":{"ID":"pgdata","Attributes":{"driver":"local"}},"scope":"local","time":1791515131}`,
			Event{Type: TypeVolume, Action: "destroy", ID: "pgdata", Attributes: map[string]string{"driver": "local"}, Time: time.Unix(1791515131, 0)}},
		{`{"Type":"image","Action":"pull","Actor":{"ID":"nginx:latest","Attributes":{"name":"nginx"}},"scope":"local","time":1791515131,"timeNano":1791515131000000002}`,
			Event{Type: TypeImage, Action: "pull", ID: "nginx:latest", Name: "nginx", Attributes: map[string]string{"name": "nginx"}, Time: time.Unix(0, 1791515131000000002)}},
		{`{"Type":"image","Action":"delete","Actor":{"ID":"sha256:0123","Attributes":{"name":"sha256:0123"}},"scope":"local","time":1791515131,"timeNano":1791515131000000003}`,
			Event{Type: TypeImage, Action: "delete", ID: "sha256:0123", Attributes: map[string]string{"name": "sha256:0123"}, Time: time.Unix(0, 1791515131000000003)}},
		{`{"Type":"container","Action":"exec_start: sh -c echo: hi","Actor":{"ID":"abcd","Attributes":{"name":"web","execID":"e1"}},"time":1791515131,"timeNano":1791515131000000004}`,
			Event{Type: TypeContainer, Action: "exec_start", Detail: "sh -c echo: hi", ID: "abcd", Name: "web", Attributes: map[string]string{"name": "web", "execID": "e1"}, Time: time.Unix(0, 1791515131000000004)}},
		{`{"Type":"container","Action":"health_status: healthy","Actor":{"ID":"abcd","Attributes":{"name":"web"}},"time":1791515131,"timeNano":1791515131000000005}`,
			Event{Type: TypeContainer, Action: "health_status", Detail: "healthy", ID: "abcd", Name: "web", Attributes: map[string]string{"name": "web"}, Time: time.Unix(0, 1791515131000000005)}},
	}
	for _, tc := range cases {
		got, err := Normalise([]byte(tc.line))
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s\n got %+v, %v\nwant %+v", tc.line, got, err, tc.want)
		}
	}
	for _, bad := range []string{``, `not json`, `[]`, `{}`, `{"Type":"container"}`, `{"Action":"start"}`, `{"Type":7,"Action":"start"}`} {
		if e, err := Normalise([]byte(bad)); err == nil {
			t.Errorf("%q was taken as the event %+v", bad, e)
		}
	}
}

func TestAStreamSkipsWhatIsNotAnEventAndRefusesAnEndlessLine(t *testing.T) {
	good := `{"Type":"volume","Action":"create","Actor":{"ID":"v"}}`
	long := `{"Type":"volume","Action":"create","Actor":{"ID":"long","Attributes":{"label":"` + strings.Repeat("x", 200<<10) + `"}}}`
	input := "\n" + "garbage\n" + good + "\n" + `{"Type":"x"}` + "\n" + long + "\n" + strings.Repeat("y", maxEventLine+10) + "\n" + good + "\n"
	s := &Stream{body: io.NopCloser(strings.NewReader("")), r: bufio.NewReaderSize(strings.NewReader(input), 64<<10)}
	for _, want := range []string{"v", "long"} {
		e, err := s.Next()
		if err != nil || e.ID != want {
			t.Fatalf("got %q, %v, want %q", e.ID, err, want)
		}
	}
	if e, err := s.Next(); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("after an endless line: %+v, %v", e, err)
	}
	s = &Stream{body: io.NopCloser(strings.NewReader("")), r: bufio.NewReader(strings.NewReader(good))}
	if _, err := s.Next(); err != io.ErrUnexpectedEOF {
		t.Fatalf("got %v", err)
	}
}

func TestInspectReadsThePublishedPorts(t *testing.T) {
	engine := fakeguest.NewEngine()
	defer engine.Close()
	c := serve(t, engine)
	id, err := engine.Run(fakeguest.Spec{Name: "web", Image: "nginx", Ports: []fakeguest.Port{
		{HostPort: 8080, Proto: "tcp", ContainerPort: 80},
		{HostIP: "127.0.0.1", HostPort: 5432, Proto: "tcp", ContainerPort: 5432},
		{HostIP: "::1", HostPort: 5353, Proto: "udp", ContainerPort: 53},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Inspect(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(got.Bindings, func(i, j int) bool {
		a, b := got.Bindings[i], got.Bindings[j]
		return fmt.Sprint(a.HostPort, a.HostIP) < fmt.Sprint(b.HostPort, b.HostIP)
	})
	want := Container{ID: id, Name: "web", Image: "nginx", Running: true, NetworkMode: "bridge", Bindings: []Binding{
		{Proto: "udp", HostIP: netip.MustParseAddr("::1"), HostPort: 5353, ContainerPort: 53},
		{Proto: "tcp", HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: 5432, ContainerPort: 5432},
		{Proto: "tcp", HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: 8080, ContainerPort: 80},
		{Proto: "tcp", HostIP: netip.MustParseAddr("::"), HostPort: 8080, ContainerPort: 80},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if ids, err := c.Running(bg); err != nil || !reflect.DeepEqual(ids, []string{id}) {
		t.Fatalf("running: %v, %v", ids, err)
	}
	engine.Die(id)
	if got, err := c.Inspect(bg, id); err != nil || got.Running || len(got.Bindings) != 0 {
		t.Fatalf("after die: %+v, %v", got, err)
	}
	if ids, err := c.Running(bg); err != nil || len(ids) != 0 {
		t.Fatalf("running after die: %v, %v", ids, err)
	}
	if _, err := c.Inspect(bg, strings.Repeat("0", 64)); !NotFound(err) {
		t.Fatalf("a container that does not exist: %v", err)
	}
	if err := c.Ping(bg); err != nil {
		t.Fatal(err)
	}
}

func TestInspectDropsWhatIsNotAPort(t *testing.T) {
	doc := `{"Id":"abc123","Name":"/odd","State":{"Running":true},"Config":{"Image":"x"},"NetworkSettings":{"Ports":{
		"80/tcp":[{"HostIp":"0.0.0.0","HostPort":"8080"},{"HostIp":"not an address","HostPort":"8081"},{"HostIp":"fe80::1%en0","HostPort":"8082"},
		          {"HostIp":"","HostPort":"8083"},{"HostIp":"::ffff:10.0.0.9","HostPort":"8084"},{"HostIp":"0.0.0.0","HostPort":"0"},
		          {"HostIp":"0.0.0.0","HostPort":"70000"},{"HostIp":"0.0.0.0","HostPort":"-1"},{"HostIp":"0.0.0.0","HostPort":""}],
		"443/tcp":null,
		"53/sctp":[{"HostIp":"0.0.0.0","HostPort":"53"}],
		"0/tcp":[{"HostIp":"0.0.0.0","HostPort":"9"}],
		"99999/udp":[{"HostIp":"0.0.0.0","HostPort":"9"}],
		"nonsense":[{"HostIp":"0.0.0.0","HostPort":"9"}]}}}`
	var asked []string
	c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		switch r.URL.Path {
		case "/containers/abc123/json":
			io.WriteString(w, doc)
		case "/containers/liar/json":
			io.WriteString(w, `{"Id":"../../etc","State":{"Running":true}}`)
		case "/containers/huge/json":
			io.WriteString(w, `{"Id":"huge","Name":"`+strings.Repeat("x", maxBody)+`"}`)
		case "/containers/json":
			io.WriteString(w, `[{"Id":"abc123"},{"Id":"../x"},{"Id":""},{"Id":"a/b?c"},{}]`)
		default:
			w.WriteHeader(500)
			io.WriteString(w, "plain text failure")
		}
	}))
	got, err := c.Inspect(bg, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(got.Bindings, func(i, j int) bool { return got.Bindings[i].HostPort < got.Bindings[j].HostPort })
	want := []Binding{
		{Proto: "tcp", HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: 8080, ContainerPort: 80},
		{Proto: "tcp", HostPort: 8083, ContainerPort: 80},
		{Proto: "tcp", HostIP: netip.MustParseAddr("10.0.0.9"), HostPort: 8084, ContainerPort: 80},
	}
	if !reflect.DeepEqual(got.Bindings, want) {
		t.Fatalf("got  %+v\nwant %+v", got.Bindings, want)
	}
	if _, err := c.Inspect(bg, "liar"); err == nil {
		t.Error("an answer with an id that is not one was accepted")
	}
	if _, err := c.Inspect(bg, "huge"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("an answer past the bound: %v", err)
	}
	if ids, err := c.Running(bg); err != nil || !reflect.DeepEqual(ids, []string{"abc123"}) {
		t.Errorf("running: %v, %v", ids, err)
	}
	before := len(asked)
	for _, id := range []string{"", "../x", "a/b", "a b", "a?b", "-rf", strings.Repeat("a", 256)} {
		if _, err := c.Inspect(bg, id); err == nil {
			t.Errorf("%q was inspected", id)
		}
	}
	if len(asked) != before {
		t.Errorf("requests were made for ids that are not ids: %v", asked[before:])
	}
	if err := c.Ping(bg); err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "plain text failure") {
		t.Errorf("a failure that is not JSON: %v", err)
	}
}

type recorder struct {
	mu      sync.Mutex
	log     []string
	events  []Event
	changed chan struct{}
}

func newRecorder() *recorder { return &recorder{changed: make(chan struct{}, 1)} }

func (r *recorder) note(s string) {
	r.mu.Lock()
	r.log = append(r.log, s)
	r.mu.Unlock()
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

func ports(c *Container) string {
	var parts []string
	for _, b := range c.Bindings {
		ip := ""
		if b.HostIP.IsValid() {
			ip = b.HostIP.String()
		}
		parts = append(parts, fmt.Sprintf("%s:%d->%d/%s", ip, b.HostPort, b.ContainerPort, b.Proto))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (r *recorder) Container(id string, c *Container) {
	if c == nil {
		r.note("gone " + id[:4])
		return
	}
	r.note(fmt.Sprintf("up %s %s [%s]", id[:4], c.Name, ports(c)))
}

func (r *recorder) Synced(running map[string]bool) {
	var ids []string
	for id := range running {
		ids = append(ids, id[:4])
	}
	sort.Strings(ids)
	r.note("synced " + strings.Join(ids, ","))
}

func (r *recorder) Event(e Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *recorder) wait(t *testing.T, want ...string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		r.mu.Lock()
		got := append([]string{}, r.log...)
		r.mu.Unlock()
		if len(got) >= len(want) && reflect.DeepEqual(got[len(got)-len(want):], want) {
			return
		}
		select {
		case <-r.changed:
		case <-deadline:
			t.Fatalf("the sink was told\n  %s\nwhich does not end with\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
	}
}

func watch(t *testing.T, c *Client, sink Sink) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	w := &Watcher{Client: c, Sink: sink, Backoff: 20 * time.Millisecond, Attempts: 4}
	go func() { done <- w.Run(ctx) }()
	stopped := false
	stop = func() error {
		if stopped {
			return nil
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("the watcher did not stop")
			return nil
		}
	}
	t.Cleanup(func() { stop() })
	return stop
}

var web = fakeguest.Spec{Name: "web", Ports: []fakeguest.Port{{HostPort: 8080, Proto: "tcp", ContainerPort: 80}}}

func TestTheWatcherFollowsContainersFromStartToDie(t *testing.T) {
	engine := fakeguest.NewEngine()
	defer engine.Close()
	c := serve(t, engine)
	old, _ := engine.Run(fakeguest.Spec{Name: "db", Ports: []fakeguest.Port{{HostIP: "127.0.0.1", HostPort: 5432, Proto: "tcp", ContainerPort: 5432}}})
	sink := newRecorder()
	stop := watch(t, c, sink)
	sink.wait(t, "up "+old[:4]+" db [127.0.0.1:5432->5432/tcp]", "synced "+old[:4])

	paths := engine.Stats().Paths
	if len(paths) < 2 || paths[0] != "GET /events" || paths[1] != "GET /containers/json" {
		t.Fatalf("the first requests were %v", paths)
	}

	id, _ := engine.Run(web)
	sink.wait(t, "up "+id[:4]+" web [0.0.0.0:8080->80/tcp,:::8080->80/tcp]")
	engine.Disconnect(id)
	sink.wait(t, "up "+id[:4]+" web []")
	engine.Connect(id)
	sink.wait(t, "up "+id[:4]+" web [0.0.0.0:8080->80/tcp,:::8080->80/tcp]")
	engine.Die(id)
	sink.wait(t, "gone "+id[:4])
	engine.Start(id)
	sink.wait(t, "up "+id[:4]+" web [0.0.0.0:8080->80/tcp,:::8080->80/tcp]")

	engine.Emit("volume", "create", "pgdata", map[string]string{"driver": "local"})
	engine.Emit("image", "pull", "nginx:latest", map[string]string{"name": "nginx"})
	engine.Die(old)
	sink.wait(t, "gone "+old[:4])
	sink.mu.Lock()
	var feed []string
	for _, e := range sink.events {
		feed = append(feed, e.Type+" "+e.Action)
	}
	sink.mu.Unlock()
	want := "container create, container start, network disconnect, network connect, container die, container start, volume create, image pull, container die"
	if got := strings.Join(feed, ", "); got != want {
		t.Fatalf("the feed was\n  %s\nwant\n  %s", got, want)
	}
	if err := stop(); err != context.Canceled {
		t.Fatalf("stopped with %v", err)
	}
}

func TestAContainerStartedBetweenSubscribeAndListIsNotMissed(t *testing.T) {
	engine := fakeguest.NewEngine()
	defer engine.Close()
	var once sync.Once
	started := make(chan string, 1)
	c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/json" {
			engine.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, r)
		once.Do(func() {
			id, _ := engine.Run(web)
			started <- id
		})
		w.Write(rec.Body.Bytes())
	}))
	sink := newRecorder()
	watch(t, c, sink)
	late := <-started
	sink.wait(t, "synced ", "up "+late[:4]+" web [0.0.0.0:8080->80/tcp,:::8080->80/tcp]")
}

func TestAfterTheStreamEndsTheWatcherReconnectsAndReconciles(t *testing.T) {
	engine := fakeguest.NewEngine()
	defer engine.Close()
	c := serve(t, engine)
	a, _ := engine.Run(web)
	b, _ := engine.Run(fakeguest.Spec{Name: "api", Ports: []fakeguest.Port{{HostPort: 3000, Proto: "tcp", ContainerPort: 3000}}})
	sink := newRecorder()
	watch(t, c, sink)
	first := []string{a[:4], b[:4]}
	sort.Strings(first)
	sink.wait(t, "synced "+strings.Join(first, ","))

	engine.DropEvents()
	engine.Die(a)
	n, _ := engine.Run(fakeguest.Spec{Name: "new"})
	second := []string{b[:4], n[:4]}
	sort.Strings(second)
	sink.wait(t, "synced "+strings.Join(second, ","))
	sink.mu.Lock()
	log := strings.Join(sink.log, "\n")
	sink.mu.Unlock()
	if !strings.Contains(log, "up "+n[:4]+" new []") {
		t.Fatalf("the container started meanwhile was not delivered:\n%s", log)
	}
	engine.Die(b)
	sink.wait(t, "gone "+b[:4])
}

func TestTheWatcherGivesUpOnAnEngineThatIsNotThere(t *testing.T) {
	engine := fakeguest.NewEngine()
	defer engine.Close()
	c, engineGone := serveUntil(t, engine)
	sink := newRecorder()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	done := make(chan error, 1)
	w := &Watcher{Client: c, Sink: sink, Backoff: 5 * time.Millisecond, Attempts: 4}
	go func() { done <- w.Run(ctx) }()
	sink.wait(t, "synced ")
	engineGone()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "after 4 attempts") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher kept trying")
	}
}

func TestAFailedListIsTriedAgain(t *testing.T) {
	engine := fakeguest.NewEngine()
	defer engine.Close()
	id, _ := engine.Run(web)
	var mu sync.Mutex
	failures := 2
	c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fail := r.URL.Path == "/containers/json" && failures > 0
		if fail {
			failures--
		}
		mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"message":"not yet"}`)
			return
		}
		engine.ServeHTTP(w, r)
	}))
	sink := newRecorder()
	watch(t, c, sink)
	sink.wait(t, "up "+id[:4]+" web [0.0.0.0:8080->80/tcp,:::8080->80/tcp]", "synced "+id[:4])
}

func TestInspectGivesTheBindMountSources(t *testing.T) {
	doc := `{"Id":"abc123","Name":"/dev","State":{"Running":true},"Config":{"Image":"x"},
		"NetworkSettings":{"Ports":{"53/sctp":[{"HostIp":"0.0.0.0","HostPort":"53"}],"54/sctp":null,"80/tcp":[{"HostIp":"","HostPort":"8080"}]}},
		"Mounts":[{"Type":"bind","Source":"/Users/me/src/app","Destination":"/app","RW":true},
		          {"Type":"volume","Name":"node_modules","Source":"/var/lib/docker/volumes/node_modules/_data","Destination":"/app/node_modules"},
		          {"Type":"bind","Source":"/tmp/tmp.Xq3f","Destination":"/t","RW":false},
		          {"Type":"bind","Source":"relative","Destination":"/r"},
		          {"Type":"bind","Source":"/` + strings.Repeat("x", 5000) + `","Destination":"/l"},
		          {"Type":"tmpfs","Source":"","Destination":"/run"}]}`
	c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/containers/abc123/json":
			io.WriteString(w, doc)
		case "/containers/stopped/json":
			io.WriteString(w, `{"Id":"stopped","State":{"Running":false},"Mounts":[{"Type":"bind","Source":"/Users/me/x"}]}`)
		}
	}))
	got, err := c.Inspect(bg, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Binds, []string{"/Users/me/src/app", "/tmp/tmp.Xq3f"}) {
		t.Errorf("binds: %q", got.Binds)
	}
	if !reflect.DeepEqual(got.SCTP, []string{"53/sctp"}) || len(got.Bindings) != 1 {
		t.Errorf("sctp %q, bindings %+v", got.SCTP, got.Bindings)
	}
	if got, err := c.Inspect(bg, "stopped"); err != nil || len(got.Binds) != 0 {
		t.Errorf("a stopped container: %+v, %v", got, err)
	}
}
