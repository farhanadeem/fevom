package main

import (
	"bytes"
	"os"
	"testing"
)

func TestGeneratedFileIsCurrent(t *testing.T) {
	want, err := Generate("../../../proto/daemon")
	if err != nil {
		t.Fatal(err)
	}
	have, err := os.ReadFile("../../Sources/FevomAPI/DaemonAPI.generated.swift")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(have, want) {
		t.Fatal("app/Sources/FevomAPI/DaemonAPI.generated.swift is stale: run `go run .` in app/tools/swiftgen")
	}
}

func TestNames(t *testing.T) {
	for in, want := range map[string]string{
		"APIVersion": "apiVersion", "VMMTail": "vmmTail", "ID": "id", "Seq": "seq",
		"SSHAuthSock": "sshAuthSock", "RetryInMS": "retryInMS", "PID": "pid", "Default": "default",
	} {
		if got := propName(in); got != want {
			t.Errorf("propName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"machines.set_default": "machinesSetDefault", "daemon.hello": "daemonHello", "log.backlog_end": "logBacklogEnd",
	} {
		if got := methodIdent(in); got != want {
			t.Errorf("methodIdent(%q) = %q, want %q", in, got, want)
		}
	}
	if got := swiftQuote("a\"b\\c\n\x01"); got != `"a\"b\\c\n\u{1}"` {
		t.Errorf("swiftQuote: %s", got)
	}
}
