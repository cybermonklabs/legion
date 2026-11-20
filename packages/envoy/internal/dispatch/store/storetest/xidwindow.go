package storetest

import (
	"context"
	"fmt"
	"math/rand/v2"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The xid window rig (INFRA-636) measures how long a write transaction holds its transaction id
// (xid) and what its backend does meanwhile. Every xid holds back the snapshot horizon of every
// reader until it ends, so the time a transaction spends with an xid and no statement running - Go
// doing document work or advice between two statements - is time the whole database waits on that
// Go code.
//
// Two sources are joined, both stamped with this process's monotonic clock:
//   - XidTracer, a pgx tracer on the pool under test, records each statement every backend runs:
//     its start, its end, its SQL and whether its own result says it wrote (xidAssigned, which
//     reads the command tag Postgres answered it with). pgx sends BEGIN and COMMIT as ordinary
//     statements, so the trace splits each backend's statements into its transactions.
//   - A poller on a connection of its own reads pg_stat_activity every xidPollInterval and records
//     which backends hold an xid. A statement's command tag does not say which statement assigned
//     the xid when the write was a trigger's or a function's; Postgres does.
//
// A transaction's xid window starts at the earlier of the two sources' answers: the last statement
// that began before the first poll that saw the backend hold an xid, and the first statement whose
// own result says it wrote (xidAssigned). It runs to the end of its COMMIT. A transaction the
// poller never saw - a write phase that is one batch can hold its xid for less than a poll
// interval - is judged from its statements alone; one that wrote nothing by either source holds
// no xid to judge (MayHoldNoXid). Inside the window the rig checks two things. Its shape, in
// every build: the window holds at most the statements the measurement declared (XidShape), and
// nothing else. And, where the build judges a window's time at all (XidJudgesGaps), D6 of the
// INFRA-636 plan: every statement-free gap is at most XidMaxGap, and the gaps together take no
// longer than the statements do.
//
// A gap is judged by the time the code made in it (XidGap.Made), not by the time it took on the
// clock. On a loaded machine the clock reads the machine: a thread kept waiting for a CPU, or a
// stop of the world the collector could not finish until the process's slowest thread stopped,
// opens a gap of milliseconds behind code that ran for microseconds (TestXidWindowsOnTheLargeSpec
// read 13 ms after one batch at a load of 660 on a 288-CPU machine). So the rig holds what it can
// still. The goroutine that begins a marked transaction is locked to its OS thread until the
// transaction ends, and that thread is kept out of NUMA balancing meanwhile
// (XidTracer.lockTransaction), so it sends every statement of the window and runs nothing else.
// The operation runs with the collector held off, after a collection that leaves the window
// memory the heap has already used, and with every page the process has mapped kept out of NUMA
// balancing (runWithoutCollector), so none of the runtime's pauses lands in a window, no
// allocation in it is the first touch of a page, and no page is moved under it. And the rig does
// its own work on a statement inside that statement, between its Start and its End, and none in
// the gaps around it (XidTracer.begin, XidTracer.end). Each gap is read off that thread's own
// scheduler clocks (readXidThreadClock), read at the very moments the gap's two ends are stamped,
// and off an execution trace of the operation (xidwindow_trace.go): the time the kernel kept the
// thread runnable on a run queue, and the time the Go runtime kept its goroutine runnable - after
// a preemption, a stop of the world, a system call that lost its P - are the machine's, and the
// rest - the time the thread ran, and the time the goroutine waited on something it asked for or
// sat in a blocking system call - is the code's. A sleep, a lock, a channel or a document call in
// the window is in that rest, and a machine that is merely busy is not.
//
// Which transactions are judged is decided by context, not guessed from timing: the caller marks
// the context its operation runs under (MarkXid) and the tracer marks every statement whose
// context carries it. A transaction is the operation's when any statement of it is marked, and
// then every statement its backend ran between BEGIN and COMMIT is counted, marked or not, since a
// connection serves only its transaction's holder until it commits. Background work of the server
// (settlement timers, the outbox, listeners) runs under contexts of its own, so it shares the pool
// but never the verdict.

// XidMaxGap is the longest statement-free gap D6 allows the code to make inside an xid window
// (XidGap.Made), in a build that judges a window's time at all (XidJudgesGaps).
const XidMaxGap = 5 * time.Millisecond

// XidFaultGap is the statement-free gap the rig's deliberate-fault self-tests inject behind the
// short statements of a rig transaction: four times XidMaxGap, and well under what a real
// document call inside a window costs, which leaves the docs package's control
// (TestXidWindowCatchesADocumentCallInsideTheWindow) as the test that goes red first if the bound
// is ever widened too far.
const XidFaultGap = 20 * time.Millisecond

// XidJudgesGaps reports whether this build judges how long a window's statement-free gaps are.
// A plain build judges them only when its thread-clock probe can read the calling thread's clocks.
// That probe is read once at run time (xidThreadClocksAvailable): Linux kernels do not all expose
// schedstat. A build with the race detector does not, and the build tag is the only thing that
// says so (raceDetector); where either makes gap judgment unavailable, the rig holds window shape
// alone, as the race build does.
//
// The reason for the race build is that under the race detector no bound can tell what it is
// meant to tell. The detector's instrumentation and the scheduling it forces leave a goroutine off
// the processor between two statements of a window nothing is wrong with, for as long as a slow Go
// call would: measured on this plan's two-core box, a scheduler stall of 92.683 ms behind
// 20.130 ms of statements (the issue patch window, now a step of the api package's
// TestXidWindowsOnTheLargeSpec, once in twenty race iterations), and on a GitHub runner 9.331 ms
// behind 27.368 ms (TestXidWindowReconcileTaskProgress, envoy-go-race-docs job 127074699024 of
// run 41975931479). A bound set above those stops catching the faults it exists for; one set
// below them reddens a required lane for every pull request on main. Neither a constant nor a
// share of the window's own statement time escapes that, because the stall and the fault are the
// same measurement.
//
// What holds under the race detector instead is the rule no scheduling can move, which every
// window test asserts for itself: the window holds the write batch and its COMMIT and nothing
// else. A document call put back inside a window that runs a statement of its own is caught
// there by that rule, in either build. One that spends its time in Go alone - the rig's own
// control, and its deliberate-fault self-tests, which inject a sleep - is caught only by a plain
// run. So in CI a race lane checks a window's shape alone, and the gap bound is judged in plain
// runs: the docs window tests run under -race in envoy-go-race-docs (shape) and without it in
// envoy-go-docs-finalizers (shape and gaps); the api and delivery window tests are
// `//go:build !race`, so no race lane runs them and envoy-go's step "Test what the race build
// cannot run at all or at full size" runs them plain (shape and gaps); refs' runs plain in
// envoy-go's unit step. envoy-go and envoy-go-docs-finalizers are among the slow suites the
// changes job holds back on a pull request: each runs on a merge group (its first attempt only),
// on a manual dispatch, and on a pull request only while it carries the `full-ci` label, and
// only when its filter (envoy_go, envoy_go_race_docs) selects the change. So the gap bound is
// judged in the merge queue before every merge, not on every pull-request push. Under the race
// detector the docs tests that inject a sleep skip their timing arm by name.
func XidJudgesGaps() bool { return XidNoGapJudgment() == "" }

// xidPollInterval is how often the poller reads pg_stat_activity: the plan's 0.5 ms. A poll's own
// round trip is of the same order, so in practice a backend is looked at about every millisecond.
const xidPollInterval = 500 * time.Microsecond

// xidSQLWidth is how much of a statement's SQL a report quotes.
const xidSQLWidth = 100

type xidMarkKey struct{}

type xidSpanKey struct{}

// MarkXid marks ctx as the operation an XidTracer.Measure judges: every statement run under it,
// or under a context derived from it, is the operation's.
func MarkXid(ctx context.Context) context.Context {
	return context.WithValue(ctx, xidMarkKey{}, true)
}

func markedXid(ctx context.Context) bool {
	marked, _ := ctx.Value(xidMarkKey{}).(bool)
	return marked
}

// XidStatement is one statement a backend ran, as the tracer saw it from the client: Start is
// when pgx began sending it and End when its result was read (for a query, when its rows were
// closed). A batch is one statement whose SQL joins the batch's queries.
type XidStatement struct {
	PID    uint32
	Start  time.Time
	End    time.Time
	SQL    string
	Marked bool
	// Wrote is true when the statement's own result says it took a transaction id (xidAssigned):
	// for a batch, when any query of it did.
	Wrote bool
	// started and ended are the scheduler clocks of the OS thread that sent the statement, read at
	// Start and at End, which are their stamps (readXidThreadClock); a clock the rig could not read
	// names no thread. locked is the OS thread the goroutine that began the statement's
	// transaction was locked to at its BEGIN (XidTracer.lockTransaction), zero when none was.
	started, ended xidThreadClock
	locked         int
}

// xidThreadClock is one reading of an OS thread's scheduler clocks: which thread, the moment of
// the reading (at), the CPU time it has run, and the time it has waited runnable on a run queue
// for a CPU, both as of that moment. A zero thread is a reading the rig could not take.
type xidThreadClock struct {
	thread int
	at     time.Time
	ran    time.Duration
	waited time.Duration
}

// stamp is the moment the reading was taken, or now when the rig could not take it.
func (c xidThreadClock) stamp() time.Time {
	if c.thread == 0 {
		return time.Now()
	}
	return c.at
}

// XidTracer is a pgx QueryTracer, BatchTracer and CopyFromTracer that records the statements each
// backend runs while Measure runs. Install it on a pool with Traced.
type XidTracer struct {
	mu sync.Mutex
	// recording is read at the start of every statement, before the statement's Start, so it is
	// read without the mutex: a statement waiting on it would wait inside the gap before it.
	recording atomic.Bool
	// spans are the statements begun while the tracer records, in the order they began. A span is
	// a statement of the trace once its end has filled it in (xidSpan.done).
	spans []*xidSpan
	// blind is how long Measure's poller waits before its first read of pg_stat_activity; zero
	// polls from the start (BlindPoller).
	blind time.Duration
	// locked is, for each backend whose marked transaction began while the tracer recorded and
	// has not ended, the OS thread the goroutine that began it is locked to and held on
	// (lockTransaction).
	locked map[uint32]xidHeldThread
}

// xidHeldThread is the OS thread a marked transaction's goroutine is locked to, and what lets it
// go as it was when the transaction ends: its memory policy put back and its clock's descriptor
// closed (release), nil where the rig could not hold it. Only a held thread's clocks are a gap's
// (XidStatement.locked). The thread is kept even where it is not held, since the goroutine that
// locked it must still unlock it (unlockTransaction).
type xidHeldThread struct {
	thread  int
	release func()
}

// gapThread is the thread a statement's gaps are read off: the locked thread when the rig holds
// it, and none when it does not.
func (h xidHeldThread) gapThread() int {
	if h.release == nil {
		return 0
	}
	return h.thread
}

// NewXidTracer returns a tracer that records nothing until Measure starts it.
func NewXidTracer() *XidTracer { return &XidTracer{} }

// BlindPoller makes the next Measure's poller wait d before its first read of pg_stat_activity, so
// a transaction that begins and commits inside d is never seen holding an xid. Only the rig's own
// self-tests use it, to prove that such a transaction is still judged from the window its own
// statements derive; a measurement of the code under test leaves it unset.
func (x *XidTracer) BlindPoller(d time.Duration) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.blind = d
}

type xidSpan struct {
	statement XidStatement
	sql       []string
	// ends is true for a COMMIT or a ROLLBACK, whose end lets its transaction's goroutine go
	// (unlockTransaction).
	ends bool
	// done is set once end has written the statement whole. Measure reads a span's statement only
	// after it sees done, and leaves out a span still in flight.
	done atomic.Bool
}

// begin opens a statement's span. The thread's clocks are read and Start is taken first, and the
// rig's own work on the statement - classifying it, locking and holding its transaction's thread,
// recording the span - comes after, inside the statement. A gap is the code's (XidGap.Made), and
// before this order the rig's work ran in it: on a loaded machine its allocations' first touches
// of fresh pages took 3.5 to 10 ms of the thread's CPU in the kernel (runWithoutCollector),
// which the gap's clocks read as the code's.
func (x *XidTracer) begin(ctx context.Context, conn *pgx.Conn, sql string) context.Context {
	if !x.recording.Load() {
		if verb := xidVerb(sql); verb == "commit" || verb == "rollback" {
			x.unlockTransaction(conn.PgConn().PID())
		}
		return ctx
	}
	started := readXidThreadClock()
	start := started.stamp()
	pid := conn.PgConn().PID()
	verb := xidVerb(sql)
	marked := markedXid(ctx)
	if verb == "begin" && marked {
		x.lockTransaction(pid)
	}
	span := &xidSpan{ends: verb == "commit" || verb == "rollback", statement: XidStatement{
		PID: pid, Start: start, SQL: sql, Marked: marked, started: started,
	}}
	x.mu.Lock()
	span.statement.locked = x.locked[pid].gapThread()
	x.spans = append(x.spans, span)
	x.mu.Unlock()
	return context.WithValue(ctx, xidSpanKey{}, span)
}

// end closes the span ctx carries with tag, the command tag the statement answered - the zero tag
// for a batch, whose queries report their own (TraceBatchQuery). What the rig works out about the
// statement - its batch's text, whether it wrote - is worked out before its End, inside it, as
// begin's work is after its Start; after End the rig only reads the thread's clocks and marks the
// span done, so the gap that follows holds the code's work and not the rig's.
func (x *XidTracer) end(ctx context.Context, tag pgconn.CommandTag) {
	span, ok := ctx.Value(xidSpanKey{}).(*xidSpan)
	if !ok {
		return
	}
	if len(span.sql) > 0 {
		span.statement.SQL = strings.Join(span.sql, "; ")
	}
	span.statement.Wrote = span.statement.Wrote || xidAssigned(span.statement.SQL, tag)
	span.statement.ended = readXidThreadClock()
	span.statement.End = span.statement.ended.stamp()
	span.done.Store(true)
	if span.ends {
		x.unlockTransaction(span.statement.PID)
	}
}

// lockTransaction locks the calling goroutine, which is sending the BEGIN of a marked
// transaction on backend pid, to its OS thread until that transaction's COMMIT or ROLLBACK
// (unlockTransaction), holds that thread out of NUMA balancing meanwhile (xidHoldThread), and
// opens the thread's clock for the transaction's readings (xidWatchThreadClock). A thread locked
// to a goroutine runs that goroutine and no other, so every gap between two statements the
// goroutine sends from it is read off clocks of its own: the time the thread ran, and waited for a
// CPU, is the goroutine's. It is the BEGIN that locks, not the measured operation, because an
// operation sends a transaction from any goroutine it starts - a broadcast delivers to each
// session on a goroutine of its own. A thread the rig could not hold is not recorded as held, so
// its gaps are read off no thread's clocks and are a rig problem (analyzeXidWindows).
func (x *XidTracer) lockTransaction(pid uint32) {
	runtime.LockOSThread()
	held := xidHeldThread{thread: xidThread()}
	if release, ok := xidHoldThread(); !ok {
		release()
	} else if unwatch, ok := xidWatchThreadClock(); !ok {
		release()
	} else {
		held.release = func() {
			unwatch()
			release()
		}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.locked == nil {
		x.locked = map[uint32]xidHeldThread{}
	}
	x.locked[pid] = held
}

// unlockTransaction puts back and lets go of the thread lockTransaction locked for backend pid's
// transaction, when the calling goroutine is the one it locked: a transaction that ended on
// another goroutine leaves the goroutine that began it locked, which the runtime then ends its
// thread with.
func (x *XidTracer) unlockTransaction(pid uint32) {
	x.mu.Lock()
	held, ok := x.locked[pid]
	mine := ok && held.thread == xidThread()
	if mine {
		delete(x.locked, pid)
	}
	x.mu.Unlock()
	if mine {
		if held.release != nil {
			held.release()
		}
		runtime.UnlockOSThread()
	}
}

// TraceQueryStart implements pgx.QueryTracer.
func (x *XidTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return x.begin(ctx, conn, data.SQL)
}

// TraceQueryEnd implements pgx.QueryTracer.
func (x *XidTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	x.end(ctx, data.CommandTag)
}

// TraceBatchStart implements pgx.BatchTracer.
func (x *XidTracer) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	ctx = x.begin(ctx, conn, "batch")
	if span, ok := ctx.Value(xidSpanKey{}).(*xidSpan); ok && data.Batch != nil {
		for _, queued := range data.Batch.QueuedQueries {
			span.sql = append(span.sql, queued.SQL)
		}
	}
	return ctx
}

// TraceBatchQuery implements pgx.BatchTracer. A batch is one statement of the trace, and it wrote
// when any query of it did: the first of them to write is what gave the transaction its xid, and
// the batch's own start is when that query was sent.
func (x *XidTracer) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if span, ok := ctx.Value(xidSpanKey{}).(*xidSpan); ok && xidAssigned(data.SQL, data.CommandTag) {
		span.statement.Wrote = true
	}
}

// TraceBatchEnd implements pgx.BatchTracer.
func (x *XidTracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchEndData) {
	x.end(ctx, pgconn.CommandTag{})
}

// TraceCopyFromStart implements pgx.CopyFromTracer.
func (x *XidTracer) TraceCopyFromStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceCopyFromStartData) context.Context {
	return x.begin(ctx, conn, "copy "+data.TableName.Sanitize())
}

// TraceCopyFromEnd implements pgx.CopyFromTracer.
func (x *XidTracer) TraceCopyFromEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromEndData) {
	x.end(ctx, data.CommandTag)
}

// XidTransaction is one transaction of the measured operation: every statement its backend ran
// from BEGIN through COMMIT.
type XidTransaction struct {
	PID        uint32
	Statements []XidStatement
	// Committed is false for a transaction that rolled back or had not ended when the operation
	// returned; neither is judged.
	Committed bool
	// Sighted is when the first poll that saw the backend hold an xid answered; zero when no poll
	// did.
	Sighted time.Time
	// Unseen is when the last poll before that one, which saw no xid, was sent; zero when no poll
	// sent after BEGIN preceded the sighting. The xid was assigned between the two.
	Unseen time.Time
}

// XidWindow is the part of a transaction during which it held its xid, as D6 judges it.
type XidWindow struct {
	Transaction XidTransaction
	// Statements are the transaction's statements from the window's first through COMMIT.
	Statements []XidStatement
	Start, End time.Time
	// StatementTime is the time the window's statements took less the machine's part of it, read as
	// a gap is (XidStatement.span), so rule 2 sets the code's gaps against the code's statements and
	// a loaded machine stretches neither.
	StatementTime time.Duration
	// Gaps are the gaps between Statements, Gaps[i] following Statements[i]. GapTime and MaxGap
	// are of the time the code made in them (XidGap.Made).
	Gaps    []XidGap
	GapTime time.Duration
	MaxGap  time.Duration
	// MaxGapAfter indexes Statements: the biggest gap follows Statements[MaxGapAfter].
	MaxGapAfter int
}

// XidGap is a stretch of a window - the time between two statements, or a statement's own - from
// one end to the other on this process's clock (Clock), read as well off the scheduler clocks of
// the thread that sent it (XidTracer.lockTransaction) - the CPU time it ran meanwhile (Ran) and the
// time it waited runnable on a run queue for a CPU (Runnable) - and off the execution trace of the
// goroutine that sent it: the time the Go runtime kept it runnable but not running (GoRunnable),
// the time it waited on something it asked for (GoWaiting), the time it spent in a system call
// (GoSyscall), and how much of its runnable time its thread spent woken but on the run queue after
// another thread handed it a P (Overlap, counted in both Runnable and GoRunnable). Read is false
// when the rig could not read the thread's clocks at both ends on the thread its transaction's
// goroutine was locked to, and Traced is false when the trace does not show that goroutine.
type XidGap struct {
	Clock      time.Duration
	Ran        time.Duration
	Runnable   time.Duration
	GoRunnable time.Duration
	GoWaiting  time.Duration
	GoSyscall  time.Duration
	Overlap    time.Duration
	Read       bool
	Traced     bool
}

// xidGoStates is the time a goroutine spent over a stretch of the trace in each state the rig
// reads, and the part of its runnable time its locked thread spent on the run queue after a
// handoff (xidTraceReading.goStates).
type xidGoStates struct {
	Runnable, Waiting, Syscall, Handoff time.Duration
}

// Made is the time the code made in the gap. The machine's part of a gap is the time its thread
// waited runnable for a CPU (Runnable) and the time the Go runtime kept its goroutine from running
// (GoRunnable: a preemption, a stop of the world, a syscall that lost its P), counted once where
// they overlap: a goroutine handed back to its locked thread stays runnable until that thread,
// woken, gets a CPU, and that wait is in both (Overlap). The rest is the code's: the time the
// thread ran, and the time it was off a CPU of its own accord - asleep, in a system call, or
// waiting on a lock, a channel or the network. Made never reads less than the CPU the thread ran
// and the waits the goroutine asked for (GoWaiting), nor less than its waits and its system calls
// beyond the run queue's part of them - the code's whatever the machine does - and never more than
// the gap's own time on the clock. A gap whose clocks were not read (Read) is its time on the
// clock.
func (g XidGap) Made() time.Duration {
	if !g.Read {
		return g.Clock
	}
	queued := g.Runnable - g.Overlap
	return min(g.Clock, max(g.Clock-g.GoRunnable-queued, g.Ran+g.GoWaiting, g.GoWaiting+g.GoSyscall-queued))
}

// xidStretch reads the stretch from to to off thread's clocks, from's reading and to's, where both
// were read on it, and off the execution trace of the goroutine running on that thread, where the
// rig took one (reading).
func xidStretch(from, to time.Time, fromClock, toClock xidThreadClock, thread int, reading *xidTraceReading) XidGap {
	span := XidGap{Clock: max(to.Sub(from), 0)}
	if thread != 0 && fromClock.thread == thread && toClock.thread == thread {
		span.Read = true
		span.Ran = toClock.ran - fromClock.ran
		span.Runnable = toClock.waited - fromClock.waited
		if reading != nil {
			var states xidGoStates
			states, span.Traced = reading.goStates(thread, from, to)
			span.GoRunnable, span.GoWaiting, span.GoSyscall = states.Runnable, states.Waiting, states.Syscall
			span.Overlap = min(states.Handoff, span.Runnable, span.GoRunnable)
		}
	}
	return span
}

// xidGapBetween is the gap from before's end to after's start, read where both were sent from the
// thread their transaction's goroutine was locked to (xidStretch).
func xidGapBetween(before, after XidStatement, reading *xidTraceReading) XidGap {
	thread := before.locked
	if after.locked != thread {
		thread = 0
	}
	return xidStretch(before.End, after.Start, before.ended, after.started, thread, reading)
}

// span is the statement's own stretch, from its Start to its End, read as a gap is (xidStretch).
func (s XidStatement) span(reading *xidTraceReading) XidGap {
	return xidStretch(s.Start, s.End, s.started, s.ended, s.locked, reading)
}

// Ran is the CPU time the statement's thread ran from its Start to its End, zero where the rig did
// not read the thread's clocks at both.
func (s XidStatement) Ran() time.Duration {
	if s.locked == 0 || s.started.thread != s.locked || s.ended.thread != s.locked {
		return 0
	}
	return s.ended.ran - s.started.ran
}

// Length is the window's span, its first statement's start to its COMMIT's end.
func (w XidWindow) Length() time.Duration { return w.End.Sub(w.Start) }

// Violations are the D6 rules the window breaks, empty when it keeps them. Both of them are
// readings of the time the code made in the window's gaps (XidGap.Made), so a build that judges
// none (XidJudgesGaps) breaks none here, and what holds there is the shape each window test
// asserts of its own window.
func (w XidWindow) Violations() []string {
	if !XidJudgesGaps() {
		return nil
	}
	var broken []string
	if w.MaxGap > XidMaxGap {
		broken = append(broken, fmt.Sprintf("a statement-free gap of %s (over %s) follows statement %d: %s",
			XidMillis(w.MaxGap), XidMillis(XidMaxGap), w.MaxGapAfter, xidSQL(w.Statements[w.MaxGapAfter].SQL)))
	}
	if w.GapTime > w.StatementTime {
		broken = append(broken, fmt.Sprintf("its gaps take %s, more than its statements' %s",
			XidMillis(w.GapTime), XidMillis(w.StatementTime)))
	}
	return broken
}

// Summary is the window in one line: what a passing run logs and a red run quotes.
func (w XidWindow) Summary() string {
	var largest XidGap
	if len(w.Gaps) > 0 {
		largest = w.Gaps[w.MaxGapAfter]
	}
	return fmt.Sprintf("backend %d: xid window %s, %d statements taking %s, gaps %s, largest gap %s after statement %d (%s on the clock, %s of it waiting for a CPU) (%s)",
		w.Transaction.PID, XidMillis(w.Length()), len(w.Statements), XidMillis(w.StatementTime), XidMillis(w.GapTime),
		XidMillis(w.MaxGap), w.MaxGapAfter, XidMillis(largest.Clock), XidMillis(largest.Runnable), xidSQL(w.Statements[w.MaxGapAfter].SQL))
}

// Detail lists every statement of the window and every gap between them, times relative to the
// window's start, so a red run shows where the time with the xid went.
func (w XidWindow) Detail() string {
	var b strings.Builder
	begin := w.Transaction.Statements[0].Start
	unseen := "none"
	if !w.Transaction.Unseen.IsZero() {
		unseen = XidMillis(w.Transaction.Unseen.Sub(begin))
	}
	sighted := "never (the window is its own statements')"
	if !w.Transaction.Sighted.IsZero() {
		sighted = XidMillis(w.Transaction.Sighted.Sub(begin))
	}
	fmt.Fprintf(&b, "  transaction began at 0.000 ms; last poll without its xid sent at %s; xid first seen at %s; window %s -> %s\n",
		unseen, sighted, XidMillis(w.Start.Sub(begin)), XidMillis(w.End.Sub(begin)))
	for i, statement := range w.Statements {
		if i > 0 {
			gap := w.Gaps[i-1]
			flag := ""
			if XidJudgesGaps() && gap.Made() > XidMaxGap {
				flag = "  <-- over " + XidMillis(XidMaxGap)
			}
			read := "its thread's clocks unread"
			if gap.Read {
				read = fmt.Sprintf("ran %s, off a CPU %s, waiting for one %s; its goroutine waited %s on the runtime (%s of it on the run queue after a handoff), %s on what it asked for and %s in system calls",
					XidMillis(gap.Ran), XidMillis(max(gap.Clock-gap.Runnable-gap.Ran, 0)), XidMillis(gap.Runnable),
					XidMillis(gap.GoRunnable), XidMillis(gap.Overlap), XidMillis(gap.GoWaiting), XidMillis(gap.GoSyscall))
			}
			fmt.Fprintf(&b, "      gap %10s  (%s on the clock: %s)%s\n", XidMillis(gap.Made()), XidMillis(gap.Clock), read, flag)
		}
		fmt.Fprintf(&b, "  %3d %10s +%9s  %s\n", i, XidMillis(statement.Start.Sub(w.Start)),
			XidMillis(statement.End.Sub(statement.Start)), xidSQL(statement.SQL))
	}
	return b.String()
}

// XidReport is what one Measure saw.
type XidReport struct {
	// Windows are the operation's committed transactions that held an xid, in commit order: each
	// one a poll saw hold it, or whose own statements show it wrote (XidStatement.Wrote).
	Windows []XidWindow
	// Unsighted are the operation's committed transactions no poll saw hold an xid and whose
	// statements show no write of their own: nothing says they held one.
	Unsighted []XidTransaction
	// Unjudged are the operation's transactions that rolled back or were still open when the
	// operation returned.
	Unjudged []XidTransaction
	// Problems are the reasons the rig cannot judge the operation: its verdict would be about the
	// rig, not the code under test.
	Problems []string
	// MayHoldNoXid lets an operation that may write nothing - a read under an advisory lock, a
	// write whose statements all matched no row - pass with no window at all. Unset, a report with
	// no window is a rig problem: the operation wrote nothing a statement's result showed and no
	// poll saw it hold an xid, so either it made no write or the wrong transaction was marked.
	MayHoldNoXid bool
	Duration     time.Duration
	Polls        int
	// LongestPoll is the longest time between two polls' sends, how blind the poller was at its
	// worst.
	LongestPoll time.Duration
}

// xidWriteSQL is a statement that may assign an xid. It never decides that a transaction held one:
// it picks the window's first statement among those the polls leave possible (xidWindowOf). What a
// statement did assign is read off its result instead (xidAssigned).
var xidWriteSQL = regexp.MustCompile(`(?is)\b(insert\s+into|update\s+\S+\s+set|delete\s+from)\b|\bfor\s+(no\s+key\s+)?update\b|\bfor\s+(key\s+)?share\b|pg_current_xact_id\(\)`)

// XidAssigningSQL reports whether a statement may give its transaction a transaction id, read off
// its SQL alone (xidWriteSQL): an insert, update or delete, a select that takes a row lock, and a
// call of pg_current_xact_id(). It is exported for the guard of a transaction this rig measures
// nothing of - a room append, whose BEGIN and COMMIT go inside pipelines, so no transaction of it
// is seen here at all (docs.appendXidWindowFailure) - so that guard and this rig judge a statement by the
// one rule. A guard over queued statements has the SQL and nothing else: a statement queued into a
// pipeline is recorded before it runs, so no command tag says yet whether it touched a row, which
// is what xidAssigned reads for the statements this rig does see.
func XidAssigningSQL(sql string) bool { return xidWriteSQL.MatchString(sql) }

// xidRowLockSQL is a select that takes a row lock, which Postgres gives the transaction an xid for
// as it does a write.
var xidRowLockSQL = regexp.MustCompile(`(?is)\bfor\s+(no\s+key\s+update|update|key\s+share|share)\b`)

// xidForcedSQL is a statement that asks Postgres for the transaction's xid outright, which assigns
// one.
var xidForcedSQL = regexp.MustCompile(`(?i)pg_current_xact_id\(\)`)

// xidAssigned reports whether a statement's own result says it took a transaction id: an insert,
// update or delete that touched a row, a merge, a select that locked one, a copy that wrote one,
// or a call of pg_current_xact_id(). Postgres assigns an xid when a statement first changes or
// locks a row, so a statement that touched none - an update whose where clause matched nothing -
// took none, and the command tag's row count is what says so. The tag counts the statement's own
// rows and not a trigger's, so a statement that wrote nothing itself and whose trigger wrote is
// left to the poller, which sees an xid whichever write assigned it.
func xidAssigned(sql string, tag pgconn.CommandTag) bool {
	if xidForcedSQL.MatchString(sql) {
		return true
	}
	if strings.HasPrefix(tag.String(), "MERGE") {
		return true
	}
	switch {
	case tag.Insert(), tag.Update(), tag.Delete(), strings.HasPrefix(tag.String(), "COPY"):
		return tag.RowsAffected() > 0
	case tag.Select():
		return tag.RowsAffected() > 0 && xidRowLockSQL.MatchString(sql)
	}
	return false
}

// analyzeXidWindows splits statements into the operation's transactions and judges their windows.
// A window gap not read off its transaction's locked thread (XidGap.Read) is a rig problem where
// the build judges gaps.
func analyzeXidWindows(statements []XidStatement, polls []xidPoll, duration time.Duration, reading *xidTraceReading) *XidReport {
	report := &XidReport{Duration: duration, Polls: len(polls)}
	for i := 1; i < len(polls); i++ {
		report.LongestPoll = max(report.LongestPoll, polls[i].sent.Sub(polls[i-1].sent))
	}
	byBackend := map[uint32][]XidStatement{}
	for _, statement := range statements {
		byBackend[statement.PID] = append(byBackend[statement.PID], statement)
	}
	var transactions []XidTransaction
	for pid, run := range byBackend {
		slices.SortFunc(run, func(a, b XidStatement) int { return a.Start.Compare(b.Start) })
		var open *XidTransaction
		for _, statement := range run {
			switch xidVerb(statement.SQL) {
			case "begin":
				if open != nil {
					transactions = append(transactions, *open)
				}
				open = &XidTransaction{PID: pid, Statements: []XidStatement{statement}}
			case "commit", "rollback":
				if open == nil {
					continue
				}
				open.Statements = append(open.Statements, statement)
				open.Committed = xidVerb(statement.SQL) == "commit"
				transactions = append(transactions, *open)
				open = nil
			default:
				if open != nil {
					open.Statements = append(open.Statements, statement)
				}
			}
		}
		if open != nil {
			transactions = append(transactions, *open)
		}
	}
	slices.SortFunc(transactions, func(a, b XidTransaction) int {
		return a.Statements[len(a.Statements)-1].End.Compare(b.Statements[len(b.Statements)-1].End)
	})
	for _, transaction := range transactions {
		if !slices.ContainsFunc(transaction.Statements, func(s XidStatement) bool { return s.Marked }) {
			continue
		}
		if !transaction.Committed {
			report.Unjudged = append(report.Unjudged, transaction)
			continue
		}
		begin := transaction.Statements[0].Start
		end := transaction.Statements[len(transaction.Statements)-1].End
		// A poll sent before BEGIN started could still be showing the xid of the transaction the
		// same connection ran before this one, so only a poll sent after it counts.
		for _, sample := range polls {
			if sample.sent.Before(begin) || sample.received.After(end) {
				continue
			}
			if slices.Contains(sample.holders, transaction.PID) {
				transaction.Sighted = sample.received
				break
			}
			transaction.Unseen = sample.sent
		}
		// A transaction whose statements show no write of their own and that no poll saw took no
		// xid to judge: an operation that may write nothing passes on MayHoldNoXid, and one that
		// was to write is the report's own rig problem (problems).
		if transaction.Sighted.IsZero() && xidFirstWrote(transaction) < 0 {
			report.Unsighted = append(report.Unsighted, transaction)
			continue
		}
		window := xidWindowOf(transaction, reading)
		if XidJudgesGaps() {
			for i, gap := range window.Gaps {
				switch {
				case !gap.Read:
					report.Problems = append(report.Problems, fmt.Sprintf(
						"the gap after statement %d of backend %d's window (%s) was not read off one thread's clocks on both sides: the transaction's goroutine was not locked to its thread from its BEGIN (a BEGIN the operation's marked context did not send), the rig could not hold that thread out of NUMA balancing (xidHoldThread), or another goroutine sent its statements",
						i, transaction.PID, xidSQL(window.Statements[i].SQL)))
				case !gap.Traced:
					report.Problems = append(report.Problems, fmt.Sprintf(
						"the gap after statement %d of backend %d's window (%s) was not read off the execution trace: the trace shows no goroutine running on the transaction's thread by the gap's start",
						i, transaction.PID, xidSQL(window.Statements[i].SQL)))
				}
			}
		}
		report.Windows = append(report.Windows, window)
	}
	return report
}

// xidWindowOf cuts transaction at its xid window and measures it. The window starts at the earlier
// of what the two sources say, and runs to the end of COMMIT.
//
// The polls say: the plan's rule starts the window at the last statement that began before the
// first poll that saw the xid answered. That poll answers a round trip after its snapshot, and on
// a busy box later still, so a statement or two may have begun meanwhile; the statement that
// assigned the xid is one that was still running after the last poll that saw none was sent. When
// a statement in that blind spot may assign an xid (xidWriteSQL), the window starts at the first
// such one instead, so a fast statement after the assigning one cannot hide the time between them.
// The polls bound that choice: text only picks among the statements they leave possible.
//
// The statements say, exactly: the first one whose own result shows it wrote rows (XidStatement
// .Wrote) held an xid from its start, whatever the polls saw. A write phase that is one batch can
// hold its xid for less than a poll interval, so a transaction the poller never saw is judged from
// this alone; a transaction it did see is judged from whichever source names the earlier statement,
// since both name a statement that already held the xid.
func xidWindowOf(transaction XidTransaction, reading *xidTraceReading) XidWindow {
	first := 0
	if !transaction.Sighted.IsZero() {
		for i, statement := range transaction.Statements {
			if statement.Start.Before(transaction.Sighted) {
				first = i
			}
		}
		unseen := transaction.Unseen
		if unseen.IsZero() {
			unseen = transaction.Statements[0].Start
		}
		for i, statement := range transaction.Statements[1:max(first, 1)] {
			if statement.End.After(unseen) && xidWriteSQL.MatchString(statement.SQL) {
				first = i + 1
				break
			}
		}
	}
	if wrote := xidFirstWrote(transaction); wrote >= 0 && (transaction.Sighted.IsZero() || wrote < first) {
		first = wrote
	}
	window := XidWindow{Transaction: transaction, Statements: transaction.Statements[first:]}
	window.Start = window.Statements[0].Start
	window.End = window.Statements[len(window.Statements)-1].End
	for i, statement := range window.Statements {
		window.StatementTime += statement.span(reading).Made()
		if i == 0 {
			continue
		}
		measured := xidGapBetween(window.Statements[i-1], statement, reading)
		window.Gaps = append(window.Gaps, measured)
		gap := measured.Made()
		window.GapTime += gap
		if gap > window.MaxGap {
			window.MaxGap = gap
			window.MaxGapAfter = i - 1
		}
	}
	return window
}

// xidFirstWrote indexes the transaction's first statement whose own result shows it wrote rows,
// and so held an xid from its start (XidStatement.Wrote); -1 when none did.
func xidFirstWrote(transaction XidTransaction) int {
	return slices.IndexFunc(transaction.Statements, func(s XidStatement) bool { return s.Wrote })
}

// xidVerb classifies a statement as one that opens or ends a transaction. A savepoint's "rollback
// to savepoint" ends nothing.
func xidVerb(sql string) string {
	verb := strings.ToLower(strings.TrimSpace(sql))
	switch {
	case strings.HasPrefix(verb, "begin"):
		return "begin"
	case verb == "commit":
		return "commit"
	case verb == "rollback":
		return "rollback"
	}
	return ""
}

func xidSQL(sql string) string {
	sql = strings.Join(strings.Fields(sql), " ")
	if len(sql) > xidSQLWidth {
		sql = sql[:xidSQLWidth]
	}
	return sql
}

// XidMillis formats d as the rig's reports do: milliseconds to three places.
func XidMillis(d time.Duration) string {
	return fmt.Sprintf("%.3f ms", float64(d)/float64(time.Millisecond))
}

// problems are the report's rig problems: Problems, and a report that judged no window when the
// operation was to hold an xid.
func (r *XidReport) problems() []string {
	problems := r.Problems
	if len(r.Windows) == 0 && !r.MayHoldNoXid {
		problems = append(slices.Clone(problems), fmt.Sprintf(
			"no transaction of the operation held an xid: none wrote a row a statement's result showed and no poll saw one hold an xid (%d polls, %d unsighted, %d unjudged transactions); the rig judged nothing",
			r.Polls, len(r.Unsighted), len(r.Unjudged)))
	}
	return problems
}

// Err is the report's verdict: nil when every window keeps D6 and the rig judged one (or the
// operation MayHoldNoXid). A rig problem comes first, worded so a red run cannot be mistaken for a
// finding.
func (r *XidReport) Err() error {
	if problems := r.problems(); len(problems) > 0 {
		return fmt.Errorf("XID RIG PROBLEM (not a finding about the code): %s", strings.Join(problems, "; "))
	}
	var b strings.Builder
	for _, window := range r.Windows {
		if broken := window.Violations(); len(broken) > 0 {
			fmt.Fprintf(&b, "XID WINDOW VIOLATION (INFRA-636 D6) %s\n  %s\n%s", window.Summary(), strings.Join(broken, "\n  "), window.Detail())
		}
	}
	if b.Len() > 0 {
		return fmt.Errorf("%s", b.String())
	}
	return nil
}

// XidShape is what the windows of a measured operation must hold, which every measurement
// declares: the most statements one of its windows may run while it holds its transaction id,
// and what those statements are, for a failure to quote. A count of statements is the one
// reading of a window no scheduling can move, so the rig holds every window to it in every
// build - it is the whole of what a race build judges (XidJudgesGaps), and it stands beside D6's
// bound in a plain one.
//
// The declaration is a parameter rather than a default so that a window test cannot be written
// without it: a measurement that declares no shape does not compile, and one that declares an
// empty shape fails as a rig problem.
type XidShape struct {
	// Statements is the most statements one window may hold.
	Statements int
	// Want names them as the failure quotes them.
	Want string
}

// XidBatchedWindow is the shape of a route whose write phase is one pipelined batch: that batch
// and the COMMIT ending it, and nothing else. It is what the plan asks of every route that
// queues its writes.
var XidBatchedWindow = XidShape{Statements: 2, Want: "the write phase's one batch and its COMMIT"}

// XidWindowOf is the shape of an operation whose window holds statements of its own beside that
// commit - a route that writes a row inline, or a background pass that writes one row per item -
// with statements the most it may hold and want naming them.
func XidWindowOf(statements int, want string) XidShape {
	return XidShape{Statements: statements, Want: want}
}

// AssertXidWindows logs every window the report judged, so a run quotes its window length and
// largest gap whichever way it goes, and in a build that judges no gap says why, once, so a run
// that judged shape alone does not read as one that judged the timing; then it fails t when the
// rig could not judge the operation, a window holds more than shape allows, or a window breaks D6
// in a build that judges a window's time.
func AssertXidWindows(t testing.TB, report *XidReport, shape XidShape) {
	t.Helper()
	if reason := XidNoGapJudgment(); reason != "" {
		t.Logf("xid rig: this run judges window shape alone, not statement-free gaps: %s", reason)
	}
	t.Logf("xid rig: operation took %s, %d polls, longest poll interval %s, %d window(s), %d committed transaction(s) never seen with an xid, %d unjudged",
		XidMillis(report.Duration), report.Polls, XidMillis(report.LongestPoll), len(report.Windows), len(report.Unsighted), len(report.Unjudged))
	for _, window := range report.Windows {
		t.Logf("xid rig: %s", window.Summary())
	}
	if shape.Statements < 1 || shape.Want == "" {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): the measurement declared no window shape (%+v)", shape)
	}
	for _, window := range report.Windows {
		if len(window.Statements) > shape.Statements {
			t.Errorf("XID WINDOW VIOLATION (INFRA-636 D6) %s\n  the transaction ran %d statements inside its xid window, want at most %d, %s: a read it runs there holds back every reader's snapshot\n%s",
				window.Summary(), len(window.Statements), shape.Statements, shape.Want, window.Detail())
		}
	}
	if err := report.Err(); err != nil {
		if len(report.problems()) > 0 {
			t.Fatal(err)
		}
		t.Error(err)
	}
}

// xidWords is the load harness's vocabulary (packages/envoy/loadtest/seed.go), so the fixture
// reads like the documents the load reproduction measured.
var xidWords = strings.Fields(`the outbox publishes each committed event to its issue topic and every session that
watches the issue receives it within a second while the listener keeps interests in a key value bucket
dispatch reads issues documents comments and asks from postgres through a shared pool of connections
a reviewer anchors a comment to a line of the specification and the author answers with a new version
latency throughput memory goroutines profile scaling knee saturation connection queue lock contention`)

func xidSentence(r *rand.Rand, n int) string {
	words := make([]string, n)
	for i := range words {
		words[i] = xidWords[r.IntN(len(xidWords))]
	}
	return strings.Join(words, " ")
}

// XidSpec is the xid window tests' fixture document: a table-heavy spec of about 40 KB, built as
// the load harness builds its large specs (specMarkdown) - sections of prose, each with an
// eighteen-row, six-column table whose every row carries a unique requirement cell an anchor can
// quote - with one "Revision token r0" line an edit can bump. It answers the markdown and its
// requirement cells in document order; the same seed gives the same document.
func XidSpec(project string, seed uint64) (string, []string) {
	r := rand.New(rand.NewPCG(seed, 636))
	var b strings.Builder
	var quotes []string
	fmt.Fprintf(&b, "# Spec for %s\n\nRevision token r0\n\n", project)
	row, section := 0, 0
	for b.Len() < 40000 {
		section++
		fmt.Fprintf(&b, "## Section %d\n\n%s.\n\n%s.\n\n", section, xidSentence(r, 40), xidSentence(r, 30))
		b.WriteString("| Id | Requirement | Owner | Status | Evidence | Notes |\n|---|---|---|---|---|---|\n")
		for range 18 {
			row++
			quote := fmt.Sprintf("Requirement %s-R%d holds", project, row)
			quotes = append(quotes, quote)
			fmt.Fprintf(&b, "| R%d | %s | team-%d | open | %s | %s |\n", row, quote, r.IntN(9), xidSentence(r, 6), xidSentence(r, 5))
		}
		b.WriteString("\n")
	}
	return b.String(), quotes
}
