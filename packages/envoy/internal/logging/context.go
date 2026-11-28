package logging

import (
	"context"
	"log/slog"
)

// contextLogger is the key of the logger a context carries (NewContext).
type contextLogger struct{}

// NewContext is ctx carrying logger, the logger FromContext answers for ctx and every context
// made from it. A caller that must decide what becomes of the lines a piece of work writes, its
// callees' included, hands that work a context carrying a logger of its own: the GitHub intake's
// delivery holds its lines until it knows whether it hands its envelope back
// (dispatch/delivery's Intake.deliver).
func NewContext(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, contextLogger{}, logger)
}

// FromContext is the logger ctx carries (NewContext), and slog.Default() where it carries none, so
// a line written through it goes where its caller sends it, and to the default logger everywhere
// else.
func FromContext(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(contextLogger{}).(*slog.Logger); ok {
		return logger
	}
	return slog.Default()
}
