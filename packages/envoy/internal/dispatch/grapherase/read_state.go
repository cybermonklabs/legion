package grapherase

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/inboxwrite"
)

// readStateTable is one of the six tables a person's read state is projected onto
// (dispatch://INFRA-1152 R6): each row mirrors tuples from the person's node to what the mark is
// about, and so derives from both ends. Key is the table's primary key, the order the erase takes
// a node's rows of it in (lockReadStateRows) and how the listing names each. Items names, by the
// kind of an erased item, the predicate over $1, the item's id, that picks the rows naming it; a
// kind it does not name has no row of this table, and an erased person's rows are picked by login
// in every table (readStateScope).
type readStateTable struct {
	Table, Key string
	Items      map[string]string
}

// readStateTables are the six, in the order their writers take them (api/inbox_read.go's
// markInboxReadThreads writes it down): user_threads rows (step 3), then the Agent dismissals
// (step 4), then user_agent_state, user_agent_read and the reply-read rows in putUserAgentState's
// order, the reply-read rows being step 5, and the snoozes last, whose writer takes nothing else.
// Within a table the rows go in key order: for one person that is ascending thread_id, message_id
// or reply_id, the order the mark routes take one login's rows in, and for one item ascending
// login, the order inboxwrite.Record takes a thread's rows in.
//
// An erased item's thread is its own root, so the user_threads rows whose thread is the item are
// its, and so are the message's Agent dismissals, which name it as the root of an Agent thread, and
// the reply-read rows that name it as the reply read. A snooze of an erased ask is no row of this
// list: user_ask_snooze's ask_id is a required reference into asks, so the erase deletes it with
// the asks row (apply's walk of every required reference into a projected table) and lists it
// there. A comment or a message that is a reply in another thread is named by the rows of that
// thread it reached as well, which the erase keeps and rewrites (readRepliedRows).
var readStateTables = []readStateTable{
	{Table: "user_threads", Key: "login, thread_id", Items: map[string]string{
		graphstore.KindComment: `thread_id = $1::uuid and kind = 'comment'`,
		graphstore.KindAsk:     `thread_id = $1::uuid and kind = 'ask'`,
		graphstore.KindMessage: `thread_id = $1::uuid and kind = 'message'`,
	}},
	{Table: "user_agent_thread_dismissed", Key: "message_id, login", Items: map[string]string{
		graphstore.KindMessage: `message_id = $1::uuid`,
	}},
	{Table: "user_agent_state", Key: "login, session_id"},
	{Table: "user_agent_read", Key: "login, session_id"},
	{Table: "user_agent_reply_read", Key: "login, reply_id", Items: map[string]string{
		graphstore.KindMessage: `reply_id = $1::uuid`,
	}},
	{Table: "user_ask_snooze", Key: "login, ask_id"},
}

// readStateScope is which rows of the six tables derive from the erased node. A person's are every
// row keyed on one of their spellings: the canonical login their node is named by, each raw
// actor id a record stored for them (the recorded_as values of their node, graphstore's aliases),
// and any other spelling that canonicalises to their login (user_threads_login, migration 0095),
// since user_agent_state keys its Clear on the raw actor id the route was given (api/state.go)
// and the census counts every row by its canonical login. An item's are the rows that name it
// (readStateTables' Items). No other node has any.
type readStateScope struct {
	kind, id  string
	spellings []string
	// rootKind and rootID are the root of the thread an erased comment or message is a reply in
	// (inboxwrite.ThreadRoot), and empty for any other node and for an item that is its own root.
	rootKind, rootID string
}

// readStateScope reads the erased node's scope over the read-state tables. A person's spellings
// are read off the store's tuples, so a confirmed run reads them once it holds the node's row; so
// is the thread a comment or a message is a reply in.
func (e *Erasure) readStateScope(ctx context.Context, tx pgx.Tx) (readStateScope, error) {
	scope := readStateScope{kind: e.Kind, id: e.ID}
	if e.Kind == graphstore.KindComment || e.Kind == graphstore.KindMessage {
		rootKind, rootID, err := inboxwrite.ThreadRoot(ctx, tx, e.Kind, e.ID)
		if err != nil {
			return readStateScope{}, err
		}
		if rootKind != e.Kind || rootID != e.ID {
			scope.rootKind, scope.rootID = rootKind, rootID
		}
		return scope, nil
	}
	if e.Kind != graphstore.KindPerson {
		return scope, nil
	}
	rows, err := tx.Query(ctx, `
		select $2::text
		union
		select to_id from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4
		order by 1`, graphstore.KindPerson, e.ID, graphstore.TupleRecordedAs, graphstore.KindValue)
	if err != nil {
		return readStateScope{}, fmt.Errorf("read the spellings of %s:%s: %w", e.Kind, e.ID, err)
	}
	if scope.spellings, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return readStateScope{}, fmt.Errorf("read the spellings of %s:%s: %w", e.Kind, e.ID, err)
	}
	return scope, nil
}

// where is the predicate that picks the scope's rows of table, with its arguments, and false where
// the node has no row of that table.
func (s readStateScope) where(table readStateTable) (string, []any, bool) {
	if s.kind == graphstore.KindPerson {
		return `(login = any($1::text[]) or user_threads_login(login) = $2)`, []any{s.spellings, s.id}, true
	}
	predicate, ok := table.Items[s.kind]
	return predicate, []any{s.id}, ok
}

// replyCaused is true of a delivered_to tuple d into a person that a reply caused - the deliveries
// user_threads.version counts - by the rule graphcensus's user_threads_version kind holds every
// marked row to (its replyCaused): a tuple whose event is a reply's write, and one with no event
// that is out of a comment or a message, or out of an ask whose node states an answered_at at the
// tuple's own moment.
const replyCaused = `(case
	when d.source_event <> 0
		then exists (select 1 from events e where e.id = d.source_event
		             and e.type in ('comment.created', 'comment.answered', 'message.created', 'message.answered', 'ask.answered'))
	when d.from_kind in ('` + graphstore.KindComment + `', '` + graphstore.KindMessage + `') then true
	else exists (
		select 1 from graph_tuples answered
		where answered.from_kind = d.from_kind and answered.from_id = d.from_id and answered.to_kind = '` + graphstore.KindValue + `'
		  and answered.kind = '` + graphstore.TupleAnsweredAt + `'
		  and case when pg_input_is_valid(answered.to_id, 'timestamptz') then answered.to_id::timestamptz = d.at else false end)
	end)`

// replied is the predicate over user_threads u that picks the rows of the thread a reply the erase
// removes was written in that the reply's removal changes: each that names it as its last reply,
// and each marked row whose version counts a delivery of it, a reply-caused delivered_to into the
// row's person above the row's marked_seq (R6's equation, D4). It binds $1 the reply's id, $2 its
// kind, $3 and $4 the thread root's kind and id.
var replied = `u.thread_id = $4::uuid and u.kind = $3::text and (u.last_reply_id = $1::uuid or (u.marked_seq is not null and exists (
		select 1 from graph_tuples d
		where d.from_kind = $2::text and d.from_id = $1::text and d.kind = '` + graphstore.TupleDeliveredTo + `'
		  and d.to_kind = '` + graphstore.KindPerson + `' and d.to_id = user_threads_login(u.login) and d.asserted
		  and d.seq > u.marked_seq and ` + replyCaused + `)))`

// repliedArgs are what replied binds, and false where the node is no reply in another thread.
func (s readStateScope) repliedArgs() ([]any, bool) {
	if s.rootID == "" {
		return nil, false
	}
	return []any{s.id, s.kind, s.rootKind, s.rootID}, true
}

// readStateRows are the rows of one read-state table the erase lists and deletes, each by its key
// as row(Key)::text spells it.
type readStateRows struct {
	table readStateTable
	keys  []string
}

// repliedRow is one user_threads row a reply the erase removes reached, as the erase leaves it
// (readRepliedRows): its version less each delivery of the reply it counted since its mark, and the
// reply it names, which is the newest reply-caused delivery left into its person from its thread
// where it named the one removed, and none where no such delivery is left.
type repliedRow struct {
	login, thread string
	version       int64
	lastReply     *string
	repliedAt     *time.Time
}

// lockReadStateRows takes every read-state row of the node `for update`, the level the delete in
// apply takes, table by table in readStateTables' order and by key within each. A confirmed run
// takes them right after the node's projection rows: an item's row (its comments, asks or messages
// row, which for an Agent thread's root message is step 2 of markInboxReadThreads' order) comes
// before the rows of its thread, as every writer of those rows takes the thread's owner or its root
// first, and an erased person, whom no owner row stands over, takes their node's row and then their
// rows. A writer that takes a person's rows and then writes that person's node (inboxwrite.Record
// through graphstore.ToPerson, a mark route through graphstore.FromPerson) writes it `do nothing`,
// which waits on no lock of a row already there, and the erase deletes the node only after it holds
// every one of these rows. The user_threads rows a reply reached (replied), which the erase
// rewrites, are taken in the same statement as the rows it deletes, in the one key order.
func lockReadStateRows(ctx context.Context, tx pgx.Tx, scope readStateScope) error {
	for _, table := range readStateTables {
		where, args, ok := scope.where(table)
		if repliedArgs, reply := scope.repliedArgs(); reply && table.Table == "user_threads" {
			// The item's own rows bind $1, its id, as replied does.
			where, args, ok = `(`+where+`) or (`+replied+`)`, repliedArgs, true
		}
		if !ok {
			continue
		}
		if _, err := tx.Exec(ctx, `select 1 from `+table.Table+` u where `+where+` order by `+table.Key+` for update`,
			args...); err != nil {
			return fmt.Errorf("lock the %s rows of %s:%s: %w", table.Table, scope.kind, scope.id, err)
		}
	}
	return nil
}

// readReadState lists every read-state row of the node by its key, so the run without --confirm
// says what it deletes, and holds the keys for apply, which deletes those rows and no others.
func (e *Erasure) readReadState(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	var err error
	if read.readStateScope, err = e.readStateScope(ctx, tx); err != nil {
		return err
	}
	for _, table := range readStateTables {
		where, args, ok := read.readStateScope.where(table)
		if !ok {
			continue
		}
		rows, err := tx.Query(ctx, `select row(`+table.Key+`)::text from `+table.Table+` where `+where+
			` order by `+table.Key, args...)
		if err != nil {
			return fmt.Errorf("read the %s rows of %s:%s: %w", table.Table, e.Kind, e.ID, err)
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("read the %s rows of %s:%s: %w", table.Table, e.Kind, e.ID, err)
		}
		if len(keys) == 0 {
			continue
		}
		for _, key := range keys {
			read.listing.rows = append(read.listing.rows, fmt.Sprintf("delete %s (%s)=%s", table.Table, table.Key, key))
		}
		read.readState = append(read.readState, readStateRows{table: table, keys: keys})
	}
	return e.readRepliedRows(ctx, tx, read)
}

// readRepliedRows reads the user_threads rows a comment or a message the erase removes reached in
// the thread it is a reply in (replied), each as the erase leaves it (repliedRow), and lists each
// as a row the run rewrites. The reply's delivered_to tuples go with its node, so a row that kept
// naming it, or kept counting it, would break R6's equation: its version is its marked_version
// plus the reply-caused deliveries into its person from its thread above its mark, and its
// last_reply_id and replied_at the newest of those or the one its mark carried. The reply left in
// its place is read off the store's own deliveries from the thread's nodes - its root, and each
// node whose replies_to names it, as inboxwrite's threadOf climbs them - which the census reads
// the same rule over. It reads the rows the erase holds alone: a confirmed run took each of them
// `for update` with the node's other read-state rows (lockReadStateRows), so what it computes is
// what the run writes back, and the change it writes is the one the run makes to the store - the
// removed reply's deliveries - and no mending of a row whose store side the run leaves alone.
func (e *Erasure) readRepliedRows(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	args, reply := read.readStateScope.repliedArgs()
	if !reply {
		return nil
	}
	var nodes string
	switch read.readStateScope.rootKind {
	case graphstore.KindMessage:
		nodes = `tree (id, depth) as (
			select $4::text, 0
			union all
			select t.from_id, tree.depth + 1
			from tree
			join graph_tuples t
			  on t.to_kind = '` + graphstore.KindMessage + `' and t.to_id = tree.id and t.kind = '` + graphstore.TupleRepliesTo + `'
			 and t.from_kind = '` + graphstore.KindMessage + `' and t.asserted
			where tree.depth < 64
		), nodes (kind, id) as (
			select '` + graphstore.KindMessage + `', tree.id from tree
		)`
	default:
		// A comment's thread is one step from its root: the ask its replies_to names, else the
		// comment it names, so a comment under a comment root that also names an ask is the
		// ask's.
		askedElsewhere := ""
		if read.readStateScope.rootKind == graphstore.KindComment {
			askedElsewhere = ` and not exists (
				select 1 from graph_tuples asked
				where asked.from_kind = reply.from_kind and asked.from_id = reply.from_id
				  and asked.kind = '` + graphstore.TupleRepliesTo + `' and asked.to_kind = '` + graphstore.KindAsk + `' and asked.asserted)`
		}
		nodes = `nodes (kind, id) as (
			select $3::text, $4::text
			union
			select reply.from_kind, reply.from_id
			from graph_tuples reply
			where reply.to_kind = $3::text and reply.to_id = $4::text and reply.kind = '` + graphstore.TupleRepliesTo + `'
			  and reply.from_kind = '` + graphstore.KindComment + `' and reply.asserted` + askedElsewhere + `
		)`
	}
	rows, err := tx.Query(ctx, `
		with recursive `+nodes+`
		select row(u.login, u.thread_id)::text, u.login, u.thread_id::text,
		       u.version - case when u.marked_seq is null then 0 else (
		           select count(*) from graph_tuples d
		           where d.from_kind = $2::text and d.from_id = $1::text and d.kind = '`+graphstore.TupleDeliveredTo+`'
		             and d.to_kind = '`+graphstore.KindPerson+`' and d.to_id = user_threads_login(u.login) and d.asserted
		             and d.seq > u.marked_seq and `+replyCaused+`) end,
		       case when u.last_reply_id = $1::uuid then left_newest.from_id else u.last_reply_id::text end,
		       case when u.last_reply_id = $1::uuid then left_newest.at else u.replied_at end
		from user_threads u
		left join lateral (
			select d.from_id, d.at
			from graph_tuples d
			join nodes on nodes.kind = d.from_kind and nodes.id = d.from_id
			where d.kind = '`+graphstore.TupleDeliveredTo+`' and d.to_kind = '`+graphstore.KindPerson+`'
			  and d.to_id = user_threads_login(u.login) and d.asserted
			  and not (d.from_kind = $2::text and d.from_id = $1::text) and `+replyCaused+`
			order by d.at desc, d.seq desc
			limit 1
		) left_newest on true
		where `+replied+`
		order by u.login, u.thread_id`, args...)
	if err != nil {
		return fmt.Errorf("read the user_threads rows %s:%s reached: %w", e.Kind, e.ID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var row repliedRow
		if err := rows.Scan(&key, &row.login, &row.thread, &row.version, &row.lastReply, &row.repliedAt); err != nil {
			return fmt.Errorf("read a user_threads row %s:%s reached: %w", e.Kind, e.ID, err)
		}
		read.listing.rows = append(read.listing.rows, fmt.Sprintf("rewrite user_threads (login, thread_id)=%s", key))
		read.replied = append(read.replied, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the user_threads rows %s:%s reached: %w", e.Kind, e.ID, err)
	}
	return nil
}

// deleteReadState deletes the read-state rows the listing named, by their keys, under the
// predicate that found them, so a row written since the listing was read is no row it deletes.
func (e *Erasure) deleteReadState(ctx context.Context, tx pgx.Tx, read erasedRead) error {
	for _, held := range read.readState {
		where, args, ok := read.readStateScope.where(held.table)
		if !ok {
			continue
		}
		tag, err := tx.Exec(ctx, `delete from `+held.table.Table+` where `+where+
			fmt.Sprintf(` and row(%s)::text = any($%d::text[])`, held.table.Key, len(args)+1), append(args, held.keys)...)
		if err != nil {
			return fmt.Errorf("delete the %s rows of %s:%s: %w", held.table.Table, e.Kind, e.ID, err)
		}
		e.removed(held.table.Table, tag.RowsAffected())
	}
	return nil
}

// rewriteRepliedRows writes back the user_threads rows the listing named as a removed reply
// reached, as readRepliedRows left them: each row's version, last_reply_id and replied_at, and
// nothing else, so its mark holds as it stood (the erase's connection carries this build's marker
// writer setting, connect). Each is a row the run holds `for update` since lockReadStateRows, and
// it writes no row the listing did not name.
func (e *Erasure) rewriteRepliedRows(ctx context.Context, tx pgx.Tx, read erasedRead) error {
	if len(read.replied) == 0 {
		return nil
	}
	logins := make([]string, len(read.replied))
	threads := make([]string, len(read.replied))
	versions := make([]int64, len(read.replied))
	lastReplies := make([]*string, len(read.replied))
	repliedAts := make([]*time.Time, len(read.replied))
	for index, row := range read.replied {
		logins[index], threads[index], versions[index] = row.login, row.thread, row.version
		lastReplies[index], repliedAts[index] = row.lastReply, row.repliedAt
	}
	tag, err := tx.Exec(ctx, `
		update user_threads u
		set version = mended.version, last_reply_id = mended.last_reply::uuid, replied_at = mended.replied_at
		from unnest($1::text[], $2::uuid[], $3::bigint[], $4::text[], $5::timestamptz[])
		  as mended (login, thread, version, last_reply, replied_at)
		where u.login = mended.login and u.thread_id = mended.thread
	`, logins, threads, versions, lastReplies, repliedAts)
	if err != nil {
		return fmt.Errorf("rewrite the user_threads rows %s:%s reached: %w", e.Kind, e.ID, err)
	}
	e.redacted("user_threads", tag.RowsAffected())
	return nil
}
