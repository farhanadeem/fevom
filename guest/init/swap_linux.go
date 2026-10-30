package main

import (
	"golang.org/x/sys/unix"

	"fev/guest/internal/logbuf"
	"fev/guest/internal/netcfg"
	"fev/guest/storage"
)

func (s *system) startSwap() {
	which := netcfg.Arg(s.cmdline, "fev.swap")
	if which == "0" {
		return
	}
	<-s.ready
	var si unix.Sysinfo_t
	zram := int64(0)
	if unix.Sysinfo(&si) == nil {
		zram = int64(si.Totalram) * int64(si.Unit)
	}
	data := netcfg.Arg(s.cmdline, dataDiskParam)
	if data == "" {
		data = "/dev/vda"
	}
	skip := []string{data}
	if which == "disk" {
		zram = 0
	}
	storage.SetupSwap(storage.SwapConfig{
		Proc: "/proc", Sys: "/sys", Dev: "/dev", Skip: skip, ZramBytes: zram, NoDisk: which == "zram",
		Logf: func(format string, args ...any) { s.log.Logf(logbuf.Info, format, args...) },
	})
}
