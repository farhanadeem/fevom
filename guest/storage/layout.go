package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

const StampName = "fev-format.json"

const stampTemp = ".fev-format.json.tmp"

const CurrentLayout = 2

var Subvolumes = []string{"@system", "@docker", "@machines"}

const ControlSubvolume = "@ctl"

type Stamp struct {
	Layout    int    `json:"layout"`
	CreatedBy string `json:"created_by,omitempty"`
	Mkfs      string `json:"mkfs,omitempty"`
}

type Volumes interface {
	CreateSubvolume(path string) error
	Sync() error
	EnsureQuota(root string) error
}

var (
	ErrNewerLayout = errors.New("the data image was written by a newer version of fev")
	ErrOlderLayout = errors.New("the data image was made by a fev build from before this layout")
	ErrNoStamp     = errors.New("the filesystem holds data but no " + StampName)
)

func ReadStamp(root string) (Stamp, error) {
	data, err := os.ReadFile(filepath.Join(root, StampName))
	if err != nil {
		return Stamp{}, err
	}
	var s Stamp
	if err := json.Unmarshal(data, &s); err != nil {
		return Stamp{}, fmt.Errorf("%s is not valid: %w", StampName, err)
	}
	if s.Layout < 1 {
		return Stamp{}, fmt.Errorf("%s names layout %d", StampName, s.Layout)
	}
	return s, nil
}

func writeStamp(root string, s Stamp, v Volumes) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := filepath.Join(root, stampTemp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(root, StampName)); err != nil {
		return err
	}
	return v.Sync()
}

func EnsureLayout(root string, v Volumes, fresh Stamp, logf func(string, ...any)) (int, error) {
	layout, _, err := ensureLayout(root, v, fresh, logf)
	return layout, err
}

func ensureLayout(root string, v Volumes, fresh Stamp, logf func(string, ...any)) (layout int, first bool, err error) {
	stamp, err := ReadStamp(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		entries, err := os.ReadDir(root)
		if err != nil {
			return 0, false, err
		}
		ours := append(slices.Clone(Subvolumes), ControlSubvolume)
		for _, e := range entries {
			if e.Name() != stampTemp && !(e.IsDir() && slices.Contains(ours, e.Name())) {
				return 0, false, fmt.Errorf("%w (found %q)", ErrNoStamp, e.Name())
			}
		}
		if err := ensureSubvolumes(root, v, logf); err != nil {
			return 0, false, err
		}
		layoutStep(v, StepSubvolumesMade)
		if err := v.EnsureQuota(root); err != nil {
			return 0, false, fmt.Errorf("the quota group of a new filesystem: %w", err)
		}
		fresh.Layout = CurrentLayout
		if err := writeStamp(root, fresh, v); err != nil {
			return 0, false, fmt.Errorf("write %s: %w", StampName, err)
		}
		logf("storage: new filesystem laid out at version %d", CurrentLayout)
		layoutStep(v, StepStampWritten)
		return CurrentLayout, true, nil
	case err != nil:
		return 0, false, err
	case stamp.Layout > CurrentLayout:
		return 0, false, fmt.Errorf("%w: it has layout %d and this build knows up to %d", ErrNewerLayout, stamp.Layout, CurrentLayout)
	case stamp.Layout < CurrentLayout:
		return 0, false, olderLayout(stamp.Layout)
	}
	if err := ensureSubvolumes(root, v, logf); err != nil {
		return 0, false, err
	}
	if _, err := os.Lstat(filepath.Join(root, ControlSubvolume)); errors.Is(err, fs.ErrNotExist) {
		if err := v.EnsureQuota(root); err != nil {
			return 0, false, fmt.Errorf("put back subvolume %s: %w", ControlSubvolume, err)
		}
		logf("storage: put back subvolume %s", ControlSubvolume)
	} else if err != nil {
		return 0, false, err
	}
	return stamp.Layout, false, nil
}

type olderLayoutError struct{ layout int }

func (e olderLayoutError) Error() string {
	return fmt.Sprintf("the data disk was made by a development build of fev from before data layout %d "+
		"(the disk has layout %d), and this build does not convert it: no disk from before fev's first release is converted. "+
		"The disk was not changed. %s (the build that made it still reads it)",
		CurrentLayout, e.layout, ResetRemedy)
}

func (e olderLayoutError) Is(target error) bool { return target == ErrOlderLayout }

func olderLayout(layout int) error { return olderLayoutError{layout} }

const (
	StepSubvolumesMade = "subvolumes-made"
	StepStampWritten   = "stamp-written"
)

func layoutStep(v Volumes, name string) {
	if s, ok := v.(interface{ step(string) }); ok {
		s.step(name)
	}
}

func ensureSubvolumes(root string, v Volumes, logf func(string, ...any)) error {
	for _, name := range Subvolumes {
		path := filepath.Join(root, name)
		if _, err := os.Lstat(path); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := v.CreateSubvolume(path); err != nil {
			return fmt.Errorf("create subvolume %s: %w", name, err)
		}
		logf("storage: created subvolume %s", name)
	}
	return nil
}
