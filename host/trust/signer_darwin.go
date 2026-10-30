//go:build darwin && cgo

package trust

import (
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

const SignerName = "fev-keysigner"

const tagSigner = "k2s:"

const (
	errSecInvalidKeyRef = -67712
	statusUnlocked      = 1
)

func signerPath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return filepath.Join(filepath.Dir(exe), SignerName), nil
}

type signerProc struct {
	path string

	mu        sync.Mutex
	cmd       *exec.Cmd
	in        io.WriteCloser
	out       io.ReadCloser
	cdhash    string
	kc, label string
}

var errSigner = errors.New("fev's key signer")

func startSigner(path string) (*signerProc, error) {
	p, err := signerPath(path)
	if err != nil {
		return nil, err
	}
	s := &signerProc{path: p}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.start(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *signerProc) start() error {
	cmd := exec.Command(s.path, "serve")
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w %s: %v", errSigner, s.path, err)
	}
	s.cmd, s.in, s.out = cmd, in, out
	st, b, err := s.roundTrip('V')
	if err != nil || st != 0 {
		s.stop()
		return fmt.Errorf("%w %s: no version answer (status %d, %v)", errSigner, s.path, st, err)
	}
	f := fields(b)
	if len(f) != 2 || string(f[0]) != "fev-keysigner 1" || len(f[1]) == 0 {
		s.stop()
		return fmt.Errorf("%w %s: not fev's key signer", errSigner, s.path)
	}
	s.cdhash = string(f[1])
	return nil
}

func (s *signerProc) stop() {
	if s.cmd == nil {
		return
	}
	s.in.Close()
	s.cmd.Wait()
	s.cmd = nil
}

func (s *signerProc) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop()
	return nil
}

func (s *signerProc) roundTrip(op byte, fs ...[]byte) (int32, []byte, error) {
	body := []byte{op}
	for _, f := range fs {
		body = binary.BigEndian.AppendUint16(body, uint16(len(f)))
		body = append(body, f...)
	}
	req := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
	if _, err := s.in.Write(append(req, body...)); err != nil {
		return 0, nil, err
	}
	var h [8]byte
	if _, err := io.ReadFull(s.out, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[:4])
	if n > 1<<20 {
		return 0, nil, fmt.Errorf("%w: an answer of %d bytes", errSigner, n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(s.out, b); err != nil {
		return 0, nil, err
	}
	return int32(binary.BigEndian.Uint32(h[4:])), b, nil
}

func (s *signerProc) call(op byte, fs ...[]byte) (int32, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for try := 0; ; try++ {
		if s.cmd == nil {
			if err := s.start(); err != nil {
				return 0, nil, err
			}
			if s.label != "" && op == 'S' {
				if st, _, err := s.roundTrip('F', []byte(s.kc), []byte(s.label)); err != nil || st != 0 {
					return st, nil, err
				}
			}
		}
		st, b, err := s.roundTrip(op, fs...)
		if err == nil {
			if op == 'F' || op == 'M' {
				if st == 0 {
					s.kc, s.label = string(fs[0]), string(fs[1])
				} else {
					s.label = ""
				}
			}
			return st, b, nil
		}
		s.stop()
		if try == 1 {
			return 0, nil, fmt.Errorf("%w %s: %v", errSigner, s.path, err)
		}
	}
}

func fields(b []byte) [][]byte {
	var out [][]byte
	for len(b) >= 2 {
		n := int(binary.BigEndian.Uint16(b))
		if len(b) < 2+n {
			break
		}
		out = append(out, b[2:2+n])
		b = b[2+n:]
	}
	return out
}

func (s *signerProc) find(kc, label string) (tag string, pub *ecdsa.PublicKey, st int32, err error) {
	st, b, err := s.call('F', []byte(kc), []byte(label))
	if err != nil || st != 0 {
		return "", nil, st, err
	}
	return parseFound(b)
}

func (s *signerProc) make(kc, label, tag string) (*ecdsa.PublicKey, int32, error) {
	st, b, err := s.call('M', []byte(kc), []byte(label), []byte(tag), []byte("self"))
	if err != nil || st != 0 {
		return nil, st, err
	}
	_, pub, st, err := parseFound(b)
	return pub, st, err
}

func parseFound(b []byte) (string, *ecdsa.PublicKey, int32, error) {
	f := fields(b)
	if len(f) != 2 {
		return "", nil, 0, fmt.Errorf("%w: a malformed answer", errSigner)
	}
	if _, err := ecdh.P256().NewPublicKey(f[1]); err != nil {
		return "", nil, 0, fmt.Errorf("the CA's key is not P-256: %w", err)
	}
	raw := f[1]
	return string(f[0]), &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(raw[1:33]), Y: new(big.Int).SetBytes(raw[33:65])}, 0, nil
}

func (s *signerProc) delete(kc, label string) (int32, error) {
	st, _, err := s.call('D', []byte(kc), []byte(label))
	return st, err
}

func (s *signerProc) unlocked(kc string) bool {
	st, b, err := s.call('K', []byte(kc))
	return err == nil && st == 0 && len(b) == 4 && binary.BigEndian.Uint32(b)&statusUnlocked != 0
}

type signerKey struct {
	p   *signerProc
	pub *ecdsa.PublicKey
}

func (k *signerKey) Public() crypto.PublicKey { return k.pub }

func (k *signerKey) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	if opts != nil && opts.HashFunc() != crypto.SHA256 || len(digest) != 32 {
		return nil, errors.New("the CA's key signs SHA-256 digests only")
	}
	st, b, err := k.p.call('S', digest)
	if err != nil {
		return nil, err
	}
	if st != 0 {
		return nil, k.p.refusal(st)
	}
	return b, nil
}

func (s *signerProc) refusal(st int32) error {
	switch {
	case (st == errSecAuthFailed || st == errSecInteractionNotAllow) && !s.unlocked(s.kc):
		return fmt.Errorf("%w: the login keychain is locked (OSStatus %d)", ErrKeyUnusable, st)
	case st == errSecAuthFailed || st == errSecInteractionNotAllow || st == errSecInvalidKeyRef:
		return fmt.Errorf("%w: the keychain does not admit fev's key signer %s to the key (OSStatus %d)", ErrKeyUnusable, s.cdhash, st)
	}
	return fmt.Errorf("fev's key signer: SecKeyCreateSignature: OSStatus %d", st)
}

func (s *signerProc) probe() ([]exportAttempt, error) {
	st, b, err := s.call('P')
	if err != nil {
		return nil, err
	}
	if st != 0 {
		return nil, fmt.Errorf("probe: OSStatus %d", st)
	}
	var out []exportAttempt
	for len(b) >= 2 {
		n := int(binary.BigEndian.Uint16(b))
		if len(b) < 2+n+8 {
			break
		}
		a := exportAttempt{Way: string(b[2 : 2+n])}
		b = b[2+n:]
		a.Status = int(int32(binary.BigEndian.Uint32(b)))
		m := int(binary.BigEndian.Uint32(b[4:]))
		b = b[8:]
		if len(b) < m {
			break
		}
		if m > 0 {
			a.Data = append([]byte(nil), b[:m]...)
		}
		b = b[m:]
		out = append(out, a)
	}
	return out, nil
}

func (s *signerProc) acl() (string, error) {
	st, b, err := s.call('A')
	if err != nil {
		return "", err
	}
	if st != 0 {
		return "", fmt.Errorf("access list: OSStatus %d", st)
	}
	return string(b), nil
}
