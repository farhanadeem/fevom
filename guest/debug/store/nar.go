package store

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func fdFile(fd int) *os.File { return os.NewFile(uintptr(fd), "nar-file") }

type narReader struct {
	r    *bufio.Reader
	st   unpackStats
	max  int64
	ents int64
}

const maxNarEntries = 1 << 21

type unpackStats struct{ files, dirs, links int64 }

func (n *narReader) u64() (uint64, error) {
	var b [8]byte
	if _, err := io.ReadFull(n.r, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b[:]), nil
}

func (n *narReader) pad(l uint64) error {
	if p := (8 - l%8) % 8; p > 0 {
		var b [8]byte
		_, err := io.ReadFull(n.r, b[:p])
		return err
	}
	return nil
}

func (n *narReader) str() (string, error) {
	l, err := n.u64()
	if err != nil {
		return "", err
	}
	if l > 4096 {
		return "", fmt.Errorf("NAR string of %d bytes", l)
	}
	b := make([]byte, l)
	if _, err := io.ReadFull(n.r, b); err != nil {
		return "", err
	}
	return string(b), n.pad(l)
}

func (n *narReader) expect(want string) error {
	s, err := n.str()
	if err != nil {
		return err
	}
	if s != want {
		return fmt.Errorf("NAR: %q where %q belongs", s, want)
	}
	return nil
}

var epoch = []unix.Timeval{{Sec: 1}, {Sec: 1}}

func (n *narReader) node(parentFd int, name string, depth int) error {
	if depth > MaxNarDepth {
		return fmt.Errorf("NAR: tree deeper than %d", MaxNarDepth)
	}
	if n.ents++; n.ents > maxNarEntries {
		return fmt.Errorf("NAR: more than %d entries", maxNarEntries)
	}
	if err := n.expect("("); err != nil {
		return err
	}
	if err := n.expect("type"); err != nil {
		return err
	}
	t, err := n.str()
	if err != nil {
		return err
	}
	switch t {
	case "regular":
		if err := n.regular(parentFd, name); err != nil {
			return err
		}
	case "symlink":
		if err := n.symlink(parentFd, name); err != nil {
			return err
		}
	case "directory":
		return n.directory(parentFd, name, depth)
	default:
		return fmt.Errorf("NAR: node type %q", t)
	}
	return n.expect(")")
}

func (n *narReader) regular(parentFd int, name string) error {
	s, err := n.str()
	if err != nil {
		return err
	}
	mode := uint32(0o444)
	if s == "executable" {
		if err := n.expect(""); err != nil {
			return err
		}
		mode = 0o555
		if s, err = n.str(); err != nil {
			return err
		}
	}
	if s != "contents" {
		return fmt.Errorf("NAR: %q where contents belongs", s)
	}
	l, err := n.u64()
	if err != nil {
		return err
	}
	if int64(l) > n.max {
		return fmt.Errorf("NAR: contents over the byte budget")
	}
	n.max -= int64(l)
	fd, err := openatBeneath(parentFd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, mode)
	if err != nil {
		return err
	}
	f := fdFile(fd)
	if _, err := io.CopyN(f, n.r, int64(l)); err != nil {
		f.Close()
		return err
	}
	unix.Futimes(fd, epoch)
	if err := f.Close(); err != nil {
		return err
	}
	n.st.files++
	return n.pad(l)
}

func (n *narReader) symlink(parentFd int, name string) error {
	if err := n.expect("target"); err != nil {
		return err
	}
	target, err := n.str()
	if err != nil {
		return err
	}
	if err := unix.Symlinkat(target, parentFd, name); err != nil {
		return err
	}
	n.st.links++
	return nil
}

func (n *narReader) directory(parentFd int, name string, depth int) error {
	if err := unix.Mkdirat(parentFd, name, 0o755); err != nil {
		return err
	}
	fd, err := openatBeneath(parentFd, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	n.st.dirs++
	prev := ""
	for {
		s, err := n.str()
		if err != nil {
			return err
		}
		if s == ")" {
			unix.Futimes(fd, epoch)
			return nil
		}
		if s != "entry" {
			return fmt.Errorf("NAR: %q in a directory", s)
		}
		if err := n.expect("("); err != nil {
			return err
		}
		if err := n.expect("name"); err != nil {
			return err
		}
		ename, err := n.str()
		if err != nil {
			return err
		}
		if ename == "" || ename == "." || ename == ".." || strings.ContainsAny(ename, "/\x00") || ename <= prev {
			return fmt.Errorf("NAR: entry name %q", ename)
		}
		prev = ename
		if err := n.expect("node"); err != nil {
			return err
		}
		if err := n.node(fd, ename, depth+1); err != nil {
			return err
		}
		if err := n.expect(")"); err != nil {
			return err
		}
	}
}

func unpackNAR(r io.Reader, tmpFd int, tmpName string, budget int64) (unpackStats, error) {
	nr := &narReader{r: bufio.NewReaderSize(r, 1<<20), max: budget}
	if err := nr.expect("nix-archive-1"); err != nil {
		return nr.st, err
	}
	if err := nr.node(tmpFd, tmpName, 0); err != nil {
		return nr.st, err
	}
	return nr.st, nil
}
