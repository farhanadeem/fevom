package machines

import (
	"regexp"
	"strings"
)

func swapHostname(text, old, new string) (string, bool) {
	if strings.TrimSpace(text) != old || old == new {
		return text, false
	}
	return new + "\n", true
}

var hostsSpace = regexp.MustCompile(`[ \t]+`)

func swapHosts(text, old, new string) (string, bool) {
	if old == new || old == "" {
		return text, false
	}
	changed := false
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		body, comment, hasComment := strings.Cut(line, "#")
		if strings.TrimSpace(body) == "" {
			continue
		}
		seps := hostsSpace.FindAllStringIndex(body, -1)
		var out strings.Builder
		start := 0
		field := 0
		emit := func(tok string) {
			if field > 0 {
				if tok == old {
					tok, changed = new, true
				} else if rest, ok := strings.CutPrefix(tok, old+"."); ok {
					tok, changed = new+"."+rest, true
				}
			}
			if tok != "" {
				field++
			}
			out.WriteString(tok)
		}
		for _, s := range seps {
			emit(body[start:s[0]])
			out.WriteString(body[s[0]:s[1]])
			start = s[1]
		}
		emit(body[start:])
		lines[i] = out.String()
		if hasComment {
			lines[i] += "#" + comment
		}
	}
	return strings.Join(lines, "\n"), changed
}

func resetMetaData(text, instanceID, hostname string) string {
	set := map[string]string{"instance-id": instanceID}
	if hostname != "" {
		set["local-hostname"] = hostname
	}
	lines := strings.Split(text, "\n")
	done := map[string]bool{}
	for i, line := range lines {
		key, _, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		if v, want := set[key]; want {
			lines[i] = key + ": " + v
			done[key] = true
		}
	}
	var head []string
	for _, key := range []string{"instance-id", "local-hostname"} {
		if v, want := set[key]; want && !done[key] {
			head = append(head, key+": "+v)
		}
	}
	return strings.Join(append(head, lines...), "\n")
}

func swapMetaHostname(text, old, new string) (string, bool) {
	lines := strings.Split(text, "\n")
	changed := false
	for i, line := range lines {
		if v, ok := strings.CutPrefix(line, "local-hostname:"); ok && strings.TrimSpace(v) == old && old != new {
			lines[i], changed = "local-hostname: "+new, true
		}
	}
	return strings.Join(lines, "\n"), changed
}

func machineID(uuid string) string {
	return strings.ReplaceAll(strings.ToLower(uuid), "-", "")
}

func isSSHHostKey(name string) bool {
	return strings.HasPrefix(name, "ssh_host_") && (strings.HasSuffix(name, "_key") || strings.HasSuffix(name, "_key.pub"))
}
