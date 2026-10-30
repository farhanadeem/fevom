package registry

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func PeerInode(c net.Conn) (uint64, error) {
	local, ok1 := c.LocalAddr().(*net.TCPAddr)
	remote, ok2 := c.RemoteAddr().(*net.TCPAddr)
	if !ok1 || !ok2 || remote.IP.To4() == nil || local.IP.To4() == nil {
		return 0, errors.New("not an IPv4 TCP connection")
	}
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	req := make([]byte, 16+56)
	binary.LittleEndian.PutUint32(req[0:], uint32(len(req)))
	binary.LittleEndian.PutUint16(req[4:], unix.SOCK_DIAG_BY_FAMILY)
	binary.LittleEndian.PutUint16(req[6:], unix.NLM_F_REQUEST)
	binary.LittleEndian.PutUint32(req[8:], 1)
	r := req[16:]
	r[0], r[1] = unix.AF_INET, unix.IPPROTO_TCP
	binary.LittleEndian.PutUint32(r[4:], 0xffffffff)
	id := r[8:]
	binary.BigEndian.PutUint16(id[0:], uint16(remote.Port))
	binary.BigEndian.PutUint16(id[2:], uint16(local.Port))
	copy(id[4:8], remote.IP.To4())
	copy(id[20:24], local.IP.To4())
	binary.LittleEndian.PutUint32(id[40:], 0xffffffff)
	binary.LittleEndian.PutUint32(id[44:], 0xffffffff)
	if err := unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return 0, err
	}
	buf := make([]byte, 8192)
	n, _, err := unix.Recvfrom(fd, buf, 0)
	if err != nil {
		return 0, err
	}
	if n < 16 {
		return 0, errors.New("sock_diag: a short answer")
	}
	typ := binary.LittleEndian.Uint16(buf[4:])
	if typ == unix.NLMSG_ERROR {
		code := int32(binary.LittleEndian.Uint32(buf[16:]))
		return 0, fmt.Errorf("sock_diag: %w", unix.Errno(-code))
	}
	if typ != unix.SOCK_DIAG_BY_FAMILY || n < 16+72 {
		return 0, fmt.Errorf("sock_diag: answer of type %d", typ)
	}
	msg := buf[16:]
	return uint64(binary.LittleEndian.Uint32(msg[68:])), nil
}

func InLeaf(inode uint64, procs, proc string) (bool, error) {
	data, err := os.ReadFile(procs)
	if err != nil {
		return false, err
	}
	want := "socket:[" + strconv.FormatUint(inode, 10) + "]"
	for _, p := range strings.Fields(string(data)) {
		dir := filepath.Join(proc, p, "fd")
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if l, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil && l == want {
				return true, nil
			}
		}
	}
	return false, nil
}

func Peer(procs string) func(net.Conn) (bool, error) {
	return func(c net.Conn) (bool, error) {
		inode, err := PeerInode(c)
		if err != nil {
			return false, err
		}
		return InLeaf(inode, procs, "/proc")
	}
}
