package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func fileOf(v any) (*os.File, bool) {
	f, ok := v.(*os.File)
	return f, ok
}

func terminal(v any) bool {
	f, ok := fileOf(v)
	return ok && isTerminal(f)
}

func rawMode(f *os.File) (restore func(), err error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TIOCSETA, &raw); err != nil {
		return nil, err
	}
	return func() { unix.IoctlSetTermios(fd, unix.TIOCSETA, old) }, nil
}

func termSize(f *os.File) (rows, cols int) {
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0
	}
	return int(ws.Row), int(ws.Col)
}

func readSecret(in io.Reader, stderr io.Writer, prompt string) (string, error) {
	f, ok := fileOf(in)
	if !ok || !isTerminal(f) {
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			return "", fmt.Errorf("no password on standard input")
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return "", err
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	quiet.Lflag |= unix.ICANON | unix.ISIG
	if err := unix.IoctlSetTermios(fd, unix.TIOCSETA, &quiet); err != nil {
		return "", err
	}
	defer unix.IoctlSetTermios(fd, unix.TIOCSETA, old)
	fmt.Fprint(stderr, prompt)
	var line []byte
	var b [1]byte
	for {
		n, err := f.Read(b[:])
		if n == 1 {
			if b[0] == '\n' || b[0] == '\r' {
				break
			}
			line = append(line, b[0])
		}
		if err != nil {
			fmt.Fprintln(stderr)
			return "", err
		}
	}
	fmt.Fprintln(stderr)
	return string(line), nil
}
