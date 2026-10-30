package nfsview

import (
	"encoding/binary"
	"fmt"
)

const (
	CLDMsgLen   = 8 + 2 + 1024
	CLDMsgLenV2 = 8 + 2 + 1024 + 1 + 32
	CLDVersion  = 2
)

const (
	CLDCreate     = 0
	CLDRemove     = 1
	CLDCheck      = 2
	CLDGraceDone  = 3
	CLDGraceStart = 4
	CLDGetVersion = 5
)

func CLDCommandName(cmd byte) string {
	switch cmd {
	case CLDCreate:
		return "create"
	case CLDRemove:
		return "remove"
	case CLDCheck:
		return "check"
	case CLDGraceDone:
		return "grace_done"
	case CLDGraceStart:
		return "grace_start"
	case CLDGetVersion:
		return "get_version"
	}
	return fmt.Sprintf("command %d", cmd)
}

const (
	statusNoEntry      = -2
	statusNotSupported = -95
)

func AnswerCLD(msg []byte) ([]byte, byte, error) {
	if len(msg) != CLDMsgLen && len(msg) != CLDMsgLenV2 {
		return nil, 0, fmt.Errorf("nfsview: a tracking upcall of %d bytes", len(msg))
	}
	out := append([]byte(nil), msg...)
	cmd := out[1]
	var status int16
	switch cmd {
	case CLDGetVersion:
		out[8] = CLDVersion
	case CLDCheck:
		status = statusNoEntry
	case CLDCreate, CLDRemove, CLDGraceDone, CLDGraceStart:
	default:
		status = statusNotSupported
	}
	binary.LittleEndian.PutUint16(out[2:4], uint16(status))
	return out, cmd, nil
}
