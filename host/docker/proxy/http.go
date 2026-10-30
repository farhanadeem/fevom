package proxy

import (
	"bytes"
	"strconv"
	"strings"
)

const maxHeader = 64 << 10

type class int

const (
	stream class = iota
	read
	work
	upgraded
	removal
)

func classify(method, target string) (string, class) {
	path, query := target, ""
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path, query = path[:i], path[i+1:]
	}
	if strings.HasPrefix(path, "/v") {
		if i := strings.IndexByte(path[1:], '/'); i > 0 {
			if _, err := strconv.ParseFloat(path[2:i+1], 64); err == nil {
				path = path[i+1:]
			}
		}
	}
	seg := strings.Split(strings.Trim(path, "/"), "/")
	last := seg[len(seg)-1]
	containers := seg[0] == "containers" && len(seg) >= 3
	get := method == "GET" || method == "HEAD"
	switch {
	case method == "POST" && path == "/images/create":
		if strings.Contains(query, "fromSrc=") {
			return "an image import", work
		}
		return "a pull", work
	case method == "POST" && path == "/images/load":
		return "an image load", work
	case method == "GET" && (path == "/images/get" || (len(seg) >= 3 && seg[0] == "images" && last == "get")):
		return "an image save", work
	case method == "POST" && len(seg) >= 3 && seg[0] == "images" && last == "push":
		return "a push", work
	case method == "GET" && len(seg) == 3 && seg[0] == "containers" && last == "export":
		return "a container export", work
	case containers && last == "archive":
		return "a container copy", work
	case method == "POST" && path == "/commit":
		return "a commit", work
	case method == "POST" && path == "/build":
		return "a build", work
	case method == "POST" && (path == "/session" || path == "/grpc"):
		return "a build", upgraded
	case method == "POST" && containers && last == "wait":
		if queryValue(query, "condition") == "removed" {
			return "a --rm container", removal
		}
		return "", stream
	case containers && (last == "attach" || (last == "ws" && seg[len(seg)-2] == "attach")):
		return "", stream
	case get && path == "/events":
		return "", stream
	case get && containers && last == "logs" && truthy(queryValue(query, "follow")):
		return "", stream
	case get && containers && last == "stats" && queryValue(query, "stream") != "0" && queryValue(query, "stream") != "false":
		return "", stream
	case get:
		return "a Docker request", read
	}
	return "a Docker request", work
}

func queryValue(query, name string) string {
	for _, kv := range strings.Split(query, "&") {
		k, v, _ := strings.Cut(kv, "=")
		if k == name {
			return v
		}
	}
	return ""
}

func truthy(v string) bool { return v == "1" || v == "true" || v == "True" }

func hijacks(method, target string) bool {
	if method != "POST" {
		return false
	}
	path := target
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	return strings.HasSuffix(path, "/attach") || (strings.Contains(path, "/exec/") && strings.HasSuffix(path, "/start"))
}

type phase int

const (
	inHeader phase = iota
	inBody
	inChunkSize
	inChunkData
	inChunkEnd
	inTrailer
	inClose
	inRaw
)

type framer struct {
	response bool
	phase    phase
	head     []byte
	left     int64
	line     []byte
}

type message struct {
	method, target string
	status         int
	upgrade        bool
}

func (f *framer) feed(b []byte, header func(m message, h []byte) (hasBody bool, raw bool), done func()) {
	for len(b) > 0 {
		switch f.phase {
		case inRaw, inClose:
			return
		case inHeader:
			if len(f.head) == 0 {
				for len(b) > 0 && (b[0] == '\r' || b[0] == '\n') {
					b = b[1:]
				}
				if len(b) == 0 {
					return
				}
			}
			start := len(f.head)
			f.head = append(f.head, b...)
			from := max(start-3, 0)
			i := bytes.Index(f.head[from:], []byte("\r\n\r\n"))
			if i < 0 {
				if len(f.head) > maxHeader {
					f.phase, f.head = inRaw, nil
				}
				return
			}
			end := from + i + 4
			consumed := end - start
			h := f.head[:end]
			m, ok := parseHeader(h, f.response)
			if !ok {
				f.phase, f.head = inRaw, nil
				return
			}
			b = b[consumed:]
			hasBody, raw := header(m, h)
			f.head = f.head[:0]
			switch {
			case raw:
				f.phase = inRaw
				return
			case !hasBody:
				done()
				continue
			}
			te := strings.ToLower(headerValue(h, "transfer-encoding"))
			cl := headerValue(h, "content-length")
			switch {
			case te != "" && strings.HasSuffix(strings.TrimSpace(te), "chunked"):
				f.phase, f.line = inChunkSize, f.line[:0]
			case cl != "":
				n, err := strconv.ParseInt(strings.TrimSpace(cl), 10, 64)
				if err != nil || n < 0 {
					f.phase = inRaw
					return
				}
				if n == 0 {
					done()
					continue
				}
				f.phase, f.left = inBody, n
			case f.response:
				f.phase = inClose
				return
			default:
				done()
			}
		case inBody:
			n := min(int64(len(b)), f.left)
			b, f.left = b[n:], f.left-n
			if f.left == 0 {
				f.phase = inHeader
				done()
			}
		case inChunkSize, inTrailer:
			i := bytes.IndexByte(b, '\n')
			if i < 0 {
				f.line = append(f.line, b...)
				if len(f.line) > maxHeader {
					f.phase = inRaw
				}
				return
			}
			f.line = append(f.line, b[:i]...)
			b = b[i+1:]
			line := strings.TrimSpace(string(f.line))
			f.line = f.line[:0]
			if f.phase == inTrailer {
				if line == "" {
					f.phase = inHeader
					done()
				}
				continue
			}
			if j := strings.IndexByte(line, ';'); j >= 0 {
				line = line[:j]
			}
			n, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
			if err != nil || n < 0 {
				f.phase = inRaw
				return
			}
			if n == 0 {
				f.phase = inTrailer
				continue
			}
			f.phase, f.left = inChunkData, n
		case inChunkData:
			n := min(int64(len(b)), f.left)
			b, f.left = b[n:], f.left-n
			if f.left == 0 {
				f.phase, f.left = inChunkEnd, 2
			}
		case inChunkEnd:
			n := min(int64(len(b)), f.left)
			b, f.left = b[n:], f.left-n
			if f.left == 0 {
				f.phase = inChunkSize
			}
		}
	}
}

func parseHeader(h []byte, response bool) (message, bool) {
	line := h
	if i := bytes.Index(h, []byte("\r\n")); i >= 0 {
		line = h[:i]
	}
	f := strings.Fields(string(line))
	var m message
	if response {
		if len(f) < 2 || !strings.HasPrefix(f[0], "HTTP/1.") {
			return m, false
		}
		n, err := strconv.Atoi(f[1])
		if err != nil {
			return m, false
		}
		m.status = n
	} else {
		if len(f) != 3 || !strings.HasPrefix(f[2], "HTTP/1.") {
			return m, false
		}
		m.method, m.target = f[0], f[1]
	}
	m.upgrade = headerValue(h, "upgrade") != ""
	return m, true
}

func headerValue(h []byte, name string) string {
	for _, l := range bytes.Split(h, []byte("\r\n"))[1:] {
		k, v, ok := bytes.Cut(l, []byte(":"))
		if ok && strings.EqualFold(strings.TrimSpace(string(k)), name) {
			return strings.TrimSpace(string(v))
		}
	}
	return ""
}
