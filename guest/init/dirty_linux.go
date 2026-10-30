package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"

	api "fev/proto/agent"

	"fev/guest/internal/logbuf"
	"fev/guest/internal/netcfg"
)

const dirtyBytesParam = "fev.dirty_bytes"

func dirtyCap(memAvailableBytes int64, ratio int, want int64) (dirty, background int64) {
	if want <= 0 || ratio <= 0 || memAvailableBytes <= 0 {
		return 0, 0
	}
	if kernel := memAvailableBytes / 100 * int64(ratio); want >= kernel {
		return 0, 0
	}
	return want, want / 2
}

func (s *system) capDirtyData() {
	want := int64(api.DirtyCapBytes)
	if v := netcfg.Arg(s.cmdline, dirtyBytesParam); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			s.log.Logf(logbuf.Warning, "init: %s=%s is not a number of bytes; the default is used", dirtyBytesParam, v)
		} else {
			want = n
		}
	}
	ratio, err := readInt("/proc/sys/vm/dirty_ratio")
	if err != nil {
		s.log.Logf(logbuf.Warning, "init: cannot read vm.dirty_ratio: %v", err)
		return
	}
	dirty, background := dirtyCap(memAvailable(), int(ratio), want)
	if dirty == 0 {
		return
	}
	for _, kv := range []struct {
		key   string
		value int64
	}{{"vm/dirty_background_bytes", background}, {"vm/dirty_bytes", dirty}} {
		if err := os.WriteFile("/proc/sys/"+kv.key, []byte(strconv.FormatInt(kv.value, 10)), 0); err != nil {
			s.log.Logf(logbuf.Warning, "init: cannot set %s: %v", kv.key, err)
			return
		}
		if got, err := readInt("/proc/sys/" + kv.key); err != nil || got != kv.value {
			s.log.Logf(logbuf.Warning, "init: %s reads %d after being set to %d (%v)", kv.key, got, kv.value, err)
			return
		}
	}
	s.dirtyBytes = dirty
	s.log.Logf(logbuf.Info, "init: unwritten data is capped at %d MiB (write-back from %d MiB); the kernel's own limit was %d%% of memory",
		dirty>>20, background>>20, ratio)
}

func readInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

func memAvailable() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	for sc := bufio.NewScanner(f); sc.Scan(); {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			kb, _ := strconv.ParseInt(fields[1], 10, 64)
			return kb << 10
		}
	}
	return 0
}
