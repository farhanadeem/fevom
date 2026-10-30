package thp

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestHugePagesOffAreKeptAcrossExecAndGivenBack(t *testing.T) {
	if os.Getenv("FEV_THP_CHILD") == "1" {
		off, err := IsOff()
		fmt.Printf("off=%v err=%v\n", off, err)
		os.Exit(0)
	}
	child := func() string {
		cmd := exec.Command("/proc/self/exe", "-test.run=^TestHugePagesOffAreKeptAcrossExecAndGivenBack$")
		cmd.Env = append(os.Environ(), "FEV_THP_CHILD=1")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("the child: %v", err)
		}
		return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	}
	if err := Off(); err != nil {
		t.Fatal(err)
	}
	defer Restore()
	if off, err := IsOff(); !off || err != nil {
		t.Fatalf("after Off: %v %v", off, err)
	}
	if got := child(); got != "off=true err=<nil>" {
		t.Errorf("a program started after Off: %s", got)
	}
	if err := Restore(); err != nil {
		t.Fatal(err)
	}
	if off, err := IsOff(); off || err != nil {
		t.Fatalf("after Restore: %v %v", off, err)
	}
	if got := child(); got != "off=false err=<nil>" {
		t.Errorf("a program started after Restore: %s", got)
	}
}
