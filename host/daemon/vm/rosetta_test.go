package vm

import (
	"slices"
	"testing"
)

func TestRosettaIsRequiredOnlyWhenX86IsOn(t *testing.T) {
	for _, tc := range []struct {
		words []string
		want  []string
	}{
		{nil, nil},
		{[]string{"fev.x86=0"}, nil},
		{[]string{"fev.emulator=rosetta"}, []string{"--rosetta"}},
		{[]string{"fev.emulator=rosetta", "fev.x86=0"}, []string{"--rosetta-optional"}},
		{[]string{"fev.emulator=auto"}, []string{"--rosetta-optional"}},
		{[]string{"fev.emulator=auto", "fev.x86=0"}, []string{"--rosetta-optional"}},
	} {
		if got := rosettaArgs(tc.words); !slices.Equal(got, tc.want) {
			t.Errorf("%v: %v, want %v", tc.words, got, tc.want)
		}
	}
}
