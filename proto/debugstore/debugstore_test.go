package debugstore

import (
	"strings"
	"testing"

	"fev/proto/contract"
	"fev/proto/jsonrpc"
)

func TestFixturesMatchTheSchema(t *testing.T) {
	fixtures, err := Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range contract.Verify(fixtures, Schema) {
		t.Error(err)
	}
}

func TestTheCacheKeyAndLayoutAreWellFormed(t *testing.T) {
	if CacheKey != CacheName+":"+CachePublicKey {
		t.Errorf("cache key %q is not name:key", CacheKey)
	}
	if !strings.HasPrefix(StoreDir, "/nix/") || !strings.HasPrefix(ProfileDir, "/nix/") ||
		!strings.HasPrefix(EtcDir, "/nix/") || !strings.HasPrefix(StoreSocket, RunDir+"/") {
		t.Errorf("a layout path is outside /nix: %s %s %s %s", StoreDir, ProfileDir, EtcDir, StoreSocket)
	}
	if MaxNarBytes <= 0 || MaxClosurePaths <= 0 || MaxCandidates <= 0 || MaxNarDepth <= 0 {
		t.Error("a limit is not positive")
	}
	if !strings.Contains(FingerprintForm, "NARHASH") || !strings.Contains(FingerprintForm, "REFS") {
		t.Errorf("the fingerprint form is missing a field: %q", FingerprintForm)
	}
}

func TestTheLookupExampleOffersOneCandidate(t *testing.T) {
	fixtures, err := Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	for _, fx := range fixtures {
		if fx.Method != MethodLookup {
			continue
		}
		r, err := jsonrpc.Bind[Lookup](fx.Result)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Candidates) > MaxCandidates {
			t.Errorf("%d candidates, more than %d", len(r.Candidates), MaxCandidates)
		}
		picks := 0
		for _, c := range r.Candidates {
			if c.Pick {
				picks++
			}
		}
		if picks != 1 {
			t.Errorf("%d candidates marked pick, want 1", picks)
		}
	}
}
