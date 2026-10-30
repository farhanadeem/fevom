package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"fev/proto/debugstore"
	"fev/proto/jsonrpc"

	_ "golang.org/x/crypto/x509roots/fallback"
)

func main() {
	prog := filepath.Base(os.Args[0])
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "%s: install|remove|list|search|which|info|not-found|serve|status|reset|resolve|fetch|stamp\n", prog)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serveCmd(args)
	case "status":
		err = statusCmd(args)
	case "reset":
		err = resetCmd(args)
	case "resolve":
		err = resolveCmd(args)
	case "fetch":
		err = fetchCmd(args)
	case "stamp":
		err = stampCmd(args)
	case "install", "remove", "list", "search", "which", "info", "not-found":
		err = galleyCmd(cmd, args)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", prog, err)
		os.Exit(1)
	}
}

func sockFlag(args []string) (string, []string) {
	sock := debugstore.StoreSocket
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-sock" && i+1 < len(args) {
			sock = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	return sock, rest
}

func dial(sock string) (*jsonrpc.Conn, error) {
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("the debug tool store is not reachable at %s: %w", sock, err)
	}
	show := progressPrinter(os.Stderr, "installing")
	return jsonrpc.NewConn(conn, jsonrpc.HandlerFunc(func(_ context.Context, _ *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
		if method == debugstore.NotifyProgress {
			if p, err := jsonrpc.Bind[debugstore.Progress](params); err == nil {
				show(p)
			}
		}
		return nil, nil
	})), nil
}

func progressPrinter(w *os.File, what string) func(debugstore.Progress) {
	tty := isatty(w.Fd())
	return func(p debugstore.Progress) {
		if !tty || installDone.Load() {
			return
		}
		fmt.Fprintf(w, "\r%s: %d of %d paths, %s of %s\x1b[K", what, p.PathsDone, p.PathsTotal, humanBytes(p.DoneBytes), humanBytes(p.TotalBytes))
		progressShown = true
	}
}

var (
	progressShown bool
	installDone   atomic.Bool
)

func endProgress(w *os.File) {
	if progressShown {
		fmt.Fprint(w, "\r\x1b[K")
		progressShown = false
	}
}

func galleyCmd(cmd string, args []string) error {
	sock, rest := sockFlag(args)
	if cmd == "not-found" {
		return notFound(sock, rest)
	}
	c, err := dial(sock)
	if err != nil {
		return err
	}
	defer c.Close()
	ctx := context.Background()
	switch cmd {
	case "install":
		if len(rest) == 0 {
			return fmt.Errorf("install NAME...")
		}
		return runInstall(ctx, c, rest, os.Stderr)
	case "remove":
		if len(rest) == 0 {
			return fmt.Errorf("remove NAME...")
		}
		var r debugstore.Removed
		if err := c.Call(ctx, debugstore.MethodRemove, debugstore.RemoveParams{Names: rest}, &r); err != nil {
			return err
		}
		fmt.Printf("removed %d, freed %s\n", len(r.Removed), humanBytes(r.FreedBytes))
		return nil
	case "list":
		var r debugstore.ListResult
		if err := c.Call(ctx, debugstore.MethodList, debugstore.Empty{}, &r); err != nil {
			return err
		}
		for _, p := range r.Packages {
			mark := ""
			if p.Default {
				mark = " (default set)"
			}
			fmt.Printf("%s\t%s%s\n", p.Name, strings.Join(p.Commands, " "), mark)
		}
		return nil
	case "search":
		if len(rest) == 0 {
			return fmt.Errorf("search TEXT")
		}
		var r debugstore.SearchResult
		if err := c.Call(ctx, debugstore.MethodSearch, debugstore.SearchParams{Text: strings.Join(rest, " "), Limit: 50}, &r); err != nil {
			return err
		}
		for _, m := range r.Matches {
			fmt.Printf("%s\t%s\t%s%s\n", m.Command, m.Attr, m.Name, licenceTag(m.License, m.Unfree))
		}
		return nil
	case "which":
		if len(rest) != 1 {
			return fmt.Errorf("which CMD")
		}
		var r debugstore.Lookup
		if err := c.Call(ctx, debugstore.MethodLookup, debugstore.LookupParams{Command: rest[0]}, &r); err != nil {
			return err
		}
		for _, cd := range r.Candidates {
			star := " "
			if cd.Pick {
				star = "*"
			}
			fmt.Printf("%s %s\t%s%s\n", star, cd.Attr, cd.Name, licenceTag(cd.License, cd.Unfree))
		}
		return nil
	case "info":
		if len(rest) != 1 {
			return fmt.Errorf("info NAME")
		}
		var r debugstore.Lookup
		if err := c.Call(ctx, debugstore.MethodLookup, debugstore.LookupParams{Command: rest[0]}, &r); err != nil {
			return err
		}
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	return fmt.Errorf("unknown command %q", cmd)
}

func runInstall(ctx context.Context, c *jsonrpc.Conn, names []string, w *os.File) error {
	var r debugstore.Installed
	err := c.Call(ctx, debugstore.MethodInstall, debugstore.InstallParams{Names: names}, &r)
	installDone.Store(true)
	endProgress(w)
	if err != nil {
		return err
	}
	var cmds []string
	for _, p := range r.Installed {
		cmds = append(cmds, p.Name)
	}
	fmt.Fprintf(w, "installed %s\n", strings.Join(cmds, ", "))
	return nil
}

func licenceTag(lic string, unfree bool) string {
	switch {
	case lic == "" && unfree:
		return "\t(unfree)"
	case lic == "":
		return ""
	case unfree:
		return "\t(" + lic + ", unfree)"
	default:
		return "\t(" + lic + ")"
	}
}

func humanBytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(u), 0
	for v := n / u; v >= u; v /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
