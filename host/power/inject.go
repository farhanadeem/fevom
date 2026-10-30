package power

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
)

func InjectOn(path string) (Source, error) {
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	src := &injectedSocket{Injected: NewInjected(), l: l, path: path}
	go src.accept()
	return src, nil
}

type injectedSocket struct {
	*Injected
	l    net.Listener
	path string
}

func (s *injectedSocket) Close() {
	s.l.Close()
	os.Remove(s.path)
}

func (s *injectedSocket) accept() {
	for {
		conn, err := s.l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			lines := bufio.NewScanner(conn)
			for lines.Scan() {
				var kind Kind
				switch strings.TrimSpace(lines.Text()) {
				case "sleep":
					kind = WillSleep
				case "wake":
					kind = HasPoweredOn
				case "poweron":
					kind = WillPowerOn
				case "notsleep":
					kind = WillNotSleep
				default:
					fmt.Fprintf(conn, "{\"error\":\"want sleep, wake, poweron or notsleep\"}\n")
					continue
				}
				out, _ := json.Marshal(s.Send(kind))
				conn.Write(append(out, '\n'))
			}
		}()
	}
}
