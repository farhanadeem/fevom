package dockerctx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"fev/host/docker/fakeguest"
	api "fev/proto/daemon"
)

func scratch(t *testing.T) Store {
	t.Helper()
	dir := t.TempDir()
	return Store{Dir: filepath.Join(dir, "docker"), Socket: "/Users/someone/.fev/run/docker.sock", StateFile: filepath.Join(dir, "fev", "config", "docker-context.json")}
}

func (s Store) write(t *testing.T, config string) {
	t.Helper()
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.configFile(), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (s Store) read(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(s.configFile())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (s Store) other(t *testing.T, name string) {
	t.Helper()
	dir := metaDir(s.Dir, name)
	os.MkdirAll(dir, 0o755)
	data := fmt.Sprintf(`{"Name":%q,"Metadata":{"Description":"Another runtime"},"Endpoints":{"docker":{"Host":"unix:///tmp/other.sock","SkipTLSVerify":false}}}`, name)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func status(t *testing.T, s Store) api.DockerContext {
	t.Helper()
	st, err := s.Status()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestTheContextIsDefinedWhereTheDockerCLILooks(t *testing.T) {
	s := scratch(t)
	if st := status(t, s); st.Exists || st.Selected || st.Current != Default || st.Name != "fev" {
		t.Fatalf("before: %+v", st)
	}
	if err := s.Ensure(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir, "contexts", "meta", "e8473e0e1fecfff1b2a506e693d14ce5640b7e55427ac04f77bbea7067c74e76", "meta.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"Endpoints":{"docker":{"Host":"unix:///Users/someone/.fev/run/docker.sock","SkipTLSVerify":false}},"Metadata":{"Description":"Fevom"},"Name":"fev"}`
	if string(data) != want {
		t.Fatalf("meta.json is\n%s\nwant\n%s", data, want)
	}
	if _, err := os.Stat(s.configFile()); !os.IsNotExist(err) {
		t.Fatal("Ensure wrote config.json")
	}
	if st := status(t, s); !st.Exists || st.Selected {
		t.Fatalf("after: %+v", st)
	}
	before, _ := os.Stat(path)
	s.Ensure()
	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) {
		t.Error("an up-to-date definition was written again")
	}
	s.Socket = "/tmp/elsewhere/docker.sock"
	s.Ensure()
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "unix:///tmp/elsewhere/docker.sock") {
		t.Fatalf("meta.json did not follow the socket: %s", data)
	}
}

func TestSelectingWhenNothingIsSelected(t *testing.T) {
	s := scratch(t)
	selected, left, err := s.Select()
	if err != nil || !selected || left != "" {
		t.Fatalf("%v, %q, %v", selected, left, err)
	}
	if got := s.read(t); got != "{\n\t\"currentContext\": \"fev\"\n}" {
		t.Fatalf("config.json is %q", got)
	}
	if info, _ := os.Stat(s.configFile()); info.Mode().Perm() != 0o600 {
		t.Fatalf("config.json mode %v", info.Mode())
	}
	if st := status(t, s); !st.Exists || !st.Selected || st.Current != "fev" {
		t.Fatalf("%+v", st)
	}
	before, _ := os.Stat(s.configFile())
	if selected, _, err := s.Select(); err != nil || !selected {
		t.Fatal(selected, err)
	}
	if after, _ := os.Stat(s.configFile()); !after.ModTime().Equal(before.ModTime()) {
		t.Error("config.json was written again")
	}
	if err := s.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := s.read(t); got != "{}" {
		t.Fatalf("after restore config.json is %q", got)
	}
	if st := status(t, s); !st.Exists || st.Selected || st.Current != Default {
		t.Fatalf("after restore: %+v", st)
	}
	if _, err := os.Stat(s.StateFile); !os.IsNotExist(err) {
		t.Error("fev's note is still there")
	}
}

func TestSelectingKeepsEverythingElseInTheConfiguration(t *testing.T) {
	s := scratch(t)
	escaped := "\\" + "u00e9"
	original := `{
	"auths": {
		"ghcr.io": {},
		"https://index.docker.io/v1/": {"auth": "dXNlcjpwYXNz"}
	},
	"credsStore": "osxkeychain",
	"currentContext": "default",
	"cliPluginsExtraDirs": ["/Users/someone/.other/cli-plugins"],
	"plugins": {"-x-cli-hints": {"enabled": "true"}},
	"unknownToUs": [1, 2.50, null, "caf` + escaped + ` <&>", {"deep": {"er": true}}],
	"zeta": 9007199254740993
}
`
	s.write(t, original)
	os.Chmod(s.configFile(), 0o644)
	if selected, _, err := s.Select(); err != nil || !selected {
		t.Fatal(selected, err)
	}
	got := s.read(t)
	var before, after map[string]json.RawMessage
	json.Unmarshal([]byte(original), &before)
	if err := json.Unmarshal([]byte(got), &after); err != nil {
		t.Fatalf("config.json is no longer JSON: %v\n%s", err, got)
	}
	if string(after["currentContext"]) != `"fev"` || len(after) != len(before) {
		t.Fatalf("config.json is\n%s", got)
	}
	for key, value := range before {
		if key == "currentContext" {
			continue
		}
		var a, b bytes.Buffer
		json.Compact(&a, value)
		json.Compact(&b, after[key])
		if a.String() != b.String() {
			t.Errorf("%s changed from %s to %s", key, a.String(), b.String())
		}
	}
	for _, verbatim := range []string{"9007199254740993", "caf" + escaped + " <&>", "2.50", "\"auths\": {\n\t\t\"ghcr.io\": {},\n\t\t\"https://index.docker.io/v1/\": {\"auth\": \"dXNlcjpwYXNz\"}\n\t},"} {
		if !strings.Contains(got, verbatim) {
			t.Errorf("config.json no longer contains %q:\n%s", verbatim, got)
		}
	}
	var order []string
	for _, f := range mustParse(t, got) {
		order = append(order, f.key)
	}
	if strings.Join(order, " ") != "auths credsStore currentContext cliPluginsExtraDirs plugins unknownToUs zeta" {
		t.Errorf("key order is now %v", order)
	}
	if info, _ := os.Stat(s.configFile()); info.Mode().Perm() != 0o644 {
		t.Errorf("config.json mode became %v", info.Mode())
	}
	if err := s.Restore(); err != nil {
		t.Fatal(err)
	}
	after = nil
	json.Unmarshal([]byte(s.read(t)), &after)
	if _, has := after["currentContext"]; has || len(after) != len(before)-1 {
		t.Fatalf("after restore:\n%s", s.read(t))
	}
}

func mustParse(t *testing.T, doc string) []field {
	t.Helper()
	fields, err := parseObject([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestAnotherRuntimesSelectionIsLeftAlone(t *testing.T) {
	s := scratch(t)
	original := "{\n  \"currentContext\" : \"desktop-linux\",\n  \"credsStore\":\"osxkeychain\"\n}\n"
	s.write(t, original)
	s.other(t, "desktop-linux")
	before, _ := os.Stat(s.configFile())
	selected, left, err := s.Select()
	if err != nil || selected || left != "desktop-linux" {
		t.Fatalf("%v, %q, %v", selected, left, err)
	}
	if got := s.read(t); got != original {
		t.Fatalf("config.json was rewritten:\n%s", got)
	}
	if after, _ := os.Stat(s.configFile()); !after.ModTime().Equal(before.ModTime()) {
		t.Error("config.json was written")
	}
	if st := status(t, s); !st.Exists || st.Selected || st.Current != "desktop-linux" {
		t.Fatalf("%+v", st)
	}
	if err := s.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := s.read(t); got != original {
		t.Fatalf("restore rewrote config.json:\n%s", got)
	}
	if !exists(s.Dir, "desktop-linux") {
		t.Fatal("the other context is gone")
	}
}

func TestAContextThatNoLongerExistsIsReplaced(t *testing.T) {
	s := scratch(t)
	s.write(t, `{"currentContext":"colima"}`)
	if selected, _, err := s.Select(); err != nil || !selected {
		t.Fatal(selected, err)
	}
	if st := status(t, s); !st.Selected {
		t.Fatalf("%+v", st)
	}
	if err := s.Restore(); err != nil {
		t.Fatal(err)
	}
	if st := status(t, s); st.Current != Default {
		t.Fatalf("restored to %q", st.Current)
	}
	s.write(t, `{"currentContext":"colima"}`)
	s.Select()
	s.other(t, "colima")
	if err := s.Restore(); err != nil {
		t.Fatal(err)
	}
	if st := status(t, s); st.Current != "colima" {
		t.Fatalf("restored to %q, want colima", st.Current)
	}
}

func TestRestoreDoesNotUndoALaterChoice(t *testing.T) {
	s := scratch(t)
	s.Select()
	s.other(t, "desktop-linux")
	s.write(t, `{"currentContext":"desktop-linux"}`)
	if err := s.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := s.read(t); got != `{"currentContext":"desktop-linux"}` {
		t.Fatalf("config.json is %q", got)
	}
	if err := s.Remove(); err != nil {
		t.Fatal(err)
	}
	if exists(s.Dir, Name) || !exists(s.Dir, "desktop-linux") {
		t.Fatal("Remove removed the wrong thing")
	}
}

func TestUnselectUndoesOnlyOurOwnSelection(t *testing.T) {
	s := scratch(t)
	if err := s.Ensure(); err != nil {
		t.Fatal(err)
	}
	s.write(t, `{"currentContext":"fev"}`)
	if err := s.Unselect(); err != nil {
		t.Fatal(err)
	}
	if got := s.read(t); got != `{"currentContext":"fev"}` {
		t.Fatalf("the user's own selection was undone: config.json is %q", got)
	}
	s.write(t, `{}`)
	if selected, _, err := s.Select(); err != nil || !selected {
		t.Fatal(selected, err)
	}
	if err := s.Unselect(); err != nil {
		t.Fatal(err)
	}
	if st := status(t, s); st.Current != Default || st.Selected {
		t.Fatalf("after Unselect: %+v", st)
	}
	other := s
	other.Dir = filepath.Join(filepath.Dir(s.Dir), "docker2")
	if _, _, err := s.Select(); err != nil {
		t.Fatal(err)
	}
	if err := other.Ensure(); err != nil {
		t.Fatal(err)
	}
	other.write(t, `{"currentContext":"fev"}`)
	if err := other.Unselect(); err != nil {
		t.Fatal(err)
	}
	if got := other.read(t); got != `{"currentContext":"fev"}` {
		t.Fatalf("config.json is %q", got)
	}
}

func TestAConfigurationThatCannotBeReadIsNotWrittenOver(t *testing.T) {
	for name, doc := range map[string]string{
		"not JSON":              `{"auths": {`,
		"an array":              `["currentContext"]`,
		"two documents":         `{} {}`,
		"a context of a number": `{"currentContext": 7}`,
	} {
		s := scratch(t)
		s.write(t, doc)
		if selected, _, err := s.Select(); err == nil || selected {
			t.Errorf("%s: selected=%v, err=%v", name, selected, err)
		}
		if got := s.read(t); got != doc {
			t.Errorf("%s: config.json was changed to %q", name, got)
		}
		if _, err := s.Status(); err == nil {
			t.Errorf("%s: Status gave no error", name)
		}
	}
	s := scratch(t)
	s.write(t, "\n")
	if selected, _, err := s.Select(); err != nil || !selected {
		t.Fatal(selected, err)
	}
}

func TestALinkedConfigurationIsWrittenThrough(t *testing.T) {
	s := scratch(t)
	dotfiles := t.TempDir()
	real := filepath.Join(dotfiles, "docker-config.json")
	os.WriteFile(real, []byte(`{"credsStore":"osxkeychain"}`), 0o600)
	os.MkdirAll(s.Dir, 0o700)
	if err := os.Symlink(real, s.configFile()); err != nil {
		t.Fatal(err)
	}
	if selected, _, err := s.Select(); err != nil || !selected {
		t.Fatal(selected, err)
	}
	if info, _ := os.Lstat(s.configFile()); info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced by a file")
	}
	data, _ := os.ReadFile(real)
	if !strings.Contains(string(data), `"currentContext": "fev"`) || !strings.Contains(string(data), "osxkeychain") {
		t.Fatalf("the linked file is %s", data)
	}
}

func TestDirFollowsDOCKERCONFIG(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", "/tmp/some/where")
	if dir, err := Dir(); err != nil || dir != "/tmp/some/where" {
		t.Fatal(dir, err)
	}
	t.Setenv("DOCKER_CONFIG", "")
	t.Setenv("HOME", "/Users/nobody")
	if dir, err := Dir(); err != nil || dir != "/Users/nobody/.docker" {
		t.Fatal(dir, err)
	}
}

func dockerCLI(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("FEV_DOCKER_CLI"); path != "" {
		return path
	}
	found, _ := filepath.Glob("../../../tests/fixtures/out/docker-cli/*/docker")
	if len(found) == 0 {
		t.Skip("the pinned docker CLI has not been fetched: run tests/fixtures/docker-cli.sh")
	}
	path, _ := filepath.Abs(found[len(found)-1])
	return path
}

func TestTheDockerCLIUsesTheContext(t *testing.T) {
	cli := dockerCLI(t)
	dir, err := os.MkdirTemp("/tmp", "bc.")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	s := Store{Dir: filepath.Join(dir, "docker"), Socket: filepath.Join(dir, "docker.sock"), StateFile: filepath.Join(dir, "state.json")}
	engine := fakeguest.NewEngine()
	defer engine.Close()
	l, err := net.Listen("unix", s.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go engine.Serve(l)
	engine.Run(fakeguest.Spec{Name: "seen-through-the-context"})

	docker := func(args ...string) (string, error) {
		cmd := exec.Command(cli, args...)
		cmd.Env = []string{"DOCKER_CONFIG=" + s.Dir, "HOME=" + dir, "PATH=/usr/bin:/bin"}
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := docker("context", "ls", "--format", "{{.Name}} {{.Current}}"); err != nil || out != "default true" {
		t.Fatalf("before: %q, %v", out, err)
	}
	if selected, _, err := s.Select(); err != nil || !selected {
		t.Fatal(selected, err)
	}
	out, err := docker("context", "ls", "--format", "{{.Name}} {{.Current}} {{.DockerEndpoint}}")
	if err != nil || !strings.Contains(out, "fev true unix://"+s.Socket) || !strings.Contains(out, "default false") {
		t.Fatalf("context ls: %q, %v", out, err)
	}
	if out, err := docker("context", "inspect", "fev", "--format", "{{.Name}} {{.Endpoints.docker.Host}}"); err != nil || out != "fev unix://"+s.Socket {
		t.Fatalf("context inspect: %q, %v", out, err)
	}
	if out, err := docker("ps", "--format", "{{.Names}}"); err != nil || out != "seen-through-the-context" {
		t.Fatalf("docker ps: %q, %v", out, err)
	}
	if out, err := docker("info", "--format", "{{.Name}}"); err != nil || out != "fev-fake" {
		t.Fatalf("docker info: %q, %v", out, err)
	}
	if out, err := docker("context", "use", "default"); err != nil {
		t.Fatalf("context use default: %q, %v", out, err)
	}
	if st := status(t, s); st.Selected || st.Current != Default || !st.Exists {
		t.Fatalf("after the CLI switched away: %+v", st)
	}
	if selected, _, err := s.Select(); err != nil || !selected {
		t.Fatal(selected, err)
	}
	if out, err := docker("context", "show"); err != nil || out != "fev" {
		t.Fatalf("context show: %q, %v", out, err)
	}
	if err := s.Restore(); err != nil {
		t.Fatal(err)
	}
	if out, err := docker("context", "show"); err != nil || out != "default" {
		t.Fatalf("context show after restore: %q, %v", out, err)
	}
}

func TestUseReplacesAnotherSelectionAndRemembersIt(t *testing.T) {
	s := scratch(t)
	s.write(t, "{\n\t\"currentContext\": \"other\",\n\t\"credsStore\": \"osxkeychain\"\n}")
	s.other(t, "other")
	if err := s.Use(); err != nil {
		t.Fatal(err)
	}
	if got := s.read(t); got != "{\n\t\"currentContext\": \"fev\",\n\t\"credsStore\": \"osxkeychain\"\n}" {
		t.Fatalf("config.json:\n%s", got)
	}
	if st := status(t, s); !st.Exists || !st.Selected {
		t.Fatalf("%+v", st)
	}
	before, _ := os.Stat(s.configFile())
	if err := s.Use(); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(s.configFile()); !after.ModTime().Equal(before.ModTime()) {
		t.Error("a second Use wrote config.json")
	}
	if err := s.Restore(); err != nil {
		t.Fatal(err)
	}
	if st := status(t, s); st.Current != "other" {
		t.Fatalf("restore after use: %+v", st)
	}
}
