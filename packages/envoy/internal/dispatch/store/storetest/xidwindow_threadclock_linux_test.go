//go:build linux

package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A Linux build does not promise a schedstat file: gVisor exposes neither /proc/self/schedstat
// nor /proc/thread-self/schedstat. Where the probe cannot read it, the rig must follow its race
// build and judge window shape alone, rather than fail every Measure before it starts.
//
// The readable row proves a build whose probe reads the clocks still judges a planted gap. It
// fakes only the probe and judges a window built here, so it holds on a kernel whose real reader
// reads nothing too: Measure records each statement through the real readXidThreadClock, and a
// Measure run where that reader fails would judge the run's own unread gaps, not the probe.
//
// The unreadable row drives one real transaction through Measure and must not fail: the old
// compile-time `xidThreadClocks = true` makes that row call Fatal before it begins, so it is the
// control for this runtime probe.
func TestXidGapJudgmentNeedsReadableThreadClocks(t *testing.T) {
	if raceDetector {
		t.Skip("the race build judges window shape alone")
	}
	for _, test := range []struct {
		name    string
		read    func() xidThreadClock
		judge   bool
		measure bool
	}{
		{
			name:  "readable",
			read:  func() xidThreadClock { return xidThreadClock{thread: xidThread()} },
			judge: true,
		},
		{
			name:    "unreadable",
			read:    func() xidThreadClock { return xidThreadClock{} },
			measure: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			previous := xidThreadClockAvailability
			xidThreadClockAvailability = &xidThreadClockProbe{read: test.read}
			t.Cleanup(func() { xidThreadClockAvailability = previous })

			if got := XidJudgesGaps(); got != test.judge {
				t.Fatalf("XidJudgesGaps() = %t, want %t when the thread-clock probe is %s", got, test.judge, test.name)
			}

			if test.measure {
				database := Open(t)
				tracer := NewXidTracer()
				traced := &store.Store{Pool: Traced(t, database.Pool, tracer, nil)}
				report := tracer.Measure(t, traced.Pool, func() {
					ctx := MarkXid(context.Background())
					tx, err := traced.Pool.Begin(ctx)
					if err != nil {
						t.Fatalf("begin the transaction: %v", err)
					}
					defer tx.Rollback(ctx)
					if _, err := tx.Exec(ctx, "select pg_current_xact_id()"); err != nil {
						t.Fatalf("take a transaction id: %v", err)
					}
					if err := tx.Commit(ctx); err != nil {
						t.Fatalf("commit the transaction: %v", err)
					}
				})
				AssertXidWindows(t, report, XidWindowOf(2, "the forced xid and its COMMIT"))
			}

			window := XidWindow{
				Statements:    []XidStatement{{SQL: "select pg_current_xact_id()"}},
				MaxGap:        2 * XidMaxGap,
				GapTime:       2 * XidMaxGap,
				StatementTime: time.Nanosecond,
			}
			if got := window.Violations(); !test.judge && len(got) != 0 {
				t.Fatalf("an unreadable thread clock made the rig report gap violations %v, want shape alone", got)
			}
			if test.judge && len(window.Violations()) == 0 {
				t.Fatal("a readable thread clock judged no planted gap")
			}
		})
	}
}
