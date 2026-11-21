package storetest

import (
	"context"
	"math"
	"runtime"
	"runtime/debug"
	"runtime/trace"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// xidPoll is one read of pg_stat_activity: sent when the query left, received when its answer
// came back, and the backends that held an xid in the snapshot taken between the two.
type xidPoll struct {
	sent     time.Time
	received time.Time
	holders  []uint32
}

// Measure runs operation while it records every statement and polls which backends hold an xid,
// and answers the xid windows of the transactions operation's marked context ran (MarkXid). pool
// is the pool under test; the poller dials a connection of its own from its configuration,
// untraced, so it never waits for one of the pool's. operation runs on its caller's goroutine with
// idle Ps beside it (runWithIdlePs) and the collector held off (runWithoutCollector). Measures run
// one at a time (xidMeasuring).
//
// A run that sets XidRequireGapsEnv and cannot judge gaps fails here, naming why
// (xidUnjudgedGapsProblem, as RequireXidGapJudgment fails), instead of judging the window's shape
// alone; and a run that judges gaps fails here if an execution trace is already running, since the
// rig takes one of its own.
func (x *XidTracer) Measure(t testing.TB, pool *store.Pool, operation func()) *XidReport {
	t.Helper()
	xidMeasuring.Lock()
	defer xidMeasuring.Unlock()
	if problem := xidUnjudgedGapsProblem(); problem != "" {
		t.Fatal(problem)
	}
	if XidJudgesGaps() && trace.IsEnabled() {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): an execution trace is already running - go test -trace takes one for the whole test binary - and the runtime runs one at a time, so the rig cannot take the trace it reads the runtime's own waits off; run the xid window tests without go test -trace")
	}

	ctx := context.Background()
	config := pool.Config().ConnConfig.Copy()
	config.Tracer = nil
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("xid rig: open the poll connection: %v", err)
	}
	defer conn.Close(ctx)
	const holdersQuery = `
		select pid from pg_stat_activity
		where datname = current_database() and backend_xid is not null and pid <> pg_backend_pid()`
	poll := func() (xidPoll, error) {
		sample := xidPoll{sent: time.Now()}
		rows, err := conn.Query(ctx, holdersQuery)
		if err != nil {
			return sample, err
		}
		holders, err := pgx.CollectRows(rows, pgx.RowTo[uint32])
		sample.received = time.Now()
		sample.holders = holders
		return sample, err
	}
	// The first poll prepares the statement, so no later one pays for it.
	if _, err := poll(); err != nil {
		t.Fatalf("xid rig: poll pg_stat_activity: %v", err)
	}

	x.mu.Lock()
	x.spans = nil
	blind := x.blind
	x.mu.Unlock()
	x.recording.Store(true)

	stop := make(chan struct{})
	var polls []xidPoll
	var pollErr error
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		if blind > 0 {
			select {
			case <-stop:
				return
			case <-time.After(blind):
			}
		}
		for {
			sample, err := poll()
			if err != nil {
				pollErr = err
				return
			}
			polls = append(polls, sample)
			select {
			case <-stop:
				return
			default:
			}
			if wait := xidPollInterval - time.Since(sample.sent); wait > 0 {
				time.Sleep(wait)
			}
		}
	}()
	// The capture ends however operation leaves: a t.Fatalf inside it exits through the deferred
	// calls alone. Left running, the poller would query a connection Measure's defer closes, the
	// tracer would go on recording, and the execution tracer would stay on, so the next Measure in
	// the process could not start its own (TestAStoppedMeasureLeavesTheNextItsOwnTrace). A
	// transaction the stop left open leaves its thread's clock open too, which the capture's end
	// drops (xidDropThreadClock). endCapture is deferred after conn.Close, so it runs first;
	// traced.stop is deferred inside the callback.
	endCapture := sync.OnceFunc(func() {
		close(stop)
		<-polled
		x.recording.Store(false)
		x.mu.Lock()
		for _, held := range x.locked {
			xidDropThreadClock(held.thread)
		}
		x.mu.Unlock()
	})
	defer endCapture()

	var traced *xidTrace
	var started, finished time.Time
	runWithIdlePs(func() {
		runWithoutCollector(func() {
			if XidJudgesGaps() {
				var err error
				if traced, err = startXidTrace(); err != nil {
					t.Fatalf("XID RIG PROBLEM (not a finding about the code): start the execution tracer, which the rig reads the runtime's own waits off: %v", err)
				}
				defer traced.stop()
			}
			started = time.Now()
			operation()
			finished = time.Now()
		})
	})
	endCapture()

	x.mu.Lock()
	spans := x.spans
	x.spans = nil
	x.mu.Unlock()
	statements := make([]XidStatement, 0, len(spans))
	for _, span := range spans {
		if span.done.Load() {
			statements = append(statements, span.statement)
		}
	}
	if pollErr != nil {
		t.Fatalf("xid rig: poll pg_stat_activity: %v", pollErr)
	}
	var reading *xidTraceReading
	if traced != nil {
		var err error
		if reading, err = traced.read(); err != nil {
			t.Fatalf("XID RIG PROBLEM (not a finding about the code): %v", err)
		}
	}
	return analyzeXidWindows(statements, polls, finished.Sub(started), reading)
}

// xidMeasuring makes Measures run one at a time. A Measure changes the whole process's GOMAXPROCS
// and collector for its operation, so two at once would put each other's settings back early. No
// test that measures calls t.Parallel, and Go runs a parallel test only beside other parallel
// ones, so no other test runs beside a Measure and this lock is never waited on; it makes sure
// no later one interleaves.
var xidMeasuring sync.Mutex

// xidIdlePs is how many Ps Measure adds to GOMAXPROCS for the operation. A goroutine locked to its
// thread, as a window's is, that comes back from a system call to find every P taken waits
// runnable on the runtime with its thread asleep until a P is handed to it (exitsyscall, then
// stoplockedm): a sleep of the runtime's that the thread's clocks cannot tell from a sleep of the
// code's (XidGap.Made), and Made counts it as the code's. With a P to spare, the system call's
// exit takes an idle one at once, and sysmon takes no P from a system call under 10 ms while one
// is idle (retake). In 1,000 forced runs at GOMAXPROCS 8 on a loaded machine (2026-11-21), 18
// gaps waited over 1 ms on the runtime, and one, which slept 8.3 ms through such a wait, broke D6
// behind 0.121 ms of the code's CPU. A P is idle whenever fewer goroutines want one than there
// are: in 238 operations measured with these Ps - 100 forced runs, the route table and the xid
// suites, sampled every 200 µs off runtime/metrics on the same day - at most 36 goroutines were
// running, runnable or in a system call at once, and 64 Ps, whatever the process had, are more
// than that. That premise is checked, not assumed: the trace says when no P was idle, and a window
// that keeps D6 only by taking out its goroutine's wait for a P then is UNKNOWN (xidHeldProblem),
// as is one whose operation's own goroutines take every P. Ps are cheap: each holds a cache of
// small objects, and an idle one runs nothing. A test that wants fewer Ps sets them inside its
// operation.
const xidIdlePs = 64

// runWithIdlePs runs operation with xidIdlePs more Ps than the process had, and puts GOMAXPROCS
// back after it on every way out: a deferred call runs on a panic and on t.Fatal's
// runtime.Goexit. Setting GOMAXPROCS stops the world, so it is done outside the operation, and no
// window holds that pause. It also stops the runtime from moving a default GOMAXPROCS to a CPU
// limit changed later, for the rest of the process, as each test that sets GOMAXPROCS already did.
func runWithIdlePs(operation func()) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(runtime.GOMAXPROCS(0) + xidIdlePs))
	operation()
}

// runWithoutCollector runs operation with the collector held off - no memory limit, and a GOGC
// of off, which waits for a cycle already marking to end - and puts both back after it. So no
// stop-the-world pause, stack scan or mark assist of the runtime's lands inside a window of the
// operation's: a pause takes as long as the slowest thread of the process takes to stop, which on
// a loaded machine is the load's doing, and beside these tests at a load of 400 on a 288-CPU
// machine (2026-11-19) one took 33 ms.
//
// With the collector off the heap only grows, so an allocation would land on a page the process
// has never touched; and a first touch that reads before it writes maps the kernel's shared zero
// page, which the write then copies, changing a mapping every CPU the process ran on may cache.
// The kernel waits for each of those CPUs to drop it, and on a loaded machine that wait is
// milliseconds of the thread's CPU: traced at a load of 400 to 500 on a 288-CPU machine
// (2026-11-20), 3.5 to 10 ms in the kernel's TLB shootdown behind a few microseconds of code. So
// one collection runs first, with the collector and the scavenger already off (both goals at
// their maximum, so nothing it frees is handed back to the system): the window allocates out of
// what that collection freed, memory the heap has used before, as it does with the collector on.
// Each marked BEGIN then holds the process's memory still (XidTracer.lockTransaction,
// xidHoldMemory), mappings the collection or an earlier window made included.
func runWithoutCollector(operation func()) {
	limit := debug.SetMemoryLimit(math.MaxInt64)
	defer debug.SetMemoryLimit(limit)
	percent := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(percent)
	runtime.GC()
	operation()
}
