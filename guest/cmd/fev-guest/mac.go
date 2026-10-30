//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"os/user"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"fev/proto/bridge"
)

var socketPath = bridge.Socket

var stdin, stdout, stderr = os.Stdin, os.Stdout, os.Stderr

const failed = 255

var sayAs = "mac"

func say(format string, args ...any) {
	fmt.Fprintf(stderr, sayAs+": "+format+"\n", args...)
}

func dial(hdr *bridge.Header) (net.Conn, *bufio.Reader, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
			return nil, nil, fmt.Errorf("this is not a fev machine, or the machine has no command bridge (%s is missing)", socketPath)
		}
		return nil, nil, fmt.Errorf("cannot reach the Mac: %v", err)
	}
	werr := bridge.WriteHeader(conn, hdr)
	r := bufio.NewReader(conn)
	if err := bridge.ReadReply(r); err != nil {
		conn.Close()
		if werr != nil {
			var refused *bridge.Refused
			if !errors.As(err, &refused) {
				return nil, nil, werr
			}
		}
		return nil, nil, err
	}
	return conn, r, nil
}

func ask(hdr *bridge.Header) error {
	conn, _, err := dial(hdr)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func info() (*bridge.Info, error) {
	conn, r, err := dial(&bridge.Header{Verb: bridge.VerbInfo})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var i bridge.Info
	if err := json.NewDecoder(r).Decode(&i); err != nil {
		return nil, fmt.Errorf("the Mac's answer: %v", err)
	}
	return &i, nil
}

func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}

func termSize(files ...*os.File) (rows, cols int) {
	for _, f := range files {
		if ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ); err == nil && ws.Row > 0 {
			return int(ws.Row), int(ws.Col)
		}
	}
	return 0, 0
}

func macEnv(tty bool) []string {
	var env []string
	seen := map[string]bool{}
	add := func(name string) {
		if seen[name] || name == "" {
			return
		}
		if v, ok := os.LookupEnv(name); ok {
			seen[name] = true
			env = append(env, name+"="+v)
		}
	}
	if tty {
		add("TERM")
	}
	add("COLORTERM")
	for _, name := range strings.FieldsFunc(os.Getenv("FEVENV"), func(r rune) bool { return r == ':' || r == ' ' }) {
		name, _, _ = strings.Cut(name, "/")
		add(name)
	}
	return env
}

func userName() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return fmt.Sprint(os.Getuid())
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) frame(stream byte, p []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bridge.WriteFrame(l.w, stream, p)
}

var signalNames = map[os.Signal]string{
	syscall.SIGHUP: "HUP", syscall.SIGINT: "INT", syscall.SIGQUIT: "QUIT",
	syscall.SIGTERM: "TERM", syscall.SIGUSR1: "USR1", syscall.SIGUSR2: "USR2",
}

func runOnMac(argv []string) int {
	if len(argv) == 0 || argv[0] == "" {
		say("no command")
		return 2
	}
	return runHeader(&bridge.Header{Verb: bridge.VerbRun, Argv: argv})
}

func runHeader(hdr *bridge.Header) int {
	tin, tout, terr := isTerminal(stdin), isTerminal(stdout), isTerminal(stderr)
	hdr.Env, hdr.User = macEnv(tin || tout || terr), userName()
	hdr.Stdin, hdr.Stdout, hdr.Stderr = tin, tout, terr
	hdr.Dir, _ = os.Getwd()
	if hdr.TTY() {
		hdr.Rows, hdr.Cols = termSize(stdin, stdout, stderr)
	}
	conn, r, err := dial(hdr)
	if err != nil {
		say("%v", err)
		return failed
	}
	defer conn.Close()
	w := &lockedWriter{w: conn}

	if tin {
		if old, err := unix.IoctlGetTermios(int(stdin.Fd()), unix.TCGETS); err == nil {
			raw := *old
			raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
			raw.Oflag &^= unix.OPOST
			raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
			raw.Cflag &^= unix.CSIZE | unix.PARENB
			raw.Cflag |= unix.CS8
			raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
			if unix.IoctlSetTermios(int(stdin.Fd()), unix.TCSETS, &raw) == nil {
				defer unix.IoctlSetTermios(int(stdin.Fd()), unix.TCSETS, old)
			}
		}
	}

	var sizeMu sync.Mutex
	sentRows, sentCols := hdr.Rows, hdr.Cols
	sendSize := func() error {
		rows, cols := termSize(stdin, stdout, stderr)
		sizeMu.Lock()
		defer sizeMu.Unlock()
		if rows == 0 || rows == sentRows && cols == sentCols {
			return nil
		}
		sentRows, sentCols = rows, cols
		b, _ := json.Marshal(bridge.Control{Rows: rows, Cols: cols})
		return w.frame(bridge.FrameControl, b)
	}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGWINCH, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGUSR1, syscall.SIGUSR2)
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			if s == syscall.SIGWINCH {
				if hdr.TTY() && sendSize() != nil {
					return
				}
				continue
			}
			b, _ := json.Marshal(bridge.Control{Signal: signalNames[s]})
			if w.frame(bridge.FrameControl, b) != nil {
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, bridge.MaxFrame)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				if hdr.TTY() && sendSize() != nil {
					return
				}
				if w.frame(bridge.FrameStdin, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				w.frame(bridge.FrameStdin, nil)
				return
			}
		}
	}()

	buf := make([]byte, bridge.MaxFrame)
	for {
		stream, n, err := bridge.ReadFrame(r, buf)
		if err != nil {
			say("the connection to the Mac ended before the command did: %v", err)
			return failed
		}
		switch stream {
		case bridge.FrameStdout:
			stdout.Write(buf[:n])
		case bridge.FrameStderr:
			stderr.Write(buf[:n])
		case bridge.FrameExit:
			var e bridge.Exit
			if err := json.Unmarshal(buf[:n], &e); err != nil {
				say("the command's end: %v", err)
				return failed
			}
			if e.Error != "" {
				say("%s", e.Error)
			}
			return e.Status()
		}
	}
}
