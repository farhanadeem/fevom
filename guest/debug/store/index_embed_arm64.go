//go:build arm64

package store

import _ "embed"

//go:embed index-aarch64-linux.tsv.zst
var fevIndexTableZstd []byte
