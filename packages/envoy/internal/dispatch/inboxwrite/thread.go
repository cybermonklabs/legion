package inboxwrite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/failpoint"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// thread is the root of the thread a write is in, as the store addresses its node: an ask, a
// comment or a message. Its kind is the kind a user_threads row of the thread names.
type thread struct{ kind, id string }

// MaxThreadDepth bounds the walk up a message thread, as the trigger user_threads_message bounds
// its walk up in_reply_to (0095): sixty-four steps, where production threads are seven deep at
// most.
const MaxThreadDepth = 64

// threadOf is the root of the thread w is written in, read from the write's head
// pointer tuples rather than from a projection's columns, as the triggers of 0095-0097 read it from
// the row. An ask is its own thread. A comment's thread is one step away, as user_threads_comment
// takes it (coalesce(ask_id, reply_to, id)): the ask its replies_to names, else the comment its
// replies_to names, which is the root of its thread (api's threadHeadOf stores every reply against
// its head), else the comment itself. comments.reply_to carries no acyclicity constraint, so the
// step is never repeated. A message's replies_to names the message it answers, and the walk climbs
// those to the root its in_reply_to chain reaches within MaxThreadDepth steps, as
// user_threads_message walks it. A message on the way whose row names no node - an older build's,
// which states no tuple - is climbed by its in_reply_to, which is the old arm answering a row the
// marker test passes until the backfill gives it its tuples.
//
// A write that plans its record before its own write phase has run (PlanRecord) has queued its
// head pointer tuples and not written them, so no read finds them: it names the node they point
// at instead (Write.RepliesTo), which is read as that tuple would be - a comment's thread is that
// node, and a message's walk starts there, every step above it stored - or says it answers
// nothing (Write.StartsThread), and is the root of its own thread with nothing read.
func threadOf(ctx context.Context, tx pgx.Tx, w Write) (thread, error) {
	root := thread{kind: w.NodeKind, id: w.NodeID}
	if w.root {
		return root, nil
	}
	switch w.NodeKind {
	case graphstore.KindAsk:
		return root, nil
	case graphstore.KindComment:
		if w.parent != nil {
			return *w.parent, nil
		}
		err := tx.QueryRow(ctx, `
			select t.to_kind, t.to_id
			from graph_tuples t
			where t.from_kind = $1 and t.from_id = $2 and t.kind = $3 and t.to_kind = any($4::text[]) and t.asserted
			order by t.to_kind = $5 desc, t.seq desc
			limit 1
		`, w.NodeKind, w.NodeID, graphstore.TupleRepliesTo, []string{graphstore.KindAsk, graphstore.KindComment},
			graphstore.KindAsk).Scan(&root.kind, &root.id)
		if errors.Is(err, pgx.ErrNoRows) {
			return root, nil
		}
		if err != nil {
			return thread{}, fmt.Errorf("read the thread comment %s is written in: %w", w.NodeID, err)
		}
		return root, nil
	}
	start, depth := w.NodeID, 0
	if w.parent != nil {
		start, depth = w.parent.id, 1
	}
	if err := tx.QueryRow(ctx, messageWalk+`
		select id from walk order by depth desc limit 1
	`, start, graphstore.KindMessage, graphstore.TupleRepliesTo, MaxThreadDepth, depth).Scan(&root.id); err != nil {
		return thread{}, fmt.Errorf("read the thread message %s is written in: %w", w.NodeID, err)
	}
	return root, nil
}

// messageWalk is threadOf's walk up a message's replies_to chain, a common-table expression walk
// of the steps from $1, at depth $5, over the message kind $2 and the relation $3, at most $4
// deep, whose deepest step is the root.
const messageWalk = `
		with recursive walk(id, depth) as (
			select $1::text, $5::int
			union all
			select up.id, walk.depth + 1
			from walk
			cross join lateral (
				select coalesce(stored.id, older.id) as id
				from (values (1)) as one (row)
				left join lateral (
					select t.to_id as id
					from graph_tuples t
					where t.from_kind = $2 and t.from_id = walk.id and t.kind = $3 and t.to_kind = $2 and t.asserted
					order by t.seq desc
					limit 1
				) stored on true
				left join lateral (
					select m.in_reply_to::text as id
					from messages m
					where m.id = walk.id::uuid and m.node_kind is null
				) older on true
			) up
			where walk.depth < $4 and up.id is not null
		)`

// threadAndMembers is threadOf and readMembers of w, a message's reply, in one statement: the walk
// up its replies_to chain (messageWalk) and the members of the root it reaches (membersColumn), read
// as each would read them on its own.
func threadAndMembers(ctx context.Context, tx pgx.Tx, w Write) (thread, []member, error) {
	root := thread{kind: graphstore.KindMessage, id: w.NodeID}
	start, depth := w.NodeID, 0
	if w.parent != nil {
		start, depth = w.parent.id, 1
	}
	var column []byte
	if err := tx.QueryRow(ctx, messageWalk+`,
		root as (select id from walk order by depth desc limit 1)
		select root.id, `+membersColumn(`'`+graphstore.KindMessage+`'`, `root.id`)+`
		from root
	`, start, graphstore.KindMessage, graphstore.TupleRepliesTo, MaxThreadDepth, depth).Scan(&root.id, &column); err != nil {
		return thread{}, nil, fmt.Errorf("read the thread message %s is written in and its members: %w", w.NodeID, err)
	}
	members, err := decodeMembers(column)
	if err != nil {
		return thread{}, nil, fmt.Errorf("read the members of thread %s/%s: %w", root.kind, root.id, err)
	}
	return root, members, nil
}

// ThreadRoot is the root of the thread the node kind/id is written in, as (kind, id), read as
// Record reads a write's (threadOf).
func ThreadRoot(ctx context.Context, tx pgx.Tx, kind, id string) (string, string, error) {
	root, err := threadOf(ctx, tx, Write{NodeKind: kind, NodeID: id})
	return root.kind, root.id, err
}

// member is one person a thread holds: a row of user_threads it read, a participant, or a
// person whose dismissal of it stands, or several of those.
type member struct {
	login string
	// row is whether the person holds a user_threads row of the thread, and marked whether that
	// row is marked (marked_seq, migration 20261116160000).
	row, marked bool
	// takesPart is whether a participates_in from the person into the thread stands.
	takesPart bool
	// dismissed is whether a dismissed tuple from the person into the thread stands.
	dismissed bool
}

// readMembers is step 2 of Record's plan, in one statement: every user_threads row of the thread,
// and, beside them, the people whose participates_in and whose dismissed into the thread's root
// stand now, each read the way CurrentIn reads a relation (CurrentTupleRead), so a retraction
// takes a person out. It locks nothing and runs in the write's read phase, under the owner lock
// every writer of a thread's rows and the backfill take first (store.LockOwner), so nothing that
// moves the thread's rows, their markers or its participation moves them between this read and
// the write phase; the rows themselves are taken `for no key update` by the write phase's first
// statement of the step (queueThreadLock), before the deliveries this read decides are appended,
// and the dismissals the write ends are read again where they are ended (graphstore.QueueMarksEnded),
// since the dismiss route takes no owner lock. The read takes every row of the thread, which holds
// the row of each person the write's catch-up delivers to (owedBy) as well as every participant's,
// since each of those is a row of the thread. A session or nobody keys no row, and neither does a
// thread whose owner resolves to no project, so such a thread holds no row and still answers its
// participants. A row's marker is read to tell a marked row from one the marker test passes, and
// no other column of a marked row is read. A write whose read phase reads its thread already may
// read its members in that statement instead (MembersColumn, Write.Members), and a message's
// reply reads them with its walk (threadAndMembers).
func readMembers(ctx context.Context, tx pgx.Tx, thread thread) ([]member, error) {
	var column []byte
	if err := tx.QueryRow(ctx, `select `+membersColumn(`$1::text`, `$2::text`), thread.kind, thread.id).Scan(&column); err != nil {
		return nil, fmt.Errorf("read the members of thread %s/%s: %w", thread.kind, thread.id, err)
	}
	members, err := decodeMembers(column)
	if err != nil {
		return nil, fmt.Errorf("read the members of thread %s/%s: %w", thread.kind, thread.id, err)
	}
	return members, nil
}

// MembersColumn is readMembers of the thread whose root is kind/id as one column of a select the
// caller makes already in the write's read phase, under the owner lock that read is made under:
// its SQL and its parameters, bound from $first on. The caller hands the column's value to the
// record it plans (Write.Members), which then reads no members of its own; kind/id must be the
// root that record's thread is (threadOf): a comment's head (Write.RepliesTo), an answer's ask.
func MembersColumn(first int, kind, id string) (string, []any) {
	return membersColumn(`$`+strconv.Itoa(first)+`::text`, `$`+strconv.Itoa(first+1)+`::text`), []any{kind, id}
}

// membersColumn is readMembers' read of the thread whose root is kind/id, each SQL text of the
// statement it is spliced into, as one scalar jsonb column: an array of the thread's members in
// login order (memberRow). The read binds the root's kind, so `t.to_kind <> 'value'`, always true
// of a thread's root, is written out: it is graph_tuples_as_of_in's predicate, which a generic plan
// of the statement - the plan pgx's statement cache reaches after five runs on one connection -
// cannot prove from a bound kind, and without it that plan walks graph_tuples_as_of_out across
// every person's participation of the kind (190 ms against 12.5 ms over the thread at the bound,
// on 2026-11-21).
func membersColumn(kind, id string) string {
	current := func(relation string) string {
		return graphstore.CurrentTupleRead(`t.from_id`, `t.from_id, t.asserted`,
			`t.to_kind = `+kind+` and t.to_id = `+id+` and t.kind = '`+relation+`' and t.from_kind = '`+graphstore.KindPerson+`'
				and t.to_kind <> '`+graphstore.KindValue+`'`)
	}
	return `(
		select coalesce(jsonb_agg(jsonb_build_object('login', held_member.login, 'row', held_member.row_held, 'marked', held_member.marked,
			'takes_part', held_member.takes_part, 'dismissed', held_member.was_dismissed) order by held_member.login), '[]'::jsonb)
		from (
			select login, bool_or(row_held) as row_held, bool_or(marked) as marked, bool_or(takes_part) as takes_part,
			       bool_or(was_dismissed) as was_dismissed
			from (
				select u.login, true, u.marked_seq is not null, false, false
				from user_threads u
				where u.thread_id = (` + id + `)::uuid
				union all
				select current.from_id, false, false, true, false
				from (` + current(graphstore.TupleParticipatesIn) + `) current where current.asserted
				union all
				select current.from_id, false, false, false, true
				from (` + current(graphstore.TupleDismissed) + `) current where current.asserted
			) as fact (login, row_held, marked, takes_part, was_dismissed)
			group by login
		) held_member
	)`
}

// memberRow is one member as membersColumn answers it.
type memberRow struct {
	Login     string `json:"login"`
	Row       bool   `json:"row"`
	Marked    bool   `json:"marked"`
	TakesPart bool   `json:"takes_part"`
	Dismissed bool   `json:"dismissed"`
}

// decodeMembers reads membersColumn's answer, in login order.
func decodeMembers(column []byte) ([]member, error) {
	var rows []memberRow
	if err := json.Unmarshal(column, &rows); err != nil {
		return nil, err
	}
	members := make([]member, 0, len(rows))
	for _, row := range rows {
		members = append(members, member{login: row.Login, row: row.Row, marked: row.Marked, takesPart: row.TakesPart, dismissed: row.Dismissed})
	}
	return members, nil
}

// queueThreadLock is the write phase's lock of step 2: every user_threads row of the thread,
// `for no key update` in ascending login order in one pass, the one order every writer of a
// thread's rows and the backfill take them in, queued before the deliveries to the people holding
// them, so the backfill, which marks a row under the same lock, marks it above each of them (the
// lock rule Record's steps 2 to 4 hold), and the dismiss route, which takes the caller's rows
// first, ends no dismissal this write does not see.
func queueThreadLock(b *store.WriteBatch, thread thread) {
	b.Queue(fmt.Sprintf("lock the rows of thread %s/%s", thread.kind, thread.id), `
		select 1 from user_threads u
		where u.thread_id = $1::uuid
		order by u.login
		for no key update
	`, thread.id)
}

// queueMoveRows is step 4 of Record over the rows logins name, every one of which the thread's
// lock took: a write by somebody else, so each row is replied to - replied_at and last_reply_id
// name the newer of the write and what the row named - its dismissal ends, and its version counts
// the write. The statement is user_threads_record's own (20261105093501), so a row moves alike
// whichever of the two writers moved it.
func queueMoveRows(b *store.WriteBatch, thread thread, w Write, logins []string) {
	if len(logins) == 0 {
		return
	}
	b.Queue(fmt.Sprintf("move the rows of thread %s/%s", thread.kind, thread.id), `
		update user_threads
		set replied_at = greatest(replied_at, $3),
		    last_reply_id = case when replied_at is null or $3 >= replied_at then $4::uuid else last_reply_id end,
		    dismissed_at = null,
		    version = version + 1
		where thread_id = $1::uuid and login = any($2::text[])
	`, thread.id, logins, w.At, w.NodeID)
}

// queueMoveUnmarked is the move of an Unstored write (Recording.queueUnstored): every row of the
// thread but writer's, "" for a writer who keeps no row, every one of which the thread's lock
// took, moved by user_threads_record's own update - `login is distinct from v_writer`, so every
// row, not only a participant's - and each given up its marker in the same statement, as that
// update did when the trigger body ran it (migration 20261116160000's depth test): no
// delivered_to stands beside its version's move, so the row is the backfill's thread pass's
// until that pass marks it again under the thread's lock.
func queueMoveUnmarked(b *store.WriteBatch, thread thread, w Write, writer string) {
	b.Queue(fmt.Sprintf("move the rows of thread %s/%s, unmarked", thread.kind, thread.id), `
		update user_threads
		set replied_at = greatest(replied_at, $3),
		    last_reply_id = case when replied_at is null or $3 >= replied_at then $4::uuid else last_reply_id end,
		    dismissed_at = null,
		    version = version + 1,
		    marked_version = null,
		    marked_seq = null
		where thread_id = $1::uuid and login is distinct from nullif($2::text, '')
	`, thread.id, writer, w.At, w.NodeID)
}

// queueKeepUnmarkedRow is the row of login, an Unstored write's writer, on the thread: the
// trigger body's upsert, its read mark moved to the write's moment where it stood earlier, and the
// row unmarked, as that upsert left a row it began and an update of the body's left one already
// there, since no read_through stands beside the mark it moved.
func queueKeepUnmarkedRow(b *store.WriteBatch, thread thread, w Write, login string) {
	b.Queue(fmt.Sprintf("keep the row of %s on thread %s/%s, unmarked", login, thread.kind, thread.id), `
		insert into user_threads (login, thread_id, kind, project_key, issue_key, artifact_id, read_through)
		values ($1, $2::uuid, $3, $4, $5, case when $5::text is null then $6::uuid end, $7)
		on conflict (login, thread_id) do update
			set read_through = greatest(user_threads.read_through, excluded.read_through),
			    marked_version = null,
			    marked_seq = null
	`, login, thread.id, thread.kind, w.Project, w.IssueKey, w.ArtifactID, w.At)
}

// queueKeepRow is step 5 of Record: the row of login, w's writer, on the thread, its read mark
// moved to the write's moment where it stood earlier, and, in the same statement, the
// read_through tuple that mark stands for (graphstore.MarkArm), stated where the upsert moved the
// mark - a row it began, or a mark that stood earlier - at the read mark the row stands at once
// the upsert ran: a write at or before the mark its writer's row holds leaves the row's mark, and
// its tuple, as they were. One statement reads the upsert's answer in SQL, so nothing is read
// back in Go between the two. A row this write begins is marked from its insert: marked_version
// 0, since no write by somebody else has reached it, and marked_seq the newest position of the log
// this statement sees, which is above every tuple an earlier write into the thread appended - each
// of those committed before this write took the owner lock - and below every one a later write
// appends. A row that was there keeps the marker it holds, or holds none, which is a row the
// backfill takes; as the trigger's own insert, it touches no version. The conflict arm that moves
// no mark still locks the row, as the trigger's upsert did. The writer's node is written earlier
// in the write phase, with their participation (step 1).
//
// Where dismiss holds - the write is its writer's own answer or reply (PlanRecord, R1) - the
// writer's dismissed tuple to the thread's root is queued first, at the write's moment
// (graphstore.QueueFromPerson), and the upsert sets dismissed_at to that moment on a row it begins
// and on one already there, whatever the row's read mark and version: the dismissal takes no
// version guard, unlike a Dismiss press (api's inboxDismissMarkThreads). Its conflict arm then
// always updates the row, so the read mark the row held is read beside it (`held_before`, the
// statement's own snapshot, which holds the row as the thread's lock took it, queueThreadLock) to
// state read_through only where the mark moved.
func queueKeepRow(b *store.WriteBatch, thread thread, w Write, login string, event int64, dismiss bool) error {
	mark, params, err := graphstore.MarkArm(8, "kept", graphstore.TupleReadThrough, thread.kind, w.Who, event)
	if err != nil {
		return fmt.Errorf("record %s's read mark on %s/%s: %w", login, thread.kind, thread.id, err)
	}
	args := append([]any{login, thread.id, thread.kind, w.Project, w.IssueKey, w.ArtifactID, w.At}, params...)
	if !dismiss {
		b.Queue(fmt.Sprintf("keep the row of %s on thread %s/%s", login, thread.kind, thread.id), `
			with kept as (
				insert into user_threads (login, thread_id, kind, project_key, issue_key, artifact_id, read_through, marked_version, marked_seq)
				values ($1, $2::uuid, $3, $4, $5, case when $5::text is null then $6::uuid end, $7, 0,
				        (select coalesce(max(seq), 0) from graph_tuples))
				on conflict (login, thread_id) do update
					set read_through = excluded.read_through
					where user_threads.read_through < excluded.read_through
				returning login, read_through as at, thread_id::text as to_id
			),
			`+mark+`
			select 1
		`, args...)
		return nil
	}
	graphstore.QueueFromPerson(b, w.At, []graphstore.Tuple{dismissalOf(login, thread, w, event)})
	b.Queue(fmt.Sprintf("keep and dismiss the row of %s on thread %s/%s", login, thread.kind, thread.id), `
		with held_before as (
			select read_through from user_threads where login = $1 and thread_id = $2::uuid
		), written as (
			insert into user_threads (login, thread_id, kind, project_key, issue_key, artifact_id, read_through, marked_version, marked_seq, dismissed_at)
			values ($1, $2::uuid, $3, $4, $5, case when $5::text is null then $6::uuid end, $7, 0,
			        (select coalesce(max(seq), 0) from graph_tuples), $7)
			on conflict (login, thread_id) do update
				set read_through = greatest(user_threads.read_through, excluded.read_through),
				    dismissed_at = excluded.dismissed_at
			returning login, read_through as at, thread_id::text as to_id
		), kept as (
			select written.login, written.at, written.to_id from written
			where not exists (select 1 from held_before) or (select held_before.read_through from held_before) < written.at
		),
		`+mark+`
		select 1
	`, args...)
	return nil
}

// queueAgentDismissal is step 6's row: login's user_agent_thread_dismissed row at root, the root
// message of the Agent thread w replies under, dismissed at w's moment and marked at that moment
// (tuples_stored_at, migration 20261116170000), as the dismissed tuple Record queues before it
// states. A row already there takes both moments whatever it held, with no guard, so the row
// always matches the newest tuple. The writer took root `for update` before its own row's insert
// (D6, api's createStoredMessage), so this upsert is ordered against the dismiss route, which
// takes the root before the person's rows (markInboxReadThreads, api/inbox_read.go).
func queueAgentDismissal(b *store.WriteBatch, login, root string, w Write) {
	b.Queue(fmt.Sprintf("dismiss the agent thread %s for %s", root, login), `
		insert into user_agent_thread_dismissed (login, message_id, dismissed_at, tuples_stored_at)
		values ($1, $2::uuid, $3, $3)
		on conflict (login, message_id) do update
			set dismissed_at = excluded.dismissed_at, tuples_stored_at = excluded.tuples_stored_at
	`, login, root, w.At)
}

// agentRepliesRead is the head of step 6's last statement, the rows its graphstore.MarkArm reads:
// the replies a person's own reply under an Agent thread marks read by id, as a Dismiss press of
// that thread marks them (api's dismissInboxAgentThreads, through markInboxReadAgentReplies), so
// the thread it takes out of their Inbox holds nothing unread, which the Inbox's own states and
// the Agents page's count rely on (api's myAgentUnreadRoots and unreadDirectRepliesCTE). They are
// each session's reply under the root $2, at any depth, that the read mark of $1, the person, on
// the root's session (their user_agent_read row) does not cover already - the dismiss route's own
// insert leaves those out, since the prune of a covered row would delete it again - inserted into
// user_agent_reply_read marked at the write's moment $3 and answered as (login, at, to_id). A reply
// already read by its id keeps its row (`on conflict do nothing`) and answers none, so no mark is
// stated twice. The rows are inserted in ascending reply id, the order that route sorts its ids
// in, so two writes lock new rows in one order. A root no session was the target of names no
// session and reads no row. The statement reads the thread once the root is held - its writer took
// it `for update` before its own row's insert (D6) - so the reply of a session that committed while
// the write waited there is read and marked too, and one that commits after it takes the root
// next and clears the dismissal (queueAgentDismissalsClear).
const agentRepliesRead = `
	with recursive thread as (
		select written.id, written.author, written.created_at from messages written where written.in_reply_to = $2::uuid
		union all
		select written.id, written.author, written.created_at from messages written join thread on written.in_reply_to = thread.id
	), replies_read as (
		insert into user_agent_reply_read (login, session_id, reply_id, tuples_stored_at)
		select $1, root.session_id, thread.id, $3
		from thread
		cross join (
			select substr(target, length('session:') + 1) as session_id from messages
			where id = $2::uuid and target like 'session:%'
		) root
		where thread.author->>'kind' = 'session'
		  and not exists (
			select 1 from user_agent_read
			where user_agent_read.login = $1 and user_agent_read.session_id = root.session_id
			  and thread.created_at <= user_agent_read.read_through
		  )
		order by thread.id
		on conflict do nothing
		returning login, tuples_stored_at as at, reply_id::text as to_id
	),`

// queueAgentDismissalsClear queues the clear of migration 20261105093507 for the Agent dismissals
// of root, the message a session's reply climbed to: the root `for no key update`
// (queueRootLock), which the reply's writer took already before its insert
// (Recording.QueueRootLock), so this one waits for nothing, and which orders the clear against
// the dismiss route's `for update` of the same root (inboxDismissLockAgentRoots,
// api/inbox_dismiss.go) and the restore's `for no key update` of it (inboxRestoreLockAgentRoots)
// whichever takes it first; then the retraction of the dismissed tuple of each person whose
// user_agent_thread_dismissed row names the root, read by the statement that retracts it
// (graphstore.MarkedIn); then the failure a test arms there (fail, failpoint.AgentDismissalClear);
// then the delete of every one of those rows, whose trigger clears the root's agent_dismissed_at
// (migration 20261125053125) under the lock the root already holds. Both statements after the
// lock read the rows as they stand once it is held, nothing read in the write's read phase, so a
// dismissal the route committed before the reply's write phase - while the reply waited for the
// root among them - ends with the reply, as the trigger ends it; and no dismissal of the root can
// commit between the two, since the route marks one only under the root's `for update`.
func queueAgentDismissalsClear(b *store.WriteBatch, root string, fail bool, w Write, event int64) {
	queueRootLock(b, root)
	dismissed := graphstore.MarkedIn(`select login from user_agent_thread_dismissed where message_id = $4::uuid`, root)
	graphstore.QueueMarksEnded(b, graphstore.TupleDismissed, graphstore.KindMessage, root, dismissed, w.At, w.Who, event)
	failpoint.Queue(b, fail)
	b.Queue(fmt.Sprintf("clear the dismissals of message %s", root),
		`delete from user_agent_thread_dismissed where message_id = $1::uuid`, root)
}

// queueRootLock queues the lock of root, the message a session's reply climbed to, `for no key
// update`: the level the trigger of migration 20261125053125 updates the root's row at when the
// reply's clear deletes a mark of it, taken before any mark so every writer of a root's
// agent_dismissed_at takes the root first (the lock order written on api/inbox_read.go's
// markInboxReadThreads).
func queueRootLock(b *store.WriteBatch, root string) {
	b.Queue(fmt.Sprintf("take the root message %s", root), `select 1 from messages where id = $1::uuid for no key update`, root)
}
