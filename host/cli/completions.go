package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

var completionPrograms = []string{"fev", "docker", "docker-compose"}

func completionsDir(home string) string { return filepath.Join(home, "completions") }

func completionFile(home, shell, program string) string {
	dir := filepath.Join(completionsDir(home), shell)
	switch shell {
	case "zsh":
		return filepath.Join(dir, "_"+program)
	case "fish":
		return filepath.Join(dir, program+".fish")
	}
	return filepath.Join(dir, program)
}

func (a *app) completionScript(shell, program string) ([]byte, error) {
	cmd := &cobra.Command{Use: program}
	if program == "fev" {
		cmd = (&app{Env: a.Env}).root()
	}
	var b bytes.Buffer
	var err error
	switch shell {
	case "bash":
		err = cmd.GenBashCompletionV2(&b, true)
	case "zsh":
		err = cmd.GenZshCompletion(&b)
	case "fish":
		err = cmd.GenFishCompletion(&b, true)
	default:
		err = fmt.Errorf("no completions for %q", shell)
	}
	return b.Bytes(), err
}

func (a *app) writeCompletions(home string) (changed bool, err error) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		for _, program := range completionPrograms {
			data, err := a.completionScript(shell, program)
			if err != nil {
				return changed, err
			}
			path := completionFile(home, shell, program)
			if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, data) {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return changed, err
			}
			if err := replaceFile(path, data, 0o644); err != nil {
				return changed, err
			}
			changed = true
		}
	}
	return changed, nil
}

func completionLines(shell, home, userHome string) string {
	dir := shellPath(completionsDir(home), userHome)
	switch shell {
	case "zsh":
		return fmt.Sprintf("fpath=(\"%s/zsh\" $fpath)\n", dir)
	case "bash":
		return fmt.Sprintf("case $- in *i*) for f in \"%s/bash/\"*; do [ -r \"$f\" ] && . \"$f\"; done ;; esac\n", dir)
	case "fish":
		return fmt.Sprintf("set -p fish_complete_path \"%s/fish\"\n", dir)
	}
	return ""
}
