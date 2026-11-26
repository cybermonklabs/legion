package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The Agent rows of GET /api/v1/me/replies: the statements each state reads them through, the
// statement picker, the row read and the session names the page shows. The route itself, the Reply
// rows and everything both halves share - the kinds, the states, the paging, the cursor and
// listMyReplies, which merges the two - are api/replies.go's.
//
// It is a file of its own because the two halves share nothing but that route. A Reply row is a
// user_threads row read through one of three partial indexes; an Agent row is built from the
// messages themselves, one walk per root, under unread_replies.go's definition of an unread reply,
// and the pair of them in one file ran to twelve hundred lines of which no reader needed both.
// What the split costs is that a change to the answer's shape (model.MyReplyRow) touches two
// files, which is what the route's own comment on the two blocks already says of its two reads.

// The Agent half of every Inbox state, read one root at a time.
//
// An Agent row is one of the person's one-off messages to an agent session - a BTW, a broadcast's
// copy, or a Send or Aside the session did not take as a turn of its live conversation - holding a
// reply from that session. Those rows come from the messages themselves, since an Agent thread has
// no row of its own, so every read here starts from the roots its state names and then asks each
// root's own thread what the row shows:
//
//   - state=unread reads the roots holding a reply the person has not read;
//   - state=inbox reads the roots they have not dismissed;
//   - state=dismissed reads the dismissal marks themselves, in the order the view answers in, cut
//     to the page before any thread is walked, so a page costs the page.
//
// Each read is one pass over its own roots with index probes per root: the walk of one root's
// thread (messages_in_reply_to), the session's marks and the replies read by id. A dismissal is the
// root's own column for the Inbox's two states (messages.agent_dismissed_at, whose index of the
// live roots, messages_direct_roots_live, holds no dismissed root), and the marks themselves,
// through user_agent_thread_dismissed's login/dismissed_at index, for the Dismissed view. The shape
// this replaces built one recursive CTE over every one-off message the person had ever sent and
// then scanned that whole CTE once per root, which cost the Dismissed view's page read 433 ms and
// its count 7.4 s at 10,000 dismissed Agent threads (INFRA-450's a14).
//
// The definition of "a reply this login has not read in this session" is unread_replies.go's
// (unreadDirectRepliesCTE), which the Agents page's New replies badge and the conversation window
// read: a session's own reply under a root this person targeted at that session, newer than the
// later of their read mark and their Clear, both read from their one person node ($1) whatever
// spelling of their login wrote them (directMarks), and not one they have read by its id
// (replyReadByID). The reads here ask that question of one root at a time rather than of every
// root at once, through those same two readings of the marks, and myAgentUnreadReplyTest is the
// one place this file spells it, so the two can differ in what they cost and not in what they
// mean.
//
// Parameters are unreadDirectRepliesCTE's own (unreadDirectRepliesArgs), so the first free one is
// $4, the number of rows a page read takes (null for every row), and a Dismissed page resumes from
// $5 and $6; a count read binds neither.

// agentThreadCTE walks one root's thread: every message under it, at any depth, through
// messages_in_reply_to. root is the expression naming that root, so the walk is per root and a
// read pays for the roots it answers.
func agentThreadCTE(root string) string {
	// The walk's own alias is `written`, never `m`: root is an expression of the read above this
	// one, and an alias this CTE shared with that read would shadow it - `m.in_reply_to = m.id`,
	// which answers no row at all.
	return `
		with recursive thread as (
			select written.id, written.author, written.created_at, written.body
			from messages written where written.in_reply_to = ` + root + `
			union all
			select written.id, written.author, written.created_at, written.body
			from messages written join thread on written.in_reply_to = thread.id
		)`
}

// agentSessionOf reads the session a root was targeted at out of its target, as
// unreadDirectRepliesCTE reads it.
func agentSessionOf(target string) string {
	return `substr(` + target + `, length('session:') + 1)`
}

// myAgentMarks is every session this person has a read mark or a Clear for, which is
// unreadDirectRepliesCTE's own direct_marks read for every session (directMarks): the read
// mark and the Clear from the person's one node, and a row no tuple states yet on the key the
// release before read it by. It reads $1 and $2 alone, since a read here may bind $3 for a statement
// of its own (inboxDismissCheckAgentRoots). It is materialized and read once per statement rather
// than per root: it is one row a session they have ever read or cleared, each read off the index
// in two probes a kind however many marks they stated there (newestMarks), where the roots are
// every one-off message they ever sent.
var myAgentMarks = `
	with marks as materialized (
		select session_id, at from (` + directMarks(false) + `
		) direct
	),`

// agentMarkOf is the later of the person's read mark and their Clear for one session, which a
// reply is unread after. A session they have neither for reads as -infinity, so every reply in it
// is unread, exactly as directMarks leaves it.
func agentMarkOf(session string) string {
	return `coalesce((select at from marks where marks.session_id = ` + session + `),
			  '-infinity'::timestamptz)`
}

// myAgentUnreadReplyTest is this file's one spelling of "a reply the person has not read": newer
// than the mark for its session, and not read by its id (replyReadByID). row names the walk's row
// the reply is read from and mark the mark, as expressions, since each read asks it of a row of
// its own walk - and each read takes the mark once per root rather than once per reply, which is
// two index reads a reply saved.
//
// The time test is asked first, and the read-by-id test only of a reply newer than the mark, by a
// case rather than an `and`: Postgres orders a filter's conditions by what it estimates each
// costs, and it costs replyReadByID's two probes at nothing while user_agent_reply_read holds no
// row that no tuple states yet, which is every database the backfill has passed. So an `and` ran
// both probes for every session reply of every root before it looked at the time, which answers
// no for nearly all of them: c-p99's Mark all read of the heavy person spent 19.5 ms in its Agent
// read, which walks 2,000 roots, where the base build, whose one probe Postgres runs as an
// anti-join after the time test, spent 7.7 ms (pg_stat_statements, 201 presses, 2026-11-25), and
// the same read with this case took 7.3 ms at its p50 against 18.4 to 24.5 as an `and`. The two
// answer alike for every row: neither side of the `and` is ever null - a message's created_at is
// never null, and mark is read through agentMarkOf, which is never null either.
func myAgentUnreadReplyTest(row, mark string) string {
	return `(case when ` + row + `.created_at > ` + mark + `
			  then not ` + replyReadByID(row+".id") + ` else false end)`
}

// myAgentRootFilter is what makes a message one of the person's one-off messages to a session: an
// issue-less message of theirs targeted at a session, which no session took as a turn of its live
// conversation. row names the relation it reads the message from, since the Dismissed view reads
// that message by its primary key (myAgentRootIsOneOff) and the Inbox's own states read it off
// messages_direct_roots_live (myAgentLiveRoot).
//
// The acceptance test reads accepted_at alone, the predicate of migration 0054's partial unique
// index message_deliveries_one_acceptance, so it is one probe of that index a root, answered from
// the index. It means accepted_as = 'user_turn': that migration's checks set accepted_at and
// accepted_as together (message_deliveries_accepted_check) and admit no accepted_as but user_turn
// (message_deliveries_accepted_as_check), so an attempt carries an acceptance exactly when a session
// took it as a user turn, and a second kind of acceptance added to that check is to be named here.
// Naming accepted_as, alone or beside accepted_at, leaves the probe a filter to read off the heap,
// and the plan, which takes the roots to be one, costs that above a scan of the whole table a root:
// c-p99's heavy read scanned message_deliveries once for each of 2,000 roots, 34 ms of the read
// (INFRA-4948).
func myAgentRootFilter(row string) string {
	return row + `.issue_key is null and ` + row + `.in_reply_to is null
		  and ` + row + `.target like 'session:%'
		  and ` + row + `.author->>'kind' = 'user' and lower(` + row + `.author->>'id') = $1
		  and ($3::text is null or ` + row + `.target = 'session:' || $3::text)
		  and not exists (
			select 1 from message_deliveries delivered
			where delivered.message_id = ` + row + `.id and delivered.accepted_at is not null
		  )`
}

// myAgentRootIsOneOff asks that of the root a dismissal mark names, read by the message's primary
// key. Two fences, both load-bearing. The outer `offset 0` stops the planner pulling the test up
// into a join, which would drive the read from messages_direct_roots - every one-off message the
// person ever sent - rather than from the marks the page is cut from. The inner one leaves the
// lookup with one condition, the id, so it is a primary-key read: given the author test as well,
// the planner reads messages_direct_roots and filters it by id instead, which walks that index to
// wherever the root sits - 0.6 ms a root 50,000 marks deep, against 0.02 ms by key.
var myAgentRootIsOneOff = `
		exists (
			select 1 from (
				select m.id, m.issue_key, m.in_reply_to, m.target, m.author
				from messages m where m.id = d.message_id
				offset 0
			) root
			where ` + myAgentRootFilter("root") + `
			offset 0
		)`

// myAgentLiveRoot is what leaves a dismissed root out of the Inbox's own two states: the root's own
// mark of its author's dismissal (messages.agent_dismissed_at, migration 20261125052902), which the
// trigger of 20261125053125 keeps from user_agent_thread_dismissed in the statement that writes a
// mark, whichever build writes it. What "dismissed" means on the Agent half is a mark existing,
// with no comparison of two times: a session's reply deletes its root's marks in the transaction
// it is written in (inboxwrite's queueAgentDismissalsClear, and for an older build's reply the
// trigger of migration 20261105093507), as user_threads_record clears a Reply thread's
// (20261105093501), so a thread that came back carries no mark and its root no column to find.
//
// The test is the predicate of the partial index messages_direct_roots_live (20261125053341), so a
// read takes the person's live roots off that index and reads no dismissed root and no mark. It
// replaced the marks read as one set the roots were hashed against: one index-only scan of every
// mark the person ever wrote, beside an index entry of messages_direct_roots for every root they
// ever sent, each dismissed one read and thrown away - at 5,000 dismissed Agent threads 10,020 rows
// for 20 roots in the Inbox, where 250 dismissed read 520
// (TestTheOneReadsInboxStatesDoNotGrowWithTheDismissedHistory).
//
// A root whose column outlived its author's mark is an owed one (myAgentOwedRoot), which each
// state reads in a branch of its own beside this one.
func myAgentLiveRoot(root string) string {
	return root + `.agent_dismissed_at is null`
}

// myAgentOwedRoot is the other way a root is live: a root whose column stands set with no mark of
// the person's behind it. A delete of a mark that found the root held by another transaction - a
// restore of the release before, which takes the mark and no root, meeting a reply of either
// build - leaves the column as it stood and appends the root to user_agent_thread_dismissed_owed
// (migration 20261125053125's header) until the writer holding the root, or the maintenance sweep
// (sweepAgentDismissalsOwed), settles it. Each of the Inbox's two states reads such roots in a
// branch of their own, over the owed roots alone, rather than as an `or` beside myAgentLiveRoot:
// the live branch keeps its plan off messages_direct_roots_live, and this one reads nothing while
// the table is empty, which it is but for those moments: it reads the roots off the owed rows
// (myAgentOwedRoots), never the person's roots off messages, which a filter on their column would
// read whole - 5,120 Agent-arm rows at 5,000 dismissed threads where 620 read at 250
// (TestTheOneReadsInboxStatesDoNotGrowWithTheDismissedHistory). A root its author's mark still
// names is dismissed, whatever the table owes of it.
func myAgentOwedRoot(root string) string {
	return root + `.agent_dismissed_at is not null
		  and not exists (
			select 1 from user_agent_thread_dismissed d
			where d.message_id = ` + root + `.id and d.login = $1
		  )`
}

// myAgentOwedRoots is the owed branch's source of roots: each root the owed table names, once,
// and its message read by its key. The lateral's `offset 0` keeps the planner from pulling the
// message read up into a join it could start from the person's roots, so the branch reads as many
// messages as the table names roots.
const myAgentOwedRoots = `(select distinct owed.root_id from user_agent_thread_dismissed_owed owed) owed
		cross join lateral (select * from messages m where m.id = owed.root_id offset 0) m`

// myAgentRowFacts is what one root's own thread says about its row, read in one walk of it: the
// session it is in and the root's text as the subject, the newest reply the session wrote there -
// the row's reply, and the time the Inbox's order reads - and the replies the person has not
// read, how many and their ids, which a dismissal and a mark-all write by id. It is an inner
// lateral, so a root no session ever answered is no Agent row, as it was when these rows were
// read from the replies themselves.
//
// `newest` - `order by created_at desc, id desc limit 1` - and unread_reply_ids are what the
// page draws of an Agent row and what a dismissal of that row carries back
// (inboxDismissCheckAgentRoots, api/inbox_dismiss.go). That check reads them out of this same
// lateral rather than spelling the walk again, so the reply the page drew and the reply the
// check reads are one reply by construction: a second spelling that named another would refuse
// every dismissal this page's own answer asks for.
var myAgentRowFacts = `
	join lateral (` + agentThreadCTE("r.root_id") + `,
		root as (
			select m.body, ` + agentSessionOf("m.target") + ` as session_id
			from messages m where m.id = r.root_id
		),
		mark as (
			select ` + agentMarkOf("(select session_id from root)") + ` as at
		),
		replies as (
			select t.id, t.author, t.created_at, t.body,
			       ` + myAgentUnreadReplyTest("t", "(select at from mark)") + ` as unread
			from thread t where t.author->>'kind' = 'session'
		)
		select (select session_id from root) as session_id, (select body from root) as subject,
		       (select max(created_at) from replies) as replied_at,
		       (select count(*) from replies where unread)::int as unread_count,
		       coalesce((select array_agg(id::text order by created_at, id) from replies where unread),
		                '{}'::text[]) as unread_reply_ids,
		       newest.id as reply_id, newest.author as reply_author, newest.body as reply_body,
		       newest.created_at as reply_at
		from (select id, author, body, created_at from replies order by created_at desc, id desc limit 1) newest
	) agent on true`

// myAgentRootsWithFacts is what every read over a state's roots selects from: the roots, and per
// root its own thread's facts.
var myAgentRootsWithFacts = `
	from roots r` + myAgentRowFacts

// myAgentUnreadRootsOf is one branch of state=unread's roots: the person's one-off roots that pass
// live, each holding a session reply the person has not read. myAgentInboxRootsOf is one branch of
// state=inbox's: the person's one-off roots that pass live, read or not. Each reads its roots from
// from, `messages m` for the live branch and myAgentOwedRoots for the owed one
// (myAgentLiveAndOwedRoots).
func myAgentUnreadRootsOf(from, live string) string {
	return `
		select m.id as root_id, null::timestamptz as dismissed_at
		from ` + from + `
		cross join lateral (select ` + agentMarkOf(agentSessionOf("m.target")) + ` as at) mark
		where ` + myAgentRootFilter("m") + `
		  and ` + live + `
		  and exists (` + agentThreadCTE("m.id") + `
			select 1 from thread t
			where t.author->>'kind' = 'session'
			  and ` + myAgentUnreadReplyTest("t", "mark.at") + `
		  )`
}

func myAgentInboxRootsOf(from, live string) string {
	return `
		select m.id as root_id, null::timestamptz as dismissed_at
		from ` + from + `
		where ` + myAgentRootFilter("m") + `
		  and ` + live
}

// myAgentLiveAndOwedRoots is a state's roots with each branch built by branch: the live branch
// over messages (myAgentLiveRoot), then, in one union, the owed branch over myAgentOwedRoots
// (myAgentOwedRoot).
func myAgentLiveAndOwedRoots(branch func(from, live string) string) string {
	return branch(`messages m`, myAgentLiveRoot("m")) + `
		union all` + branch(myAgentOwedRoots, myAgentOwedRoot("m"))
}

// The roots each state reads, one `roots` set per state, each carrying the root and the dismissal
// mark, which only the Dismissed view's rows have.
var (
	// The Inbox's own two states leave a dismissed root out before they read anything of its
	// thread: the root's own column says it is dismissed (myAgentLiveRoot), so the test is the
	// predicate of the index the roots are read off rather than a walk of every thread the person
	// ever dismissed. The unread state leaves them out too, and may: a dismissal - a Dismiss press,
	// or the person's own reply under the thread (inboxwrite.Record's step 6, the own-answer rule,
	// R1) - marks every reply in the thread read by id, and a reply written since cleared the mark
	// in its own transaction, so a root that still carries one holds nothing unread. Each adds, in a
	// branch of its own, the owed roots whose column outlived the person's mark (myAgentOwedRoot); a
	// root is in at most one of the two branches, since one reads its column null and the other set.
	myAgentUnreadRoots = myAgentMarks + `
	roots as (` + myAgentLiveAndOwedRoots(myAgentUnreadRootsOf) + `
	)`
	myAgentInboxRoots = myAgentMarks + `
	roots as (` + myAgentLiveAndOwedRoots(myAgentInboxRootsOf) + `
	)`
	// The Dismissed view's roots are the marks themselves, in the order the view answers in, so
	// one index scan answers the narrowing, the order and the cursor
	// (user_agent_thread_dismissed_login_dismissed_at). Every test a mark has to pass to be a row
	// is above the cut, so the page the index cuts is the page the read answers.
	// myAgentDismissedRootsSelect is that read of the marks, which GET /api/v1/me/inbox cuts and
	// merges with the Reply rows' own (meInboxDismissedKeys), and myAgentDismissedRootsHead opens
	// the roots set with it.
	myAgentDismissedRootsSelect = `
		select d.message_id as root_id, d.dismissed_at
		from user_agent_thread_dismissed d
		where d.login = $1
		  and ` + myAgentRootIsOneOff
	myAgentDismissedRootsHead = myAgentMarks + `
	roots as (` + myAgentDismissedRootsSelect
	myAgentDismissedCursor = `
		  and (d.dismissed_at, d.message_id) < ($5::text::timestamptz, $6::uuid)`
	// The list's own cut. A write route over the same marks appends a narrowing of its own
	// instead, its $4 being a mark or a list of ids rather than a page size.
	myAgentDismissedPageCut = `
		order by d.dismissed_at desc, d.message_id desc
		limit $4
	)`
	myAgentDismissedRootsEnd = `
	)`
	myAgentDismissedRoots = myAgentDismissedRootsHead + myAgentDismissedRootsEnd
	myAgentDismissedPage  = myAgentDismissedRootsHead + myAgentDismissedPageCut
	myAgentDismissedFrom  = myAgentDismissedRootsHead + myAgentDismissedCursor + myAgentDismissedPageCut
)

// myAgentRowColumns is what a list read answers for one row, and myAgentRowsOrder the order the
// Inbox's own two states answer in. The Dismissed view needs no order of its own: its roots are cut
// in the order it answers in, and the route sorts the two kinds of row together (listMyReplies)
// whatever order each half read them in.
const myAgentRowColumns = `
	select r.root_id::text, agent.session_id, agent.replied_at, r.dismissed_at, agent.subject,
	       agent.reply_id::text, agent.reply_author, agent.reply_body, agent.reply_at,
	       agent.unread_count, agent.unread_reply_ids`

const myAgentRowsOrder = `
	order by agent.replied_at desc, r.root_id desc
	limit $4`

// myAgentCountedColumns adds the state's own total to each row, for a paged read of the Inbox's
// own two states: both already read every root the state holds in order to order them, so the
// count over that set is the window's and costs no second pass over the person's agent threads -
// one of the two reads a badge refresh used to make, and the more expensive of them at a14's seed.
// The Dismissed view takes no total at all (listMyReplies), which is what leaves its page free to
// stop at the rows it answers.
const myAgentCountedColumns = myAgentRowColumns + `,
	       count(*) over ()::int as total`

var (
	myAgentRowsSelect        = myAgentRowColumns + myAgentRootsWithFacts
	myAgentCountedRowsSelect = myAgentCountedColumns + myAgentRootsWithFacts
)

var (
	// myAgentRowsQuery reads the one-off roots holding a reply the caller has not read, and
	// myAgentCountedRowsQuery the same rows with the state's own total beside each.
	myAgentRowsQuery        = myAgentUnreadRoots + myAgentRowsSelect + myAgentRowsOrder
	myAgentCountedRowsQuery = myAgentUnreadRoots + myAgentCountedRowsSelect + myAgentRowsOrder
	// myAgentInboxRowsQuery reads every undismissed one-off root holding a session reply, read or
	// not, and myAgentDismissedRowsQuery the dismissed ones, newest dismissal first, from the
	// cursor in myAgentDismissedFromQuery's case.
	myAgentInboxRowsQuery        = myAgentInboxRoots + myAgentRowsSelect + myAgentRowsOrder
	myAgentCountedInboxRowsQuery = myAgentInboxRoots + myAgentCountedRowsSelect + myAgentRowsOrder
	myAgentDismissedRowsQuery    = myAgentDismissedPage + myAgentRowsSelect
	myAgentDismissedFromQuery    = myAgentDismissedFrom + myAgentRowsSelect
)

// myAgentPageQuery is the statement one state reads its Agent rows through: the cursor's own query
// when a Dismissed page resumes from one, and the form that answers the state's total beside each
// row when the read is counted. The Dismissed view is never counted, so it has no counted form.
func myAgentPageQuery(state string, from *myRepliesCursor, counted bool) string {
	switch state {
	case myRepliesStateInbox:
		if counted {
			return myAgentCountedInboxRowsQuery
		}
		return myAgentInboxRowsQuery
	case myRepliesStateDismissed:
		if from != nil {
			return myAgentDismissedFromQuery
		}
		return myAgentDismissedRowsQuery
	default:
		if counted {
			return myAgentCountedRowsQuery
		}
		return myAgentRowsQuery
	}
}

// readMyAgentRows reads one state's Agent rows under the parameters myRepliesPageArgs built, and
// the state's own total when the query carries it (counted), which is the same number for every
// row of the page and 0 when the page holds none - the read having matched no root at all, since
// the route cuts the offset itself and the statement takes none. Their sessions are named once the
// merged page is cut, by nameAgentSessions.
func (s *server) readMyAgentRows(
	ctx context.Context, tx pgx.Tx, query string, args []any, counted bool,
) ([]model.MyReplyRow, int, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list my agent threads: %w", err)
	}
	defer rows.Close()
	read := []model.MyReplyRow{}
	total := 0
	var extra []any
	if counted {
		extra = append(extra, &total)
	}
	for rows.Next() {
		row, err := scanMyAgentRow(rows, s.deps.ServerURL, extra...)
		if err != nil {
			return nil, 0, err
		}
		read = append(read, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list my agent threads: %w", err)
	}
	return read, total, nil
}

// scanMyAgentRow decodes one Agent row myAgentRowColumns selects, extra receiving the columns
// after them: the row listMyReplies answers, and the agent row of an entry GET /api/v1/me/inbox
// answers. Its session is named by its id until nameAgentSessions names it, and its subject and
// its reply's first line are cut against serverURL, this server's deployment (refs.FirstLine).
func scanMyAgentRow(scanned pgx.Row, serverURL string, extra ...any) (model.MyReplyRow, error) {
	row := model.MyReplyRow{Kind: myReplyKindAgent, Reply: model.MyReplyReply{Kind: "reply"}}
	var session, subject, body string
	var author []byte
	var repliedAt time.Time
	if err := scanned.Scan(append([]any{
		&row.ThreadID, &session, &repliedAt, &row.DismissedAt, &subject,
		&row.Reply.ID, &author, &body, &row.Reply.CreatedAt,
		&row.UnreadCount, &row.UnreadReplyIDs,
	}, extra...)...); err != nil {
		return model.MyReplyRow{}, fmt.Errorf("scan my agent thread: %w", err)
	}
	if err := json.Unmarshal(author, &row.Reply.Author); err != nil {
		return model.MyReplyRow{}, fmt.Errorf("decode my agent reply author: %w", err)
	}
	row.Ref = "/agents/" + session + "/live"
	row.Subject = refs.FirstLine(subject, serverURL)
	row.Reply.FirstLine = refs.FirstLine(body, serverURL)
	row.Unread = row.UnreadCount > 0
	row.Session = &model.MyReplySession{ID: session}
	return row, nil
}

// nameAgentSessions names the session of every Agent row on the page. A session's name is the
// newest title it stamped on anything it wrote (delivery.ResolveSessionTitles, the Delivery page's
// own read), which outlives the session; one it never stamped is named by its id.
func nameAgentSessions(ctx context.Context, tx pgx.Tx, rows []model.MyReplyRow) error {
	sessions := []string{}
	for _, row := range rows {
		if row.Session != nil {
			sessions = append(sessions, row.Session.ID)
		}
	}
	if len(sessions) == 0 {
		return nil
	}
	titles, err := delivery.ResolveSessionTitles(ctx, tx, sessions)
	if err != nil {
		return fmt.Errorf("name my agent threads' sessions: %w", err)
	}
	for index := range rows {
		if rows[index].Session != nil {
			rows[index].Session.Name = delivery.SessionTitle(titles, rows[index].Session.ID)
		}
	}
	return nil
}
