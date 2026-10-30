package binfmt

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func init() {
	if os.Getenv("FEV_BINFMT_INTERP") == "1" {
		fmt.Println(os.Args[0])
		os.Exit(0)
	}
}

func TestFevomsEntryPutBackDoesNotRunAheadOfAnOperatorsEntry(t *testing.T) {
	if os.Getenv("FEV_BINFMT_NS") == "1" {
		if err := operatorFirst(); err != nil {
			fmt.Println("FAIL:", err)
			os.Exit(1)
		}
		fmt.Println("PASS")
		os.Exit(0)
	}
	cmd := exec.Command("/proc/self/exe", "-test.run=^TestFevomsEntryPutBackDoesNotRunAheadOfAnOperatorsEntry$", "-test.v")
	cmd.Env = append(os.Environ(), "FEV_BINFMT_NS=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	out, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(out), "binfmt_misc: operation not permitted") {
		t.Skipf("no binfmt_misc of a user namespace's own here: %s", out)
	}
	if err != nil || !strings.Contains(string(out), "PASS") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func operatorFirst() error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "binfmt")
	if err != nil {
		return err
	}
	dir := filepath.Join(tmp, "binfmt_misc")
	os.Mkdir(dir, 0o755)
	if err := unix.Mount("binfmt_misc", dir, "binfmt_misc", 0, ""); err != nil {
		return fmt.Errorf("binfmt_misc: %w", err)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	ours, theirs := filepath.Join(tmp, "fev-interp"), filepath.Join(tmp, "operator-interp")
	for _, p := range []string{ours, theirs} {
		if err := copyFile(self, p); err != nil {
			return err
		}
	}
	prog := filepath.Join(tmp, "prog")
	head := make([]byte, 64)
	copy(head, Fevom(ours)[0].Magic)
	head[20] = 1
	if err := os.WriteFile(prog, head, 0o755); err != nil {
		return err
	}
	runs := func() (string, error) {
		c := exec.Command(prog)
		c.Env = []string{"FEV_BINFMT_INTERP=1"}
		out, err := c.Output()
		return strings.TrimSpace(string(out)), err
	}

	k := &Keeper{Table: Table{Dir: dir}, Dir: dir, Specs: Fevom(ours), Logf: func(string, ...any) {}}
	if err := k.Start(true); err != nil {
		return fmt.Errorf("start: %v", err)
	}
	if got, err := runs(); got != ours {
		return fmt.Errorf("fev's alone: %q %v", got, err)
	}
	op := Spec{Name: "operator-x86_64", Magic: Fevom(theirs)[0].Magic, Mask: x86Mask, Interpreter: theirs, Flags: "POCF"}
	if err := (Table{Dir: dir}).Register(op); err != nil {
		return fmt.Errorf("the operator's entry: %v", err)
	}
	if got, err := runs(); got != theirs {
		return fmt.Errorf("with the operator's registered later: %q %v", got, err)
	}
	if err := (Table{Dir: dir}).Remove(Fevom(ours)[0].Name); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	if got, err := runs(); got != theirs {
		return fmt.Errorf("fev's removed while the operator's is there: %q runs (%v); fev's in the table: %v", got, err, k.Table.Has(Fevom(ours)[0].Name))
	}
	if _, entries, _, _, _ := k.State(); WhyNot(true, entries, Fevom(ours)) == "" {
		return fmt.Errorf("the status says nothing of the operator's entry")
	}
	if err := (Table{Dir: dir}).Remove(op.Name); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for !k.Table.Has(Fevom(ours)[0].Name) {
		if time.Now().After(deadline) {
			return fmt.Errorf("fev's not written again within 2 s of the operator's removal")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got, err := runs(); got != ours {
		return fmt.Errorf("after the operator's went: %q %v", got, err)
	}
	return k.Set(false)
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
