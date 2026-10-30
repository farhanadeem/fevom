//go:build amd64

package store

import _ "embed"

//go:embed index-x86_64-linux.tsv.zst
var fevIndexTableZstd []byte
