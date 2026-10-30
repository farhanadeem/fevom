package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var moveData = []int64{0, 100 << 30, (1 << 40) - (8 << 20)}

const moveZeros = int64(300 << 30)

func makeMoveImage(t *testing.T, dir string) (string, map[int64][]byte) {
	t.Helper()
	path := filepath.Join(dir, ImageName)
	if _, err := Ensure(path, Options{}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	want := map[int64][]byte{}
	for _, off := range moveData {
		b := make([]byte, 4<<20)
		rand.Read(b)
		if _, err := f.WriteAt(b, off); err != nil {
			t.Fatal(err)
		}
		want[off] = b
	}
	if _, err := f.WriteAt(make([]byte, 4<<20), moveZeros); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(SwapImagePath(path)); err != nil {
		t.Fatalf("no swap image beside the source: %v", err)
	}
	return path, want
}

func TestAnImageMovesToAnotherVolumeWithItsHoles(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "excluded", true: "backed-up"}[allow], func(t *testing.T) {
			src, want := makeMoveImage(t, t.TempDir())
			srcAlloc, _ := Allocated(src)
			mnt := volumeOf(t, "APFS", "128m")
			dstDir := filepath.Join(mnt, "fev", "data")
			committed := false
			var last int64
			res, err := Move(context.Background(), src, dstDir, MoveOptions{
				AllowBackup: allow, Margin: 1 << 20,
				Progress: func(done, _ int64) { last = done },
				Committed: func() error {
					if _, err := os.Stat(src); err != nil {
						t.Errorf("the old image was gone before the new place was noted")
					}
					committed = true
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(dstDir, ImageName)
			if !committed || res.Renamed {
				t.Errorf("committed %v renamed %v", committed, res.Renamed)
			}
			st, err := os.Stat(dst)
			if err != nil || st.Size() != DefaultSize {
				t.Fatalf("the moved image: %v %v", st, err)
			}
			f, _ := os.Open(dst)
			defer f.Close()
			for off, b := range want {
				got := make([]byte, len(b))
				if _, err := f.ReadAt(got, off); err != nil || !bytes.Equal(got, b) {
					t.Errorf("data at %d differs after the move (%v)", off, err)
				}
			}
			alloc, _ := Allocated(dst)
			data := int64(len(moveData)) * 4 << 20
			t.Logf("source %d MiB allocated, moved %d MiB allocated, %d MiB written, %d MiB looked at", srcAlloc>>20, alloc>>20, res.Copied>>20, last>>20)
			if res.Copied != data || alloc > data+(1<<20) || alloc != res.Allocated {
				t.Errorf("the moved image occupies %d bytes (%d written; its data is %d)", alloc, res.Copied, data)
			}
			if ex, _ := BackupExcluded(dst); ex == allow {
				t.Errorf("backup excluded %v with allow %v", ex, allow)
			}
			for _, gone := range []string{src, SwapImagePath(src), dst + ".moving"} {
				if _, err := os.Lstat(gone); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s is still there", gone)
				}
			}
		})
	}
}

func TestAMoveThatCannotBeMadeLeavesTheImage(t *testing.T) {
	src, want := makeMoveImage(t, t.TempDir())
	check := func(t *testing.T, dstDir string) {
		t.Helper()
		f, err := os.Open(src)
		if err != nil {
			t.Fatalf("the image is gone: %v", err)
		}
		defer f.Close()
		for off, b := range want {
			got := make([]byte, len(b))
			if f.ReadAt(got, off); !bytes.Equal(got, b) {
				t.Errorf("data at %d changed", off)
			}
		}
		for _, n := range []string{ImageName, ImageName + ".moving"} {
			if _, err := os.Lstat(filepath.Join(dstDir, n)); err == nil && dstDir != "" {
				t.Errorf("%s left in %s", n, dstDir)
			}
		}
	}
	t.Run("exfat", func(t *testing.T) {
		dir := filepath.Join(volume(t, "ExFAT"), "d")
		_, err := Move(context.Background(), src, dir, MoveOptions{})
		if !errors.Is(err, ErrNoHoles) || !strings.Contains(err.Error(), "(exfat)") {
			t.Fatalf("exFAT: %v", err)
		}
		check(t, dir)
	})
	t.Run("taken", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, ImageName), []byte("someone's"), 0o600)
		_, err := Move(context.Background(), src, dir, MoveOptions{})
		if err == nil || !strings.Contains(err.Error(), "already holds a data image") {
			t.Fatalf("taken: %v", err)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, ImageName)); string(b) != "someone's" {
			t.Errorf("the image that was there was written over")
		}
	})
	t.Run("room", func(t *testing.T) {
		dir := filepath.Join(volume(t, "APFS"), "d")
		_, err := Move(context.Background(), src, dir, MoveOptions{Margin: 60 << 20})
		if err == nil || !strings.Contains(err.Error(), "must stay free") {
			t.Fatalf("room: %v", err)
		}
		check(t, dir)
	})
	t.Run("not-noted", func(t *testing.T) {
		dir := filepath.Join(volumeOf(t, "APFS", "128m"), "d")
		_, err := Move(context.Background(), src, dir, MoveOptions{Committed: func() error { return errors.New("no note") }})
		if err == nil || !strings.Contains(err.Error(), "no note") {
			t.Fatalf("not noted: %v", err)
		}
		check(t, dir)
	})
	t.Run("cancelled", func(t *testing.T) {
		dir := filepath.Join(volumeOf(t, "APFS", "128m"), "d")
		ctx, cancel := context.WithCancel(context.Background())
		_, err := Move(ctx, src, dir, MoveOptions{Progress: func(done, _ int64) {
			if done > 4<<20 {
				cancel()
			}
		}})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled: %v", err)
		}
		check(t, dir)
	})
	t.Run("same-place", func(t *testing.T) {
		if _, err := Move(context.Background(), src, filepath.Dir(src), MoveOptions{}); err == nil {
			t.Fatal("a move to where the image is was made")
		}
		check(t, "")
	})
}

func TestAMoveOnOneVolumeIsARename(t *testing.T) {
	base := t.TempDir()
	src, want := makeMoveImage(t, filepath.Join(base, "a"))
	before, _ := Allocated(src)
	dstDir := filepath.Join(base, "b")
	res, err := Move(context.Background(), src, dstDir, MoveOptions{})
	if err != nil || !res.Renamed || res.Copied != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	dst := filepath.Join(dstDir, ImageName)
	f, _ := os.Open(dst)
	defer f.Close()
	got := make([]byte, 4<<20)
	f.ReadAt(got, moveData[1])
	if !bytes.Equal(got, want[moveData[1]]) {
		t.Error("data differs")
	}
	if after, _ := Allocated(dst); after != before {
		t.Errorf("allocated %d, was %d", after, before)
	}
	if _, err := os.Lstat(src); !errors.Is(err, os.ErrNotExist) {
		t.Error("the old name is still there")
	}
}

func TestAMoveWithNoImageNotesThePlace(t *testing.T) {
	noted := false
	dir := filepath.Join(t.TempDir(), "d")
	if _, err := Move(context.Background(), filepath.Join(t.TempDir(), ImageName), dir, MoveOptions{Committed: func() error { noted = true; return nil }}); err != nil || !noted {
		t.Fatalf("%v %v", noted, err)
	}
	if _, err := Move(context.Background(), filepath.Join(t.TempDir(), ImageName), filepath.Join(volume(t, "ExFAT"), "d"), MoveOptions{}); !errors.Is(err, ErrNoHoles) {
		t.Fatalf("exFAT with no image: %v", err)
	}
}
