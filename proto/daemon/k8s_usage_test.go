package daemon

import (
	"testing"

	"fev/proto/jsonrpc"
)

func TestTheKubernetesUsageLineIsItsParts(t *testing.T) {
	fixtures, err := Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, fx := range fixtures {
		if fx.Method != MethodStorageUsage {
			continue
		}
		p, _ := jsonrpc.Bind[UsageParams](fx.Params)
		r, _ := jsonrpc.Bind[StorageUsage](fx.Result)
		k := r.Kubernetes
		if k == nil {
			continue
		}
		seen = true
		if k.ImagesCounted != p.Engine || (!k.ImagesCounted && k.ImagesBytes != 0) {
			t.Errorf("%s: images counted %v with the engine's account asked %v", fx.Method, k.ImagesCounted, p.Engine)
		}
		if k.Bytes != k.ClusterBytes+k.CacheBytes+k.ImagesBytes || k.Bytes > r.UsedBytes {
			t.Errorf("%s: %+v of %d used", fx.Method, k, r.UsedBytes)
		}
	}
	if !seen {
		t.Fatal("no storage.usage example has the kubernetes line")
	}
}
