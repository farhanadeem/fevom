package fakeguest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNetworkEndpoints(t *testing.T) {
	e := NewEngine()
	srv := httptest.NewServer(e)
	defer srv.Close()
	defer e.Close()
	do := func(method, path, body string) (int, []byte) {
		req, _ := http.NewRequest(method, srv.URL+"/v1.44"+path, strings.NewReader(body))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	names := func() []string {
		_, b := do("GET", "/networks", "")
		var l []struct{ Name string }
		if err := json.Unmarshal(b, &l); err != nil {
			t.Fatalf("%v: %s", err, b)
		}
		var out []string
		for _, n := range l {
			out = append(out, n.Name)
		}
		return out
	}
	if got := strings.Join(names(), " "); got != "bridge host none" {
		t.Fatalf("the predefined networks: %q", got)
	}
	if code, b := do("POST", "/networks/create", `{"Name":"app_default","IPAM":{"Config":[{"Subnet":"192.168.97.0/24","Gateway":"192.168.97.1"}]}}`); code != 200 {
		t.Fatalf("create: %d %s", code, b)
	}
	if code, _ := do("POST", "/networks/create", `{"Name":"app_default"}`); code != http.StatusConflict {
		t.Errorf("a second network of the same name: %d", code)
	}
	code, b := do("GET", "/networks/app_default", "")
	var n struct {
		Name, Driver, Created string
		IPAM                  struct {
			Config []struct{ Subnet, Gateway string }
		}
	}
	if err := json.Unmarshal(b, &n); err != nil || code != 200 || n.Driver != "bridge" || len(n.IPAM.Config) != 1 || n.IPAM.Config[0].Subnet != "192.168.97.0/24" || n.Created == "" {
		t.Fatalf("inspect: %d %s", code, b)
	}
	if code, b := do("DELETE", "/networks/bridge", ""); code != http.StatusForbidden || !strings.Contains(string(b), "pre-defined") {
		t.Errorf("bridge removed: %d %s", code, b)
	}
	if code, _ := do("DELETE", "/networks/app_default", ""); code != http.StatusNoContent {
		t.Errorf("remove: %d", code)
	}
	if got := strings.Join(names(), " "); got != "bridge host none" {
		t.Errorf("after the removal: %q", got)
	}
}
