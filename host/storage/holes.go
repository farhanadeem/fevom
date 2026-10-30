package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const holeProbe = 1 << 20

var ErrNoHoles = errors.New("the volume does not keep a file's holes")

func KeepsHoles(dir string) error {
	f, err := os.CreateTemp(dir, ".fev-holes-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Truncate(holeProbe); err != nil {
		return err
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	if st.Blocks*512 < holeProbe/2 {
		return nil
	}
	kind := "?"
	var fs unix.Statfs_t
	if unix.Statfs(dir, &fs) == nil {
		kind = unix.ByteSliceToString(fs.Fstypename[:])
	}
	abs, _ := filepath.Abs(dir)
	return fmt.Errorf("%s (%s): %w, and a data image there would take its whole %d GiB at once; put fev's data on an APFS volume (Disk Utility formats an external disk as APFS)",
		abs, kind, ErrNoHoles, DefaultSize>>30)
}
