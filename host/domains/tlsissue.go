package domains

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"time"

	"fev/host/trust"
	"fev/proto/agent"
)

type Issuer struct {
	Registry *Registry
	CA       func() (*trust.CA, error)
	OnMac    func(entry string) (on, wait bool)
}

const maxIssueLine = agent.MaxCSRBytes + 4096

const issueWait = 10 * time.Second

type deadliner interface {
	SetReadDeadline(time.Time) error
}

func (s *Issuer) Serve(conn io.ReadWriteCloser) error {
	defer conn.Close()
	if d, ok := conn.(deadliner); ok {
		d.SetReadDeadline(time.Now().Add(issueWait))
	}
	line, err := bufio.NewReaderSize(io.LimitReader(conn, maxIssueLine), 4096).ReadBytes('\n')
	if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
		return err
	}
	var req agent.TLSIssueRequest
	out := s.issue(line, &req)
	b, _ := json.Marshal(out)
	_, err = conn.Write(append(b, '\n'))
	return err
}

func (s *Issuer) issue(line []byte, req *agent.TLSIssueRequest) agent.TLSIssued {
	if err := json.Unmarshal(line, req); err != nil {
		return agent.TLSIssued{Error: "the request is not a tls.issue request"}
	}
	if s.CA == nil {
		return agent.TLSIssued{Error: "no CA"}
	}
	ca, err := s.CA()
	if err != nil || ca == nil {
		why := "HTTPS is off"
		if err != nil {
			why = err.Error()
		}
		return agent.TLSIssued{Error: why}
	}
	lifetime := agent.LeafLifetime
	if req.Requester != "" {
		lifetime = agent.ContainerLeafLifetime
	}
	certPEM, notAfter, err := ca.Issue(req.CSR, lifetime, func(name string) error {
		return s.Registry.CanIssue(name, req.Requester, s.OnMac)
	})
	if err != nil {
		return agent.TLSIssued{Error: err.Error()}
	}
	return agent.TLSIssued{Certificate: certPEM, Root: ca.RootPEM(), NotAfter: notAfter.UTC().Format(time.RFC3339)}
}
