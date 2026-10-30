package logs

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const fileTime = "2006-01-02T15:04:05.000000Z"

func parseLine(line string) (Entry, bool) {
	parts := strings.SplitN(line, " ", 4)
	if len(parts) < 3 {
		return Entry{}, false
	}
	at, err := time.Parse(fileTime, parts[0])
	if err != nil {
		return Entry{}, false
	}
	level, err := strconv.Atoi(parts[2])
	if err != nil || level < 0 || level > 7 {
		return Entry{}, false
	}
	e := Entry{Time: at, Source: parts[1], Level: level}
	if len(parts) == 4 {
		e.Text = parts[3]
	}
	return e, true
}

type fileReader struct {
	paths []string
	f     *os.File
	sc    *bufio.Scanner
	next  Entry
	ok    bool
}

func (r *fileReader) advance() {
	for {
		if r.sc == nil {
			if len(r.paths) == 0 {
				r.ok = false
				return
			}
			f, err := os.Open(r.paths[0])
			r.paths = r.paths[1:]
			if err != nil {
				continue
			}
			r.f, r.sc = f, bufio.NewScanner(f)
			r.sc.Buffer(make([]byte, 64<<10), MaxLine+4096)
		}
		for r.sc.Scan() {
			if e, ok := parseLine(r.sc.Text()); ok {
				r.next, r.ok = e, true
				return
			}
		}
		r.f.Close()
		r.f, r.sc = nil, nil
	}
}

func ReadAll(dir string, sources []string, fn func(Entry)) error {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, s := range sources {
		want[s] = true
	}
	var readers []*fileReader
	for _, name := range Files() {
		r := &fileReader{}
		for g := Generations; g >= 1; g-- {
			r.paths = append(r.paths, filepath.Join(dir, fmt.Sprintf("%s.%d", name, g)))
		}
		r.paths = append(r.paths, filepath.Join(dir, name))
		r.advance()
		if r.ok {
			readers = append(readers, r)
		}
	}
	defer func() {
		for _, r := range readers {
			if r.f != nil {
				r.f.Close()
			}
		}
	}()
	for {
		var first *fileReader
		for _, r := range readers {
			if r.ok && (first == nil || r.next.Time.Before(first.next.Time)) {
				first = r
			}
		}
		if first == nil {
			return nil
		}
		if len(want) == 0 || want[first.next.Source] {
			fn(first.next)
		}
		first.advance()
	}
}
