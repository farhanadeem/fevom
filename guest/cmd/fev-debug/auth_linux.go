package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func authzPeer(sessionsDir string) func(net.Conn) error {
	return func(conn net.Conn) error {
		uc, ok := conn.(*net.UnixConn)
		if !ok {
			return fmt.Errorf("not a Unix socket")
		}
		pid, uid, err := peerCred(uc)
		if err != nil {
			return err
		}
		roots, err := sessionRoots(sessionsDir)
		if err != nil || len(roots) == 0 {
			return fmt.Errorf("no debug session is open")
		}
		if pidInTree(pid, roots) {
			return nil
		}
		return fmt.Errorf("peer pid %d (uid %d) is not in a debug session's process tree", pid, uid)
	}
}

func peerCred(uc *net.UnixConn) (pid, uid int, err error) {
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cred *unix.Ucred
	var serr error
	if cerr := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); cerr != nil {
		return 0, 0, cerr
	}
	if serr != nil {
		return 0, 0, serr
	}
	return int(cred.Pid), int(cred.Uid), nil
}

func sessionRoots(dir string) (map[int]bool, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	roots := map[int]bool{}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
			roots[pid] = true
		}
	}
	return roots, nil
}

func pidInTree(pid int, roots map[int]bool) bool {
	for i := 0; i < 256 && pid > 1; i++ {
		if roots[pid] {
			return true
		}
		ppid, ok := parentPID(pid)
		if !ok {
			return false
		}
		pid = ppid
	}
	return roots[pid]
}

func parentPID(pid int) (int, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	s := string(b)
	close := strings.LastIndexByte(s, ')')
	if close < 0 || close+2 >= len(s) {
		return 0, false
	}
	fields := strings.Fields(s[close+2:])
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}
