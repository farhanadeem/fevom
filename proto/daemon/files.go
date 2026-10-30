package daemon

import "fmt"

const KeyFilesViewHidden = "files.view_hidden"

const (
	ViewUnmounted = "unmounted"
	ViewMounting  = "mounting"
	ViewMounted   = "mounted"
	ViewFailed    = "failed"
)

type FilesView struct {
	State  string `json:"state"`
	Path   string `json:"path"`
	Socket string `json:"socket"`
	Hidden bool   `json:"hidden"`
	Error  string `json:"error,omitempty"`
}

func CheckFilesView(v FilesView) error {
	switch v.State {
	case ViewUnmounted, ViewMounting, ViewMounted, ViewFailed:
	default:
		return fmt.Errorf("files.view: unknown state %q", v.State)
	}
	if (v.State == ViewFailed) != (v.Error != "") {
		return fmt.Errorf("files.view: an error with state %s: %q", v.State, v.Error)
	}
	if v.Path == "" || v.Socket == "" {
		return fmt.Errorf("files.view: no path or socket")
	}
	return nil
}
