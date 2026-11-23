package logtest

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"slices"
)

// DefaultText is the handler that writes each record to w as slog's default handler writes it
// through a log package whose flags are cleared: "LEVEL message key=value ...", one line a record,
// the attributes as slog's TextHandler writes them, at INFO and above. A test hands a logger over
// it to the code it tests, in place of the process's default logger, so it reads the very lines
// that code writes in production and no other test's. Each record is one Write, so a w that locks,
// such as Buffer, keeps each line whole while goroutines log at once.
//
// A top-level attribute keyed time, level or msg is left out of the line, as the built-ins of a
// TextHandler are; the default handler would write it. The broker logs none.
func DefaultText(w io.Writer) slog.Handler {
	return &defaultText{w: w}
}

type defaultText struct {
	w io.Writer
	// scope is the WithAttrs and WithGroup calls made to reach this handler, in order, replayed on
	// the TextHandler each record's attributes are written with.
	scope []func(slog.Handler) slog.Handler
}

func (h *defaultText) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (h *defaultText) Handle(ctx context.Context, record slog.Record) error {
	var attrs bytes.Buffer
	var text slog.Handler = slog.NewTextHandler(&attrs, &slog.HandlerOptions{ReplaceAttr: withoutBuiltIns})
	for _, scoped := range h.scope {
		text = scoped(text)
	}
	if err := text.Handle(ctx, record); err != nil {
		return err
	}
	line := record.Level.String() + " " + record.Message
	if attrs.Len() > len("\n") {
		line += " " + attrs.String()
	} else {
		line += "\n"
	}
	_, err := io.WriteString(h.w, line)
	return err
}

func (h *defaultText) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.scoped(func(next slog.Handler) slog.Handler { return next.WithAttrs(attrs) })
}

func (h *defaultText) WithGroup(name string) slog.Handler {
	return h.scoped(func(next slog.Handler) slog.Handler { return next.WithGroup(name) })
}

func (h *defaultText) scoped(scope func(slog.Handler) slog.Handler) slog.Handler {
	return &defaultText{w: h.w, scope: append(slices.Clip(h.scope), scope)}
}

// withoutBuiltIns leaves out a TextHandler's time, level and message, which DefaultText writes as
// the default handler does, before the attributes.
func withoutBuiltIns(groups []string, attr slog.Attr) slog.Attr {
	if len(groups) == 0 && (attr.Key == slog.TimeKey || attr.Key == slog.LevelKey || attr.Key == slog.MessageKey) {
		return slog.Attr{}
	}
	return attr
}
