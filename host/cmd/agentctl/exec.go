package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"fev/proto/agent"
	"fev/proto/relay"
)

type envList []string

func (e *envList) String() string { return fmt.Sprint([]string(*e)) }
func (e *envList) Set(v string) error {
	*e = append(*e, v)
	return nil
}

func runExec(client *agent.Client, socket string, args []string) int {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	target := fs.String("debug", "", "a debug session on this container or image")
	kind := fs.String("kind", "", `"", "container" or "image"`)
	readOnly := fs.Bool("read-only", false, "a stopped container's root read-only")
	engine := fs.Bool("engine", false, "run in the engine's context")
	tty := fs.Bool("tty", false, "give the process a terminal")
	stdin := fs.Bool("stdin", false, "forward standard input")
	rows := fs.Int("rows", 0, "terminal rows")
	cols := fs.Int("cols", 0, "terminal cols")
	dir := fs.String("dir", "", "working directory in the target")
	showJSON := fs.Bool("json", false, "print ExecStarted.Debug as JSON on standard error")
	limit := fs.Duration("for", 120*time.Second, "give up after this long")
	var env envList
	fs.Var(&env, "env", "an environment entry NAME=value (repeatable)")
	_ = fs.Parse(args)

	p := agent.ExecStartParams{
		Context: agent.ExecEngine,
		Argv:    fs.Args(),
		Env:     env,
		Dir:     *dir,
		TTY:     *tty,
		Rows:    *rows,
		Cols:    *cols,
	}
	switch {
	case *target != "":
		p.Context = agent.ExecDebug
		p.Debug = &agent.DebugTarget{Target: *target, Kind: *kind, ReadOnly: *readOnly}
	case *engine:
		p.Context = agent.ExecEngine
	default:
		p.Context = agent.ExecInit
	}

	ctx, cancel := context.WithTimeout(context.Background(), *limit)
	defer cancel()
	exits := make(chan agent.ExecExit, 1)
	started, err := client.ExecStart(ctx, p, func(e agent.ExecExit) { exits <- e })
	if err != nil {
		fail("exec.start: %v", err)
	}
	if *showJSON && started.Debug != nil {
		b, _ := json.Marshal(started.Debug)
		fmt.Fprintln(os.Stderr, string(b))
	}

	dir4relay := filepath.Dir(socket)
	conn, err := net.Dial("unix", filepath.Join(dir4relay, fmt.Sprintf("vsock_%d.sock", relay.Port)))
	if err != nil {
		fail("relay: %v", err)
	}
	if err := relay.Open(conn, relay.Request{Kind: relay.Exec, Target: started.Token}); err != nil {
		fail("relay open: %v", err)
	}
	conn.SetDeadline(time.Now().Add(*limit))

	var wg sync.WaitGroup
	if *tty {
		if *stdin {
			wg.Add(1)
			go func() { defer wg.Done(); io.Copy(conn, os.Stdin) }()
		}
		buf := make([]byte, 32<<10)
		for {
			n, rerr := conn.Read(buf)
			if n > 0 {
				os.Stdout.Write(buf[:n])
			}
			if rerr != nil {
				break
			}
		}
	} else {
		if *stdin {
			wg.Add(1)
			go func() {
				defer wg.Done()
				buf := make([]byte, relay.MaxFrame)
				for {
					n, rerr := os.Stdin.Read(buf)
					if n > 0 {
						relay.WriteFrame(conn, relay.Stdin, buf[:n])
					}
					if rerr != nil {
						relay.WriteFrame(conn, relay.Stdin, nil)
						return
					}
				}
			}()
		} else {
			relay.WriteFrame(conn, relay.Stdin, nil)
		}
		buf := make([]byte, relay.MaxFrame)
		for {
			stream, n, rerr := relay.ReadFrame(conn, buf)
			if rerr != nil {
				break
			}
			if stream == relay.Stderr {
				os.Stderr.Write(buf[:n])
			} else {
				os.Stdout.Write(buf[:n])
			}
		}
	}

	select {
	case e := <-exits:
		if e.Error != "" {
			fmt.Fprintf(os.Stderr, "agentctl: %s\n", e.Error)
			return 125
		}
		if e.Reason != "" {
			fmt.Fprintf(os.Stderr, "agentctl: %s\n", e.Reason)
		}
		if e.Signal != 0 {
			return 128 + e.Signal
		}
		return e.ExitCode
	case <-client.Done():
		fmt.Fprintln(os.Stderr, "agentctl: the agent's connection ended")
		return 125
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "agentctl: no exit within the time limit")
		return 124
	}
}
