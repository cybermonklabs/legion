package logtest

import (
	"errors"
	"log"
	"log/slog"
	"testing"
	"time"
)

// TestDefaultTextWritesTheDefaultHandlersLines: DefaultText is what a test reads the broker's
// lines through where it compares their text, in place of slog's own default handler writing
// through a log package with no flags, so for the same records the two write the same bytes: the
// level and message, every attribute as a TextHandler quotes it, the attributes a logger was made
// With and the groups it was made WithGroup, and nothing below INFO.
//
// The named revert: write the message without its level in front (DefaultText.Handle), and
// every line differs.
func TestDefaultTextWritesTheDefaultHandlersLines(t *testing.T) {
	// Not parallel: it reads slog's own default handler, which writes through the log package's
	// output and flags, process-wide, by pointing them at a buffer of its own until it ends.
	var want, got Buffer
	flags, output := log.Flags(), log.Writer()
	log.SetFlags(0)
	log.SetOutput(&want)
	t.Cleanup(func() { log.SetFlags(flags); log.SetOutput(output) })

	at := time.Date(2026, 11, 23, 10, 41, 7, 123456789, time.UTC)
	for _, logger := range []*slog.Logger{slog.Default(), slog.New(DefaultText(&got))} {
		logger.Info("plain")
		logger.Warn("broker: a line", "name", "WORKER_TOKEN", "count", 3, "spaced", "a b", "quoted", `say "hi"`,
			"empty", "", "error", errors.New("dial tcp 10.0.0.1:5432: connect: refused"), "wait", 1500*time.Millisecond, "at", at)
		logger.Error("agent secret policy refused", "name", "example/agent-secrets/key", "first_load", true, "nothing", nil)
		logger.Debug("below the level both write", "k", "v")
		logger.With("service", "legion").WithGroup("req").Info("scoped", "id", 7, slog.Group("inner", "a", 1, "b", "x y"))
		logger.WithGroup("empty").Info("a group with no attributes")
		logger.Info("a message with = and \"quotes\"", slog.Int("n", 1), slog.Any("list", []string{"a", "b"}))
	}
	if want.Len() == 0 {
		t.Fatal("slog's default handler wrote nothing into the log package's output: the test is blind")
	}
	if got.String() != want.String() {
		t.Fatalf("DefaultText wrote:\n%s\nslog's default handler wrote:\n%s", got.String(), want.String())
	}
}
