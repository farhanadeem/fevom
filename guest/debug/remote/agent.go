package remote

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"fev/guest/debug"
)

const (
	StagePath  = "/fev-debug-stage"
	DebugPath  = "/fev-debug"
	LockPath   = "/defaultset.lock"
	StoreMount = "/nix"
)

func ready() bool {
	b, err := os.ReadFile(LockPath)
	if err != nil {
		return false
	}
	return debug.StoreReady(StoreMount, debug.Stamp(b))
}

var Build = "dev"

func Main(argv []string) int {
	if len(argv) >= 1 && (argv[0] == "--version" || argv[0] == "version") {
		fmt.Println(Build)
		return 0
	}
	if len(argv) >= 1 && argv[0] == "fetch" {
		if err := Fetch(); err != nil {
			fmt.Fprintln(os.Stderr, "fev-debug-agent: fetch:", err)
			return 1
		}
		return 0
	}
	if !ready() {
		fmt.Fprintln(os.Stderr, "fev debug: this host's debug tool store is not whole for this release; run fev debug --reset -c CONTEXT and try again")
		return 1
	}
	return execStage(argv)
}

func Fetch() error {
	if ready() {
		return nil
	}
	if err := os.MkdirAll(StoreMount+"/var/fev", 0o755); err != nil {
		return err
	}
	lk, err := os.OpenFile(StoreMount+"/var/fev/.fetch.lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lk.Close()
	if err := syscall.Flock(int(lk.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	if ready() {
		return nil
	}
	if _, err := os.Stat(LockPath); err != nil {
		return fmt.Errorf("the default set (%s): %w", LockPath, err)
	}
	args := []string{"fetch", "-store", StoreMount, "-lock", LockPath}
	if c := os.Getenv("FEV_DEBUG_CACHE"); c != "" {
		args = append(args, "-cache", c)
	}
	fmt.Fprintf(os.Stderr, "fev: fetching the debug tools into this host's %q volume...\n", "fev-debug-store")
	t0 := time.Now()
	cmd := exec.Command(DebugPath, args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	for _, d := range []string{"homes", "tmp"} {
		os.MkdirAll(StoreMount+"/"+d, 0o755)
	}
	fmt.Fprintf(os.Stderr, "fev: debug tools fetched in %.1f s\n", time.Since(t0).Seconds())
	return nil
}

func execStage(argv []string) int {
	full := append([]string{StagePath}, argv...)
	if err := syscall.Exec(StagePath, full, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "fev-debug-agent: exec stage:", err)
		return 126
	}
	return 0
}
