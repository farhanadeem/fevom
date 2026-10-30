package netstack

import "os"

func removeAll(path string) { os.RemoveAll(path) }
