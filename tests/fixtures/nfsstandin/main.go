package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/smallfz/libnfs-go/auth"
	"github.com/smallfz/libnfs-go/backend"
	"github.com/smallfz/libnfs-go/fs"
	"github.com/smallfz/libnfs-go/log"
	"github.com/smallfz/libnfs-go/memfs"
	"github.com/smallfz/libnfs-go/server"
)

const Readme = "fev view stand-in\n"

func main() {
	tcp := flag.String("tcp", "", "TCP address")
	path := flag.String("unix", "", "unix socket path")
	sw := flag.String("switch", "", "the rig's switch: a file naming the mode")
	objects := flag.String("objects", "", "directories to make at the root, comma-separated")
	lifeline := flag.Bool("lifeline", false, "end when standard input ends (the test's end)")
	flag.Parse()
	if *lifeline {
		go func() {
			io.Copy(io.Discard, os.Stdin)
			os.Exit(0)
		}()
	}
	log.UpdateLevel(log.ERROR)
	mfs := memfs.NewMemFS()
	b := backend.New(func() fs.FS { return mfs }, auth.Null)
	mfs.MkdirAll("/docker/volumes", 0o755)
	mfs.MkdirAll("/m1/etc", 0o755)
	f, err := mfs.OpenFile("/README.txt", os.O_CREATE|os.O_RDWR, 0o644)
	if err == nil {
		f.Write([]byte(Readme))
		f.Close()
	}
	if *objects != "" {
		makeObjects(mfs, *objects)
	}
	var ln net.Listener
	if *path != "" {
		os.Remove(*path)
		ln, err = net.Listen("unix", *path)
	} else {
		ln, err = net.Listen("tcp", *tcp)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *sw != "" {
		front := ln
		inner := front.Addr().String() + ".inner"
		if *path == "" {
			inner = *tcp + ".inner"
		}
		os.Remove(inner)
		if ln, err = net.Listen("unix", inner); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := startSwitch(front, inner, *sw); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("listening", front.Addr())
		s, err := server.NewServer(ln, b)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, s.Serve())
		return
	}
	s, err := server.NewServer(ln, b)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("listening", ln.Addr())
	fmt.Fprintln(os.Stderr, s.Serve())
}
