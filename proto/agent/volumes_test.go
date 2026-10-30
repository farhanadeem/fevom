package agent

import (
	"strings"
	"testing"
)

func TestVolumeNamesAreTheEnginesAndNoPath(t *testing.T) {
	for _, ok := range []string{"pgdata", "my_vol.1", "a-b", "00", strings.Repeat("v", MaxVolumeName)} {
		if err := CheckVolumeName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a", ".", "..", "../x", "a/b", "-a", "_a", ".hidden", "a b", "a\x00", "é1", strings.Repeat("v", MaxVolumeName+1)} {
		if err := CheckVolumeName(bad); err == nil {
			t.Errorf("%q taken", bad)
		}
	}
}
