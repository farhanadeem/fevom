package fw

import (
	"fmt"
	"net/netip"
	"strings"
)

const K8sWebTable = "fev-k8sweb"

type ServiceTarget struct {
	Address    netip.Addr
	Redirect80 bool
	Own443     bool
}

func K8sWeb(services []ServiceTarget, webHTTP, webHTTPS int, web, https bool) string {
	var rules strings.Builder
	if web {
		for _, sv := range services {
			if !sv.Address.Is4() {
				continue
			}
			if sv.Redirect80 {
				fmt.Fprintf(&rules, "\t\tip daddr %s tcp dport 80 counter redirect to :%d\n", sv.Address, webHTTP)
			}
			if https && !sv.Own443 {
				fmt.Fprintf(&rules, "\t\tip daddr %s tcp dport 443 counter redirect to :%d\n", sv.Address, webHTTPS)
			}
		}
	}
	if rules.Len() == 0 {
		return K8sWebNone
	}
	return K8sWebNone + fmt.Sprintf("table ip %s {\n\tchain pre {\n\t\ttype nat hook prerouting priority -101; policy accept;\n%s\t}\n}\n", K8sWebTable, rules.String())
}

const K8sWebNone = "table ip " + K8sWebTable + "\ndelete table ip " + K8sWebTable + "\n"
