package inboxwrite

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// AnsweredBefore is an answer an older build wrote to ask askID: the answer the ask.answered event
// that recorded it carries, by the person it names at its own moment. The backfill reads it from
// that event and never from asks.answer (RecordOlder).
func AnsweredBefore(askID, user string, at time.Time) Write {
	who := model.Actor{Kind: "user", ID: user}
	return Write{Arm: Replies, NodeKind: graphstore.KindAsk, NodeID: askID, Author: who, Who: who, At: at}
}

// RecordOlder records w, a write an older build made during a roll, whose event is event
// (graphstore.NoEvent where the log holds none), as the tuples this build's writer would have
// appended for it (Record), in tx, the backfill's transaction (graphcensus.Backfill). That build's
// triggers kept the user_threads projection of the write and stated nothing in the store, so this
// appends the tuples alone and moves no row:
//
//   - every write: participates_in from its writer to its thread's root, dated at its own moment
//     (Record's step 1);
//   - an ask's create: the assignee arm, participates_in and delivered_to between the ask and the
//     person it waits on now, while it is open, with no delivery to its asker (waitingOnTuples);
//   - a reply - a comment, a message or an answer: one delivered_to from the write's node to each
//     person the old trigger raised the row of, rebuilt from history by that trigger's own rule
//     (reachedBefore), after the rows of those persons in the thread are locked `for no key
//     update` in ascending login order, the order every writer of a thread's rows takes them in,
//     so a row the backfill marks after this holds each such delivery at or below its marked_seq;
//     and the retraction of each dismissal the old trigger cleared by it: the dismissed tuple
//     into the root of each person it reached, and for a session's reply to a message every
//     person's (user_agent_thread_dismissed_clear), whose newest tuple asserts it at a moment
//     before the write's;
//   - a comment or an issue message whose text names identities (Write.Names), first: the
//     mentions tuple to each of them, as this release's write of the text states it - a session's
//     and a role's, which reach nobody by themselves, and a person's; then, for the persons, with
//     the deliveries above, one delivered_to to each of them but its writer whom the old trigger
//     raised no row of (R2's mention arm, Record's step 3), their rows of the thread locked in the
//     same ascending pass; and last the participates_in of each but its writer in the thread's
//     root (MentionParticipation, R4's mention arm). A person among them who holds a row of the
//     thread that is marked already, and whom the old trigger did not reach, began that row after
//     the write, through this release's writer: the row's mark counts no delivery the write made,
//     and one stated now would stand above it, so R6's equation would count it, and it takes
//     none. A write the log holds no event of states no participation of theirs: a participation
//     no mention explains under its own event is one the census holds to a row (graphcensus'
//     mentioned_alone), which a mention keeps none of.
//
// Every tuple carries event, the older write's own, so the repeat key graph_tuples_once keeps one
// copy beside the same tuple an answer's catch-up states first (owedBy). A declined ask takes
// part and delivers nothing (Declined).
//
// It reads the thread from the head pointer tuples its caller has just appended for the write,
// or, for a message on the way to its root whose row names no node, from that row (threadOf), and
// it reads no column of a row the marker test does not pass. It answers how many tuples it handed
// the store, the upper bound a backfill reports (a repeat under one event appends nothing). A
// reply's lock of the rows it reached and its read of the dismissals it ended hang on who it
// reached alone, so the two go to Postgres in one round trip (lockReachedAndReadCleared).
func RecordOlder(ctx context.Context, tx pgx.Tx, w Write, event int64) (int, error) {
	thread, err := threadOf(ctx, tx, w)
	if err != nil {
		return 0, err
	}
	authorKind, authorID, named := storeAuthor(w.Author)
	person := ""
	if named && authorKind == graphstore.KindPerson {
		person = authorID
	}
	stated := 0
	var people []graphstore.Tuple
	if named {
		taking := graphstore.Tuple{
			FromKind: authorKind, FromID: authorID, Kind: graphstore.TupleParticipatesIn,
			ToKind: thread.kind, ToID: thread.id,
			At: w.At, Who: w.Who, SourceEvent: event, Asserted: true,
		}
		if person != "" {
			people = append(people, taking)
		} else if err := graphstore.Write(ctx, tx, nil, []graphstore.Tuple{taking}); err != nil {
			return 0, fmt.Errorf("record that %s/%s took part in %s/%s: %w", authorKind, authorID, thread.kind, thread.id, err)
		} else {
			stated++
		}
	}
	if w.Arm == Opens {
		waiting, err := waitingOn(ctx, tx, w.NodeID)
		if err != nil {
			return 0, err
		}
		people = append(people, waitingOnTuples(w.NodeID, waiting, person, w.At, w.Who, event)...)
	}
	if w.Arm != Replies {
		if err := graphstore.ToPerson(ctx, tx, people); err != nil {
			return 0, fmt.Errorf("record who took part in %s/%s by %s/%s: %w", thread.kind, thread.id, w.NodeKind, w.NodeID, err)
		}
		return stated + len(people), nil
	}

	reached, err := reachedBefore(ctx, tx, thread, w.At, person)
	if err != nil {
		return 0, err
	}
	var mentioned []string
	for _, login := range w.mentioned {
		if login != person && !slices.Contains(reached, login) {
			mentioned = append(mentioned, login)
		}
	}
	everyone := w.NodeKind == graphstore.KindMessage && authorKind == graphstore.KindSession && thread.id != w.NodeID
	cleared, joined, err := lockReachedAndReadCleared(ctx, tx, thread, w.At, reached, mentioned, everyone)
	if err != nil {
		return 0, err
	}
	// What the write's text names is stated under its event and dated by it, as this release's
	// write of the text states it (graphstore.Cause); the deliveries and the participation below
	// carry the event alone.
	cause := graphstore.CauseOf(event)
	if len(w.identities) > 0 {
		identities := cause.Append(make([]graphstore.Tuple, 0, len(w.identities)), true, w.identities...)
		if err := graphstore.Write(ctx, tx, nil, identities); err != nil {
			return 0, fmt.Errorf("state the sessions and roles %s/%s names: %w", w.NodeKind, w.NodeID, err)
		}
		stated += len(identities)
	}
	if len(w.mentionedPeople) > 0 {
		people = append(cause.Append(make([]graphstore.Tuple, 0, len(w.mentionedPeople)+len(people)), true, w.mentionedPeople...), people...)
	}
	for _, login := range reached {
		people = append(people, deliveredTo(w.NodeKind, w.NodeID, login, w.At, w.Who, event))
	}
	for _, login := range mentioned {
		if !slices.Contains(joined, login) {
			people = append(people, deliveredTo(w.NodeKind, w.NodeID, login, w.At, w.Who, event))
		}
	}
	if event != graphstore.NoEvent {
		people = append(people, MentionParticipation(thread.kind, thread.id, graphstore.Uncaused{People: w.mentionedPeople}, w.Who, w.At, event)...)
	}
	if err := graphstore.ToPerson(ctx, tx, people); err != nil {
		return 0, fmt.Errorf("deliver %s/%s to who wrote in %s/%s before it: %w", w.NodeKind, w.NodeID, thread.kind, thread.id, err)
	}
	stated += len(people)
	if len(cleared) > 0 {
		if err := graphstore.FromPerson(ctx, tx, w.At, dismissalsEnded(cleared, thread, w, event)); err != nil {
			return 0, fmt.Errorf("retract the dismissals %s/%s cleared of %s/%s: %w", w.NodeKind, w.NodeID, thread.kind, thread.id, err)
		}
	}
	return stated + len(cleared), nil
}

// lockReachedAndReadCleared takes the user_threads rows of thread that reached and mentioned hold,
// and reads who a write at `at` into thread ended a dismissal of and which of mentioned hold a
// marked row of it, as statements sent as one pgx batch: they hang on reached and mentioned alone
// and not on each other, and a batch runs its statements in order, each in a snapshot of its own,
// so the reads still run once the lock is held.
//
// The lock takes the rows `for no key update` in ascending login order, before the deliveries to
// those persons are appended (the lock rule Record's steps 2 to 4 hold): the row the backfill
// marks later is then marked above each of those deliveries. A person who holds no row of the
// thread - one whose thread's owner resolves to no project - locks nothing. mentioned is the
// people the write's text names whom the old trigger did not reach (RecordOlder), whose rows the
// one pass takes beside the reached ones.
//
// The first read answers who the write ended a dismissal of, as the old triggers cleared it: each
// person of reached, or each person at all where everyone is set (a session's reply to a message,
// whose root's Agent dismissals user_agent_thread_dismissed_clear deletes), whose newest dismissed
// tuple into the thread's root asserts it at a moment before the write's. A dismissal stated at or
// after the write is a later act, which the write did not end. It reads the store alone, sorted,
// each login once. The second, made only where mentioned names anybody, answers each of them whose
// row of the thread is marked (marked_seq set, migration 20261116160000), sorted.
func lockReachedAndReadCleared(ctx context.Context, tx pgx.Tx, thread thread, at time.Time, reached, mentioned []string, everyone bool) (cleared, joined []string, err error) {
	held := slices.Concat(reached, mentioned)
	slices.Sort(held)
	held = slices.Compact(held)
	locks := len(held) > 0
	reads := locks || everyone
	if !reads {
		return nil, nil, nil
	}
	batch := &pgx.Batch{}
	if locks {
		batch.Queue(`
			select 1 from user_threads
			where thread_id = $1::uuid and login = any($2::text[])
			order by login
			for no key update
		`, thread.id, held)
	}
	batch.Queue(`
		select current.from_id from (
			`+graphstore.CurrentTupleRead(`t.from_id`, `t.from_id, t.asserted, t.at`,
		`t.to_kind = $1 and t.to_id = $2 and t.kind = $3 and t.from_kind = $4`)+`
		) current
		where current.asserted and current.at < $5 and ($6::boolean or current.from_id = any($7::text[]))
		order by 1
	`, thread.kind, thread.id, graphstore.TupleDismissed, graphstore.KindPerson, at, everyone, reached)
	if len(mentioned) > 0 {
		batch.Queue(`
			select login from user_threads
			where thread_id = $1::uuid and login = any($2::text[]) and marked_seq is not null
			order by login
		`, thread.id, mentioned)
	}
	results := tx.SendBatch(ctx, batch)
	defer results.Close()
	if locks {
		if _, err := results.Exec(); err != nil {
			return nil, nil, fmt.Errorf("lock the rows of thread %s/%s: %w", thread.kind, thread.id, err)
		}
	}
	rows, err := results.Query()
	if err != nil {
		return nil, nil, fmt.Errorf("read the dismissals of %s/%s a write ended: %w", thread.kind, thread.id, err)
	}
	if cleared, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return nil, nil, fmt.Errorf("read the dismissals of %s/%s a write ended: %w", thread.kind, thread.id, err)
	}
	if len(mentioned) > 0 {
		rows, err := results.Query()
		if err != nil {
			return nil, nil, fmt.Errorf("read the marked rows of %s/%s of the people a write names: %w", thread.kind, thread.id, err)
		}
		if joined, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return nil, nil, fmt.Errorf("read the marked rows of %s/%s of the people a write names: %w", thread.kind, thread.id, err)
		}
	}
	if err := results.Close(); err != nil {
		return nil, nil, fmt.Errorf("lock the rows of thread %s/%s and read the dismissals a write ended: %w", thread.kind, thread.id, err)
	}
	return cleared, joined, nil
}

// reachedBefore is who a reply into thread at `at` by writer reached, by the rule of the trigger
// that raised their rows for an older build's reply (user_threads_record, migration
// 20261105093501): every user_threads row of the thread but its writer's, which is the row of
// each person who had written in the thread before it, and never of one who joined after it. The
// thread's rows carry no moment of their own, so that set is rebuilt from history, from the
// store wherever it holds a write and from a row the marker test passes where it holds none yet:
//
//   - an ask: who asked it, through the authored_by of its node, or its own row while its marker
//     is null; each earlier answer, through its ask.answered event; and each comment on it,
//     through the authored_by of each comment whose replies_to names the ask, or a comments row
//     naming it whose tuples_stored is false;
//   - a comment's thread: its root's authored_by and the authored_by of each comment whose
//     replies_to names the root and none an ask, or a comments row of the thread whose
//     tuples_stored is false;
//   - a message's thread: the authored_by of each message whose replies_to chain reaches the
//     root, or a messages row on the way that names no node, read by its author and in_reply_to,
//     down at most MaxThreadDepth steps.
//
// Only a person writes a row, so the set is of persons, by their canonical login, writer left
// out. It reads no column of a marked row. The answer is sorted, each login once.
func reachedBefore(ctx context.Context, tx pgx.Tx, thread thread, at time.Time, writer string) ([]string, error) {
	// $1 the thread's root, $2 the write's moment, $3 its writer, $4 authored_by, $5 person and
	// $6 replies_to, then $7 and $8 of each kind's own.
	args := []any{thread.id, at, writer, graphstore.TupleAuthoredBy, graphstore.KindPerson, graphstore.TupleRepliesTo}
	var with, earlier string
	switch thread.kind {
	case graphstore.KindAsk:
		args = append(args, graphstore.KindAsk, graphstore.KindComment)
		earlier = `
			select t.to_id
			from graph_tuples t
			where t.from_kind = $7 and t.from_id = $1 and t.kind = $4 and t.to_kind = $5
			  and t.asserted and t.at < $2
			union all
			select user_threads_login(a.author->>'id')
			from asks a
			where a.id = $1::uuid and a.tuples_stored_at is null and a.author->>'kind' = 'user' and a.created_at < $2
			union all
			select written.to_id
			from graph_tuples reply
			join graph_tuples written
			  on written.from_kind = reply.from_kind and written.from_id = reply.from_id
			 and written.kind = $4 and written.to_kind = $5 and written.asserted
			where reply.to_kind = $7 and reply.to_id = $1 and reply.kind = $6 and reply.from_kind = $8
			  and reply.asserted and reply.at < $2
			union all
			select user_threads_login(c.author->>'id')
			from comments c
			where c.ask_id = $1::uuid and not c.tuples_stored and c.author->>'kind' = 'user' and c.created_at < $2
			union all
			select user_threads_login(e.payload->'answer'->>'user')
			from events e
			where e.type in (` + AskEventTypes + `) and e.type = 'ask.answered' and e.payload->>'id' = $1
			  and (e.payload->'answer'->>'at')::timestamptz < $2`
	case graphstore.KindComment:
		args = append(args, graphstore.KindComment, graphstore.KindAsk)
		earlier = `
			select t.to_id
			from graph_tuples t
			where t.from_kind = $7 and t.from_id = $1 and t.kind = $4 and t.to_kind = $5
			  and t.asserted and t.at < $2
			union all
			select written.to_id
			from graph_tuples reply
			join graph_tuples written
			  on written.from_kind = reply.from_kind and written.from_id = reply.from_id
			 and written.kind = $4 and written.to_kind = $5 and written.asserted
			where reply.to_kind = $7 and reply.to_id = $1 and reply.kind = $6 and reply.from_kind = $7
			  and reply.asserted and reply.at < $2
			  and not exists (
			      select 1 from graph_tuples asked
			      where asked.from_kind = reply.from_kind and asked.from_id = reply.from_id
			        and asked.kind = $6 and asked.to_kind = $8 and asked.asserted)
			union all
			select user_threads_login(c.author->>'id')
			from comments c
			where c.id = $1::uuid and c.ask_id is null and not c.tuples_stored
			  and c.author->>'kind' = 'user' and c.created_at < $2
			union all
			select user_threads_login(c.author->>'id')
			from comments c
			where c.reply_to::text = $1 and c.ask_id is null and not c.tuples_stored
			  and c.author->>'kind' = 'user' and c.created_at < $2`
	case graphstore.KindMessage:
		// Every read here is a probe by key, each made for one row of the walk. The walk reads each
		// unmarked child's row, its writer with it, through in_reply_to alone and tests the marker
		// on the rows it read, rather than as an index condition of its own: planned with no
		// statistics of node_kind, that test took the node_kind-is-null range of
		// messages_node_address, every message still unmarked, for each step of the walk. The
		// root's own row is read by its key, and the writer of each stored message of the walk by
		// that message's own node: a join of the walk against messages or the store was hashed,
		// reading every message, or every authored_by out of one, for each reply the backfill
		// walked. So no plan over messages here hangs on its statistics; the probes of the store
		// are planned on the store's, which the backfill takes again as it grows the store
		// (graphcensus's storeGrowth).
		args = append(args, graphstore.KindMessage, MaxThreadDepth)
		with = `
			with recursive tree (id, depth, login) as (
				select $1::text, 0, null::text
				union all
				select child.id, tree.depth + 1, child.login
				from tree
				cross join lateral (
					select t.from_id as id, null::text as login
					from graph_tuples t
					where t.to_kind = $7 and t.to_id = tree.id and t.kind = $6 and t.from_kind = $7 and t.asserted
					union
					select older.id, older.login
					from (
						select array_agg(m.id::text) filter (where m.node_kind is null) as ids,
						       array_agg(case when m.author->>'kind' = 'user' and m.created_at < $2
						                      then user_threads_login(m.author->>'id') end)
						         filter (where m.node_kind is null) as logins
						from messages m
						where m.in_reply_to::text = tree.id
					) unmarked
					cross join lateral unnest(unmarked.ids, unmarked.logins) as older (id, login)
				) child
				where tree.depth < $8
			)`
		earlier = `
			select stored.login
			from tree
			cross join lateral (
				select array_agg(written.to_id) as logins
				from graph_tuples written
				where written.from_kind = $7 and written.from_id = tree.id and written.kind = $4
				  and written.to_kind = $5 and written.asserted and written.at < $2
			) writers
			cross join lateral unnest(writers.logins) as stored (login)
			union all
			select tree.login from tree
			union all
			select user_threads_login(m.author->>'id')
			from messages m
			where m.id = $1::uuid and m.node_kind is null and m.author->>'kind' = 'user' and m.created_at < $2`
	default:
		return nil, fmt.Errorf("read who wrote in %s/%s: not a thread kind", thread.kind, thread.id)
	}
	rows, err := tx.Query(ctx, with+`
		select distinct earlier.login from (`+earlier+`
		) as earlier (login)
		where earlier.login is not null and earlier.login <> '' and earlier.login <> $3
		order by 1
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("read who wrote in %s/%s before %s: %w", thread.kind, thread.id, at, err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
