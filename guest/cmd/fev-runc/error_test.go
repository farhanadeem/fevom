package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectionErrorReachesContainerd(t *testing.T) {
	for _, equals := range []bool{false, true} {
		log := filepath.Join(t.TempDir(), "log.json")
		args := []string{"--log", log, "--log-format", "json"}
		if equals {
			args = []string{"--log=" + log, "--log-format=json"}
		}
		c := parse(append(args, "create", "--bundle", "/bundle", "id"))
		runtimeError(c, errors.New("dev.fevom.emulation must be auto, fex or rosetta"))
		b, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		var entry struct{ Level, Msg, Time string }
		if err := json.Unmarshal(b, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Level != "error" || !strings.Contains(entry.Msg, "must be auto, fex or rosetta") || entry.Time == "" {
			t.Fatalf("containerd error: %s", b)
		}
	}
}
