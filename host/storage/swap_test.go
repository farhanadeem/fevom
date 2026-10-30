package storage

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureMakesTheSwapImageAndResetRemovesIt(t *testing.T) {
	home := t.TempDir()
	path := ImagePath(home)
	if _, err := Ensure(path, Options{Size: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	swap := filepath.Join(home, "data", SwapImageName)
	st, err := os.Stat(swap)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != SwapSize || st.Mode().Perm() != 0o600 {
		t.Fatalf("swap image: size %d mode %v", st.Size(), st.Mode())
	}
	if n, _ := Allocated(swap); n > 64<<10 {
		t.Fatalf("a new swap image occupies %d bytes", n)
	}
	if !hasSwapHeader(swap) {
		t.Fatal("no swap header with fev's label")
	}
	b, _ := os.ReadFile(swap)
	if last := binary.LittleEndian.Uint32(b[1028:]); last != SwapSize/4096-1 {
		t.Fatalf("last page %d", last)
	}
	if n := binary.LittleEndian.Uint32(b[1032:]); n != 511 {
		t.Fatalf("%d bad pages", n)
	}
	for i := uint32(1); i < 512; i++ {
		if got := binary.LittleEndian.Uint32(b[1536+4*(i-1):]); got != i {
			t.Fatalf("bad page %d listed as %d", i, got)
		}
	}
	if 1536+4*511 > 4096-10 {
		t.Fatal("the bad pages run into the magic")
	}
	if ex, _ := BackupExcluded(swap); !ex {
		t.Fatal("the swap image is not excluded from backups")
	}
	f, _ := os.OpenFile(swap, os.O_WRONLY, 0)
	f.WriteAt(make([]byte, 4096), 0)
	f.Close()
	if _, err := Ensure(path, Options{Size: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if !hasSwapHeader(swap) {
		t.Fatal("a swap image without its header was not made again")
	}
	os.Truncate(swap, 1<<20)
	Ensure(path, Options{Size: 1 << 30})
	if st, _ := os.Stat(swap); st.Size() != SwapSize {
		t.Fatalf("a swap image of the wrong size was kept: %d", st.Size())
	}
	if err := Reset(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(swap); err == nil {
		t.Fatal("reset left the swap image")
	}
}

func TestASwapImageThatCannotBeMadeIsNotTheDataImagesFailure(t *testing.T) {
	home := t.TempDir()
	path := ImagePath(home)
	swap := filepath.Join(home, "data", SwapImageName)
	if err := os.MkdirAll(filepath.Join(swap, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	var said []error
	created, err := Ensure(path, Options{Size: 1 << 30, Swap: func(err error) { said = append(said, err) }})
	if err != nil || !created {
		t.Fatalf("the data image with no swap image: created %v, %v", created, err)
	}
	if len(said) != 1 || said[0] == nil {
		t.Fatalf("the swap image's failure was told as %v", said)
	}
	if st, err := os.Stat(swap); err == nil && st.Mode().IsRegular() {
		t.Fatal("a swap image is there to attach")
	}
	os.RemoveAll(swap)
	said = nil
	if _, err := Ensure(path, Options{Size: 1 << 30, Swap: func(err error) { said = append(said, err) }}); err != nil {
		t.Fatal(err)
	}
	if len(said) != 1 || said[0] != nil || !hasSwapHeader(swap) {
		t.Fatalf("made again: %v", said)
	}
}
