package machines

import (
	"bytes"
	"regexp"
	"strings"

	api "fev/proto/agent"
)

const consoleRingLines = 1000

const consoleMaxLine = 4 << 10

var ansi = regexp.MustCompile(`\x1b(\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[()][0-9A-Za-z])`)

type lineSplitter struct {
	partial []byte
}

func (s *lineSplitter) feed(p []byte) []string {
	var out []string
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.partial = append(s.partial, p...)
			for len(s.partial) >= consoleMaxLine {
				out = append(out, cleanLine(s.partial[:consoleMaxLine]))
				s.partial = append([]byte(nil), s.partial[consoleMaxLine:]...)
			}
			break
		}
		s.partial = append(s.partial, p[:i]...)
		p = p[i+1:]
		for len(s.partial) > consoleMaxLine {
			out = append(out, cleanLine(s.partial[:consoleMaxLine]))
			s.partial = s.partial[consoleMaxLine:]
		}
		out = append(out, cleanLine(s.partial))
		s.partial = s.partial[:0:0]
	}
	return out
}

func (s *lineSplitter) flush() []string {
	if len(s.partial) == 0 {
		return nil
	}
	l := cleanLine(s.partial)
	s.partial = nil
	return []string{l}
}

func cleanLine(b []byte) string {
	s := ansi.ReplaceAllString(string(b), "")
	s = strings.TrimRight(s, "\r")
	if i := strings.LastIndexByte(s, '\r'); i >= 0 {
		s = s[i+1:]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func RuntimeLinePrefix(name string) string { return "machines: " + name + ": " }

func AgentLinePrefix(name string) string { return "agent: machine " + name + ": " }

func runtimeLineMachine(text string) (string, bool) {
	for _, prefix := range []string{"machines: ", "agent: machine "} {
		rest, ok := strings.CutPrefix(text, prefix)
		if !ok {
			continue
		}
		name, _, ok := strings.Cut(rest, ": ")
		if ok && api.CheckMachineName(name) == nil {
			return name, true
		}
	}
	return "", false
}
