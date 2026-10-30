package webproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"

	api "fev/proto/agent"
)

const (
	CSRPath  = "/csr"
	RootPath = "/ca.pem"
)

const MachineRequester = "machine:"

func (p *Proxy) requester(a netip.Addr) (api.DomainTarget, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := a.Unmap().String()
	for _, t := range p.set.Targets {
		switch t.Kind {
		case api.TargetContainer:
			if t.Target == s {
				return t, true
			}
		case api.TargetMachine:
			if t.Address == s {
				return t, true
			}
		}
	}
	return api.DomainTarget{}, false
}

func (p *Proxy) serveIndex(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case RootPath:
		p.mu.Lock()
		root := p.set.CA
		p.mu.Unlock()
		if root == "" {
			textError(w, http.StatusNotFound, "fev: HTTPS is off: no root")
			return
		}
		w.Header().Set("Content-Type", "application/x-pem-file")
		io.WriteString(w, root)
	case CSRPath:
		p.csr(w, r)
	default:
		p.index(w, r)
	}
}

func (p *Proxy) csr(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		textError(w, http.StatusMethodNotAllowed, "fev: POST a PEM certificate request to "+CSRPath)
		return
	}
	p.mu.Lock()
	https := p.set.HTTPS
	p.mu.Unlock()
	if !https {
		textError(w, http.StatusServiceUnavailable, "fev: HTTPS is off (network.https)")
		return
	}
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		textError(w, http.StatusForbidden, "fev: no client address")
		return
	}
	t, ok := p.requester(peer.Addr())
	if !ok {
		textError(w, http.StatusForbidden, fmt.Sprintf("fev: %s is not a container's or a machine's own address (a container just started: ask again in a moment)", peer.Addr()))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, api.MaxCSRBytes+1))
	if err != nil {
		textError(w, http.StatusBadRequest, "fev: "+err.Error())
		return
	}
	if len(body) > api.MaxCSRBytes {
		textError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("fev: a request of over %d bytes", api.MaxCSRBytes))
		return
	}
	if _, err := api.CSRNames(string(body)); err != nil {
		textError(w, http.StatusBadRequest, "fev: "+err.Error())
		return
	}
	who := t.ID
	if t.Kind == api.TargetMachine {
		who = MachineRequester + t.ID
	}
	ctx, cancel := context.WithTimeout(r.Context(), issueWait)
	defer cancel()
	out, err := p.cfg.Issuer.Issue(ctx, api.TLSIssueRequest{CSR: string(body), Requester: who})
	switch {
	case err != nil:
		textError(w, http.StatusBadGateway, "fev: the daemon: "+err.Error())
	case out.Error != "":
		textError(w, http.StatusForbidden, "fev: "+out.Error)
	default:
		p.cfg.Logf("webproxy: a leaf for %s's own names until %s", targetLabel(t), out.NotAfter)
		w.Header().Set("Content-Type", "application/x-pem-file")
		io.WriteString(w, out.Certificate)
	}
}

func textError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	io.WriteString(w, msg+"\n")
}
