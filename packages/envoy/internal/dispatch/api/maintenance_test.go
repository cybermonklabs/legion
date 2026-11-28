package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// maintenanceTestInterval is the pace both runners keep here, far shorter than the server's
// minute so a handful of periods passes inside one test.
const maintenanceTestInterval = 300 * time.Millisecond

// keySweep is the statement a pass of the key sweep is recognised by (sweepRequestKeys).
const keySweep = "delete from request_keys"

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
// inside a period already served makes no statement of its own. The periods the passes served are
// read off those records, the lease's own, rather than off the test's clock around the passes.
func TestTheMaintenanceLoopSweepsOncePerPeriodAcrossTwoRunners(t *testing.T) {
	t.Parallel()
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
	// Each pass reads, under the lease, the period the pass before it served: duty.Exclusive
	// records a pass's period before it gives the lease back, so the record a pass reads is its
	// predecessor's, whichever runner made it. It reads before it counts itself, so the sixth
	// count comes after the fifth pass's period is in served.
	var servedMu sync.Mutex
	var served []time.Time
	resume := func(ctx context.Context) error {
		var boundary time.Time
		err := database.Pool.QueryRow(ctx, `select boundary from duty_passes where lease = $1 and period_ns = $2`,
			maintenanceLease, maintenanceTestInterval.Nanoseconds()).Scan(&boundary)
		switch {
		case err == nil:
			servedMu.Lock()
			served = append(served, boundary)
			servedMu.Unlock()
		case !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil:
			t.Errorf("read the period the pass before this one served: %v", err)
		}
		pass := resumes.Add(1)
		if sweeps := swept(); int64(sweeps) != pass {
			message := fmt.Sprintf("pass %d resumed deliveries after %d key sweeps; every pass runs both", pass, sweeps)
			unpaired.CompareAndSwap(nil, &message)
		}
		return nil
	}
	runners := make([]func(context.Context), 0, 2)
	for _, process := range []*store.Store{database, second} {
		recorder := &storetest.StatementRecorder{}
		pool := storetest.Traced(t, process.Pool, recorder, nil)
		leases := sweepLeases(t, &store.Store{Pool: pool})
		recorders = append(recorders, recorder)
		runners = append(runners, func(ctx context.Context) {
			RunMaintenanceSweep(ctx, pool, maintenanceTestInterval, leases, resume)
		})
	}

	// Both runners start a quarter of a period past a boundary, so every tick of both falls well
	// inside one bucket rather than straddling two: this row is about the two processes' ticks
	// inside one period, not about which bucket a tick landed in.
	started := startOfPeriodAfterNext(maintenanceTestInterval).Add(maintenanceTestInterval / 4)
	time.Sleep(time.Until(started))
	for _, run := range runners {
		runSweep(t, run)
	}

	const passes = 6
	waitUntil(t, "six completed passes across the two runners", func() bool { return resumes.Load() >= passes })
	// One pass per period puts each pass's period a period or more after the one before it; two
	// passes of one period record it twice.
	servedMu.Lock()
	periods := slices.Clone(served)
	servedMu.Unlock()
	if len(periods) < passes-1 {
		t.Fatalf("UNKNOWN: the passes read %d recorded periods, want the %d the passes before the sixth served", len(periods), passes-1)
	}
	for i := 1; i < len(periods); i++ {
		if step := periods[i].Sub(periods[i-1]); step < maintenanceTestInterval {
			t.Fatalf("pass %d served the period at %s, %v after pass %d served the period at %s: one pass per period of %v puts each a period or more after the last, so both processes are sweeping every period",
				i+1, periods[i].UTC().Format("15:04:05.000"), step, i, periods[i-1].UTC().Format("15:04:05.000"), maintenanceTestInterval)
		}
	}
	if message := unpaired.Load(); message != nil {
		t.Fatal(*message)
	}
}
