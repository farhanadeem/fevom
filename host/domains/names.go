package domains

import (
	"errors"
	"fmt"
	"strings"
)

const Zone = "fev.local"

const MaxNameLen = 253

const (
	labelComposeProject = "com.docker.compose.project"
	labelComposeService = "com.docker.compose.service"
	labelComposeNumber  = "com.docker.compose.container-number"
)

func canonical(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

func checkName(name string, wildcard bool) error {
	if wildcard {
		name = strings.TrimPrefix(name, "*.")
	}
	if name == "" || len(name) > MaxNameLen {
		return fmt.Errorf("%q is empty or over %d bytes", name, MaxNameLen)
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 {
			return fmt.Errorf("%q has a label of %d bytes", name, len(label))
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
			case c == '-' && i > 0 && i < len(label)-1:
			default:
				return fmt.Errorf("%q: %q is not a host name label", name, label)
			}
		}
	}
	return nil
}

func ParseLabel(value string) (names []string, errs []error) {
	seen := map[string]bool{}
	for _, f := range strings.Split(value, ",") {
		n := canonical(strings.TrimSpace(f))
		if n == "" {
			continue
		}
		if err := checkName(n, true); err != nil {
			errs = append(errs, err)
			continue
		}
		if n == "*."+Zone || n == Zone {
			errs = append(errs, fmt.Errorf("%q is fev's own zone", n))
			continue
		}
		if z := ownSubtree(strings.TrimPrefix(n, "*.")); z != "" {
			errs = append(errs, fmt.Errorf("%q is under %s, whose names fev answers itself", n, z))
			continue
		}
		if !strings.Contains(strings.TrimPrefix(n, "*."), ".") {
			errs = append(errs, fmt.Errorf("%q is a single label: a label's names have two or more", n))
			continue
		}
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	return names, errs
}

var ownZones = []string{"fev.internal", "docker.internal"}

func ownSubtree(name string) string {
	for _, z := range ownZones {
		if under(name, z) {
			return z
		}
	}
	return ""
}

func under(name, zone string) bool {
	return name == zone || strings.HasSuffix(name, "."+zone)
}

func IsLocal(name string) bool {
	name = strings.TrimPrefix(name, "*.")
	return strings.HasSuffix(name, ".local") && !under(name, Zone)
}

var errNoName = errors.New("no such name")
