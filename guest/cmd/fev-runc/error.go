package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func runtimeError(c call, err error) {
	message := "fev-runc: " + err.Error()
	fmt.Fprintln(os.Stderr, message)
	if c.log == "" {
		return
	}
	f, openErr := os.OpenFile(c.log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if openErr != nil {
		return
	}
	defer f.Close()
	at := time.Now().UTC().Format(time.RFC3339Nano)
	if c.logFormat == "json" {
		_ = json.NewEncoder(f).Encode(struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
			Time  string `json:"time"`
		}{"error", message, at})
	} else {
		fmt.Fprintf(f, "time=%q level=error msg=%q\n", at, message)
	}
}
