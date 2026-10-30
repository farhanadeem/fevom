package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	api "fev/proto/daemon"
)

func (a *app) configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show and change fev's settings",
		Long: `Show and change fev's settings. Each key has a type, a default and a
moment when a new value takes effect: at once, when the Docker engine next
starts, or when the VM next starts ("fev restart").

A refused key or value exits 5 and changes nothing.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(a.configShowCmd(), a.configGetCmd(), a.configSetCmd(), a.configResetCmd(),
		a.configListEdit("add"), a.configListEdit("remove"), a.configDockerCmd())
	return cmd
}

func valueText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(data)
}

func applyText(apply string) string {
	switch apply {
	case api.ApplyVMRestart:
		return `waits for "fev restart"`
	case api.ApplyEngineRestart:
		return `waits for "fev restart docker"`
	case api.ApplyMachineRestart:
		return "waits for the machine's next start"
	}
	return "waits for a restart"
}

func valuesText(w io.Writer, values map[string]api.ConfigValue) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		v := values[key]
		text := valueText(v.Value)
		switch {
		case v.Pending:
			text += ` (waits for a restart)`
		case v.IsDefault:
			text += " (default)"
		}
		kv(w, key, text)
	}
}

func changesText(w io.Writer, changed []api.ConfigChange) {
	for _, ch := range changed {
		text := valueText(ch.Value)
		if ch.Pending {
			text += " (" + applyText(ch.Apply) + ")"
		}
		kv(w, ch.Key, text)
	}
}

func (a *app) configShowCmd() *cobra.Command {
	var describe bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print every setting and its value",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withDaemon(cmd.Context(), false, func(c *api.Client) error {
				if describe {
					keys, err := c.ConfigDescribe(cmd.Context())
					if err != nil {
						return err
					}
					return a.emit(api.ConfigKeys{Keys: keys}, func(w io.Writer) { describeText(w, keys) })
				}
				values, err := c.ConfigList(cmd.Context())
				if err != nil {
					return err
				}
				return a.emit(api.ConfigValues{Values: values}, func(w io.Writer) { valuesText(w, values) })
			})
		},
	}
	cmd.Flags().BoolVar(&describe, "describe", false, "print each key's type, default, bounds and what it does instead")
	return cmd
}

func describeText(w io.Writer, keys []api.ConfigKey) {
	rows := make([][]string, 0, len(keys))
	for _, k := range keys {
		bounds := ""
		if k.Min != nil && k.Max != nil {
			bounds = fmt.Sprintf("%d to %d", *k.Min, *k.Max)
		}
		apply := "at once"
		if k.Apply != api.ApplyLive {
			apply = strings.TrimPrefix(applyText(k.Apply), "waits for ")
		}
		rows = append(rows, []string{k.Key, k.Type, valueText(k.Default), bounds, apply, k.Description})
	}
	table(w, []string{"KEY", "TYPE", "DEFAULT", "BOUNDS", "APPLIES", "WHAT"}, rows)
}

func (a *app) configGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get KEY...",
		Short: "Print the value of one or more settings",
		Long: `Print the value of one or more settings. With -q and one key, the bare
value is printed and nothing else.`,
		Args: args(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, keys []string) error {
			return a.withDaemon(cmd.Context(), false, func(c *api.Client) error {
				values, err := c.ConfigGet(cmd.Context(), keys...)
				if err != nil {
					return err
				}
				if a.quiet {
					for _, key := range keys {
						fmt.Fprintln(a.Stdout, valueText(values[key].Value))
					}
					return nil
				}
				return a.emit(api.ConfigValues{Values: values}, func(w io.Writer) { valuesText(w, values) })
			})
		},
	}
}

func typed(text, kind string) any {
	switch kind {
	case api.TypeInt:
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			return n
		}
	case api.TypeBool:
		if b, err := strconv.ParseBool(text); err == nil && (text == "true" || text == "false") {
			return b
		}
	case api.TypeList:
		var list []any
		if strings.HasPrefix(strings.TrimSpace(text), "[") && json.Unmarshal([]byte(text), &list) == nil {
			return list
		}
	case api.TypeObject:
		var obj map[string]any
		if json.Unmarshal([]byte(text), &obj) == nil && obj != nil {
			return obj
		}
	}
	return text
}

func (a *app) configSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set KEY VALUE | KEY=VALUE...",
		Short: "Change one or more settings",
		Long: `Change one or more settings: "fev config set cpu 4", or several at once as
"fev config set cpu=4 memory_mib=8192". Every value is checked before any
is stored: either all are set or none is. What is printed are the keys whose
values changed, and what each waits for if it is not in effect at once.`,
		Args: args(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			pairs := map[string]string{}
			if len(argv) == 2 && !strings.Contains(argv[0], "=") {
				pairs[argv[0]] = argv[1]
			} else {
				for _, arg := range argv {
					key, value, ok := strings.Cut(arg, "=")
					if !ok || key == "" {
						return usagef("config set: %q is not KEY=VALUE", arg)
					}
					pairs[key] = value
				}
			}
			return a.withDaemon(cmd.Context(), false, func(c *api.Client) error {
				keys, err := c.ConfigDescribe(cmd.Context())
				if err != nil {
					return err
				}
				kinds := map[string]string{}
				for _, k := range keys {
					kinds[k.Key] = k.Type
				}
				values := map[string]any{}
				for key, text := range pairs {
					values[key] = typed(text, kinds[key])
				}
				changed, err := c.ConfigSet(cmd.Context(), values)
				if err != nil {
					return err
				}
				return a.emit(api.ConfigChanges{Changed: changed}, func(w io.Writer) { changesText(w, changed) })
			})
		},
	}
}

func (a *app) configResetCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "reset KEY... | --all",
		Short: "Put settings back to their defaults",
		Args: func(cmd *cobra.Command, keys []string) error {
			if (len(keys) == 0) != all {
				return usagef("config reset: name the keys, or give --all for every key")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, keys []string) error {
			return a.withDaemon(cmd.Context(), false, func(c *api.Client) error {
				changed, err := c.ConfigReset(cmd.Context(), keys...)
				if err != nil {
					return err
				}
				return a.emit(api.ConfigChanges{Changed: changed}, func(w io.Writer) { changesText(w, changed) })
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "every key")
	return cmd
}

func (a *app) configListEdit(verb string) *cobra.Command {
	long := `Add values to a setting whose value is a list, such as
network.proxy_exclude: "fev config add network.proxy_exclude .corp.example
10.0.0.0/8". Each value is checked as "config set" checks the list's, and is
added at the end unless the list has it already.`
	if verb == "remove" {
		long = `Remove values from a setting whose value is a list, wherever they are in
it. A value the list does not have is no error.`
	}
	return &cobra.Command{
		Use:   verb + " KEY VALUE...",
		Short: strings.ToUpper(verb[:1]) + verb[1:] + " values of a setting that is a list",
		Long: long + `

What is printed is the setting's new value, if it changed. A key that is
not a list, or a refused value, exits 5 and changes nothing.`,
		Args: args(cobra.MinimumNArgs(2)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			return a.withDaemon(cmd.Context(), false, func(c *api.Client) error {
				edit := c.ConfigAdd
				if verb == "remove" {
					edit = c.ConfigRemove
				}
				changed, err := edit(cmd.Context(), argv[0], argv[1:]...)
				if err != nil {
					return err
				}
				return a.emit(api.ConfigChanges{Changed: changed}, func(w io.Writer) { changesText(w, changed) })
			})
		},
	}
}

func (a *app) configDockerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "docker",
		Short: "Edit the Docker engine's configuration and restart the engine with it",
		Long: `Open the Docker engine's configuration file (a daemon.json) in $VISUAL or
$EDITOR. A file that is not a JSON object is said to be so at once and is
not given to the engine: it is kept as it was saved, the engine keeps the
last configuration that passed, and the exit status is 5.

If the file was changed and the VM is running, the engine is restarted with
it and the verdict is printed. A file the engine refuses is not used: the
last configuration that passed stays in force, and the exit status is 5.
If the VM is not running, nothing is started: the engine is given the file
when it next starts, and "fev status" says so if it refuses it then.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withDaemon(cmd.Context(), false, func(c *api.Client) error {
				ctx := cmd.Context()
				ds, err := c.DockerStatus(ctx)
				if err != nil {
					return err
				}
				if ds.ConfigFile == "" {
					return failf(ExitFailed, "this daemon has no engine configuration file")
				}
				before, _ := os.ReadFile(ds.ConfigFile)
				if err := a.edit(ds.ConfigFile); err != nil {
					return err
				}
				after, _ := os.ReadFile(ds.ConfigFile)
				if bytes.Equal(before, after) {
					if problem := engineConfigProblem(after); problem != "" {
						return &exit{code: ExitConfig, err: fmt.Errorf("%s is unchanged, and %s. The engine was not restarted and is not given this file; it keeps the last configuration that passed. \"fev config docker\" opens it again", ds.ConfigFile, problem)}
					}
					fmt.Fprintln(a.Stderr, "unchanged; the engine was not restarted")
					return a.emit(ds, func(w io.Writer) { dockerText(w, ds) })
				}
				st, err := c.Status(ctx)
				if err != nil {
					return err
				}
				up := st.State == api.StateRunning || st.State == api.StatePaused
				when := "when the VM next starts"
				if st.State == api.StateStarting {
					when = "when its engine is ready"
				}
				if problem := engineConfigProblem(after); problem != "" {
					what := "the engine was not restarted and keeps the configuration it runs on"
					if !up {
						what = fmt.Sprintf("the VM is %s, and %s the engine is not given this file: it keeps the last configuration that passed, and \"fev status\" warns until the file is mended", st.State, when)
					}
					return &exit{code: ExitConfig, err: fmt.Errorf("%s: %s. It is saved as you left it; %s. \"fev config docker\" opens it again", ds.ConfigFile, problem, what)}
				}
				if !up {
					fmt.Fprintf(a.Stderr, "saved; it is valid JSON. The VM is %s and nothing was started: the engine is given the file %s, and if it refuses it then, it keeps the last configuration that passed and \"fev status\" says why\n", st.State, when)
					return nil
				}
				if ds, err = c.DockerRestart(ctx); err != nil {
					return err
				}
				if ds.ConfigState == api.EngineConfigRefused {
					return &exit{code: ExitConfig, err: fmt.Errorf("the engine refused %s: %s; the last configuration that passed is still in force", ds.ConfigFile, ds.ConfigError)}
				}
				return a.emit(ds, func(w io.Writer) { dockerText(w, ds) })
			})
		},
	}
}

const maxEngineConfig = 1 << 20

func engineConfigProblem(data []byte) string {
	if len(data) > maxEngineConfig {
		return fmt.Sprintf("it is %d bytes; an engine configuration is at most %d", len(data), maxEngineConfig)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return ""
	}
	var object map[string]json.RawMessage
	err := json.Unmarshal(data, &object)
	var syntax *json.SyntaxError
	switch {
	case errors.As(err, &syntax):
		line, column := position(data, syntax.Offset)
		return fmt.Sprintf("it is not valid JSON: %v (line %d, column %d)", err, line, column)
	case err != nil:
		return "it must hold a JSON object: " + err.Error()
	case object == nil:
		return "it must hold a JSON object, not null"
	}
	return ""
}

func position(data []byte, offset int64) (line, column int) {
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	if offset > 0 {
		offset--
	}
	before := data[:offset]
	line = 1 + bytes.Count(before, []byte("\n"))
	return line, int(offset) - (bytes.LastIndexByte(before, '\n') + 1) + 1
}

func (a *app) edit(path string) error {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	c := exec.Command("/bin/sh", "-c", editor+` "$1"`, "editor", path)
	c.Stdin, c.Stdout, c.Stderr = a.Stdin, a.Stdout, a.Stderr
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return failf(ExitFailed, "the editor (%s) exited with status %d; nothing was applied", editor, ee.ExitCode())
		}
		return failf(ExitFailed, "cannot run the editor (%s): %v", editor, err)
	}
	return nil
}
