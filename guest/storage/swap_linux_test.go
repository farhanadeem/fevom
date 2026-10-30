package storage

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestSwapHeaderAndTheSwapDiskByItsLabel(t *testing.T) {
	page := SwapHeader(1<<30, 4096, SwapLabel, [16]byte{1})
	if string(page[4086:]) != "SWAPSPACE2" || binary.LittleEndian.Uint32(page[1024:]) != 1 ||
		binary.LittleEndian.Uint32(page[1028:]) != (1<<30)/4096-1 {
		t.Fatalf("header: %x", page[1024:1040])
	}
	if l, ok := swapLabelOf(page); !ok || l != SwapLabel {
		t.Fatalf("label %q %v", l, ok)
	}
	if _, ok := swapLabelOf(make([]byte, 4096)); ok {
		t.Fatal("a page of zeros has a swap header")
	}
	root := t.TempDir()
	sys, dev := filepath.Join(root, "sys"), filepath.Join(root, "dev")
	for _, d := range []string{"vda", "vdb", "vdc"} {
		os.MkdirAll(filepath.Join(sys, "block", d), 0o755)
	}
	os.MkdirAll(dev, 0o755)
	os.WriteFile(filepath.Join(dev, "vda"), page, 0o600)
	os.WriteFile(filepath.Join(dev, "vdb"), make([]byte, 8192), 0o600)
	os.WriteFile(filepath.Join(dev, "vdc"), page, 0o600)
	got, err := findSwapDisk(SwapConfig{Sys: sys, Dev: dev, Skip: []string{"/dev/vda"}})
	if err != nil || got != "vdc" {
		t.Fatalf("found %q %v, want vdc", got, err)
	}
	os.WriteFile(filepath.Join(dev, "vdc"), SwapHeader(1<<30, 4096, "other", [16]byte{}), 0o600)
	if got, _ := findSwapDisk(SwapConfig{Sys: sys, Dev: dev, Skip: []string{"/dev/vda"}}); got != "" {
		t.Fatalf("a swap header with another label was taken: %q", got)
	}
}

func TestSwapDevicesFromProcSwaps(t *testing.T) {
	root := t.TempDir()
	proc, sys := filepath.Join(root, "proc"), filepath.Join(root, "sys")
	os.MkdirAll(proc, 0o755)
	os.MkdirAll(filepath.Join(sys, "block/zram0"), 0o755)
	os.WriteFile(filepath.Join(proc, "swaps"), []byte("Filename\tType\tSize\tUsed\tPriority\n"+
		"/dev/vdc                                partition\t1048572\t2048\t1\n"+
		"/dev/zram0                              partition\t2025984\t524288\t32767\n"), 0o644)
	os.WriteFile(filepath.Join(sys, "block/zram0/comp_algorithm"), []byte("lzo-rle [lz4]\n"), 0o644)
	os.WriteFile(filepath.Join(sys, "block/zram0/mm_stat"), []byte("536870912 178956970 180000000 0 180000000 0 0 0 0\n"), 0o644)
	d, err := SwapDevices(proc, sys)
	if err != nil || len(d) != 2 {
		t.Fatalf("%+v %v", d, err)
	}
	if d[0].Kind != "zram" || d[0].Algorithm != "lz4" || d[0].StoredBytes != 178956970 || d[0].UsedBytes != 524288<<10 || d[0].Priority != 32767 {
		t.Fatalf("zram: %+v", d[0])
	}
	if d[1].Kind != "disk" || d[1].Device != "/dev/vdc" || d[1].SizeBytes != 1048572<<10 || d[1].Algorithm != "" {
		t.Fatalf("disk: %+v", d[1])
	}
}
