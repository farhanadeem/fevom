package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	api "fev/proto/daemon"
)

func (a *app) dockerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docker",
		Short: "Docker commands of fev's own",
		Long: `Docker commands of fev's own, beside the docker CLI's. The docker command
itself is fev's entry point under that name, not this.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	volume := &cobra.Command{
		Use:   "volume",
		Short: "Copy Docker volumes: clone, export, import",
		Args:  args(cobra.NoArgs),
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	volume.AddCommand(a.volumeCloneCmd(), a.volumeExportCmd(), a.volumeImportCmd())
	cmd.AddCommand(volume)
	return cmd
}

func volumeText(w io.Writer, v *api.Volume) {
	kv(w, "volume", v.Name)
	keys := make([]string, 0, len(v.Labels))
	for k := range v.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		kv(w, "label", k+"="+v.Labels[k])
	}
	kv(w, "entries", v.Files)
}

func (a *app) volumeCloneCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "clone [flags] VOLUME [NEW]",
		Short: "Copy a Docker volume",
		Long: `Copy a Docker volume, in use or not, as a new volume NEW (or -n's) with its
labels. The copy shares the files' data with VOLUME until either changes
it, so it takes no room of its own at first and is quick at any size. A
volume of another driver, or one made with options (a bind, tmpfs or NFS
mount), is not copied.`,
		Args: args(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			newName := name
			if len(argv) == 2 {
				if name != "" && name != argv[1] {
					return usagef("clone: the new name is given twice: %q and -n %q", argv[1], name)
				}
				newName = argv[1]
			}
			if newName == "" {
				return usagef("clone: name the new volume: NEW or -n NEW")
			}
			return a.withDaemon(cmd.Context(), false, func(c *api.Client) error {
				v, err := c.CloneVolume(cmd.Context(), argv[0], newName)
				if err != nil {
					return err
				}
				return a.emit(v, func(w io.Writer) { volumeText(w, v) })
			})
		},
	}
	cmd.Flags().StringVarP(&name, "name", "n", "", "the new volume's name")
	return cmd
}

func (a *app) volumeExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export VOLUME FILE",
		Short: "Write a Docker volume to an archive",
		Long: `Write a Docker volume to FILE on the Mac: a zstd tar of its files and labels,
which "fev docker volume import" makes a volume of. The files are read as
they are while the export runs: stop what writes to the volume first for a
copy of one moment. An existing FILE is not replaced.`,
		Args: args(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			path, err := filepath.Abs(argv[1])
			if err != nil {
				return usagef("export: %v", err)
			}
			ctx := cmd.Context()
			return a.withDaemon(ctx, false, func(c *api.Client) error {
				what := "export " + argv[0]
				end, err := a.follow(ctx, c, what, func() (*api.Operation, error) {
					return c.ExportVolume(ctx, api.VolumesExportParams{Name: argv[0], Path: path})
				})
				if err != nil {
					return err
				}
				if err := ended(what, end); err != nil {
					return err
				}
				var r api.VolumeExported
				json.Unmarshal(end.Result, &r)
				return a.emit(end, func(w io.Writer) {
					kv(w, "archive", r.Path)
					kv(w, "size", bytesText(r.Bytes))
				})
			})
		},
	}
}

func (a *app) volumeImportCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "import [flags] FILE",
		Short: "Make a Docker volume from an archive",
		Long: `Make a Docker volume from FILE, an archive "fev docker volume export" wrote,
under the name it was exported from or -n's, with its labels. A volume of
that name must not exist.`,
		Args: args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			path, err := filepath.Abs(argv[0])
			if err != nil {
				return usagef("import: %v", err)
			}
			if _, err := os.Stat(path); err != nil {
				return failf(ExitFailed, "import: %v", err)
			}
			ctx := cmd.Context()
			return a.withDaemon(ctx, false, func(c *api.Client) error {
				end, err := a.follow(ctx, c, "import", func() (*api.Operation, error) {
					return c.ImportVolume(ctx, api.VolumesImportParams{Path: path, Name: name})
				})
				if err != nil {
					return err
				}
				if err := ended("import", end); err != nil {
					return err
				}
				var v api.Volume
				json.Unmarshal(end.Result, &v)
				return a.emit(end, func(w io.Writer) { volumeText(w, &v) })
			})
		},
	}
	cmd.Flags().StringVarP(&name, "name", "n", "", "the new volume's name (default: the one it was exported from)")
	return cmd
}
