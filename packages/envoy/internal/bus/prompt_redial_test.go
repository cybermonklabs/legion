package bus

import (
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// The waits of WithPromptReconnect, pass by pass of an outage's redials: the first within the
// 50 ms jitter, at once; then each drawn across the whole of [b/2, b), where b starts at 250 ms and
// doubles each pass up to the 2 s cap, which every later pass keeps, however many there are. Each
// row reads the wait at the lowest and the highest draw, and the width the draw was asked for.
func TestPromptRedialWaitsDoubleFromAQuarterSecondToTheCap(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		pass     int
		low, top time.Duration
	}{
		{1, 0, 50 * time.Millisecond},
		{2, 125 * time.Millisecond, 250 * time.Millisecond},
		{3, 250 * time.Millisecond, 500 * time.Millisecond},
		{4, 500 * time.Millisecond, time.Second},
		{5, time.Second, 2 * time.Second},
		{6, time.Second, 2 * time.Second},
		{7, time.Second, 2 * time.Second},
		{64, time.Second, 2 * time.Second},
		{math.MaxInt, time.Second, 2 * time.Second},
	} {
		var width int64
		lowest := promptRedialWait(row.pass, func(n int64) int64 { width = n; return 0 })
		highest := promptRedialWait(row.pass, func(n int64) int64 { return n - 1 })
		if lowest != row.low || highest != row.top-1 || time.Duration(width) != row.top-row.low {
			t.Errorf("pass %d waits [%s, %s] drawn over %s, want [%s, %s) drawn over %s",
				row.pass, lowest, highest, time.Duration(width), row.low, row.top, row.top-row.low)
		}
	}
}

// WithPromptReconnect hands nats.go its waits (CustomReconnectDelayCB, which takes precedence over
// ReconnectWait and its jitter), and a connection without it keeps the second every other
// connection waits.
func TestPromptReconnectSetsTheConnectionsRedialWaits(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		prompt bool
	}{{"prompt", true}, {"default", false}} {
		t.Run(row.name, func(t *testing.T) {
			opts := options(slog.Default(), "prompt-redial-test", []string{"nats://127.0.0.1:4222"}, nil, nil, nil, nil, nil)
			settings := connectOpts{promptReconnect: row.prompt}.connectionSettings(nats.Name("prompt-redial-test"))
			if err := settings(&opts); err != nil {
				t.Fatalf("apply the settings: %v", err)
			}
			if !row.prompt {
				if opts.CustomReconnectDelayCB != nil || opts.ReconnectWait != time.Second {
					t.Fatalf("a connection without WithPromptReconnect waits %s with a custom delay set %t, want 1s and none",
						opts.ReconnectWait, opts.CustomReconnectDelayCB != nil)
				}
				return
			}
			if opts.CustomReconnectDelayCB == nil {
				t.Fatal("WithPromptReconnect set no redial waits")
			}
			for pass := 1; pass <= 12; pass++ {
				wait := opts.CustomReconnectDelayCB(pass)
				low := promptRedialWait(pass, func(int64) int64 { return 0 })
				top := promptRedialWait(pass, func(n int64) int64 { return n - 1 })
				if wait < low || wait > top {
					t.Errorf("pass %d waits %s, outside [%s, %s]", pass, wait, low, top)
				}
			}
		})
	}
}
