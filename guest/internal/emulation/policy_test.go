package emulation

import "testing"

func TestKnownIncompatibilitiesAndExplicitOverrides(t *testing.T) {
	c := Config{Mode: Auto, Rosetta: true}
	for _, tc := range []struct {
		override string
		facts    Facts
		want     string
	}{
		{"", Facts{Distro: "alpine"}, Rosetta},
		{"", Facts{Machine: true}, Rosetta},
		{"", Facts{Machine: true, Distro: "debian", Version: "13"}, Rosetta},
		{"", Facts{Machine: true, Distro: "ubuntu", Version: "26.04"}, FEX},
		{"", Facts{Machine: true, Distro: "fedora", Version: "44"}, FEX},
		{"", Facts{Machine: true, Distro: "arch"}, FEX},
		{"", Facts{Distro: "arch"}, FEX},
		{"", Facts{Distro: "ubuntu", Version: "26.04"}, FEX},
		{"", Facts{Distro: "ubuntu", Version: "24.04"}, Rosetta},
		{"", Facts{Distro: "fedora", Version: "44"}, FEX},
		{"", Facts{Sandbox: "nix"}, FEX},
		{Rosetta, Facts{Machine: true, Distro: "arch"}, Rosetta},
		{FEX, Facts{}, FEX},
	} {
		d, err := Select(c, tc.override, tc.facts)
		if err != nil || d.Engine != tc.want || d.Reason == "" {
			t.Fatalf("%+v => %+v, %v", tc, d, err)
		}
	}
	if SandboxName("/sbin/init") != "" || SandboxName("/usr/lib/systemd/systemd") != "" {
		t.Fatal("a machine's init judged a sandbox")
	}
	c.Faccessat2, c.Seccomp = true, true
	d, err := Select(c, "", Facts{Machine: true, Distro: "arch", Sandbox: "nix"})
	if err != nil || d.Engine != Rosetta {
		t.Fatalf("new Rosetta capabilities ignored: %+v, %v", d, err)
	}
	c.Rosetta, c.Reason = false, "probe failed"
	d, err = Select(c, "", Facts{})
	if err != nil || d.Engine != FEX {
		t.Fatalf("unavailable: %+v %v", d, err)
	}
	if _, err := Select(c, Rosetta, Facts{}); err == nil {
		t.Fatal("forced unavailable Rosetta accepted")
	}
	if _, err := Select(c, "typo", Facts{}); err == nil {
		t.Fatal("invalid override accepted")
	}
	c.Mode = Rosetta
	if _, err := Select(c, "", Facts{AMD64: true}); err == nil {
		t.Fatal("amd64 workload started without the Rosetta it was set to")
	}
	for _, f := range []Facts{{}, {Machine: true}} {
		if d, err := Select(c, "", f); err != nil || d.Engine != FEX {
			t.Fatalf("native workload %+v refused for a missing Rosetta: %+v %v", f, d, err)
		}
	}
}
