package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"fev/guest/internal/k3spayload"
	"fev/guest/internal/registry"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "unpack":
		fs := flag.NewFlagSet("unpack", flag.ExitOnError)
		hash := fs.String("hash", "", "the payload's hash (else read from the binary)")
		check := fs.Bool("check", true, "check every file and link before the payload is marked")
		fs.Parse(os.Args[2:])
		if fs.NArg() != 3 {
			usage()
		}
		ex, err := k3spayload.ExtractWith(fs.Arg(0), fs.Arg(1), fs.Arg(2), k3spayload.Options{Hash: *hash, Check: *check})
		if err != nil {
			fmt.Fprintln(os.Stderr, "fev-registry unpack:", err)
			os.Exit(1)
		}
		json.NewEncoder(os.Stdout).Encode(ex)
	case "check":
		if len(os.Args) != 4 {
			usage()
		}
		files, links, err := k3spayload.Check(os.Args[2], os.Args[3])
		if err != nil {
			fmt.Fprintln(os.Stderr, "fev-registry check:", err)
			os.Exit(1)
		}
		fmt.Printf("checked %d files and %d links\n", files, links)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: fev-registry serve -containerd SOCK -content DIR -procs FILE | unpack [-hash HASH] [-check=false] WRAPPER DEST ROOT | check DEST HASH")
	os.Exit(2)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	sock := fs.String("containerd", "", "containerd's socket")
	content := fs.String("content", "", "containerd's content store (io.containerd.content.v1.content)")
	procs := fs.String("procs", "", "containerd's cgroup leaf's cgroup.procs")
	ns := fs.String("namespace", "moby", "Docker's containerd namespace")
	fs.Parse(args)
	if *sock == "" || *content == "" || *procs == "" {
		usage()
	}
	log.SetFlags(0)
	log.SetPrefix("fev-registry: ")
	l, err := net.FileListener(os.NewFile(3, "registry"))
	if err != nil {
		log.Fatalf("the listener init passed: %v", err)
	}
	srv := &http.Server{Handler: &registry.Server{
		Store: registry.NewContainerd(*sock, *ns, *content),
		Logf:  log.Printf,
	}}
	err = srv.Serve(&registry.Checked{Listener: l, Check: registry.Peer(*procs), Logf: log.Printf})
	log.Fatalf("stopped: %v", err)
}
