package nfsview

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const (
	ChannelClient   = "auth.unix.ip"
	ChannelFH       = "nfsd.fh"
	ChannelExport   = "nfsd.export"
	ChannelNameToID = "nfs4.nametoid"
	ChannelIDToName = "nfs4.idtoname"
)

var Channels = []string{ChannelClient, ChannelFH, ChannelExport, ChannelNameToID, ChannelIDToName}

const Domain = "fev"

const fsidNum = 1

type IDMap interface {
	NameToID(group bool, name string) (uint32, bool)
}

const NoID = 4294967294

type MacNames struct {
	UID, GID    uint32
	User, Group string
}

func (m MacNames) NameToID(group bool, name string) (uint32, bool) {
	if n, err := strconv.ParseUint(name, 10, 32); err == nil {
		return uint32(n), true
	}
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	switch {
	case group && name == "wheel", !group && name == "root":
		return 0, true
	case group && name != "" && name == m.Group:
		return m.GID, true
	case !group && name != "" && name == m.User:
		return m.UID, true
	}
	return NoID, true
}

type Answerer struct {
	Table   *Table
	Clients []netip.Addr
	Expiry  time.Duration
	IDs     IDMap
	Now     func() time.Time
}

func (a *Answerer) expiry() string {
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	d := a.Expiry
	if d == 0 {
		d = time.Hour
	}
	return strconv.FormatInt(now().Add(d).Unix(), 10)
}

func (a *Answerer) Answer(channel, line string) (string, error) {
	w := Fields(line)
	exp := a.expiry()
	switch channel {
	case ChannelClient:
		if len(w) < 2 {
			return "", fmt.Errorf("nfsview: %s: %q", channel, line)
		}
		addr, err := netip.ParseAddr(w[1])
		if err == nil && a.isClient(addr.Unmap()) {
			return w[0] + " " + w[1] + " " + exp + " " + Domain, nil
		}
		return w[0] + " " + w[1] + " " + exp, nil
	case ChannelFH:
		if len(w) < 3 {
			return "", fmt.Errorf("nfsview: %s: %q", channel, line)
		}
		reply := w[0] + " " + w[1] + " " + w[2] + " " + exp
		key, err := Unquote(w[2])
		if err != nil {
			return "", fmt.Errorf("nfsview: %s: %q: %v", channel, line, err)
		}
		if w[0] != Domain {
			return reply, nil
		}
		if w[1] != strconv.Itoa(fsidNum) {
			typ, err := strconv.Atoi(w[1])
			if err == nil {
				if p, ok := a.Table.PathOfKey(typ, []byte(key)); ok {
					reply += " " + Quote(p)
				}
			}
			return reply, nil
		}
		if len(key) != 4 {
			return reply, nil
		}
		if p, ok := a.Table.PathOf(binary.LittleEndian.Uint32([]byte(key))); ok {
			reply += " " + Quote(p)
		}
		return reply, nil
	case ChannelExport:
		if len(w) < 2 {
			return "", fmt.Errorf("nfsview: %s: %q", channel, line)
		}
		reply := w[0] + " " + w[1] + " " + exp
		path, err := Unquote(w[1])
		if err != nil {
			return "", fmt.Errorf("nfsview: %s: %q: %v", channel, line, err)
		}
		if w[0] != Domain {
			return reply, nil
		}
		if e, ok := a.Table.Lookup(path); ok {
			reply += fmt.Sprintf(" %d %d %d %d secinfo 1 %d %d", e.Flags, e.AnonUID, e.AnonGID, e.FSID, FlavorSys, e.Flags)
		}
		return reply, nil
	case ChannelNameToID:
		if len(w) < 3 {
			return "", fmt.Errorf("nfsview: %s: %q", channel, line)
		}
		reply := w[0] + " " + w[1] + " " + w[2] + " " + exp
		name, err := Unquote(w[2])
		if err != nil {
			return "", fmt.Errorf("nfsview: %s: %q: %v", channel, line, err)
		}
		if a.IDs != nil {
			if id, ok := a.IDs.NameToID(w[1] == "group", name); ok {
				reply += " " + strconv.FormatUint(uint64(id), 10)
			}
		}
		return reply, nil
	case ChannelIDToName:
		if len(w) < 3 {
			return "", fmt.Errorf("nfsview: %s: %q", channel, line)
		}
		if _, err := strconv.ParseUint(w[2], 10, 32); err != nil {
			return "", fmt.Errorf("nfsview: %s: %q", channel, line)
		}
		return w[0] + " " + w[1] + " " + w[2] + " " + exp + " " + w[2], nil
	}
	return "", fmt.Errorf("nfsview: no channel %q", channel)
}

func (a *Answerer) isClient(addr netip.Addr) bool {
	for _, c := range a.Clients {
		if c == addr {
			return true
		}
	}
	return false
}
