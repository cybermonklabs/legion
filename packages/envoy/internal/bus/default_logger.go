package bus

import (
	"context"
	"log/slog"
	"slices"
)

// followsDefault is the logger of a client built without WithLogger, and of a bucket call made on
// a connection no client owns (loggerOwning). It writes each record to the handler slog.Default()
// has at the moment the record is written, as every line of the package went before a client took
// a logger: a program that sets the default after it built a client still finds that client's
// lines where it set them, in the same form.
var followsDefault = slog.New(defaultHandler{})

// defaultHandler sends each record to slog.Default()'s handler as it is when the record is
// written, with the attributes and groups a With or WithGroup on the logger added, in their order.
type defaultHandler struct {
	steps []func(slog.Handler) slog.Handler
}

func (h defaultHandler) current() slog.Handler {
	handler := slog.Default().Handler()
	for _, step := range h.steps {
		handler = step(handler)
	}
	return handler
}

func (h defaultHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.current().Enabled(ctx, level)
}

// Handle reads the default's handler once and checks the record against that handler: a
// SetDefault between the logger's Enabled and this call would otherwise hand a record to a handler
// that does not take its level.
func (h defaultHandler) Handle(ctx context.Context, record slog.Record) error {
	handler := h.current()
	if !handler.Enabled(ctx, record.Level) {
		return nil
	}
	return handler.Handle(ctx, record)
}

func (h defaultHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.then(func(handler slog.Handler) slog.Handler { return handler.WithAttrs(attrs) })
}

func (h defaultHandler) WithGroup(name string) slog.Handler {
	return h.then(func(handler slog.Handler) slog.Handler { return handler.WithGroup(name) })
}

func (h defaultHandler) then(step func(slog.Handler) slog.Handler) defaultHandler {
	return defaultHandler{steps: append(slices.Clip(h.steps), step)}
}
