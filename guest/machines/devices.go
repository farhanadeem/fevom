package machines

import (
	"strconv"
	"strings"
)

func parseBlockMajor(text, driver string) (uint32, bool) {
	block := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "Block devices:":
			block = true
		case strings.HasSuffix(line, ":"):
			block = false
		case block:
			f := strings.Fields(line)
			if len(f) == 2 && f[1] == driver {
				if n, err := strconv.ParseUint(f[0], 10, 32); err == nil {
					return uint32(n), true
				}
			}
		}
	}
	return 0, false
}
