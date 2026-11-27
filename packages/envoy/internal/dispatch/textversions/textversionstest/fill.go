package textversionstest

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Fill is a keyword fill a test runs as a serving process runs it (textversions.Run, through the
// start StartFill takes), in a goroutine of its own: its events wake is the test's (Wake), signalled
// where the listener hub's wake on the events channel would be, its lines are logged at debug level
// as JSON for the test to read (Log, Lines, Logged, AwaitLine), and it runs until the test stops
// it (Stop) or ends.
type Fill struct {
	events  chan struct{}
	logs    *logtest.Buffer
	cancel  context.CancelFunc
	stopped chan struct{}
}

// StartFill runs start, which runs textversions.Run with the test's own Config, in a goroutine of
// its own: on a context Stop ends, with the events wake Wake signals and the logger whose lines
// the Fill reads, both of which start puts in its Config. start answering an error fails the test.
// The fill is stopped once the test ends. It takes *testing.T, as Stop does, whose wait is
// testwait's.
func StartFill(t *testing.T, start func(ctx context.Context, wake <-chan struct{}, logger *slog.Logger) error) *Fill {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	fill := &Fill{events: make(chan struct{}, 1), logs: &logtest.Buffer{}, cancel: cancel, stopped: make(chan struct{})}
	logger := slog.New(slog.NewJSONHandler(fill.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	go func() {
		defer close(fill.stopped)
		if err := start(ctx, fill.events, logger); err != nil {
			t.Errorf("run the keyword fill: %v", err)
		}
	}()
	t.Cleanup(func() { fill.Stop(t) })
	return fill
}

// Wake signals the fill's events wake, as a commit that wrote an event does through the listener
// hub. A wake the fill has not taken yet stands for this one too.
func (f *Fill) Wake() {
	select {
	case f.events <- struct{}{}:
	default:
	}
}

// Stop ends the fill's context, as a serving process's stop does, and waits for Run to return. Its
// one bound is the test binary's deadline less testwait's margin (testwait.Until): a Run that never
// returns fails the test with the fill's log as it stood then.
func (f *Fill) Stop(t *testing.T) {
	t.Helper()
	f.cancel()
	testwait.Until(t, "the keyword fill's Run to return once its context ended (the fill's log)", func() (string, bool) {
		select {
		case <-f.stopped:
			return "", true
		default:
			return f.Log(), false
		}
	})
}

// Log is every line the fill has logged, for a failure to show.
func (f *Fill) Log() string {
	return f.logs.String()
}

// Lines is the messages of the lines the fill has logged whose message starts with prefix, in
// order.
func (f *Fill) Lines(t testing.TB, prefix string) []string {
	t.Helper()
	var messages []string
	for _, line := range logtest.JSONLines(t, f.logs.String()) {
		if message, _ := line["msg"].(string); strings.HasPrefix(message, prefix) {
			messages = append(messages, message)
		}
	}
	return messages
}

// Logged is whether the fill has logged a line whose message starts with prefix.
func (f *Fill) Logged(t testing.TB, prefix string) bool {
	t.Helper()
	return len(f.Lines(t, prefix)) > 0
}

// AwaitLine waits until the fill logs a line whose message starts with prefix, reading its log
// every 20 ms. Its one bound is the test binary's deadline less testwait's margin
// (testwait.UntilPolling): a fill that never logs the line fails the test with its log as it stood
// then.
func (f *Fill) AwaitLine(t *testing.T, prefix string) {
	t.Helper()
	testwait.UntilPolling(t, 20*time.Millisecond, fmt.Sprintf("the keyword fill to log a %q line (its log)", prefix), func() (string, bool) {
		if f.Logged(t, prefix) {
			return "", true
		}
		return f.Log(), false
	})
}
