package machines

import (
	"reflect"
	"testing"

	api "fev/proto/agent"
)

func TestASessionsProgramIsTheLoginShellWithACommandLine(t *testing.T) {
	u := &User{Name: "ada", UID: 501, GID: 501, Home: "/home/ada", Shell: "/bin/bash"}
	for _, c := range []struct {
		name string
		p    api.ExecStartParams
		argv []string
		file string
	}{
		{"argv", api.ExecStartParams{Argv: []string{"ls", "-l"}}, []string{"ls", "-l"}, ""},
		{"login", api.ExecStartParams{Login: true}, []string{"-bash"}, "/bin/bash"},
		{"shell", api.ExecStartParams{Shell: true, Argv: []string{"ls", "-l"}}, []string{"/bin/bash", "-c", "ls -l"}, "/bin/bash"},
		{"login shell", api.ExecStartParams{Login: true, Shell: true, Argv: []string{"nosuchcmd", "'a b'"}}, []string{"-bash", "-c", "nosuchcmd 'a b'"}, "/bin/bash"},
		{"sftp", api.ExecStartParams{Subsystem: api.SubsystemSFTP}, []string{"fev-guest", "sftp"}, "/opt/fev/fev-guest"},
	} {
		argv, file := sessionArgv(u, c.p)
		if !reflect.DeepEqual(argv, c.argv) || file != c.file {
			t.Errorf("%s: %q from %q, want %q from %q", c.name, argv, file, c.argv, c.file)
		}
	}
}
