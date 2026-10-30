package webproxy

import (
	"container/list"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"
	"time"

	api "fev/proto/agent"
)

type Issuer interface {
	Issue(ctx context.Context, req api.TLSIssueRequest) (api.TLSIssued, error)
}

const MaxLeaves = 1024

type leaf struct {
	name      string
	cert      tls.Certificate
	notBefore time.Time
	notAfter  time.Time
}

func (l *leaf) stale(now time.Time) bool {
	life := l.notAfter.Sub(l.notBefore)
	return now.After(l.notAfter.Add(-life / 3))
}

type leaves struct {
	issuer Issuer
	now    func() time.Time

	mu      sync.Mutex
	byName  map[string]*list.Element
	order   *list.List
	pending map[string]*call
	gen     uint64
}

type call struct {
	gen  uint64
	done chan struct{}
	l    *leaf
	err  error
}

func newLeaves(issuer Issuer, now func() time.Time) *leaves {
	return &leaves{issuer: issuer, now: now, byName: map[string]*list.Element{}, order: list.New(), pending: map[string]*call{}}
}

func (c *leaves) get(ctx context.Context, name string) (*tls.Certificate, error) {
	now := c.now()
	c.mu.Lock()
	var have *leaf
	if e, ok := c.byName[name]; ok {
		have = e.Value.(*leaf)
		c.order.MoveToFront(e)
		if !have.stale(now) {
			c.mu.Unlock()
			return &have.cert, nil
		}
	}
	w, ok := c.pending[name]
	if !ok {
		w = &call{gen: c.gen, done: make(chan struct{})}
		c.pending[name] = w
		go c.fetch(name, w)
	}
	c.mu.Unlock()
	select {
	case <-w.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if w.err != nil {
		if have != nil && now.Before(have.notAfter) {
			return &have.cert, nil
		}
		return nil, w.err
	}
	return &w.l.cert, nil
}

func (c *leaves) fetch(name string, w *call) {
	defer func() {
		c.mu.Lock()
		if c.pending[name] == w {
			delete(c.pending, name)
		}
		if w.err == nil && w.gen == c.gen {
			if e, ok := c.byName[name]; ok {
				c.order.Remove(e)
			}
			c.byName[name] = c.order.PushFront(w.l)
			for c.order.Len() > MaxLeaves {
				old := c.order.Back()
				c.order.Remove(old)
				delete(c.byName, old.Value.(*leaf).name)
			}
		}
		c.mu.Unlock()
		close(w.done)
	}()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		w.err = err
		return
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{name}}, key)
	if err != nil {
		w.err = err
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), issueWait)
	defer cancel()
	out, err := c.issuer.Issue(ctx, api.TLSIssueRequest{CSR: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))})
	if err != nil {
		w.err = fmt.Errorf("a leaf for %s: %w", name, err)
		return
	}
	if out.Error != "" {
		w.err = fmt.Errorf("a leaf for %s: %s", name, out.Error)
		return
	}
	b, _ := pem.Decode([]byte(out.Certificate))
	if b == nil || b.Type != "CERTIFICATE" {
		w.err = errors.New("the daemon's answer holds no certificate")
		return
	}
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		w.err = err
		return
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(key.Public()) {
		w.err = errors.New("the daemon signed another key")
		return
	}
	w.l = &leaf{name: name, cert: tls.Certificate{Certificate: [][]byte{b.Bytes}, PrivateKey: key}, notBefore: cert.NotBefore, notAfter: cert.NotAfter}
}

const issueWait = 10 * time.Second

func (c *leaves) flush() {
	c.mu.Lock()
	c.gen++
	c.byName = map[string]*list.Element{}
	c.order.Init()
	c.pending = map[string]*call{}
	c.mu.Unlock()
}

func (c *leaves) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
