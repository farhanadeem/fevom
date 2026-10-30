package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	api "fev/proto/daemon"
)

const salvageCaveat = "Files were read from an older commit or with the filesystem's checks turned off: " +
	"a file rewritten in place since that commit has the commit's size with newer bytes inside it, " +
	"so check each database with its own tool (pg_amcheck, sqlite3's integrity_check, mysqlcheck) before relying on it."

func (a *app) salvageCmd() *cobra.Command {
	var volumes, machines []string
	var docker bool
	cmd := &cobra.Command{
		Use:   "salvage DIR",
		Short: "Copy volumes and machines off a damaged data disk to a folder",
		Long: `Copy what can be read off fev's data disk to the folder DIR on the Mac:
each Docker volume's data to DIR/volumes/NAME and each machine's files to
DIR/machines/NAME, or only those named. It is for a disk that will not
mount read-write (fev status warns of one); it reads the disk and
changes nothing on it, so it can be run again, and the disk then reset.

The disk is mounted read-only when it can be, and read by btrfs's own
restore tool when it cannot. Either way the files come from an older
commit or with the filesystem's checks off: check a database with its own
tool before relying on it. Files belong to you on the Mac, not to the
users that owned them in Linux; a copy from a read-only mount keeps their
permission bits and times, and btrfs's restore tool does not.

The VM is started for it if it is stopped; the Docker engine is not.`,
		Args: args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			dir, err := filepath.Abs(argv[0])
			if err != nil {
				return usagef("salvage: %v", err)
			}
			p := api.SalvageParams{Dest: dir, Volumes: volumes, Machines: machines, Docker: docker}
			return a.withDaemon(cmd.Context(), false, func(c *api.Client) error {
				return a.salvage(cmd.Context(), c, p)
			})
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&volumes, "volume", nil, "copy this Docker volume (repeat for more)")
	f.StringArrayVar(&machines, "machine", nil, "copy this machine (repeat for more)")
	f.BoolVar(&docker, "docker", false, "copy all of the Docker engine's data as well, to DIR/docker")
	return cmd
}

type operations struct {
	mu     sync.Mutex
	early  map[string][]api.Operation
	waitID string
	done   chan api.Operation
	moved  func(api.Operation)
}

func (o *operations) seen(op api.Operation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.waitID == "" {
		o.early[op.ID] = append(o.early[op.ID], op)
		return
	}
	if op.ID == o.waitID {
		o.deliver(op)
	}
}

func (o *operations) deliver(op api.Operation) {
	if op.State == api.OperationRunning {
		if o.moved != nil {
			o.moved(op)
		}
		return
	}
	select {
	case o.done <- op:
	default:
	}
}

func (o *operations) wait(id string) <-chan api.Operation {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.waitID = id
	o.done = make(chan api.Operation, 1)
	for _, op := range o.early[id] {
		o.deliver(op)
	}
	o.early = nil
	return o.done
}

func (a *app) salvage(ctx context.Context, c *api.Client, p api.SalvageParams) error {
	ops := &operations{early: map[string][]api.Operation{}}
	if !a.quiet && !a.json() {
		phase := ""
		ops.moved = func(op api.Operation) {
			if op.Phase != "" && op.Phase != phase {
				phase = op.Phase
				fmt.Fprintf(a.Stderr, "fev: salvage: %s\n", salvagePhase(phase))
			}
		}
	}
	_, unsubscribe, err := c.Subscribe(ctx, api.Events{Operation: ops.seen})
	if err != nil {
		return err
	}
	defer unsubscribe()
	op, err := c.StorageSalvage(ctx, p)
	if err != nil {
		return err
	}
	var end api.Operation
	select {
	case end = <-ops.wait(op.ID):
	case <-c.Done():
		return failf(ExitUnreachable, "salvage: the daemon's connection ended; what was copied is in %s", p.Dest)
	case <-ctx.Done():
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c.OperationCancel(cctx, op.ID)
		return failf(ExitFailed, "salvage: interrupted; what was copied before is in %s", p.Dest)
	}
	if end.State != api.OperationDone {
		why := end.Error
		if why == "" {
			why = end.State
		}
		return failf(ExitFailed, "salvage: %s", why)
	}
	var r api.Salvaged
	if err := json.Unmarshal(end.Result, &r); err != nil {
		return fmt.Errorf("salvage: the result: %w", err)
	}
	if a.json() {
		return a.emit(end, nil)
	}
	return a.emit(end, func(w io.Writer) {
		kv(w, "to", p.Dest)
		kv(w, "read by", map[string]string{"copy": "a read-only mount", "restore": "btrfs restore (nothing mounted)"}[r.Method])
		kv(w, "files", r.Files)
		kv(w, "size", bytesText(r.Bytes))
		kv(w, "not read", r.Lost)
		for _, e := range r.Errors {
			fmt.Fprintf(w, "  %s\n", e)
		}
		if int64(len(r.Errors)) < r.Lost {
			fmt.Fprintf(w, "  (and %d more)\n", r.Lost-int64(len(r.Errors)))
		}
		fmt.Fprintln(w, salvageCaveat)
	})
}

func salvagePhase(phase string) string {
	switch phase {
	case "copy":
		return "copying from the disk mounted read-only"
	case "restore":
		return "the disk does not mount; reading it with btrfs restore (this can take long, and shows no progress)"
	}
	return strings.ReplaceAll(phase, "_", " ")
}
