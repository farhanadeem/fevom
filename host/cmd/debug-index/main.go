package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"fev/host/debugindex"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "build" {
		fmt.Fprintln(os.Stderr, "usage: debug-index build -full FILE -packages FILE.br -out TABLE [-header H] [-stats FILE]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	full := fs.String("full", "", "nix-index-database full index for the arch (NIXI/frcode/zstd)")
	pkgs := fs.String("packages", "", "channels.nixos.org packages.json.br (brotli)")
	out := fs.String("out", "", "the table to write")
	header := fs.String("header", "", "a header line naming the channel/arch")
	statsOut := fs.String("stats", "", "write build stats as JSON here")
	fs.Parse(os.Args[2:])
	if *full == "" || *pkgs == "" || *out == "" {
		fs.Usage()
		os.Exit(2)
	}

	fullData, err := os.ReadFile(*full)
	must(err)
	rows, err := debugindex.DecodeFullIndex(fullData)
	must(err)

	pf, err := os.Open(*pkgs)
	must(err)
	pm, err := debugindex.ParsePackages(pf)
	pf.Close()
	must(err)

	table, st := debugindex.Build(rows, pm)

	f, err := os.Create(*out)
	must(err)
	must(debugindex.WriteTable(f, table, *header))
	must(f.Close())

	tb, err := os.ReadFile(*out)
	must(err)
	sum := sha256.Sum256(tb)

	fmt.Fprintf(os.Stderr, "packages.json: version %s, %d packages\n", pm.Version, pm.Count)
	fmt.Fprintf(os.Stderr, "full index rows: %d\n", st.RawRows)
	fmt.Fprintf(os.Stderr, "table rows: %d, distinct commands: %d\n", st.Rows, st.Commands)
	fmt.Fprintf(os.Stderr, "from sbin: %d, from libexec: %d\n", st.FromSbin, st.FromLibexec)
	fmt.Fprintf(os.Stderr, "with a licence: %d, unfree: %d\n", st.WithLicense, st.Unfree)
	fmt.Fprintf(os.Stderr, "meta join: by name %d, by pname %d, by attr %d, missed %d\n",
		st.MetaByName, st.MetaByPname, st.MetaByAttr, st.MetaMissed)
	fmt.Fprintf(os.Stderr, "table bytes: %d\n", len(tb))
	fmt.Printf("%x  %s\n", sum, *out)

	if *statsOut != "" {
		sb, _ := json.MarshalIndent(st, "", "  ")
		os.WriteFile(*statsOut, sb, 0o644)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "debug-index:", err)
		os.Exit(1)
	}
}
