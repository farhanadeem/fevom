package nfsview

import (
	"strings"
)

func (a *Answerer) Handle(channel string, data []byte) (answers [][]byte, bad []string) {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		reply, err := a.Answer(channel, line)
		if err != nil {
			bad = append(bad, err.Error())
			continue
		}
		answers = append(answers, []byte(reply+"\n"))
	}
	return answers, bad
}
