package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	api "fev/proto/daemon"
)

const resetWord = "reset"

func (a *app) resetCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Delete all containers, images, volumes and machines",
		Long: `Delete all Linux data: every container, image, volume and machine. Settings
are kept. The VM is stopped first, and started again afterwards if it was
running.

It asks for the word "reset" to be typed; --yes is for scripts.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				fmt.Fprintf(a.Stderr, "This deletes every container, image, volume and machine. Settings are kept.\nType %q to go on: ", resetWord)
				line, _ := bufio.NewReader(a.Stdin).ReadString('\n')
				if strings.TrimSpace(line) != resetWord {
					return failf(ExitFailed, "not reset: %q was not typed", resetWord)
				}
			}
			return a.withDaemon(cmd.Context(), false, func(c *api.Client) error {
				r, err := c.DataReset(cmd.Context())
				if err != nil {
					return err
				}
				if r.Status.State == api.StateStarting {
					st, err := c.VMStart(cmd.Context(), api.StartParams{})
					if err != nil {
						return err
					}
					r.Status = *st
				}
				return a.emit(r, func(w io.Writer) {
					kv(w, "freed", bytesText(r.FreedBytes))
					kv(w, "state", stateText(&r.Status))
				})
			})
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	return cmd
}
