//go:build !race

package storetest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// These tests are of the rig's judgment of gaps, which the race build does not make, so they are
// out of it: envoy-go's plain step "Test what the race build cannot run at all or at full size"
// runs them, by name, as ci_job_reads.py derives that step from this file's tag.

// A Linux build does not promise a schedstat file: gVisor exposes neither /proc/self/schedstat
// nor /proc/thread-self/schedstat. Nor does it promise that a thread, or the process's memory, can
// be held out of NUMA balancing: a container's seccomp profile may refuse set_mempolicy or mbind.
// Where the probe cannot do one of these, the rig follows its race build and judges window shape
// alone, rather than fail every Measure before it starts - unless the run sets XidRequireGapsEnv,
// as the CI jobs that judge gaps do, where Measure fails and names what the probe could not do.
//
// The readable row proves a build whose probe reads the clocks and holds the thread still judges
// a planted gap. It fakes only the probe and judges a window built here, so it holds on a kernel
// whose real reader reads nothing too: Measure records each statement through the real
// readXidThreadClock, and a Measure run where that reader fails would judge the run's own unread
// gaps, not the probe.
//
// The unreadable, unholdable and unbindable rows drive one real transaction through Measure and
// must not fail, with XidRequireGapsEnv unset: the old compile-time `xidThreadClocks = true` makes
// the unreadable row call Fatal before it begins, so it is the control for this runtime probe. The
// required row sets it: Measure must stop, naming the clocks it could not read.
func TestXidGapJudgmentNeedsReadableThreadClocks(t *testing.T) {
	read := func() xidThreadClock { return xidThreadClock{thread: 1} }
	unread := func() xidThreadClock { return xidThreadClock{} }
	held := func() (func(), bool) { return func() {}, true }
	bound := func() bool { return true }
	for _, test := range []struct {
		name     string
		probe    *xidThreadClockProbe
		judge    bool
		measure  bool
		required string
	}{
		{name: "readable", probe: &xidThreadClockProbe{read: read, hold: held, memory: bound}, judge: true},
		{name: "unreadable", probe: &xidThreadClockProbe{read: unread, hold: held, memory: bound}, measure: true},
		{name: "unholdable", probe: &xidThreadClockProbe{read: read, hold: func() (func(), bool) { return func() {}, false }, memory: bound}, measure: true},
		{name: "unbindable", probe: &xidThreadClockProbe{read: read, hold: held, memory: func() bool { return false }}, measure: true},
		{name: "required", probe: &xidThreadClockProbe{read: unread, hold: held, memory: bound}, required: "scheduler clocks cannot be read"},
	} {
		t.Run(test.name, func(t *testing.T) {
			previous := xidThreadClockAvailability
			xidThreadClockAvailability = test.probe
			t.Cleanup(func() { xidThreadClockAvailability = previous })
			required := ""
			if test.required != "" {
				required = "1"
			}
			t.Setenv(XidRequireGapsEnv, required)

			if got := XidJudgesGaps(); got != test.judge {
				t.Fatalf("XidJudgesGaps() = %t, want %t when the thread-clock probe is %s (%s)", got, test.judge, test.name, XidNoGapJudgment())
			}
			if !test.judge && XidNoGapJudgment() == "" {
				t.Fatalf("a thread-clock probe that is %s names no reason it judges no gap", test.name)
			}

			if test.measure || test.required != "" {
				database := Open(t)
				tracer := NewXidTracer()
				traced := &store.Store{Pool: Traced(t, database.Pool, tracer, nil)}
				stopping := &stoppingTB{TB: t}
				measured := make(chan *XidReport, 1)
				go func() {
					defer close(measured)
					measured <- tracer.Measure(stopping, traced.Pool, func() {
						ctx := MarkXid(context.Background())
						tx, err := traced.Pool.Begin(ctx)
						if err != nil {
							stopping.Fatalf("begin the transaction: %v", err)
						}
						defer tx.Rollback(ctx)
						if _, err := tx.Exec(ctx, "select pg_current_xact_id()"); err != nil {
							stopping.Fatalf("take a transaction id: %v", err)
						}
						if err := tx.Commit(ctx); err != nil {
							stopping.Fatalf("commit the transaction: %v", err)
						}
					})
				}()
				report, finished := <-measured
				stopped := stopping.stop()
				switch {
				case test.required != "" && (finished || !strings.Contains(stopped, XidRequireGapsEnv) || !strings.Contains(stopped, test.required)):
					t.Fatalf("with %s set and a probe that is %s, Measure stopped with %q, want a stop naming the variable and %q", XidRequireGapsEnv, test.name, stopped, test.required)
				case test.required == "" && !finished:
					t.Fatalf("a probe that is %s stopped Measure: %s", test.name, stopped)
				case test.required == "":
					AssertXidWindows(t, report, XidWindowOf(2, "the forced xid and its COMMIT"))
				}
			}

			window := XidWindow{
				Statements:    []XidStatement{{SQL: "select pg_current_xact_id()"}},
				MaxGap:        2 * XidMaxGap,
				GapTime:       2 * XidMaxGap,
				StatementTime: time.Nanosecond,
			}
			if got := window.Violations(); !test.judge && len(got) != 0 {
				t.Fatalf("a thread-clock probe that is %s made the rig report gap violations %v, want shape alone", test.name, got)
			}
			if test.judge && len(window.Violations()) == 0 {
				t.Fatal("a readable thread clock judged no planted gap")
			}
		})
	}
}
