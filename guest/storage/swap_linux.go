package storage

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"

	api "fev/proto/agent"
)

const SwapLabel = "fev-swap"

const (
	ZramPriority = 32767
	DiskPriority = 1
)

const Swappiness = 20

type swapHeader struct {
	bootbits   [1024]byte
	version    uint32
	lastPage   uint32
	nrBadpages uint32
	uuid       [16]byte
	volumeName [16]byte
}

const swapMagic = "SWAPSPACE2"

func SwapHeader(size int64, pageSize int, label string, uuid [16]byte) []byte {
	page := make([]byte, pageSize)
	h := (*swapHeader)(unsafe.Pointer(&page[0]))
	h.version = 1
	h.lastPage = uint32(size/int64(pageSize)) - 1
	h.uuid = uuid
	copy(h.volumeName[:], label)
	copy(page[pageSize-len(swapMagic):], swapMagic)
	return page
}

func swapLabelOf(page []byte) (string, bool) {
	if len(page) < int(unsafe.Sizeof(swapHeader{})) || string(page[len(page)-len(swapMagic):]) != swapMagic {
		return "", false
	}
	h := (*swapHeader)(unsafe.Pointer(&page[0]))
	if h.version != 1 {
		return "", false
	}
	return string(bytes.TrimRight(h.volumeName[:], "\x00")), true
}

const (
	swapFlagPrefer   = 0x8000
	swapFlagDiscard  = 0x10000
	swapFlagPrioMask = 0x7fff
)

func swapon(dev string, prio int, discard bool) error {
	p, err := unix.BytePtrFromString(dev)
	if err != nil {
		return err
	}
	flags := swapFlagPrefer | prio&swapFlagPrioMask
	if discard {
		flags |= swapFlagDiscard
	}
	if _, _, e := unix.Syscall(unix.SYS_SWAPON, uintptr(unsafe.Pointer(p)), uintptr(flags), 0); e != 0 {
		return e
	}
	return nil
}

type SwapConfig struct {
	Proc, Sys, Dev string
	Skip           []string
	ZramBytes      int64
	NoDisk         bool
	Logf           func(format string, args ...any)
}

func SetupSwap(cfg SwapConfig) {
	if cfg.ZramBytes > 0 {
		if err := setupZram(cfg); err != nil {
			cfg.Logf("storage: no compressed swap: %v", err)
		}
	}
	dev, err := "", error(nil)
	if !cfg.NoDisk {
		dev, err = findSwapDisk(cfg)
	}
	switch {
	case err != nil:
		cfg.Logf("storage: no swap disk: %v", err)
	case dev == "":
	default:
		if err := swapon(filepath.Join(cfg.Dev, dev), DiskPriority, true); err != nil {
			cfg.Logf("storage: swap disk %s: swapon: %v", dev, err)
		}
	}
	if err := os.WriteFile(filepath.Join(cfg.Proc, "sys/vm/swappiness"), []byte(strconv.Itoa(Swappiness)), 0); err != nil {
		cfg.Logf("storage: vm.swappiness: %v", err)
	}
}

func setupZram(cfg SwapConfig) error {
	block := filepath.Join(cfg.Sys, "block/zram0")
	if _, err := os.Stat(block); err != nil {
		return fmt.Errorf("the kernel has no zram0: %w", err)
	}
	if err := os.WriteFile(filepath.Join(block, "comp_algorithm"), []byte("lz4"), 0); err != nil {
		return fmt.Errorf("comp_algorithm lz4: %w", err)
	}
	size := cfg.ZramBytes &^ 4095
	if err := os.WriteFile(filepath.Join(block, "disksize"), []byte(strconv.FormatInt(size, 10)), 0); err != nil {
		return fmt.Errorf("disksize %d: %w", size, err)
	}
	dev := filepath.Join(cfg.Dev, "zram0")
	f, err := os.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	var id [16]byte
	if _, err := f.WriteAt(SwapHeader(size, os.Getpagesize(), "zram", id), 0); err != nil {
		f.Close()
		return fmt.Errorf("write its swap header: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := swapon(dev, ZramPriority, false); err != nil {
		return fmt.Errorf("swapon: %w", err)
	}
	return nil
}

func findSwapDisk(cfg SwapConfig) (string, error) {
	entries, err := os.ReadDir(filepath.Join(cfg.Sys, "block"))
	if err != nil {
		return "", err
	}
	page := make([]byte, os.Getpagesize())
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "vd") {
			continue
		}
		skip := false
		for _, s := range cfg.Skip {
			if strings.TrimPrefix(s, "/dev/") == name {
				skip = true
			}
		}
		if skip {
			continue
		}
		f, err := os.Open(filepath.Join(cfg.Dev, name))
		if err != nil {
			continue
		}
		n, _ := f.ReadAt(page, 0)
		f.Close()
		if n < len(page) {
			continue
		}
		if label, ok := swapLabelOf(page); ok && label == SwapLabel {
			return name, nil
		}
	}
	return "", nil
}

func SwapDevices(proc, sys string) ([]api.SwapDevice, error) {
	f, err := os.Open(filepath.Join(proc, "swaps"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []api.SwapDevice
	sc := bufio.NewScanner(f)
	sc.Scan()
	for sc.Scan() {
		w := strings.Fields(sc.Text())
		if len(w) < 5 {
			continue
		}
		size, _ := strconv.ParseInt(w[2], 10, 64)
		used, _ := strconv.ParseInt(w[3], 10, 64)
		prio, _ := strconv.Atoi(w[4])
		d := api.SwapDevice{Device: w[0], Priority: prio, SizeBytes: size << 10, UsedBytes: used << 10, Kind: api.SwapDisk}
		if name := filepath.Base(w[0]); strings.HasPrefix(name, "zram") {
			d.Kind = api.SwapZram
			if b, err := os.ReadFile(filepath.Join(sys, "block", name, "comp_algorithm")); err == nil {
				s := string(b)
				if i, j := strings.Index(s, "["), strings.Index(s, "]"); i >= 0 && j > i {
					d.Algorithm = s[i+1 : j]
				}
			}
			if b, err := os.ReadFile(filepath.Join(sys, "block", name, "mm_stat")); err == nil {
				if m := strings.Fields(string(b)); len(m) > 1 {
					d.StoredBytes, _ = strconv.ParseInt(m[1], 10, 64)
				}
			}
		}
		out = append(out, d)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Priority > out[j-1].Priority; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, sc.Err()
}
