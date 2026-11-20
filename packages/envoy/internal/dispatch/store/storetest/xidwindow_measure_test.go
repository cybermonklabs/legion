package storetest

import (
	"context"
	"fmt"
	"runtime"
	"runtime/trace"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A measured operation may stop its test: the docs package's runRigTransactionDoing calls
// t.Fatalf on a transaction's error, inside the window. That exit runs only Measure's deferred
// calls, so the capture must end on that path too: the tracer no longer recording, and the
// execution tracer off, since the runtime runs one at a time and the next Measure in the process
// starts its own. Before this rule the execution tracer was stopped only once operation
// returned, so after a stopped operation the next Measure could not start one ("tracing is
// already enabled") and failed as a rig problem. The stopped operation runs on a goroutine of its
// own under a testing.TB whose Fatalf stops that goroutine as a test's does (stoppingTB), so this
// test watches the stop without failing.
func TestAStoppedMeasureLeavesTheNextItsOwnTrace(t *testing.T) {
	if !XidJudgesGaps() {
		t.Skip("this build judges no gap, so Measure takes no execution trace")
	}
	// A run of this test against a rig that leaks the trace leaves it on; stop it, so the tests
	// after this one still start their own.
	t.Cleanup(trace.Stop)
	database := Open(t)
	tracer := NewXidTracer()
	traced := &store.Store{Pool: Traced(t, database.Pool, tracer, nil)}
	const stopWord = "the operation stops its test inside its window"
	transaction := func(tb testing.TB, stop bool) {
		ctx := MarkXid(context.Background())
		tx, err := traced.Pool.Begin(ctx)
		if err != nil {
			tb.Fatalf("begin the transaction: %v", err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "select pg_current_xact_id()"); err != nil {
			tb.Fatalf("take a transaction id: %v", err)
		}
		if stop {
			tb.Fatalf(stopWord)
		}
		if err := tx.Commit(ctx); err != nil {
			tb.Fatalf("commit the transaction: %v", err)
		}
	}

	stopping := &stoppingTB{TB: t}
	measured := make(chan struct{})
	go func() {
		defer close(measured)
		tracer.Measure(stopping, traced.Pool, func() { transaction(stopping, true) })
	}()
	<-measured
	if got := stopping.stop(); got != stopWord {
		t.Fatalf("the measured operation stopped with %q, want %q", got, stopWord)
	}
	if tracer.recording.Load() {
		t.Error("the tracer is still recording after its operation stopped Measure")
	}

	report := tracer.Measure(t, traced.Pool, func() { transaction(t, false) })
	AssertXidWindows(t, report, XidWindowOf(2, "the forced xid and its COMMIT"))
	for i, gap := range report.Windows[0].Gaps {
		if !gap.Traced {
			t.Errorf("the gap after statement %d of the next Measure's window was not read off its own execution trace", i)
		}
	}
}

// stoppingTB is a testing.TB whose Fatal, Fatalf and FailNow record why and stop the goroutine that
// calls them (runtime.Goexit), as a test's own do, without failing the test it wraps.
type stoppingTB struct {
	testing.TB
	mu      sync.Mutex
	stopped string
}

func (s *stoppingTB) Fatalf(format string, args ...any) {
	s.mu.Lock()
	s.stopped = fmt.Sprintf(format, args...)
	s.mu.Unlock()
	runtime.Goexit()
}

func (s *stoppingTB) Fatal(args ...any) { s.Fatalf("%s", fmt.Sprint(args...)) }

func (s *stoppingTB) FailNow() { s.Fatalf("FailNow") }

// stop is why the TB was stopped, empty when it was not.
func (s *stoppingTB) stop() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}
