package agent

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

const MaxTerminfo = 64 << 10

const MaxTerminfoName = 128

const (
	terminfoMagic    = 0o432
	terminfoMagic32  = 0o1036
	terminfoHeader   = 12
	terminfoMaxNames = 4096
)

func CheckTerminfoName(name string) error {
	switch {
	case name == "":
		return errors.New("no terminal name")
	case len(name) > MaxTerminfoName:
		return fmt.Errorf("terminal name of %d bytes, more than %d", len(name), MaxTerminfoName)
	case strings.ContainsRune(name, '/'):
		return fmt.Errorf("terminal name %q has a '/'", name)
	case strings.ContainsFunc(name, func(r rune) bool { return r <= ' ' || r == 0x7f }):
		return fmt.Errorf("terminal name %q has a control character or a blank", name)
	case name[0] == '.':
		return fmt.Errorf("terminal name %q starts with '.'", name)
	}
	return nil
}

func CheckTerminfo(name string, b []byte) error {
	if err := CheckTerminfoName(name); err != nil {
		return err
	}
	if len(b) > MaxTerminfo {
		return fmt.Errorf("an entry of %d bytes, more than %d", len(b), MaxTerminfo)
	}
	if len(b) < terminfoHeader {
		return fmt.Errorf("an entry of %d bytes has no header", len(b))
	}
	magic := binary.LittleEndian.Uint16(b[0:])
	if magic != terminfoMagic && magic != terminfoMagic32 {
		return fmt.Errorf("not a compiled terminfo entry (magic %#o)", magic)
	}
	size := int(binary.LittleEndian.Uint16(b[2:]))
	if size < 2 || size > terminfoMaxNames || terminfoHeader+size > len(b) {
		return fmt.Errorf("a names section of %d bytes in an entry of %d", size, len(b))
	}
	names := b[terminfoHeader : terminfoHeader+size]
	if names[size-1] != 0 || bytes.IndexByte(names[:size-1], 0) >= 0 {
		return errors.New("the names section is not one string")
	}
	fields := strings.Split(string(names[:size-1]), "|")
	if len(fields) > 1 {
		fields = fields[:len(fields)-1]
	}
	for _, f := range fields {
		if f == name {
			return nil
		}
	}
	return fmt.Errorf("the entry is for %q, not %q", string(names[:size-1]), name)
}

func TermOf(env []string) string {
	term := ""
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "TERM="); ok {
			term = v
		}
	}
	return term
}

func CheckExecTerminfo(p ExecStartParams) error {
	switch {
	case len(p.Terminfo) == 0:
		return nil
	case p.Context != ExecMachine && p.Context != ExecDebug:
		return fmt.Errorf("a terminfo entry is for context %s or %s", ExecMachine, ExecDebug)
	case !p.TTY && !p.StdinTTY:
		return errors.New("a terminfo entry goes with a terminal")
	case TermOf(p.Env) == "":
		return errors.New("a terminfo entry goes with a TERM")
	case len(p.Terminfo) > MaxTerminfo:
		return fmt.Errorf("a terminfo entry of %d bytes, more than %d", len(p.Terminfo), MaxTerminfo)
	}
	return nil
}
