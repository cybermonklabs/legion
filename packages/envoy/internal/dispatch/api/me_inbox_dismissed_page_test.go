package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The two piles TestTheDismissedPageReadsOnePageUnderTheGenericPlan reads a page of, each seeded by
// the scale test's own helpers (inbox_dismissed_scale_seed_test.go) on a database of its own: 250
// dismissed threads and then 1,000, 40 of them Agent roots and the rest Reply threads. The Reply
// threads are dismissed a minute apart and the Agent roots ten minutes apart, so a page holds both
// kinds, and the newest 200 entries are the same mix at either size. Each arm then takes as many
// rows at 1,000 as at 250 - the Reply arm the page and the one past it (201), since it holds more
// than that after the cursor too, and the Agent arm its 40 - so a statement that reads the page and
// no more reads the same page's rows at either size, and one that reads the pile reads at least
// the 750 more Reply threads the larger pile holds.
const (
	dismissedPageAgents = 40
	dismissedPageFewer  = 250
	dismissedPageMore   = 1_000
	// dismissedPageCursorDepth is the depth the cursor case resumes from: the next_cursor of a
	// page of five, whose entries are all Reply threads (dismissed one to five minutes ago, before
	// the first Agent root at ten), so 205 Reply threads follow it at 250 and its arm still takes
	// 201.
	dismissedPageCursorDepth = 5
	// dismissedPageMargin is how many more rows the larger pile's statement may read than the
	// smaller's. Both read the same page out of arms that take the same rows, so the rows they
	// read differ only where the planner reads a table that is larger at 1,000 - comments,
	// user_threads - otherwise than at 250. It is a quarter of the page's bound, so it lets that
	// planner noise through and no read of the pile: the larger pile holds 750 more dismissed
	// Reply threads, and a cut that reads them reads every one at least once.
	dismissedPageMargin = (dismissedScalePage + 1) / 4
)

// The cuts of the Dismissed view's two arms read one page under the generic plan, the plan every
// execution of the one read runs (meInboxGenericPlan): the statement GET
// /api/v1/me/inbox?state=dismissed&limit=200 runs - built as the handler builds it (meInboxStatement),
// prepared, and its execute explained with ANALYZE under the read's own settings (meInboxSettings:
// JIT off and the generic plan) - takes no more of either arm's index
// (user_threads_dismissed_replied, user_agent_thread_dismissed_login_dismissed_at) than the page
// and the one row past it, and its scan nodes read no more rows with 1,000 dismissed threads than
// with 250, but for dismissedPageMargin. That holds for the first page, for the first page of one
// issue, which reads the Reply arm alone, and for the page after a cursor. Where an owner's test
// the request does not name was bound to an argument on every shape, the generic plan took the
// Reply arm to keep one row, read the whole arm through a bitmap and sorted it (meInboxPageRows).
func TestTheDismissedPageReadsOnePageUnderTheGenericPlan(t *testing.T) {
	t.Parallel()
	piles := []*dismissedScaleShape{seedDismissedPagePile(t, dismissedPageFewer), seedDismissedPagePile(t, dismissedPageMore)}
	page := func() url.Values {
		return url.Values{"state": {myRepliesStateDismissed}, "limit": {fmt.Sprint(dismissedScalePage)}}
	}
	both := []string{"user_threads_dismissed_replied", "user_agent_thread_dismissed_login_dismissed_at"}
	for _, read := range []struct {
		name string
		// query is the request on shape, and indexes the arms' indexes its statement reads.
		query   func(t *testing.T, shape *dismissedScaleShape) url.Values
		indexes []string
	}{
		{"the first page", func(*testing.T, *dismissedScaleShape) url.Values { return page() }, both},
		{"the first page of one issue", func(_ *testing.T, shape *dismissedScaleShape) url.Values {
			query := page()
			query.Set("issue", shape.issue)
			return query
		}, both[:1]},
		{"the page after a cursor", func(t *testing.T, shape *dismissedScaleShape) url.Values {
			first := readMeInboxAs(t, shape.handler, dismissedScaleLogin, url.Values{
				"state": {myRepliesStateDismissed}, "limit": {fmt.Sprint(dismissedPageCursorDepth)},
			})
			if first.NextCursor == "" {
				t.Fatalf("a page of %d on %s names no cursor", dismissedPageCursorDepth, shape.name)
			}
			query := page()
			query.Set("cursor", first.NextCursor)
			return query
		}, both},
	} {
		t.Run(read.name, func(t *testing.T) {
			explained := make([]dismissedPagePlan, len(piles))
			for index, shape := range piles {
				explained[index] = explainDismissedPage(t, shape, read.name, read.query(t, shape))
			}
			fewer, more := explained[0], explained[1]
			taken := func(plan dismissedPagePlan) []float64 {
				rows := make([]float64, 0, len(read.indexes))
				for _, index := range read.indexes {
					rows = append(rows, planRowsOver(t, plan.plan, index))
				}
				return rows
			}
			t.Logf("%s: its scan nodes read %.0f rows with %d dismissed and %.0f with %d; of %v it takes %v and %v; the scan nodes that read otherwise:\n%s",
				read.name, fewer.rows, dismissedPageFewer, more.rows, dismissedPageMore, read.indexes, taken(fewer), taken(more),
				dismissedPageDifference(fewer, more))
			if fewer.threads != more.threads || fewer.agents != more.agents {
				t.Fatalf("%s answers %d Reply and %d Agent rows with %d dismissed and %d and %d with %d, want the same page at either, which is what makes the two counts comparable",
					read.name, fewer.threads, fewer.agents, dismissedPageFewer, more.threads, more.agents, dismissedPageMore)
			}
			bound := float64(dismissedScalePage + 1)
			for _, plan := range explained {
				for _, index := range read.indexes {
					if !planReadsIndex(t, plan.plan, index) {
						t.Errorf("%s with %s does not read %s; its scan nodes:\n%s", read.name, plan.pile, index, plan.scans)
						continue
					}
					if rows := planRowsOver(t, plan.plan, index); rows > bound {
						t.Errorf("%s with %s takes %.0f rows of %s, want at most the page and the one past it, %.0f: the arm is read whole before it is cut; its scan nodes:\n%s",
							read.name, plan.pile, rows, index, bound, plan.scans)
					}
				}
			}
			if more.rows > fewer.rows+dismissedPageMargin {
				t.Errorf("%s: its scan nodes read %.0f rows with %d dismissed and %.0f with %d, want no more than %.0f + %d: the rows it reads grow with the pile\nits scan nodes with %d dismissed:\n%s\nits scan nodes with %d dismissed:\n%s",
					read.name, more.rows, dismissedPageMore, fewer.rows, dismissedPageFewer, fewer.rows, dismissedPageMargin,
					dismissedPageMore, more.scans, dismissedPageFewer, fewer.scans)
			}
			if t.Failed() {
				for _, plan := range explained {
					t.Logf("%s with %s, the generic plan:\n%s", read.name, plan.pile, plan.text)
				}
			}
		})
	}
}

// The pile TestTheDismissedPageWalksFromThePagesOwnNodes dismisses in one Dismiss, and the most rows
// of graph_tuples its first page may read.
const (
	dismissedWalkPile = 1_000
	// dismissedWalkTuples bounds the rows the page's statement reads of graph_tuples, each scan's
	// answer and what its filter removed: a few relations of each of the page's offset + limit + 1
	// nodes. A probe of graph_tuples_as_of_out, led by the person and the kind, reads the person's
	// whole history of its kind instead, 1,000 dismissed tuples or 1,000 read_through ones, for each
	// of the 201 nodes.
	dismissedWalkTuples = 20 * (dismissedScalePage + 1)
)

// dismissedWalkKeyed is how the index condition of a probe keyed by a node of the page starts: its
// index's leading end, the near end or the far end, bound to page_nodes p (meInboxBuild's walk).
var dismissedWalkKeyed = regexp.MustCompile(`^\(\((from|to)_kind = p(_[0-9]+)?\.node_kind\)`)

// The Dismissed page's walk reads each relation between the person and a node of the page from that
// node, whose own tuples of a kind are few (meInboxBuild): over the first page of 1,000 threads
// dismissed in one Dismiss - each with a dismissed and a read_through tuple out of the person, as
// POST /api/v1/me/inbox/dismiss writes them - every scan of graph_tuples the plan runs once for each
// of the page's nodes leads its index condition with that node, and the page reads at most a few
// tuples of each node. A probe led by the person - graph_tuples_current on the whole key, or
// graph_tuples_as_of_out on the person and the kind - is estimated at one row either way, so a
// generic plan that took the person's end on the index took one or the other by the statistics it
// planned under, and graph_tuples_as_of_out reads the person's whole history of a kind for each
// node: in the server's log of CI's chromium-alone runner, six of eleven loads of c-dismissed-window
// read the page in 439 to 477 ms and the other five in 49 to 99 (run 45518823268 at 50f167fa).
func TestTheDismissedPageWalksFromThePagesOwnNodes(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "SCALE", "A pile of dismissed threads", "A spec")
	shape := &dismissedScaleShape{
		name: fmt.Sprintf("%d dismissed in one Dismiss", dismissedWalkPile), handler: handler, database: database,
		issue: issue.Key, dismissedReplies: dismissedWalkPile,
	}
	dismissedScaleCommentPairs(t, shape, dismissedScalePileLabel, dismissedWalkPile, dismissedScalePileAge)
	if dismissed := dismissThrough(t, handler, dismissedScaleLogin, time.Now(), map[string]any{"issue": issue.Key}); dismissed != dismissedWalkPile {
		t.Fatalf("the Dismiss took %d threads, want the pile's %d", dismissed, dismissedWalkPile)
	}
	plan := explainDismissedPage(t, shape, "the first page",
		url.Values{"state": {myRepliesStateDismissed}, "limit": {fmt.Sprint(dismissedScalePage)}})
	if plan.threads != dismissedScalePage {
		t.Fatalf("the first page answers %d Reply rows, want %d", plan.threads, dismissedScalePage)
	}
	var read float64
	var scans, unkeyed []string
	probes := 0
	for _, node := range explaintest.Nodes(t, plan.plan) {
		if node.Relation != "graph_tuples" {
			continue
		}
		rows := (node.Rows + node.RowsRemovedByFilter) * node.Loops
		read += rows
		described := fmt.Sprintf("  %.0f rows over %.0f loops: %s, %s", rows, node.Loops, node, node.IndexCond)
		scans = append(scans, described)
		if node.Loops < dismissedScalePage+1 {
			continue
		}
		probes++
		if !dismissedWalkKeyed.MatchString(node.IndexCond) {
			unkeyed = append(unkeyed, described)
		}
	}
	t.Logf("the first page reads %.0f rows of graph_tuples:\n%s", read, strings.Join(scans, "\n"))
	if probes == 0 {
		t.Fatalf("no scan of graph_tuples runs once for each of the page's %d nodes, so the walk this test holds is not in the plan\n%s",
			dismissedScalePage+1, plan.text)
	}
	if len(unkeyed) > 0 {
		t.Errorf("%d of the walk's %d probes for each of the page's nodes are not keyed by that node, want none:\n%s",
			len(unkeyed), probes, strings.Join(unkeyed, "\n"))
	}
	if read > dismissedWalkTuples {
		t.Errorf("the first page reads %.0f rows of graph_tuples, want at most %d: its walk reads the person's tuples of a kind for each of the page's nodes\n%s",
			read, dismissedWalkTuples, plan.text)
	}
}

// seedDismissedPagePile seeds one pile of pile dismissed threads, dismissedPageAgents of them Agent
// roots, on a server and database of its own, gives the planner its statistics as the scale seed
// does, and holds the tables to the pile before anything is explained.
func seedDismissedPagePile(t *testing.T, pile int) *dismissedScaleShape {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "SCALE", "A pile of dismissed threads", "A spec")
	replies := pile - dismissedPageAgents
	shape := &dismissedScaleShape{
		name: fmt.Sprintf("%d dismissed", pile), handler: handler, database: database, issue: issue.Key,
		dismissedReplies: replies, dismissedAgents: dismissedPageAgents,
	}
	dismissedScaleCommentPairs(t, shape, dismissedScalePileLabel, replies, dismissedScalePileAge)
	markDismissedScaleThreads(t, shape, replies)
	dismissedScaleMessagePairs(t, shape, dismissedScaleAgentPileLabel, dismissedPageAgents, dismissedScaleAgentAge)
	markDismissedScaleAgentThreads(t, shape, dismissedPageAgents)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, "vacuum analyze"); err != nil {
		t.Fatalf("analyze %s: %v", shape.name, err)
	}
	var threads, roots int
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from user_threads where login = $1 and dismissed_at is not null),
		       (select count(*) from user_agent_thread_dismissed where login = $1)
	`, dismissedScaleLogin).Scan(&threads, &roots); err != nil {
		t.Fatalf("count %s: %v", shape.name, err)
	}
	if threads != replies || roots != dismissedPageAgents {
		t.Fatalf("%s holds %d dismissed Reply threads and %d Agent roots, want %d and %d", shape.name, threads, roots, replies, dismissedPageAgents)
	}
	return shape
}

// dismissedPagePlan is one statement's analysed generic plan on one pile: the plan and its text, the
// rows its scan nodes read, those nodes each with its rows (scans) and the rows the nodes of each
// name read together (byNode), and the Reply and Agent rows the handler answers the same request
// with. A bitmap heap scan stands for the bitmap index scans under it, as walkScaleScan has it: the
// rows those index scans find are the rows the heap scan reads, so they are counted once, and a
// planner that reads the same rows through a bitmap rather than one index scan reads no more.
type dismissedPagePlan struct {
	pile            string
	plan            json.RawMessage
	text            string
	rows            float64
	scans           string
	byNode          map[string]float64
	threads, agents int
}

// explainDismissedPage answers the request query on shape twice: through the handler, for the rows
// of each kind its page holds, and as the read runs its statement - built by meInboxStatement as
// listMeInbox builds it for this server (no display trees, no stored roster), prepared, and its
// execute explained with ANALYZE under meInboxSettings and so its generic plan
// (explainDismissedScaleGeneric).
func explainDismissedPage(t *testing.T, shape *dismissedScaleShape, name string, query url.Values) dismissedPagePlan {
	t.Helper()
	ctx := context.Background()
	explained := dismissedPagePlan{pile: shape.name, byNode: map[string]float64{}}
	for _, entry := range readMeInboxAs(t, shape.handler, dismissedScaleLogin, query).Entries {
		switch {
		case entry.Thread != nil:
			explained.threads++
		case entry.Agent != nil:
			explained.agents++
		}
	}
	request, err := parseMeInboxRequest(httptest.NewRequest(http.MethodGet, "/api/v1/me/inbox?"+query.Encode(), nil))
	if err != nil {
		t.Fatalf("read the request of %s: %v", name, err)
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
	explained.plan, explained.text = explainDismissedScaleGeneric(t, connection.Conn(), name+" with "+shape.name,
		statement, args, meInboxSettings...)
	type scanned struct {
		node        string
		rows, loops float64
	}
	var read []scanned
	for _, node := range explaintest.Nodes(t, explained.plan) {
		if strings.HasSuffix(node.Type, " Scan") && node.Type != "Bitmap Index Scan" {
			explained.rows += node.Rows * node.Loops
			if node.Rows*node.Loops > 0 {
				described := node.String()
				if node.Alias != "" {
					described += " as " + node.Alias
				}
				read = append(read, scanned{described, node.Rows * node.Loops, node.Loops})
				explained.byNode[described] += node.Rows * node.Loops
			}
		}
	}
	explained.rows = math.Round(explained.rows)
	// The scan nodes that read a row, most rows first, one a line.
	slices.SortStableFunc(read, func(a, b scanned) int { return cmp.Compare(b.rows, a.rows) })
	lines := make([]string, 0, len(read))
	for _, scan := range read {
		lines = append(lines, fmt.Sprintf("  %.0f rows over %.0f loops: %s", scan.rows, scan.loops, scan.node))
	}
	explained.scans = strings.Join(lines, "\n")
	return explained
}

// dismissedPageDifference names each scan node whose rows differ between the two piles' plans, with
// the rows the nodes of that name read in each, one a line.
func dismissedPageDifference(fewer, more dismissedPagePlan) string {
	names := slices.Sorted(maps.Keys(fewer.byNode))
	for name := range more.byNode {
		if _, held := fewer.byNode[name]; !held {
			names = append(names, name)
		}
	}
	lines := []string{}
	for _, name := range names {
		if fewer.byNode[name] != more.byNode[name] {
			lines = append(lines, fmt.Sprintf("  %s: %.0f with %s, %.0f with %s", name, fewer.byNode[name], fewer.pile, more.byNode[name], more.pile))
		}
	}
	return strings.Join(lines, "\n")
}

// TestTheDismissedStateWithNoLimitAnswersOnePage holds GET /api/v1/me/inbox?state=dismissed with no
// limit to the page GET /api/v1/me/replies?state=dismissed answered, myRepliesDismissedPage entries,
// and not to the whole pile: on a pile of dismissedPageFewer dismissed threads the read answers the
// newest 200 with limit 200 and a next_cursor, and the read from that cursor answers the other 50
// and no next_cursor, so every dismissed thread is reached a page at a time and none twice.
func TestTheDismissedStateWithNoLimitAnswersOnePage(t *testing.T) {
	t.Parallel()
	shape := seedDismissedPagePile(t, dismissedPageFewer)
	first := readMeInboxAs(t, shape.handler, dismissedScaleLogin, url.Values{"state": {myRepliesStateDismissed}})
	if len(first.Entries) != myRepliesDismissedPage || first.Limit != myRepliesDismissedPage || first.NextCursor == "" {
		t.Fatalf("state=dismissed with no limit over %d dismissed threads answers %d entries, limit %d and next_cursor %q; want one page of %d, limit %d and a next_cursor",
			dismissedPageFewer, len(first.Entries), first.Limit, first.NextCursor, myRepliesDismissedPage, myRepliesDismissedPage)
	}
	next := readMeInboxAs(t, shape.handler, dismissedScaleLogin, url.Values{
		"state": {myRepliesStateDismissed}, "cursor": {first.NextCursor},
	})
	if want := dismissedPageFewer - myRepliesDismissedPage; len(next.Entries) != want || next.NextCursor != "" {
		t.Fatalf("the page after the first page's next_cursor answers %d entries and next_cursor %q, want the other %d and none",
			len(next.Entries), next.NextCursor, want)
	}
	reached := map[string]bool{}
	for _, entry := range slices.Concat(first.Entries, next.Entries) {
		node := entry.Node.Kind + ":" + entry.Node.ID
		if reached[node] {
			t.Errorf("%s is answered on both pages", node)
		}
		reached[node] = true
	}
	if len(reached) != dismissedPageFewer {
		t.Errorf("the two pages reach %d dismissed threads, want all %d", len(reached), dismissedPageFewer)
	}
}

// TestTheDismissedStateRefusesAnOffsetItsPageCannotCountTo holds GET /api/v1/me/inbox under
// state=dismissed to the bound its statement needs: the page is read to offset + limit + 1, a
// bigint sum, so an offset that takes it past the largest integer is 400 INVALID_QUERY and not the
// 500 Postgres' bigint out of range made of it, and an offset at the bound answers an empty page.
// The other states take no such sum, so the largest offset answers them an empty page too.
func TestTheDismissedStateRefusesAnOffsetItsPageCannotCountTo(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	dismissed := func(limit string, offset int) url.Values {
		query := url.Values{"state": {myRepliesStateDismissed}, "offset": {fmt.Sprint(offset)}}
		if limit != "" {
			query.Set("limit", limit)
		}
		return query
	}
	for _, query := range []url.Values{
		// The largest offset, under a page of one.
		dismissed("1", math.MaxInt),
		// One past the bound of a page of one.
		dismissed("1", math.MaxInt-1),
		// One past the bound of the page of 200 an absent limit reads.
		dismissed("", math.MaxInt-myRepliesDismissedPage),
	} {
		response := meInboxRequestAs(t, handler, context.Background(), "alice", query)
		var refusal struct{ Code, Error string }
		if response.Code != http.StatusBadRequest || json.Unmarshal(response.Body.Bytes(), &refusal) != nil ||
			refusal.Code != "INVALID_QUERY" || !strings.HasPrefix(refusal.Error, "offset must be at most ") {
			t.Errorf("?%s answers %d %s, want 400 INVALID_QUERY naming the bound of offset", query.Encode(), response.Code, response.Body.String())
		}
	}
	for _, query := range []url.Values{
		dismissed("1", math.MaxInt-2),
		dismissed("", math.MaxInt-myRepliesDismissedPage-1),
		{"state": {myRepliesStateInbox}, "limit": {"1"}, "offset": {fmt.Sprint(math.MaxInt)}},
		{"limit": {"1"}, "offset": {fmt.Sprint(math.MaxInt)}},
	} {
		if answer := readMeInboxAs(t, handler, "alice", query); len(answer.Entries) != 0 || answer.NextCursor != "" {
			t.Errorf("?%s answers %d entries and next_cursor %q, want an empty page", query.Encode(), len(answer.Entries), answer.NextCursor)
		}
	}
}
