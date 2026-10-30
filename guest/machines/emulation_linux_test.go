package machines

import (
	"os"
	"path/filepath"
	"testing"

	policy "fev/guest/internal/emulation"
	api "fev/proto/agent"
)

func TestOnlyAnAmd64MachineHasAnX86EngineChosenAndReported(t *testing.T) {
	arm := t.TempDir()
	amd := t.TempDir()
	header := make([]byte, 20)
	copy(header, "\x7fELF")
	header[4], header[5], header[18] = 2, 1, 62
	if err := os.MkdirAll(filepath.Join(amd, "sbin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(amd, "sbin/init"), header, 0o755); err != nil {
		t.Fatal(err)
	}
	ros := policy.Config{Mode: policy.Auto, Rosetta: true}
	gone := policy.Config{Mode: policy.Rosetta, Reason: "probe failed"}
	fex := policy.Config{Mode: policy.FEX, Reason: "Rosetta is not attached in fex mode"}
	armRec := &Record{Image: api.ImageRef{Arch: "arm64"}}
	amdRec := &Record{Image: api.ImageRef{Arch: "amd64"}}
	for _, tc := range []struct {
		name     string
		profile  policy.Config
		rec      *Record
		root     string
		override string
		engine   string
		amd64    bool
		fails    bool
	}{
		{"arm64 under auto", ros, armRec, arm, "", policy.Rosetta, false, false},
		{"arm64 with rosetta asked under fex", fex, armRec, arm, policy.Rosetta, "", false, false},
		{"arm64 with Rosetta gone", gone, armRec, arm, "", policy.FEX, false, false},
		{"amd64 by its record", ros, amdRec, arm, policy.Rosetta, policy.Rosetta, true, false},
		{"amd64 by its init", fex, armRec, amd, policy.FEX, policy.FEX, true, false},
		{"amd64 with rosetta asked under fex", fex, amdRec, amd, policy.Rosetta, "", true, true},
	} {
		d, amd64, err := selectEmulation(tc.profile, tc.rec, api.MachineConfig{Emulation: tc.override}, tc.root)
		if (err != nil) != tc.fails || d.Engine != tc.engine || amd64 != tc.amd64 {
			t.Errorf("%s: %+v amd64=%v %v", tc.name, d, amd64, err)
		}
	}
}
