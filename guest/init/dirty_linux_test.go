package main

import "testing"

func TestDirtyCap(t *testing.T) {
	const gib = int64(1) << 30
	const want = int64(5 << 30 / 4)
	for name, c := range map[string]struct {
		available   int64
		ratio       int
		want        int64
		dirty, back int64
	}{
		"8 GiB guest: the kernel's 1.6 GiB is above 1.25": {8 * gib, 20, want, want, want / 2},
		"32 GiB guest": {32 * gib, 20, want, want, want / 2},
		"4 GiB guest: the kernel's 0.8 GiB is lower already": {4 * gib, 20, want, 0, 0},
		"1 GiB guest": {gib, 20, want, 0, 0},
		"exactly at the kernel's limit: left alone": {want * 5, 20, want, 0, 0},
		"a ratio of 40":                    {4 * gib, 40, want, want, want / 2},
		"switched off on the command line": {32 * gib, 20, 0, 0, 0},
		"a cap asked for by hand":          {8 * gib, 20, 256 << 20, 256 << 20, 128 << 20},
		"memory not read":                  {0, 20, want, 0, 0},
	} {
		dirty, back := dirtyCap(c.available, c.ratio, c.want)
		if dirty != c.dirty || back != c.back {
			t.Errorf("%s: %d and %d, want %d and %d", name, dirty, back, c.dirty, c.back)
		}
	}
}
