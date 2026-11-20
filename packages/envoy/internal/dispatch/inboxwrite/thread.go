package inboxwrite

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/failpoint"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// thread is the root of the thread a write is in, as the store addresses its node: an ask, a
// comment or a message. Its kind is the kind a user_threads row of the thread names.
type thread struct{ kind, id string }

// maxThreadDepth bounds the walk up a message thread, as the trigger user_threads_message bounds
// its walk up in_reply_to (0095): sixty-four steps, where production threads are seven deep at
// most.
const maxThreadDepth = 64

// threadOf is the root of the thread w is written in (decision 12), read from the write's head
// pointer tuples rather than from a projection's columns, as the triggers of 0095-0097 read it from
// the row. An ask is its own thread. A comment's thread is one step away, as user_threads_comment
// takes it (coalesce(ask_id, reply_to, id)): the ask its replies_to names, else the comment its
// replies_to names, which is the root of its thread (api's threadHeadOf stores every reply against
// its head), else the comment itself. comments.reply_to carries no acyclicity constraint, so the
// step is never repeated. A message's replies_to names the message it answers, and the walk climbs
// those to the root its in_reply_to chain reaches within maxThreadDepth steps, as
// user_threads_message walks it. A message on the way whose row names no node - an older build's,
// which states no tuple - is climbed by its in_reply_to, which is the old arm answering a row the
// marker test passes until the backfill gives it its tuples.
//
// A write that plans its record before its own write phase has run (PlanRecord) has queued its
// head pointer tuples and not written them, so no read finds them: it names the node they point
// at instead (Write.RepliesTo), which is read as that tuple would be - a comment's thread is that
// node, and a message's walk starts there, every step above it stored.
func threadOf(ctx context.Context, tx pgx.Tx, w Write) (thread, error) {
	root := thread{kind: w.NodeKind, id: w.NodeID}
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
	if err := tx.QueryRow(ctx, `
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
		)
		select id from walk order by depth desc limit 1
	`, start, graphstore.KindMessage, graphstore.TupleRepliesTo, maxThreadDepth, depth).Scan(&root.id); err != nil {
		return thread{}, fmt.Errorf("read the thread message %s is written in: %w", w.NodeID, err)
	}
	return root, nil
}

// ThreadRoot is the root of the thread the node kind/id is written in, as (kind, id), read as
// Record reads a write's (threadOf): the erase reads it of a reply it removes, whose thread's rows
// that reply reached (grapherase's readRepliedRows).
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
// no other column of a marked row is read.
func readMembers(ctx context.Context, tx pgx.Tx, thread thread) ([]member, error) {
	current := func(kind string) string {
		return graphstore.CurrentTupleRead(`t.from_id`, `t.from_id, t.asserted`,
			`t.to_kind = $2 and t.to_id = $3 and t.kind = `+kind+` and t.from_kind = $6`)
	}
	rows, err := tx.Query(ctx, `
		with held as (
			select u.login, u.marked_seq is not null as marked
			from user_threads u
			where u.thread_id = $1::uuid
		), taking_part as (
			select current.from_id as login from (`+current("$4")+`) current where current.asserted
		), standing_dismissals as (
			select current.from_id as login from (`+current("$5")+`) current where current.asserted
		)
		select login, bool_or(row_held), bool_or(marked), bool_or(takes_part), bool_or(was_dismissed)
		from (
			select login, true, marked, false, false from held
			union all
			select login, false, false, true, false from taking_part
			union all
			select login, false, false, false, true from standing_dismissals
		) as fact (login, row_held, marked, takes_part, was_dismissed)
		group by login
		order by login
	`, thread.id, thread.kind, thread.id, graphstore.TupleParticipatesIn, graphstore.TupleDismissed, graphstore.KindPerson)
	if err != nil {
		return nil, fmt.Errorf("read the members of thread %s/%s: %w", thread.kind, thread.id, err)
	}
	defer rows.Close()
	var members []member
	for rows.Next() {
		var held member
		if err := rows.Scan(&held.login, &held.row, &held.marked, &held.takesPart, &held.dismissed); err != nil {
			return nil, fmt.Errorf("read a member of thread %s/%s: %w", thread.kind, thread.id, err)
		}
		members = append(members, held)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the members of thread %s/%s: %w", thread.kind, thread.id, err)
	}
	return members, nil
}

// queueThreadLock is the write phase's lock of step 2: every user_threads row of the thread,
// `for no key update` in ascending login order in one pass, the one order every writer of a
// thread's rows and the backfill take them in, queued before the deliveries to the people holding
// them, so the backfill, which marks a row under the same lock, marks it above each of them (D4's
// lock rule), and the dismiss route, which takes the caller's rows first, ends no dismissal this
// write does not see.
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
// backfill takes; as the trigger's own insert, it touches neither version nor the dismissal. The
// conflict arm that moves no mark still locks the row, as the trigger's upsert did. The writer's
// node is written earlier in the write phase, with their participation (step 1).
func queueKeepRow(b *store.WriteBatch, thread thread, w Write, login string, event int64) error {
	mark, params, err := graphstore.MarkArm(8, "kept", graphstore.TupleReadThrough, thread.kind, thread.id, w.Who, event)
	if err != nil {
		return fmt.Errorf("record %s's read mark on %s/%s: %w", login, thread.kind, thread.id, err)
	}
	b.Queue(fmt.Sprintf("keep the row of %s on thread %s/%s", login, thread.kind, thread.id), `
		with kept as (
			insert into user_threads (login, thread_id, kind, project_key, issue_key, artifact_id, read_through, marked_version, marked_seq)
			values ($1, $2::uuid, $3, $4, $5, case when $5::text is null then $6::uuid end, $7, 0,
			        (select coalesce(max(seq), 0) from graph_tuples))
			on conflict (login, thread_id) do update
				set read_through = excluded.read_through
				where user_threads.read_through < excluded.read_through
			returning login, read_through as at
		),
		`+mark+`
		select 1
	`, append([]any{login, thread.id, thread.kind, w.Project, w.IssueKey, w.ArtifactID, w.At}, params...)...)
	return nil
}

// queueAgentDismissalsClear queues the clear of migration 20261105093507 for the Agent dismissals
// of root, the message a session's reply climbed to, as that trigger takes it: the root `for key
// share`, which orders the clear against the dismiss route's `for update` of the same root
// (inboxDismissLockAgentRoots, api/inbox_dismiss.go) whichever takes it first; then the
// retraction of the dismissed tuple of each person whose user_agent_thread_dismissed row names
// the root, read by the statement that retracts it (graphstore.MarkedIn); then the failure a test
// arms there (fail, failpoint.AgentDismissalClear); then the delete of every one of those rows.
// Both statements after the lock read the rows as they stand once it is held, nothing read in the
// write's read phase, so a dismissal the route committed before the reply's write phase - while
// the reply waited for the root among them - ends with the reply, as the trigger ends it; and no
// dismissal of the root can commit between the two, since the route marks one only under the
// root's `for update`.
func queueAgentDismissalsClear(b *store.WriteBatch, root string, fail bool, w Write, event int64) error {
	b.Queue(fmt.Sprintf("take the root message %s", root), `select 1 from messages where id = $1::uuid for key share`, root)
	dismissed := graphstore.MarkedIn(`select login from user_agent_thread_dismissed where message_id = $4::uuid`, root)
	if err := graphstore.QueueMarksEnded(b, graphstore.TupleDismissed, graphstore.KindMessage, root, dismissed, w.At, w.Who, event); err != nil {
		return fmt.Errorf("retract the dismissals of message %s: %w", root, err)
	}
	failpoint.Queue(b, fail)
	b.Queue(fmt.Sprintf("clear the dismissals of message %s", root),
		`delete from user_agent_thread_dismissed where message_id = $1::uuid`, root)
	return nil
}
