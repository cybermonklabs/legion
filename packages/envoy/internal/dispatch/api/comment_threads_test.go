package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// specLoopFixture is a corpus of the shape the Verified Linux spec loop files: two proposal
// issues of one project, each with requirement threads anchored on its own targets, threads at
// both states, replies under some of them, and threads of a second project beside them. It is
// what proves a filter returns exactly the threads it names and no others.
type specLoopFixture struct {
	handler  http.Handler
	database *store.Store
	server   *server
	project  string
	proposal struct {
		key      string
		artifact string
	}
	other struct {
		key      string
		artifact string
	}
	elsewhere struct {
		key string
	}
}

// requirementThread opens one requirement thread anchored on target, as the copy program opens
// one per requirement id (dispatch://LINUX-1239).
func requirementThread(t *testing.T, handler http.Handler, issueKey, target, body string) model.Comment {
	t.Helper()
	return createThreadComment(t, handler, issueKey, map[string]any{
		"body":   body,
		"anchor": map[string]any{"artifact": "spec", "quote": target},
	}, "alice")
}

// proposalIssue opens one proposal issue in a project that already exists, the shape the copy
// program gives each proposal: one issue whose document holds an entry for each target.
func proposalIssue(t *testing.T, handler http.Handler, project, title, entries string) struct {
	Key               string `json:"key"`
	PrimaryArtifactID string `json:"primary_artifact_id"`
} {
	t.Helper()
	// force: the two proposals of one project share their entries on purpose, which is what the
	// near-copy gate refuses, and sharing them is the point of the fixture.
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": project, "title": title, "spec": entries, "force": true,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, response)
}

// specLoopEntries is each proposal's document: one line per target the loop files against.
const specLoopEntries = "sys_open holds.\nsys_close holds.\nOther targets\n"

// newSpecLoopFixture seeds the corpus. Each issue's spec quotes its own entries, so a thread can
// anchor on them, and the two projects' entries share a target name on purpose: a project-scoped
// read must not reach the other project's thread of the same name.
func newSpecLoopFixture(t *testing.T) specLoopFixture {
	t.Helper()
	handler, database, _, registered := newRegisteredTestServer(t, testServerOptions{})
	fixture := specLoopFixture{handler: handler, database: database, server: registered, project: "LOOP"}

	proposal := createInteractionIssue(t, fixture.handler, fixture.project, "Proposal DS-exec-context", specLoopEntries)
	fixture.proposal.key, fixture.proposal.artifact = proposal.Key, proposal.PrimaryArtifactID
	other := proposalIssue(t, fixture.handler, fixture.project, "Proposal DS-vfs", specLoopEntries)
	fixture.other.key, fixture.other.artifact = other.Key, other.PrimaryArtifactID
	elsewhere := createInteractionIssue(t, fixture.handler, "OTHERLOOP", "Proposal elsewhere", specLoopEntries)
	fixture.elsewhere.key = elsewhere.Key

	return fixture
}

// decodeThreadPage reads one listing's answer, failing on any status but 200.
func decodeThreadPage(t *testing.T, handler http.Handler, target string) commentThreadPage {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	return decodeBody[commentThreadPage](t, response)
}

// threadBodies is the page's root bodies in the order it answered them, which is what a test
// compares: the body carries the requirement line, so it names the thread.
func threadBodies(page commentThreadPage) []string {
	bodies := make([]string, 0, len(page.Threads))
	for _, thread := range page.Threads {
		bodies = append(bodies, thread.Root.Body)
	}
	return bodies
}

func wantBodies(t *testing.T, page commentThreadPage, want ...string) {
	t.Helper()
	got := threadBodies(page)
	if len(got) != len(want) {
		t.Fatalf("threads = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("threads = %v, want %v", got, want)
		}
	}
}

// The requirement query by target: exactly the threads anchored on that entry, across every
// issue of the project, and none anchored on another entry, in another project, or replying
// inside a matching thread. Before this route the loop asked this question of GET
// /api/v1/search, which answered it by meaning over the whole corpus (dispatch://INFRA-988).
func TestCommentThreadsByTargetListsOnlyTheThreadsAnchoredOnIt(t *testing.T) {
	fixture := newSpecLoopFixture(t)

	first := requirementThread(t, fixture.handler, fixture.proposal.key, "sys_open holds.", "R:1 open sys_open")
	requirementThread(t, fixture.handler, fixture.proposal.key, "sys_close holds.", "R:2 open sys_close")
	requirementThread(t, fixture.handler, fixture.other.key, "sys_open holds.", "R:3 open sys_open, other proposal")
	requirementThread(t, fixture.handler, fixture.elsewhere.key, "sys_open holds.", "R:4 open sys_open, other project")
	createThreadComment(t, fixture.handler, fixture.proposal.key, map[string]any{
		"body": "A reply inside R:1, which is no thread of its own.", "reply_to": first.ID,
	}, "alice")

	page := decodeThreadPage(t, fixture.handler,
		"/api/v1/comment-threads/by-target?project="+fixture.project+"&target=sys_open+holds.")
	wantBodies(t, page, "R:1 open sys_open", "R:3 open sys_open, other proposal")
	if page.Limit != commentThreadDefaultLimit || page.NextAfter != nil {
		t.Fatalf("page = {limit %d, next_after %v}, want {%d, nil}",
			page.Limit, page.NextAfter, commentThreadDefaultLimit)
	}
	if page.Threads[0].Replies != 1 {
		t.Fatalf("R:1 replies = %d, want 1", page.Threads[0].Replies)
	}
	if page.Threads[1].Replies != 0 {
		t.Fatalf("R:3 replies = %d, want 0", page.Threads[1].Replies)
	}
	// last_reply_at is when a reply was posted and nothing else: the thread with one carries it,
	// the thread with none carries null rather than its root's own time.
	if page.Threads[0].LastReplyAt == nil || !page.Threads[0].LastReplyAt.After(page.Threads[0].Root.CreatedAt) {
		t.Fatalf("R:1 last_reply_at = %v, want a time after its root's %v",
			page.Threads[0].LastReplyAt, page.Threads[0].Root.CreatedAt)
	}
	if page.Threads[1].LastReplyAt != nil {
		t.Fatalf("R:3 last_reply_at = %v, want null for a thread with no reply", page.Threads[1].LastReplyAt)
	}

	// The same target read of one issue alone reaches that issue's thread and no other's.
	wantBodies(t, decodeThreadPage(t, fixture.handler,
		"/api/v1/comment-threads/by-target?issue="+fixture.proposal.key+"&target=sys_open+holds."),
		"R:1 open sys_open")

	// A target no entry carries is an empty page, never another entry's threads.
	empty := decodeThreadPage(t, fixture.handler,
		"/api/v1/comment-threads/by-target?project="+fixture.project+"&target=sys_read+holds.")
	if len(empty.Threads) != 0 || empty.NextAfter != nil {
		t.Fatalf("unknown target = %v (next_after %v), want an empty page", threadBodies(empty), empty.NextAfter)
	}
}

// The target is matched whole on both sides: the caller's value is not trimmed, so a target
// carrying a space the stored quote does not carry reaches nothing. Trimming it here would make
// an entry whose own text opens or ends with a space unmatchable, since the quote is the
// document's own text.
func TestCommentThreadsByTargetMatchesTheQuoteWhole(t *testing.T) {
	fixture := newSpecLoopFixture(t)
	requirementThread(t, fixture.handler, fixture.proposal.key, "sys_open holds.", "R:5 sys_open")

	wantBodies(t, decodeThreadPage(t, fixture.handler,
		"/api/v1/comment-threads/by-target?project="+fixture.project+"&target=sys_open+holds."),
		"R:5 sys_open")
	for _, padded := range []string{"%20sys_open%20holds.", "sys_open%20holds.%20"} {
		page := decodeThreadPage(t, fixture.handler,
			"/api/v1/comment-threads/by-target?project="+fixture.project+"&target="+padded)
		if len(page.Threads) != 0 {
			t.Fatalf("target %q = %v, want no thread: a target is matched whole, never trimmed",
				padded, threadBodies(page))
		}
	}
}

// The by-target page: its order is the comment listing's, limit cuts it, next_after carries the
// caller to the next page, and the last page carries none.
func TestCommentThreadsByTargetPagesByKey(t *testing.T) {
	fixture := newSpecLoopFixture(t)
	for index := 1; index <= 3; index++ {
		requirementThread(t, fixture.handler, fixture.proposal.key, "sys_open holds.", fmt.Sprintf("R:%d open sys_open", index))
	}

	first := decodeThreadPage(t, fixture.handler,
		"/api/v1/comment-threads/by-target?project="+fixture.project+"&target=sys_open+holds.&limit=2")
	wantBodies(t, first, "R:1 open sys_open", "R:2 open sys_open")
	if first.NextAfter == nil || *first.NextAfter != first.Threads[1].Root.ID {
		t.Fatalf("first page next_after = %v, want its last root %s", first.NextAfter, first.Threads[1].Root.ID)
	}

	second := decodeThreadPage(t, fixture.handler,
		"/api/v1/comment-threads/by-target?project="+fixture.project+
			"&target=sys_open+holds.&limit=2&after="+*first.NextAfter)
	wantBodies(t, second, "R:3 open sys_open")
	if second.NextAfter != nil {
		t.Fatalf("last page next_after = %v, want null", second.NextAfter)
	}
}

// What keyset paging buys over an offset, on the normal path of the by-thread-state route:
// membership is mutable in place, so resolving a thread the first page already answered shifts
// every later row left under an offset and the second page skips one. Paging by key does not.
func TestCommentThreadsByThreadStatePagesWithoutSkippingAResolvedThread(t *testing.T) {
	fixture := newSpecLoopFixture(t)
	roots := make([]model.Comment, 0, 4)
	for index := 1; index <= 4; index++ {
		roots = append(roots, requirementThread(t, fixture.handler, fixture.proposal.key,
			"sys_open holds.", fmt.Sprintf("R:%d open", index)))
	}

	first := decodeThreadPage(t, fixture.handler,
		"/api/v1/comment-threads/by-thread-state?project="+fixture.project+"&state=open&limit=2")
	wantBodies(t, first, "R:1 open", "R:2 open")

	// Between the two reads, a thread of the first page leaves the open set. Under an offset of
	// 2 the second page would start at R:4 and R:3 would never be answered.
	if response := dispatchRequest(t, fixture.handler, http.MethodPost,
		"/api/v1/comments/"+roots[0].ID+"/resolve", nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("resolve R:1: status=%d body=%s", response.Code, response.Body.String())
	}

	second := decodeThreadPage(t, fixture.handler,
		"/api/v1/comment-threads/by-thread-state?project="+fixture.project+
			"&state=open&limit=2&after="+*first.NextAfter)
	wantBodies(t, second, "R:3 open", "R:4 open")
}

// The requirement query by the state of the line's thread: open lists the threads nobody
// resolved, resolved lists the rest, a reply in a resolved thread moves it back to open, and a
// reply is never a thread of its own. The state is the root's: resolving a reply resolves that
// reply alone and leaves the thread open here, which the route's description states and this
// test pins, since the write path is not this change's to move.
func TestCommentThreadsByThreadStateListsOnlyTheThreadsInThatState(t *testing.T) {
	fixture := newSpecLoopFixture(t)

	met := requirementThread(t, fixture.handler, fixture.proposal.key, "sys_open holds.", "R:1 met")
	second := requirementThread(t, fixture.handler, fixture.proposal.key, "sys_close holds.", "R:2 open")
	reopened := requirementThread(t, fixture.handler, fixture.other.key, "Other targets", "R:3 reopened")
	requirementThread(t, fixture.handler, fixture.elsewhere.key, "sys_open holds.", "R:4 open, other project")
	replyInSecond := createThreadComment(t, fixture.handler, fixture.proposal.key, map[string]any{
		"body": "A reply of R:2, resolved on its own below.", "reply_to": second.ID,
	}, "alice")

	for _, id := range []string{met.ID, reopened.ID, replyInSecond.ID} {
		if response := dispatchRequest(t, fixture.handler, http.MethodPost,
			"/api/v1/comments/"+id+"/resolve", nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("resolve %s: status=%d body=%s", id, response.Code, response.Body.String())
		}
	}
	// A reply in a resolved thread reopens it (comment_create.go), so R:3 is open again and the
	// state read has to follow the row rather than the resolve that once happened.
	createThreadComment(t, fixture.handler, fixture.other.key, map[string]any{
		"body": "The owner answers, which reopens R:3.", "reply_to": reopened.ID,
	}, "alice")

	// R:2 stays open although a reply of it is resolved: the state is the root's.
	wantBodies(t, decodeThreadPage(t, fixture.handler,
		"/api/v1/comment-threads/by-thread-state?project="+fixture.project+"&state=open"),
		"R:2 open", "R:3 reopened")
	wantBodies(t, decodeThreadPage(t, fixture.handler,
		"/api/v1/comment-threads/by-thread-state?project="+fixture.project+"&state=resolved"),
		"R:1 met")
	wantBodies(t, decodeThreadPage(t, fixture.handler,
		"/api/v1/comment-threads/by-thread-state?issue="+fixture.other.key+"&state=open"),
		"R:3 reopened")

	// The document narrowing is the listing's own ?artifact=: the threads anchored in one
	// document of the issue, which is how a proposal split over part documents is read.
	wantBodies(t, decodeThreadPage(t, fixture.handler,
		"/api/v1/comment-threads/by-thread-state?project="+fixture.project+"&state=open&artifact="+fixture.proposal.artifact),
		"R:2 open")
}

// The scope and the filter are required, and each refusal names what is wrong rather than
// answering a page the caller cannot tell from the one it asked for.
func TestCommentThreadListingsRefuseAnUnscopedOrUnfilteredRequest(t *testing.T) {
	fixture := newSpecLoopFixture(t)
	for _, refusal := range []struct {
		target string
		code   string
	}{
		{"/api/v1/comment-threads/by-target?target=sys_open+holds.", "PROJECT_OR_ISSUE_REQUIRED"},
		{"/api/v1/comment-threads/by-target?project=LOOP&issue=" + fixture.proposal.key + "&target=x", "PROJECT_OR_ISSUE_CONFLICT"},
		{"/api/v1/comment-threads/by-target?project=LOOP", "TARGET_REQUIRED"},
		{"/api/v1/comment-threads/by-target?project=LOOP&target=x&limit=0", "INVALID_QUERY"},
		{"/api/v1/comment-threads/by-target?project=LOOP&target=x&after=not-a-comment", "INVALID_AFTER"},
		{"/api/v1/comment-threads/by-target?project=LOOP&target=x&after=11111111-1111-4111-8111-111111111111", "INVALID_AFTER"},
		{"/api/v1/comment-threads/by-thread-state?state=open", "PROJECT_OR_ISSUE_REQUIRED"},
		{"/api/v1/comment-threads/by-thread-state?project=LOOP", "THREAD_STATE_REQUIRED"},
		{"/api/v1/comment-threads/by-thread-state?project=LOOP&state=met", "INVALID_THREAD_STATE"},
	} {
		response := dispatchRequest(t, fixture.handler, http.MethodGet, refusal.target, nil, "alice")
		if response.Code != http.StatusBadRequest {
			t.Errorf("GET %s: status=%d body=%s, want 400", refusal.target, response.Code, response.Body.String())
			continue
		}
		if code := decodeBody[errorBody](t, response).Code; code != refusal.code {
			t.Errorf("GET %s: code=%s, want %s", refusal.target, code, refusal.code)
		}
	}
}

// Both routes are read by any authenticated caller and by no other, which is the visibility the
// reads they replace have: GET /api/v1/search and GET /api/v1/issues/{key}/comments are authAny
// and no GET route of this API is project-scoped (project_scope.go).
func TestCommentThreadListingsRefuseAnUnauthenticatedCaller(t *testing.T) {
	fixture := newSpecLoopFixture(t)
	requirementThread(t, fixture.handler, fixture.proposal.key, "sys_open holds.", "R:1 open sys_open")
	for _, target := range []string{
		"/api/v1/comment-threads/by-target?project=LOOP&target=sys_open+holds.",
		"/api/v1/comment-threads/by-thread-state?project=LOOP&state=open",
	} {
		if response := dispatchRequest(t, fixture.handler, http.MethodGet, target, nil, ""); response.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with no caller: status=%d body=%s, want 401", target, response.Code, response.Body.String())
		}
		// A session's bearer token reads them, as it reads the listing they replace.
		if response := sessionRequest(t, fixture.handler, http.MethodGet, target, nil); response.Code != http.StatusOK {
			t.Errorf("GET %s as a session: status=%d body=%s, want 200", target, response.Code, response.Body.String())
		}
	}
}

// --- the plans ---------------------------------------------------------------------------------
//
// These routes exist to stop a read that took 84.2% of a database's time, so a plan that reads
// comments through no index is the defect, not a detail. Two things decide how that is proven,
// both measured rather than assumed.
//
// It is explained under both plan-cache modes, not under enable_seqscan = off. pgx prepares
// statements, so the plan production runs is the generic one, and a predicate written
// `($3 = '' or col = $3)` plans as a Bitmap Index Scan under force_custom_plan and as a Seq Scan
// with the whole predicate in Filter under force_generic_plan. Turning sequential scans off
// hides the same defect the other way: an unindexed read comes back as a full scan of
// comments_pkey and passes.
//
// And the test reads whether a scan of comments carries an index condition, rather than whether
// its node type is "Seq Scan": an index scan with no Index Cond is the same whole-table read
// under another name.

// seedCommentThreadCorpus fills comments with enough threads, over enough owners, that reading
// the whole table is not simply the cheapest answer: on a handful of rows the planner reads
// everything whole however the statement is written, so a plan test on a small table proves
// nothing about the statement.
//
// The rows are written with one INSERT rather than through the API, because the plan is what is
// being read and the API's own writes would cost minutes for this many: the table's user
// triggers (the search vector, the block-index queue, the reference queues) are off for the
// insert and on again after it, since nothing below reads what they maintain.
func seedCommentThreadCorpus(t *testing.T, fixture specLoopFixture) {
	t.Helper()
	ctx := t.Context()
	// An unlinked project document, so the project scope's document arm has rows of its own.
	response := dispatchRequest(t, fixture.handler, http.MethodPost,
		"/api/v1/projects/"+fixture.project+"/artifacts",
		map[string]any{"name": "handbook.md", "content": "# Handbook\n"}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create the project document: status=%d body=%s", response.Code, response.Body.String())
	}
	document := decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, response).Artifact.ID

	for _, table := range []string{"issues", "comments"} {
		if _, err := fixture.database.Pool.Exec(ctx, "alter table "+table+" disable trigger user"); err != nil {
			t.Fatalf("hold %s's triggers off for the seed: %v", table, err)
		}
		defer func(table string) {
			if _, err := fixture.database.Pool.Exec(ctx, "alter table "+table+" enable trigger user"); err != nil {
				t.Fatalf("put %s's triggers back: %v", table, err)
			}
		}(table)
	}

	// The proposals of the project, as the loop holds them: 536 live ones at the window
	// dispatch://LINUX-1239 measured, so one proposal's threads are a small part of the table
	// rather than a third of it. A read of one issue that scanned the table whole would be
	// cheaper than an index on a corpus of three issues, and would say nothing about production.
	// The bulk sits in a project of its own, as production's does: the project under test is a
	// small part of comments, never most of it, and a read of it that scanned the table whole
	// would cost more than the index rather than less.
	const bulkProject = "OTHERLOOP"
	const proposals = 200
	if _, err := fixture.database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		select $1 || '-' || (1000 + n), $1, 1000 + n, 'Seeded proposal ' || n,
			jsonb_build_object('kind', 'user', 'id', 'alice@example.com'),
			'm' || lpad(n::text, 6, '0')
		from generate_series(1, $2::int) as n`, bulkProject, proposals); err != nil {
		t.Fatalf("seed %d proposals: %v", proposals, err)
	}

	// The bulk of the corpus, spread over those proposals, so that no one issue, document or
	// anchor is a large share of comments: the threads these routes read are always a small part
	// of the table, which is the shape production has and the shape that decides the plan.
	if _, err := fixture.database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body, anchor, resolved, created_at)
		select
			$1 || '-' || (1000 + 1 + (n % $2::int)),
			jsonb_build_object('kind', 'user', 'id', 'alice@example.com'),
			'R:' || n || ' seeded',
			jsonb_build_object(
				'artifact_id', gen_random_uuid()::text,
				'mark_id', 'm-' || n,
				'version', 1,
				'quote', 'entry ' || (n % 97),
				'orphaned', false),
			n % 5 = 0,
			now() - make_interval(secs => n)
		from generate_series(1, 30000) as n`, bulkProject, proposals); err != nil {
		t.Fatalf("seed the corpus's bulk: %v", err)
	}

	// The threads the plans below read: a few hundred on each owner the shapes name, one third
	// of them anchored on another entry so the target filter is not every row of the owner.
	seed := `
		insert into comments (issue_key, artifact_id, author, body, anchor, resolved, created_at)
		select
			case when $2 = '' then null else $2 end,
			case when $3 = '' then null else $3::uuid end,
			jsonb_build_object('kind', 'user', 'id', 'alice@example.com'),
			'R:' || n || ' seeded',
			jsonb_build_object(
				'artifact_id', $4::text,
				'mark_id', 'm-' || n,
				'version', 1,
				'quote', case when n % 3 = 0 then 'sys_close holds.' else 'sys_open holds.' end,
				'orphaned', false),
			n % 5 = 0,
			now() - make_interval(secs => n)
		from generate_series(1, $1::int) as n`
	for _, owner := range []struct {
		issue    string
		document string
		anchor   string
		rows     int
	}{
		{fixture.proposal.key, "", fixture.proposal.artifact, 300},
		{fixture.other.key, "", fixture.other.artifact, 300},
		{fixture.elsewhere.key, "", fixture.proposal.artifact, 300},
		{"", document, document, 300},
	} {
		if _, err := fixture.database.Pool.Exec(ctx, seed,
			owner.rows, owner.issue, owner.document, owner.anchor); err != nil {
			t.Fatalf("seed %d comments: %v", owner.rows, err)
		}
	}
	if _, err := fixture.database.Pool.Exec(ctx, "analyze comments"); err != nil {
		t.Fatalf("analyze comments: %v", err)
	}
	if _, err := fixture.database.Pool.Exec(ctx, "analyze issues"); err != nil {
		t.Fatalf("analyze issues: %v", err)
	}
	if _, err := fixture.database.Pool.Exec(ctx, "analyze artifacts"); err != nil {
		t.Fatalf("analyze artifacts: %v", err)
	}
}

// resolvedArms is the arms a request of this scope resolves to, read through the server's own
// resolver so the plans below are the plans a request builds.
func resolvedArms(t *testing.T, fixture specLoopFixture, scope commentThreadScope) []commentThreadArm {
	t.Helper()
	arms, err := fixture.server.commentThreadArms(t.Context(), scope)
	if err != nil {
		t.Fatalf("resolve the scope's arms: %v", err)
	}
	if len(arms) == 0 {
		t.Fatalf("the seeded project resolved to no arm")
	}
	return arms
}

// planNodes walks one EXPLAIN plan and yields every node, parents first.
func planNodes(t *testing.T, raw json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var node map[string]json.RawMessage
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatalf("decode plan node: %v", err)
	}
	nodes := []map[string]json.RawMessage{node}
	if children, held := node["Plans"]; held {
		var list []json.RawMessage
		if err := json.Unmarshal(children, &list); err != nil {
			t.Fatalf("decode child plans: %v", err)
		}
		for _, child := range list {
			nodes = append(nodes, planNodes(t, child)...)
		}
	}
	return nodes
}

// commentsReadWithoutAnIndexCondition names every node of a plan that reads comments without an
// index condition on it: a sequential scan, and an index or bitmap scan whose only predicate is
// a filter, which reads the whole index and then the whole heap.
func commentsReadWithoutAnIndexCondition(t *testing.T, plan json.RawMessage) []string {
	t.Helper()
	var unindexed []string
	for _, node := range planNodes(t, plan) {
		var relation, nodeType string
		if raw, held := node["Relation Name"]; held {
			_ = json.Unmarshal(raw, &relation)
		}
		if raw, held := node["Node Type"]; held {
			_ = json.Unmarshal(raw, &nodeType)
		}
		if relation != "comments" {
			continue
		}
		// An index condition sits on the scan that uses it: "Index Cond" on an index or bitmap
		// index scan, and "Recheck Cond" on the bitmap heap scan its bitmap feeds. A node with
		// neither reads comments whole, whatever it is called: a sequential scan, or an index
		// scan whose only predicate is a filter, which walks the whole index and the whole heap.
		_, hasIndexCond := node["Index Cond"]
		_, hasRecheck := node["Recheck Cond"]
		if !hasIndexCond && !hasRecheck {
			var index string
			if raw, held := node["Index Name"]; held {
				_ = json.Unmarshal(raw, &index)
			}
			unindexed = append(unindexed, strings.TrimSpace(nodeType+" "+index))
		}
	}
	return unindexed
}

// commentIndexesUsed names every index of comments one plan reads through.
func commentIndexesUsed(t *testing.T, plan json.RawMessage) []string {
	t.Helper()
	var used []string
	for _, node := range planNodes(t, plan) {
		var relation, index string
		if raw, held := node["Relation Name"]; held {
			_ = json.Unmarshal(raw, &relation)
		}
		if raw, held := node["Index Name"]; held {
			_ = json.Unmarshal(raw, &index)
		}
		if relation == "comments" && index != "" {
			used = append(used, index)
		}
	}
	return used
}

// explainUnderPlanMode explains one statement under one plan_cache_mode, through a prepared
// statement, so the plan read is the plan that mode gives a prepared statement.
func explainUnderPlanMode(
	t *testing.T, ctx context.Context, database *store.Store, mode, statement string, arguments ...any,
) json.RawMessage {
	t.Helper()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "set local plan_cache_mode = "+mode); err != nil {
		t.Fatalf("set %s: %v", mode, err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (format json) "+statement, arguments...).Scan(&raw); err != nil {
		t.Fatalf("explain under %s: %v", mode, err)
	}
	var plans []struct {
		Plan json.RawMessage `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatalf("decode explain output: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("explain plans = %d, want one", len(plans))
	}
	return plans[0].Plan
}

// The two indexes this change adds are built with CREATE INDEX CONCURRENTLY, which can finish
// INVALID: such an index is never used and announces nothing, so a test that asserts it exists
// proves nothing about the reads below.
func TestTheCommentThreadIndexesAreValid(t *testing.T) {
	_, database := newTestHandlerWithStore(t)
	for _, index := range []string{"comments_anchor_artifact_created", "comments_artifact_created"} {
		var valid, ready bool
		err := database.Pool.QueryRow(t.Context(), `
			select indisvalid, indisready
			from pg_index join pg_class on pg_class.oid = pg_index.indexrelid
			where pg_class.relname = $1`, index).Scan(&valid, &ready)
		if err != nil {
			t.Fatalf("read %s: %v", index, err)
		}
		if !valid || !ready {
			t.Fatalf("%s is indisvalid=%t indisready=%t; an invalid index is never used", index, valid, ready)
		}
	}
}

// Every shape either route accepts reads comments through an index, under a custom plan and
// under the generic plan a prepared statement gets. The corpus is seeded: on an empty table the
// planner reads everything whole whatever is written, so an empty table proves nothing either.
func TestCommentThreadQueriesReadCommentsThroughAnIndex(t *testing.T) {
	fixture := newSpecLoopFixture(t)
	seedCommentThreadCorpus(t, fixture)

	// A cursor into the corpus, so the keyset shapes are planned with the key they take.
	seed := decodeThreadPage(t, fixture.handler,
		"/api/v1/comment-threads/by-target?project="+fixture.project+"&target=sys_open+holds.&limit=1")
	if seed.NextAfter == nil {
		t.Fatalf("the seeded corpus answered one page with no next_after")
	}
	after := &commentThreadCursor{createdAt: seed.Threads[0].Root.CreatedAt, id: seed.Threads[0].Root.ID}

	// The scopes as a request resolves them: one issue, and a project's issues beside its own
	// unlinked documents. The owner lists are read first and compared with = any(), which is what
	// makes the comparison an index condition whatever plan shape the planner picks.
	issueScope := commentThreadScope{
		issue: fixture.proposal.key, limit: commentThreadDefaultLimit,
		arms: []commentThreadArm{{sql: commentThreadsOfIssues, owners: []string{fixture.proposal.key}}},
	}
	projectOwners := commentThreadScope{project: fixture.project, limit: commentThreadDefaultLimit}
	projectOwners.arms = resolvedArms(t, fixture, projectOwners)
	projectScope := projectOwners
	issueWithDocument := issueScope
	issueWithDocument.artifact = fixture.proposal.artifact
	projectWithDocument := projectScope
	projectWithDocument.artifact = fixture.proposal.artifact
	// A project holding most of the corpus, narrowed to one document: the owner arm decides
	// little there, so this is the shape the anchor index exists for.
	bulkWithDocument := commentThreadScope{
		project: "OTHERLOOP", artifact: fixture.proposal.artifact, limit: commentThreadDefaultLimit,
	}
	bulkWithDocument.arms = resolvedArms(t, fixture, bulkWithDocument)

	for _, shape := range []struct {
		name   string
		scope  commentThreadScope
		filter string
		value  any
		after  *commentThreadCursor
	}{
		{"by target, one issue", issueScope, commentThreadsByTargetFilter, "sys_open holds.", nil},
		{"by target, one project", projectScope, commentThreadsByTargetFilter, "sys_open holds.", nil},
		{"by target, one issue, one document", issueWithDocument, commentThreadsByTargetFilter, "sys_open holds.", nil},
		{"by target, one project, one document", projectWithDocument, commentThreadsByTargetFilter, "sys_open holds.", nil},
		{"by target, one project, after a key", projectScope, commentThreadsByTargetFilter, "sys_open holds.", after},
		{"by target, one project, one document, after a key", projectWithDocument, commentThreadsByTargetFilter, "sys_open holds.", after},
		{"by target, a large project, one document", bulkWithDocument, commentThreadsByTargetFilter, "sys_open holds.", nil},
		{"by state, one issue", issueScope, commentThreadsByStateFilter, false, nil},
		{"by state, one project", projectScope, commentThreadsByStateFilter, false, nil},
		{"by state, one issue, one document", issueWithDocument, commentThreadsByStateFilter, false, nil},
		{"by state, one project, one document", projectWithDocument, commentThreadsByStateFilter, false, nil},
		{"by state, one project, after a key", projectScope, commentThreadsByStateFilter, false, after},
		{"by state, one project, one document, after a key", projectWithDocument, commentThreadsByStateFilter, false, after},
		{"by state, a large project, one document", bulkWithDocument, commentThreadsByStateFilter, false, nil},
	} {
		statement, arguments := commentThreadStatement(shape.scope, shape.filter, shape.value, shape.after)
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			t.Run(shape.name+", "+mode, func(t *testing.T) {
				plan := explainUnderPlanMode(t, t.Context(), fixture.database, mode, statement, arguments...)
				t.Logf("indexes on comments: %v", commentIndexesUsed(t, plan))
				if unindexed := commentsReadWithoutAnIndexCondition(t, plan); len(unindexed) > 0 {
					t.Fatalf("reads comments with no index condition (%s); statement:\n%s\nplan:\n%s",
						strings.Join(unindexed, ", "), statement, plan)
				}
			})
		}
	}
}
