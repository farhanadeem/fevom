package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"fev/host/debugremote/fakeengine"
)

func main() {
	sock := flag.String("socket", "", "unix socket to serve the Engine API on")
	arch := flag.String("arch", "aarch64", "the engine's architecture")
	config := flag.String("config", "", "write a Docker context store for this socket into this DOCKER_CONFIG dir")
	context := flag.String("context", "remote", "the context name to write")
	flag.Parse()
	if *sock == "" {
		log.Fatal("debugfakeengine: -socket is required")
	}
	if *config != "" {
		if err := writeContextStore(*config, *context, "unix://"+*sock); err != nil {
			log.Fatalf("debugfakeengine: writing the context store: %v", err)
		}
	}
	os.Remove(*sock)
	ln, err := net.Listen("unix", *sock)
	if err != nil {
		log.Fatalf("debugfakeengine: listen: %v", err)
	}
	fe := fakeengine.New(*arch, ln)
	fe.AddContainer("api", "ghcr.io/example/api:latest", true)
	fe.AddContainer("db", "postgres:17", false)
	fe.AddImage("ghcr.io/example/api:latest")
	log.Printf("debugfakeengine: serving on %s (%s)", *sock, *arch)
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c
	fe.Close()
}

func writeContextStore(config, name, host string) error {
	sum := sha256.Sum256([]byte(name))
	dir := filepath.Join(config, "contexts", "meta", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	meta := map[string]any{
		"Name":      name,
		"Metadata":  map[string]any{"Description": "a scratch engine for the tests"},
		"Endpoints": map[string]any{"docker": map[string]any{"Host": host}},
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644); err != nil {
		return err
	}
	cfg, _ := json.Marshal(map[string]any{"currentContext": name})
	return os.WriteFile(filepath.Join(config, "config.json"), cfg, 0o644)
}
