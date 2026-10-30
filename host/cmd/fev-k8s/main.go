package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"

	"fev/host/k8s"
	"fev/host/k8s/kube"
	"fev/proto/version"
)

func main() {
	socket := flag.String("socket", "", "the Unix socket to serve the wire on")
	showVersion := flag.Bool("version", false, "print the build identifier and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Build)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *socket == "" {
		fmt.Fprintln(os.Stderr, "fev-k8s: --socket PATH is required")
		os.Exit(2)
	}
	os.Remove(*socket)
	l, err := net.Listen("unix", *socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fev-k8s:", err)
		os.Exit(1)
	}
	os.Chmod(*socket, 0o600)
	go func() {
		io.Copy(io.Discard, os.Stdin)
		l.Close()
		os.Remove(*socket)
		os.Exit(0)
	}()
	srv := &k8s.Server{B: &kube.Cluster{Log: log}}
	fmt.Println("ready")
	if err := srv.Serve(l); err != nil {
		log.Info("fev-k8s: the socket closed", "err", err)
	}
}
