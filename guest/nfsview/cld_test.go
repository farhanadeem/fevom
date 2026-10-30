package nfsview

import (
	"encoding/binary"
	"testing"
)

func cldMsg(n int, cmd byte, xid uint32) []byte {
	m := make([]byte, n)
	m[0], m[1] = 1, cmd
	binary.LittleEndian.PutUint16(m[2:], 0xffff)
	binary.LittleEndian.PutUint32(m[4:], xid)
	return m
}

func TestTheTrackingUpcallSkipsTheGrace(t *testing.T) {
	for _, c := range []struct {
		n      int
		cmd    byte
		status int16
	}{
		{CLDMsgLen, CLDGetVersion, 0},
		{CLDMsgLenV2, CLDGraceStart, 0},
		{CLDMsgLenV2, CLDGraceDone, 0},
		{CLDMsgLenV2, CLDCreate, 0},
		{CLDMsgLenV2, CLDRemove, 0},
		{CLDMsgLenV2, CLDCheck, statusNoEntry},
		{CLDMsgLenV2, 9, statusNotSupported},
	} {
		in := cldMsg(c.n, c.cmd, 77)
		out, cmd, err := AnswerCLD(in)
		if err != nil || cmd != c.cmd || len(out) != c.n {
			t.Fatalf("%s: %d bytes, %v", CLDCommandName(c.cmd), len(out), err)
		}
		if s := int16(binary.LittleEndian.Uint16(out[2:])); s != c.status {
			t.Errorf("%s: status %d, want %d", CLDCommandName(c.cmd), s, c.status)
		}
		if binary.LittleEndian.Uint32(out[4:]) != 77 {
			t.Errorf("%s: xid lost", CLDCommandName(c.cmd))
		}
		if c.cmd == CLDGetVersion && out[8] != CLDVersion {
			t.Errorf("version %d", out[8])
		}
	}
	if CLDMsgLen != 1034 || CLDMsgLenV2 != 1067 {
		t.Errorf("message lengths %d %d (measured: 1034 and 1067)", CLDMsgLen, CLDMsgLenV2)
	}
	if _, _, err := AnswerCLD(make([]byte, 100)); err == nil {
		t.Error("a message of the wrong length was answered")
	}
}
