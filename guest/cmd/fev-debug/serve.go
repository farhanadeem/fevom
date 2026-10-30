package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"fev/guest/debug"
	"fev/guest/debug/store"
	"fev/proto/agent"
	"fev/proto/debugstore"
	"fev/proto/jsonrpc"
)

type server struct {
	st      *store.Store
	authz   func(conn net.Conn) error
	install func(ctx context.Context, names []string, progress func(debugstore.Progress)) (*debugstore.Installed, error)

	mu   sync.Mutex
	live int
}

func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	storeDir := fs.String("store", "", "the @debug store directory")
	listen := fs.String("listen", "", "bind and listen on this Unix socket path")
	listenFd := fs.Int("listen-fd", -1, "serve an inherited listener fd (from init)")
	cache := fs.String("cache", "", "the binary cache URL (debug.cache_url)")
	indexURL := fs.String("index-url", "", "the command index URL (debug.index_url)")
	idleMS := fs.Int("idle-ms", 2000, "exit this long after the last connection closes")
	sessions := fs.String("sessions", "", "the open sessions' registrations (init's; default STORE/sessions)")
	pins := fs.String("pins", "", "the open sessions' pins (init's; default STORE/pins)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *storeDir == "" {
		return errors.New("serve: -store is required")
	}
	st, err := store.Open(*storeDir, *cache)
	if err != nil {
		return err
	}
	st.SetIndexURL(*indexURL)
	if *pins != "" {
		st.SetPinsDir(*pins)
	}
	sdir := *sessions
	if sdir == "" {
		sdir = filepath.Join(st.Root(), "sessions")
	}

	var ln net.Listener
	switch {
	case *listenFd >= 0:
		f := os.NewFile(uintptr(*listenFd), "listener")
		if ln, err = net.FileListener(f); err != nil {
			return err
		}
		f.Close()
	case *listen != "":
		os.Remove(*listen)
		if ln, err = net.Listen("unix", *listen); err != nil {
			return err
		}
	default:
		return errors.New("serve: -listen or -listen-fd is required")
	}

	s := &server{st: st, authz: authzPeer(sdir)}
	return s.serve(ln, time.Duration(*idleMS)*time.Millisecond)
}

func (s *server) serve(ln net.Listener, idle time.Duration) error {
	conns := make(chan net.Conn)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(conns)
				return
			}
			conns <- conn
		}
	}()
	closed := make(chan struct{}, 16)
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		select {
		case conn, ok := <-conns:
			if !ok {
				return nil
			}
			if s.live == 0 && !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			s.live++
			go func() {
				s.handleConn(conn)
				closed <- struct{}{}
			}()
		case <-closed:
			s.live--
			if s.live == 0 {
				timer.Reset(idle)
			}
		case <-timer.C:
			if s.live == 0 {
				return nil
			}
		}
	}
}

func (s *server) handleConn(conn net.Conn) {
	c := jsonrpc.NewConn(conn, jsonrpc.HandlerFunc(func(ctx context.Context, c *jsonrpc.Conn, method string, params json.RawMessage) (any, error) {
		return s.handleOn(ctx, c, conn, method, params)
	}))
	<-c.Done()
}

func writeMethod(method string) bool {
	switch method {
	case debugstore.MethodInstall, debugstore.MethodRemove, debugstore.MethodGC:
		return true
	}
	return false
}

func (s *server) handle(ctx context.Context, conn net.Conn, method string, params json.RawMessage) (any, error) {
	return s.handleOn(ctx, nil, conn, method, params)
}

func (s *server) handleOn(ctx context.Context, c *jsonrpc.Conn, conn net.Conn, method string, params json.RawMessage) (any, error) {
	if writeMethod(method) && s.authz != nil {
		if err := s.authz(conn); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidRequest, "debug store: %v", err)
		}
	}
	switch method {
	case debugstore.MethodLookup:
		p, err := jsonrpc.Bind[debugstore.LookupParams](params)
		if err != nil {
			return nil, err
		}
		return s.st.Lookup(ctx, p.Command)
	case debugstore.MethodSearch:
		p, err := jsonrpc.Bind[debugstore.SearchParams](params)
		if err != nil {
			return nil, err
		}
		return s.st.Search(ctx, p.Text, p.Limit)
	case debugstore.MethodList:
		return s.st.List()
	case debugstore.MethodInstall:
		p, err := jsonrpc.Bind[debugstore.InstallParams](params)
		if err != nil {
			return nil, err
		}
		var progress func(debugstore.Progress)
		if c != nil {
			last := time.Time{}
			progress = func(pr debugstore.Progress) {
				if now := time.Now(); now.Sub(last) >= 100*time.Millisecond || pr.PathsDone == pr.PathsTotal {
					last = now
					c.Notify(debugstore.NotifyProgress, pr)
				}
			}
		}
		if s.install != nil {
			return s.install(ctx, p.Names, progress)
		}
		return s.st.Install(ctx, p.Names, progress)
	case debugstore.MethodRemove:
		p, err := jsonrpc.Bind[debugstore.RemoveParams](params)
		if err != nil {
			return nil, err
		}
		return s.st.Remove(p.Names)
	case debugstore.MethodGC:
		return s.st.GC()
	default:
		return nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "no method %q", method)
	}
}

func statusCmd(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	storeDir := fs.String("store", "", "the @debug store directory")
	exists := fs.Bool("exists", true, "whether @debug exists (init decides)")
	defaultSet := fs.String("default-set", "", "the defaultset.lock, for the whole-set check")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *storeDir == "" {
		return errors.New("status: -store is required")
	}
	out := agent.DebugStatus{Exists: *exists}
	if *exists {
		st, err := store.Open(*storeDir, "")
		if err != nil {
			return err
		}
		bytes, paths := st.Bytes()
		out.Bytes, out.Paths = bytes, paths
		list, _ := st.List()
		for _, p := range list.Packages {
			out.Packages = append(out.Packages, agent.DebugPackage{
				Name: p.Name, Attr: p.Attr, StorePath: p.StorePath,
				Commands: p.Commands, Default: p.Default,
			})
		}
		out.IndexDate = st.IndexDate()
		if *defaultSet != "" {
			id, whole := st.DefaultSetState(*defaultSet)
			out.DefaultSet, out.DefaultWhole = id, whole
		}
	}
	b, _ := json.Marshal(out)
	fmt.Println(string(b))
	return nil
}

func resetCmd(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	storeDir := fs.String("store", "", "the @debug store directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *storeDir == "" {
		return errors.New("reset: -store is required")
	}
	st, err := store.Open(*storeDir, "")
	if err != nil {
		return err
	}
	freed, err := st.Reset()
	if err != nil {
		return err
	}
	b, _ := json.Marshal(agent.DebugReset{FreedBytes: freed})
	fmt.Println(string(b))
	return nil
}

func fetchCmd(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	storeDir := fs.String("store", "", "the store directory to fill")
	lock := fs.String("lock", "", "the defaultset.lock naming the tool set")
	cache := fs.String("cache", "", "the binary cache URL (default cache.nixos.org)")
	quiet := fs.Bool("q", false, "print no progress")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *storeDir == "" || *lock == "" {
		return errors.New("fetch: -store and -lock are required")
	}
	lockBytes, err := os.ReadFile(*lock)
	if err != nil {
		return err
	}
	stamp := debug.Stamp(lockBytes)
	if debug.StoreReady(*storeDir, stamp) {
		return nil
	}
	st, err := store.Open(*storeDir, *cache)
	if err != nil {
		return err
	}
	var progress func(debugstore.Progress)
	if !*quiet {
		progress = progressPrinter(os.Stderr, "the debug tools")
	}
	res, err := st.InstallDefaultSet(context.Background(), *lock, progress)
	if err != nil {
		return err
	}
	etc := filepath.Join(*storeDir, "etc")
	if err := debug.WriteEtc(etc); err != nil {
		return err
	}
	if err := st.LinkCABundle(etc); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*storeDir, debug.StampFile+".tmp"), []byte(stamp+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(*storeDir, debug.StampFile+".tmp"), filepath.Join(*storeDir, debug.StampFile)); err != nil {
		return err
	}
	b, _ := json.Marshal(res)
	fmt.Println(string(b))
	return nil
}

func stampCmd(args []string) error {
	fs := flag.NewFlagSet("stamp", flag.ContinueOnError)
	lock := fs.String("lock", "", "the defaultset.lock")
	if err := fs.Parse(args); err != nil {
		return err
	}
	b, err := os.ReadFile(*lock)
	if err != nil {
		return err
	}
	fmt.Println(debug.Stamp(b))
	return nil
}

func resolveCmd(args []string) error {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	storeDir := fs.String("store", "", "a store whose cached index to use (fetched if absent)")
	indexURL := fs.String("index-url", "", "the command index URL (empty: the shipped index)")
	indexFile := fs.String("index-file", "", "load this table file (.tsv or .tsv.zst) instead of the shipped index")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := *storeDir
	if dir == "" {
		d, err := os.MkdirTemp("", "fev-debug-resolve")
		if err != nil {
			return err
		}
		defer os.RemoveAll(d)
		dir = d
	}
	st, err := store.Open(dir, "")
	if err != nil {
		return err
	}
	st.SetIndexURL(*indexURL)
	st.SetIndexFile(*indexFile)
	for _, name := range fs.Args() {
		sp, rerr := st.ResolveName(context.Background(), name)
		if rerr != nil {
			return fmt.Errorf("%s: %w", name, rerr)
		}
		fmt.Printf("%s\t%s\n", name, sp)
	}
	return nil
}
