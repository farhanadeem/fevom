package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func (a *app) printJSON(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.Stdout, string(data))
	return err
}

func (a *app) emit(v any, text func(w io.Writer)) error {
	switch {
	case a.quiet:
		return nil
	case a.json():
		return a.printJSON(v)
	}
	text(a.Stdout)
	return nil
}

func kv(w io.Writer, key string, value any) { fmt.Fprintf(w, "%s: %v\n", key, value) }

func table(w io.Writer, header []string, rows [][]string) {
	width := make([]int, len(header))
	for i, h := range header {
		width[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			width[i] = max(width[i], len(cell))
		}
	}
	line := func(cells []string) {
		var b strings.Builder
		for i, cell := range cells {
			if i == len(cells)-1 {
				b.WriteString(cell)
				break
			}
			fmt.Fprintf(&b, "%-*s  ", width[i], cell)
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	line(header)
	for _, row := range rows {
		line(row)
	}
}

func bytesText(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, suffix := float64(n)/unit, "KiB"
	for _, s := range []string{"MiB", "GiB", "TiB"} {
		if value < unit {
			break
		}
		value, suffix = value/unit, s
	}
	return fmt.Sprintf("%.1f %s", value, suffix)
}

func jsonLine(v any) (string, error) {
	data, err := json.Marshal(v)
	return string(data), err
}
