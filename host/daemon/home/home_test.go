package home

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAMovedHomeTakesTheDataImageWithIt(t *testing.T) {
	t.Setenv(Env, "")
	l, err := Resolve("", "")
	if err != nil {
		t.Fatal(err)
	}
	user, _ := os.UserHomeDir()
	if l.Home != filepath.Join(user, ".fev") || l.DataImage() != filepath.Join(user, "Library/Application Support/dev.fevom/data/data.img") {
		t.Errorf("default layout: %+v", l)
	}

	l, _ = Resolve("/tmp/bh.x", "")
	if l.Home != "/tmp/bh.x" || l.DataImage() != "/tmp/bh.x/data/data.img" {
		t.Errorf("--home: %+v", l)
	}
	t.Setenv(Env, "/tmp/bh.env")
	l, _ = Resolve("", "")
	if l.Home != "/tmp/bh.env" || l.DataDir != "/tmp/bh.env/data" {
		t.Errorf("FEV_HOME: %+v", l)
	}
	l, _ = Resolve("/tmp/bh.flag", "/tmp/elsewhere")
	if l.Home != "/tmp/bh.flag" || l.DataDir != "/tmp/elsewhere" {
		t.Errorf("--home and --data-dir with FEV_HOME set: %+v", l)
	}
	if l.Socket() != "/tmp/bh.flag/run/fevd.sock" || l.VMDir() != "/tmp/bh.flag/run/vm" {
		t.Errorf("paths: %s %s", l.Socket(), l.VMDir())
	}
}

func TestTheUsersOwnHomeNamedByAFlagIsNotMoved(t *testing.T) {
	t.Setenv(Env, "")
	user, _ := os.UserHomeDir()
	own := filepath.Join(user, ".fev")
	data := filepath.Join(user, "Library/Application Support/dev.fevom/data")
	for _, how := range []string{"flag", "env"} {
		flag := ""
		if how == "flag" {
			flag = own
		} else {
			t.Setenv(Env, own+"/")
		}
		l, err := Resolve(flag, "")
		if err != nil {
			t.Fatal(err)
		}
		if l.Home != own || l.DataDir != data || l.Moved {
			t.Errorf("%s naming %s: %+v", how, own, l)
		}
		t.Setenv(Env, "")
	}
	if l, _ := Resolve("/tmp/bh.x", ""); !l.Moved {
		t.Errorf("a scratch home is not moved: %+v", l)
	}

	account := accountHome()
	if account == "" {
		t.Skip("no account home in the user database")
	}
	t.Setenv("HOME", t.TempDir())
	l, err := Resolve(filepath.Join(account, ".fev"), "")
	if err != nil {
		t.Fatal(err)
	}
	if l.Moved || l.DataDir != filepath.Join(account, "Library/Application Support/dev.fevom/data") {
		t.Errorf("HOME a scratch directory, --home the account's ~/.fev: %+v", l)
	}
}

func TestAHomeTooLongForUnixSocketsIsRefused(t *testing.T) {
	if err := (Layout{Home: "/tmp/bh.abcdef"}).Validate(); err != nil {
		t.Errorf("a short home was refused: %v", err)
	}
	fits := "/" + strings.Repeat("h", MaxSocketPath-len("/run/vm/"+longestSocket)-1)
	if err := (Layout{Home: fits}).Validate(); err != nil {
		t.Errorf("a home whose longest socket is exactly %d bytes was refused: %v", MaxSocketPath, err)
	}
	err := (Layout{Home: fits + "h"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "shorter --home") {
		t.Errorf("a home one byte too long: %v", err)
	}
}

func TestTheRunDirectoriesAreForThisUserAlone(t *testing.T) {
	l := Layout{Home: filepath.Join(t.TempDir(), "h")}
	os.MkdirAll(l.VMDir(), 0o755)
	os.MkdirAll(l.LogDir(), 0o755)
	if err := l.Create(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{l.RunDir(), l.VMDir(), l.LogDir()} {
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v %v", dir, info.Mode().Perm(), err)
		}
	}
	for _, dir := range []string{filepath.Dir(l.ConfigFile())} {
		if _, err := os.Stat(dir); err != nil {
			t.Error(err)
		}
	}
	want := Info{PID: 42, Build: "abc", APIVersion: 1, VMMPID: 43, Started: "2026-10-08T12:00:00Z"}
	if err := l.WriteInfo(want); err != nil {
		t.Fatal(err)
	}
	if got, err := l.ReadInfo(); err != nil || got != want {
		t.Errorf("info: %+v %v", got, err)
	}
}

func TestALockIsHeldUntilItsFileIsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	first, held, err := Lock(path, false)
	if err != nil || held {
		t.Fatalf("first lock: held=%v err=%v", held, err)
	}
	if f, held, err := Lock(path, false); err != nil || !held || f != nil {
		t.Fatalf("second lock while the first is held: held=%v err=%v", held, err)
	}
	got := make(chan error, 1)
	go func() {
		f, _, err := Lock(path, true)
		if f != nil {
			defer f.Close()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("a blocking lock returned while the lock was held: %v", err)
	default:
	}
	first.Close()
	if err := <-got; err != nil {
		t.Fatal(err)
	}
}

func TestThePlacedDataDirectoryIsResolved(t *testing.T) {
	h := filepath.Join(t.TempDir(), "h")
	l, err := Resolve(h, "")
	if err != nil || l.Placed || l.DataDir != filepath.Join(h, "data") || l.DefaultDataDir != l.DataDir {
		t.Fatalf("%+v %v", l, err)
	}
	if err := l.WritePlacement("/Volumes/Ext/fev"); err != nil {
		t.Fatal(err)
	}
	if l, err = Resolve(h, ""); err != nil || !l.Placed || l.DataDir != "/Volumes/Ext/fev" || l.DataImage() != "/Volumes/Ext/fev/data.img" {
		t.Fatalf("placed: %+v %v", l, err)
	}
	if f, _ := Resolve(h, "/tmp/elsewhere"); f.Placed || f.DataDir != "/tmp/elsewhere" {
		t.Fatalf("the flag: %+v", f)
	}
	if err := l.WritePlacement(l.DefaultDataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.PlacementFile()); !os.IsNotExist(err) {
		t.Fatalf("the note is still there: %v", err)
	}
	if l, _ = Resolve(h, ""); l.Placed || l.DataDir != filepath.Join(h, "data") {
		t.Fatalf("back: %+v", l)
	}
	os.WriteFile(l.PlacementFile(), []byte(`{"dir":"rel/dir"}`), 0o644)
	if _, err := Resolve(h, ""); err == nil {
		t.Fatal("a relative directory was taken")
	}
}
