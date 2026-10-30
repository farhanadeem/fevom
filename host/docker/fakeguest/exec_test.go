package fakeguest

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExecWithATerminal(t *testing.T) {
	e := NewEngine()
	srv := httptest.NewServer(e)
	defer srv.Close()
	defer e.Close()
	if _, err := e.Run(Spec{Name: "web", Image: "nginx"}); err != nil {
		t.Fatal(err)
	}
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
	create := func(ref string) (int, string) {
		code, b := do("POST", "/containers/"+ref+"/exec", `{"AttachStdin":true,"AttachStdout":true,"AttachStderr":true,"Tty":true,"Cmd":["/bin/sh"],"ConsoleSize":[30,100]}`)
		var r struct{ Id string }
		json.Unmarshal(b, &r)
		return code, r.Id
	}
	start := func(id string) (net.Conn, *bufio.Reader) {
		conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		body := `{"Detach":false,"Tty":true}`
		io.WriteString(conn, "POST /v1.44/exec/"+id+"/start HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/json\r\nContent-Length: "+itoa(len(body))+"\r\n\r\n"+body)
		r := bufio.NewReader(conn)
		status, _ := r.ReadString('\n')
		if !strings.HasPrefix(status, "HTTP/1.1 101") {
			t.Fatalf("start: %q", status)
		}
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if l == "\r\n" {
				break
			}
		}
		return conn, r
	}
	upTo := func(r *bufio.Reader, want string) string {
		var got []byte
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(string(got), want) {
			if time.Now().After(deadline) {
				t.Fatalf("waited for %q, read %q", want, got)
			}
			b, err := r.ReadByte()
			if err != nil {
				t.Fatalf("waited for %q, read %q: %v", want, got, err)
			}
			got = append(got, b)
		}
		return string(got)
	}

	code, id := create("web")
	if code != 201 || id == "" {
		t.Fatalf("exec create: %d %q", code, id)
	}
	conn, r := start(id)
	upTo(r, "fake# ")
	io.WriteString(conn, "stty size\r")
	if got := upTo(r, "fake# "); !strings.Contains(got, "stty size\r\n30 100\r\n") {
		t.Errorf("the size given at create: %q", got)
	}
	if code, _ := do("POST", "/exec/"+id+"/resize?h=40&w=120", ""); code != 200 {
		t.Fatalf("resize: %d", code)
	}
	io.WriteString(conn, "echo hi there\r")
	if got := upTo(r, "fake# "); !strings.Contains(got, "hi there\r\n") {
		t.Errorf("echo: %q", got)
	}
	io.WriteString(conn, "stty size\r")
	if got := upTo(r, "fake# "); !strings.Contains(got, "40 120\r\n") {
		t.Errorf("the size after a resize: %q", got)
	}
	_, b := do("GET", "/exec/"+id+"/json", "")
	if !strings.Contains(string(b), `"Running":true`) {
		t.Errorf("running exec: %s", b)
	}
	io.WriteString(conn, "exit 3\r")
	if rest, _ := io.ReadAll(r); !strings.Contains(string(rest), "\r\n") {
		t.Errorf("after exit: %q", rest)
	}
	conn.Close()
	_, b = do("GET", "/exec/"+id+"/json", "")
	if !strings.Contains(string(b), `"Running":false`) || !strings.Contains(string(b), `"ExitCode":3`) {
		t.Errorf("ended exec: %s", b)
	}

	_, id2 := create("web")
	conn2, r2 := start(id2)
	upTo(r2, "fake# ")
	if err := e.Die("web"); err != nil {
		t.Fatal(err)
	}
	io.ReadAll(r2)
	conn2.Close()
	_, b = do("GET", "/exec/"+id2+"/json", "")
	if !strings.Contains(string(b), `"ExitCode":137`) {
		t.Errorf("an exec whose container died: %s", b)
	}
	if code, _ := create("web"); code != 409 {
		t.Errorf("an exec in a stopped container: %d", code)
	}
	if code, _ := do("GET", "/exec/nosuch/json", ""); code != 404 {
		t.Errorf("no such exec: %d", code)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
