package fakeguest

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

type fakeExec struct {
	id, container string
	cmd           []string
	user          string

	mu         sync.Mutex
	rows, cols int
	running    bool
	started    bool
	exitCode   int
	conn       net.Conn
}

type execConfig struct {
	AttachStdin, AttachStdout, AttachStderr, Tty bool
	Cmd                                          []string
	Env                                          []string
	User, WorkingDir                             string
	ConsoleSize                                  []int
}

func (e *Engine) execRoute(w http.ResponseWriter, r *http.Request, path string) bool {
	switch {
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/exec") && r.Method == http.MethodPost:
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/exec")
		var cfg execConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			fail(w, http.StatusBadRequest, "%v", err)
			return true
		}
		if len(cfg.Cmd) == 0 {
			fail(w, http.StatusBadRequest, "No exec command specified")
			return true
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		c := e.find(ref)
		if c == nil {
			fail(w, http.StatusNotFound, "No such container: %s", ref)
			return true
		}
		if !c.running {
			fail(w, http.StatusConflict, "container %s is not running", c.id)
			return true
		}
		if c.paused {
			fail(w, http.StatusConflict, "container %s is paused, unpause the container before exec", c.id)
			return true
		}
		var b [32]byte
		rand.Read(b[:])
		x := &fakeExec{id: hex.EncodeToString(b[:]), container: c.id, cmd: cfg.Cmd, user: cfg.User, rows: 24, cols: 80}
		if len(cfg.ConsoleSize) == 2 && cfg.ConsoleSize[0] > 0 && cfg.ConsoleSize[1] > 0 {
			x.rows, x.cols = cfg.ConsoleSize[0], cfg.ConsoleSize[1]
		}
		if e.execs == nil {
			e.execs = map[string]*fakeExec{}
		}
		e.execs[x.id] = x
		e.emit("container", "exec_create: "+strings.Join(cfg.Cmd, " "), c.id, map[string]string{"name": c.name, "image": c.image, "execID": x.id})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"Id": x.id})
		return true
	case strings.HasPrefix(path, "/exec/"):
		id, action, _ := strings.Cut(strings.TrimPrefix(path, "/exec/"), "/")
		e.mu.Lock()
		x := e.execs[id]
		e.mu.Unlock()
		if x == nil {
			fail(w, http.StatusNotFound, "No such exec instance: %s", id)
			return true
		}
		switch {
		case action == "start" && r.Method == http.MethodPost:
			e.execStart(w, r, x)
		case action == "resize" && r.Method == http.MethodPost:
			h, errH := strconv.Atoi(r.URL.Query().Get("h"))
			wd, errW := strconv.Atoi(r.URL.Query().Get("w"))
			if errH != nil || errW != nil || h <= 0 || wd <= 0 {
				fail(w, http.StatusBadRequest, "resize needs h and w")
				return true
			}
			x.mu.Lock()
			x.rows, x.cols = h, wd
			x.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case action == "json" && r.Method == http.MethodGet:
			x.mu.Lock()
			doc := map[string]any{
				"ID": x.id, "ContainerID": x.container, "Running": x.running, "ExitCode": x.exitCode, "Pid": 2,
				"OpenStdin": true, "OpenStdout": true, "OpenStderr": true,
				"ProcessConfig": map[string]any{"tty": true, "entrypoint": x.cmd[0], "arguments": x.cmd[1:], "user": x.user},
			}
			if !x.started {
				doc["ExitCode"] = nil
			}
			x.mu.Unlock()
			reply(w, doc)
		default:
			return false
		}
		return true
	}
	return false
}

func (e *Engine) execStart(w http.ResponseWriter, r *http.Request, x *fakeExec) {
	var p struct {
		Detach, Tty bool
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if !p.Tty || p.Detach {
		fail(w, http.StatusNotImplemented, "the fake engine runs an exec only attached, with a terminal")
		return
	}
	x.mu.Lock()
	if x.started {
		x.mu.Unlock()
		fail(w, http.StatusConflict, "Error: Exec command %s is already running", x.id)
		return
	}
	x.started, x.running = true, true
	x.mu.Unlock()
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	x.mu.Lock()
	x.conn = conn
	x.mu.Unlock()
	if strings.EqualFold(r.Header.Get("Connection"), "upgrade") {
		io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	} else {
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n")
	}
	code := x.play(rw.Reader, conn)
	x.mu.Lock()
	if x.running {
		x.running = false
		x.exitCode = code
	}
	x.mu.Unlock()
	conn.Close()
	e.mu.Lock()
	if c := e.containers[x.container]; c != nil {
		e.emit("container", "exec_die", c.id, map[string]string{"name": c.name, "image": c.image, "execID": x.id, "exitCode": strconv.Itoa(x.exitCode)})
	}
	e.mu.Unlock()
}

func (x *fakeExec) play(in *bufio.Reader, out io.Writer) int {
	prompt := "fake# "
	if x.user != "" && x.user != "root" && x.user != "0" {
		prompt = "fake$ "
	}
	io.WriteString(out, prompt)
	var line []byte
	for {
		b, err := in.ReadByte()
		if err != nil {
			return 0
		}
		switch b {
		case '\r', '\n':
			io.WriteString(out, "\r\n")
			words := strings.Fields(string(line))
			line = line[:0]
			if len(words) == 0 {
				io.WriteString(out, prompt)
				continue
			}
			switch {
			case words[0] == "exit":
				code := 0
				if len(words) > 1 {
					code, _ = strconv.Atoi(words[1])
				}
				return code
			case words[0] == "echo":
				io.WriteString(out, strings.Join(words[1:], " ")+"\r\n")
			case words[0] == "stty" && len(words) > 1 && words[1] == "size":
				x.mu.Lock()
				fmt.Fprintf(out, "%d %d\r\n", x.rows, x.cols)
				x.mu.Unlock()
			case words[0] == "tty":
				io.WriteString(out, "/dev/pts/0\r\n")
			default:
				fmt.Fprintf(out, "fake: %s: not found\r\n", words[0])
			}
			io.WriteString(out, prompt)
		case 0x7f, 0x08:
			if len(line) > 0 {
				line = line[:len(line)-1]
				io.WriteString(out, "\b \b")
			}
		case 0x03:
			line = line[:0]
			io.WriteString(out, "^C\r\n"+prompt)
		case 0x04:
			if len(line) == 0 {
				io.WriteString(out, "exit\r\n")
				return 0
			}
		default:
			line = append(line, b)
			out.Write([]byte{b})
		}
	}
}

func (e *Engine) endExecs(id string) {
	for _, x := range e.execs {
		if x.container != id {
			continue
		}
		x.mu.Lock()
		if x.running {
			x.running = false
			x.exitCode = 137
			if x.conn != nil {
				x.conn.Close()
			}
		}
		x.mu.Unlock()
	}
}
