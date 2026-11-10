package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// specLoopFixture is a corpus of the shape the Verified Linux spec loop files: two proposal
// issues of one project, each with requirement threads anchored on its own targets, threads at
// both statuses, replies under some of them, and threads of a second project beside them. It is
// what proves a filter returns exactly the threads it names and no others.
type specLoopFixture struct {
	handler  http.Handler
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
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": project, "title": title, "spec": entries,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, response)
}

// newSpecLoopFixture seeds the corpus. Each issue's spec quotes its own entries, so a thread can
// anchor on them, and the two projects' entries share a target name on purpose: a project-scoped
// read must not reach the other project's thread of the same name.
func newSpecLoopFixture(t *testing.T) specLoopFixture {
	t.Helper()
	fixture := specLoopFixture{handler: newTestHandler(t), project: "LOOP"}

	const entries = "sys_open holds.\nsys_close holds.\nOther targets\n"
	proposal := createInteractionIssue(t, fixture.handler, fixture.project, "Proposal DS-exec-context", entries)
	fixture.proposal.key, fixture.proposal.artifact = proposal.Key, proposal.PrimaryArtifactID
	other := proposalIssue(t, fixture.handler, fixture.project, "Proposal DS-vfs", entries)
	fixture.other.key, fixture.other.artifact = other.Key, other.PrimaryArtifactID
	elsewhere := createInteractionIssue(t, fixture.handler, "OTHERLOOP", "Proposal elsewhere", entries)
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
	if page.Total != 2 || page.Limit != commentThreadDefaultLimit || page.Offset != 0 {
		t.Fatalf("page = {total %d, limit %d, offset %d}, want {2, %d, 0}",
			page.Total, page.Limit, page.Offset, commentThreadDefaultLimit)
	}
	if page.Threads[0].Replies != 1 {
		t.Fatalf("R:1 replies = %d, want 1", page.Threads[0].Replies)
	}
	if page.Threads[1].Replies != 0 {
		t.Fatalf("R:3 replies = %d, want 0", page.Threads[1].Replies)
	}
	if !page.Threads[0].LastActivityAt.After(page.Threads[0].Root.CreatedAt) {
		t.Fatalf("R:1 last_activity_at = %v, want after its root's %v",
			page.Threads[0].LastActivityAt, page.Threads[0].Root.CreatedAt)
	}
	if !page.Threads[1].LastActivityAt.Equal(page.Threads[1].Root.CreatedAt) {
		t.Fatalf("R:3 last_activity_at = %v, want its root's %v",
			page.Threads[1].LastActivityAt, page.Threads[1].Root.CreatedAt)
	}

	// The same target read of one issue alone reaches that issue's thread and no other's.
	wantBodies(t, decodeThreadPage(t, fixture.handler,
		"/api/v1/comment-threads/by-target?issue="+fixture.proposal.key+"&target=sys_open+holds."),
		"R:1 open sys_open")

	// A target no entry carries is an empty page, never another entry's threads.
	empty := decodeThreadPage(t, fixture.handler,
		"/api/v1/comment-threads/by-target?project="+fixture.project+"&target=sys_read+holds.")
	if len(empty.Threads) != 0 || empty.Total != 0 {
		t.Fatalf("unknown target = %v (total %d), want an empty page", threadBodies(empty), empty.Total)
	}
}

// The by-target page: its order is the comment listing's, its limit and offset cut it, and the
// total counts every matching thread rather than the page.
func TestCommentThreadsByTargetPages(t *testing.T) {
	fixture := newSpecLoopFixture(t)
	for index := 1; index <= 3; index++ {
		requirementThread(t, fixture.handler, fixture.proposal.key, "sys_open holds.", fmt.Sprintf("R:%d open sys_open", index))
	}

	page := decodeThreadPage(t, fixture.handler,
		"/api/v1/comment-threads/by-target?project="+fixture.project+"&target=sys_open+holds.&limit=2")
	wantBodies(t, page, "R:1 open sys_open", "R:2 open sys_open")
	if page.Total != 3 || page.Limit != 2 || page.Offset != 0 {
		t.Fatalf("first page = {total %d, limit %d, offset %d}, want {3, 2, 0}", page.Total, page.Limit, page.Offset)
	}

	second := decodeThreadPage(t, fixture.handler,
		"/api/v1/comment-threads/by-target?project="+fixture.project+"&target=sys_open+holds.&limit=2&offset=2")
	wantBodies(t, second, "R:3 open sys_open")
	if second.Total != 3 || second.Offset != 2 {
		t.Fatalf("second page = {total %d, offset %d}, want {3, 2}", second.Total, second.Offset)
	}
}

// The requirement query by the state of the line's thread: open lists the threads nobody
// resolved, resolved lists the rest, a reply in a resolved thread moves it back to open, and a
// reply is never a thread of its own. Before this route the loop asked this question of GET
// /api/v1/search too. The state is the thread's own and never the spec loop's requirement
// status, which is why `met` - a status word of a requirement line - is refused rather than
// answered (TestCommentThreadListingsRefuseAnUnscopedOrUnfilteredRequest).
func TestCommentThreadsByThreadStateListsOnlyTheThreadsInThatState(t *testing.T) {
	fixture := newSpecLoopFixture(t)

	met := requirementThread(t, fixture.handler, fixture.proposal.key, "sys_open holds.", "R:1 met")
	requirementThread(t, fixture.handler, fixture.proposal.key, "sys_close holds.", "R:2 open")
	reopened := requirementThread(t, fixture.handler, fixture.other.key, "Other targets", "R:3 reopened")
	requirementThread(t, fixture.handler, fixture.elsewhere.key, "sys_open holds.", "R:4 open, other project")

	for _, id := range []string{met.ID, reopened.ID} {
		if response := dispatchRequest(t, fixture.handler, http.MethodPost,
			"/api/v1/comments/"+id+"/resolve", nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("resolve %s: status=%d body=%s", id, response.Code, response.Body.String())
		}
	}
	// A reply in a resolved thread reopens it (comment_create.go), so R:3 is open again and the
	// status read has to follow the row rather than the resolve that once happened.
	createThreadComment(t, fixture.handler, fixture.other.key, map[string]any{
		"body": "The owner answers, which reopens R:3.", "reply_to": reopened.ID,
	}, "alice")

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
		{"/api/v1/comment-threads/by-target?project=LOOP&target=x&offset=-1", "INVALID_QUERY"},
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

// Both listings read comments through an index in every scope they serve. This is the point of
// them: the meaning search they replace read the whole corpus, and a listing that scanned
// comments instead would be the same cost under a new name. The plan is taken with sequential
// scans off, as every plan test of this package takes one, so a read no index serves shows up as
// a scan carrying no index condition (planWithoutSeqScans).
func TestCommentThreadQueriesAvoidSequentialCommentsScan(t *testing.T) {
	_, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("disable sequential scans: %v", err)
	}

	issue := commentThreadScope{arms: []string{commentThreadsOfIssue}, limit: commentThreadDefaultLimit}
	project := commentThreadScope{
		arms:  []string{commentThreadsOfProjectIssues, commentThreadsOfProjectDocuments},
		limit: commentThreadDefaultLimit,
	}
	for _, query := range []struct {
		name   string
		scope  commentThreadScope
		filter string
		value  any
	}{
		{"by target, one issue", issue, `c.anchor->>'quote' = $2`, "sys_open holds."},
		{"by target, one project", project, `c.anchor->>'quote' = $2`, "sys_open holds."},
		{"by thread state, one issue", issue, `c.resolved = $2`, false},
		{"by thread state, one project", project, `c.resolved = $2`, false},
	} {
		t.Run(query.name, func(t *testing.T) {
			assertPlanAvoidsSequentialRelation(t, ctx, tx,
				commentThreadStatement(query.scope, query.filter), "comments",
				"LOOP-1", query.value, "", commentThreadDefaultLimit, 0)
		})
	}
}
