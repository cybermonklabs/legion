package api

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/failpoint"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// POST /api/v1/me/inbox/read is the Inbox's Mark all read: one request marks every thread the
// list route would show the caller as unread, loaded on the page or not, narrowed as the list is
// narrowed and no further than one read mark. A thread whose newest reply landed after
// read_through stays unread, so a write the person has not seen is never swallowed by the button.
//
// The set it marks is exactly what GET /api/v1/me/replies?state=unread answers under the same
// narrowing, so it is read through that route's own filters - myRepliesUnread for the Reply rows
// (api/replies.go) and the Agent read beside it (api/replies_agent.go) - rather than predicates of
// its own: two spellings of "unread" would let the button mark a row the list never showed, or
// leave one it did.

// inboxReplyMatchQuery builds the Reply read both write routes over these rows make: the caller's
// rows in one state (source, one of the list route's own myRepliesUnread, myRepliesInbox and
// myRepliesDismissed, with that route's parameters $1 login, $2 issue, $3 document) whose mark -
// the column that state is ordered by, named by mark - is at or before $4, of the priorities ($5,
// null for every one, with $6 for a thread whose issue carries none) and the projects ($7, null
// for every one) the request names. It answers each thread's id, the owner this transaction locks
// before it writes - its issue or its document - and the row's own version (user_threads.version,
// migration 20261105093500), which is what a filtered dismissal holds the row to, the server
// standing in for the page there (inboxDismissMarkThreads, api/inbox_dismiss.go); the mark-all
// answers it and ignores it, since one read serves both routes. The project whose event announces
// the write is not read here: the update answers the project of every row it moved, and those are
// the projects this transaction locks, after the rows (lockInboxReadOwners).
//
// One builder rather than a statement per state and route: the mark-all reads the caller's unread
// rows and the dismiss route the Inbox's own or the dismissed ones (api/inbox_dismiss.go), and the
// three differ in the state they match and the mark they narrow by and in nothing else, so three
// spellings of it would be three places to carry a fix to the one plan below.
//
// The rows are matched first, as myRepliesCount matches them, so the planner reads them through
// the state's own partial index rather than misjudging the stored flag; the narrowing these routes
// add is applied over that set, where the issue join costs one lookup per matched row.
//
// The match reads the index and nothing else - the thread id it answers and the mark it narrows by
// are both columns the state's partial index holds - and each matched row's owner is then read by
// that id, fenced from the planner as the list route's page read is (myRepliesPageRows,
// api/replies.go): a one-step match reads the owner columns too, and the cheapest way to read a
// few columns of a thousand rows out of a table one login owns is a scan of the whole table, which
// at INFRA-450's a14 seed is 101,540 rows read to mark 1,300.
func inboxReplyMatchQuery(source, mark string) string {
	return `
	with matched as materialized (
		select u.thread_id, u.` + mark + `
		` + source + `
	)
	select m.thread_id::text, owner.issue_key, owner.artifact_id::text, owner.version
	from matched m
	join lateral (
		select u.issue_key, u.artifact_id, u.project_key, u.version
		from user_threads u
		where u.login = $1 and u.thread_id = m.thread_id
		offset 0
	) owner on true
	left join issues i on i.key = owner.issue_key
	where m.` + mark + ` <= least($4::text::timestamptz, now())
	  and ($5::int[] is null or i.priority = any($5::int[]) or ($6::boolean and i.priority is null))
	  and ($7::text[] is null or owner.project_key = any($7::text[]))
	order by m.thread_id`
}

// inboxReadReplyThreads reads the Reply threads one request marks: the caller's unread rows
// (myRepliesUnread, read through user_threads_unread) whose newest reply is at or before the read
// mark.
var inboxReadReplyThreads = inboxReplyMatchQuery(myRepliesUnread, "replied_at")

// inboxAgentRootsQuery builds the Agent read both write routes make over one state's roots: each
// root the request changes, its session - which one event names - the replies a write that
// marks the thread read writes by id, which is inboxAgentUnreadReplies for such a write and an
// empty array for one that leaves the read mark alone, and the newest session reply in the
// root's thread (inboxAgentNewestReply), which is what a dismissal holds the thread to. Every
// read answers that column, the request that named its rows included: one read shape for the
// three routes, and a named dismissal writes what the page drew over it afterwards
// (readInboxDismissThreads, api/inbox_dismiss.go). match is what narrows those roots: the mark
// the request carries ($4), or the ids it named ($4 again, as the array). Each root's own facts
// are read as the list route reads them (myAgentRootsWithFacts, api/replies_agent.go), so a
// change to either moves the list and these routes together.
func inboxAgentRootsQuery(roots, replies, match string) string {
	return roots + `
	select r.root_id::text, agent.session_id, ` + replies + `, ` + inboxAgentNewestReply +
		myAgentRootsWithFacts + `
	where ` + match + `
	order by r.root_id`
}

// inboxAgentUnreadReplies is what a write that marks the thread read selects,
// inboxAgentRepliedThrough the narrowing the Inbox's own states take - the roots whose newest
// session reply is at or before the mark ($4) - and inboxAgentNewestReply the column every one
// of these reads answers last: the newest reply the session wrote in that thread, which is what
// the list route shows as the row's reply (agent.reply_id, myAgentRowFacts' `newest`,
// api/replies_agent.go). Only the dismissal's check reads it
// (inboxDismissCheckAgentRoots, api/inbox_dismiss.go); the mark-all answers it and ignores it,
// since one read serves both routes.
const (
	inboxAgentUnreadReplies  = `agent.unread_reply_ids`
	inboxAgentRepliedThrough = `agent.replied_at <= least($4::text::timestamptz, now())`
	inboxAgentNewestReply    = `agent.reply_id::text`
)

// inboxReadAgentThreads is the list route's own unread roots with that read over them: the roots
// holding a reply this login has not read, and each root's own facts, are myAgentUnreadRoots and
// myAgentRootsWithFacts, the constants myAgentRowsQuery is built from (api/replies_agent.go), so a
// change to either moves the list and this button together.
var inboxReadAgentThreads = inboxAgentRootsQuery(
	myAgentUnreadRoots, inboxAgentUnreadReplies, inboxAgentRepliedThrough)

// The lock statements of this transaction, which it takes in the order markInboxReadThreads
// writes down: the owners of the threads it marks, in shared mode (lockInboxReadOwners,
// store.LockOwnersShared), then the caller's own user_threads rows `for no key update`. The
// owners are two statements, the marked threads' documents and then their issues, each sorting
// and deduplicating its own lock keys, and the rows are ordered before they are locked, so the
// locks are taken in the sorted order rather than whatever order the scan returns, and the
// update below re-locks the rows rather than taking them in the order its own plan reaches
// them.
//
// The owners are taken by their advisory lock rather than `for share` on their rows: a row lock
// assigns the transaction a transaction id, which it would then hold across everything this
// request does, and every writer of an owner now serialises on that advisory lock instead
// (store.LockOwner, api/lock_level_test.go's TestNoForUpdateOnOwnerTables).
const inboxReadLockThreads = `
	select 1 from user_threads
	where login = $1 and thread_id = any($2::uuid[])
	order by thread_id
	for no key update`

// inboxReadMarkThreads moves the caller's mark on every Reply thread it was given, and answers
// each row it moved in the shape every mark statement of these two routes answers
// (writeInboxThreadMarks): its project, its thread and the thread root's kind, the read mark it now
// holds, the moment of the mark the statement wrote, here that read mark again, and whether it
// moved that read mark, which every row it answers did. Every row it reaches is one
// inboxReadLockThreads has just taken, so the order its own plan scans them in decides nothing.
// read_through is cast by Postgres from the caller's own text rather than bound as a parsed time,
// and stored as no later than now, exactly as putThreadRead stores one mark: a mark never moves
// back, and a time carrying nanoseconds is not truncated a microsecond short of the write it means
// to cover.
const inboxReadMarkThreads = `
	update user_threads set read_through = least($2::text::timestamptz, now())
	where login = $1 and thread_id = any($3::uuid[])
	  and read_through < least($2::text::timestamptz, now())
	returning project_key, thread_id::text, kind, read_through, read_through, true`

// inboxReadMarkAgentReplies writes the agent replies this request reads, by id, as
// putUserAgentState writes the ones a view showed: the ids are sorted before the insert so two
// writes lock new rows in one order, a reply the session's own read mark has already passed gets
// no row (the prune below would delete it again), and a row already there is left alone. No
// session's user_agent_read.read_through moves, so a reply in the live conversation that this
// request did not mark stays unread. Each row it writes is marked at the moment of the reading
// ($4 says whether the caller's marks fit the store, markFits), and answered with that moment,
// which its reply_read tuple states.
const inboxReadMarkAgentReplies = `
	insert into user_agent_reply_read (login, session_id, reply_id, tuples_stored_at)
	select $1, wanted.session_id, wanted.reply_id, case when $4::boolean then now() end
	from unnest($2::text[], $3::uuid[]) with ordinality as wanted(session_id, reply_id, position)
	where not exists (
		select 1 from messages
		join user_agent_read on user_agent_read.login = $1 and user_agent_read.session_id = wanted.session_id
		where messages.id = wanted.reply_id and messages.created_at <= user_agent_read.read_through
	)
	order by wanted.position
	on conflict do nothing
	returning session_id, reply_id::text, now()`

// inboxReadStatementTimeout bounds every statement of the mark-all's transaction. One request
// marking the acceptance seed - 600 unread Reply threads and 20 Agent threads for one login -
// takes 30 to 43 ms end to end on the test database, of which the ordered lock pass is 7 to 10
// (the same seed takes 24 to 33 ms without it), so ten seconds leaves better than two orders of
// magnitude of headroom for a busier server and cuts no honest request off. The ceiling
// matters because this transaction holds its owners' locks in shared mode: every comment, answer and
// issue message on those owners waits behind a statement that hangs, so one that cannot finish
// is better killed, which rolls the transaction back and frees them. The dismissal's transaction
// (dismissInboxThreads) holds the same locks and takes the same bound.
const inboxReadStatementTimeout = 10 * time.Second

// boundInboxStatements sets the statement_timeout of the mark-all's and the dismissal's
// transactions in their first statement, for tx alone (set_config's is_local, as `set local` sets
// it), and answers the transaction's now() it reads beside it: every statement after it is bounded
// by inboxReadStatementTimeout, unless a test set a bound of its own (server.inboxStatementBound).
func (s *server) boundInboxStatements(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var at time.Time
	err := tx.QueryRow(ctx, `select set_config('statement_timeout', $1, true), now()`,
		fmt.Sprintf("%dms", cmp.Or(s.inboxStatementBound, inboxReadStatementTimeout).Milliseconds())).Scan(new(string), &at)
	return at, err
}

// inboxReadPriorityLevels are the priorities a request names, as the Inbox's filter writes them,
// against the issues.priority each stands for. inboxReadNone is the value that stands for the
// absence of one: a thread whose issue carries no priority, a thread on a project document, which
// has no issue at all, and every Agent row, which has neither issue nor project.
var inboxReadPriorityLevels = map[string]int32{"P0": 0, "P1": 1, "P2": 2, "P3": 3}

const inboxReadNone = "none"

// inboxThreadFilters is the narrowing the Inbox's filter writes into the list route's query, as
// the two write routes over these rows read it off their body: the mark-all (inboxReadRequest) and
// the dismiss route (inboxDismissRequest, api/inbox_dismiss.go), which read it with one parser
// (parseInboxReadScope). Every list is optional and an absent one means every value. It is one
// embedded struct rather than six fields spelled in each body, so the two cannot drift into
// refusing different requests.
type inboxThreadFilters struct {
	Kind     []string `json:"kind"`
	Priority []string `json:"priority"`
	Urgency  []string `json:"urgency"`
	Project  []string `json:"project"`
	Issue    string   `json:"issue"`
	Artifact string   `json:"artifact"`
}

// narrows is whether a request carries any of them, which is what a request that names its own
// rows may not do (dismissInbox).
func (filters inboxThreadFilters) narrows() bool {
	return len(filters.Kind) > 0 || len(filters.Priority) > 0 || len(filters.Urgency) > 0 ||
		len(filters.Project) > 0 || filters.Issue != "" || filters.Artifact != ""
}

// inboxReadRequest is the body: the read mark, and that narrowing.
type inboxReadRequest struct {
	ReadThrough *string `json:"read_through"`
	inboxThreadFilters
}

// inboxReadResponse answers how many threads the request marked: the Reply threads whose mark
// moved plus the Agent threads that gained a read reply. A request that moves nothing answers 0.
type inboxReadResponse struct {
	Marked int `json:"marked"`
}

// inboxReadScope is one request's narrowing, read off its body. A nil priorities or projects list
// is every value; priorityNone and projectNone say whether a row carrying neither is in the set,
// which is what decides whether an Agent row - no issue, no project, no urgency - can be marked
// at all.
type inboxReadScope struct {
	reply, agent bool
	priorities   []int32
	priorityNone bool
	projects     []string
	projectNone  bool
	// urgencyNone is false only when the request names urgencies and none of them is "none".
	// A Reply row and an Agent row carry no urgency - only an ask the Inbox lists in Waiting on
	// you does - so such a request matches no thread and the route marks nothing.
	urgencyNone bool
	issue       string
	artifact    *string
}

// parseInboxReadScope reads the narrowing. kind defaults to both, unlike the list route's default
// of reply alone: the button sits over one list of both kinds, and a person who has filtered
// nothing means all of it.
func parseInboxReadScope(input inboxThreadFilters) (inboxReadScope, error) {
	scope := inboxReadScope{priorityNone: true, projectNone: true, urgencyNone: true}
	reply, agent, err := parseThreadKinds(input.Kind)
	if err != nil {
		return scope, err
	}
	// A request that named no kind means both of them, which is this route's default alone: the
	// list route's is reply.
	if !reply && !agent {
		reply, agent = true, true
	}
	scope.reply, scope.agent = reply, agent
	if len(input.Priority) > 0 {
		scope.priorities, scope.priorityNone = []int32{}, false
		for _, value := range input.Priority {
			name := strings.TrimSpace(value)
			if name == inboxReadNone {
				scope.priorityNone = true
				continue
			}
			level, known := inboxReadPriorityLevels[name]
			if !known {
				return scope, errorf(http.StatusBadRequest, "INVALID_QUERY",
					"priority takes P0, P1, P2, P3 and %s", inboxReadNone)
			}
			scope.priorities = append(scope.priorities, level)
		}
	}
	if len(input.Urgency) > 0 {
		scope.urgencyNone = false
		for _, value := range input.Urgency {
			name := strings.TrimSpace(value)
			if name == inboxReadNone {
				scope.urgencyNone = true
				continue
			}
			if !validUrgency(name) {
				return scope, errorf(http.StatusBadRequest, "INVALID_QUERY",
					"urgency takes low, med, high, blocking and %s", inboxReadNone)
			}
		}
	}
	if len(input.Project) > 0 {
		scope.projects, scope.projectNone = []string{}, false
		for _, value := range input.Project {
			key := strings.TrimSpace(value)
			if key == inboxReadNone {
				scope.projectNone = true
				continue
			}
			scope.projects = append(scope.projects, key)
		}
	}
	// The owner narrowing is the list route's (parseThreadOwner), read from the body rather than
	// the query.
	issue, artifact, err := parseThreadOwner(input.Issue, input.Artifact)
	if err != nil {
		return scope, err
	}
	scope.issue, scope.artifact = issue, artifact
	return scope, nil
}

// wantsReplyThreads and wantsAgentThreads say which of the two reads this scope can match a row
// in. An Agent row carries no issue, no document, no project, no priority and no urgency, so any
// narrowing to one of those leaves it out, exactly as the list route leaves it out of an
// owner-narrowed page.
func (scope inboxReadScope) wantsReplyThreads() bool {
	return scope.reply && scope.urgencyNone
}

func (scope inboxReadScope) wantsAgentThreads() bool {
	return scope.agent && scope.urgencyNone && scope.priorityNone && scope.projectNone &&
		scope.issue == "" && scope.artifact == nil
}

// inboxReadThreads is what the transaction read before it took a lock or wrote a row: the Reply
// threads to write over with the owners to lock for them, the Agent threads' unread replies, their
// sessions and their roots, and the Agent roots themselves. Both write routes over these rows read
// into it (api/inbox_dismiss.go), which is what lets them share this read, the owner lock and the
// agent-reply write.
type inboxReadThreads struct {
	threads   []string
	documents []string
	issues    []string
	// sessions and replies are one agent reply each, in the replies' sorted order, and roots
	// names each reply's thread, which is what the answer counts.
	sessions []string
	replies  []string
	roots    map[string]string
	// agentRoots are the Agent threads the request changes, sorted, and rootSessions names each
	// root's session, which is one event each. They are the dismiss route's to use: the mark-all
	// writes no row per root, it marks the replies.
	agentRoots   []string
	rootSessions map[string]string
	// replyVersions and rootVersions are what the dismissal holds each row it changes to, keyed
	// by thread id and by root id: a Reply row's user_threads.version and an Agent root's newest
	// session reply. Every read above answers the row's own; a request that named its rows drew
	// them itself, so readInboxDismissThreads (api/inbox_dismiss.go) writes what the page sent
	// over them. They are the dismissal's alone to read: the mark-all's reads fill both and
	// nothing reads them back.
	replyVersions map[string]int64
	rootVersions  map[string]string
	// drawnUnread are the unread replies the page drew in all of the Agent threads at once -
	// reply ids are unique, so one flat set serves every root, which is how the check binds it
	// (inboxDismissCheckAgentRoots, api/inbox_dismiss.go). readInboxDismissThreads fills it, from
	// the request where it named its rows and from replies above under the list's filters, where
	// the server is the page; the mark-all holds no thread to anything and leaves it empty.
	drawnUnread []string
}

// newInboxReadThreads is the empty read both routes fill, so neither has to spell which of its
// maps must exist before the first row lands in it.
func newInboxReadThreads() inboxReadThreads {
	return inboxReadThreads{
		roots: map[string]string{}, rootSessions: map[string]string{},
		replyVersions: map[string]int64{}, rootVersions: map[string]string{},
	}
}

// markInboxRead answers POST /api/v1/me/inbox/read.
func (s *server) markInboxRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input inboxReadRequest
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.ReadThrough == nil {
		writeError(w, "INVALID_STATE", http.StatusBadRequest, "read_through is required")
		return
	}
	// The cutoff is checked as every read mark here is checked - RFC3339, and no further ahead of
	// the server clock than a minute - and then carried on as the caller's own text, which the
	// statements cast.
	if _, err := parseAgentStateCutoff("read_through", input.ReadThrough); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	scope, err := parseInboxReadScope(input.inboxThreadFilters)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	marked, events, err := s.markInboxReadThreads(r.Context(), actor, *input.ReadThrough, scope)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), events...)
	WriteJSON(w, http.StatusOK, inboxReadResponse{Marked: marked})
}

// markInboxReadThreads is the one transaction: it reads the threads, takes their owners, writes
// the three marks, takes the projects it wrote in and appends one event per project and per
// session it touched. It answers how many threads it marked and the events to publish once it
// has committed.
//
// This comment is where the Inbox's read-mark lock order is written down. putThreadRead
// (api/replies.go), the dismiss route (api/inbox_dismiss.go), the lock statements above and the
// race tests point here rather than saying it again.
//
// The order is owner, then the root of every Agent thread a dismissal writes a mark for or a
// restore deletes one of, then the caller's rows, then the agent rows it writes, then the project
// its events name. Steps 2 and 4 are the dismiss route's alone - this button writes no dismissal -
// and they are numbered here rather than there because one order written in two places is two
// orders as soon as one of them is edited:
//
//  1. the owner of every thread it marks, in shared mode (store.LockOwnersShared), documents
//     then issues, each statement taking its own in ascending lock-key order
//     (lockInboxReadOwners);
//  2. the root message of every Agent thread a dismissal writes a mark for, `for update`, in
//     ascending id order (inboxDismissLockAgentRoots, api/inbox_dismiss.go), which the version
//     check that follows reads each thread under (inboxDismissCheckAgentRoots) before the insert
//     writes any mark; for a restore, the root of every Agent thread it deletes a mark of, `for no
//     key update`, in ascending id order (inboxRestoreLockAgentRoots), since that delete moves the
//     root's agent_dismissed_at (migration 20261125053125's trigger updates the root's row);
//  3. the caller's user_threads rows, `for no key update`, in ascending thread_id order
//     (inboxReadLockThreads), which the update that follows re-locks;
//  4. the caller's own user_agent_thread_dismissed rows, `for no key update`, in ascending
//     message_id order (inboxDismissLockAgentThreads, api/inbox_dismiss.go), which the insert
//     or the delete that follows re-locks;
//  5. the caller's dead user_agent_reply_read rows, `for no key update`, in ascending reply_id
//     order, which the same statement deletes (pruneAgentReplies, api/state.go); the other
//     writer of that table, putUserAgentState, takes them in that order too;
//  6. the project of every row that update moved, through LockOwners, in sorted key order and
//     before the first append.
//
// Step 2 comes before the caller's rows because that is the order a session's reply takes those
// two in, and before the caller's marks because every writer of a mark writes its root's
// agent_dismissed_at through the trigger of migration 20261125053125, which updates the root's
// row: the root is the first row of a thread any of them takes. A reply of this release takes the
// thread's root `for no key update` before its insert (inboxwrite.Recording.QueueRootLock, which
// api's replyMessage and createStoredMessage run before they take the message's attempt or write
// the row), and its writer, inboxwrite.Record, clears the root's Agent dismissals under it before
// it takes any user_threads row of the thread (Record's step 2, below). An older build's reply
// does the same through its triggers: Postgres fires a table's row triggers in name order, so on
// messages user_agent_thread_dismissed_clear (migration 20261105093507, whose body 20261125053125
// replaced) runs before user_threads_message (0095, attached by 0097) on an issue message, and on
// an Agent thread's reply it is the only one of the two that runs at all: that other trigger
// returns at once for a message with no issue key, and an Agent thread's messages carry none. It
// takes the root `for key share`, and `for no key update` before its delete wherever a mark or an
// owed row (below) of the root stands. So a session's reply takes the thread's root before any row
// of this caller's it reaches. A dismiss or a restore that took the caller's rows first and the
// root second would be the opposite order, and the two would meet each holding what the other
// waits for.
//
// The restore of the release before is that opposite order: it takes the caller's marks and no
// root, and deletes the marks. So the trigger's delete of a mark never waits at the root: it takes
// the root `for no key update skip locked`, and where another transaction holds it, leaves the
// column standing and appends the root to user_agent_thread_dismissed_owed (migration
// 20261125053125's header). Every writer of this order that holds a root then settles it after its
// own delete or write of the marks (user_agent_thread_dismissed_settle): a restore its roots
// (inboxRestoreSettleAgentRoots, api/inbox_dismiss.go) and a session's reply its own
// (inboxwrite's queueAgentDismissalsClear, and the clear trigger for an older build's reply).
//
// PUT /api/v1/me/threads/{id}/read is ordered against this transaction by the owner alone, as an
// ordinary writer is (below), and not by this list: it takes its one thread's owner lock
// exclusively and then, before its own update, the project its event names (putThreadRead,
// api/replies.go), which is steps 3 and 6 in the opposite order. The two never cross on it,
// because this transaction holds a caller's row only while it holds that row's owner in shared
// mode, and that exclusive lock conflicts with the shared one: whichever of the two reaches the
// owner second waits there holding neither the row nor the project. A shared lock there would
// cross, two share locks conflicting with nothing: this press would hold the row while it waited
// for the project and that route would hold the project while it waited for the row, a cycle
// Postgres breaks with a 40P01 that the caller reads as a 500.
//
// Rows before project here rather than project before rows, because the projects this
// transaction locks are the projects of the rows its update moved, which it learns from that
// update (inboxReadMarkThreads answers them) and not before it; a press that moved nothing locks
// none and appends none.
//
// Two presses of this button by one person - two tabs, two devices, a narrowed press against an
// unfiltered one - are ordered by step 3 and nothing else: they can name the same threads and
// the same projects, and both take their owners in shared mode. Step 3 is a pass of its own rather
// than the update's own row locks because `thread_id = any($3)` locks what its plan reaches in
// the order it reaches it, and the plans differ with the size of the array: a press of a
// thousand threads is planned as a bitmap heap scan, which locks in heap order, where a small
// press and the generic plan take an index scan on the primary key, which locks in thread_id
// order. With random thread ids those two orders disagree, so without the pass two presses over
// one set of rows could each hold a row the other waits on (TestInboxReadRacingAnotherMarkAll
// forces the two plans apart and is red without it). The pass costs one index scan of the rows
// the press named, and it takes `for no key update` on every row it named rather than on the
// ones the update moves, so a second press waits for the first over rows it would have skipped.
//
// An ordinary writer is ordered by the owner instead. A comment, an answer or an issue message
// takes its issue's or document's owner lock (store.LockOwner) before it moves the user_threads
// rows of everyone in the thread; that exclusive lock conflicts with the shared one taken here,
// and every row such a write moves belongs to a thread on the owner it holds, which is an owner
// this transaction holds too, since it locks the owner of every thread it marks. One of the two
// therefore waits at the owner while holding nothing the other wants, whichever order the writer
// itself reaches the project in: a block ask's answer appends an anchor refresh, and so takes the
// project, before its own update moves those rows, and it is ordered all the same.
//
// The rows such a write moves are moved by the one writer of a thread, inboxwrite's record
// (inboxwrite.PlanRecord and Recording.Queue), which every comment, message, ask and answer write
// of this release plans in its read phase and queues right after its event, under the owner lock
// it took first, and for a write an older build makes by the triggers of 0095-0097. The record
// takes them in this order: the participates_in tuples of the write, and of what an answer's ask
// still owes the store; then every user_threads row of the thread, `for no key update`, in
// ascending login order, in one pass (queueThreadLock), which holds the row of every person the
// write is about to deliver to; only then the delivered_to tuple of each of those people, through
// graphstore.QueueToPerson; then the move of their rows; and last the writer's own row. The
// backfill marks a row under that same lock, so no delivery falls on both sides of a row's mark
// (D4). Two writes into one thread queue at their owner lock before either reaches this pass, so
// the pass orders nothing between writers; it is what holds a mark to the deliveries around it.
// The delivered_to an ask's create appends to the person the ask waits on, with its
// participation, and the ones a move of that person appends (inboxwrite.QueueMoveWaiting) take
// no user_threads row first: neither moves a row, and the mark counts only the deliveries a reply
// caused (P13).
//
// Who an ask waits on (asks.AskedOfPersonExpression) is read from three rows, and each write that
// decides a delivery by it holds the locks those rows are written under, so the delivered person
// is the one that expression names once every such write has committed. The ask's asked_of and
// its issue's assignee are written under the owner's lock, which every create and every move of
// an ask on that owner takes first. The project's default is written under the project's owner
// lock, taken exclusive, which putProjectDefaultAssignee takes first, as its owner. A write that
// reads the default to deliver an ask - a create that falls through to it, an issue's assignee
// cleared, a reopen restoring its withdrawn asks, a settlement that opens an ask or restores one
// it retracted - takes that project's owner lock shared (inboxwrite.HoldDefault) after its own
// owner's, the order events.Broker.LockOwners takes an issue's owner and a project's, and before
// it reads who the ask waits on and before any asks row, node or tuple it writes. Then a move of
// who asks wait on (inboxwrite.QueueMoveWaiting) locks the asks rows it moves `for no key
// update`, in ascending id, right after its event and before the person nodes and tuples it
// writes, and reads who each ask waits on, and whom its delivery stands to, in the statement after
// that lock, so a move it waited for is read as it committed. A write locking several asks rows in
// one pass takes them in that ascending id order (docs' lockAskBlocksForWrite), but a write that
// locks them in several passes is in no one order with the move: the close's withdrawal, its final
// sweep and the repair's, one document after another (prepareWithdrawal, sweepIssueAskBlocksTx),
// the reopen's restore (prepareRestoration), the decisions step's restore (docs'
// restoreIssueDecisions), and a version write that locks its document's ask blocks and then moves
// its approval request (docs' moveApprovalAsk). Each takes the project's owner lock shared too,
// where the asks it writes fall through to the default, after its own owner's and before it locks
// any asks row, so a move either committed first or waits for it (docs' lockAskBlocksForWrite
// names them all). A snooze's foreign key takes its ask `for key share`, which no move waits on.
//
// The node and tuple store (internal/dispatch/graphstore) adds five invariants rather than a
// sequence of its own. Its writers do not all run their steps in one order - one learns its
// event id before it has a node to write, another writes the node first and moves source_event
// later - so a numbered step would be false for some of them, while each of these holds for
// every writer:
//
//  1. the owner's lock comes first, through store.LockOwner (store/ownerlock.go) or
//     lockArtifactOwner (docs/service.go), as it does for every other writer above: the advisory
//     lock, taken before the read phase reads what the write phase writes, and so before any
//     row of the store is taken. The owner's row is taken later, in the write phase, where an
//     event the writer appends takes it; while an earlier build, which serialises on the row,
//     is in the fleet, the row is taken right after the advisory lock (store.LockOwners);
//  2. a node's graph_nodes row is written before any projection row whose foreign key names it
//     (document_blocks, block_refs and comments, migration 20261109022248), so the write that
//     makes a node and the write that points at it never meet in the opposite order;
//  3. the event is appended before the tuples whose cause it is, so a tuple's source_event
//     names a row the log already holds;
//  4. a graph_nodes write touching several nodes takes them in ascending (kind, id)
//     (graphstore's queueNodes), so two such writes cannot each hold what the other waits for;
//  5. a writer under the revision fence (R23 of this change: the version write, the
//     block index reconciliation tick and the backfill) takes the document's owner lock, through
//     lockArtifactOwner or events.LockOwnerRows, then the document's document_block_versions row
//     `for no key update` (indexOwedDocument, refs' queueStand, graphcensus's
//     lockBlockDocuments, and a document settlement's lockSettlementFence, which takes it before
//     the asks the settlement opens are followed), before it writes a node, a tuple or a
//     projection row, so two passes over one document queue at the fence and the one that loses
//     it has written nothing.
//
// Appending a tuple locks no row of another table: graph_tuples has no foreign key and nothing
// updates or deletes a tuple, so where the append falls in a writer's sequence moves no lock,
// and invariant 3 is about which rows exist, not about which are held. A writer owning no
// issue and no document - an operator's own write, a session-addressed event - takes no owner
// lock at all, and invariant 1 says nothing about it. graphstore's
// TestEveryStoreWriteTakesOneLockOrder reads this list and fails if these five are not in it.
func (s *server) markInboxReadThreads(
	ctx context.Context, actor model.Actor, readThrough string, scope inboxReadScope,
) (int, []model.Event, error) {
	login := model.CanonicalLogin(actor.ID)
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(ctx)
	// The statement bound, set for this transaction alone as `set local` sets it, and the
	// transaction's now() beside it, which the events this route appends and the marks they cause
	// are dated at (commitInboxMarks): one statement, before the first lock.
	at, err := s.boundInboxStatements(ctx, tx)
	if err != nil {
		return 0, nil, fmt.Errorf("bound the mark-all's statements: %w", err)
	}
	read, err := readInboxReadThreads(ctx, tx, actor, login,
		inboxReadReplyThreads, inboxReadAgentThreads, readThrough, scope)
	if err != nil {
		return 0, nil, err
	}
	if err := lockInboxReadOwners(ctx, tx, read); err != nil {
		return 0, nil, err
	}

	events := []inboxEvent{}
	marked := 0
	if len(read.threads) > 0 {
		projects, err := writeInboxThreadMarks(ctx, tx, login, read.threads, inboxReadMarkThreads, []any{readThrough})
		if err != nil {
			return 0, nil, fmt.Errorf("mark the inbox's threads read: %w", err)
		}
		marked += projects.marked
		for _, project := range projects.keys {
			// One event per project, naming no thread: the viewer's other tabs and devices
			// refetch the list, which is what a thread's own mark asks of them too. It causes the
			// read_through of each thread the mark moved in that project.
			var marks []graphstore.Tuple
			for _, thread := range projects.threads[project] {
				marks = append(marks, personMark(actor, graphstore.TupleReadThrough, thread.kind, thread.id,
					thread.readThrough, true, graphstore.LatestEvent))
			}
			events = append(events, inboxEvent{
				event: projectOwner(project).event(
					"user_thread_state.updated", actor, model.UserThreadStateEventPayload{Login: login},
				),
				marks: marks,
			})
		}
	}
	if len(read.replies) > 0 {
		replies, err := s.markInboxReadAgentReplies(ctx, tx, actor, login, read)
		if err != nil {
			return 0, nil, err
		}
		marked += replies.roots
		for _, session := range replies.sessions {
			events = append(events, inboxEvent{event: userAgentStateEvent(actor, session), marks: replies.marks[session]})
		}
	}
	return s.commitInboxMarks(ctx, tx, failpoint.InboxRead, at, marked, events, false)
}

// inboxEvent is one event a write route over these rows appends, with the marks it causes: the
// tuples from the caller's person node of what the route wrote that the event announces - each
// Reply thread's for its project's event, each Agent thread's and reply's for its session's
// (api/read_marks.go). Their source_event is graphstore.LatestEvent: the event commitInboxMarks
// appends right before it queues them.
type inboxEvent struct {
	event model.Event
	marks []graphstore.Tuple
}

// commitInboxMarks ends both write routes over these rows. A request that changed nothing
// announces nothing: the Inbox marks itself read on every click, and each event would refetch the
// list in every tab the viewer has open. Such a request rolls back unless mayHaveWritten, which
// only a restore that locked Agent roots passes: the one write a request that changed nothing can
// hold is that restore's settle of its roots (inboxRestoreSettleAgentRoots, api/inbox_dismiss.go),
// which clears a column another restore left owed and changes no answer - an owed root with no mark
// reads as live before it and after it (myAgentOwedRoot) - and which it commits. A request that
// changed something takes the project of every event it appends - step 6 of the lock order above,
// in one sorted order after the rows it wrote and before the first append, an agent reply's event
// naming no owner and so taking no lock here - and then appends them, each followed by the marks
// it causes, in one statement under that event (graphstore.QueueFromPerson), which takes no row
// lock but the person's node, `do nothing` where the store holds it, all in one batch, and
// commits. Every mark is dated at, the transaction's now(), which the events carry too. It answers
// the count its caller answers, 0 when nothing changed, and the events to publish once the
// transaction has committed. writer names the route to the seam a test injects a failure through
// between the rows the route wrote and the marks (failpoint.InboxRead, failpoint.InboxDismiss),
// decided before the batch is sent and queued before the first mark.
func (s *server) commitInboxMarks(
	ctx context.Context, tx pgx.Tx, writer string, at time.Time, changed int, events []inboxEvent, mayHaveWritten bool,
) (int, []model.Event, error) {
	if len(events) == 0 {
		if !mayHaveWritten {
			return 0, nil, tx.Rollback(ctx)
		}
		return 0, nil, tx.Commit(ctx)
	}
	owners := make([]model.Event, len(events))
	for index, event := range events {
		owners[index] = event.event
	}
	if err := s.deps.Events.LockOwners(ctx, tx, owners...); err != nil {
		return 0, nil, fmt.Errorf("lock the changed threads' projects: %w", err)
	}
	fails := failpoint.Planned(ctx, writer)
	appended := make([]model.Event, len(events))
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		for index, event := range events {
			if err := s.deps.Events.QueueAppend(b, event.event, func(written model.Event) { appended[index] = written }); err != nil {
				return err
			}
			if index == 0 {
				failpoint.Queue(b, fails)
			}
			graphstore.QueueFromPerson(b, at, event.marks)
		}
		return nil
	}); err != nil {
		return 0, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, err
	}
	return changed, appended, nil
}

// readInboxReadThreads reads both kinds of thread one filtered request matches, and the owners
// to lock for them, each list sorted so the locks that follow are taken in one order. Both write
// routes over these rows read through it - the mark-all with its own two statements and
// read_through, the dismiss route with its plan's two and dismiss_through
// (readInboxDismissThreads, api/inbox_dismiss.go) - which is why the two statements and the mark
// are given to it rather than named in it. The mark is bound rather than read here, so it is
// whatever the caller binds its own text as: the mark-all's string, the dismissal's *string.
func readInboxReadThreads(
	ctx context.Context, tx pgx.Tx, actor model.Actor, login string,
	replyThreads, agentThreads string, mark any, scope inboxReadScope,
) (inboxReadThreads, error) {
	read := newInboxReadThreads()
	if scope.wantsReplyThreads() {
		if err := readInboxReplyRows(ctx, tx, &read, replyThreads,
			login, scope.issue, scope.artifact, mark,
			scope.priorities, scope.priorityNone, scope.projects); err != nil {
			return read, err
		}
	}
	if scope.wantsAgentThreads() {
		args := append(unreadDirectRepliesArgs(actor.ID, nil), mark)
		if err := readInboxAgentRows(ctx, tx, &read, agentThreads, args); err != nil {
			return read, err
		}
	}
	return read, nil
}

// readInboxReplyRows reads the Reply threads one statement matches, the owners to lock for them
// and the version of each row, each list sorted. Both write routes over these rows read through
// it: their statements differ in the state they match and in the mark they narrow by
// (inboxReplyMatchQuery), and every one of them answers the version (api/inbox_dismiss.go).
func readInboxReplyRows(
	ctx context.Context, tx pgx.Tx, read *inboxReadThreads, query string, args ...any,
) error {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read the inbox's threads: %w", err)
	}
	defer rows.Close()
	documents, issues := map[string]struct{}{}, map[string]struct{}{}
	// The scan's targets are the same for every row, so they are taken once and read out below.
	var thread string
	var issueKey, artifactID *string
	var version int64
	for rows.Next() {
		if err := rows.Scan(&thread, &issueKey, &artifactID, &version); err != nil {
			return fmt.Errorf("scan one of the inbox's threads: %w", err)
		}
		read.threads = append(read.threads, thread)
		read.replyVersions[thread] = version
		// One owner per thread, as every reader and writer of its rows names it (store.OwnerOf):
		// the issue when it has one, since an issue owns its document and everything under it,
		// and otherwise the document a project document's thread hangs on.
		if issueKey != nil {
			issues[*issueKey] = struct{}{}
		} else if artifactID != nil {
			documents[*artifactID] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the inbox's threads: %w", err)
	}
	read.documents = slices.Sorted(maps.Keys(documents))
	read.issues = slices.Sorted(maps.Keys(issues))
	return nil
}

// readInboxAgentRows reads the Agent threads one statement matches: each root, its session, the
// replies a write that marks the thread read writes by id, sorted as those writes take them, and
// the newest session reply in the root's own thread, which every one of these statements answers
// (inboxAgentRootsQuery). The roots are sorted too, for the dismissal that writes one row per
// root (api/inbox_dismiss.go).
func readInboxAgentRows(
	ctx context.Context, tx pgx.Tx, read *inboxReadThreads, query string, args []any,
) error {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read the inbox's agent threads: %w", err)
	}
	defer rows.Close()
	sessions := map[string]string{}
	var root, session, newest string
	var replies []string
	for rows.Next() {
		if err := rows.Scan(&root, &session, &replies, &newest); err != nil {
			return fmt.Errorf("scan one of the inbox's agent threads: %w", err)
		}
		read.agentRoots = append(read.agentRoots, root)
		read.rootSessions[root] = session
		read.rootVersions[root] = newest
		for _, reply := range replies {
			read.replies = append(read.replies, reply)
			read.roots[reply] = root
			sessions[reply] = session
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the inbox's agent threads: %w", err)
	}
	slices.Sort(read.agentRoots)
	// One lock order for the new rows, the ids' own, as putUserAgentState sorts them: two
	// writes naming the same replies in opposite orders would wait on each other.
	slices.Sort(read.replies)
	read.sessions = make([]string, 0, len(read.replies))
	for _, reply := range read.replies {
		read.sessions = append(read.sessions, sessions[reply])
	}
	return nil
}

// lockInboxReadOwners takes the owner of every thread this request marks, in shared mode, step 1
// of the lock order markInboxReadThreads writes down. The caller's rows follow, in
// writeInboxThreadMarks, and the projects after them.
//
// The documents go first and the issues after them, one statement each, which is the order every
// request takes them in; each statement sorts its own owners by lock key (store.LockOwnersShared),
// so two requests naming overlapping sets take them in one order throughout. A single statement
// over both would order the two kinds by their hashes instead, and a request waiting on a
// document would then already hold the issues that hashed below it, which is a lock an ordinary
// writer of that issue waits for while the request waits for something else
// (TestDismissLeavesAThreadRepliedToWhileItRan).
func lockInboxReadOwners(ctx context.Context, tx pgx.Tx, read inboxReadThreads) error {
	documents := make([]store.Owner, 0, len(read.documents))
	for _, document := range read.documents {
		documents = append(documents, store.ArtifactOwner(document))
	}
	if err := store.LockOwnersShared(ctx, tx, documents...); err != nil {
		return fmt.Errorf("lock the marked threads' documents: %w", err)
	}
	issues := make([]store.Owner, 0, len(read.issues))
	for _, issue := range read.issues {
		issues = append(issues, store.IssueOwner(issue))
	}
	if err := store.LockOwnersShared(ctx, tx, issues...); err != nil {
		return fmt.Errorf("lock the marked threads' issues: %w", err)
	}
	return nil
}

// inboxReadMarked is what one mark statement moved: how many rows, the distinct projects they
// belong to, sorted, which is one event each, and the rows themselves by project, which are the
// marks those events cause.
type inboxReadMarked struct {
	marked  int
	keys    []string
	threads map[string][]inboxMarkedThread
}

// inboxMarkedThread is one row a mark statement moved, as every such statement answers it: its
// thread and the kind of the thread's root, which is the far end of the mark's tuple, the read
// mark the row now holds, the moment of the mark the statement wrote - the read mark itself for
// the mark-all, the dismissal for a dismissal, the moment of the write for a restore
// (api/inbox_dismiss.go) - and whether the statement moved the row's read mark, which is where a
// read_through tuple is stated.
type inboxMarkedThread struct {
	id, kind    string
	readThrough time.Time
	at          time.Time
	readMoved   bool
}

// writeInboxThreadMarks takes the caller's rows in thread_id order, runs one mark statement over
// them and reads back every row it changed. A row the statement leaves alone - a thread already
// read past the mark, one already dismissed, one that carries no dismissal under a restore - is
// locked with the rest, since what orders two requests must be the rows they named rather than the
// rows one of them happens to move, and it is not among those answered.
//
// Every statement it runs binds the caller ($1), the mark it stores when it stores one, the rows,
// and last whatever the statement checks those rows against (inboxMarkArgs), so mark carries that
// one parameter or none - a restore stores none - and seen the dismissal's versions or none: the
// mark-all checks no version and binds neither (api/inbox_dismiss.go). Every one of them answers
// each row it moved as inboxMarkedThread reads it, after its project.
func writeInboxThreadMarks(
	ctx context.Context, tx pgx.Tx, login string, threads []string, query string, mark []any, seen ...any,
) (inboxReadMarked, error) {
	moved := inboxReadMarked{threads: map[string][]inboxMarkedThread{}}
	// Step 3 of that order: the update below locks what its plan reaches in the order it
	// reaches it, which is not one order across two presses (markInboxReadThreads).
	if _, err := tx.Exec(ctx, inboxReadLockThreads, login, threads); err != nil {
		return moved, fmt.Errorf("lock the caller's threads: %w", err)
	}
	rows, err := tx.Query(ctx, query, inboxMarkArgs(login, mark, threads, seen...)...)
	if err != nil {
		return moved, err
	}
	defer rows.Close()
	for rows.Next() {
		var project string
		var thread inboxMarkedThread
		if err := rows.Scan(&project, &thread.id, &thread.kind, &thread.readThrough, &thread.at, &thread.readMoved); err != nil {
			return moved, err
		}
		moved.marked++
		moved.threads[project] = append(moved.threads[project], thread)
	}
	if err := rows.Err(); err != nil {
		return moved, err
	}
	moved.keys = slices.Sorted(maps.Keys(moved.threads))
	return moved, nil
}

// inboxMarkArgs are one mark statement's parameters, the shape every mark on these two routes
// takes: the caller, the mark it stores if it stores one, the rows it writes over, and what it
// checks them against if it checks anything.
func inboxMarkArgs(login string, mark []any, rows []string, seen ...any) []any {
	return append(append(append([]any{login}, mark...), rows), seen...)
}

// inboxReadReplies is what the agent-reply write did: the sessions whose insert wrote a row,
// sorted, which is one event each, how many agent threads those rows belong to, which the answer
// counts, and by session the marks those events cause - the reply_read of each row it wrote, and
// the retraction of each dead row its prune deleted.
type inboxReadReplies struct {
	sessions []string
	roots    int
	marks    map[string][]graphstore.Tuple
}

// markInboxReadAgentReplies writes the agent replies by id and prunes what the sessions' own read
// marks already cover, and answers what it did (inboxReadReplies).
func (s *server) markInboxReadAgentReplies(
	ctx context.Context, tx pgx.Tx, actor model.Actor, login string, read inboxReadThreads,
) (inboxReadReplies, error) {
	written := inboxReadReplies{marks: map[string][]graphstore.Tuple{}}
	rows, err := tx.Query(ctx, inboxReadMarkAgentReplies, login, read.sessions, read.replies, markFits(actor))
	if err != nil {
		return written, fmt.Errorf("mark the inbox's agent replies read: %w", err)
	}
	roots := map[string]struct{}{}
	for rows.Next() {
		var session, reply string
		var at time.Time
		if err := rows.Scan(&session, &reply, &at); err != nil {
			rows.Close()
			return written, fmt.Errorf("scan a marked agent reply: %w", err)
		}
		written.marks[session] = append(written.marks[session],
			personMark(actor, graphstore.TupleReplyRead, graphstore.KindMessage, reply, at, true, graphstore.LatestEvent))
		roots[read.roots[reply]] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return written, fmt.Errorf("mark the inbox's agent replies read: %w", err)
	}
	written.sessions = slices.Sorted(maps.Keys(written.marks))
	written.roots = len(roots)
	if len(written.sessions) > 0 {
		// Step 5 of the lock order above: the dead rows, taken in one order before the delete
		// reaches them, as putUserAgentState takes them. Nothing this request writes is ever
		// dead (the insert above skips what the mark covers), so this clears only what an older
		// write left behind; why the statement locks them in that order is written on
		// pruneAgentReplies (api/state.go). Each row it deletes is a reply_read its session's
		// event retracts.
		pruned, err := prunedAgentReplies(ctx, tx, actor, login, written.sessions)
		if err != nil {
			return written, fmt.Errorf("prune the agent replies the read marks cover: %w", err)
		}
		for session, retracted := range pruned {
			written.marks[session] = append(written.marks[session], retracted...)
		}
	}
	return written, nil
}
