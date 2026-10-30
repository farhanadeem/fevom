package listen

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cilium/ebpf/asm"
)

func TestTheProgramsAssemble(t *testing.T) {
	for name, in := range map[string]asm.Instructions{
		"sock_ops": sockOps(10, 11), "post_bind4": postBind(10, 12, 11, false), "post_bind6": postBind(10, 12, 11, true), "release": release(10, 12, 11),
		"bind": bindHook(12),
	} {
		for i, x := range in {
			if strings.HasPrefix(fmt.Sprint(x), "INVALID") {
				t.Errorf("%s: instruction %d is not one: %v", name, i, x)
			}
		}
		syms := map[string]int{}
		for _, x := range in {
			if s := x.Symbol(); s != "" {
				syms[s]++
			}
		}
		for i, x := range in {
			if r := x.Reference(); r != "" && x.OpCode.JumpOp() != asm.InvalidJumpOp && syms[r] != 1 {
				t.Errorf("%s: instruction %d jumps to %q, which is defined %d times", name, i, r, syms[r])
			}
		}
	}
}
