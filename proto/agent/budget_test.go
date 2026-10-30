package agent

import (
	"encoding/json"
	"testing"
)

func TestBudgetBytes(t *testing.T) {
	const gib = int64(1) << 30
	const mib = int64(1) << 20
	for name, c := range map[string]struct{ available, margin, image, used, reserved, want int64 }{
		"plain: 15 GiB free, the image holds what is used":         {15 * gib, 5 * gib, 3 * gib, 3 * gib, 0, 10 * gib},
		"a preallocation the image does not hold yet is charged":   {15 * gib, 5 * gib, 3 * gib, 5 * gib, 0, 8 * gib},
		"what an open transaction has landed is counted once":      {15 * gib, 5 * gib, 4 * gib, 3 * gib, gib, 11 * gib},
		"and no more of it than the group has reserved":            {15 * gib, 5 * gib, 4 * gib, 3 * gib, 256 * mib, 10*gib + 256*mib},
		"at the margin the cleanup's floor is left":                {5 * gib, 5 * gib, gib, gib, 0, CleanupBytes},
		"just above the margin, the floor":                         {5*gib + 4*mib, 5 * gib, gib, gib, 0, CleanupBytes},
		"under the margin, above its half: the floor":              {3 * gib, 5 * gib, gib, gib, 0, CleanupBytes},
		"at half the margin it is nothing, not negative":           {2*gib + 512*mib + 16*mib, 5 * gib, gib, gib, 0, 0},
		"under the margin's half it is nothing, not negative":      {2 * gib, 5 * gib, gib, gib, 0, 0},
		"the space suite's claim after its fill: the floor":        {263073792, 268435456, 1875709952, 1874628608, 0, CleanupBytes},
		"a preallocation larger than what the Mac has":             {8 * gib, 5 * gib, gib, 9 * gib, 0, 0},
		"freed and untrimmed blocks in the image do not widen it":  {10 * gib, 5 * gib, 2 * gib, gib, 0, 5 * gib},
		"the review's fill of small files: nothing, not 447 MiB":   {32899072, 268435456, 2105950208, 1423036416, 64 * mib, 0},
		"new promises never pass what the Mac has over its margin": {6 * gib, 5 * gib, 9 * gib, 3 * gib, 2 * gib, 3 * gib},
		"no margin": {gib, 0, 0, 0, 0, gib},
	} {
		got := BudgetBytes(c.available, c.margin, c.image, c.used, c.reserved)
		if got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
		if over := max(c.available-c.margin, CleanupBytes); got-c.reserved > over && got-c.reserved > 0 {
			t.Errorf("%s: %d more can be promised with %d above the margin", name, got-c.reserved, over)
		}
		if got > max(c.available-c.margin, 0) && c.available-(c.used-c.image)-got < c.margin/2 {
			t.Errorf("%s: %d takes the Mac below half its margin", name, got)
		}
	}
	for name, c := range map[string]struct{ image, used, reserved, want int64 }{
		"the image holds what is used":                 {3 * gib, 3 * gib, 0, 0},
		"freed and not discarded":                      {5 * gib, 3 * gib, 0, 2 * gib},
		"less what the group has reserved":             {5 * gib, 3 * gib, gib, gib},
		"a preallocation: nothing to give back":        {3 * gib, 5 * gib, 0, 0},
		"all of it may be an open transaction's":       {4 * gib, 3 * gib, 2 * gib, 0},
		"the review's delete: 1,871 MiB held, 19 used": {1871 * mib, 19 * mib, 0, 1852 * mib},
	} {
		if got := ReclaimableBytes(c.image, c.used, c.reserved); got != c.want {
			t.Errorf("reclaimable, %s: %d, want %d", name, got, c.want)
		}
	}
	if DirtyCapBytes != 1342177280 {
		t.Errorf("the cap is %d bytes, not a quarter of 5 GiB", DirtyCapBytes)
	}
}

func TestBudgetFixtureObeysTheSum(t *testing.T) {
	fixtures, err := Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, fx := range fixtures {
		if fx.Method != MethodStorageBudget {
			continue
		}
		seen++
		var p BudgetParams
		var r Budget
		if err := json.Unmarshal(fx.Params, &p); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(fx.Result, &r); err != nil {
			t.Fatal(err)
		}
		if p.Unlimited {
			if r.Limited || r.LimitBytes != 0 || r.BudgetBytes != 0 {
				t.Errorf("%s: unlimited, and the result is %+v", fx.Method, r)
			}
			continue
		}
		if want := BudgetBytes(p.AvailableBytes, p.MarginBytes, p.ImageBytes, r.UsedBytes, r.ReservedBytes); r.BudgetBytes != want {
			t.Errorf("%s: budget %d, the sum gives %d", fx.Method, r.BudgetBytes, want)
		}
		if !r.Limited || r.LimitBytes != r.ReferencedBytes+r.BudgetBytes {
			t.Errorf("%s: limit %d is not referenced %d plus budget %d", fx.Method, r.LimitBytes, r.ReferencedBytes, r.BudgetBytes)
		}
	}
	if seen < 2 {
		t.Errorf("%d storage.budget fixtures, want the plain one and the unlimited one", seen)
	}
}
