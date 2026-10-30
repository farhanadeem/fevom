package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"fev/host/daemon/doctor"
	"fev/host/daemon/report"
)

func (a *app) reportCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Write a diagnostic report for a bug report",
		Long: `Write fev's diagnostic report: one zip file, in the current directory
unless -o names another place. It holds fev's build, macOS and the
Mac's model, "fev doctor"'s findings, what the running daemon says of
itself, fev's directory listed (names and sizes, not contents), the
configuration with credentials in URLs redacted, and the logs.

It is written on this Mac and sent nowhere: read it before you attach it.
The logs name containers, machines, images and Mac paths. Never in it: the
data disk, fev's CA and keys, its SSH keys, and your environment.

It starts nothing; with fev not running, it has no daemon/ part.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := a.layout()
			if err != nil {
				return err
			}
			now := time.Now()
			path := output
			if path == "" {
				path = report.DefaultName(now)
			}
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				path = filepath.Join(path, report.DefaultName(now))
			}
			if path, err = filepath.Abs(path); err != nil {
				return usagef("report: %v", err)
			}
			e, c := a.doctorEnv(cmd.Context(), l)
			if c != nil {
				defer c.Close()
			}
			var d doctor.Report = doctor.Check(e)
			w, err := report.Write(cmd.Context(), path, report.Options{Layout: l, Client: c, Doctor: d, Now: now})
			if err != nil {
				return failf(ExitFailed, "report: %v", err)
			}
			if a.quiet {
				fmt.Fprintln(a.Stdout, w.Path)
				return nil
			}
			return a.emit(w, func(out io.Writer) {
				fmt.Fprintf(out, "Wrote %s (%s, %d files).\n", w.Path, bytesText(w.Bytes), len(w.Files))
				for _, s := range w.Skipped {
					fmt.Fprintf(out, "  not included: %s\n", s)
				}
				fmt.Fprintln(out, "It was sent nowhere. Read it before you attach it to a bug report: its logs name containers, machines, images and Mac paths.")
			})
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "the file to write, or a directory to write it in (default: the current directory)")
	return cmd
}
