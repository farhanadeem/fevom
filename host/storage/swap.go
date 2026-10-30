package storage

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	SwapImageName = "swap.img"
	SwapSize      = 1 << 30
	SwapLabel     = "fev-swap"
	swapPage      = 4096
)

func SwapImagePath(dataImage string) string {
	return filepath.Join(filepath.Dir(dataImage), SwapImageName)
}

const swapCluster = 512

func SwapHeader(size int64, label string) []byte {
	page := make([]byte, swapPage)
	binary.LittleEndian.PutUint32(page[1024:], 1)
	binary.LittleEndian.PutUint32(page[1028:], uint32(size/swapPage)-1)
	binary.LittleEndian.PutUint32(page[1032:], swapCluster-1)
	rand.Read(page[1036:1052])
	copy(page[1052:1068], label)
	for i := uint32(1); i < swapCluster; i++ {
		binary.LittleEndian.PutUint32(page[1536+4*(i-1):], i)
	}
	copy(page[swapPage-10:], "SWAPSPACE2")
	return page
}

func ensureSwap(dataImage string) error {
	path := SwapImagePath(dataImage)
	if st, err := os.Lstat(path); err == nil && st.Mode().IsRegular() && st.Size() == SwapSize && hasSwapHeader(path) {
		return SetBackupExcluded(path, true)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	os.Remove(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".new"
	os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("make the swap image %s: %w", path, err)
	}
	if err := f.Truncate(SwapSize); err != nil {
		return fail(err)
	}
	if _, err := f.WriteAt(SwapHeader(SwapSize, SwapLabel), 0); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	if err := SetBackupExcluded(tmp, true); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func hasSwapHeader(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	page := make([]byte, swapPage)
	if n, _ := f.ReadAt(page, 0); n != swapPage {
		return false
	}
	return string(page[swapPage-10:]) == "SWAPSPACE2" &&
		binary.LittleEndian.Uint32(page[1024:]) == 1 &&
		binary.LittleEndian.Uint32(page[1032:]) == swapCluster-1 &&
		string(bytes.TrimRight(page[1052:1068], "\x00")) == SwapLabel
}
