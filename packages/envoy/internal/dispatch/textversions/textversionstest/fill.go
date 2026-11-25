package textversionstest

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

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
// The fill is stopped once the test ends.
func StartFill(t testing.TB, start func(ctx context.Context, wake <-chan struct{}, logger *slog.Logger) error) *Fill {
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
	t.Cleanup(func() { fill.Stop(t, "the keyword fill to return after its stop") })
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

// Stop ends the fill's context, as a serving process's stop does, and waits, through the shared
// wait, for Run to return: what names that wait, and its failure carries the fill's log. The
// reading is the fill's log buffer, which the shared wait renders (String) only when it gives up,
// so an ask reads nothing of the log.
func (f *Fill) Stop(t testing.TB, what string) {
	t.Helper()
	f.cancel()
	testwait.Until(t, what, func() (fmt.Stringer, bool) {
		select {
		case <-f.stopped:
			return nil, true
		default:
			return f.logs, false
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

// AwaitLine waits, through the shared wait, until the fill logs a line whose message starts with
// prefix. Its failure carries the fill's log as it stood at the last read.
func (f *Fill) AwaitLine(t testing.TB, prefix string) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the keyword fill to log a %q line", prefix), func() (string, bool) {
		return f.Log(), f.Logged(t, prefix)
	})
}
