package privhelper

import (
	"encoding/json"
	"io"
	"os"
	"syscall"
)

func ReadRecord(path string, owner int) (InstallRecord, bool) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return InstallRecord{}, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return InstallRecord{}, false
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != owner {
		return InstallRecord{}, false
	}
	var rec InstallRecord
	if err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&rec); err != nil {
		return InstallRecord{}, false
	}
	return rec, true
}
