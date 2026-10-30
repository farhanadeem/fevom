package macexec

import (
	"encoding/json"
	"errors"
	"fmt"
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
	if os.Getenv("FEV_MACEXEC_INTERP") == "1" {
		json.NewEncoder(os.Stdout).Encode(os.Args)
		os.Exit(0)
	}
}

func TestTheTableIsWrittenAndKept(t *testing.T) {
	if os.Getenv("FEV_MACEXEC_NS") == "1" {
		if err := inNamespace(); err != nil {
			fmt.Println("FAIL:", err)
			os.Exit(1)
		}
		fmt.Println("PASS")
		os.Exit(0)
	}
	cmd := exec.Command("/proc/self/exe", "-test.run=^TestTheTableIsWrittenAndKept$", "-test.v")
	cmd.Env = append(os.Environ(), "FEV_MACEXEC_NS=1")
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

func inNamespace() error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "macexec")
	if err != nil {
		return err
	}
	dir := filepath.Join(tmp, "binfmt_misc")
	os.Mkdir(dir, 0o755)
	if err := unix.Mount("binfmt_misc", dir, "binfmt_misc", 0, ""); err != nil {
		return fmt.Errorf("binfmt_misc: %w", err)
	}
	if names, _ := filepath.Glob(filepath.Join(dir, "*")); len(names) != 2 {
		return fmt.Errorf("the table is not empty: %v", names)
	}
	if err := Mount(dir); err != nil {
		return fmt.Errorf("Mount over a mounted table: %v", err)
	}
	interp, err := os.Executable()
	if err != nil {
		return err
	}
	other := ":other:M::\\x01\\x02\\x03\\x04::/bin/true:"
	if err := os.WriteFile(filepath.Join(dir, "register"), []byte(other), 0); err != nil {
		return fmt.Errorf("another's entry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "register"), []byte(Entries[1].Line(interp)), 0); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, Entries[1].Name), []byte("0"), 0); err != nil {
		return err
	}
	added, err := Ensure(dir, interp, Entries)
	if err != nil {
		return err
	}
	if fmt.Sprint(added) != fmt.Sprint([]string{Entries[0].Name, Entries[2].Name, Entries[3].Name}) {
		return fmt.Errorf("Ensure added %v", added)
	}
	for _, e := range Entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name))
		if err != nil {
			return err
		}
		s := string(b)
		want := "enabled"
		if e.Name == Entries[1].Name {
			want = "disabled"
		}
		if !strings.HasPrefix(s, want+"\n") || !strings.Contains(s, "interpreter "+interp+"\n") || !strings.Contains(s, "flags: PF\n") {
			return fmt.Errorf("%s reads:\n%s", e.Name, s)
		}
	}

	prog := filepath.Join(tmp, "tool")
	os.WriteFile(prog, []byte{0xca, 0xfe, 0xba, 0xbe, 0, 0, 0, 2, 1, 0, 0, 7}, 0o755)
	run := exec.Command(prog, "a b", "c")
	run.Args[0] = "my-tool"
	run.Env = append(os.Environ(), "FEV_MACEXEC_INTERP=1")
	out, err := run.Output()
	if err != nil {
		return fmt.Errorf("run by path: %v", err)
	}
	var argv []string
	if err := json.Unmarshal(out, &argv); err != nil {
		return fmt.Errorf("the interpreter said %q", out)
	}
	if want := []string{interp, prog, "my-tool", "a b", "c"}; fmt.Sprint(argv) != fmt.Sprint(want) {
		return fmt.Errorf("the interpreter was given %q, want %q", argv, want)
	}
	for name, b := range map[string][]byte{
		"Hello.class": {0xca, 0xfe, 0xba, 0xbe, 0, 0, 0, 65, 0, 0x1d},
		"lib.dylib":   {0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0, 0, 1, 0, 0, 0, 0, 6, 0, 0, 0},
	} {
		p := filepath.Join(tmp, name)
		os.WriteFile(p, b, 0o755)
		if err := unix.Exec(p, []string{p}, []string{"FEV_MACEXEC_INTERP=1"}); !errors.Is(err, unix.ENOEXEC) {
			return fmt.Errorf("%s: exec gave %v, want exec format error", name, err)
		}
	}

	go Watch(dir, interp, Entries, func(string, ...any) {})
	time.Sleep(100 * time.Millisecond)
	present := func() error {
		deadline := time.Now().Add(2 * time.Second)
		for {
			var missing []string
			for _, e := range Entries {
				if _, err := os.Stat(filepath.Join(dir, e.Name)); err != nil {
					missing = append(missing, e.Name)
				}
			}
			if len(missing) == 0 {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("not put back within 2 s: %v", missing)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, Entries[2].Name), []byte("-1"), 0); err != nil {
		return err
	}
	if err := present(); err != nil {
		return fmt.Errorf("after -1 to %s: %v", Entries[2].Name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other"), []byte("-1"), 0); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte("-1"), 0); err != nil {
		return err
	}
	if err := present(); err != nil {
		return fmt.Errorf("after -1 to status: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "other")); err == nil {
		return errors.New("another's removed entry was written again")
	}

	op := Entries[0]
	op.Name = "operator-macho"
	if err := os.WriteFile(filepath.Join(dir, "register"), []byte(op.Line("/bin/true")), 0); err != nil {
		return fmt.Errorf("the operator's entry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, Entries[0].Name), []byte("-1"), 0); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, Entries[0].Name)); err == nil {
		return fmt.Errorf("%s written again ahead of the operator's entry for the same files", Entries[0].Name)
	}
	if err := os.WriteFile(filepath.Join(dir, op.Name), []byte("-1"), 0); err != nil {
		return err
	}
	if err := present(); err != nil {
		return fmt.Errorf("after the operator's entry went: %v", err)
	}
	return nil
}
