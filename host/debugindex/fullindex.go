package debugindex

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
)

type RawRow struct {
	Command   string
	Attr      string
	Output    string
	TopLevel  bool
	StorePath string
	Where     string
}

func DecodeFullIndex(data []byte) ([]RawRow, error) {
	if len(data) < 12 || string(data[:4]) != "NIXI" {
		return nil, fmt.Errorf("full index: not a nix-index database")
	}
	version := binary.LittleEndian.Uint64(data[4:12])
	if version != 1 {
		return nil, fmt.Errorf("full index: format version %d, this decoder knows 1", version)
	}
	zr, err := zstd.NewReader(bytes.NewReader(data[12:]), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	var rows []RawRow
	type fileEnt struct {
		path string
		kind byte
	}
	var prev []byte
	shared := 0
	var files []fileEnt
	i, n := 0, len(raw)
	for i < n {
		z := bytes.IndexByte(raw[i:], 0)
		if z < 0 {
			return nil, fmt.Errorf("full index: truncated record")
		}
		z += i
		footer := raw[i:z]
		j := z + 1
		if j >= n {
			return nil, fmt.Errorf("full index: truncated record")
		}
		var d int
		if raw[j] == 0x80 {
			if j+3 > n {
				return nil, fmt.Errorf("full index: truncated delta")
			}
			d = int(int16(binary.BigEndian.Uint16(raw[j+1 : j+3])))
			j += 3
		} else {
			if raw[j] > 127 {
				d = int(raw[j]) - 256
			} else {
				d = int(raw[j])
			}
			j++
		}
		e := bytes.IndexByte(raw[j:], '\n')
		if e < 0 {
			return nil, fmt.Errorf("full index: unterminated line")
		}
		e += j
		shared += d
		if shared < 0 || shared > len(prev) {
			return nil, fmt.Errorf("full index: bad shared prefix")
		}
		content := make([]byte, 0, shared+(e-j))
		content = append(content, prev[:shared]...)
		content = append(content, raw[j:e]...)
		prev = content
		i = e + 1
		if len(footer) == 1 && footer[0] == 'p' {
			var pkg struct {
				StoreDir string `json:"store_dir"`
				Hash     string `json:"hash"`
				Name     string `json:"name"`
				Origin   struct {
					Attr     string `json:"attr"`
					Output   string `json:"output"`
					TopLevel bool   `json:"toplevel"`
				} `json:"origin"`
			}
			if err := json.Unmarshal(content, &pkg); err != nil {
				return nil, fmt.Errorf("full index: package JSON: %w", err)
			}
			sp := pkg.StoreDir + "/" + pkg.Hash + "-" + pkg.Name
			for _, f := range files {
				cmd, where, ok := commandOf(f.path, f.kind)
				if !ok {
					continue
				}
				rows = append(rows, RawRow{cmd, pkg.Origin.Attr, pkg.Origin.Output, pkg.Origin.TopLevel, sp, where})
			}
			files = files[:0]
		} else if len(footer) > 0 {
			files = append(files, fileEnt{string(content), footer[len(footer)-1]})
		}
	}
	return rows, nil
}

func commandOf(path string, kind byte) (cmd, where string, ok bool) {
	if kind != 'x' && kind != 's' {
		return "", "", false
	}
	for _, p := range []struct{ pre, where string }{
		{"/bin/", "bin"},
		{"/sbin/", "sbin"},
	} {
		if rest, found := strings.CutPrefix(path, p.pre); found {
			if rest == "" || strings.Contains(rest, "/") {
				return "", "", false
			}
			if strings.HasPrefix(rest, ".") || strings.HasSuffix(rest, "-wrapped") {
				return "", "", false
			}
			return rest, p.where, true
		}
	}
	return "", "", false
}
