package registry

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestThePeerIsFoundByItsSocket(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	a, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	inode, err := PeerInode(a)
	if err != nil || inode == 0 {
		t.Fatalf("inode %d %v", inode, err)
	}
	dir := t.TempDir()
	mine := filepath.Join(dir, "mine")
	os.WriteFile(mine, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
	if ok, err := InLeaf(inode, mine, "/proc"); !ok || err != nil {
		t.Errorf("in this process's leaf: %v %v", ok, err)
	}
	other := filepath.Join(dir, "other")
	os.WriteFile(other, []byte("999999999\n"), 0o644)
	if ok, err := InLeaf(inode, other, "/proc"); ok || err != nil {
		t.Errorf("in another's leaf: %v %v", ok, err)
	}
	self, err := PeerInode(c)
	if err != nil || self == inode {
		t.Errorf("the other end %d (%v), the peer %d", self, err, inode)
	}
}
