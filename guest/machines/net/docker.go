package machnet

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

func uidIn(f io.Reader, user string) (uint32, bool) {
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 3 || fields[0] != user {
			continue
		}
		v, err := strconv.ParseUint(fields[2], 10, 32)
		return uint32(v), err == nil
	}
	return 0, false
}

func refusal(uid uint32, user string) string {
	who := "root"
	if user != "" {
		who = "root and " + user
	}
	msg := fmt.Sprintf("fev: uid %d may not use the engine from this machine; its socket serves %s", uid, who)
	body := fmt.Sprintf("{\"message\":%s}\n", strconv.Quote(msg))
	return fmt.Sprintf("HTTP/1.1 403 Forbidden\r\nContent-Type: application/json\r\nConnection: close\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
}
