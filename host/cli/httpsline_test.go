package cli

import (
	"strings"
	"testing"

	api "fev/proto/daemon"
)

func TestAnUnusableCAKeyNamesFevomSetupHTTPS(t *testing.T) {
	for _, h := range []api.HTTPSStatus{
		{Enabled: true, Trust: api.TrustTrusted, Key: api.KeyUnusable},
		{Enabled: true, Trust: api.TrustTrusted, Key: api.KeyUnusable, Identity: "Apple Development: X (ABCDE12345)"},
		{Enabled: true, Trust: api.TrustTrusted, Key: api.KeyUnusable, Error: `the key labelled dev.fevom.ca was made by another program; "fev setup --https" replaces it with a new key and root`},
	} {
		line := httpsLine(&h)
		if n := strings.Count(line, `"fev setup --https"`); n != 1 {
			t.Errorf("%+v: %q names fev setup --https %d times, want once", h, line, n)
		}
	}
	if line := httpsLine(&api.HTTPSStatus{Enabled: true, Trust: api.TrustTrusted, Key: api.KeySigner}); strings.Contains(line, "setup") {
		t.Errorf("a usable key: %q", line)
	}
}
