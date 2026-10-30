package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"fev/host/daemon/logs"
	api "fev/proto/daemon"
)

type logHandler struct {
	hub   *logs.Hub
	attrs string
}

func (h logHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h logHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	b.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Resolve())
		return true
	})
	level := 7
	switch {
	case r.Level >= slog.LevelError:
		level = 3
	case r.Level >= slog.LevelWarn:
		level = 4
	case r.Level >= slog.LevelInfo:
		level = 6
	}
	h.hub.Write(logs.Entry{Source: api.LogDaemon, Time: r.Time, Level: level, Text: b.String()})
	return nil
}

func (h logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	for _, a := range attrs {
		h.attrs += fmt.Sprintf(" %s=%v", a.Key, a.Value.Resolve())
	}
	return h
}

func (h logHandler) WithGroup(string) slog.Handler { return h }
