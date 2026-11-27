package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// INFRA-450's a14: what the Inbox's reads cost once a person has dismissed a very great many
// threads. A dismissal keeps the row rather than deleting it, so the pile only grows, and the
// question this measures is the one the person it is for asked - whether reading the Inbox, or
// reading a page of what he has dismissed, gets slower as it does.
//
// The seed is his own shape, an order of magnitude past where he is: 100,000 dismissed Reply
// threads, 10,000 dismissed Agent threads, 600 undismissed threads (300 of them unread) and 1,000
// more unread threads for Mark all read to work over - 1,000 rather than a token number because he
// had 804 unread replies the morning this was written.
//
// Six operations: the whole Inbox read (state=inbox), the Inbox count's read
// (state=unread&limit=1), the first Dismissed page, a Dismissed page 50,000 rows deep, a dismissal
// naming 120 threads, and one Mark all read. Each is held to the work of every statement its
// request sends, by Postgres's own count, and timed over 20 runs for the reader.
//
// # Three shapes, and which bound each number is held to
//
// Every shape is seeded by the same code and differs only in the pile:
//
//   - no dismissed: no dismissed row at all. The ratio baseline for the four operations whose
//     answer is identical in both shapes - state=inbox, the unread count, the 120-thread dismiss
//     and the Mark all read - since those four read the same 1,600 undismissed rows either way.
//   - one page dismissed: 200 dismissed threads, exactly one full Dismissed page. The ratio
//     baseline for the two Dismissed reads, because with no dismissed row at all those two answer
//     nothing: a ratio against an answer of 0 rows measures the empty case rather than the pile,
//     and reporting it as a pass would say nothing. What the person's worry actually asks is not
//     "is reading a page slow" but "does a pile make reading a page slower", which is one page at
//     200 against one page at 110,000.
//   - loaded: the whole pile, 100,000 + 10,000.
//
// Each operation's work at the pile is held against its baseline's - the no-dismissed shape for
// four of them, the one-page shape for the two Dismissed reads. Its request is sent once on each
// shape with every statement the server sends recorded as sent (dismissedScaleTrace): the reads
// are run again under EXPLAIN (ANALYZE, BUFFERS) with the arguments they were sent, for the rows
// every plan node produced or threw away and the shared buffers it accessed, under both plans a
// connection runs a statement with; the rest - writes, row and advisory locks, the transaction's
// own begin and commit - are counted in number and in the rows they changed or locked
// (dismissedScaleWork). What each is held to, and the measurement that set it, is written at
// dismissedScaleBounds.
//
// # What these numbers are, and what they are not
//
// The counts are Postgres's own, taken under EXPLAIN (ANALYZE, BUFFERS) on one connection: the rows
// each plan node handled and the pages it read, cached or not, and neither is a time. A busy box
// makes the same plan do the same work more slowly, and each shape's plans are made from the
// statistics its own vacuum analyze wrote, so the counts do not move with the box's load. When
// the operations were held to times instead - 250 ms on the p95 and twice the baseline's p50 -
// the box decided the ratio: at main 6ef60ff0ce, at a load of 187, the first Dismissed page read
// 7.25 times its baseline's p50, and on a cluster pod of its own 5.73 times (2026-11-22). There,
// the plan of that page's Agent half at the pile printed 27.4 ms of JIT compilation in a 31.9 ms
// run, where at one page its estimate stays under jit_above_cost and nothing is compiled; the
// compilation is CPU work outside every count, which a busy box stretches. It is the test
// database's alone: CI's image runs with jit on, and production runs without it, since Cloud SQL
// does not support PostgreSQL's LLVM JIT (benchtime.Open turns it off for the same reason).
//
// The times are still printed, so a reader can see what the operations took on the box that ran
// them; no verdict reads them. The time of record is the server's own: the clock runs around
// handler.ServeHTTP alone, with the request body encoded before it starts, so no client, no socket
// and no JSON encoding of the caller's is on it.
//
// One more thing the seed is: every row of user_threads belongs to one login, because the
// measurement is one person's pile. That is the pessimistic case for the planner's choice between
// a state's partial index and a scan of the table, since the login this read narrows by rules out
// nothing here, where in production it rules out everyone else's rows. A plan that scans
// user_threads at this seed may still read the index at the same pile size on a table many people
// write in; a plan that reads the index here reads it there too.
//
// Each shape is a database of its own (storetest.Open), dropped when the test ends, so the
// 110,000 rows are gone by the time the next test runs.
const (
	// The pile, as the measurement asks for it.
	dismissedScaleReplyPile = 100_000
	dismissedScaleAgentPile = 10_000
	// One full Dismissed page, in the pile's own proportion of the two kinds: the second
	// baseline's whole pile.
	dismissedScalePageReplies = 180
	dismissedScalePageAgents  = 20
	// The Inbox every shape holds: 600 undismissed threads, 300 of them unread, of both kinds.
	dismissedScaleInboxReplies = 540
	dismissedScaleInboxAgents  = 60
	dismissedScaleReadReplies  = 270
	dismissedScaleReadAgents   = 30
	// The unread threads Mark all read works over, beside those 300.
	dismissedScaleMarkAll = 1_000
	// The measurement itself.
	dismissedScaleRuns        = 20
	dismissedScaleDismissRows = 120
	dismissedScaleCursorDepth = 50_000
	// The bounds verification.md v17 holds each operation to, on the work its reads do rather
	// than the time they take (dismissedScaleBounds): twice its baseline's rows and buffer
	// accesses, with the two Dismissed reads' buffer accesses held to 2.5 times.
	dismissedScaleWorkRatio      = 2.0
	dismissedScaleDismissedPages = 2.5
	// The session every seeded Agent thread is answered by, and the login every row belongs to.
	dismissedScaleSession = "01a1058e-f14f-7684-87eb-3dc885955552"
	dismissedScaleLogin   = "alice"
	// dismissedScaleEnv is what a14's own command sets to run this measurement.
	dismissedScaleEnv = "LEGION_SCALE_TEST"
)

// The minute band each seed's threads are aged into, so the pile is older than the Inbox and no
// two bands overlap: the pile's newest reply is 100,003 minutes old, the mark-all's 2,003 and the
// Inbox's 3.
const (
	dismissedScalePileAge   = 100_000
	dismissedScaleMarkAge   = 2_000
	dismissedScaleInboxAge  = 0
	dismissedScaleAgentAge  = 100_000
	dismissedScaleAgentStep = 10
)

// The bodies each seed writes, which is also how a later statement finds its own rows.
const (
	dismissedScalePileLabel       = "dismissed"
	dismissedScaleInboxLabel      = "inbox"
	dismissedScaleMarkLabel       = "markall"
	dismissedScaleAgentPileLabel  = "dismissed agent"
	dismissedScaleAgentInboxLabel = "inbox agent"
)

// dismissedScaleShape is one seeded database and the server over it, with the rows the
// measurement needs to name: the cursor a page 50,000 deep resumes from, the 120 rows one
// dismissal names, and the unread rows a Mark all read marks, which the next run puts back. trace
// records what the server sends on a marked request (dismissedScaleTrace).
type dismissedScaleShape struct {
	name             string
	handler          http.Handler
	database         *store.Store
	trace            *dismissedScaleTrace
	issue            string
	dismissedReplies int
	dismissedAgents  int
	deepCursor       string
	dismissRows      []string
	unreadThreads    []string
	unreadReplies    []string
}

// dismissedScaleRun is one operation's request on one shape, on ctx, answering the server's own
// time for it (timeDismissedScaleRequest).
type dismissedScaleRun func(context.Context, *testing.T, *dismissedScaleShape) time.Duration

// TestDismissedScaleHoldsTheInboxsReads is a14 itself: it seeds the three shapes, asserts each
// one's counts before it measures anything, sends each of the six operations' requests once and
// counts the work of every statement the request sent, explaining each at the pile's own size,
// holds the one read's Dismissed pages to their cut under both its plans
// (explainDismissedScaleOneRead), times the six operations 20 runs each, and holds each
// operation's work to its bound (dismissedScaleBounds). The times are printed; no verdict reads
// them.
//
// Every shape's database runs with jit = off (jitOffStore), read back on a new session of each
// before the first request and printed above the table (turnJITOff says why).
//
// It runs when LEGION_SCALE_TEST is 1, which is what a14's own command sets, and skips otherwise:
// seeding 110,000 rows three times over and taking 120 timed readings costs minutes, which no
// ordinary run of this package should pay.
func TestDismissedScaleHoldsTheInboxsReads(t *testing.T) {
	t.Parallel()
	if os.Getenv(dismissedScaleEnv) != "1" {
		t.Skipf("set %s=1 to run the dismissed-scale measurement", dismissedScaleEnv)
	}
	loaded := seedDismissedScale(t, "loaded", dismissedScaleReplyPile, dismissedScaleAgentPile)
	none := seedDismissedScale(t, "no dismissed", 0, 0)
	onePage := seedDismissedScale(t, "one page dismissed", dismissedScalePageReplies, dismissedScalePageAgents)
	shapes := []*dismissedScaleShape{loaded, none, onePage}
	var jit []string
	for _, shape := range shapes {
		jit = append(jit, shape.name+": "+requireJITOff(t, "the "+shape.name+" shape's database", shape.database.Pool.Config().ConnString()))
	}

	// What each operation's request sends, recorded on one untimed request of each: every
	// statement the server ran for it, whatever sent it (dismissedScaleTrace). The two write
	// routes put back what they changed, as their timed runs do, and each shape is then vacuumed
	// and analysed once, so the counts below read each shape on the statistics and pages that one
	// vacuum left, which nothing changes until the timed runs; a count taken after those would read
	// whatever autovacuum chose to clear by then.
	operations := []struct {
		operation string
		run       dismissedScaleRun
		shapes    []*dismissedScaleShape
	}{
		{"state=inbox", runDismissedScaleInbox, []*dismissedScaleShape{loaded, none}},
		{"unread count", runDismissedScaleUnreadCount, []*dismissedScaleShape{loaded, none}},
		{"dismissed page 1", runDismissedScaleFirstPage, []*dismissedScaleShape{loaded, onePage}},
		{"dismissed page 50,000 deep", runDismissedScaleDeepPage, []*dismissedScaleShape{loaded}},
		{"120-thread dismiss", runDismissedScaleDismiss, []*dismissedScaleShape{loaded, none}},
		{"mark all read", runDismissedScaleMarkAll, []*dismissedScaleShape{loaded, none}},
	}
	sent := map[string][]dismissedScaleSent{}
	for _, measured := range operations {
		for _, shape := range measured.shapes {
			shape.trace.take()
			measured.run(context.WithValue(context.Background(), dismissedScaleTraced{}, true), t, shape)
			statements := shape.trace.take()
			if len(statements) == 0 {
				t.Fatalf("%s @ %s sent no statement the trace saw: nothing was measured", measured.operation, shape.name)
			}
			sent[measured.operation+" @ "+shape.name] = statements
		}
	}
	for _, shape := range shapes {
		if _, err := shape.database.Pool.Exec(context.Background(), "vacuum analyze"); err != nil {
			t.Fatalf("vacuum %s after the recorded requests: %v", shape.name, err)
		}
	}

	// The work and the plans. The plans the test names are asserted on the loaded shape alone:
	// with no dismissed row at all a scan of an empty table is the right plan and says nothing
	// about scale, so the other two shapes' plans are printed and nothing is held to them.
	work := map[string]dismissedScaleWork{}
	for _, measured := range operations {
		for _, shape := range measured.shapes {
			key := measured.operation + " @ " + shape.name
			work[key] = explainDismissedScale(t, shape, measured.operation, sent[key], shape == loaded)
		}
	}
	for _, shape := range shapes {
		explainDismissedScaleOneRead(t, shape, shape == loaded)
	}

	// The readings, each the p50 and p95 of 20 runs, for the reader: what the operations took on
	// this box while it ran, which is a reading of the box as much as of the reads, so nothing is
	// held to them. The two Dismissed reads are timed on the no-dismissed shape as well, where they
	// answer nothing, so a reader can see what the empty case costs and why it is nobody's
	// baseline.
	readings := map[string]dismissedScaleReading{}
	taken := []string{}
	record := func(operation string, shape *dismissedScaleShape, run dismissedScaleRun) {
		name := operation + " @ " + shape.name
		readings[name] = measureDismissedScale(t, shape, operation, run)
		taken = append(taken, name)
	}
	for _, shape := range []*dismissedScaleShape{loaded, none} {
		record("state=inbox", shape, runDismissedScaleInbox)
		record("unread count", shape, runDismissedScaleUnreadCount)
		record("dismissed page 1", shape, runDismissedScaleFirstPage)
		record("120-thread dismiss", shape, runDismissedScaleDismiss)
		record("mark all read", shape, runDismissedScaleMarkAll)
	}
	record("dismissed page 1", onePage, runDismissedScaleFirstPage)
	record("dismissed page 50,000 deep", loaded, runDismissedScaleDeepPage)

	// The table, in one block, which is what goes in the pull request's comment: each operation's
	// work at the pile against its baseline's under both plans, the statements it sent and the
	// rows those that are not reads changed or locked, the bound it is held to, and the two times
	// beside them. The line above it says what each shape's database read on a new session before
	// the first request.
	t.Logf("the shapes' databases, each read on a new session: %s", strings.Join(jit, "; "))
	t.Log("operation	plan	at the pile	at the baseline	rows x	buffer accesses x	statements	rows changed or locked	bound	p50 ms	p95 ms	baseline p50 ms")
	for _, held := range dismissedScaleBounds() {
		pile, ok := work[held.operation+" @ loaded"]
		if !ok {
			t.Fatalf("no count of %s", held.operation)
		}
		baseline := work[held.baselineOperation+" @ "+held.baselineShape]
		reading := readings[held.operation+" @ loaded"]
		against := readings[held.baselineOperation+" @ "+held.baselineShape]
		for _, plan := range held.plans(pile, baseline) {
			t.Logf("%s\t%s\t%s\t%s at %s\t%.2f\t%.2f\t%d against %d\t%d against %d\t%s\t%.1f\t%.1f\t%.1f",
				held.operation, plan.name, plan.pile, plan.baseline, held.baselineShape,
				plan.pile.rows/plan.baseline.rows, plan.pile.buffers/plan.baseline.buffers,
				pile.statements, baseline.statements, pile.effects, baseline.effects, held.bound(),
				msOf(reading.p50), msOf(reading.p95), msOf(against.p50))
		}
	}
	for _, name := range taken {
		reading := readings[name]
		t.Logf("%s\tp50 %.1f ms\tp95 %.1f ms\truns %v", name, msOf(reading.p50), msOf(reading.p95), reading.runs)
	}

	// The bounds, one subtest per operation so a read that is a finding names itself and the rest
	// still report their numbers. Each holds the operation's work at the pile against its
	// baseline's under both plans a connection runs its statements with, and the statements it
	// sent and the rows those that are not reads changed or locked to the same multiple as its rows.
	for _, held := range dismissedScaleBounds() {
		t.Run(held.operation, func(t *testing.T) {
			pile := work[held.operation+" @ loaded"]
			baseline := work[held.baselineOperation+" @ "+held.baselineShape]
			for _, plan := range held.plans(pile, baseline) {
				if plan.baseline.rows == 0 || plan.baseline.buffers == 0 || plan.pile.buffers == 0 {
					t.Fatalf("%s under %s counted %s at the pile and %s at %s: a count of nothing is a read this test could not see, so nothing was measured",
						held.operation, plan.name, plan.pile, plan.baseline, held.baselineShape)
				}
				rows, buffers := plan.pile.rows/plan.baseline.rows, plan.pile.buffers/plan.baseline.buffers
				if dismissedScaleOver(rows, held.rows) || dismissedScaleOver(buffers, held.buffers) {
					t.Errorf("%s under %s: %s at the pile against %s at %s (%s), %.2fx the rows and %.2fx the buffer accesses, want %s",
						held.operation, plan.name, plan.pile, plan.baseline, held.baselineShape, held.baselineOperation,
						rows, buffers, held.bound())
				}
			}
			if statements := float64(pile.statements) / float64(baseline.statements); dismissedScaleOver(statements, held.rows) {
				t.Errorf("%s sent %d statements at the pile against %d at %s (%s), %.2fx, want at most %.2fx",
					held.operation, pile.statements, baseline.statements, held.baselineShape, held.baselineOperation, statements, held.rows)
			}
			if pile.effects > int64(held.rows*float64(max(baseline.effects, 1))) {
				t.Errorf("%s's statements that are not reads changed or locked %d rows at the pile against %d at %s (%s), want at most %.2fx",
					held.operation, pile.effects, baseline.effects, held.baselineShape, held.baselineOperation, held.rows)
			}
		})
	}

	// The negative control, last: the shape this change replaced, counted at the pile against the
	// first Dismissed page's bound, which it must be outside of.
	control, ok := dismissedScaleBoundOf("dismissed page 1")
	if !ok {
		t.Fatalf("no bound is named %q, so the negative control has nothing to be held against", "dismissed page 1")
	}
	dismissedScaleNegativeControl(t, loaded, work[control.operation+" @ loaded"].custom,
		work[control.baselineOperation+" @ "+control.baselineShape].custom, control)
}

// dismissedScaleBoundOf is the bound of one operation, by its name.
func dismissedScaleBoundOf(operation string) (dismissedScaleHeld, bool) {
	for _, held := range dismissedScaleBounds() {
		if held.operation == operation {
			return held, true
		}
	}
	return dismissedScaleHeld{}, false
}

// dismissedScaleHeld is one operation, the baseline its work is held against, and the most rows
// and buffer accesses it may make at the pile, each as a multiple of the baseline's. A zero
// multiple is one that is recorded rather than bounded.
type dismissedScaleHeld struct {
	operation         string
	baselineOperation string
	baselineShape     string
	rows              float64
	buffers           float64
}

// dismissedScalePlanWork is one operation's work at the pile and at its baseline under one plan.
type dismissedScalePlanWork struct {
	name           string
	pile, baseline dismissedScaleCount
}

// plans pairs an operation's work at the pile with its baseline's under each of the two plans,
// each against the same plan: the plan made for the arguments against that plan, the generic
// against the generic.
func (held dismissedScaleHeld) plans(pile, baseline dismissedScaleWork) []dismissedScalePlanWork {
	return []dismissedScalePlanWork{
		{"the plan made for its arguments", pile.custom, baseline.custom},
		{"the generic plan", pile.generic, baseline.generic},
	}
}

func (held dismissedScaleHeld) bound() string {
	limit := func(ratio float64) string {
		if ratio == 0 {
			return "recorded"
		}
		return fmt.Sprintf("at most %.2fx", ratio)
	}
	return fmt.Sprintf("rows %s, buffer accesses %s", limit(held.rows), limit(held.buffers))
}

// dismissedScaleOver is whether a multiple of the baseline is over the bound it is held to; a zero
// bound is recorded and holds nothing.
func dismissedScaleOver(ratio, bound float64) bool {
	return bound > 0 && ratio > bound
}

// dismissedScaleBounds names the six operations, the baseline each is held against and the bound
// each is held to (verification.md v17, where the bound was twice the baseline's time). The two
// Dismissed reads are taken against one full page at 200 dismissed threads rather than against a
// shape with none, where they would answer no rows at all, and the other four against the
// no-dismissed shape, which holds the same 1,600 undismissed rows they read at the pile. Each
// bound's rows multiple also holds the statements the request sent and the rows its statements
// that are not reads changed or locked (TestDismissedScaleHoldsTheInboxsReads).
//
// What correct code counts over every statement its request sent, in two runs on PostgreSQL 18
// (CI's image, 2026-11-23), as a multiple of its baseline's rows and buffer accesses under the plan
// made for each statement's arguments, and under the generic plan. The rows repeated exactly; the
// buffer accesses moved by under one percent with where each seed's random ids put the rows, the
// dismissal's by more. Every operation sent as many statements at the pile as at its baseline (7,
// 8, 7, 7, 12 and 17), and its writes and locks changed or locked as many rows (0 for the four
// reads, 245 for the dismissal, 2,576 for Mark all read). Besides the reads the test names, every
// read request sends the caller's identity upsert (`insert into people … on conflict do nothing`,
// counted as a change), `select now()` and the read of the page's session names
// (nameAgentSessions: 126 rows and 18 accesses at the pile, 60 and 20 at one page dismissed).
// The four reads' figures were counted over GET /api/v1/me/replies (listMyReplies), the route the
// bounds were set on; the four ask the one read here (meInboxStatement), which the bounds hold to
// the same multiples, and a run prints its counts in the table.
//
//   - state=inbox: 1.10x and 1.38x (1.09x and 1.34x), held to twice in both.
//   - The Inbox count: 1.06x and 1.55x to 1.57x (1.05x and 1.90x), held to twice in both. Its
//     time ratio was recorded and not bounded, at 1.96x to 2.57x, because its Agent half read an
//     index entry for each of the person's 10,060 one-off roots at the pile against 60 at the
//     baseline: by count that pass was about 460 more buffer accesses over 935. It reads the 60
//     live roots alone since messages_direct_roots_live (migration 20261125053341, myAgentLiveRoot)
//     left the dismissed ones out. Under the generic plan the Reply count reads
//     user_threads_unread and each row it matches, 500 accesses at the pile against 52. The shape
//     this change replaced, which asked each dismissed thread its own question, is the negative
//     control (dismissedScaleNegativeControl). With its unread roots walking every dismissed
//     root's thread as well (myAgentUnreadRoots without its dismissal test, which answers the same
//     rows), it counts 71.01x the buffer accesses at 1.96x the rows, which is why buffer accesses
//     are held as well as rows.
//   - The first Dismissed page and the page 50,000 deep: 0.69x and 2.21x (0.68x and 1.98x to
//     2.00x), held to twice in rows and dismissedScaleDismissedPages in buffer accesses, for the
//     page's composition and not the pile. Each half reads up to a full page before the route
//     merges the two (listMyReplies), so at the pile the page reads 200 Reply rows and 200 Agent
//     roots to answer 200 (3,815 and 2,605 accesses), where at one page dismissed it reads the 180
//     and the 20 there are (2,528 and 365): per row read, 19 and 13 accesses against 14 and 18.
//     The nearest defect measured above the bound is the Agent half's lookup of each root by id
//     left unfenced (myAgentRootIsOneOff), which walks messages_direct_roots to each root: 2.62x at
//     the first page and 23.09x at the page 50,000 deep. A Reply count of the whole Dismissed view
//     beside each page, the shape replies.go's listMyReplies says it does not take (110,000 rows,
//     8 s of a 9.4 s read at a14's seed), is a statement no test names and is counted all the same.
//     The shape this change replaced, the negative control, adds to the first page's 2.21x to make
//     it 19.73x.
//   - The dismissal of 120 threads: 0.33x the rows (0.33x to 1.00x under the generic plan), held
//     to twice; its buffer accesses are recorded. At the baseline the planner reads the 120 rows
//     of a 1,740-row table by one bitmap or by 120 probes, as the random ids fall, which was 14 to
//     125 accesses over the runs on 2026-11-22 and 23, so no multiple of it means anything. At the
//     pile it is 120 probes of user_threads_pkey, 407 to 450 accesses, 3.4 to 3.8 a row. A read of
//     the pile shows in its rows, and the plan checks hold it to that index
//     (dismissedScaleStatements).
//   - Mark all read: 1.06x and 1.38x (1.04x and 1.45x), held to twice in both.
func dismissedScaleBounds() []dismissedScaleHeld {
	return []dismissedScaleHeld{
		{"state=inbox", "state=inbox", "no dismissed", dismissedScaleWorkRatio, dismissedScaleWorkRatio},
		{"unread count", "unread count", "no dismissed", dismissedScaleWorkRatio, dismissedScaleWorkRatio},
		{"dismissed page 1", "dismissed page 1", "one page dismissed", dismissedScaleWorkRatio, dismissedScaleDismissedPages},
		{"dismissed page 50,000 deep", "dismissed page 1", "one page dismissed", dismissedScaleWorkRatio, dismissedScaleDismissedPages},
		{"120-thread dismiss", "120-thread dismiss", "no dismissed", dismissedScaleWorkRatio, 0},
		{"mark all read", "mark all read", "no dismissed", dismissedScaleWorkRatio, dismissedScaleWorkRatio},
	}
}

// The five requests. Each returns the server's own time for one run: the body is encoded before
// the clock starts, and the clock covers handler.ServeHTTP alone.

func runDismissedScaleInbox(ctx context.Context, t *testing.T, shape *dismissedScaleShape) time.Duration {
	t.Helper()
	taken, response := timeDismissedScaleRequest(ctx, t, shape, http.MethodGet,
		"/api/v1/me/inbox?state=inbox", nil)
	page := decodeDismissedScaleReplies(t, response)
	if len(page.Rows) != dismissedScaleInboxRows() {
		t.Fatalf("state=inbox answered %d rows, want the %d the seed holds", len(page.Rows), dismissedScaleInboxRows())
	}
	return taken
}

func runDismissedScaleUnreadCount(ctx context.Context, t *testing.T, shape *dismissedScaleShape) time.Duration {
	t.Helper()
	taken, response := timeDismissedScaleRequest(ctx, t, shape, http.MethodGet,
		"/api/v1/me/inbox?state=unread&limit=1", nil)
	page := decodeDismissedScaleReplies(t, response)
	if page.Total != dismissedScaleUnreadRows() {
		t.Fatalf("the Inbox count read answered total=%d, want %d", page.Total, dismissedScaleUnreadRows())
	}
	return taken
}

func runDismissedScaleFirstPage(ctx context.Context, t *testing.T, shape *dismissedScaleShape) time.Duration {
	t.Helper()
	taken, response := timeDismissedScaleRequest(ctx, t, shape, http.MethodGet,
		"/api/v1/me/inbox?state=dismissed&limit="+fmt.Sprint(dismissedScalePage), nil)
	page := decodeDismissedScaleReplies(t, response)
	want := min(shape.dismissedReplies+shape.dismissedAgents, dismissedScalePage)
	if len(page.Rows) != want {
		t.Fatalf("the first Dismissed page answered %d rows, want %d", len(page.Rows), want)
	}
	return taken
}

func runDismissedScaleDeepPage(ctx context.Context, t *testing.T, shape *dismissedScaleShape) time.Duration {
	t.Helper()
	taken, response := timeDismissedScaleRequest(ctx, t, shape, http.MethodGet,
		"/api/v1/me/inbox?state=dismissed&limit="+fmt.Sprint(dismissedScalePage)+"&cursor="+shape.deepCursor, nil)
	page := decodeDismissedScaleReplies(t, response)
	if len(page.Rows) != dismissedScalePage {
		t.Fatalf("the page %d deep answered %d rows, want a full page",
			dismissedScaleCursorDepth, len(page.Rows))
	}
	return taken
}

// runDismissedScaleDismiss times one dismissal naming 120 Reply rows and puts them back
// afterwards, off the clock, so every run of the 20 dismisses the same 120 threads out of the
// same Inbox. The 120 are threads the seed already marked read, so dismissing them moves no read
// mark and the unread count the other operations read is the same before and after.
//
// The versions it carries are read off the seed before the clock starts (dismissBody,
// api/inbox_dismiss_test.go), as a page would have carried them: nobody writes in these threads
// while the measurement runs, so each row still holds the version it was seeded with and every
// run dismisses all 120.
func runDismissedScaleDismiss(ctx context.Context, t *testing.T, shape *dismissedScaleShape) time.Duration {
	t.Helper()
	rows := make([]string, 0, len(shape.dismissRows))
	for _, thread := range shape.dismissRows {
		rows = append(rows, inboxDismissReplyPrefix+thread)
	}
	body := dismissBody(t, shape.database, dismissedScaleLogin, rows)
	taken, response := timeDismissedScaleRequest(ctx, t, shape, http.MethodPost, "/api/v1/me/inbox/dismiss", body)
	if dismissed := decodeDismissedScaleCount(t, response, "dismissed"); dismissed != dismissedScaleDismissRows {
		t.Fatalf("the dismissal answered %d threads, want the %d it named", dismissed, dismissedScaleDismissRows)
	}
	restore := dismissDismissedScaleRestore(t, shape, rows)
	if restore != dismissedScaleDismissRows {
		t.Fatalf("the restore put back %d threads, want %d", restore, dismissedScaleDismissRows)
	}
	return taken
}

// runDismissedScaleMarkAll times one Mark all read over the unread threads the seed holds, with
// read_through taken from a list read's own as_of, as the page sends it, and puts every mark back
// afterwards, off the clock.
func runDismissedScaleMarkAll(ctx context.Context, t *testing.T, shape *dismissedScaleShape) time.Duration {
	t.Helper()
	asOf := dismissedScaleAsOf(t, shape)
	taken, response := timeDismissedScaleRequest(ctx, t, shape, http.MethodPost, "/api/v1/me/inbox/read",
		map[string]any{"read_through": asOf, "kind": []string{"reply", "agent"}})
	marked := decodeDismissedScaleCount(t, response, "marked")
	if marked != dismissedScaleUnreadRows() {
		t.Fatalf("Mark all read marked %d threads, want the %d unread the seed holds",
			marked, dismissedScaleUnreadRows())
	}
	unmarkDismissedScale(t, shape)
	return taken
}

// dismissedScaleInboxRows and dismissedScaleUnreadRows are what every shape's Inbox holds: the
// 600 undismissed threads plus the 1,000 the mark-all works over, and the unread among them.
func dismissedScaleInboxRows() int {
	return dismissedScaleInboxReplies + dismissedScaleInboxAgents + dismissedScaleMarkAll
}

func dismissedScaleUnreadRows() int {
	return dismissedScaleInboxReplies - dismissedScaleReadReplies +
		dismissedScaleInboxAgents - dismissedScaleReadAgents + dismissedScaleMarkAll
}

// timeDismissedScaleRequest runs one request and answers the server's own time for it. The body
// is encoded first, so only handler.ServeHTTP is on the clock.
func timeDismissedScaleRequest(
	ctx context.Context, t *testing.T, shape *dismissedScaleShape, method, target string, body any,
) (time.Duration, *httptest.ResponseRecorder) {
	t.Helper()
	var reader *strings.Reader
	if body == nil {
		reader = strings.NewReader("")
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s %s: %v", method, target, err)
		}
		reader = strings.NewReader(string(encoded))
	}
	request := httptest.NewRequestWithContext(ctx, method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", dismissedScaleLogin)
	response := httptest.NewRecorder()
	started := time.Now()
	shape.handler.ServeHTTP(response, request)
	taken := time.Since(started)
	if response.Code != http.StatusOK {
		t.Fatalf("%s %s on %s: status=%d body=%s", method, target, shape.name, response.Code, response.Body.String())
	}
	return taken, response
}

// dismissedScalePage is how many rows one Dismissed page draws: the page the Dismissed view reads
// again from the cursor.
const dismissedScalePage = 200

// dismissedScalePageRows is one answer of the one read cut to what a14 reads of it: the Thread and
// Agent rows it draws, its total and its as_of.
type dismissedScalePageRows struct {
	Rows  []string
	Total int
	AsOf  time.Time
}

func decodeDismissedScaleReplies(t *testing.T, response *httptest.ResponseRecorder) dismissedScalePageRows {
	t.Helper()
	var read meInboxResponse
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the Inbox read: %v", err)
	}
	page := dismissedScalePageRows{Total: read.Total, AsOf: read.AsOf}
	for _, entry := range read.Entries {
		if entry.Thread != nil || entry.Agent != nil {
			page.Rows = append(page.Rows, entry.Node.ID)
		}
	}
	return page
}

func decodeDismissedScaleCount(t *testing.T, response *httptest.ResponseRecorder, field string) int {
	t.Helper()
	counted := map[string]int{}
	if err := json.Unmarshal(response.Body.Bytes(), &counted); err != nil {
		t.Fatalf("decode the write's answer: %v", err)
	}
	return counted[field]
}

// dismissedScaleAsOf is the as_of of one list read, which is what the page sends a Mark all read
// as its read_through. It is off the clock.
func dismissedScaleAsOf(t *testing.T, shape *dismissedScaleShape) string {
	t.Helper()
	response := dispatchRequest(t, shape.handler, http.MethodGet,
		"/api/v1/me/inbox?state=unread&limit=1", nil, dismissedScaleLogin)
	if response.Code != http.StatusOK {
		t.Fatalf("read as_of on %s: status=%d body=%s", shape.name, response.Code, response.Body.String())
	}
	return decodeDismissedScaleReplies(t, response).AsOf.UTC().Format(time.RFC3339Nano)
}

// dismissDismissedScaleRestore puts the dismissed rows back, off the clock.
func dismissDismissedScaleRestore(t *testing.T, shape *dismissedScaleShape, rows []string) int {
	t.Helper()
	response := dispatchRequest(t, shape.handler, http.MethodPost, "/api/v1/me/inbox/dismiss",
		map[string]any{"threads": rows, "restore": true}, dismissedScaleLogin)
	if response.Code != http.StatusOK {
		t.Fatalf("restore on %s: status=%d body=%s", shape.name, response.Code, response.Body.String())
	}
	return decodeDismissedScaleCount(t, response, "dismissed")
}

// unmarkDismissedScale puts every read mark one Mark all read moved back where it was, off the
// clock: each Reply thread's mark to the minute before its newest reply, which is where the
// trigger left it, and the Agent threads' replies unread, the way the prune of a row ends a reply
// read by its id (prunedAgentReplies, api/state.go): the rows the mark-all wrote, deleted by id,
// and in the same transaction the retraction of the reply_read tuple each of those rows' marks
// stated. That tuple is what an Agent row's unread test reads (replyReadByID,
// api/unread_replies.go), so a put-back that deleted the rows alone would leave every Agent reply
// read, and every Mark all read after the first would mark the Reply threads alone: 1,270 of the
// 1,300. The dismissed pile's own read rows are named here one by one, so they stay.
func unmarkDismissedScale(t *testing.T, shape *dismissedScaleShape) {
	t.Helper()
	ctx := context.Background()
	tag, err := shape.database.Pool.Exec(ctx, `
		update user_threads set read_through = replied_at - interval '1 minute'
		where login = $1 and thread_id = any($2::uuid[])
	`, dismissedScaleLogin, shape.unreadThreads)
	if err != nil {
		t.Fatalf("put the thread marks back: %v", err)
	}
	if tag.RowsAffected() != int64(len(shape.unreadThreads)) {
		t.Fatalf("put back %d thread marks, want %d", tag.RowsAffected(), len(shape.unreadThreads))
	}
	writeInTx(t, shape.database, func(tx pgx.Tx) error {
		deleted, err := tx.Exec(ctx, `
			delete from user_agent_reply_read where login = $1 and reply_id = any($2::uuid[])
		`, dismissedScaleLogin, shape.unreadReplies)
		if err != nil {
			return fmt.Errorf("put the agent marks back: %w", err)
		}
		if deleted.RowsAffected() != int64(len(shape.unreadReplies)) {
			return fmt.Errorf("put back %d agent marks, want %d", deleted.RowsAffected(), len(shape.unreadReplies))
		}
		at := time.Now().UTC()
		ended := make([]graphstore.Tuple, 0, len(shape.unreadReplies))
		for _, reply := range shape.unreadReplies {
			ended = append(ended, personMark(model.Actor{Kind: "user", ID: dismissedScaleLogin},
				graphstore.TupleReplyRead, graphstore.KindMessage, reply, at, false, graphstore.NoEvent))
		}
		return graphstore.FromPerson(ctx, tx, at, ended)
	})
}

// dismissedScaleReading is one operation's 20 runs: the two percentiles and every reading, so a
// number in the table can be read against the spread it came from.
type dismissedScaleReading struct {
	p50  time.Duration
	p95  time.Duration
	runs []time.Duration
}

// measureDismissedScale runs one operation 20 times and answers its percentiles, with one warm-up
// run that is not counted: the first run of a statement on a fresh connection pays for the pages
// every run after it finds cached, and a measurement of the pile is not a measurement of a cold
// cache.
func measureDismissedScale(
	t *testing.T, shape *dismissedScaleShape, operation string,
	run dismissedScaleRun,
) dismissedScaleReading {
	t.Helper()
	ctx := context.Background()
	run(ctx, t, shape)
	taken := make([]time.Duration, 0, dismissedScaleRuns)
	for range dismissedScaleRuns {
		taken = append(taken, run(ctx, t, shape))
	}
	sorted := slices.Clone(taken)
	slices.Sort(sorted)
	reading := dismissedScaleReading{
		p50:  percentileOfDismissedScale(sorted, 0.50),
		p95:  percentileOfDismissedScale(sorted, 0.95),
		runs: taken,
	}
	t.Logf("%s @ %s: p50 %.1f ms, p95 %.1f ms over %d runs",
		operation, shape.name, msOf(reading.p50), msOf(reading.p95), dismissedScaleRuns)
	return reading
}

// percentileOfDismissedScale is the nearest-rank percentile of sorted readings: the ceil(q*n)th,
// so p50 of 20 is the tenth and p95 the nineteenth. No interpolation, so every number printed is
// a run that happened.
func percentileOfDismissedScale(sorted []time.Duration, quantile float64) time.Duration {
	rank := int(math.Ceil(quantile*float64(len(sorted)))) - 1
	return sorted[max(rank, 0)]
}

func msOf(taken time.Duration) float64 {
	return float64(taken.Microseconds()) / 1000
}

// turnJITOff sets jit = off on the database databaseURL names, which a failure calls what: `alter
// database ... set jit = off`. Every session opened on that database afterwards starts with it,
// whoever opens it: a pool of this process, or a build's own process given only the URL. A session
// already open keeps the value it opened with, so a measurement turns JIT off before any pool or
// process it times opens one.
//
// Production's Postgres, Cloud SQL, runs no JIT, and compiling a plan inflates a reading: in a
// builds Job, whose Postgres compiles by default, JIT took 129 ms in a references read whose
// median run was 122.5 ms, a read of 25.7 ms at p50 with jit = off.
func turnJITOff(t *testing.T, what, databaseURL string) {
	t.Helper()
	ctx := context.Background()
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to %s to turn JIT off: %v", what, err)
	}
	defer connection.Close(ctx)
	var name string
	if err := connection.QueryRow(ctx, `select current_database()`).Scan(&name); err != nil {
		t.Fatalf("read the name of %s: %v", what, err)
	}
	if _, err := connection.Exec(ctx, `alter database `+pgx.Identifier{name}.Sanitize()+` set jit = off`); err != nil {
		t.Fatalf("turn JIT off for %s: %v", what, err)
	}
}

// jitOffStore turns JIT off for database's own database (turnJITOff) and answers a store on a pool
// of its own opened after that, so every session a server built on it reads through, and every
// pool reopened from it (storetest.Traced), holds jit = off. database's own pool already holds the
// session store.Open pinged on, opened before the setting, so it is left to its cleanup.
func jitOffStore(t *testing.T, what string, database *store.Store) *store.Store {
	t.Helper()
	turnJITOff(t, what, database.Pool.Config().ConnString())
	return storetest.Reopened(t, database, func(*pgxpool.Config) {})
}

// requireJITOff reads current_setting('jit') and pg_jit_available() on a new session of the
// database databaseURL names, through the URL its measured pool or process was given, and fails
// naming what unless jit is off. Where pg_jit_available() is false the server cannot compile and
// the setting is moot, but turnJITOff leaves it off all the same, so one rule holds on every
// server. It answers both as the readings' lines print them: "jit=off pg_jit_available=false".
func requireJITOff(t *testing.T, what, databaseURL string) string {
	t.Helper()
	ctx := context.Background()
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to %s to read its jit setting: %v", what, err)
	}
	defer connection.Close(ctx)
	var jit string
	var available bool
	if err := connection.QueryRow(ctx, `select current_setting('jit'), pg_jit_available()`).Scan(&jit, &available); err != nil {
		t.Fatalf("read the jit setting of %s: %v", what, err)
	}
	if jit != "off" {
		t.Fatalf("a new session of %s reads jit = %s (pg_jit_available() = %t), want off: its readings would time the "+
			"plans Postgres compiles, which production's never does (turnJITOff)", what, jit, available)
	}
	return fmt.Sprintf("jit=%s pg_jit_available=%t", jit, available)
}
