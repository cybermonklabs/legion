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
// interval - is judged from its statements alone: from the first whose result says it wrote, or
// from an earlier one whose result cannot say, a select whose common table expressions write
// (xidWriteUnreported), since Postgres answers it with the select's own tag. One that wrote
// nothing by either source holds no xid to judge (MayHoldNoXid). Inside the window the rig checks
// two things. Its shape, in every build: the window holds at most the statements the measurement
// declared (XidShape), and nothing else. And, where the build judges a window's time at all
// (XidJudgesGaps), the rig's rule D6: every statement-free gap is at most its bound
// (XidGap.Bound), and the gaps together take no longer than the statements do and the time their
// CPUs' device softirqs may hold. A gap's bound is XidMaxGap, widened, on a kernel that charges
// interrupt time to the thread it lands on, by the time the device softirqs its CPU ran may have
// put on its thread (XidGap.SoftirqHold); a window's gaps hold at most XidSoftirqGapHold together
// for rule 2. Both widenings come from one measurement (xidwindow_softirq.go).
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
// memory the heap has already used, and each marked BEGIN keeps every page the process has mapped
// out of NUMA balancing and makes the execution tracer's pages present (XidTracer.lockTransaction,
// xidHoldWindowMemory), so none of the runtime's pauses lands in a window, no allocation of the
// code's and no event of the tracer's in it is the first touch of a page, and no page is moved
// under it; a window whose BEGIN could not hold the memory is UNKNOWN, naming why.
// And the rig does its own work on a statement inside that statement, between its Start and its
// End, and none in the gaps around it (XidTracer.begin, XidTracer.end). Each gap is read off that
// thread's own scheduler clocks (readXidThreadClock), read at the very moments the gap's two ends
// are stamped, and off an execution trace of the operation (xidwindow_trace.go): the time the
// kernel kept the thread runnable on a run queue, and the time the Go runtime kept its goroutine
// runnable - after a preemption, a stop of the world, a system call that lost its P - are the
// machine's, and the rest - the time the thread ran, and the time the goroutine waited on
// something it asked for or sat in a blocking system call - is the code's. A sleep, a lock, a
// channel or a document call in the window is in that rest, and a machine that is merely busy is
// not; on a kernel that charges interrupt time to the thread it lands on, up to
// XidSoftirqGapHold of a gap's CPU time beside a counted device softirq is not counted against
// the code (XidGap.SoftirqHold).
//
// Which transactions are judged is decided by context, not guessed from timing: the caller marks
// the context its operation runs under (MarkXid) and the tracer marks every statement whose
// context carries it. A transaction is the operation's when any statement of it is marked, and
// then every statement its backend ran between BEGIN and COMMIT is counted, marked or not, since a
// connection serves only its transaction's holder until it commits. Background work of the server
// (settlement timers, the outbox, listeners) runs under contexts of its own, so it shares the pool
// but never the verdict.

// XidMaxGap is the longest statement-free gap D6 allows the code to make inside an xid window
// (XidGap.Made), in a build that judges a window's time at all (XidJudgesGaps), beside the time a
// gap's CPU's device softirqs may hold (XidGap.Bound).
const XidMaxGap = 5 * time.Millisecond

// XidFaultGap is the statement-free gap the rig's deliberate-fault self-tests inject behind the
// short statements of a rig transaction: four times XidMaxGap, and well under what a real
// document call inside a window costs, which leaves the docs package's control
// (TestXidWindowCatchesADocumentCallInsideTheWindow) as the test that goes red first if the bound
// is ever widened too far.
const XidFaultGap = 20 * time.Millisecond

// XidJudgesGaps reports whether this build judges how long a window's statement-free gaps are.
// A plain build judges them only when its thread-clock probe can read the calling thread's clocks.
// That probe is read once at run time (xidThreadClocksUnavailable): Linux kernels do not all expose
// schedstat. The probe holds the process's memory once, and that hold lasts (xidHoldMemory). A
// build with the race detector does not judge gaps, and the build tag is the only thing that says
// so (raceDetector); where either makes gap judgment unavailable, the rig holds window shape alone,
// as the race build does.
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
// run. So in CI the gap bound is judged in plain runs, and no race lane runs a window test: the
// api, delivery and docs window tests are `//go:build !race`, and envoy-go-alone's step "Test
// the xid window rows, the API's issue-list bound and the auth service's storm, alone" runs them
// plain (shape and gaps), on a runner nothing else loads; refs' runs plain in a unit leg of
// envoy-go. envoy-go is among the slow suites the changes job holds back on
// a pull request: it runs on a merge group (its first attempt only), on a manual dispatch, and on
// a pull request only while it carries the `full-ci` or `lane:envoy_go` label, and only when its
// filter (envoy_go) selects the change. So the gap bound is judged in the merge queue before
// every merge, not on every pull-request push.
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
	// WriteUnreported is true when the statement may have taken a transaction id that its own
	// result does not report (xidWriteUnreported): for a batch, when any query of it may have.
	// It decides a window only for a transaction no poll saw (xidWindowOf).
	WriteUnreported bool
	// started and ended are the scheduler clocks of the OS thread that sent the statement, read at
	// Start and at End, which are their stamps (readXidThreadClock); a clock the rig could not read
	// names no thread. locked is the OS thread the goroutine that began the statement's
	// transaction was locked to at its BEGIN (XidTracer.lockTransaction), zero when none was.
	// startedIRQ and endedIRQ are the softirq counts of the CPU that thread is pinned to, read just
	// after Start and just before End (readXidSoftirqs), so the counts from one statement's End to
	// the next one's Start cover the gap between them. unheldMemory is, on a marked BEGIN, why the
	// rig could not hold the process's memory there (XidTracer.lockTransaction), empty where it held
	// it or did not try.
	started, ended       xidThreadClock
	startedIRQ, endedIRQ xidSoftirqReading
	locked               int
	unheldMemory         string
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
	startedIRQ := readXidSoftirqs()
	pid := conn.PgConn().PID()
	verb := xidVerb(sql)
	marked := markedXid(ctx)
	var unheldMemory string
	if verb == "begin" && marked {
		unheldMemory = x.lockTransaction(pid)
	}
	span := &xidSpan{ends: verb == "commit" || verb == "rollback", statement: XidStatement{
		PID: pid, Start: start, SQL: sql, Marked: marked, started: started, startedIRQ: startedIRQ,
		unheldMemory: unheldMemory,
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
	span.statement.WriteUnreported = span.statement.WriteUnreported || xidWriteUnreported(span.statement.SQL, tag)
	span.statement.endedIRQ = readXidSoftirqs()
	span.statement.ended = readXidThreadClock()
	span.statement.End = span.statement.ended.stamp()
	span.done.Store(true)
	if span.ends {
		x.unlockTransaction(span.statement.PID)
	}
}

// lockTransaction locks the calling goroutine, which is sending the BEGIN of a marked
// transaction on backend pid, to its OS thread until that transaction's COMMIT or ROLLBACK
// (unlockTransaction). Where the build judges gaps (XidJudgesGaps), it also holds that thread out
// of NUMA balancing meanwhile (xidHoldThread), opens the thread's clock for the transaction's
// readings and pins the thread to the CPU it is on, with that CPU's softirq counts open beside its
// clock (xidWatchThreadClock), and holds the process's memory still (xidHoldWindowMemory): out of
// NUMA balancing, with the execution tracer's pages present, a hold that lasts (xidHoldWindowMemory
// says how). All of it runs inside the BEGIN, which no window holds. A build that judges no gap
// does none of that hold, since it reads no gap's clocks. A thread locked to a goroutine runs that
// goroutine and no other, so every gap between two statements the goroutine sends from it is read
// off clocks of its own: the time the thread ran, and waited for a CPU, is the goroutine's. It is
// the BEGIN that locks, not the measured operation, because an operation sends a transaction from
// any goroutine it starts - a broadcast delivers to each session on a goroutine of its own. A
// thread the rig could not hold is not recorded as held, so its gaps are read off no thread's
// clocks and are a rig problem (analyzeXidWindows). Nor is one whose BEGIN could not hold the
// process's memory, as when the hold found none of the execution tracer's memory to make present:
// lockTransaction answers why (unheld), the BEGIN's statement keeps it, and that window is UNKNOWN
// by that reason.
func (x *XidTracer) lockTransaction(pid uint32) (unheld string) {
	runtime.LockOSThread()
	held := xidHeldThread{thread: xidThread()}
	if XidJudgesGaps() {
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
		if unheld = xidHoldWindowMemory(); unheld != "" && held.release != nil {
			held.release()
			held.release = nil
		}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.locked == nil {
		x.locked = map[uint32]xidHeldThread{}
	}
	x.locked[pid] = held
	return unheld
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
	span, ok := ctx.Value(xidSpanKey{}).(*xidSpan)
	if !ok {
		return
	}
	span.statement.Wrote = span.statement.Wrote || xidAssigned(data.SQL, data.CommandTag)
	span.statement.WriteUnreported = span.statement.WriteUnreported || xidWriteUnreported(data.SQL, data.CommandTag)
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
	// Statements are the transaction's statements from the window's first through COMMIT, and
	// Spans their own stretches, Spans[i] of Statements[i], read as a gap is (XidStatement.span).
	Statements []XidStatement
	Spans      []XidGap
	Start, End time.Time
	// StatementTime is the time the code made in the window's statements (XidGap.Made of each of
	// Spans): their time less the part the rig reads as the machine's off a stretch's clocks, the
	// thread's wait for a CPU and the runtime's wait to run its goroutine, never their time on the
	// clock. It still holds the rig's own work on each statement - classifying its text, reading
	// whether it wrote, joining a batch's SQL - beyond the softirq reads it takes out, and the time
	// the client waited on the server for the answer, which a loaded server stretches. Both
	// lengthen the statements' side, so they can only make rule 2 pass a window it would fail,
	// never fail one it would pass.
	StatementTime time.Duration
	// Gaps are the gaps between Statements, Gaps[i] following Statements[i]. GapTime and MaxGap
	// are of the time the code made in them (XidGap.Made).
	Gaps    []XidGap
	GapTime time.Duration
	MaxGap  time.Duration
	// MaxGapAfter indexes Statements: the biggest gap follows Statements[MaxGapAfter].
	MaxGapAfter int
}

// Length is the window's span, its first statement's start to its COMMIT's end.
func (w XidWindow) Length() time.Duration { return w.End.Sub(w.Start) }

// Violations are the D6 rules the window breaks, empty when it keeps them. Both of them are
// readings of the time the code made in the window's gaps (XidGap.Made), so a build that judges
// none (XidJudgesGaps) breaks none here, and what holds there is the shape each window test
// asserts of its own window. Rule 1 holds each gap to its own bound (XidGap.Bound): XidMaxGap,
// and beside it, where the gap's CPU ran a device softirq on a kernel that charges that time to
// the thread it lands on, the time those softirqs may have put on the thread (XidGap.SoftirqHold).
// Rule 2 holds the gaps to the statements and the time all the window's gaps' softirqs may hold,
// at most XidSoftirqGapHold for the window: the measurement that sets that value is of one 5 ms
// stretch of one CPU, and a sum of each gap's own p99.99 would be far looser than the p99.99 of
// the gaps' sum.
func (w XidWindow) Violations() []string {
	if !XidJudgesGaps() {
		return nil
	}
	var broken []string
	var held time.Duration
	for i, gap := range w.Gaps {
		hold := gap.SoftirqHold()
		held += hold
		if gap.Made() <= gap.Bound() {
			continue
		}
		over := XidMillis(XidMaxGap)
		if hold > 0 {
			over = fmt.Sprintf("%s: the %s bound and %s its CPU's device softirqs may hold", XidMillis(gap.Bound()), XidMillis(XidMaxGap), XidMillis(hold))
		}
		broken = append(broken, fmt.Sprintf("a statement-free gap of %s (over %s) follows statement %d: %s",
			XidMillis(gap.Made()), over, i, xidSQL(w.Statements[i].SQL)))
	}
	held = min(held, XidSoftirqGapHold)
	if w.GapTime > w.StatementTime+held {
		also := ""
		if held > 0 {
			also = fmt.Sprintf(" and the %s their CPU's device softirqs may hold", XidMillis(held))
		}
		broken = append(broken, fmt.Sprintf("its gaps take %s, more than its statements' %s%s",
			XidMillis(w.GapTime), XidMillis(w.StatementTime), also))
	}
	return broken
}

// xidRuntimeWaitProblem names a window that keeps D6 only by taking its goroutine's waits on the
// runtime (XidGap.GoRunnable) out of its gaps. No such wait can be laid to the machine: the
// operation's own goroutines may hold every P, and the trace cannot show that an idle P was free
// to run the goroutine, since the runtime hands a P to a locked goroutine's thread through a stop
// that reads as idle until that thread starts it (xidTraceReading.transition). So the rig cannot
// say whose time those waits were, and the window is UNKNOWN, a rig problem that fails a run as
// one (XidReport.Err). Made still takes the waits out, so a window that keeps D6 with them counted
// as the code's passes, and one that breaks D6 anyway is a violation; this is "" for both.
func xidRuntimeWaitProblem(w XidWindow) string {
	if len(w.Violations()) > 0 {
		return ""
	}
	counted := w
	counted.Gaps = slices.Clone(w.Gaps)
	counted.GapTime = 0
	var waits []string
	for i := range counted.Gaps {
		gap := &counted.Gaps[i]
		if gap.GoRunnable > 0 {
			waits = append(waits, fmt.Sprintf("%s in the gap after statement %d", XidMillis(gap.GoRunnable), i))
		}
		gap.GoRunnable = 0
		counted.GapTime += gap.Made()
	}
	broken := counted.Violations()
	if len(waits) == 0 || len(broken) == 0 {
		return ""
	}
	return fmt.Sprintf("UNKNOWN: backend %d's window (%s) keeps D6 only if its goroutine's waits on the runtime - %s - are the machine's; the operation's own goroutines may have held every P, and the trace cannot show that a P was free, so the rig cannot tell whose they were. Counted as the code's, %s",
		w.Transaction.PID, xidSQL(w.Statements[0].SQL), strings.Join(waits, ", "), strings.Join(broken, "; "))
}

// xidPastTraceProblem names a window that ends past the execution trace's last event: the trace
// says nothing of the goroutine's states there, and goStates carries no state past it, so the
// part of the window it does not reach is read with no runtime wait, not with the last one
// extended - and is UNKNOWN, a rig problem, since the rig did not see it. "" where the trace
// reaches the window's end, or the rig took none.
func xidPastTraceProblem(w XidWindow, reading *xidTraceReading) string {
	if reading == nil {
		return ""
	}
	past := reading.pastEnd(w.End)
	if past <= 0 {
		return ""
	}
	return fmt.Sprintf("UNKNOWN: backend %d's window (%s) ends %s past the execution trace's last event: the trace stopped before the window did, so the rig did not see its goroutine's states to its end",
		w.Transaction.PID, xidSQL(w.Statements[0].SQL), XidMillis(past))
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
			switch made, hold := gap.Made(), gap.SoftirqHold(); {
			case !XidJudgesGaps() || made <= XidMaxGap:
			case made > gap.Bound():
				flag = "  <-- over " + XidMillis(gap.Bound())
			default:
				flag = fmt.Sprintf("  (over %s, within the %s its CPU's device softirqs may hold)", XidMillis(XidMaxGap), XidMillis(hold))
			}
			read := "its thread's clocks unread"
			if gap.Read {
				read = fmt.Sprintf("ran %s, off a CPU %s, waiting for one %s; its goroutine waited %s on the runtime, %s on what it asked for and %s in system calls",
					XidMillis(gap.Ran), XidMillis(gap.Asleep()), XidMillis(gap.Runnable),
					XidMillis(gap.GoRunnable), XidMillis(gap.GoWaiting), XidMillis(gap.GoSyscall))
			}
			softirqs := "; its CPU's softirqs uncounted"
			if gap.Counted {
				softirqs = fmt.Sprintf("; its CPU %d ran %s", gap.SoftirqCPU, gap.Softirqs)
			}
			fmt.Fprintf(&b, "      gap %10s  (%s on the clock: %s%s)%s\n", XidMillis(gap.Made()), XidMillis(gap.Clock), read, softirqs, flag)
		}
		fmt.Fprintf(&b, "  %3d %10s +%9s  %s\n", i, XidMillis(statement.Start.Sub(w.Start)),
			XidMillis(statement.End.Sub(statement.Start)), xidSQL(statement.SQL))
	}
	return b.String()
}

// XidReport is what one Measure saw.
type XidReport struct {
	// Windows are the operation's committed transactions that held an xid, in commit order: each
	// one a poll saw hold it, or whose own statements show it wrote (XidStatement.Wrote) or may
	// have written where their results do not say (XidStatement.WriteUnreported).
	Windows []XidWindow
	// Unsighted are the operation's committed transactions no poll saw hold an xid and whose
	// statements show no write of their own and may have made none: nothing says they held one.
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

// xidModifyingSQL is a statement's text naming an insert, an update or a delete.
var xidModifyingSQL = regexp.MustCompile(`(?is)\b(insert\s+into|update\s+\S+\s+set|delete\s+from)\b`)

// xidWriteUnreported reports whether a statement may have taken a transaction id that its own
// result cannot report: a select whose common table expressions insert, update or delete, which
// Postgres runs to completion whatever the select reads of them and answers with the select's own
// tag, counting the select's rows and none of theirs (the block-index tick's window write,
// refs.writeWindows, is one). Such a statement is the earliest a transaction no poll saw can have
// taken its xid at, so the window of that transaction starts there (xidWindowOf): read from the
// earliest place it can have begun, the window is never shorter than the one the transaction held.
func xidWriteUnreported(sql string, tag pgconn.CommandTag) bool {
	return tag.Select() && xidModifyingSQL.MatchString(sql)
}

// analyzeXidWindows splits statements into the operation's transactions and judges their windows.
// Where the build judges gaps, a window gap not read off its transaction's locked thread
// (XidGap.Read) or off the execution trace (XidGap.Traced) is a rig problem, and so is such a
// statement of a window that has a gap: rule 2 would weigh the gaps against that statement's time
// on the clock, or with the runtime's wait inside it, both of which a loaded machine stretches. A
// window whose transaction's BEGIN could not hold the process's memory (XidStatement.unheldMemory)
// has its thread not held, so its gaps and statements are unread for that one reason: the window is
// UNKNOWN, one problem naming it in place of theirs.
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
		// A transaction whose statements show no write of their own, may have made none, and that
		// no poll saw took no xid to judge: an operation that may write nothing passes on
		// MayHoldNoXid, and one that was to write is the report's own rig problem (problems).
		if transaction.Sighted.IsZero() && xidFirstWrote(transaction) < 0 && xidFirstUnreported(transaction) < 0 {
			report.Unsighted = append(report.Unsighted, transaction)
			continue
		}
		window := xidWindowOf(transaction, reading)
		if XidJudgesGaps() {
			unheld := transaction.Statements[0].unheldMemory
			if unheld != "" && len(window.Gaps) > 0 {
				report.Problems = append(report.Problems, fmt.Sprintf(
					"UNKNOWN: backend %d's window (%s) was read off no thread's clocks: at the transaction's BEGIN the rig could not hold the process's memory still (xidHoldWindowMemory): %s",
					transaction.PID, xidSQL(window.Statements[0].SQL), unheld))
			}
			for i, gap := range window.Gaps {
				switch {
				case unheld != "":
				case !gap.Read:
					report.Problems = append(report.Problems, fmt.Sprintf(
						"the gap after statement %d of backend %d's window (%s) was not read off one thread's clocks on both sides: the transaction's goroutine was not locked to its thread from its BEGIN (a BEGIN the operation's marked context did not send), the rig could not hold that thread out of NUMA balancing (xidHoldThread), the rig could not open that thread's schedstat at its BEGIN (xidWatchThreadClock), a read of the thread's clocks at one of the gap's ends found the thread switched out in each of its tries (readXidThreadClock, xidClockTries), or another goroutine sent its statements",
						i, transaction.PID, xidSQL(window.Statements[i].SQL)))
				case !gap.Traced:
					report.Problems = append(report.Problems, fmt.Sprintf(
						"the gap after statement %d of backend %d's window (%s) was not read off the execution trace: the trace shows no goroutine running on the transaction's thread by the gap's start",
						i, transaction.PID, xidSQL(window.Statements[i].SQL)))
				}
			}
			for i, span := range window.Spans {
				switch {
				case len(window.Gaps) == 0, unheld != "":
				case !span.Read:
					report.Problems = append(report.Problems, fmt.Sprintf(
						"statement %d of backend %d's window (%s) was not read off one thread's clocks at its start and end, for one of the causes an unread gap has (readXidThreadClock, xidClockTries), so rule 2 would weigh the window's gaps against its time on the clock, which a loaded machine stretches",
						i, transaction.PID, xidSQL(window.Statements[i].SQL)))
				case !span.Traced:
					report.Problems = append(report.Problems, fmt.Sprintf(
						"statement %d of backend %d's window (%s) was not read off the execution trace: the trace shows no goroutine running on the transaction's thread by its start, so rule 2 would count the runtime's wait inside it as the code's",
						i, transaction.PID, xidSQL(window.Statements[i].SQL)))
				}
			}
			if problem := xidPastTraceProblem(window, reading); problem != "" {
				report.Problems = append(report.Problems, problem)
			} else if problem := xidRuntimeWaitProblem(window); problem != "" {
				report.Problems = append(report.Problems, problem)
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
// since both name a statement that already held the xid. A transaction the poller never saw may
// also have taken its xid at a statement whose result cannot say so (XidStatement
// .WriteUnreported); its window starts at the first such one when that comes earlier, which is the
// longest window the transaction can have held.
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
	wrote := xidFirstWrote(transaction)
	if wrote >= 0 && (transaction.Sighted.IsZero() || wrote < first) {
		first = wrote
	}
	if unreported := xidFirstUnreported(transaction); transaction.Sighted.IsZero() && unreported >= 0 &&
		(wrote < 0 || unreported < wrote) {
		first = unreported
	}
	window := XidWindow{Transaction: transaction, Statements: transaction.Statements[first:]}
	window.Start = window.Statements[0].Start
	window.End = window.Statements[len(window.Statements)-1].End
	for i, statement := range window.Statements {
		// The rig's own reads of the softirq counts run inside the statement, as its other work
		// does; their CPU time is the rig's, and rule 2 sets the code's gaps against the code's
		// statements.
		span := statement.span(reading)
		window.Spans = append(window.Spans, span)
		window.StatementTime += max(span.Made()-statement.startedIRQ.cost-statement.endedIRQ.cost, 0)
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

// xidFirstUnreported indexes the transaction's first statement that may have taken an xid its own
// result does not report (XidStatement.WriteUnreported); -1 when none may have.
func xidFirstUnreported(transaction XidTransaction) int {
	return slices.IndexFunc(transaction.Statements, func(s XidStatement) bool { return s.WriteUnreported })
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
// finding - unless a window broke D6 too: then the violations follow the problems, and the run is
// not called free of a finding about the code.
func (r *XidReport) Err() error {
	var b strings.Builder
	for _, window := range r.Windows {
		if broken := window.Violations(); len(broken) > 0 {
			fmt.Fprintf(&b, "XID WINDOW VIOLATION (INFRA-636 D6) %s\n  %s\n%s", window.Summary(), strings.Join(broken, "\n  "), window.Detail())
		}
	}
	if problems := r.problems(); len(problems) > 0 {
		if b.Len() > 0 {
			return fmt.Errorf("XID RIG PROBLEM: %s\nand a window broke D6:\n%s", strings.Join(problems, "; "), b.String())
		}
		return fmt.Errorf("XID RIG PROBLEM (not a finding about the code): %s", strings.Join(problems, "; "))
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
