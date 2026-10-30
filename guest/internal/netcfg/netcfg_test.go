package netcfg

import (
	"strings"
	"testing"
)

func TestFromCmdline(t *testing.T) {
	c, err := FromCmdline("console=hvc0 quiet")
	if err != nil || c != Default {
		t.Fatalf("default: %+v %v", c, err)
	}
	c, err = FromCmdline("console=hvc0 fev.net=10.9.8.7/16,10.9.0.1,1.1.1.1 quiet")
	if err != nil {
		t.Fatal(err)
	}
	if c.Address.String() != "10.9.8.7/16" || c.Gateway.String() != "10.9.0.1" || c.DNS.String() != "1.1.1.1" {
		t.Fatalf("parsed %+v", c)
	}
	c, err = FromCmdline("fev.net=10.0.0.2/24,10.0.0.1 fev.net=192.168.5.2/24,192.168.5.1")
	if err != nil || c.DNS.String() != "192.168.5.1" || c.Address.String() != "192.168.5.2/24" {
		t.Fatalf("parsed %+v %v", c, err)
	}
	if got := Arg("fev.network=x fev.netx=y", Param); got != "" {
		t.Fatalf("Arg matched %q", got)
	}
}

func TestTheUplinkIsThePlans(t *testing.T) {
	if Default.Address.String() != "198.19.1.2/24" || Default.Gateway.String() != "198.19.1.1" || Default.DNS.String() != "198.19.1.1" {
		t.Fatalf("default %+v", Default)
	}
	c, err := FromCmdline("console=hvc0 fev.machnet=10.198.0.0/16")
	if err != nil || c.Address.String() != "10.198.1.2/24" || c.Gateway.String() != "10.198.1.1" || c.DNS.String() != "10.198.1.1" {
		t.Fatalf("from the plan: %+v %v", c, err)
	}
	c, err = FromCmdline("fev.machnet=10.198.0.0/16 fev.net=10.9.8.7/16,10.9.0.1")
	if err != nil || c.Address.String() != "10.9.8.7/16" {
		t.Fatalf("fev.net= over the plan: %+v %v", c, err)
	}
	if _, err := FromCmdline("fev.machnet=10.198.0.0/22"); err == nil || !strings.Contains(err.Error(), "fev.machnet") {
		t.Fatalf("a plan that is not a /16: %v", err)
	}
}

func TestParseRejectsWhatCannotWork(t *testing.T) {
	cases := map[string]string{
		"10.0.0.2/24":                    "want ADDRESS",
		"10.0.0.2/24,10.0.0.1,1.1.1.1,x": "want ADDRESS",
		"10.0.0.2,10.0.0.1":              "address",
		"10.0.0.2/24,gateway":            "gateway",
		"10.0.0.2/24,10.0.0.1,dns":       "DNS server",
		"10.0.0.2/24,10.0.1.1":           "is not in",
		"10.0.0.1/24,10.0.0.1":           "both",
		"fd00::2/64,fd00::1":             "IPv4",
	}
	for value, want := range cases {
		if _, err := Parse(value); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want an error about %q", value, err, want)
		}
	}
}
