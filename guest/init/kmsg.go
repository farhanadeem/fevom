package main

import (
	"strconv"
	"strings"
)

func parseKmsg(record string) (timeMS float64, level int, text string, ok bool) {
	header, rest, found := strings.Cut(record, ";")
	if !found {
		return 0, 0, "", false
	}
	fields := strings.Split(header, ",")
	if len(fields) < 3 {
		return 0, 0, "", false
	}
	prio, err1 := strconv.Atoi(fields[0])
	usec, err2 := strconv.ParseInt(fields[2], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, "", false
	}
	text, _, _ = strings.Cut(rest, "\n")
	return float64(usec) / 1000, prio & 7, text, true
}
