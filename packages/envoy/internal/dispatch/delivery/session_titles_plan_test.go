package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// correctedDistinctOnTitlesQuery writes the rule this change settled on -- a session is named by
// the newest event whose stamped title is not empty -- in the shape the read had before the
// lateral: one `distinct on (actor->>'id')` over every listed session at once. It is not the
// statement that shipped. The shipped one read `where actor->>'kind' = 'session' and
// actor->>'id' = any($1)` with no title condition at all and dropped the empty titles in Go,
// which answers the newest event's title whatever that event stamped -- the defect INFRA-1368
// fixes, since a later write carrying none took the session's name away. The title condition is
// written in here so that both sides of the comparison below ask that one corrected question;
// putting the shipped text back would not restore an equivalence, it would assert the defect,
// and the comparison would fail on this fixture, which is built to tell the two questions apart.
//
// It is held letter for letter rather than built from the package's own constant: a statement
// built from that would move with it, and the two sides would agree however the read changed.
const correctedDistinctOnTitlesQuery = `
		select distinct on (actor->>'id')
			actor->>'id' as session_id,
			actor->'origin'->>'session_title' as session_title
		from events
		where actor->>'kind' = 'session' and actor->>'id' = any($1)
			and nullif(actor->'origin'->>'session_title', '') is not null
		order by actor->>'id', created_at desc
	`

// The sessions the fixture below is read for, and what each of them is there to say:
//
//   - titledPlanSession stamped a title once and has written since, so its newest titled row is
//     neither its newest event nor its oldest: the read skips the newer untitled rows and must
//     stop before the older ones.
//   - untitledPlanSession never stamped a title, so the read walks every event it has and
//     answers no row, which is the case the title condition's absence from the index costs.
//   - unknownPlanSession is a session the events table has never heard of, which `cross join`
//     answers no row for, as the caller's map needs.
const (
	titledPlanSession   = "ses_plan_release_sender"
	untitledPlanSession = "ses_plan_never_titled"
	unknownPlanSession  = "ses_plan_unknown"
	titledPlanName      = "Release sender"
)

// sessionTitlePlanSessions is what both statements are read for, in the order the caller would
// pass them.
var sessionTitlePlanSessions = []string{titledPlanSession, untitledPlanSession, unknownPlanSession}

// The fixture's shape, as the bound below counts it. titledPlanSession holds
// titledPlanOlderEvents events older than its titled one, the titled one, and
// titledPlanNewerEvents untitled events newer than it; untitledPlanSession holds
// untitledPlanEvents events and no title at all.
const (
	titledPlanOlderEvents = 6
	titledPlanNewerEvents = 3
	untitledPlanEvents    = 4
)

// sessionTitlePlanIndexRows is every event of the listed sessions the partial index holds: what
// a read that walks each listed session's whole history touches.
const sessionTitlePlanIndexRows = titledPlanOlderEvents + 1 + titledPlanNewerEvents + untitledPlanEvents

// sessionTitlePlanStoppedRows is what a read that stops at each session's newest titled row
// touches instead: titledPlanSession's untitled newer rows and the titled row they sit above --
// its older events are never reached -- and, since the title condition is not in the index,
// every event untitledPlanSession has.
const sessionTitlePlanStoppedRows = titledPlanNewerEvents + 1 + untitledPlanEvents

// newSessionTitlesPlanFixture writes the events both statements are read over: a session whose
// only title is on an older event, a session whose every event carries none, a user actor
// sharing the titled session's id (the partial index holds no such row, and neither statement
// may answer from it), and a session nobody asked about. seedTitlesIssue clears the issue's
// events first, so a run against the database an earlier run left reads only these.
func newSessionTitlesPlanFixture(t *testing.T) (*store.Pool, context.Context) {
	t.Helper()
	pool, ctx := deliveryTestPool(t)
	seedTitlesIssue(t, ctx, pool)

	at := time.Date(2024, 5, 1, 9, 0, 0, 0, time.UTC)
	minute := 0
	next := func() time.Time {
		minute++
		return at.Add(time.Duration(minute) * time.Minute)
	}
	untitled := actorJSON(t, model.Actor{Kind: "session", ID: titledPlanSession})
	for range titledPlanOlderEvents {
		writeSessionEvent(t, ctx, pool, next(), untitled)
	}
	writeSessionEvent(t, ctx, pool, next(), actorJSON(t, model.Actor{
		Kind: "session", ID: titledPlanSession, Origin: &model.ActorOrigin{SessionTitle: titledPlanName},
	}))
	for range titledPlanNewerEvents {
		writeSessionEvent(t, ctx, pool, next(), untitled)
	}
	for range untitledPlanEvents {
		writeSessionEvent(t, ctx, pool, next(), actorJSON(t, model.Actor{Kind: "session", ID: untitledPlanSession}))
	}
	writeSessionEvent(t, ctx, pool, next(), actorJSON(t, model.Actor{
		Kind: "user", ID: titledPlanSession, Origin: &model.ActorOrigin{SessionTitle: "A person, not the session"},
	}))
	writeSessionEvent(t, ctx, pool, next(), actorJSON(t, model.Actor{
		Kind: "session", ID: "ses_plan_not_asked_for", Origin: &model.ActorOrigin{SessionTitle: "Unasked for"},
	}))
	return pool, ctx
}

// sessionTitleRows is one string per row of statement, sorted. Neither statement orders its
// rows and the caller reads the answer as a map, so the comparison is of the set of rows.
func sessionTitleRows(t *testing.T, ctx context.Context, pool *store.Pool, statement string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, statement, sessionTitlePlanSessions)
	if err != nil {
		t.Fatalf("run the session titles statement: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatalf("read a session titles row: %v", err)
		}
		got = append(got, fmt.Sprint(values...))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the session titles rows: %v", err)
	}
	slices.Sort(got)
	return got
}

// titlePlanNode is the part of an `EXPLAIN (ANALYZE, FORMAT JSON)` plan tree that explaintest
// does not read: how many times a node ran, how many rows each of those runs answered and how
// many each threw away, under the index the node reads through, which is how the one node this
// test counts is picked out. `Actual Loops`, `Actual Rows` and `Rows Removed by Filter` are only
// in an ANALYZE plan, which is why this test runs the statement rather than only planning it.
// Both counts are averages over the loops, so a total is the average times the loops. Nothing
// else of a node is read here: whether the plan holds one plan, whether a scan in it is unkeyed,
// and which index each scan reads under which condition are explaintest's own checks (OnePlan,
// ScansWithoutIndexCond, IndexKeys), which the test below runs over the same plan.
type titlePlanNode struct {
	IndexName string          `json:"Index Name,omitempty"`
	Rows      float64         `json:"Actual Rows"`
	Loops     float64         `json:"Actual Loops"`
	Filtered  float64         `json:"Rows Removed by Filter"`
	Plans     []titlePlanNode `json:"Plans,omitempty"`
}

// everyNode is every node of the plan, the subplan subtrees included.
func (node titlePlanNode) everyNode() []titlePlanNode {
	nodes := []titlePlanNode{node}
	for _, child := range node.Plans {
		nodes = append(nodes, child.everyNode()...)
	}
	return nodes
}

// touched is every row the node read: the ones it answered and the ones its filter threw away,
// over all of its loops.
func (node titlePlanNode) touched() float64 {
	return (node.Rows + node.Filtered) * node.Loops
}

// analyzedTitlePlan runs statement under `EXPLAIN (ANALYZE, FORMAT JSON)` with sequential scans
// off, in a transaction of its own that is rolled back, and answers the plan for explaintest's
// checks, its root node for the counts, and the plan's text, which a failure prints. The
// fixture holds a handful of rows, which is read fastest whole however the statement is
// written, so without that setting the planner answers a sequential scan whatever indexes exist
// and the plan says nothing about the statement; with it, a read that no index serves still
// shows up, as a scan carrying no index condition.
//
// Everything explaintest answers it is asked for, here and in the test below. What is left in
// this file is the ANALYZE run and the counters above: explaintest reads a plan's shape -- its
// scans, their indexes and their conditions -- and not how many times a node ran or how many
// rows it touched, which is this test's whole subject. The api package runs its own ANALYZE for
// the same subject (analyzePlan with planWithoutSeqScans, issue_list_plan_test.go and
// query_plan_test.go); those are unexported test helpers of another package, which no import
// reaches, and lifting them into explaintest would have to serve api's node fields (Parent
// Relationship and the join spine it walks) as well as these counters, which is a larger change
// than this one.
func analyzedTitlePlan(
	t *testing.T, ctx context.Context, pool *store.Pool, statement string, arguments ...any,
) (json.RawMessage, titlePlanNode, string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("disable sequential scans: %v", err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (analyze, format json) "+statement, arguments...).Scan(&raw); err != nil {
		t.Fatalf("explain the statement: %v", err)
	}
	var plans []struct {
		Plan titlePlanNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("decode the explain answer (%v): %s", err, raw)
	}
	return explaintest.OnePlan(t, raw), plans[0].Plan, string(raw)
}

// The two shapes of the corrected rule answer the same rows: the newest non-empty title each
// session stamped, no row for a session that never stamped one, no row for a session the events
// table has never heard of, and nothing from a row whose actor is not a session. That is what
// the move from `distinct on` to the lateral is held to -- it changed what the read costs (the
// plan test below) and not what it answers.
//
// It is no equivalence with the statement that shipped, and must never be made into one. That
// statement carried no title condition and answered the newest event's title whatever it held
// (correctedDistinctOnTitlesQuery above), which is the defect this change fixes; the lateral
// deliberately answers something else, and on this fixture the two differ -- the titled
// session's newest event carries no title at all.
func TestBothShapesOfTheCorrectedSessionTitlesRuleAnswerTheSameRows(t *testing.T) {
	pool, ctx := newSessionTitlesPlanFixture(t)
	want := sessionTitleRows(t, ctx, pool, correctedDistinctOnTitlesQuery)
	// fmt.Sprint writes no space between two strings, so a row reads as its id and its title
	// run together; both sides are built the same way, which is all the comparison needs.
	if len(want) != 1 || want[0] != fmt.Sprint(titledPlanSession, titledPlanName) {
		t.Fatalf("the `distinct on` shape of the rule answered %v; want the one session that stamped a title", want)
	}
	if got := sessionTitleRows(t, ctx, pool, resolveSessionTitlesQuery); !slices.Equal(got, want) {
		t.Fatalf("the session titles read answered\n%v\nwant the rows the `distinct on` shape of the same rule answered\n%v", got, want)
	}
}

// The session titles read reaches events only through events_session_actor_created_at_idx, the
// partial index on ((actor->>'id'), created_at desc) for a session actor, and reads it once for
// each session asked for, stopping at that session's newest titled row. Naming the index alone
// decides nothing here: the `distinct on` read this replaced searched the same index under the
// same index condition, and read every event of every listed session through it. The lateral is
// what stops the read early, and the plan says so in the loops of the one node that reads events
// and in the rows that node touched.
//
// What this proves is a bound, not one row a session, and that is where it differs from its
// model in the api package: the title condition is not in the index, so the read skips a
// session's untitled rows on the way to its titled one, and a session that never stamped a title
// reads its own events to the end. So the fixture's titled session is given six events older
// than its titled one that the read must never reach, and the assertion is that the one read
// node touched no more rows than stopping at each session's newest titled row costs -- here 8 of
// the 14 events the listed sessions have. It does not prove the read answers at most one row a
// loop (it does, but that is the `limit 1` the plan would still show after a rewrite that lost
// the early stop), and it says nothing about a session whose every event is untitled, whose
// whole history this read walks by design.
func TestTheSessionTitlesReadEventsThroughTheSessionActorIndex(t *testing.T) {
	pool, ctx := newSessionTitlesPlanFixture(t)
	plan, root, raw := analyzedTitlePlan(t, ctx, pool, resolveSessionTitlesQuery, sessionTitlePlanSessions)
	if unkeyed := explaintest.ScansWithoutIndexCond(t, plan); len(unkeyed) > 0 {
		t.Errorf("the session titles read reads %v, which no index serves:\n%s", unkeyed, plan)
	}
	const index = "events_session_actor_created_at_idx"
	explaintest.NewIndexKeys(pool).RequireIndexKeyed(t, plan, index, "the session titles read", explaintest.CondRequired)

	reads, touched := 0, float64(0)
	for _, node := range root.everyNode() {
		if node.IndexName != index {
			continue
		}
		reads++
		touched += node.touched()
		if node.Loops != float64(len(sessionTitlePlanSessions)) {
			t.Errorf("the session titles read reads events %v times for %d sessions; want one ordered read a session:\n%s",
				node.Loops, len(sessionTitlePlanSessions), raw)
		}
	}
	if reads != 1 {
		t.Errorf("the session titles read reads events through %s %d times; want the lateral's one read:\n%s", index, reads, raw)
	}
	if touched > sessionTitlePlanStoppedRows {
		t.Errorf("the session titles read touched %v rows of %s, of the %d events the listed sessions have; want at most the %d a read that stops at each session's newest titled row costs:\n%s",
			touched, index, sessionTitlePlanIndexRows, sessionTitlePlanStoppedRows, raw)
	}
}
