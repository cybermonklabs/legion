package api

// comment_threads.go serves the two reads the Verified Linux spec loop makes that no route
// answered: a requirement line by its target, and a requirement line by the state of its thread
// (dispatch://INFRA-988/artifact/report-md). A requirement line is a comment thread of the
// proposal's issue - the copy program opens one thread per requirement id, anchored on its
// target's entry in the issue's document (dispatch://LINUX-1239) - so "by target" is the threads
// anchored on one entry, and "by thread state" the threads that are open or resolved.
//
// The second route answers a thread's own state and not the requirement status of the spec
// loop's lines. The two are different things: dispatch://LINUX-1239 keeps git as the record of a
// requirement and gives Dispatch a copy, one thread per requirement id, where the line's own
// status is a word with a revision and sometimes a paragraph behind it - `met rev 21`,
// `rejected rev 27: re-filed as ...`, `open` - written in the text of the posts. A thread is
// open or resolved and nothing else. Where a requirement's own status will live is undecided,
// and this route does not decide it; its name says which of the two questions it answers, so
// that a caller cannot ask it the other one by mistake.
//
// Both were standing in on GET /api/v1/search, which answers neither question: a meaning search
// over the whole corpus took 84.2% of the database's time at 1x and 371.4 ms a call, and the
// loop's mix failed its delivery bound at 1x on a database twice production's size. The nearest
// real route, GET /api/v1/issues/{key}/comments, answers a third question — the whole thread of a
// proposal the caller already names, unfiltered and unpaged, which for a proposal split over 18
// part documents is 18 MB.
//
// Every arm here is led by a column an index already keys, so neither route reads comments
// sequentially: the issue scope by comments_issue_created (issue_key, created_at, id), the
// project scope by the same index under a semi-join on the project's issues, with
// comments_artifact_id for the project's own unlinked documents, the ?artifact= narrowing by
// comments_anchor_artifact, and each thread's replies by comments_reply_to. Nothing here adds a
// table, a column or an index: these routes read the tables main holds today, and
// dispatch://INFRA-2134 moves reads of this shape onto the node store later.
//
// Visibility is what the reads they replace have: both are authAny and ask only that the caller
// is authenticated, as GET /api/v1/search and GET /api/v1/issues/{key}/comments do. No GET route
// of this API is project-scoped (project_scope.go), so holding these two to a credential's
// project would answer a caller less than the search it replaces answered it.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const (
	// commentThreadDefaultLimit is the page size when a caller names neither limit nor offset,
	// and commentThreadMaxLimit the largest page either route serves. Both are declared once, in
	// packages/contracts/src/dispatch-tools.ts, and generated here, so the bound this server
	// enforces and the refusal the `dispatch threads-by-target` command writes are one number.
	//
	// Unlike GET /api/v1/issues, which answers every matching issue to a caller that pages with
	// neither, these two routes always page: the loop's corpus holds 334,536 requirement ids
	// (dispatch://LINUX-1239), and an unpaged answer is the 18 MB the listing they replace
	// already returns. The maximum is the loop's own batch: a batched reviewer reads up to 30
	// functions at once, and a proposal's live requirements run to a few hundred, so 200 covers
	// a unit's whole working set in one read while bounding what one request draws out of the
	// database.
	commentThreadDefaultLimit = contracts.CommentThreadPageDefault
	commentThreadMaxLimit     = contracts.CommentThreadPageMax
)

// The scope arms are the predicate that holds a thread to the caller's corpus. Each is a
// constant chosen by the parsed scope and never built from caller text, and each leads with a
// column an index keys, so a scope is an index condition rather than a filter over a scan: the
// issue arm on comments_issue_created, the project's issue arm on the same index once the
// semi-join has the project's issue keys, and the project's document arm on comments_artifact_id
// for the comments of the project's own unlinked documents, which carry no issue key (owner.go).
//
// A project is two arms and not one predicate ORed together, because an OR across two columns is
// no index condition at all: the planner answers it by reading every comment and testing both
// sides, which is the cost these routes exist to remove. Unioned, each arm keeps its own index,
// and the two cannot overlap - a comment belongs to an issue or to an unlinked document, never
// to both - so the union needs no duplicate check.
const (
	commentThreadsOfIssue            = `c.issue_key = $1`
	commentThreadsOfProjectIssues    = `c.issue_key in (select key from issues where project_key = $1)`
	commentThreadsOfProjectDocuments = `c.artifact_id in (select id from artifacts where project_key = $1 and issue_key is null)`
)

// commentThreadScope is the corpus one of the two listings reads: exactly one of a project or an
// issue, with an optional narrowing to the document a thread is anchored in.
type commentThreadScope struct {
	// arms are the scope's own predicates, one per index path, unioned into the candidate set;
	// value is what each binds to $1, a project key or an issue key.
	arms     []string
	value    string
	artifact string
	limit    int
	offset   int
}

// parseCommentThreadScope reads the scope and the page both listings take. Exactly one of
// project and issue is required, as GET /api/v1/asks/open requires exactly one of its two
// scopes: a listing with neither names no corpus, and one with both names two.
func parseCommentThreadScope(query url.Values) (commentThreadScope, error) {
	project := strings.TrimSpace(query.Get("project"))
	issue := strings.TrimSpace(query.Get("issue"))
	switch {
	case project == "" && issue == "":
		return commentThreadScope{}, errorf(http.StatusBadRequest, "PROJECT_OR_ISSUE_REQUIRED",
			"exactly one of project or issue is required")
	case project != "" && issue != "":
		return commentThreadScope{}, errorf(http.StatusBadRequest, "PROJECT_OR_ISSUE_CONFLICT",
			"project and issue are mutually exclusive; supply exactly one")
	}
	scope := commentThreadScope{
		arms:     []string{commentThreadsOfProjectIssues, commentThreadsOfProjectDocuments},
		value:    project,
		artifact: strings.TrimSpace(query.Get("artifact")),
		limit:    commentThreadDefaultLimit,
	}
	if issue != "" {
		scope.arms, scope.value = []string{commentThreadsOfIssue}, issue
	}
	if limit, present, valid := parseQueryInt(query, "limit", 1, commentThreadMaxLimit); present {
		if !valid {
			return commentThreadScope{}, errorf(http.StatusBadRequest, "INVALID_QUERY",
				"limit must be one integer from 1 to %d", commentThreadMaxLimit)
		}
		scope.limit = limit
	}
	if offset, present, valid := parseQueryInt(query, "offset", 0, math.MaxInt); present {
		if !valid {
			return commentThreadScope{}, errorf(http.StatusBadRequest, "INVALID_QUERY",
				"offset must be one non-negative integer")
		}
		scope.offset = offset
	}
	return scope, nil
}

// commentThreadRow is one thread as these two listings answer it: the root comment, how many
// replies the thread holds and when anything was last written in it. A thread's replies all hang
// from its root — a write climbs to it (threadHeadOf) — and its state lives on that root, which
// a resolve sets and a later reply clears (comment_actions.go, comment_create.go). The counts
// ride in the same statement because the loop's question is which threads match and where each
// stands: answering the root alone would send a caller back for one read per thread, which is the
// cost these routes exist to remove.
type commentThreadRow struct {
	Root           model.Comment `json:"root"`
	Replies        int           `json:"replies"`
	LastActivityAt time.Time     `json:"last_activity_at"`
}

// commentThreadPage is the page either listing answers: the threads of this page, and the count
// of every thread the filter matches, counted in the same statement over the same snapshot so
// the rows and the total cannot come from two readings of a moving table.
type commentThreadPage struct {
	Threads []commentThreadRow `json:"threads"`
	Total   int                `json:"total"`
	Limit   int                `json:"limit"`
	Offset  int                `json:"offset"`
}

// listCommentThreadsByTarget serves GET /api/v1/comment-threads/by-target: the requirement query
// by target. The target is the entry a thread is anchored on — the quoted text of the function,
// the model file or the "Other targets" heading the loop files a requirement against — matched
// whole, so a target names one entry and not every entry whose text holds those words, which is
// the difference between this and the meaning search it replaces.
func (s *server) listCommentThreadsByTarget(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("target"))
	if target == "" {
		writeError(w, "TARGET_REQUIRED", http.StatusBadRequest, "target is required")
		return
	}
	s.answerCommentThreads(w, r, `c.anchor->>'quote' = $2`, target)
}

// listCommentThreadsByThreadState serves GET /api/v1/comment-threads/by-thread-state: the
// requirement query by the state of the line's thread. A thread is open or resolved, which is
// the whole of the state Dispatch keeps of one (comments.resolved). It is not the spec loop's
// own requirement status: that is a word with a revision behind it, written in the text of the
// posts, and dispatch://LINUX-1239 leaves git as its record, so no table main holds carries it
// and no route here answers it.
func (s *server) listCommentThreadsByThreadState(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var resolved bool
	switch state := strings.TrimSpace(r.URL.Query().Get("state")); state {
	case "open":
		resolved = false
	case "resolved":
		resolved = true
	case "":
		writeError(w, "THREAD_STATE_REQUIRED", http.StatusBadRequest, "state is required: open or resolved")
		return
	default:
		writeError(w, "INVALID_THREAD_STATE", http.StatusBadRequest, "state must be open or resolved")
		return
	}
	s.answerCommentThreads(w, r, `c.resolved = $2`, resolved)
}

// answerCommentThreads is the half both listings share: the scope, the page, the read and the
// answer. filterSQL is one of the two constants above its callers and binds the caller's value
// to $2; neither is built from caller text.
func (s *server) answerCommentThreads(w http.ResponseWriter, r *http.Request, filterSQL string, filter any) {
	scope, err := parseCommentThreadScope(r.URL.Query())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	page, err := s.loadCommentThreads(r.Context(), scope, filterSQL, filter)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, page)
}

// commentThreadCandidates is one scope arm's candidate set: the thread roots of that arm the
// filter matches, each row carried whole. The roots are the comments that reply to nothing and
// answer no ask - an ask's thread is the ask's, read through GET /api/v1/asks/{id}, and a reply
// is part of the thread its root heads rather than a thread of its own. The first %s is the arm,
// the second the filter; both are constants of this file, never caller text.
//
// It carries commentThreadColumns rather than the arm's ids alone, so the page is read without
// a second pass over comments, and those columns rather than c.*, because comments.search is a
// tsvector and nothing above needs it.
const commentThreadCandidates = `
		select ` + commentThreadColumns + `
		from comments c
		where c.reply_to is null and c.ask_id is null
			and (%s)
			and (%s)
			and ($3 = '' or c.anchor->>'artifact_id' = $3)`

// commentThreadQuery reads one page of those candidates with each thread's reply count and the
// time anything was last written in it. %s is the candidate set: one arm, or the two arms of a
// project scope unioned.
//
// The order is created_at then id, the total order GET /api/v1/issues/{key}/comments already
// lists in, so a caller paging these routes walks the same sequence it reads there. count(*)
// over () takes the whole filter's count from the same snapshot the page is cut from, so the
// rows and the total cannot come from two readings of a moving table; a page past the last one
// holds no row to carry it and reads 0.
const commentThreadQuery = `
	with candidates as (%s)
	select ` + commentColumns + `, thread.replies, thread.last_activity_at, count(*) over ()::int as total
	from candidates
	join lateral (
		select
			count(reply.id)::int as replies,
			coalesce(max(reply.created_at), candidates.created_at) as last_activity_at
		from comments reply
		where reply.reply_to = candidates.id
	) thread on true
	order by candidates.created_at, candidates.id
	limit $4 offset $5`

// commentThreadStatement is the whole statement for one scope and one filter: each arm rendered
// with the filter and the arms unioned, since each keeps its own index that way and an OR across
// their two columns would keep none.
func commentThreadStatement(scope commentThreadScope, filterSQL string) string {
	arms := make([]string, 0, len(scope.arms))
	for _, arm := range scope.arms {
		arms = append(arms, fmt.Sprintf(commentThreadCandidates, arm, filterSQL))
	}
	return fmt.Sprintf(commentThreadQuery, strings.Join(arms, "\n\t\tunion all"))
}

// loadCommentThreads reads the page and hydrates each root's side tables, so a thread's root is
// the same Comment every other comment route answers.
func (s *server) loadCommentThreads(
	ctx context.Context, scope commentThreadScope, filterSQL string, filter any,
) (commentThreadPage, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, commentThreadStatement(scope, filterSQL),
		scope.value, filter, scope.artifact, scope.limit, scope.offset)
	if err != nil {
		return commentThreadPage{}, err
	}
	defer rows.Close()
	threads := []commentThreadRow{}
	roots := []model.Comment{}
	total := 0
	for rows.Next() {
		var thread commentThreadRow
		comment, err := scanComment(rows, &thread.Replies, &thread.LastActivityAt, &total)
		if err != nil {
			return commentThreadPage{}, err
		}
		thread.Root = comment
		threads = append(threads, thread)
		roots = append(roots, comment)
	}
	if err := rows.Err(); err != nil {
		return commentThreadPage{}, err
	}
	hydrated, err := s.hydrateCommentSideTables(ctx, s.deps.Store.Pool, roots)
	if err != nil {
		return commentThreadPage{}, err
	}
	for index := range threads {
		threads[index].Root = hydrated[index]
	}
	return commentThreadPage{Threads: threads, Total: total, Limit: scope.limit, Offset: scope.offset}, nil
}
