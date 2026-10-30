package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"fev/host/daemon/config"
	"fev/host/daemon/home"
	"fev/host/storage"
	api "fev/proto/daemon"
)

const moveMargin = 5 << 30

type DataInfo struct {
	Dir            string `json:"dir"`
	Image          string `json:"image"`
	Placed         bool   `json:"placed"`
	Default        string `json:"default"`
	Exists         bool   `json:"exists"`
	AllocatedBytes int64  `json:"allocated_bytes"`
	Volume         string `json:"volume,omitempty"`
}

type DataMoveResult struct {
	From           string `json:"from"`
	To             string `json:"to"`
	Renamed        bool   `json:"renamed"`
	CopiedBytes    int64  `json:"copied_bytes"`
	AllocatedBytes int64  `json:"allocated_bytes"`
	Left           bool   `json:"left,omitempty"`
}

func (a *app) dataCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "data",
		Short: "Where fev's data image is, and moving it",
		Args:  args(cobra.NoArgs),
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(a.dataShowCmd(), a.dataMoveCmd())
	return cmd
}

func (a *app) dataShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Say where the data image is and what it occupies",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := a.layout()
			if err != nil {
				return err
			}
			info := dataInfo(l)
			return a.emit(info, func(w io.Writer) {
				where := info.Dir
				if info.Placed {
					where += " (moved there; the default is " + info.Default + ")"
				}
				kv(w, "data directory", where)
				if !info.Exists {
					kv(w, "image", "none yet: the next start makes it")
					return
				}
				kv(w, "image", info.Image)
				kv(w, "occupies", bytesText(info.AllocatedBytes)+" on an "+info.Volume+" volume")
			})
		},
	}
}

func dataInfo(l home.Layout) DataInfo {
	info := DataInfo{Dir: l.DataDir, Image: l.DataImage(), Placed: l.Placed, Default: l.DefaultDataDir}
	if n, err := storage.Allocated(info.Image); err == nil {
		info.Exists, info.AllocatedBytes = true, n
	}
	var fs unix.Statfs_t
	if unix.Statfs(l.DataDir, &fs) == nil {
		info.Volume = unix.ByteSliceToString(fs.Fstypename[:])
	}
	return info
}

func (a *app) dataMoveCmd() *cobra.Command {
	var leave bool
	var marginMiB int64 = moveMargin >> 20
	cmd := &cobra.Command{
		Use:   "move DIR",
		Short: "Move the data image to another directory, an external disk say",
		Long: `Move fev's data image, which holds every container, image, volume and
machine, to the directory DIR, and keep it there from then on. DIR is made
if it is not there. Moving it to the default directory (fev data show
names it) puts it back.

fev must be stopped ("fev stop"); nothing can start it while the move
runs. On the same volume the image is renamed. On another volume it is
copied, holding only what the image holds (its unused parts stay
unallocated), flushed to the disk, and the old image removed. The volume
must keep a file's unused parts unallocated, as APFS does: an exFAT or
HFS+ disk is refused (Disk Utility formats an external disk as APFS). It
must have room for the image and 5 GiB beside it.

The image keeps the data_allow_backup setting in its new place. A disk
that holds the image must be connected for fev to start; while it is
not, fev refuses to start and says so, and never makes a new, empty
image in its place.

With --leave, nothing is moved: the old image is left where it is (for a
disk that is gone), and the next start makes a new, empty one in DIR.`,
		Args: args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			dir, err := filepath.Abs(argv[0])
			if err != nil {
				return usagef("data move: %v", err)
			}
			l, err := a.layout()
			if err != nil {
				return err
			}
			if err := l.Create(); err != nil {
				return err
			}
			startLock, _, err := home.Lock(l.StartLock(), true)
			if err != nil {
				return err
			}
			defer startLock.Close()
			daemonLock, held, err := home.Lock(l.DaemonLock(), false)
			if err != nil {
				return err
			}
			if held {
				return failf(ExitFailed, "fev is running: stop it first (fev stop), then move its data, then fev start")
			}
			defer daemonLock.Close()
			if insideHome(l, dir) {
				return usagef("data move: %s is inside fev's home (%s), whose directories fev empties or removes (run/ at a start, completions/ at setup --undo): choose a directory outside it, or the default, %s", dir, l.Home, l.DefaultDataDir)
			}
			src := l.DataImage()
			res := DataMoveResult{From: l.DataDir, To: filepath.Clean(dir)}
			note := func() error {
				if err := l.WritePlacement(res.To); err != nil {
					return fmt.Errorf("the new place could not be noted: %w", err)
				}
				stamp := l.DataImageStamp()
				if b, err := os.ReadFile(stamp); err == nil && filepath.Clean(string(trimNL(b))) == src {
					os.WriteFile(stamp, []byte(filepath.Join(res.To, storage.ImageName)+"\n"), 0o600)
				}
				return nil
			}
			if leave {
				if err := os.MkdirAll(res.To, 0o700); err != nil {
					return err
				}
				if err := storage.KeepsHoles(res.To); err != nil {
					return failf(ExitFailed, "%v", err)
				}
				if err := l.WritePlacement(res.To); err != nil {
					return err
				}
				os.Remove(l.DataImageStamp())
				res.Left = true
				return a.emit(res, func(w io.Writer) {
					kv(w, "data directory", res.To)
					kv(w, "left", res.From+" (not moved; the next start makes a new, empty image)")
				})
			}
			if l.Placed {
				if _, err := os.Stat(l.DataDir); errors.Is(err, fs.ErrNotExist) {
					return failf(ExitFailed, "the data directory %s is not there: connect its disk and move again, or --leave to start with a new, empty image in %s", l.DataDir, res.To)
				}
			}
			allow := false
			if st, err := config.Load(l.ConfigFile(), config.Keys(config.ThisMac()), nil); err == nil {
				allow = st.Bool(api.KeyDataAllowBackup)
			}
			total := int64(0)
			if n, err := storage.Allocated(src); err == nil {
				total = n
			}
			if !a.quiet && !a.json() && total > 1<<30 {
				fmt.Fprintf(a.Stderr, "fev: moving %s from %s to %s\n", bytesText(total), l.DataDir, res.To)
			}
			last := time.Now()
			r, err := storage.Move(cmd.Context(), src, res.To, storage.MoveOptions{
				AllowBackup: allow,
				Margin:      uint64(marginMiB) << 20,
				Committed:   note,
				Progress: func(done, all int64) {
					if a.quiet || a.json() || time.Since(last) < 5*time.Second {
						return
					}
					last = time.Now()
					fmt.Fprintf(a.Stderr, "fev: %s of %s\n", bytesText(done), bytesText(all))
				},
			})
			if err != nil {
				return failf(ExitFailed, "not moved: %v", err)
			}
			res.Renamed, res.CopiedBytes, res.AllocatedBytes = r.Renamed, r.Copied, r.Allocated
			return a.emit(res, func(w io.Writer) {
				kv(w, "data directory", res.To)
				how := "copied " + bytesText(res.CopiedBytes)
				if res.Renamed {
					how = "renamed on its volume"
				}
				kv(w, "image", how+"; it occupies "+bytesText(res.AllocatedBytes))
			})
		},
	}
	cmd.Flags().BoolVar(&leave, "leave", false, "move nothing: leave the old image where it is and start with a new, empty one in DIR")
	cmd.Flags().Int64Var(&marginMiB, "margin-mib", marginMiB, "MiB left free beside the image on the new volume")
	cmd.Flags().MarkHidden("margin-mib")
	return cmd
}

func insideHome(l home.Layout, dir string) bool {
	real := func(p string) string {
		p = filepath.Clean(p)
		rest := ""
		for {
			if r, err := filepath.EvalSymlinks(p); err == nil {
				return filepath.Join(r, rest)
			}
			parent := filepath.Dir(p)
			if parent == p {
				return filepath.Join(p, rest)
			}
			rest = filepath.Join(filepath.Base(p), rest)
			p = parent
		}
	}
	d, h := real(dir), real(l.Home)
	if d == real(l.DefaultDataDir) {
		return false
	}
	return d == h || strings.HasPrefix(d, h+string(filepath.Separator))
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}
