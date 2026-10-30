package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	api "fev/proto/daemon"
)

func (a *app) memoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Give the VM's idle memory back to the Mac",
		Args:  args(cobra.NoArgs),
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	var floor int
	reclaim := &cobra.Command{
		Use:   "reclaim",
		Short: "Give the VM's idle file cache back to the Mac now",
		Long: `Ask the VM to drop the file cache that no running container holds, down
to a floor, and to compact its memory, so that the Mac gets that memory
back. fev does this by itself two seconds after work on the engine has
ended (a container that exits, an image removed, a long docker command
that ends); this asks for it now.

Running containers keep their cache. Nothing happens when the cache is
already near the floor, or when the VM is short of memory, and the reason
is printed. The memory reaches the Mac within seconds afterwards; this
command does not wait for it. It needs fev running and starts nothing.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if floor < 0 {
				return usagef("--floor is in MiB and must not be negative")
			}
			c, err := a.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer c.Close()
			r, err := c.MemoryReclaim(cmd.Context(), api.ReclaimParams{FloorMiB: floor})
			if err != nil {
				return err
			}
			return a.emit(r, func(w io.Writer) { reclaimText(w, r) })
		},
	}
	reclaim.Flags().IntVar(&floor, "floor", 0, "leave this much cache, in MiB (default the VM's own, 128)")
	cmd.AddCommand(reclaim)
	return cmd
}

func reclaimText(w io.Writer, r *api.Reclaimed) {
	switch r.Skipped {
	case api.ReclaimSkippedFloor:
		fmt.Fprintf(w, "Nothing to give back: the idle file cache (%s) is at the floor of %d MiB, or close to it.\n",
			bytesText(int64(r.FileBeforeBytes)), r.FloorMiB)
		return
	case api.ReclaimSkippedPressure:
		fmt.Fprintln(w, "Nothing given back: the VM is short of memory now, and dropping its cache would slow what runs. Try again when it is quieter.")
		return
	case "":
	default:
		fmt.Fprintf(w, "Nothing given back (%s).\n", r.Skipped)
		return
	}
	kv(w, "reclaimed", bytesText(int64(r.ReclaimedBytes)))
	kv(w, "idle cache", fmt.Sprintf("%s, was %s", bytesText(int64(r.FileAfterBytes)), bytesText(int64(r.FileBeforeBytes))))
	kv(w, "floor", fmt.Sprintf("%d MiB", r.FloorMiB))
	kv(w, "took", fmt.Sprintf("%.0f ms", r.DurationMS))
	fmt.Fprintln(w, "The Mac gets the memory back within seconds.")
}
