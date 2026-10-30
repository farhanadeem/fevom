package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"fev/host/docker/dockerctx"
	"fev/host/docker/fakeguest"
	"fev/host/docker/proxy"
	"fev/host/docker/tools"
	"fev/host/docker/tools/httpfetch"
	"fev/host/docker/vsock"
)

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "dockerhost-dev: "+format+"\n", args...)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: dockerhost-dev proxy --listen PATH (--upstream PATH | --fake) [--hold DURATION] [--fail TEXT]
       dockerhost-dev tools list
       dockerhost-dev tools install --home DIR [--cache DIR] [--docker-config DIR]
       dockerhost-dev context (status|select|restore|remove) --docker-config DIR --socket PATH [--state FILE]`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "proxy":
		runProxy(os.Args[2:])
	case "tools":
		runTools(os.Args[2:])
	case "context":
		runContext(os.Args[2:])
	default:
		usage()
	}
}

func runProxy(args []string) {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	listen := fs.String("listen", "", "unix socket to serve the Docker API on")
	upstream := fs.String("upstream", "", "unix socket of the engine")
	fake := fs.Bool("fake", false, "serve a fake engine in this process")
	hold := fs.Duration("hold", 0, "how long connections are held before the engine is \"ready\"")
	failText := fs.String("fail", "", "refuse with this text when the hold ends")
	fs.Parse(args)
	if *listen == "" || (*upstream == "") == !*fake {
		usage()
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if *fake {
		dir, err := os.MkdirTemp("/tmp", "bd.")
		if err != nil {
			fail("%v", err)
		}
		defer os.RemoveAll(dir)
		*upstream = filepath.Join(dir, "engine.sock")
		l, err := net.Listen("unix", *upstream)
		if err != nil {
			fail("%v", err)
		}
		go fakeguest.NewEngine().Serve(l)
	}
	if _, err := proxy.RaiseFileLimit(); err != nil {
		log.Warn("cannot raise the open-file limit", "err", err)
	}
	p, err := proxy.Listen(*listen, log)
	if err != nil {
		fail("%v", err)
	}
	defer p.Close()
	log.Info("holding", "socket", *listen, "for", *hold)
	time.AfterFunc(*hold, func() {
		if *failText != "" {
			p.Refuse(*failText)
			log.Info("refused")
			return
		}
		r := p.Release(func(ctx context.Context) (net.Conn, error) { return vsock.Dial(ctx, *upstream) })
		log.Info("released", "joined", r.Joined, "abandoned", r.Abandoned)
	})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	held, active, joined := p.Counts()
	log.Info("stopping", "held", held, "active", active, "joined_in_all", joined)
}

func runTools(args []string) {
	if len(args) == 0 {
		usage()
	}
	switch args[0] {
	case "list":
		lock, err := tools.Lock()
		if err != nil {
			fail("%v", err)
		}
		for _, t := range lock {
			fmt.Printf("%-32s %-9s %-11s %s\n", t.Name, t.Version, t.Licence, t.URL)
		}
	case "install":
		fs := flag.NewFlagSet("tools install", flag.ExitOnError)
		home := fs.String("home", "", "fev home; the tools go in its tools directory")
		cache := fs.String("cache", "", "where downloads are kept (default HOME/cache/tools)")
		dockerConfig := fs.String("docker-config", "", "Docker configuration directory to link the CLI plugins into")
		fs.Parse(args[1:])
		if *home == "" {
			usage()
		}
		if *cache == "" {
			*cache = filepath.Join(*home, "cache", "tools")
		}
		in := tools.Installer{Cache: *cache, Bin: filepath.Join(*home, "tools"), Fetch: httpfetch.Fetch, Logf: func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", a...)
		}}
		done, err := in.Install(context.Background())
		if err != nil {
			fail("%v", err)
		}
		for _, d := range done {
			fmt.Printf("%s %s %s\n", d.Path, d.Version, d.Licence)
		}
		if *dockerConfig != "" {
			left, err := tools.LinkPlugins(*dockerConfig, in.Bin)
			if err != nil {
				fail("%v", err)
			}
			for _, path := range left {
				fmt.Fprintf(os.Stderr, "left alone, it is not ours: %s\n", path)
			}
		}
	default:
		usage()
	}
}

func runContext(args []string) {
	if len(args) == 0 {
		usage()
	}
	fs := flag.NewFlagSet("context", flag.ExitOnError)
	dir := fs.String("docker-config", "", "Docker configuration directory")
	socket := fs.String("socket", "", "the Docker API socket the context points at")
	state := fs.String("state", "", "file for the note of what was selected before")
	fs.Parse(args[1:])
	if *dir == "" || *socket == "" {
		usage()
	}
	s := dockerctx.Store{Dir: *dir, Socket: *socket, StateFile: *state}
	var err error
	switch args[0] {
	case "status":
	case "select":
		var selected bool
		var left string
		if selected, left, err = s.Select(); err == nil && !selected {
			fmt.Fprintf(os.Stderr, "not selected: the current context is %s\n", left)
		}
	case "restore":
		err = s.Restore()
	case "remove":
		err = s.Remove()
	default:
		usage()
	}
	if err != nil {
		fail("%v", err)
	}
	st, err := s.Status()
	if err != nil {
		fail("%v", err)
	}
	json.NewEncoder(os.Stdout).Encode(st)
}
