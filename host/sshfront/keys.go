package sshfront

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/crypto/ssh"
)

type Files struct {
	Dir string

	mu     sync.Mutex
	host   ssh.Signer
	stamp  stamp
	keys   map[string]bool
	akErr  string
	loaded bool
}

const (
	Alias       = "fev"
	DefaultUser = "default"
)

func (f *Files) HostKeyPath() string        { return filepath.Join(f.Dir, "host_ed25519") }
func (f *Files) IdentityPath() string       { return filepath.Join(f.Dir, "id_ed25519") }
func (f *Files) KnownHostsPath() string     { return filepath.Join(f.Dir, "known_hosts") }
func (f *Files) ConfigPath() string         { return filepath.Join(f.Dir, "config") }
func (f *Files) AuthorizedKeysPath() string { return filepath.Join(f.Dir, "authorized_keys") }

func (f *Files) Ensure() error {
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return err
	}
	if info, err := os.Stat(f.Dir); err != nil {
		return err
	} else if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(f.Dir, 0o700); err != nil {
			return err
		}
	}
	host, err := ensureKey(f.HostKeyPath(), "fev host key")
	if err != nil {
		return err
	}
	client, err := ensureKey(f.IdentityPath(), "fev")
	if err != nil {
		return err
	}
	pubLine := ssh.MarshalAuthorizedKey(client.PublicKey())
	pubLine = append(bytes.TrimRight(pubLine, "\n"), " fev\n"...)
	if err := writeIfChanged(f.IdentityPath()+".pub", pubLine, 0o644); err != nil {
		return err
	}
	if err := createOnce(f.AuthorizedKeysPath(), pubLine, 0o600); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	f.mu.Lock()
	f.host = host
	f.mu.Unlock()
	return nil
}

func ensureKey(path, comment string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		block, err := ssh.MarshalPrivateKey(priv, comment)
		if err != nil {
			return nil, err
		}
		if err := createOnce(path, pem.EncodeToMemory(block), 0o600); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, err
		}
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return signer, nil
}

func createOnce(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Link(tmp.Name(), path)
}

func writeIfChanged(path string, data []byte, mode fs.FileMode) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	return err
}

func (f *Files) HostSigner() (ssh.Signer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.host == nil {
		return nil, errors.New("sshfront: the keys are not loaded (Ensure)")
	}
	return f.host, nil
}

func (f *Files) HostFingerprint() string {
	s, err := f.HostSigner()
	if err != nil {
		return ""
	}
	return "256 " + ssh.FingerprintSHA256(s.PublicKey()) + " fev (ED25519)"
}

func (f *Files) HostKeyLine() string {
	s, err := f.HostSigner()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey())))
}

type stamp struct {
	ino, size            uint64
	mtime, ctime         syscall.Timespec
	mode                 fs.FileMode
	dirMode, uid, dirUID uint32
}

func strictModes(st stamp, uid int, path, dir string) string {
	switch {
	case st.mode&0o022 != 0:
		return path + " is writable by others; no key in it is accepted until it is not"
	case st.dirMode&0o022 != 0:
		return dir + " is writable by others; no key in " + path + " is accepted until it is not"
	case int(st.uid) != uid:
		return path + " belongs to another user; no key in it is accepted"
	case int(st.dirUID) != uid:
		return dir + " belongs to another user; no key in " + path + " is accepted"
	}
	return ""
}

func (f *Files) Authorized(key ssh.PublicKey) (ok bool, why string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloadLocked()
	if f.akErr != "" {
		return false, f.akErr
	}
	return f.keys[string(key.Marshal())], ""
}

func (f *Files) AuthorizedKeysError() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloadLocked()
	return f.akErr
}

func (f *Files) reloadLocked() {
	path := f.AuthorizedKeysPath()
	var st syscall.Stat_t
	var dir syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		f.keys, f.akErr, f.loaded, f.stamp = nil, fmt.Sprintf("%s cannot be read: %v", path, err), false, stamp{}
		return
	}
	syscall.Stat(f.Dir, &dir)
	now := stamp{ino: st.Ino, size: uint64(st.Size), mtime: st.Mtimespec, ctime: st.Ctimespec,
		mode: fs.FileMode(st.Mode & 0o7777), dirMode: uint32(dir.Mode & 0o7777), uid: st.Uid, dirUID: dir.Uid}
	if f.loaded && now == f.stamp {
		return
	}
	f.stamp, f.loaded = now, true
	f.keys, f.akErr = nil, ""
	if f.akErr = strictModes(now, os.Getuid(), path, f.Dir); f.akErr != "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		f.akErr = fmt.Sprintf("%s cannot be read: %v", path, err)
		return
	}
	f.keys = map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, options, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			continue
		}
		if len(options) > 0 {
			continue
		}
		f.keys[string(key.Marshal())] = true
	}
}

type Client struct {
	Port         int
	ProxyCommand string
}

func (f *Files) WriteClient(c Client) error {
	escape := func(s string) string { return strings.ReplaceAll(s, "%", "%%") }
	quote := func(s string) string { return `"` + escape(s) + `"` }
	config := fmt.Sprintf(`# fev's SSH entry, written by fev's daemon; ~/.ssh/config
# includes it. "ssh fev" is the default machine's default user,
# "ssh MACHINE@fev" a machine's and "ssh USER@MACHINE@fev" a user's.
Host %s
  HostName 127.0.0.1
  Port %d
  User %s
  IdentityFile %s
  IdentitiesOnly yes
  UserKnownHostsFile %s
  ProxyCommand %s
  ProxyUseFdpass yes
`, Alias, c.Port, DefaultUser, quote(f.IdentityPath()), quote(f.KnownHostsPath()), escape(c.ProxyCommand))
	if err := writeIfChanged(f.ConfigPath(), []byte(config), 0o600); err != nil {
		return err
	}
	line := f.HostKeyLine()
	if line == "" {
		return errors.New("sshfront: the keys are not loaded (Ensure)")
	}
	known := fmt.Sprintf("%s,[127.0.0.1]:%d,[::1]:%d,[localhost]:%d %s\n", Alias, c.Port, c.Port, c.Port, line)
	return writeIfChanged(f.KnownHostsPath(), []byte(known), 0o600)
}

func ShellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, "\"$`\\!\n") {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
