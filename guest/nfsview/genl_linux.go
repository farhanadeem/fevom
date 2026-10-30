package nfsview

import (
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

const (
	nfsdFamily           = "nfsd"
	nfsdCmdUnlockExport  = 18
	nfsdAttrUnlockPath   = 1
	nfsdFamilyVersion    = 1
	genlHeaderLen        = 4
	nlmsgHeaderLen       = unix.SizeofNlMsghdr
	nlmsgAlign           = 4
	nlattrHeaderLen      = unix.SizeofNlAttr
	ctrlAttrFamilyID     = unix.CTRL_ATTR_FAMILY_ID
	ctrlAttrFamilyName   = unix.CTRL_ATTR_FAMILY_NAME
	ctrlCmdGetFamily     = unix.CTRL_CMD_GETFAMILY
	genlIDCtrl           = unix.GENL_ID_CTRL
	nlmsgError           = unix.NLMSG_ERROR
	nlmFRequest          = unix.NLM_F_REQUEST
	nlmFAck              = unix.NLM_F_ACK
	maxNetlinkAnswerSize = 1 << 16
)

func align4(n int) int { return (n + nlmsgAlign - 1) &^ (nlmsgAlign - 1) }

func genlMessage(family uint16, cmd uint8, seq uint32, attrs map[uint16]string) []byte {
	body := []byte{cmd, nfsdFamilyVersion, 0, 0}
	for typ, v := range attrs {
		a := make([]byte, align4(nlattrHeaderLen+len(v)+1))
		binary.NativeEndian.PutUint16(a[0:], uint16(nlattrHeaderLen+len(v)+1))
		binary.NativeEndian.PutUint16(a[2:], typ)
		copy(a[nlattrHeaderLen:], v)
		body = append(body, a...)
	}
	m := make([]byte, nlmsgHeaderLen, nlmsgHeaderLen+len(body))
	binary.NativeEndian.PutUint32(m[0:], uint32(nlmsgHeaderLen+len(body)))
	binary.NativeEndian.PutUint16(m[4:], family)
	binary.NativeEndian.PutUint16(m[6:], nlmFRequest|nlmFAck)
	binary.NativeEndian.PutUint32(m[8:], seq)
	return append(m, body...)
}

func genlAnswer(b []byte, seq uint32) (map[uint16][]byte, error) {
	var attrs map[uint16][]byte
	for len(b) >= nlmsgHeaderLen {
		n := int(binary.NativeEndian.Uint32(b[0:]))
		typ := binary.NativeEndian.Uint16(b[4:])
		if n < nlmsgHeaderLen || n > len(b) {
			return attrs, errors.New("nfsview: a netlink answer cut short")
		}
		msg := b[nlmsgHeaderLen:n]
		if binary.NativeEndian.Uint32(b[8:]) == seq {
			if typ == nlmsgError {
				if len(msg) < 4 {
					return attrs, errors.New("nfsview: a netlink error cut short")
				}
				if e := int32(binary.NativeEndian.Uint32(msg)); e != 0 {
					return attrs, unix.Errno(-e)
				}
				return attrs, nil
			}
			if attrs == nil && len(msg) >= genlHeaderLen {
				attrs = map[uint16][]byte{}
				for a := msg[genlHeaderLen:]; len(a) >= nlattrHeaderLen; {
					l := int(binary.NativeEndian.Uint16(a[0:]))
					if l < nlattrHeaderLen || l > len(a) {
						break
					}
					attrs[binary.NativeEndian.Uint16(a[2:])&0x3fff] = a[nlattrHeaderLen:l]
					a = a[min(align4(l), len(a)):]
				}
			}
		}
		b = b[min(align4(n), len(b)):]
	}
	return attrs, errNoAck
}

func genlCall(family uint16, cmd uint8, attrs map[uint16]string) (map[uint16][]byte, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_GENERIC)
	if err != nil {
		return nil, fmt.Errorf("nfsview: a generic netlink socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, err
	}
	const seq = 1
	if err := unix.Sendto(fd, genlMessage(family, cmd, seq, attrs), 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, err
	}
	buf := make([]byte, maxNetlinkAnswerSize)
	var got []byte
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		got = append(got, buf[:n]...)
		attrs, err := genlAnswer(got, seq)
		if !errors.Is(err, errNoAck) {
			return attrs, err
		}
	}
}

var errNoAck = errors.New("nfsview: no acknowledgement from netlink")

func nfsdFamilyID() (uint16, error) {
	attrs, err := genlCall(genlIDCtrl, ctrlCmdGetFamily, map[uint16]string{ctrlAttrFamilyName: nfsdFamily})
	if err != nil {
		return 0, fmt.Errorf("nfsview: nfsd's netlink family: %w", err)
	}
	id, ok := attrs[ctrlAttrFamilyID]
	if !ok || len(id) < 2 {
		return 0, errors.New("nfsview: nfsd's netlink family has no id")
	}
	return binary.NativeEndian.Uint16(id), nil
}

func unlockExport(path string) error {
	fam, err := nfsdFamilyID()
	if err != nil {
		return err
	}
	_, err = genlCall(fam, nfsdCmdUnlockExport, map[uint16]string{nfsdAttrUnlockPath: path})
	return err
}
