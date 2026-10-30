package daemon

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"fev/host/trust"
	"fev/proto/agent"
)

func TrustFromKeychains() ([]string, error) {
	certs, err := trust.Roots()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(certs))
	for _, c := range certs {
		out = append(out, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})))
	}
	return out, nil
}

func TrustFromFile(path string) func() ([]string, error) {
	return func() ([]string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if len(b) > agent.MaxTrustBytes {
			return nil, fmt.Errorf("%s is more than %d bytes", path, agent.MaxTrustBytes)
		}
		var out []string
		for {
			var blk *pem.Block
			blk, b = pem.Decode(b)
			if blk == nil {
				break
			}
			if blk.Type != "CERTIFICATE" {
				continue
			}
			if _, err := x509.ParseCertificate(blk.Bytes); err != nil {
				return nil, fmt.Errorf("%s: a certificate that does not parse: %w", path, err)
			}
			out = append(out, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: blk.Bytes})))
		}
		return out, nil
	}
}

const trustBound = 30 * time.Second

func (m *Machines) sendTrust(boot string, client *agent.Client) {
	read := m.Trust
	if read == nil {
		read = TrustFromKeychains
	}
	roots, err := read()
	if err != nil {
		if !errors.Is(err, trust.ErrUnsupported) {
			m.log.Warn("machines: the Mac's trusted roots could not be read; the machines keep the ones they have", "boot", boot, "err", err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), trustBound)
	defer cancel()
	r, err := client.TrustSet(ctx, roots)
	switch {
	case err == nil:
		m.log.Info("machines: the Mac's trusted roots given to the guest", "boot", boot, "count", r.Count, "digest", clip(r.Digest, 12))
	case agent.IsNotImplemented(err):
	default:
		m.log.Warn("machines: the guest did not take the Mac's trusted roots", "boot", boot, "err", err)
	}
}
