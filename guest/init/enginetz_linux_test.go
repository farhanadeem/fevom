package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTheMacsTimeZoneIsReadThroughTheShare(t *testing.T) {
	share := t.TempDir()
	zoneinfo := filepath.Join(share, "private/var/db/timezone/tz/2026e.1.0/zoneinfo/America")
	os.MkdirAll(zoneinfo, 0o755)
	os.MkdirAll(filepath.Join(share, "private/etc"), 0o755)
	os.Symlink("private/var", filepath.Join(share, "var"))
	os.Symlink("private/etc", filepath.Join(share, "etc"))
	os.Symlink("/var/db/timezone/tz/2026e.1.0/zoneinfo", filepath.Join(share, "private/var/db/timezone/zoneinfo"))
	if _, _, ok := macTimeZone(share); ok {
		t.Fatal("a zone with no /etc/localtime")
	}
	os.Symlink("/var/db/timezone/zoneinfo/America/New_York", filepath.Join(share, "private/etc/localtime"))
	if _, _, ok := macTimeZone(share); ok {
		t.Fatal("a zone with no file behind the link")
	}
	os.WriteFile(filepath.Join(zoneinfo, "New_York"), []byte("not a zone"), 0o644)
	if _, _, ok := macTimeZone(share); ok {
		t.Fatal("a file that is not TZif taken")
	}
	os.WriteFile(filepath.Join(zoneinfo, "New_York"), []byte("TZif2 the zone's data"), 0o644)
	zone, data, ok := macTimeZone(share)
	if !ok || zone != "America/New_York" || string(data) != "TZif2 the zone's data" {
		t.Fatalf("got %q %q %v", zone, data, ok)
	}
}

func TestTheUTCZoneFileIsValid(t *testing.T) {
	loc, err := time.LoadLocationFromTZData("Etc/UTC", utcTZif)
	if err != nil {
		t.Fatal(err)
	}
	name, off := time.Date(2026, 10, 20, 12, 0, 0, 0, loc).Zone()
	if name != "UTC" || off != 0 {
		t.Fatalf("zone %q offset %d", name, off)
	}
}
