package engine

import (
	"io"
	"net/http"
	"net/netip"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fev/host/docker/fakeguest"
)

func TestAContainerWhoseInspectFailsIsSkippedNotTheEndOfTheFeed(t *testing.T) {
	engine := fakeguest.NewEngine()
	defer engine.Close()
	bad, _ := engine.Run(fakeguest.Spec{Name: "bad"})
	good, _ := engine.Run(web)
	var inspects atomic.Int32
	c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/containers/"+bad+"/json" {
			inspects.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"message":"layer does not exist"}`)
			return
		}
		engine.ServeHTTP(w, r)
	}))
	sink := newRecorder()
	stop := watch(t, c, sink)
	sink.wait(t, "synced "+good[:4])
	sink.mu.Lock()
	log := strings.Join(sink.log, "\n")
	sink.mu.Unlock()
	if !strings.Contains(log, "up "+good[:4]) || strings.Contains(log, bad[:4]) || strings.Count(log, "synced") != 1 {
		t.Fatalf("with one container that cannot be inspected the sink was told:\n%s", log)
	}
	engine.Die(bad)
	engine.Start(bad)
	later, _ := engine.Run(fakeguest.Spec{Name: "later"})
	sink.wait(t, "up "+later[:4]+" later []")
	if err := stop(); err == nil || strings.Contains(err.Error(), "attempts") {
		t.Fatalf("the watch ended with %v", err)
	}
	if n := inspects.Load(); n < 2 {
		t.Errorf("the failing container was inspected %d times; want once at the list and once at its start", n)
	}
}

func TestTheFeedsRetryIsBoundedInWaitAndInNumber(t *testing.T) {
	w := &Watcher{}
	var total time.Duration
	var waits []time.Duration
	for failure := 1; failure < DefaultAttempts; failure++ {
		d := w.delay(failure)
		waits = append(waits, d)
		total += d
	}
	if waits[0] != 100*time.Millisecond || waits[1] != 200*time.Millisecond {
		t.Errorf("the first waits are %v", waits[:2])
	}
	for i, d := range waits {
		if d > DefaultMaxBackoff || (i > 0 && d < waits[i-1]) {
			t.Fatalf("wait %d is %v after %v; none may pass %v or shrink", i, d, waits, DefaultMaxBackoff)
		}
	}
	if waits[len(waits)-1] != 5*time.Second {
		t.Errorf("the last wait is %v, want the cap", waits[len(waits)-1])
	}
	if total < 25*time.Second || total > 40*time.Second {
		t.Errorf("the feed is given up on after %v of failures in a row; want about half a minute", total)
	}
	quick := &Watcher{Backoff: 5 * time.Millisecond, MaxBackoff: 12 * time.Millisecond}
	if a, b, c := quick.delay(1), quick.delay(2), quick.delay(3); a != 5*time.Millisecond || b != 10*time.Millisecond || c != 12*time.Millisecond {
		t.Errorf("waits %v %v %v", a, b, c)
	}
	if d := w.delay(64); d != DefaultMaxBackoff {
		t.Errorf("the 64th wait is %v", d)
	}
}

func TestAVolumeBoundOverAFolderIsAmongTheBindSources(t *testing.T) {
	doc := `{"Id":"abc123","Name":"/dev","State":{"Running":true},"Config":{"Image":"x"},"NetworkSettings":{"Ports":{}},
		"Mounts":[{"Type":"bind","Source":"/Users/me/src","Destination":"/app"},
		          {"Type":"volume","Name":"data","Source":"/var/lib/docker/volumes/data/_data","Destination":"/data","Driver":"local"},
		          {"Type":"volume","Name":"plain","Source":"/var/lib/docker/volumes/plain/_data","Destination":"/p","Driver":"local"},
		          {"Type":"volume","Name":"nfs","Source":"/var/lib/docker/volumes/nfs/_data","Destination":"/n","Driver":"local"},
		          {"Type":"volume","Name":"broken","Source":"/var/lib/docker/volumes/broken/_data","Destination":"/b","Driver":"local"},
		          {"Type":"volume","Name":"other","Source":"/x","Destination":"/o","Driver":"somebody-elses"},
		          {"Type":"volume","Name":"","Source":"/var/lib/docker/volumes/5f/_data","Destination":"/anon","Driver":"local"}]}`
	var asked []string
	c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		switch r.URL.Path {
		case "/containers/abc123/json":
			io.WriteString(w, doc)
		case "/volumes/data":
			io.WriteString(w, `{"Name":"data","Driver":"local","Options":{"device":"/Users/me/data","o":"rw,bind","type":"none"}}`)
		case "/volumes/plain":
			io.WriteString(w, `{"Name":"plain","Driver":"local","Options":null}`)
		case "/volumes/nfs":
			io.WriteString(w, `{"Name":"nfs","Driver":"local","Options":{"device":":/export","o":"addr=10.0.0.1","type":"nfs"}}`)
		case "/volumes/broken":
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"message":"no"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"no such thing"}`)
		}
	}))
	got, err := c.Inspect(bg, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/Users/me/src", "/Users/me/data"}; !reflect.DeepEqual(got.Binds, want) {
		t.Errorf("bind sources %q, want %q", got.Binds, want)
	}
	if want := []string{"/containers/abc123/json", "/volumes/data", "/volumes/plain", "/volumes/nfs", "/volumes/broken"}; !reflect.DeepEqual(asked, want) {
		t.Errorf("the engine was asked %q, want %q: only the local driver's named volumes are looked at", asked, want)
	}
}

func TestInspectReadsWhatTheRegistryNeeds(t *testing.T) {
	doc := `{"Id":"abc123","Name":"/web","State":{"Running":true},
		"Config":{"Image":"x","Labels":{"com.docker.compose.project":"shop","dev.fevom.domains":"a.local"},
		          "ExposedPorts":{"80/tcp":{},"9229/tcp":{},"53/udp":{},"x/tcp":{},"7/sctp":{}}},
		"HostConfig":{"NetworkMode":"shop_default"},
		"NetworkSettings":{"Ports":{},"SandboxKey":"/var/run/docker/netns/4c1d2e3f4a5b",
		  "Networks":{"zz":{"IPAddress":"198.19.65.2"},"shop_default":{"IPAddress":"198.19.64.2"},"v6only":{"IPAddress":""}}}}`
	c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, doc) }))
	got, err := c.Inspect(bg, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got.NetworkMode != "shop_default" || got.SandboxKey != "/var/run/docker/netns/4c1d2e3f4a5b" || got.Labels["dev.fevom.domains"] != "a.local" {
		t.Errorf("%+v", got)
	}
	want := []Network{{Name: "shop_default", Address: netip.MustParseAddr("198.19.64.2")}, {Name: "zz", Address: netip.MustParseAddr("198.19.65.2")}}
	if !reflect.DeepEqual(got.Networks, want) {
		t.Errorf("networks %+v", got.Networks)
	}
	if !reflect.DeepEqual(got.Exposed, []string{"53/udp", "80/tcp", "9229/tcp"}) {
		t.Errorf("exposed %q", got.Exposed)
	}
	doc = `{"Id":"abc123","Name":"/web","State":{"Running":true},"Config":{"Image":"x"},"NetworkSettings":{"SandboxKey":"../../etc"}}`
	if got, _ := c.Inspect(bg, "abc123"); got.SandboxKey != "" {
		t.Errorf("a relative namespace path was taken: %q", got.SandboxKey)
	}
}
