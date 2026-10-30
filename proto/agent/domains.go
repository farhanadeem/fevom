package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"slices"
	"strings"
	"time"
)

const (
	MethodDomainsSet      = "domains.set"
	MethodMacAddressesSet = "mac_addresses.set"
)

const StreamTLSIssue = "tls.issue"

const (
	LabelDomains   = "dev.fevom.domains"
	LabelHTTPPort  = "dev.fevom.http-port"
	LabelHTTPSPort = "dev.fevom.https-port"
	LabelCA        = "dev.fevom.ca"
)

const (
	TargetContainer = "container"
	TargetMachine   = "machine"
	TargetK8s       = "k8s"
)

type DomainsParams struct {
	Seq     uint64         `json:"seq"`
	HTTPS   bool           `json:"https"`
	Index   string         `json:"index"`
	Targets []DomainTarget `json:"targets"`
	CA      string         `json:"ca,omitempty"`
}

type DomainTarget struct {
	Kind       string   `json:"kind"`
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Address    string   `json:"address"`
	Names      []string `json:"names"`
	Target     string   `json:"target,omitempty"`
	Target6    string   `json:"target6,omitempty"`
	Address6   string   `json:"address6,omitempty"`
	NetNS      string   `json:"netns,omitempty"`
	HTTPPort   int      `json:"http_port,omitempty"`
	HTTPSPort  int      `json:"https_port,omitempty"`
	Exposed    []string `json:"exposed,omitempty"`
	Redirect80 bool     `json:"redirect80,omitempty"`
	Own443     bool     `json:"own443,omitempty"`
	Pass443    bool     `json:"pass443,omitempty"`
	Hosts      []string `json:"hosts,omitempty"`
}

type DomainsApplied struct {
	Seq     uint64   `json:"seq"`
	Targets int      `json:"targets"`
	Errors  []string `json:"errors,omitempty"`
}

func CheckDomains(p DomainsParams) error {
	if _, err := netip.ParseAddr(p.Index); err != nil {
		return fmt.Errorf("index %q: %v", p.Index, err)
	}
	addrs := map[string]bool{p.Index: true}
	owner := map[string]string{}
	for i, t := range p.Targets {
		where := fmt.Sprintf("targets[%d]", i)
		a, err := netip.ParseAddr(t.Address)
		if err != nil || !a.Is4() {
			return fmt.Errorf("%s: address %q is not an IPv4 address", where, t.Address)
		}
		if addrs[t.Address] {
			return fmt.Errorf("%s: address %s twice", where, t.Address)
		}
		addrs[t.Address] = true
		six := func(s string) bool {
			a, err := netip.ParseAddr(s)
			return err == nil && a.Is6() && !a.Is4In6() && a.Zone() == ""
		}
		switch t.Kind {
		case TargetContainer:
			if a, err := netip.ParseAddr(t.Target); err != nil || !a.Is4() {
				return fmt.Errorf("%s: a container with target %q", where, t.Target)
			}
			if t.Address6 != "" || (t.Target6 != "" && !six(t.Target6)) {
				return fmt.Errorf("%s: a container with address6 %q or target6 %q", where, t.Address6, t.Target6)
			}
		case TargetMachine:
			if t.Target != "" || t.NetNS != "" || t.Target6 != "" {
				return fmt.Errorf("%s: a machine with a target or a namespace", where)
			}
			if t.Address6 != "" && !six(t.Address6) {
				return fmt.Errorf("%s: a machine with address6 %q", where, t.Address6)
			}
			if err := CheckMachineName(t.ID); err != nil {
				return fmt.Errorf("%s: %v", where, err)
			}
		case TargetK8s:
			if t.Target != "" || t.NetNS != "" || t.Target6 != "" || t.Address6 != "" {
				return fmt.Errorf("%s: a service with a target, a namespace or an IPv6 address", where)
			}
			if t.Own443 && t.Pass443 {
				return fmt.Errorf("%s: a service whose 443 is both its own and fev's", where)
			}
			if !slices.IsSorted(t.Hosts) {
				return fmt.Errorf("%s: hosts not sorted", where)
			}
			for _, h := range t.Hosts {
				if h == "" || h != strings.ToLower(h) || strings.ContainsAny(h, " /:") {
					return fmt.Errorf("%s: host %q", where, h)
				}
			}
		default:
			return fmt.Errorf("%s: kind %q", where, t.Kind)
		}
		if t.Kind != TargetK8s && (t.Redirect80 || t.Own443 || t.Pass443 || len(t.Hosts) > 0) {
			return fmt.Errorf("%s: a %s with a service's redirect80, own443, pass443 or hosts", where, t.Kind)
		}
		if t.ID == "" || len(t.Names) == 0 {
			return fmt.Errorf("%s: no id or no names", where)
		}
		if !slices.IsSorted(t.Names) {
			return fmt.Errorf("%s: names not sorted", where)
		}
		for _, n := range t.Names {
			if n != strings.ToLower(n) || n == "" || strings.HasSuffix(n, ".") {
				return fmt.Errorf("%s: name %q is not lower case and without its root dot", where, n)
			}
			if o, ok := owner[n]; ok {
				return fmt.Errorf("%s: name %q is also %s's", where, n, o)
			}
			owner[n] = t.ID
		}
		for _, port := range []int{t.HTTPPort, t.HTTPSPort} {
			if port < 0 || port > 65535 {
				return fmt.Errorf("%s: port %d", where, port)
			}
		}
	}
	return nil
}

type MacAddresses struct {
	Addresses []string `json:"addresses"`
}

const MaxMacAddresses = 256

func CheckMacAddresses(p MacAddresses) error {
	if len(p.Addresses) > MaxMacAddresses {
		return fmt.Errorf("more than %d addresses", MaxMacAddresses)
	}
	for i, s := range p.Addresses {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return fmt.Errorf("addresses[%d]: %q is not an IP address", i, s)
		}
		if a.IsLoopback() || a.IsMulticast() || a.IsUnspecified() {
			return fmt.Errorf("addresses[%d]: %s is not a unicast address of an interface", i, s)
		}
	}
	return nil
}

const (
	LeafLifetime          = 7 * 24 * time.Hour
	ContainerLeafLifetime = 90 * 24 * time.Hour
	LeafBackdate          = time.Hour
	MaxCSRBytes           = 16 << 10
)

type TLSIssueRequest struct {
	CSR       string `json:"csr"`
	Requester string `json:"requester,omitempty"`
}

type TLSIssued struct {
	Certificate string `json:"certificate,omitempty"`
	Root        string `json:"root,omitempty"`
	NotAfter    string `json:"not_after,omitempty"`
	Error       string `json:"error,omitempty"`
}

func CSRNames(csrPEM string) ([]string, error) {
	if len(csrPEM) > MaxCSRBytes {
		return nil, fmt.Errorf("the CSR is over %d bytes", MaxCSRBytes)
	}
	b, rest := pem.Decode([]byte(csrPEM))
	if b == nil || b.Type != "CERTIFICATE REQUEST" || strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("not one PEM block of a certificate request")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, err
	}
	k, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || k.Curve != elliptic.P256() {
		return nil, errors.New("the key is not P-256")
	}
	if len(csr.IPAddresses)+len(csr.EmailAddresses)+len(csr.URIs) != 0 || len(csr.DNSNames) == 0 {
		return nil, errors.New("the request names no DNS name, or something other than DNS names")
	}
	names := make([]string, len(csr.DNSNames))
	for i, n := range csr.DNSNames {
		names[i] = strings.ToLower(n)
	}
	return names, nil
}

const MaxMachineMounts = 32

type MachineMount struct {
	Source string `json:"source"`
	Dest   string `json:"dest,omitempty"`
}

func CheckIsolation(c MachineConfig) error {
	if !c.Isolated {
		if c.IsolateNetwork || len(c.Mounts) > 0 || c.ForwardSSHAgent {
			return errors.New("isolate_network, mounts and forward_ssh_agent are for an isolated machine")
		}
		return nil
	}
	if len(c.Mounts) > MaxMachineMounts {
		return fmt.Errorf("more than %d mounts", MaxMachineMounts)
	}
	if len(c.Mounts) > 0 && !c.Share {
		return errors.New("mounts need the Mac's share (files.share)")
	}
	clean := func(p string) bool {
		return path.IsAbs(p) && path.Clean(p) == p && p != "/" && !strings.ContainsRune(p, 0)
	}
	dests := map[string]bool{}
	for i, m := range c.Mounts {
		dest := m.Dest
		if dest == "" {
			dest = m.Source
		}
		if !clean(m.Source) || !clean(dest) {
			return fmt.Errorf("mounts[%d]: %q:%q is not two absolute, clean paths", i, m.Source, m.Dest)
		}
		if dests[dest] {
			return fmt.Errorf("mounts[%d]: %s is bound twice", i, dest)
		}
		dests[dest] = true
	}
	return nil
}

func ParseMachineMount(s string) (MachineMount, error) {
	src, dest, _ := strings.Cut(s, ":")
	m := MachineMount{Source: src, Dest: dest}
	if strings.Contains(dest, ":") {
		return MachineMount{}, fmt.Errorf("%q: more than one ':'", s)
	}
	if err := CheckIsolation(MachineConfig{Isolated: true, Share: true, Mounts: []MachineMount{m}}); err != nil {
		return MachineMount{}, fmt.Errorf("%q: %w", s, err)
	}
	return m, nil
}
