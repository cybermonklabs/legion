package api

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// keySweep is the statement a pass of the key sweep is recognised by (sweepRequestKeys).
const keySweep = "delete from request_keys"

// maintenanceSweepPeriod is the period the two runners below keep, the server's own: both tick on
// one fake clock the test moves a period at a time, so its length costs the test nothing.
const maintenanceSweepPeriod = time.Minute

// startOfPeriodAfterNext is the start of the period after the one running now, as duty.Exclusive
// buckets a tick's due time (periodBoundary: Unix nanoseconds less their remainder).
func startOfPeriodAfterNext(interval time.Duration) time.Time {
	ns := time.Now().UnixNano()
	return time.Unix(0, ns-ns%interval.Nanoseconds()).Add(interval)
}

// Two envoy-dispatch processes run the maintenance sweep on one database, each on a pool of its
// own (storetest.SecondProcess, as a second process has). The keys past their window and the
// deliveries a dead process owed need one pass per period across the two, not one per process:
// each pass records its period under the lease (duty.RunLeased), so the other process's tick
// inside a period already served makes no statement of its own. Both runners tick on one fake
// clock, which the test moves a period at a time.
func TestTheMaintenanceLoopSweepsOncePerPeriodAcrossTwoRunners(t *testing.T) {
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	recorders := make([]*storetest.StatementRecorder, 0, 2)
	swept := func() int {
		return countStatements(recorders[0].Statements(), keySweep) +
			countStatements(recorders[1].Statements(), keySweep)
	}
	// The resume is the last step of a pass, run after the key sweep's delete has returned and
	// still under the lease, so no pass of the other runner is in flight while it runs: there the
	// key sweeps that ever started are exactly the passes that ran, this one included. The
	// recorder sees a delete as it starts, so a count taken anywhere else could include a pass
	// still in its round trip.
	var resumes atomic.Int64
	var unpaired atomic.Pointer[string]
	resume := func(context.Context) error {
		pass := resumes.Add(1)
		if sweeps := swept(); int64(sweeps) != pass {
			message := fmt.Sprintf("pass %d resumed deliveries after %d key sweeps; every pass runs both", pass, sweeps)
			unpaired.CompareAndSwap(nil, &message)
		}
		return nil
	}
	// Both runners start a quarter of a period past a boundary, so every tick of both falls well
	// inside one bucket rather than straddling two: this row is about the two processes' ticks
	// inside one period, not about which bucket a tick landed in.
	start := startOfPeriodAfterNext(maintenanceSweepPeriod).Add(maintenanceSweepPeriod / 4)
	fake := clocktest.New(start)
	for _, process := range []*store.Store{database, second} {
		recorder := &storetest.StatementRecorder{}
		pool := storetest.Traced(t, process.Pool, recorder, nil)
		leases := sweepLeases(t, &store.Store{Pool: pool})
		recorders = append(recorders, recorder)
		runSweep(t, func(ctx context.Context) {
			runMaintenanceSweepOn(ctx, fake, pool, maintenanceSweepPeriod, leases, resume)
		})
	}
	// bothArmed waits until both runners have armed their tick due at due: each arms its next tick
	// once the tick before it has returned, its pass or its refusal included.
	bothArmed := func(due time.Time) {
		t.Helper()
		testwait.Until(t, "both runners' ticks armed for "+due.Format(time.RFC3339), func() ([]time.Time, bool) {
			armed := fake.Armed()
			return armed, len(slices.DeleteFunc(slices.Clone(armed), func(at time.Time) bool { return !at.Equal(due) })) >= 2
		})
	}

	const periods = 6
	for period := int64(1); period <= periods; period++ {
		due := start.Add(time.Duration(period) * maintenanceSweepPeriod)
		bothArmed(due)
		fake.Set(due)
		bothArmed(due.Add(maintenanceSweepPeriod))
		if got := resumes.Load(); got != period {
			t.Fatalf("%d passes across the two runners after %d periods of %v: one pass per period, want %d", got, period, maintenanceSweepPeriod, period)
		}
	}
	if message := unpaired.Load(); message != nil {
		t.Fatal(*message)
	}
}
