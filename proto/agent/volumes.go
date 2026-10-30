package agent

import (
	"fmt"
	"regexp"
)

const (
	MethodVolumeClone  = "volume.clone"
	MethodVolumeExport = "volume.export"
	MethodVolumeImport = "volume.import"
)

const VolumeArchiveKind = "docker-volume"

type VolumeCloneParams struct {
	Name    string `json:"name"`
	NewName string `json:"new_name"`
}

type VolumeNameParams struct {
	Name string `json:"name"`
}

type VolumeInfo struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Files  int64             `json:"files"`
}

type VolumeExported struct {
	Bytes int64 `json:"bytes"`
}

type VolumeArchiveConfig struct {
	Version  int               `json:"version"`
	Kind     string            `json:"kind"`
	Name     string            `json:"name"`
	Driver   string            `json:"driver"`
	Labels   map[string]string `json:"labels,omitempty"`
	Exported string            `json:"exported"`
}

var volumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

const MaxVolumeName = 255

func CheckVolumeName(s string) error {
	if len(s) > MaxVolumeName {
		return fmt.Errorf("a volume's name is at most %d characters", MaxVolumeName)
	}
	if !volumeName.MatchString(s) {
		return fmt.Errorf("%q is not a volume name: two or more of letters, digits, '_', '.' and '-', the first a letter or digit", s)
	}
	return nil
}
