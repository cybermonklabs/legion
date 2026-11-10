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
// A thread's state here is the state of its root row, which is what this route reads and all it
// reads. That is not an invariant of the thread: POST /api/v1/comments/{id}/resolve resolves the
// comment its path names (comment_actions.go), so resolving a reply sets that reply's own flag
// and leaves the root open, and this route answers the thread open. Changing which row a resolve
// writes is a change to the write path and belongs to its own issue; the route's description
// says what it answers rather than claiming more.
//
// Both were standing in on GET /api/v1/search, which answers neither question: a meaning search
// over the whole corpus took 84.2% of the database's time at 1x and 371.4 ms a call, and the
// loop's mix failed its delivery bound at 1x on a database twice production's size. The nearest
// real route, GET /api/v1/issues/{key}/comments, answers a third question - the whole thread of a
// proposal the caller already names, unfiltered and unpaged, which for a proposal split over 18
// part documents is 18 MB.
//
// Every shape these routes take is written as its own statement rather than one statement with
// the unused halves switched off by an `or`. pgx prepares statements, so a prepared statement
// Postgres plans generically is what production runs, and a predicate written `($3 = '' or col =
// $3)` reaches an index only under a custom plan: measured on a populated table, that form plans
// as a Bitmap Index Scan under plan_cache_mode = force_custom_plan and as a Seq Scan with the
// whole predicate in Filter under force_generic_plan. commentThreadStatement composes the
// predicates the request actually carries, so each shape is an index condition under either
// plan, and comment_threads_test.go explains every shape under both modes on a seeded corpus.
//
// The indexes each arm leads with: comments_issue_created (issue_key, created_at, id) for an
// issue and for the project's issues, comments_artifact_created (artifact_id, created_at, id)
// for the project's own unlinked documents, and comments_anchor_artifact_created
// ((anchor->>'artifact_id'), created_at, id) for the ?artifact= narrowing; each thread's replies
// read comments_reply_to. The last two are this change's own migrations: 0032's
// comments_anchor_artifact is on ((anchor->>'artifact_id')::uuid), which no text comparison can
// use, and its comments_artifact_id carries no order columns, so neither serves a keyset page.
// Nothing here reads the node store: dispatch://INFRA-2134 moves reads of this shape onto it
// later, and these routes read the tables main holds today.
//
// Visibility is what the reads they replace have: both are authAny and ask only that the caller
// is authenticated, as GET /api/v1/search and GET /api/v1/issues/{key}/comments do. No GET route
// of this API is project-scoped (project_scope.go), so holding these two to a credential's
// project would answer a caller less than the search it replaces answered it.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const (
	// commentThreadDefaultLimit is the page size when a caller names no limit, and
	// commentThreadMaxLimit the largest page either route serves. Both are declared once, in
	// packages/contracts/src/dispatch-tools.ts, and generated here, so the bound this server
	// enforces and the refusal the `dispatch threads-by-target` command writes are one number.
	//
	// Unlike GET /api/v1/issues, which answers every matching issue to a caller that pages with
	// neither limit nor offset, these two routes always page: the loop's corpus holds 334,536
	// requirement ids (dispatch://LINUX-1239), and an unpaged answer is the 18 MB the listing
	// they replace already returns. The maximum is the loop's own batch: a batched reviewer
	// reads up to 30 functions at once, and a proposal's live requirements run to a few hundred,
	// so 200 covers a unit's whole working set in one read while bounding what one request draws
	// out of the database.
	commentThreadDefaultLimit = contracts.CommentThreadPageDefault
	commentThreadMaxLimit     = contracts.CommentThreadPageMax
)

// The scope arms are the predicate that holds a thread to the caller's corpus: an arm on the
// comments its owners own, where the owners are read first and handed to the arm as a list.
//
// The owners are resolved before the statement rather than joined inside it, because a join is
// not an index condition: written `c.issue_key in (select key from issues where project_key =
// $1)` the planner is free to hash every key of the project and read comments whole, and
// measured on a seeded corpus it does exactly that. Written `c.issue_key = any($1)` the
// comparison is on the leading column of comments_issue_created whatever plan shape the planner
// picks, under a generic plan as well as a custom one. The owner reads are themselves index
// reads, of issues by project and of artifacts through artifacts_project_documents (0091), whose
// whole predicate the document read carries, `issue_key is null and session_id is null`.
//
// A project is two arms and not one predicate ORed together, because an OR across two columns is
// no index condition at all. Unioned, each arm keeps its own index, and the two cannot overlap -
// a comment belongs to an issue or to an unlinked document, never to both - so the union needs
// no duplicate check.
const (
	commentThreadsOfIssues    = `c.issue_key = any(%s)`
	commentThreadsOfDocuments = `c.artifact_id = any(%s::uuid[])`
)

// commentThreadArm is one arm of the candidate set: its predicate and the owner list that
// predicate compares against.
type commentThreadArm struct {
	sql    string
	owners []string
}

// commentThreadScope is the corpus one of the two listings reads: exactly one of a project or an
// issue, with an optional narrowing to the document a thread is anchored in, and the page the
// caller asked for. project and issue are the request's own words; arms are what they resolve to
// (commentThreadArms).
type commentThreadScope struct {
	project string
	issue   string
	// arms are the scope's resolved predicates, one per index path, unioned into the candidates.
	arms []commentThreadArm
	// artifact is the document a thread must be anchored in, empty for no narrowing.
	artifact string
	// after is the root comment the page starts after, empty for the first page.
	after string
	limit int
}

// commentThreadArms resolves the scope's owners: one issue key, or every issue and every
// unlinked document of one project. An arm with no owner is left out rather than sent with an
// empty list, so a project holding no unlinked document costs no arm at all.
func (s *server) commentThreadArms(ctx context.Context, scope commentThreadScope) ([]commentThreadArm, error) {
	if scope.issue != "" {
		return []commentThreadArm{{sql: commentThreadsOfIssues, owners: []string{scope.issue}}}, nil
	}
	issues, err := s.commentThreadOwners(ctx,
		`select key from issues where project_key = $1`, scope.project)
	if err != nil {
		return nil, err
	}
	documents, err := s.commentThreadOwners(ctx,
		`select id::text from artifacts
		 where project_key = $1 and issue_key is null and session_id is null`, scope.project)
	if err != nil {
		return nil, err
	}
	var arms []commentThreadArm
	if len(issues) > 0 {
		arms = append(arms, commentThreadArm{sql: commentThreadsOfIssues, owners: issues})
	}
	if len(documents) > 0 {
		arms = append(arms, commentThreadArm{sql: commentThreadsOfDocuments, owners: documents})
	}
	return arms, nil
}

// commentThreadOwners reads one owner list.
func (s *server) commentThreadOwners(ctx context.Context, statement, project string) ([]string, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, statement, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, err
		}
		owners = append(owners, owner)
	}
	return owners, rows.Err()
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
		project: project,
		issue:   issue,
		// The anchor's own stored text, as the document holds it: an entry whose text opens or
		// ends with a space is anchored on that text, and trimming the caller's value here would
		// make such an entry unmatchable (commentThreadsByTargetFilter does not trim either).
		artifact: query.Get("artifact"),
		after:    strings.TrimSpace(query.Get("after")),
		limit:    commentThreadDefaultLimit,
	}
	if limit, present, valid := parseQueryInt(query, "limit", 1, commentThreadMaxLimit); present {
		if !valid {
			return commentThreadScope{}, errorf(http.StatusBadRequest, "INVALID_QUERY",
				"limit must be one integer from 1 to %d", commentThreadMaxLimit)
		}
		scope.limit = limit
	}
	return scope, nil
}

// commentThreadRow is one thread as these two listings answer it: the root comment, how many
// replies the thread holds and when a reply was last posted in it. A thread's replies all hang
// from its root - a write climbs to it (threadHeadOf) - so one count over comments_reply_to
// answers both. The counts ride in the same statement because the loop's question is which
// threads match and where each stands: answering the root alone would send a caller back for one
// read per thread, which is the cost these routes exist to remove.
type commentThreadRow struct {
	Root    model.Comment `json:"root"`
	Replies int           `json:"replies"`
	// LastReplyAt is when the newest reply of this thread was posted, null for a thread holding
	// none. It is the time a reply was written and nothing else: an edit, a resolve, a reopen
	// and a suggestion's acceptance each leave it where it was, so a caller reading "nothing has
	// happened here" from it would be reading more than the field says.
	LastReplyAt *time.Time `json:"last_reply_at"`
}

// commentThreadPage is the page either listing answers. It carries no total: an exact count of
// the threads a filter matches over a whole project is the expensive half of answering the
// caller's question - every matching row has to be reached before the first one can be sent -
// and the question these routes exist for is which threads match, not how many. A caller learns
// there is another page from NextAfter, and reads it by sending that value as ?after=.
type commentThreadPage struct {
	Threads []commentThreadRow `json:"threads"`
	Limit   int                `json:"limit"`
	// NextAfter is the root comment id of this page's last thread when another page follows, and
	// null when this is the last page. The page is cut by that key rather than by an offset, so
	// a thread resolved, reopened or written between two reads moves no row across a page
	// boundary: a caller paging a whole project sees each matching thread once.
	NextAfter *string `json:"next_after"`
}

// commentThreadsByTargetFilter is the by-target route's own predicate: the anchor's quoted text,
// matched whole. The caller's value is not trimmed and neither is the stored quote, since the
// quote is the document's own text and an entry that opens or ends with a space is anchored on
// exactly that.
const commentThreadsByTargetFilter = `c.anchor->>'quote' = %s`

// commentThreadsByStateFilter is the by-thread-state route's own predicate: the root row's
// resolved flag.
const commentThreadsByStateFilter = `c.resolved = %s`

// listCommentThreadsByTarget serves GET /api/v1/comment-threads/by-target: the requirement query
// by target. The target is the entry a thread is anchored on - the quoted text of the function,
// the model file or the "Other targets" heading the loop files a requirement against - matched
// whole, so a target names one entry and not every entry whose text holds those words, which is
// the difference between this and the meaning search it replaces.
func (s *server) listCommentThreadsByTarget(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	target := r.URL.Query().Get("target")
	if target == "" {
		writeError(w, "TARGET_REQUIRED", http.StatusBadRequest, "target is required")
		return
	}
	s.answerCommentThreads(w, r, commentThreadsByTargetFilter, target)
}

// listCommentThreadsByThreadState serves GET /api/v1/comment-threads/by-thread-state: the
// requirement query by the state of the line's thread. A thread is open or resolved, which is
// the whole of the state Dispatch keeps of one, and the state read is the root row's. It is not
// the spec loop's own requirement status: that is a word with a revision behind it, written in
// the text of the posts, and dispatch://LINUX-1239 leaves git as its record, so no table main
// holds carries it and no route here answers it.
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
	s.answerCommentThreads(w, r, commentThreadsByStateFilter, resolved)
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

// commentThreadCandidates is one scope arm's candidate set: that arm's thread roots the filter
// matches, in the page's own order and cut to the page's own length, each row carried whole. The
// roots are the comments that reply to nothing and answer no ask - an ask's thread is the ask's,
// read through GET /api/v1/asks/{id}, and a reply is part of the thread its root heads rather
// than a thread of its own.
//
// It carries commentThreadColumns rather than the arm's ids alone, so the page is read without a
// second pass over comments, and those columns rather than c.*, because comments.search is a
// tsvector and nothing above needs it. The order and the limit sit inside the arm so that each
// arm reads one index range of its own and the union has at most one page from each.
//
// The %s are, in order: the arm, the filter, the ?artifact= narrowing or nothing, the keyset or
// nothing, and the limit's parameter. Every one of them is composed by commentThreadStatement
// out of this file's own constants, so a shape the request did not ask for is absent from the
// statement rather than switched off inside it.
const commentThreadCandidates = `
		(select ` + commentThreadColumns + `
		from comments c
		where c.reply_to is null and c.ask_id is null
			and (%s)
			and (%s)%s%s
		order by c.created_at, c.id
		limit %s)`

// commentThreadArtifactFilter narrows an arm to the threads anchored in one document. %s is its
// parameter.
const commentThreadArtifactFilter = `
			and c.anchor->>'artifact_id' = %s`

// commentThreadKeyset starts an arm after one thread, by the (created_at, id) of its root. The
// row-value comparison is an index condition on every arm's own index, each of which carries
// those two columns after its leading one. %s and %s are its parameters.
const commentThreadKeyset = `
			and (c.created_at, c.id) > (%s, %s)`

// commentThreadQuery takes the page out of the candidates and hangs each thread's reply count
// off it. The lateral runs after the limit, over the page's own rows and no others, so a page of
// one costs one reply count and a page of two hundred costs two hundred, whatever the project
// holds. %s is the candidate set: one arm, or the two arms of a project scope unioned.
const commentThreadQuery = `
	with candidates as (%s),
	page as (select * from candidates order by created_at, id limit %s)
	select ` + commentColumns + `, thread.replies, thread.last_reply_at
	from page
	join lateral (
		select count(reply.id)::int as replies, max(reply.created_at) as last_reply_at
		from comments reply
		where reply.reply_to = page.id
	) thread on true
	order by page.created_at, page.id`

// commentThreadStatement is the whole statement for one request, with its arguments. It composes
// only the predicates the request carries: a shape it did not ask for is not in the text at all,
// which is what makes each one an index condition under a generic plan as well as a custom one.
//
// Parameters are bound in a fixed order so a reader can follow them: $1 the scope's value, $2 the
// filter's, then the ?artifact= value where there is one, then the keyset's two where there is
// one, then the limit. The limit is read as limit+1: the extra row is never answered and says
// only that another page follows (loadCommentThreads).
func commentThreadStatement(
	scope commentThreadScope, filterSQL string, filter any, after *commentThreadCursor,
) (string, []any) {
	arguments := []any{filter}
	filterCondition := fmt.Sprintf(filterSQL, placeholder(len(arguments)))
	artifact := ""
	if scope.artifact != "" {
		arguments = append(arguments, scope.artifact)
		artifact = fmt.Sprintf(commentThreadArtifactFilter, placeholder(len(arguments)))
	}
	keyset := ""
	if after != nil {
		arguments = append(arguments, after.createdAt, after.id)
		keyset = fmt.Sprintf(commentThreadKeyset,
			placeholder(len(arguments)-1), placeholder(len(arguments)))
	}
	arguments = append(arguments, scope.limit+1)
	limit := placeholder(len(arguments))
	arms := make([]string, 0, len(scope.arms))
	for _, arm := range scope.arms {
		arguments = append(arguments, arm.owners)
		arms = append(arms, fmt.Sprintf(commentThreadCandidates,
			fmt.Sprintf(arm.sql, placeholder(len(arguments))), filterCondition, artifact, keyset, limit))
	}
	return fmt.Sprintf(commentThreadQuery, strings.Join(arms, "\n\t\tunion all"), limit), arguments
}

// placeholder is the nth bind parameter of a statement, written once so the composed predicates
// above cannot disagree about which number they take.
func placeholder(n int) string {
	return fmt.Sprintf("$%d", n)
}

// commentThreadCursor is where a page starts: the (created_at, id) of the root the previous page
// ended with. A caller sends that root's id and the server reads its created_at, so the cursor a
// caller holds is an id it already has rather than a format it has to build.
type commentThreadCursor struct {
	createdAt time.Time
	id        string
}

// commentThreadAfter reads the cursor one request names, or nil where it names none. An id that
// is not a uuid, and one no comment carries, are the same refusal: the caller sent a value that
// starts no page, and which of the two it is says nothing it can act on differently.
func (s *server) commentThreadAfter(ctx context.Context, after string) (*commentThreadCursor, error) {
	if after == "" {
		return nil, nil
	}
	cursor := commentThreadCursor{id: after}
	err := s.deps.Store.Pool.QueryRow(ctx,
		`select created_at from comments where id::text = $1`, after).Scan(&cursor.createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errorf(http.StatusBadRequest, "INVALID_AFTER",
			"after must be the id of a comment this Dispatch holds; it is the next_after of an earlier page")
	}
	if err != nil {
		return nil, err
	}
	return &cursor, nil
}

// loadCommentThreads reads one page and hydrates each root's side tables, so a thread's root is
// the same Comment every other comment route answers.
func (s *server) loadCommentThreads(
	ctx context.Context, scope commentThreadScope, filterSQL string, filter any,
) (commentThreadPage, error) {
	after, err := s.commentThreadAfter(ctx, scope.after)
	if err != nil {
		return commentThreadPage{}, err
	}
	scope.arms, err = s.commentThreadArms(ctx, scope)
	if err != nil {
		return commentThreadPage{}, err
	}
	// A project that holds no issue and no unlinked document owns no comment, so there is
	// nothing to read and no statement to send.
	if len(scope.arms) == 0 {
		return commentThreadPage{Threads: []commentThreadRow{}, Limit: scope.limit}, nil
	}
	statement, arguments := commentThreadStatement(scope, filterSQL, filter, after)
	rows, err := s.deps.Store.Pool.Query(ctx, statement, arguments...)
	if err != nil {
		return commentThreadPage{}, err
	}
	defer rows.Close()
	threads := []commentThreadRow{}
	roots := []model.Comment{}
	for rows.Next() {
		var thread commentThreadRow
		comment, err := scanComment(rows, &thread.Replies, &thread.LastReplyAt)
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
	page := commentThreadPage{Threads: threads, Limit: scope.limit}
	// The statement read one row more than the page: it is never answered, and its presence is
	// the whole of what this route knows about what follows.
	if len(threads) > scope.limit {
		threads = threads[:scope.limit]
		roots = roots[:scope.limit]
		page.Threads = threads
		last := threads[len(threads)-1].Root.ID
		page.NextAfter = &last
	}
	hydrated, err := s.hydrateCommentSideTables(ctx, s.deps.Store.Pool, roots)
	if err != nil {
		return commentThreadPage{}, err
	}
	for index := range threads {
		threads[index].Root = hydrated[index]
	}
	return page, nil
}
