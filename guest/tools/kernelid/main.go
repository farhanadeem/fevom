package main

import (
	"fmt"
	"os"

	"fev/guest/internal/kernelid"
)

func main() {
	if len(os.Args) != 3 || (os.Args[1] != "image" && os.Args[1] != "notes") {
		fmt.Fprintln(os.Stderr, "usage: kernelid image|notes FILE")
		os.Exit(2)
	}
	b, err := os.ReadFile(os.Args[2])
	if err == nil {
		var id string
		if os.Args[1] == "image" {
			id, err = kernelid.FromImage(b)
		} else {
			id, err = kernelid.FromNotes(b)
		}
		if err == nil {
			fmt.Println(id)
			return
		}
	}
	fmt.Fprintf(os.Stderr, "kernelid: %s: %v\n", os.Args[2], err)
	os.Exit(1)
}
