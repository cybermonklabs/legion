package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// A context carries the logger it was given, to every context made from it, and a context that
// carries none answers the default logger as it is when it is asked.
func TestFromContextIsTheLoggerTheContextCarriesElseTheDefault(t *testing.T) {
	var carried, other bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&carried, nil))
	ctx, cancel := context.WithTimeout(NewContext(context.Background(), logger), time.Minute)
	defer cancel()
	FromContext(ctx).Info("carried")
	if !strings.Contains(carried.String(), "msg=carried") {
		t.Errorf("a line through the logger a context made from NewContext's carries reached %q, want the carried logger", carried.String())
	}

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&other, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	FromContext(context.Background()).Info("default")
	if !strings.Contains(other.String(), "msg=default") || strings.Contains(carried.String(), "msg=default") {
		t.Errorf("a line through a context that carries no logger reached the default logger as %q and the carried one as %q, want the default logger alone", other.String(), carried.String())
	}
}
