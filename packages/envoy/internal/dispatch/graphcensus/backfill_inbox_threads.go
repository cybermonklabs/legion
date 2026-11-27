package graphcensus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/inboxwrite"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// --- what an ask's create and its older answers owe -----------------------------------------------

// asksOwed is the asks rows the ask pass owes, by the marker test of asks.tuples_stored_at
// (migration 20261116140001), the one the trigger user_threads_ask and the answer writers read:
// an ask an older build created, whose marker is null, and an ask holding an answer an older build
// wrote, whose moment the marker does not hold; each of an ask whose node the store holds
// (the ask passes, which run first).
var asksOwed = `
	from asks k
	where (k.tuples_stored_at is null
	       or (k.answer->>'user' is not null and k.tuples_stored_at is distinct from (k.answer->>'at')::timestamptz))
	  and exists (select 1 from graph_nodes n where n.kind = '` + graphstore.KindAsk + `' and n.id = k.id::text)`

// asksOwedPass states what each ask the marker test passes owes the store, as an answer's catch-up
// states it (inboxwrite's owedBy) and as the create and the answer writers of this release state
// their own (inboxwrite.Record): for an ask an older build created, under its ask.opened and dated
// at its created_at, its asker's participates_in and, while it is open, the participates_in and
// the delivered_to of the person it waits on now, and no delivery to its asker; and for each
// answer an older build wrote to it after the moment the marker holds - every answer, where the
// marker is null - under that answer's own ask.answered and at its own moment, read from that
// event and never from asks.answer, its answerer's participates_in and the delivered_to it owed
// (inboxwrite.RecordOlder). It then sets the marker to the moment of the answer the ask holds, or
// to its created_at where it holds none, which is what this release's own answer and create set
// it to. The ask's citations are its refs rows', which the citation pass states.
func asksOwedPass() inboxPass {
	return inboxPass{
		kind:  "ask_tuples",
		batch: `select k.id::text ` + asksOwed + ` and k.id > $1::uuid order by k.id limit $2`,
		start: []string{zeroUUID},
		write: func(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
			return writeAsksOwed(ctx, tx, column(keys, 0))
		},
		remaining: asksOwed,
	}
}

// writeAsksOwed takes the owner rows of ids, then the row of each project an ask of ids falls
// through to for its waiting person (inboxwrite.HoldDefault), then reads again and locks the asks
// rows of ids still owed, `for no key update` in ascending id as an answer's update takes one, and
// states them: the order every writer of an ask takes those rows in.
func writeAsksOwed(ctx context.Context, tx pgx.Tx, ids []string) (inboxWrote, error) {
	if err := lockOwnersOf(ctx, tx, askOwners, ids); err != nil {
		return inboxWrote{}, err
	}
	rows, err := tx.Query(ctx, `
		select distinct k.issue_key, k.artifact_id::text from asks k where k.id = any($1::uuid[])
		order by 1, 2
	`, ids)
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read the owners of a batch of asks: %w", err)
	}
	type owner struct{ issue, artifact *string }
	var owners []owner
	var issue, artifact *string
	if _, err := pgx.ForEachRow(rows, []any{&issue, &artifact}, func() error {
		owners = append(owners, owner{issue, artifact})
		return nil
	}); err != nil {
		return inboxWrote{}, fmt.Errorf("read the owners of a batch of asks: %w", err)
	}
	for _, held := range owners {
		if err := inboxwrite.HoldDefault(ctx, tx, held.issue, held.artifact); err != nil {
			return inboxWrote{}, err
		}
	}
	rows, err = tx.Query(ctx, `
		select k.id::text, k.author, k.created_at, k.issue_key, k.artifact_id::text, k.tuples_stored_at,
		       `+askOpenedOf("k")+`
		`+asksOwed+` and k.id = any($1::uuid[])
		order by k.id
		for no key update of k
	`, ids)
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read the asks whose tuples are owed: %w", err)
	}
	type ask struct {
		id                   string
		author               model.Actor
		at                   time.Time
		issueKey, artifactID *string
		marker               *time.Time
		opened               int64
	}
	var read []ask
	for rows.Next() {
		var held ask
		var author []byte
		if err := rows.Scan(&held.id, &author, &held.at, &held.issueKey, &held.artifactID, &held.marker, &held.opened); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read an ask whose tuples are owed: %w", err)
		}
		if err := json.Unmarshal(author, &held.author); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read the author of ask %s: %w", held.id, err)
		}
		read = append(read, held)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return inboxWrote{}, fmt.Errorf("read the asks whose tuples are owed: %w", err)
	}
	if len(read) == 0 {
		return inboxWrote{}, nil
	}
	// Every older answer of the batch, read in one statement before any is stated: the writes
	// below append no event, so a read per ask between them would answer the same rows. The ids
	// are named to events_ask_payload_id as a list too, so the index reads the batch's asks alone
	// whichever way the join is planned.
	askIDs, markers := make([]string, len(read)), make([]*time.Time, len(read))
	for index, held := range read {
		askIDs[index], markers[index] = held.id, held.marker
	}
	answers, err := tx.Query(ctx, `
		select named.id, e.id, coalesce(e.payload->'answer'->>'user', ''), (e.payload->'answer'->>'at')::timestamptz
		from unnest($1::text[], $2::timestamptz[]) as named (id, marker)
		join events e on e.type in (`+inboxwrite.AskEventTypes+`) and e.type = 'ask.answered' and e.payload->>'id' = named.id
		where e.payload->>'id' = any($1::text[])
		  and e.payload->'answer'->>'user' is not null and e.payload->'answer'->>'at' is not null
		  and (named.marker is null or (e.payload->'answer'->>'at')::timestamptz > named.marker)
		order by named.id, e.id
	`, askIDs, markers)
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read the older answers of a batch of asks: %w", err)
	}
	type answer struct {
		event int64
		user  string
		at    time.Time
	}
	older := map[string][]answer{}
	var askID string
	var each answer
	if _, err := pgx.ForEachRow(answers, []any{&askID, &each.event, &each.user, &each.at}, func() error {
		older[askID] = append(older[askID], each)
		return nil
	}); err != nil {
		return inboxWrote{}, fmt.Errorf("read the older answers of a batch of asks: %w", err)
	}
	var wrote inboxWrote
	marked := make([]string, 0, len(read))
	for _, held := range read {
		if held.marker == nil {
			owed, err := inboxwrite.RecordOlder(ctx, tx,
				inboxwrite.Opened(held.id, held.author, held.at, held.issueKey, held.artifactID, ""), held.opened)
			if err != nil {
				return inboxWrote{}, err
			}
			wrote.tuples += int64(owed)
		}
		for _, answered := range older[held.id] {
			if answered.user == "" {
				continue
			}
			owed, err := inboxwrite.RecordOlder(ctx, tx, inboxwrite.AnsweredBefore(held.id, answered.user, answered.at), answered.event)
			if err != nil {
				return inboxWrote{}, err
			}
			wrote.tuples += int64(owed)
		}
		marked = append(marked, held.id)
	}
	if _, err := tx.Exec(ctx, `
		update asks set tuples_stored_at = coalesce((answer->>'at')::timestamptz, tuples_stored_at, created_at)
		where id = any($1::uuid[])
	`, marked); err != nil {
		return inboxWrote{}, fmt.Errorf("mark a batch of asks' tuples stored: %w", err)
	}
	wrote.rows = int64(len(marked))
	return wrote, nil
}

// --- what a decline owes ------------------------------------------------------------------------

// declinesOwed is the ask.resolved events of a decline (model.AskResolutionDeclined) the decline
// pass owes: each whose decliner - the event's actor, a person or a session the store can key, as
// graphstore.AuthorOf reads it and the writer bounds it (actorAuthor) - has no participates_in
// into the ask under that event. A decline holds no marker of its own, so its marker is the tuple
// its writer states under its event (inboxwrite.Declined), which a decline this release wrote
// holds and an older build's does not.
var declinesOwed = `
	from events e
	where e.type in (` + inboxwrite.AskEventTypes + `) and e.type = 'ask.resolved'
	  and e.payload->'resolution'->>'kind' = '` + model.AskResolutionDeclined + `'
	  and coalesce(e.payload->>'id', '') <> ''
	  and ` + graphstore.AddressFitsSQL(`(`+actorAuthor(`e.actor`)+`)[2]`) + `
	  and not exists (
	      select 1 from graph_tuples t
	      where t.to_kind = '` + graphstore.KindAsk + `' and t.to_id = e.payload->>'id'
	        and t.kind = '` + graphstore.TupleParticipatesIn + `' and t.source_event = e.id)`

// declinesKind is the decline pass's kind, the one pass of the inbox the block-index tick runs over
// the events it reads rather than over rows (rollPasses, CatchUpEvents).
const declinesKind = "ask_declines"

// declinesPass gives the decliner of each decline an older build wrote their participates_in into
// the ask, under that decline's ask.resolved and at the resolution's own moment, read from the
// event and never from asks.resolution (inboxwrite.RecordOlder of inboxwrite.Declined).
func declinesPass() inboxPass {
	return inboxPass{
		kind:      declinesKind,
		batch:     `select e.id::text ` + declinesOwed + ` and e.id > $1::text::bigint order by e.id limit $2`,
		start:     []string{"0"},
		write:     writeDeclines,
		remaining: declinesOwed,
	}
}

func writeDeclines(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
	rows, err := tx.Query(ctx, `
		select e.id, e.payload->>'id', e.actor,
		       coalesce((e.payload->'resolution'->>'at')::timestamptz, e.created_at)
		`+declinesOwed+` and e.id = any($1::text[]::bigint[])
		order by e.id
	`, column(keys, 0))
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read the declines whose participation is owed: %w", err)
	}
	type decline struct {
		event int64
		ask   string
		who   model.Actor
		at    time.Time
	}
	var read []decline
	for rows.Next() {
		var held decline
		var actor []byte
		if err := rows.Scan(&held.event, &held.ask, &actor, &held.at); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read a decline whose participation is owed: %w", err)
		}
		if err := json.Unmarshal(actor, &held.who); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read the decliner of event %d: %w", held.event, err)
		}
		read = append(read, held)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return inboxWrote{}, fmt.Errorf("read the declines whose participation is owed: %w", err)
	}
	var wrote inboxWrote
	for _, held := range read {
		owed, err := inboxwrite.RecordOlder(ctx, tx, inboxwrite.Declined(held.ask, held.who, held.at), held.event)
		if err != nil {
			return inboxWrote{}, err
		}
		wrote.rows++
		wrote.tuples += int64(owed)
	}
	return wrote, nil
}

// --- the user_threads rows ----------------------------------------------------------------------

// userThreadsOwed is the user_threads rows the thread pass owes: each whose marked_seq is null
// (migration 20261116160000), whose person the store can key, of a thread whose root the store
// holds a node of. A thread whose root is left out of the store on purpose - a comment past the
// store's address bound - has no node its participation and marks could point at, as a block of an
// item the store holds no node of has none (refs' ItemScope), so its rows are no rows of this pass,
// as that comment is no row the comment passes owe.
var userThreadsOwed = `
	from user_threads u
	where u.marked_seq is null and octet_length(u.login) <= ` + addressBound + `
	  and exists (select 1 from graph_nodes root where root.kind = u.kind and root.id = u.thread_id::text)`

// userThreadsPass states what each user_threads row an older build's trigger wrote says, and marks
// the row: participates_in from its person to the thread's root, where no write the store holds
// of theirs there states it already (inboxwrite.RecordOlder states it for each write an older
// build made), dated at the read mark the row stands at, which is no earlier than the person's own
// last write there; read_through at read_through; dismissed at dismissed_at where the row holds
// one; and one delivered_to from last_reply_id's node to the person at replied_at, under that
// reply's own event, which the pass over that reply states too, so the repeat key keeps one; then,
// under the row's lock, marked_version = version and marked_seq at the log's position, above each
// tuple of the row.
//
// A row this release marked comes back to the pass when an older build updates it (migration
// 20261116160000's trigger nulls its marker), and of the marks the row stood for, only a dismissal
// can be gone from it while its tuple stands: the row always says its person takes part and reads
// through a moment, but an older build's "Move to Inbox" sets dismissed_at to null and states no
// retraction (inbox_dismiss.go's inboxRestoreThreads at 99e9da45abf1). So where the person's
// dismissed tuple into the root stands and the row holds no dismissed_at, the pass retracts it, at
// the moment it reads the row, the first the store learns the dismissal ended; a dismissal an
// older build's reply ended is retracted under that reply's event by the catch-up before the row
// is read (inboxwrite.RecordOlder's lockReachedAndReadCleared), so it no longer stands here. The
// read of the person's tuples that decides this, as the one of their participates_in beside it,
// is made for the rows the marker test passes alone - an older build's write, under the row's lock
// - and never for a marked row, so it restates what such a row says and mends nothing this
// release's writer stated.
//
// It takes a row only once every item of its thread its catch-up states holds its node
// (threadItemsStored): a comment an older build writes while the run is under way, after the
// comment pass read the table, holds none until a later run or the block-index tick gives it one,
// and a row of its thread marked meanwhile would stand at a version counting that comment's write
// while the comment's deliveries, stated with its node, land above the mark. Such a row stays owed
// (remaining counts it) until its thread's items all hold their nodes.
func userThreadsPass() inboxPass {
	return inboxPass{
		kind: "user_threads",
		batch: `select u.thread_id::text, u.login ` + userThreadsOwed + ` and ` + threadItemsStored + `
			and (u.thread_id, u.login) > ($1::uuid, $2) order by u.thread_id, u.login limit $3`,
		start:     []string{zeroUUID, ""},
		write:     writeUserThreads,
		remaining: userThreadsOwed,
	}
}

// threadOwners is the owner the user_threads rows of the threads $1 stand under, as (thread,
// issue, document) for lockOwnersOf, as commentOwners reads a comment's: the issue a row names,
// else the issue of the document it names, else that document. A comment thread on an issue's
// document names the document alone (user_threads_record takes the comment's own issue_key, which
// such a comment leaves null), and every writer of that thread takes the document's issue's row.
const threadOwners = `
	select u.thread_id::text, coalesce(u.issue_key, a.issue_key, ''),
	       case when coalesce(u.issue_key, a.issue_key) is null then coalesce(u.artifact_id::text, '') else '' end
	from user_threads u left join artifacts a on a.id = u.artifact_id
	where u.thread_id = any($1::uuid[])`

// threadCommentsOwed is the catch-up's read of the comments of the threads $1 the pointer pass
// still owes. A comment's thread is coalesce(ask_id, reply_to, id) (user_threads_comment), which no
// index answers, so the read names each of the three columns through its own index - the key,
// comments_reply_to and comments_ask_id - and keeps the coalesce to drop a comment one of them
// names whose thread is another.
var threadCommentsOwed = `select c.id::text ` + commentPointersOwed + `
	  and (c.id = any($1::uuid[]) or c.reply_to = any($1::uuid[]) or c.ask_id = any($1::uuid[]))
	  and coalesce(c.ask_id, c.reply_to, c.id) = any($1::uuid[])`

// writeUserThreads takes the owner rows of the batch's threads first (threadOwners), under which
// no write into them runs, and then catches each thread up: every comment, message and ask of
// those threads the marker test still passes - one an older build wrote after the passes above
// read the thread - is stated, with what it owed, before any row of the thread is marked, so each
// delivery it owed lands at or below the mark (writeCommentPointers, writeMessages,
// writeAsksOwed). It then reads which of the batch's rows stand in a thread each of whose items
// holds its node (threadItemsStored), locks those of them `for no key update`, in ascending login
// within each thread, and states and marks them. The test is a statement of its own, read plainly
// before the lock: it reads the documents of the thread's comments (refs.CommentStoreJoins), and a
// statement that locks a row names no owner table (api's TestNoForUpdateOnOwnerTables). What it
// reads - the thread's comments and messages, their nodes and their documents' addresses - only a
// writer of the thread's owner moves, which waits on the owner locks this batch holds from its
// first statement, so it answers under the lock what it answered before it. The store's tuples it
// reads of a row's person - whether their participates_in and their dismissed into the thread
// stand - it reads of the rows the marker test passes alone (userThreadsOwed, read again under the
// lock), to state what each such row says, a dismissal it no longer holds retracted; it reads none
// of a marked row.
func writeUserThreads(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
	threads, logins := column(keys, 0), column(keys, 1)
	if err := lockOwnersOf(ctx, tx, threadOwners, threads); err != nil {
		return inboxWrote{}, err
	}
	var wrote inboxWrote
	for _, catchUp := range []struct {
		read  string
		write func(context.Context, pgx.Tx, []string) (inboxWrote, error)
	}{
		{`select k.id::text ` + asksOwed + ` and k.id = any($1::uuid[])`, writeAsksOwed},
		{`select m.id::text ` + messagesOwed + ` and m.issue_key in (
		      select u.issue_key from user_threads u where u.thread_id = any($1::uuid[]) and u.kind = '` + graphstore.KindMessage + `')`, writeMessages},
		{threadCommentsOwed, writeOwnedCommentPointers},
	} {
		rows, err := tx.Query(ctx, catchUp.read, threads)
		if err != nil {
			return inboxWrote{}, fmt.Errorf("read what the threads of a batch still owe: %w", err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return inboxWrote{}, fmt.Errorf("read what the threads of a batch still owe: %w", err)
		}
		if len(ids) == 0 {
			continue
		}
		caught, err := catchUp.write(ctx, tx, ids)
		if err != nil {
			return inboxWrote{}, err
		}
		wrote.tuples += caught.tuples
	}
	stored, err := readKeys(ctx, tx, 2, `
		select u.thread_id::text, u.login from user_threads u
		where (u.thread_id, u.login) in (select * from unnest($1::uuid[], $2::text[])) and `+threadItemsStored,
		threads, logins)
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read which threads of a batch hold every item's node: %w", err)
	}
	if len(stored) == 0 {
		return wrote, nil
	}

	rows, err := tx.Query(ctx, `
		select u.login, u.thread_id::text, u.kind, u.read_through, u.dismissed_at, u.replied_at, u.last_reply_id::text,
		       case
		           when u.last_reply_id is null then null
		           when u.kind = '`+graphstore.KindMessage+`' then (select `+messageEventOf("m")+` from messages m where m.id = u.last_reply_id)
		           when u.kind = '`+graphstore.KindAsk+`' and u.last_reply_id = u.thread_id then coalesce((
		               select e.id from events e
		               where e.type in (`+inboxwrite.AskEventTypes+`) and e.type = 'ask.answered' and e.payload->>'id' = u.thread_id::text
		               order by (e.payload->'answer'->>'at')::timestamptz = u.replied_at desc, e.id desc
		               limit 1), `+noEvent+`)
		           else (select `+commentEventOf("c")+` from comments c where c.id = u.last_reply_id)
		       end,
		       exists (
		           select 1 from (`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.asserted`,
		`t.from_kind = '`+graphstore.KindPerson+`' and t.from_id = u.login and t.kind = '`+graphstore.TupleParticipatesIn+`'
		                and t.to_kind = u.kind and t.to_id = u.thread_id::text`)+`) taking
		           where taking.asserted),
		       exists (
		           select 1 from (`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.asserted`,
		`t.from_kind = '`+graphstore.KindPerson+`' and t.from_id = u.login and t.kind = '`+graphstore.TupleDismissed+`'
		                and t.to_kind = u.kind and t.to_id = u.thread_id::text`)+`) dismissal
		           where dismissal.asserted)
		`+userThreadsOwed+`
		  and (u.thread_id, u.login) in (select * from unnest($1::uuid[], $2::text[]))
		order by u.thread_id, u.login
		for no key update of u
	`, column(stored, 0), column(stored, 1))
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read the user_threads rows naming no mark: %w", err)
	}
	var reached, marks, restored []graphstore.Tuple
	var markedThreads, markedLogins []string
	for rows.Next() {
		var login, thread, kind string
		var readThrough time.Time
		var dismissed, replied *time.Time
		var lastReply *string
		var replyEvent *int64
		var takesPart, dismissalStands bool
		if err := rows.Scan(&login, &thread, &kind, &readThrough, &dismissed, &replied, &lastReply, &replyEvent, &takesPart, &dismissalStands); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read a user_threads row naming no mark: %w", err)
		}
		person := model.Actor{Kind: "user", ID: login}
		if !takesPart {
			reached = append(reached, graphstore.Tuple{
				FromKind: graphstore.KindPerson, FromID: login, Kind: graphstore.TupleParticipatesIn,
				ToKind: kind, ToID: thread,
				At: readThrough, Who: person, SourceEvent: graphstore.NoEvent, Asserted: true,
			})
		}
		if lastReply != nil && replied != nil && replyEvent != nil {
			replyKind := graphstore.KindComment
			switch {
			case kind == graphstore.KindMessage:
				replyKind = graphstore.KindMessage
			case kind == graphstore.KindAsk && *lastReply == thread:
				replyKind = graphstore.KindAsk
			}
			reached = append(reached, graphstore.Tuple{
				FromKind: replyKind, FromID: *lastReply, Kind: graphstore.TupleDeliveredTo,
				ToKind: graphstore.KindPerson, ToID: login,
				At: *replied, Who: backfillActor, SourceEvent: *replyEvent, Asserted: true,
			})
		}
		marks = append(marks, graphstore.Tuple{
			FromKind: graphstore.KindPerson, FromID: login, Kind: graphstore.TupleReadThrough,
			ToKind: kind, ToID: thread,
			At: readThrough, Who: person, SourceEvent: graphstore.NoEvent, Asserted: true,
		})
		if dismissed != nil {
			marks = append(marks, graphstore.Tuple{
				FromKind: graphstore.KindPerson, FromID: login, Kind: graphstore.TupleDismissed,
				ToKind: kind, ToID: thread,
				At: *dismissed, Who: person, SourceEvent: graphstore.NoEvent, Asserted: true,
			})
		} else if dismissalStands {
			restored = append(restored, graphstore.Tuple{
				FromKind: graphstore.KindPerson, FromID: login, Kind: graphstore.TupleDismissed,
				ToKind: kind, ToID: thread,
				Who: person, SourceEvent: graphstore.NoEvent, Asserted: false,
			})
		}
		markedThreads, markedLogins = append(markedThreads, thread), append(markedLogins, login)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return inboxWrote{}, fmt.Errorf("read the user_threads rows naming no mark: %w", err)
	}
	if len(markedThreads) == 0 {
		return wrote, nil
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `select now()`).Scan(&now); err != nil {
		return inboxWrote{}, fmt.Errorf("read the time of a batch of user_threads rows: %w", err)
	}
	for i := range restored {
		restored[i].At = now
	}
	marks = append(marks, restored...)
	if err := graphstore.ToPerson(ctx, tx, reached); err != nil {
		return inboxWrote{}, fmt.Errorf("state who a batch of user_threads rows names: %w", err)
	}
	if err := graphstore.FromPerson(ctx, tx, now, marks); err != nil {
		return inboxWrote{}, fmt.Errorf("state the marks of a batch of user_threads rows: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update user_threads u
		set marked_version = u.version, marked_seq = (select coalesce(max(seq), 0) from graph_tuples)
		from unnest($1::uuid[], $2::text[]) as named (thread, login)
		where u.thread_id = named.thread and u.login = named.login and u.marked_seq is null
	`, markedThreads, markedLogins); err != nil {
		return inboxWrote{}, fmt.Errorf("mark a batch of user_threads rows: %w", err)
	}
	wrote.rows += int64(len(markedThreads))
	wrote.tuples += int64(len(reached) + len(marks))
	return wrote, nil
}

// --- the other five read-state tables -----------------------------------------------------------

// readStateTable is one of the five read-state tables other than user_threads (migration
// 20261116170000): its name, its key columns as text in key order with the keyset over them, the
// mark it holds as a tuple from its person - the tuple's kind, the far end's kind and the column
// naming it, and the moment the tuple carries - and the moment its marker records, which is that
// tuple's own (api/read_marks.go).
type readStateTable struct {
	table, keys, keyset, named string
	start                      []string
	kind, toKind, toID, moment string
}

var readStateTables = []readStateTable{
	{table: "user_agent_read", keys: `r.login, r.session_id`, keyset: `(r.login, r.session_id) > ($1, $2)`,
		named: `(r.login, r.session_id) in (select * from unnest($1::text[], $2::text[]))`, start: []string{"", ""},
		kind: graphstore.TupleReadThrough, toKind: graphstore.KindSession, toID: `r.session_id`, moment: `r.read_through`},
	{table: "user_agent_state", keys: `r.login, r.session_id`, keyset: `(r.login, r.session_id) > ($1, $2)`,
		named: `(r.login, r.session_id) in (select * from unnest($1::text[], $2::text[]))`, start: []string{"", ""},
		kind: graphstore.TupleCleared, toKind: graphstore.KindSession, toID: `r.session_id`, moment: `r.cleared_before`},
	// A reply read by its id stores no moment of its own, so its tuple and its marker carry the
	// moment the backfill reads it at, as the route's carry the moment of the reading.
	{table: "user_agent_reply_read", keys: `r.login, r.reply_id::text`, keyset: `(r.login, r.reply_id) > ($1, $2::uuid)`,
		named: `(r.login, r.reply_id) in (select * from unnest($1::text[], $2::uuid[]))`, start: []string{"", zeroUUID},
		kind: graphstore.TupleReplyRead, toKind: graphstore.KindMessage, toID: `r.reply_id::text`, moment: `now()`},
	{table: "user_agent_thread_dismissed", keys: `r.message_id::text, r.login`, keyset: `(r.message_id, r.login) > ($1::uuid, $2)`,
		named: `(r.message_id, r.login) in (select * from unnest($1::uuid[], $2::text[]))`, start: []string{zeroUUID, ""},
		kind: graphstore.TupleDismissed, toKind: graphstore.KindMessage, toID: `r.message_id::text`, moment: `r.dismissed_at`},
	// A snooze's tuple carries its snoozed_until.
	{table: "user_ask_snooze", keys: `r.login, r.ask_id::text`, keyset: `(r.login, r.ask_id) > ($1, $2::uuid)`,
		named: `(r.login, r.ask_id) in (select * from unnest($1::text[], $2::uuid[]))`, start: []string{"", zeroUUID},
		kind: graphstore.TupleSnoozed, toKind: graphstore.KindAsk, toID: `r.ask_id::text`, moment: `r.snoozed_until`},
}

// owed is the table's rows the read-state pass owes: each whose tuples_stored_at is null, whose
// person and far end the store can key (api's markFits); the person is the row's login
// canonicalised (model.CanonicalLogin, user_threads_login), which user_agent_state keys on the raw
// actor id it was written by.
func (table readStateTable) owed() string {
	return `
	from ` + table.table + ` r
	where r.tuples_stored_at is null
	  and octet_length(user_threads_login(r.login)) <= ` + addressBound + `
	  and octet_length(` + table.toID + `) <= ` + addressBound
}

// readStatePass states the mark of each row of table an older build wrote as the route that writes
// that table states it (graphstore.FromPerson): one tuple from the person's node, which every
// spelling of their login meets at, to the session, the reply, the root message or the ask, at the
// mark's own moment, made by the person, at the sentinel since no event can be told to have written
// the row; and sets its tuples_stored_at to that moment.
func readStatePass(table readStateTable) inboxPass {
	return inboxPass{
		kind:      table.table,
		batch:     `select ` + table.keys + table.owed() + ` and ` + table.keyset + ` order by ` + table.keys + ` limit $3`,
		start:     table.start,
		write:     table.writeMarks,
		remaining: table.owed(),
	}
}

func (table readStateTable) writeMarks(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
	rows, err := tx.Query(ctx, `
		select `+table.keys+`, r.login, `+table.toID+`, `+table.moment+`
		`+table.owed()+` and `+table.named+`
		order by `+table.keys+`
		for no key update of r
	`, column(keys, 0), column(keys, 1))
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read the %s rows naming no mark: %w", table.table, err)
	}
	var tuples []graphstore.Tuple
	var firsts, seconds []string
	var moments []time.Time
	for rows.Next() {
		var first, second, login, toID string
		var at time.Time
		if err := rows.Scan(&first, &second, &login, &toID, &at); err != nil {
			rows.Close()
			return inboxWrote{}, fmt.Errorf("read a %s row naming no mark: %w", table.table, err)
		}
		tuples = append(tuples, graphstore.Tuple{
			FromKind: graphstore.KindPerson, FromID: login, Kind: table.kind,
			ToKind: table.toKind, ToID: toID,
			At: at, Who: model.Actor{Kind: "user", ID: login}, SourceEvent: graphstore.NoEvent, Asserted: true,
		})
		firsts, seconds, moments = append(firsts, first), append(seconds, second), append(moments, at)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return inboxWrote{}, fmt.Errorf("read the %s rows naming no mark: %w", table.table, err)
	}
	if len(tuples) == 0 {
		return inboxWrote{}, nil
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `select now()`).Scan(&now); err != nil {
		return inboxWrote{}, fmt.Errorf("read the time of a batch of %s: %w", table.table, err)
	}
	if err := graphstore.FromPerson(ctx, tx, now, tuples); err != nil {
		return inboxWrote{}, fmt.Errorf("state the marks of a batch of %s: %w", table.table, err)
	}
	if _, err := tx.Exec(ctx, `
		update `+table.table+` r set tuples_stored_at = marked.at
		from unnest($1::text[], $2::text[], $3::timestamptz[]) as marked (first, second, at)
		where (`+table.keys+`) = (marked.first, marked.second) and r.tuples_stored_at is null
	`, firsts, seconds, moments); err != nil {
		return inboxWrote{}, fmt.Errorf("mark a batch of %s: %w", table.table, err)
	}
	return inboxWrote{rows: int64(len(tuples)), tuples: int64(len(tuples))}, nil
}
