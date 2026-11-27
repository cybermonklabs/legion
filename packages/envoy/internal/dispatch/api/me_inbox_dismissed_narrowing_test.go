package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The plan TestScanRowsReadsEveryScanNodeOfAPlan reads (testdata/explain-scan-kinds.json) was
// captured from the pod's PostgreSQL 18. The database was one that seedDismissedNarrowing made,
// kept for the capture. The command was:
//
//	psql "<the seeded test database>" -X -A -t -q -c "set enable_seqscan = off" -c "explain (analyze, format json) with marks as materialized (select thread_id, read_through from user_threads where login = 'alice' and dismissed_at > now() - interval '30 minutes') select m.thread_id, (select count(*) from comments c where c.reply_to = m.thread_id) as replies, (select count(*) from marks) as total from marks m"
//
// The two plans of the Dismissed page's statement are not kept as files. The narrowing's page
// test explains both live, with 1,000 dismissed threads and with 250, and sums them with
// scanRowsOf.

// The seed of the narrowing's page test: a pile of each kind of dismissed thread, built with the
// scale seed's own helpers (inbox_dismissed_scale_seed_test.go). 960 Reply threads are dismissed a
// minute apart and 40 Agent roots ten minutes apart, so a page holds both kinds. The ten threads
// at places 196 to 205 of the order are then given one moment, as one bulk Dismiss gives every
// thread it takes, so that tie lies across the first page's edge. Beside the pile lie 1,000
// threads the person alone wrote in, each dismissed by their own reply, which no page lists.
const (
	dismissedNarrowingReplies  = 960
	dismissedNarrowingAgents   = 40
	dismissedNarrowingTieFirst = 196
	dismissedNarrowingTieLast  = 205
	// dismissedNarrowingFewer is the pile the plan's rows are read again at: all 40 Agent roots and
	// the 210 newest Reply threads, so each arm of the cut takes as many rows as it took at 1,000 -
	// the Reply arm the page and the one past it, the Agent arm its 40 - and a statement that reads
	// its page and no more reads no fewer rows with 250 than with 1,000.
	dismissedNarrowingFewer = 250
	// dismissedNarrowingOwnWrites is how many threads the person alone wrote in, each dismissed by
	// their own reply (seedOwnWriteDismissals). Both piles hold them. dismissedNarrowingOwnLabel
	// starts the bodies of their comments.
	dismissedNarrowingOwnWrites = 1000
	dismissedNarrowingOwnLabel  = "own"
	// dismissedNarrowingIndex is the index the Reply arm reads its pile through: the Dismissed
	// state's own, which holds no thread nobody else wrote in (migration 20261123085248).
	dismissedNarrowingIndex = "user_threads_dismissed_replied"
)

// c-dismissed-page: GET /api/v1/me/inbox?state=dismissed answers at most one page of 200 however
// many threads the person has dismissed. With no limit it answers 200, as it does with limit=200,
// newest dismissal first, with a next_cursor; a limit above 200 is refused by its key; the cursor
// walks every dismissed thread of both kinds once, in order, a tie across the first page's edge
// included, and the page that brings the walk to the last of them names no cursor, so the walk
// reads no empty page; each page's total is the page's own entries. The statement of the first
// page reads its pile through user_threads_dismissed_replied and never scans user_threads, under
// the custom plan, the plan of its own arguments, and under the generic plan the read runs
// (meInboxGenericPlan). It reads no more rows with 1,000 dismissed threads than with 250, because
// each arm is cut to the page before the walk (meInboxDismissedCut) and R2's gate is read for the
// page's entries alone (meInboxGateReads). The rows it reads are the actual rows of every scan
// node of its generic plan (scanRowsOf).
//
// Both piles also hold 1,000 threads the person alone wrote in, each dismissed by their own reply
// at a moment newer than every listed dismissal, as R1 writes them (seedOwnWriteDismissals). No
// page holds one. In each plan the scan of user_threads through the index the read uses removes no
// row by its filter, since that index holds none of them (dismissedNarrowingFilteredRows).
func TestTheDismissedNarrowingAnswersAtMostOnePage(t *testing.T) {
	shape := seedDismissedNarrowing(t)
	want := dismissedNarrowingOrder(t, shape)
	if len(want) != dismissedNarrowingReplies+dismissedNarrowingAgents {
		t.Fatalf("the seed dismissed %d threads, want %d", len(want), dismissedNarrowingReplies+dismissedNarrowingAgents)
	}
	ownWrites := dismissedNarrowingOwnWriteNodes(t, shape)
	unlisted := func(name string, nodes []string) {
		t.Helper()
		for _, node := range nodes {
			if ownWrites[node] {
				t.Fatalf("%s holds %s, a thread only the person wrote in, which their own reply dismissed: %v", name, node, nodes)
			}
		}
	}

	dismissed := url.Values{"state": {myRepliesStateDismissed}}
	for _, query := range []url.Values{dismissed, {"state": {myRepliesStateDismissed}, "limit": {"200"}}} {
		answer := readMeInboxAs(t, shape.handler, dismissedScaleLogin, query)
		got := meInboxNodes(answer)
		unlisted("?"+query.Encode(), got)
		if len(got) != myRepliesDismissedPage || answer.Limit != myRepliesDismissedPage {
			t.Fatalf("?%s answers %d entries under limit %d, want %d under %d",
				query.Encode(), len(got), answer.Limit, myRepliesDismissedPage, myRepliesDismissedPage)
		}
		if !slices.Equal(got, want[:myRepliesDismissedPage]) {
			t.Fatalf("?%s answers %v, want the %d newest dismissals %v", query.Encode(), got, myRepliesDismissedPage, want[:myRepliesDismissedPage])
		}
		if answer.Total != len(got) || answer.NextCursor == "" {
			t.Fatalf("?%s answers total %d and cursor %q, want the page's %d and a cursor", query.Encode(), answer.Total, answer.NextCursor, len(got))
		}
	}

	refused := meInboxRequestAs(t, shape.handler, context.Background(), dismissedScaleLogin,
		url.Values{"state": {myRepliesStateDismissed}, "limit": {"500"}})
	if refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"INVALID_QUERY"`) ||
		!strings.Contains(refused.Body.String(), "limit") {
		t.Fatalf("?state=dismissed&limit=500: status=%d body=%s, want 400 INVALID_QUERY naming limit", refused.Code, refused.Body.String())
	}

	// The walk follows each cursor until a page names none. The page whose entries bring it to the
	// last dismissed thread is the last page and names no cursor; a cursor there leads to a page of
	// no entry, which the walk never reads. Each is an error of its own, so a cursor rule that names
	// a cursor on the last page prints both.
	read := []string{}
	query := url.Values{"state": {myRepliesStateDismissed}}
	for page := 1; ; page++ {
		if page > len(want) {
			t.Fatalf("the Dismissed view read %d threads and still names a cursor", len(read))
		}
		answer := readMeInboxAs(t, shape.handler, dismissedScaleLogin, query)
		if answer.Total != len(answer.Entries) || answer.Total == len(want) {
			t.Fatalf("page %d answers total %d over %d entries, want the page it answers and never the pile of %d",
				page, answer.Total, len(answer.Entries), len(want))
		}
		nodes := meInboxNodes(answer)
		unlisted(fmt.Sprintf("page %d", page), nodes)
		if len(nodes) == 0 {
			t.Errorf("page %d answers no entry after %d of the %d dismissed threads: the page before it named a cursor with no thread past it",
				page, len(read), len(want))
		}
		completes := len(read) < len(want) && len(read)+len(nodes) >= len(want)
		read = append(read, nodes...)
		if completes && answer.NextCursor != "" {
			t.Errorf("page %d brings the walk to %d of the %d dismissed threads and names the cursor %q, want no cursor on the last page",
				page, len(read), len(want), answer.NextCursor)
		}
		if answer.NextCursor == "" {
			break
		}
		query.Set("cursor", answer.NextCursor)
	}
	if !slices.Equal(read, want) {
		t.Fatalf("the cursor walk read %d threads, want every one of the %d once, newest dismissal first and then by id: %v",
			len(read), len(want), read)
	}

	all := explainDismissedNarrowing(t, shape, "the first page with 1,000 dismissed")
	putBackOldestReplies(t, shape, len(want)-dismissedNarrowingFewer)
	if fewer := dismissedNarrowingOrder(t, shape); len(fewer) != dismissedNarrowingFewer {
		t.Fatalf("after the put-back %d threads stand dismissed, want %d", len(fewer), dismissedNarrowingFewer)
	}
	dismissedNarrowingOwnWriteNodes(t, shape)
	few := explainDismissedNarrowing(t, shape, "the first page with 250 dismissed")
	t.Logf("the first page's statement reads %.0f rows with %d dismissed and %.0f with %d", all, len(want), few, dismissedNarrowingFewer)
	if all > few {
		t.Fatalf("the first page's statement reads %.0f rows with %d dismissed threads and %.0f with %d, want no more with the larger pile: the arms meet the walk before they are cut",
			all, len(want), few, dismissedNarrowingFewer)
	}
}

// seedDismissedNarrowing seeds the person's two piles, puts the tie across the first page's edge,
// seeds the threads their own reply dismissed beside them, and gives the planner its statistics,
// as the scale seed does.
func seedDismissedNarrowing(t *testing.T) *dismissedScaleShape {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "SCALE", "A pile of dismissed threads", "A spec")
	shape := &dismissedScaleShape{
		name: "one page", handler: handler, database: database, issue: issue.Key,
		dismissedReplies: dismissedNarrowingReplies, dismissedAgents: dismissedNarrowingAgents,
	}
	dismissedScaleCommentPairs(t, shape, dismissedScalePileLabel, dismissedNarrowingReplies, dismissedScalePileAge)
	markDismissedScaleThreads(t, shape, dismissedNarrowingReplies)
	dismissedScaleMessagePairs(t, shape, dismissedScaleAgentPileLabel, dismissedNarrowingAgents, dismissedScaleAgentAge)
	markDismissedScaleAgentThreads(t, shape, dismissedNarrowingAgents)

	ctx := context.Background()
	var tied, agents int
	if err := database.Pool.QueryRow(ctx, `
		with ordered as (
			select kind, id, at, row_number() over (order by at desc, id::uuid desc) as n
			from (`+dismissedNarrowingMarks+`) marked
		), tie as (
			select at from ordered where n = $2
		), threads as (
			update user_threads u set dismissed_at = (select at from tie)
			from ordered o
			where o.n between $2 and $3 and o.kind <> 'message' and u.login = $1 and u.thread_id = o.id::uuid
			returning 1
		), roots as (
			update user_agent_thread_dismissed d set dismissed_at = (select at from tie)
			from ordered o
			where o.n between $2 and $3 and o.kind = 'message' and d.login = $1 and d.message_id = o.id::uuid
			returning 1
		)
		select (select count(*) from threads) + (select count(*) from roots), (select count(*) from roots)
	`, dismissedScaleLogin, dismissedNarrowingTieFirst, dismissedNarrowingTieLast).Scan(&tied, &agents); err != nil {
		t.Fatalf("tie the threads across the first page's edge: %v", err)
	}
	if tied != dismissedNarrowingTieLast-dismissedNarrowingTieFirst+1 || agents == 0 || agents == tied {
		t.Fatalf("tied %d threads, %d of them Agent roots, want %d of both kinds", tied, agents, dismissedNarrowingTieLast-dismissedNarrowingTieFirst+1)
	}
	seedOwnWriteDismissals(t, shape, dismissedNarrowingOwnWrites)
	if _, err := database.Pool.Exec(ctx, "vacuum analyze"); err != nil {
		t.Fatalf("analyze the seed: %v", err)
	}
	return shape
}

// seedOwnWriteDismissals writes count threads the person alone wrote in: a root and their own
// reply under it, so the triggers of migrations 0095 to 0097 make the person's user_threads row
// and no other, with a null replied_at, since nobody else wrote there. Each row then takes what
// R1 writes for that reply (inboxwrite's queueKeepRow, its dismiss arm): dismissed_at at the
// reply's moment, and the read mark moved to it where it stood earlier. The replies are written
// in the 20 seconds before the seed, so each dismissal is newer than every listed one, the newest
// of which is a minute old. The seed holds every row to that before it returns.
func seedOwnWriteDismissals(t *testing.T, shape *dismissedScaleShape, count int) {
	t.Helper()
	ctx := context.Background()
	tag, err := shape.database.Pool.Exec(ctx, `
		with roots as (
			insert into comments (issue_key, author, body, created_at)
			select $1, jsonb_build_object('kind', 'user', 'id', $2::text),
			       $3::text || ' root ' || g,
			       now() - (2 * g + 1) * interval '10 milliseconds'
			from generate_series(1, $4::int) g
			returning id, body, created_at
		)
		insert into comments (issue_key, author, body, reply_to, created_at)
		select $1, jsonb_build_object('kind', 'user', 'id', $2::text),
		       replace(roots.body, ' root ', ' reply '), roots.id,
		       roots.created_at + interval '10 milliseconds'
		from roots
	`, shape.issue, dismissedScaleLogin, dismissedNarrowingOwnLabel, count)
	if err != nil {
		t.Fatalf("seed the threads only the person wrote in: %v", err)
	}
	if tag.RowsAffected() != int64(count) {
		t.Fatalf("seeded %d own replies, want %d", tag.RowsAffected(), count)
	}
	tag, err = shape.database.Pool.Exec(ctx, `
		update user_threads u
		set dismissed_at = reply.created_at,
		    read_through = greatest(u.read_through, reply.created_at)
		from comments root
		join comments reply on reply.reply_to = root.id
		where root.issue_key = $1 and root.body like $3
		  and u.login = $2 and u.thread_id = root.id`,
		shape.issue, dismissedScaleLogin, dismissedNarrowingOwnLabel+" root %")
	if err != nil {
		t.Fatalf("dismiss the threads only the person wrote in: %v", err)
	}
	if tag.RowsAffected() != int64(count) {
		t.Fatalf("dismissed %d threads only the person wrote in, want %d", tag.RowsAffected(), count)
	}
	var newer bool
	if err := shape.database.Pool.QueryRow(ctx, `
		select (select min(dismissed_at) from user_threads
		        where login = $1 and dismissed_at is not null and replied_at is null)
		     > (select max(at) from (`+dismissedNarrowingMarks+`) marked)
	`, dismissedScaleLogin).Scan(&newer); err != nil {
		t.Fatalf("compare the own replies' dismissals with the listed ones: %v", err)
	}
	if !newer {
		t.Fatalf("a thread only the person wrote in is dismissed no later than a listed one")
	}
}

// dismissedNarrowingOwnWriteNodes is every thread the person alone wrote in that their own reply
// dismissed, as "kind:id": a row that carries a dismissal and a null replied_at. It holds the
// seed to dismissedNarrowingOwnWrites of them.
func dismissedNarrowingOwnWriteNodes(t *testing.T, shape *dismissedScaleShape) map[string]bool {
	t.Helper()
	nodes := dismissedScaleStrings(t, shape, `
		select kind || ':' || thread_id::text from user_threads
		where login = $1 and dismissed_at is not null and replied_at is null`, dismissedScaleLogin)
	if len(nodes) != dismissedNarrowingOwnWrites {
		t.Fatalf("the person holds %d dismissed threads only they wrote in, want %d", len(nodes), dismissedNarrowingOwnWrites)
	}
	held := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		held[node] = true
	}
	return held
}

// dismissedNarrowingMarks is every dismissal of the person $1 holds that the Dismissed view lists,
// of both kinds, as the node the one read answers for it: a Reply thread's root by its own kind,
// an Agent root as a message. A Reply thread nobody else wrote in is no row of the view
// (myRepliesDismissed), so its dismissal is left out.
const dismissedNarrowingMarks = `
	select dismissed_at as at, kind, thread_id::text as id from user_threads
	where login = $1 and replied_at is not null and dismissed_at is not null
	union all
	select dismissed_at, 'message', message_id::text from user_agent_thread_dismissed where login = $1`

// dismissedNarrowingOrder is every thread the person has dismissed, as "kind:id", in the order the
// Dismissed view answers them: newest dismissal first, then the root's id as a uuid, descending.
func dismissedNarrowingOrder(t *testing.T, shape *dismissedScaleShape) []string {
	t.Helper()
	return dismissedScaleStrings(t, shape, `
		select kind || ':' || id from (`+dismissedNarrowingMarks+`) marked
		order by at desc, id::uuid desc`, dismissedScaleLogin)
}

// putBackOldestReplies takes the dismissal off the count oldest dismissed Reply threads the
// Dismissed view lists.
func putBackOldestReplies(t *testing.T, shape *dismissedScaleShape, count int) {
	t.Helper()
	tag, err := shape.database.Pool.Exec(context.Background(), `
		update user_threads set dismissed_at = null
		where login = $1 and thread_id in (
			select thread_id from user_threads
			where login = $1 and replied_at is not null and dismissed_at is not null
			order by dismissed_at, thread_id limit $2
		)`, dismissedScaleLogin, count)
	if err != nil {
		t.Fatalf("put back %d threads: %v", count, err)
	}
	if tag.RowsAffected() != int64(count) {
		t.Fatalf("put back %d threads, want %d", tag.RowsAffected(), count)
	}
}

// explainDismissedNarrowing explains the first page's statement as the handler builds it for
// GET /api/v1/me/inbox?state=dismissed with no limit, twice: under meInboxNoJIT alone, so it plans
// for its own arguments (the custom plan), and under the read's own settings, so it runs the
// generic plan (meInboxSettings). It holds each plan to reading the pile through
// user_threads_dismissed_replied, never scanning user_threads, and removing no row by the filter
// of a scan of user_threads (dismissedNarrowingFilteredRows), and answers the rows the generic
// plan reads (scanRowsOf), the plan every execution of the read runs.
func explainDismissedNarrowing(t *testing.T, shape *dismissedScaleShape, name string) float64 {
	t.Helper()
	ctx := context.Background()
	request, err := parseMeInboxRequest(httptest.NewRequest(http.MethodGet, "/api/v1/me/inbox?state="+myRepliesStateDismissed, nil))
	if err != nil {
		t.Fatalf("read the first page's request: %v", err)
	}
	var horizon int64
	if err := shape.database.Pool.QueryRow(ctx, `select coalesce(max(id), 0) from events`).Scan(&horizon); err != nil {
		t.Fatalf("read the event log's horizon: %v", err)
	}
	statement, args, err := meInboxStatement(model.Actor{Kind: "user", ID: dismissedScaleLogin}, horizon, request, false, false, nil)
	if err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
	connection, err := shape.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for %s: %v", name, err)
	}
	defer connection.Release()
	customName, genericName := name+" under the custom plan", name+" under the generic plan"
	custom, customText := explainDismissedScaleStatement(t, connection.Conn(), customName, statement, args, meInboxNoJIT)
	generic, genericText := explainDismissedScaleGeneric(t, connection.Conn(), genericName, statement, args, meInboxSettings...)
	for _, explained := range []struct {
		name string
		plan json.RawMessage
		text string
	}{
		{customName, custom, customText},
		{genericName, generic, genericText},
	} {
		reads := []string{}
		for _, node := range explaintest.Nodes(t, explained.plan) {
			if node.Index == dismissedNarrowingIndex {
				reads = append(reads, node.String())
			}
		}
		t.Logf("%s reads %s by %v", explained.name, dismissedNarrowingIndex, reads)
		if len(reads) == 0 {
			t.Errorf("%s does not read %s:\n%s", explained.name, dismissedNarrowingIndex, explained.text)
		}
		if explaintest.SeqScansRelation(t, explained.plan, "user_threads") {
			t.Errorf("%s scans user_threads:\n%s", explained.name, explained.text)
		}
		if filtered := dismissedNarrowingFilteredRows(t, explained.plan); len(filtered) > 0 {
			t.Errorf("%s: %s, want none: the index the read uses holds threads the Dismissed state leaves out:\n%s",
				explained.name, strings.Join(filtered, "; "), explained.text)
		}
	}
	return scanRowsOf(t, generic)
}

// dismissedNarrowingFilteredRows names each scan of user_threads through an index, in an analysed
// plan, whose filter removed rows, with the rows it removed over every loop (Rows Removed by
// Filter, which scanRowsOf does not count): an index scan by its index, and a bitmap heap scan by
// the indexes its bitmap reads. The Reply arm's scan through the Dismissed state's own index
// removes none, since that index holds only the rows the state lists. Through
// user_threads_dismissed, which holds every dismissed row, it removes each thread nobody else
// wrote in whose dismissal is newer than the page's last row.
func dismissedNarrowingFilteredRows(t *testing.T, plan json.RawMessage) []string {
	t.Helper()
	removed := func(node explaintest.Node) float64 {
		return math.Round(node.RowsRemovedByFilter * max(node.Loops, 1))
	}
	filtered := []string{}
	for _, node := range explaintest.Nodes(t, plan) {
		if node.Relation == "user_threads" && (node.Type == "Index Scan" || node.Type == "Index Only Scan") && removed(node) > 0 {
			filtered = append(filtered, fmt.Sprintf("%s removes %.0f rows by its filter", node, removed(node)))
		}
	}
	for _, scan := range explaintest.BitmapHeapScans(t, plan, "user_threads") {
		if removed(scan.Scan) == 0 {
			continue
		}
		indexes := []string{}
		for _, index := range scan.BitmapIndexScans {
			indexes = append(indexes, index.Index)
		}
		filtered = append(filtered, fmt.Sprintf("%s through %s removes %.0f rows by its filter",
			scan.Scan, strings.Join(indexes, ", "), removed(scan.Scan)))
	}
	return filtered
}

// scanRowsOf is how many rows the scan nodes of an analysed plan read: the actual rows of every
// node whose type is a scan - of a table, an index, a bitmap, a CTE, a subquery, a function - over
// every loop of each, at any depth, an InitPlan's and a SubPlan's included.
func scanRowsOf(t testing.TB, plan json.RawMessage) float64 {
	t.Helper()
	rows := 0.0
	for _, node := range explaintest.Nodes(t, plan) {
		if strings.HasSuffix(node.Type, " Scan") {
			rows += node.Rows * node.Loops
		}
	}
	return math.Round(rows)
}

// F1: scanRowsOf over a plan PostgreSQL 18 wrote (the command at the top of this file). The plan
// holds every kind of node the walk must reach: an InitPlan and a SubPlan under the top node, a
// CTE scan, a bitmap heap scan over a bitmap index scan, and an index-only scan. The SubPlan runs
// once a row, so its loops count. The count was read off the fixture by hand, scan by scan:
//
//	CTE Scan on marks m                                28 rows x 1 loop    28
//	Bitmap Heap Scan on user_threads (CTE marks)       28 rows x 1 loop    28
//	Bitmap Index Scan on user_threads_dismissed        28 rows x 1 loop    28
//	CTE Scan on marks (InitPlan 3)                     28 rows x 1 loop    28
//	Index Only Scan using comments_reply_to (SubPlan)   1 row  x 28 loops  28
//
// The two Aggregate nodes read no rows of their own, so they are not counted.
func TestScanRowsReadsEveryScanNodeOfAPlan(t *testing.T) {
	for _, fixture := range []struct {
		file string
		rows float64
	}{
		{"explain-scan-kinds.json", 140},
	} {
		raw, err := os.ReadFile(filepath.Join("testdata", fixture.file))
		if err != nil {
			t.Fatalf("read %s: %v", fixture.file, err)
		}
		if rows := scanRowsOf(t, explaintest.OnePlan(t, raw)); rows != fixture.rows {
			t.Errorf("%s: scanRowsOf = %.0f, want %.0f", fixture.file, rows, fixture.rows)
		}
	}
}
