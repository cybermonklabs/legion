package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// What the one read's two states that answer every row of their state - state=inbox and
// state=unread - cost as a person's dismissed history grows: the read walks only the tuples
// between the person's nodes and the nodes of the state's rows, one keyed probe a relation kind
// from each (meInboxBuild), and its Agent arm reads the person's live roots off their own index,
// which holds no dismissed one (myAgentLiveRoot, messages_direct_roots_live), so the walk, the
// counts and the Agent arm read the live rows and none of the pile.
//
// Two databases hold one store each, seeded by a14's own helpers (seedDismissedScale) and by the
// same code: the measured person, alice, with a14's Inbox - 540 Reply threads and 60 Agent threads,
// half of each read, and the 1,000 unread threads Mark all read works over - and 40 other people,
// each with 50 threads of their own and 50 Agent conversations. The two differ in alice's pile
// alone: 250 dismissed Reply threads and 250 dismissed Agent threads in one, 5,000 of each in the
// other. The backfill (graphcensus.Backfill) then states every row the seed wrote in SQL in the
// store, as it states an older build's rows, so alice's node holds every tuple her pile and her
// Inbox put there: a participates_in into each root, a dismissed into each dismissed one, and
// read_through, reply_read and delivered_to beside them.
//
// Each read is held, at 5,000 against 250, to five figures of its own (walkScaleFigures): the
// median of 201 timed runs, alternating which database is read first; the rows its plan reads
// from graph_tuples and the buffers those reads access; and the rows and buffers of its Agent arm
// - the reads of messages through messages_direct_roots or messages_direct_roots_live, and of
// user_agent_thread_dismissed. The counts are Postgres's own, taken under EXPLAIN (ANALYZE, BUFFERS)
// of the statement the handler builds (meInboxStatement), under the generic plan the read runs
// (meInboxSettings), the least of three counts after a warm-up; so they do not move with the box.
// The time is the server's own, around handler.ServeHTTP (timeDismissedScaleRequest).

const (
	// walkScaleSmall and walkScaleLarge are alice's pile in the two databases: that many dismissed
	// Reply threads and that many dismissed Agent threads.
	walkScaleSmall = 250
	walkScaleLarge = 5_000
	// The other people of the store, each with walkScaleTheirs threads and walkScaleTheirs Agent
	// conversations of their own.
	walkScalePeople = 40
	walkScaleTheirs = 50
	// walkScaleRuns is how many timed runs each read takes on each database.
	walkScaleRuns = 201
	// The bounds, as multiples of the 250 database's figure (the architect's decision of
	// 2026-11-25): the median, and the rows read from graph_tuples and the Agent arm's rows, and
	// the buffers each of those accesses.
	walkScaleMedianBound  = 1.25
	walkScaleRowsBound    = 1.10
	walkScaleBuffersBound = 1.50
	// walkScaleOlderThan is the minute the other people's threads are aged from, past alice's
	// Inbox (3 minutes) and short of the Mark all read band (2,000).
	walkScaleOlderThan = 500
)

// walkScaleRead is one read the test holds: its name, as every reading of it is printed, and its
// query.
type walkScaleRead struct {
	name  string
	query url.Values
}

func (read walkScaleRead) target() string {
	return "/api/v1/me/inbox?" + read.query.Encode()
}

var walkScaleReads = []walkScaleRead{
	{name: "state=inbox", query: url.Values{"state": {myRepliesStateInbox}}},
	{name: "state=unread", query: url.Values{"state": {myRepliesStateUnread}}},
	{name: "state=unread&limit=1", query: url.Values{"state": {myRepliesStateUnread}, "limit": {"1"}}},
}

// walkScaleWork is what one read's statement read under its generic plan: the rows its scans of
// graph_tuples read, each scan's rows times its loops with what it read and threw away, and the
// buffers those scans accessed; the same of its Agent arm (walkScaleAgentArm); and the buffers the
// whole statement accessed, printed beside them.
type walkScaleWork struct {
	graphRows, graphBuffers float64
	agentRows, agentBuffers float64
	buffers                 float64
}

// walkScaleFigure is one figure the test holds, at both databases, with its bound.
type walkScaleFigure struct {
	what         string
	small, large float64
	bound        float64
	unit         string
}

func (figure walkScaleFigure) ratio() float64 {
	return figure.large / figure.small
}

func (figure walkScaleFigure) String() string {
	return fmt.Sprintf("%s 250=%.2f%s 5,000=%.2f%s x%.2f (bound %.2fx)",
		figure.what, figure.small, figure.unit, figure.large, figure.unit, figure.ratio(), figure.bound)
}

// TestTheOneReadsInboxStatesDoNotGrowWithTheDismissedHistory seeds the two databases, checks every
// answer of the three reads against what the seed makes true, times them, counts their statements'
// work under the generic plan, and holds each read's figures at 5,000 to their bounds against 250,
// each read a subtest and each figure a failure of its own name.
//
// It runs when LEGION_SCALE_TEST is 1 and skips otherwise: two seeds, two backfills and 1,206 timed
// reads cost minutes, which no ordinary run of this package pays.
func TestTheOneReadsInboxStatesDoNotGrowWithTheDismissedHistory(t *testing.T) {
	if os.Getenv(dismissedScaleEnv) != "1" {
		t.Skipf("set %s=1 to run the one read's walk-scale measurement", dismissedScaleEnv)
	}
	small := seedWalkScale(t, "250 dismissed", walkScaleSmall)
	large := seedWalkScale(t, "5,000 dismissed", walkScaleLarge)
	shapes := []*dismissedScaleShape{small, large}
	for _, shape := range shapes {
		t.Logf("walk-scale: %s: %s", shape.name, requireJITOff(t, "the "+shape.name+" database", shape.database.Pool.Config().ConnString()))
	}

	// The answers, before anything is timed: what each read answers is what the seed makes true,
	// and the two databases answer the same entries, each with the same relations, whatever the
	// pile.
	for _, read := range walkScaleReads {
		signatures := make([]map[string]int, len(shapes))
		for index, shape := range shapes {
			signatures[index] = requireWalkScaleAnswer(t, shape, read)
		}
		if fmt.Sprint(signatures[0]) != fmt.Sprint(signatures[1]) {
			t.Fatalf("%s answers entries of other relations at %s than at %s:\n%v\n%v",
				read.name, large.name, small.name, signatures[1], signatures[0])
		}
	}

	// The work, under the generic plan the read runs.
	work := map[string]walkScaleWork{}
	for _, read := range walkScaleReads {
		for _, shape := range shapes {
			counted, text := countWalkScale(t, shape, read)
			work[read.name+" @ "+shape.name] = counted
			t.Logf("walk-scale: %s @ %s, generic plan: graph_tuples %.0f rows and %.0f buffers, Agent arm %.0f rows and %.0f buffers, the statement %.0f buffers:\n%s",
				read.name, shape.name, counted.graphRows, counted.graphBuffers, counted.agentRows, counted.agentBuffers, counted.buffers, text)
		}
	}

	// The times: each read 201 times on each database, the two taken one after the other and the
	// first of them alternating, after one warm-up run of each.
	medians := map[string]time.Duration{}
	for _, read := range walkScaleReads {
		taken := map[*dismissedScaleShape][]time.Duration{}
		for _, shape := range shapes {
			runDismissedScaleRead(t, shape, read.target())
		}
		for run := range walkScaleRuns {
			order := shapes
			if run%2 == 1 {
				order = []*dismissedScaleShape{large, small}
			}
			for _, shape := range order {
				taken[shape] = append(taken[shape], runDismissedScaleRead(t, shape, read.target()))
			}
		}
		for _, shape := range shapes {
			sorted := slices.Sorted(slices.Values(taken[shape]))
			medians[read.name+" @ "+shape.name] = percentileOfDismissedScale(sorted, 0.50)
			t.Logf("walk-scale: %s @ %s: median %.2f ms, p95 %.2f ms over %d runs", read.name, shape.name,
				msOf(percentileOfDismissedScale(sorted, 0.50)), msOf(percentileOfDismissedScale(sorted, 0.95)), len(sorted))
		}
	}

	for _, read := range walkScaleReads {
		figures := walkScaleFigures(read, medians, work, small, large)
		for _, figure := range figures {
			t.Logf("walk-scale: %s: %s", read.name, figure)
		}
		t.Run(read.name, func(t *testing.T) {
			for _, figure := range figures {
				if figure.small <= 0 {
					t.Errorf("%s at %s is %.2f: a figure of nothing is a read this test could not see, so nothing was measured",
						figure.what, small.name, figure.small)
					continue
				}
				if figure.ratio() > figure.bound {
					t.Errorf("%s: %s grows with the dismissed history: %s", read.name, figure.what, figure)
				}
			}
		})
	}
}

// walkScaleFigures are one read's figures at the two databases, each with the bound it is held to.
func walkScaleFigures(
	read walkScaleRead, medians map[string]time.Duration, work map[string]walkScaleWork, small, large *dismissedScaleShape,
) []walkScaleFigure {
	at := func(shape *dismissedScaleShape) walkScaleWork { return work[read.name+" @ "+shape.name] }
	return []walkScaleFigure{
		{what: "the median", small: msOf(medians[read.name+" @ "+small.name]), large: msOf(medians[read.name+" @ "+large.name]),
			bound: walkScaleMedianBound, unit: " ms"},
		{what: "the rows read from graph_tuples", small: at(small).graphRows, large: at(large).graphRows, bound: walkScaleRowsBound},
		{what: "the buffers of graph_tuples", small: at(small).graphBuffers, large: at(large).graphBuffers, bound: walkScaleBuffersBound},
		{what: "the Agent arm's rows", small: at(small).agentRows, large: at(large).agentRows, bound: walkScaleRowsBound},
		{what: "the Agent arm's buffers", small: at(small).agentBuffers, large: at(large).agentBuffers, bound: walkScaleBuffersBound},
	}
}

// seedWalkScale builds one database: a14's shape with pile dismissed threads of each kind
// (seedDismissedScale), the other people's threads and conversations (seedWalkScaleOthers), the
// backfill of every row the seed wrote in SQL, and fresh statistics; and it holds alice's node to
// the pile before anything reads it.
func seedWalkScale(t *testing.T, name string, pile int) *dismissedScaleShape {
	t.Helper()
	started := time.Now()
	shape := seedDismissedScale(t, name, pile, pile)
	seedWalkScaleOthers(t, shape)
	ctx := context.Background()
	var out strings.Builder
	report, err := graphcensus.Backfill(ctx, shape.database, &out)
	if err != nil {
		t.Fatalf("backfill the %s database: %v\n%s", name, err, out.String())
	}
	for _, kind := range report.Kinds {
		if !kind.Complete {
			t.Fatalf("the backfill of the %s database left %s owing %d rows:\n%s", name, kind.Kind, kind.Remaining, out.String())
		}
	}
	if _, err := shape.database.Pool.Exec(ctx, "vacuum analyze"); err != nil {
		t.Fatalf("analyze the %s database: %v", name, err)
	}
	// The pile in the store: a dismissed tuple from alice's node into the root of each dismissed
	// thread of either kind, which is what a walk of every tuple of hers read for each state.
	var dismissed int
	if err := shape.database.Pool.QueryRow(ctx, `
		select count(*) from (
			select distinct on (t.to_kind, t.to_id) t.asserted
			from graph_tuples t
			where t.from_kind = $1 and t.from_id = $2 and t.kind = $3
			order by t.to_kind, t.to_id, t.seq desc
		) newest where newest.asserted
	`, graphstore.KindPerson, dismissedScaleLogin, graphstore.TupleDismissed).Scan(&dismissed); err != nil {
		t.Fatalf("count alice's dismissals in the %s database's store: %v", name, err)
	}
	if dismissed != 2*pile {
		t.Fatalf("alice's node in the %s database holds %d current dismissed tuples, want the %d the pile dismissed", name, dismissed, 2*pile)
	}
	kinds, err := readStrings(ctx, shape.database.Pool, `
		select tuple.kind || ' ' || tuple.direction || ' ' || count(*)
		from (
			select t.kind, case when t.from_kind = $1 and t.from_id = $2 then 'out' else 'in' end as direction
			from graph_tuples t
			where (t.from_kind = $1 and t.from_id = $2) or (t.to_kind = $1 and t.to_id = $2)
		) tuple
		group by tuple.kind, tuple.direction order by tuple.kind, tuple.direction
	`, graphstore.KindPerson, dismissedScaleLogin)
	if err != nil {
		t.Fatalf("read alice's tuples in the %s database: %v", name, err)
	}
	var tuples int64
	if err := shape.database.Pool.QueryRow(ctx, `select count(*) from graph_tuples`).Scan(&tuples); err != nil {
		t.Fatalf("count the %s database's tuples: %v", name, err)
	}
	t.Logf("walk-scale: seeded %s in %s: %d tuples in the store, alice's by kind and end: %s",
		name, time.Since(started).Round(time.Second), tuples, strings.Join(kinds, ", "))
	return shape
}

// seedWalkScaleOthers writes the other people's history: for each of walkScalePeople logins,
// walkScaleTheirs comment threads - a root of theirs and a session's reply under it, as a14's
// dismissedScaleCommentPairs writes alice's - and walkScaleTheirs Agent conversations - a one-off
// message of theirs to a session of their own and that session's reply, as
// dismissedScaleMessagePairs writes hers. None of it names alice.
func seedWalkScaleOthers(t *testing.T, shape *dismissedScaleShape) {
	t.Helper()
	ctx := context.Background()
	// $1 is how many people, $2 how many of each they write and $3 the minute their writes are aged
	// from, in both statements.
	const people = `
		with people as (
			select 'walk-scale-' || lpad(p::text, 2, '0') as login,
			       '0c0c0c0c-0000-4000-8000-' || lpad(p::text, 12, '0') as session
			from generate_series(1, $1::int) p
		)`
	comments, err := shape.database.Pool.Exec(ctx, people+`,
		roots as (
			insert into comments (issue_key, author, body, created_at)
			select $4, jsonb_build_object('kind', 'user', 'id', people.login), people.login || ' thread ' || g,
			       now() - (($3::int + 2 * g + 1) || ' minutes')::interval
			from people cross join generate_series(1, $2::int) g
			returning id, body, created_at
		)
		insert into comments (issue_key, author, body, reply_to, created_at)
		select $4, jsonb_build_object('kind', 'session', 'id', $5::text),
		       replace(roots.body, ' thread ', ' reply '), roots.id, roots.created_at + interval '1 minute'
		from roots
	`, walkScalePeople, walkScaleTheirs, walkScaleOlderThan, shape.issue, dismissedScaleSession)
	if err != nil {
		t.Fatalf("seed the other people's threads on %s: %v", shape.name, err)
	}
	messages, err := shape.database.Pool.Exec(ctx, people+`,
		roots as (
			insert into messages (issue_key, author, body, target, created_at)
			select null, jsonb_build_object('kind', 'user', 'id', people.login), people.login || ' ask ' || g,
			       'session:' || people.session, now() - (($3::int + 2 * g + 1) || ' minutes')::interval
			from people cross join generate_series(1, $2::int) g
			returning id, body, target, created_at
		)
		insert into messages (issue_key, author, body, in_reply_to, target, created_at)
		select null, jsonb_build_object('kind', 'session', 'id', substr(roots.target, length('session:') + 1)),
		       replace(roots.body, ' ask ', ' answer '), roots.id, roots.target, roots.created_at + interval '1 minute'
		from roots
	`, walkScalePeople, walkScaleTheirs, walkScaleOlderThan)
	if err != nil {
		t.Fatalf("seed the other people's Agent conversations on %s: %v", shape.name, err)
	}
	if want := int64(walkScalePeople * walkScaleTheirs); comments.RowsAffected() != want || messages.RowsAffected() != want {
		t.Fatalf("seeded %d replies to the other people's threads and %d to their conversations on %s, want %d of each",
			comments.RowsAffected(), messages.RowsAffected(), shape.name, want)
	}
}

// runDismissedScaleRead times one read of target on shape, the server's own time for it.
func runDismissedScaleRead(t *testing.T, shape *dismissedScaleShape, target string) time.Duration {
	t.Helper()
	taken, _ := timeDismissedScaleRequest(context.Background(), t, shape, http.MethodGet, target, nil)
	return taken
}

// requireWalkScaleAnswer holds one read's answer on shape to what the seed makes true, each spelled
// here apart from the read: the entries are alice's rows of the state, her undismissed user_threads
// rows and her undismissed one-off roots holding a session's reply, unread or not as the state asks;
// every entry carries her participates_in into it and none her dismissed; the total is the state's
// count; and a page of one is the first entry of the whole state. It answers the entries by the
// relations each carries, as a count of each set of kinds, which the caller compares between the
// two databases.
func requireWalkScaleAnswer(t *testing.T, shape *dismissedScaleShape, read walkScaleRead) map[string]int {
	t.Helper()
	ctx := context.Background()
	unread := read.query.Get("state") == myRepliesStateUnread
	threads, err := readStrings(ctx, shape.database.Pool, `
		select thread_id::text from user_threads
		where login = $1 and replied_at is not null and dismissed_at is null and ($2::boolean is false or unread)
	`, dismissedScaleLogin, unread)
	if err != nil {
		t.Fatalf("read alice's threads on %s: %v", shape.name, err)
	}
	roots, err := readStrings(ctx, shape.database.Pool, `
		select m.id::text from messages m
		where m.issue_key is null and m.in_reply_to is null and m.target like 'session:%'
		  and m.author->>'kind' = 'user' and lower(m.author->>'id') = $1
		  and not exists (select 1 from user_agent_thread_dismissed d where d.login = $1 and d.message_id = m.id)
		  and exists (
			select 1 from messages reply
			where reply.in_reply_to = m.id and reply.author->>'kind' = 'session'
			  and ($2::boolean is false or not exists (
				select 1 from user_agent_reply_read r where r.login = $1 and r.reply_id = reply.id))
		  )
	`, dismissedScaleLogin, unread)
	if err != nil {
		t.Fatalf("read alice's Agent roots on %s: %v", shape.name, err)
	}
	want := map[string]bool{}
	for _, id := range append(threads, roots...) {
		want[id] = true
	}
	rows := dismissedScaleInboxRows()
	if unread {
		rows = dismissedScaleUnreadRows()
	}
	if len(want) != rows {
		t.Fatalf("%s on %s: the seed holds %d of alice's rows of the state, want a14's %d", read.name, shape.name, len(want), rows)
	}

	answer := readMeInboxAs(t, shape.handler, dismissedScaleLogin, read.query)
	if answer.Total != rows {
		t.Fatalf("%s on %s answers a total of %d, want the state's %d rows", read.name, shape.name, answer.Total, rows)
	}
	if read.query.Get("limit") != "" {
		whole := url.Values{"state": {read.query.Get("state")}}
		first := readMeInboxAs(t, shape.handler, dismissedScaleLogin, whole).Entries
		if len(answer.Entries) != 1 || len(first) == 0 || answer.Entries[0].Node != first[0].Node {
			t.Fatalf("%s on %s answers %v, want the first entry of %s alone, %v",
				read.name, shape.name, meInboxNodes(answer), whole.Encode(), first[:min(len(first), 1)])
		}
	}
	signatures := map[string]int{}
	for _, entry := range answer.Entries {
		if (entry.Thread == nil) == (entry.Agent == nil) || !want[entry.Node.ID] {
			t.Fatalf("%s on %s answers %s:%s, which is no row of alice's in the state", read.name, shape.name, entry.Node.Kind, entry.Node.ID)
		}
		var kinds []string
		for _, relation := range entry.Relations {
			kinds = append(kinds, relation.Kind+" "+relation.Direction)
		}
		if !slices.Contains(kinds, graphstore.TupleParticipatesIn+" "+meInboxOutbound) ||
			slices.Contains(kinds, graphstore.TupleDismissed+" "+meInboxOutbound) {
			t.Fatalf("%s on %s answers %s:%s with the relations %v, want alice's participates_in into it and no dismissed",
				read.name, shape.name, entry.Node.Kind, entry.Node.ID, kinds)
		}
		signatures[entry.Node.Kind+": "+strings.Join(kinds, ", ")]++
	}
	if read.query.Get("limit") == "" && len(answer.Entries) != rows {
		t.Fatalf("%s on %s answers %d entries, want the state's %d rows", read.name, shape.name, len(answer.Entries), rows)
	}
	return signatures
}

// countWalkScale counts one read's statement on shape under the generic plan, as the handler builds
// it (meInboxStatement) and its transaction runs it (meInboxSettings): prepared on one connection
// and executed under EXPLAIN (ANALYZE, BUFFERS) in a transaction rolled back, once to warm the
// connection and three times to be counted, the least of each figure held. A plan whose node count
// changes between counts is one the counts cannot be compared over, so it stops the test. It
// answers the plan's text too, which the test prints.
func countWalkScale(t *testing.T, shape *dismissedScaleShape, read walkScaleRead) (walkScaleWork, string) {
	t.Helper()
	ctx := context.Background()
	name := read.name + " @ " + shape.name
	acquired, err := shape.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection to count %s: %v", name, err)
	}
	defer acquired.Release()
	connection := acquired.Conn()
	var horizon int64
	if err := connection.QueryRow(ctx, `select coalesce(max(id), 0) from events`).Scan(&horizon); err != nil {
		t.Fatalf("read the event log's horizon for %s: %v", name, err)
	}
	request, err := parseMeInboxRequest(httptest.NewRequest(http.MethodGet, read.target(), nil))
	if err != nil {
		t.Fatalf("read the query of %s: %v", name, err)
	}
	statement, args, err := meInboxStatement(model.Actor{Kind: "user", ID: dismissedScaleLogin}, horizon, request, false, false, nil)
	if err != nil {
		t.Fatalf("build the statement of %s: %v", name, err)
	}
	const prepared = "walk_scale_generic"
	described, err := connection.Prepare(ctx, prepared, statement)
	if err != nil {
		t.Fatalf("prepare %s: %v", name, err)
	}
	explain := "explain (analyze, buffers, format json) execute " + prepared +
		dismissedScaleLiterals(t, connection, name, described.ParamOIDs, args)
	count := func() (walkScaleWork, int) {
		tx, err := connection.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the count of %s: %v", name, err)
		}
		defer tx.Rollback(ctx)
		for _, setting := range meInboxSettings {
			if _, err := tx.Exec(ctx, setting); err != nil {
				t.Fatalf("%s before the count of %s: %v", setting, name, err)
			}
		}
		var raw []byte
		if err := tx.QueryRow(ctx, explain).Scan(&raw); err != nil {
			t.Fatalf("explain %s: %v", name, err)
		}
		var plans []struct {
			Plan walkScaleNode `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
			t.Fatalf("read the plan of %s: %d plans, %v", name, len(plans), err)
		}
		top := plans[0].Plan
		work := walkScaleWork{buffers: top.SharedHitBlocks + top.SharedReadBlocks}
		nodes := top.everyNode()
		for _, node := range nodes {
			rows := node.Loops * (node.Rows + node.RemovedByFilter + node.RemovedByRecheck)
			buffers := node.SharedHitBlocks + node.SharedReadBlocks
			if walkScaleScan(node) && node.RelationName == "graph_tuples" {
				work.graphRows += rows
				work.graphBuffers += buffers
			}
			if walkScaleAgentArm(node) {
				work.agentRows += rows
				work.agentBuffers += buffers
			}
		}
		return work, len(nodes)
	}
	count()
	least, nodes := count()
	for range dismissedScaleCounts - 1 {
		next, counted := count()
		if counted != nodes {
			t.Fatalf("%s ran %d plan nodes, then %d: its plan changed between counts, so nothing was measured", name, nodes, counted)
		}
		least.graphRows, least.graphBuffers = min(least.graphRows, next.graphRows), min(least.graphBuffers, next.graphBuffers)
		least.agentRows, least.agentBuffers = min(least.agentRows, next.agentRows), min(least.agentBuffers, next.agentBuffers)
		least.buffers = min(least.buffers, next.buffers)
	}
	if err := connection.Deallocate(ctx, prepared); err != nil {
		t.Fatalf("deallocate %s: %v", name, err)
	}
	_, text := explainDismissedScaleGeneric(t, connection, name, statement, args, meInboxSettings...)
	return least, text
}

// walkScaleNode is one node of an analysed plan as the counts read it: analyzedNode's fields and
// the filter a scan applies, which tells the Agent arm's scan of messages from the others.
type walkScaleNode struct {
	NodeType         string          `json:"Node Type"`
	RelationName     string          `json:"Relation Name,omitempty"`
	IndexName        string          `json:"Index Name,omitempty"`
	Filter           string          `json:"Filter,omitempty"`
	Rows             float64         `json:"Actual Rows"`
	Loops            float64         `json:"Actual Loops"`
	RemovedByFilter  float64         `json:"Rows Removed by Filter,omitempty"`
	RemovedByRecheck float64         `json:"Rows Removed by Index Recheck,omitempty"`
	SharedHitBlocks  float64         `json:"Shared Hit Blocks,omitempty"`
	SharedReadBlocks float64         `json:"Shared Read Blocks,omitempty"`
	Plans            []walkScaleNode `json:"Plans,omitempty"`
}

func (node walkScaleNode) everyNode() []walkScaleNode {
	nodes := []walkScaleNode{node}
	for _, child := range node.Plans {
		nodes = append(nodes, child.everyNode()...)
	}
	return nodes
}

// walkScaleScan is whether node reads a table's rows: a scan of the table itself or of one of its
// indexes, the bitmap heap scan standing for the bitmap index scans under it, whose buffers it
// counts.
func walkScaleScan(node walkScaleNode) bool {
	switch node.NodeType {
	case "Seq Scan", "Index Scan", "Index Only Scan", "Bitmap Heap Scan":
		return node.RelationName != ""
	}
	return false
}

// walkScaleAgentArm is whether node is a read of the Agent arm's roots or of their dismissals: a
// scan of user_agent_thread_dismissed, or a read of messages through messages_direct_roots or
// messages_direct_roots_live - an index scan of either, or a bitmap heap scan over one - or a scan
// of the whole table that applies the arm's own test of a root's author (myAgentRootFilter). The
// page's own reads of messages are no part of it: the conversations of the page's roots
// (agent_chain, meInboxPageReads) walk their replies, which at 250 dismissed the generic plan reads
// by a scan of the whole table at each step and at 5,000 by messages_in_reply_to.
func walkScaleAgentArm(node walkScaleNode) bool {
	if !walkScaleScan(node) {
		return false
	}
	switch node.RelationName {
	case "user_agent_thread_dismissed":
		return true
	case "messages":
		if strings.HasPrefix(node.IndexName, "messages_direct_roots") ||
			(node.NodeType == "Seq Scan" && strings.Contains(node.Filter, "author")) {
			return true
		}
		for _, child := range node.everyNode()[1:] {
			if strings.HasPrefix(child.IndexName, "messages_direct_roots") {
				return true
			}
		}
	}
	return false
}
