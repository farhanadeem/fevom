package sshfront

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestTheKeysAndAuthorizedKeys(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ssh")
	f := &Files{Dir: dir}
	if err := f.Ensure(); err != nil {
		t.Fatal(err)
	}
	mode := func(p string) os.FileMode {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return info.Mode().Perm()
	}
	if mode(dir) != 0o700 || mode(f.HostKeyPath()) != 0o600 || mode(f.IdentityPath()) != 0o600 || mode(f.AuthorizedKeysPath()) != 0o600 {
		t.Errorf("modes: dir %o, host %o, id %o, authorized %o", mode(dir), mode(f.HostKeyPath()), mode(f.IdentityPath()), mode(f.AuthorizedKeysPath()))
	}
	host1 := f.HostKeyLine()
	again := &Files{Dir: dir}
	if err := again.Ensure(); err != nil || again.HostKeyLine() != host1 {
		t.Errorf("a second start changed the host key: %v", err)
	}
	if !strings.HasPrefix(f.HostFingerprint(), "256 SHA256:") || !strings.HasSuffix(f.HostFingerprint(), " fev (ED25519)") {
		t.Errorf("fingerprint %q", f.HostFingerprint())
	}
	client, err := ssh.ParsePrivateKey(mustRead(t, f.IdentityPath()))
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := f.Authorized(client.PublicKey()); !ok {
		t.Errorf("the client key is not authorized: %s", why)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	user, _ := ssh.NewPublicKey(pub)
	if ok, _ := f.Authorized(user); ok {
		t.Error("a key nobody added is authorized")
	}
	line := string(ssh.MarshalAuthorizedKey(user))
	appendTo(t, f.AuthorizedKeysPath(), line)
	if ok, why := f.Authorized(user); !ok {
		t.Errorf("an added key is not authorized: %s", why)
	}
	os.Chmod(f.AuthorizedKeysPath(), 0o666)
	if ok, why := f.Authorized(client.PublicKey()); ok || !strings.Contains(why, "writable by others") {
		t.Errorf("a world-writable authorized_keys: %v %q", ok, why)
	}
	if f.AuthorizedKeysError() == "" {
		t.Error("ssh.info would not say why")
	}
	os.Chmod(f.AuthorizedKeysPath(), 0o600)
	if ok, _ := f.Authorized(client.PublicKey()); !ok || f.AuthorizedKeysError() != "" {
		t.Error("fixing the mode did not let the keys in again")
	}
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	limited, _ := ssh.NewPublicKey(pub2)
	time.Sleep(10 * time.Millisecond)
	appendTo(t, f.AuthorizedKeysPath(), `command="/bin/true",restrict `+string(ssh.MarshalAuthorizedKey(limited)))
	if ok, _ := f.Authorized(limited); ok {
		t.Error("a key with options was taken as unlimited")
	}
}

func TestAuthorizedKeysFollowsStrictModes(t *testing.T) {
	const me = 501
	ok := stamp{mode: 0o600, dirMode: 0o700, uid: me, dirUID: me}
	if why := strictModes(ok, me, "ak", "dir"); why != "" {
		t.Fatalf("the user's own, 0600 in 0700: %q", why)
	}
	for _, c := range []struct {
		st   stamp
		want string
	}{
		{stamp{mode: 0o620, dirMode: 0o700, uid: me, dirUID: me}, "ak is writable by others"},
		{stamp{mode: 0o600, dirMode: 0o702, uid: me, dirUID: me}, "dir is writable by others"},
		{stamp{mode: 0o600, dirMode: 0o700, uid: 0, dirUID: me}, "ak belongs to another user"},
		{stamp{mode: 0o600, dirMode: 0o700, uid: me, dirUID: 0}, "dir belongs to another user"},
		{stamp{mode: 0o600, dirMode: 0o755, uid: me, dirUID: 502}, "dir belongs to another user"},
	} {
		if why := strictModes(c.st, me, "ak", "dir"); !strings.HasPrefix(why, c.want) {
			t.Errorf("%+v: %q, want %q", c.st, why, c.want)
		}
	}
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fh.WriteString(text)
	fh.Close()
}

func TestTheClientFiles(t *testing.T) {
	f := &Files{Dir: filepath.Join(t.TempDir(), "my home", "ssh")}
	if err := f.Ensure(); err != nil {
		t.Fatal(err)
	}
	cmd := ShellQuote("/Users/a b/.fev/bin/fev") + " ssh --proxy"
	if err := f.WriteClient(Client{Port: 32122, ProxyCommand: cmd}); err != nil {
		t.Fatal(err)
	}
	config := string(mustRead(t, f.ConfigPath()))
	for _, want := range []string{
		"Host fev\n", "  HostName 127.0.0.1\n", "  Port 32122\n", "  User default\n",
		`  IdentityFile "` + f.IdentityPath() + `"` + "\n", "  IdentitiesOnly yes\n",
		`  UserKnownHostsFile "` + f.KnownHostsPath() + `"` + "\n",
		`  ProxyCommand "/Users/a b/.fev/bin/fev" ssh --proxy` + "\n", "  ProxyUseFdpass yes\n",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("config lacks %q:\n%s", want, config)
		}
	}
	known := string(mustRead(t, f.KnownHostsPath()))
	if !strings.HasPrefix(known, "fev,[127.0.0.1]:32122,[::1]:32122,[localhost]:32122 "+f.HostKeyLine()) {
		t.Errorf("known_hosts: %q", known)
	}
	if ShellQuote(`/tmp/x"y`) != `'/tmp/x"y'` {
		t.Errorf("quote: %s", ShellQuote(`/tmp/x"y`))
	}
}

func TestTheUserGrammar(t *testing.T) {
	machines := map[string]bool{"u24": true, "f43": true}
	is := func(n string) bool { return machines[n] }
	for login, want := range map[string]Target{
		"":                  {Machine: "u24"},
		"default":           {Machine: "u24"},
		"f43":               {Machine: "f43"},
		"alice":             {Machine: "u24", User: "alice"},
		"root@f43":          {Machine: "f43", User: "root"},
		"default@f43":       {Machine: "f43"},
		"root@default":      {Machine: "u24", User: "root"},
		"bob@nosuchmachine": {Machine: "nosuchmachine", User: "bob"},
	} {
		got, err := ParseUser(login, is, "u24")
		if err != nil || got != want {
			t.Errorf("%q: %+v %v, want %+v", login, got, err, want)
		}
	}
	for _, bad := range []string{"a@b@c", "a@Bad_Name", "a:b@f43"} {
		if _, err := ParseUser(bad, is, "u24"); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	if _, err := ParseUser("default", is, ""); err != ErrNoDefault {
		t.Errorf("no default machine: %v", err)
	}
	if got, err := ParseUser("f43", is, ""); err != nil || got.Machine != "f43" {
		t.Errorf("a named machine with no default: %+v %v", got, err)
	}
}
