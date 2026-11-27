package graphcensus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/inboxwrite"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// The tests of the one inbox's backfill (backfill_inbox.go, backfill_inbox_threads.go).
// Each writes, through SQL, rows as a build of the release before this one writes them - no marker
// of this child's, each beside the event its write appended - runs the backfill, and reads that
// each row took exactly the tuples this release's writer of it states, under that write's own
// event and dated by the row; that each row is marked and its kind reports nothing left; and that
// a second run appends nothing and marks nothing.

// innerTuple is one tuple as these tests compare them: its two ends as kind:id, its kind, the
// event it is caused by, its own moment and whether it asserts.
type innerTuple struct {
	from, kind, to string
	event          int64
	at             time.Time
	retracted      bool
}

func (tuple innerTuple) String() string {
	ended := ""
	if tuple.retracted {
		ended = " retracted"
	}
	return fmt.Sprintf("%s -%s-> %s @%d at %s%s", tuple.from, tuple.kind, tuple.to, tuple.event,
		tuple.at.UTC().Format(time.RFC3339Nano), ended)
}

// storedTuplesTouching is every tuple of kinds with either end one of nodes (kind:id), as
// innerTuple prints it, sorted.
func storedTuplesTouching(t *testing.T, database *store.Store, kinds []string, nodes ...string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select from_kind || ':' || from_id, kind, to_kind || ':' || to_id, source_event, at, not asserted
		from graph_tuples
		where kind = any($1) and (from_kind || ':' || from_id = any($2) or to_kind || ':' || to_id = any($2))
	`, kinds, nodes)
	if err != nil {
		t.Fatalf("read the tuples touching %v: %v", nodes, err)
	}
	read, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var tuple innerTuple
		err := row.Scan(&tuple.from, &tuple.kind, &tuple.to, &tuple.event, &tuple.at, &tuple.retracted)
		return tuple.String(), err
	})
	if err != nil {
		t.Fatalf("read the tuples touching %v: %v", nodes, err)
	}
	slices.Sort(read)
	return read
}

// requireTuples fails unless the tuples of kinds touching nodes are exactly want.
func requireTuples(t *testing.T, database *store.Store, what string, kinds []string, nodes []string, want ...innerTuple) {
	t.Helper()
	expected := make([]string, 0, len(want))
	for _, tuple := range want {
		expected = append(expected, tuple.String())
	}
	slices.Sort(expected)
	got := storedTuplesTouching(t, database, kinds, nodes...)
	if !slices.Equal(got, expected) {
		t.Errorf("%s: the store holds\n  %s\nwant exactly\n  %s", what, joinLines(got), joinLines(expected))
	}
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return "(none)"
	}
	return strings.Join(lines, "\n  ")
}

// backfillInboxOnce runs the backfill and fails unless every kind the run reports is complete,
// answering the report.
func backfillInboxOnce(t *testing.T, database *store.Store) BackfillReport {
	t.Helper()
	report, err := Backfill(context.Background(), database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	for _, kind := range report.Kinds {
		if !kind.Complete {
			t.Errorf("the backfill left %s with %d rows owed: %#v", kind.Kind, kind.Remaining, kind)
		}
	}
	return report
}

// requireASecondRunAppendsNothing runs the backfill again and fails unless it reports no row,
// node or tuple of any kind and the log's newest tuple is the one the first run left.
func requireASecondRunAppendsNothing(t *testing.T, database *store.Store) {
	t.Helper()
	newest := func() int64 {
		var seq int64
		if err := database.Pool.QueryRow(context.Background(), `select coalesce(max(seq), 0) from graph_tuples`).Scan(&seq); err != nil {
			t.Fatalf("read the log's newest tuple: %v", err)
		}
		return seq
	}
	before := newest()
	for _, done := range backfillInboxOnce(t, database).Kinds {
		if done.Rows != 0 || done.Nodes != 0 || done.Tuples != 0 {
			t.Errorf("the second run reports %#v for %s, want nothing written", done, done.Kind)
		}
	}
	if after := newest(); after != before {
		t.Errorf("the second run appended tuples %d to %d, want none", before+1, after)
	}
}

// olderWrite is one row a build of the release before this one wrote: its id, its created_at and
// the event its write appended.
type olderWrite struct {
	id    string
	at    time.Time
	event int64
}

// writeAsAnOlderBuild runs insert, the statement of one write an older build makes, which answers
// the row's id and created_at, and appends the event that write appends, of eventType on issue
// CENS-1 by by, whose payload names the row's id: an ask event's is the ask flat
// (model.AskEventPayload), which the broker holds it to, and every other's names the id alone.
func writeAsAnOlderBuild(t *testing.T, writes addressingWrites, eventType string, by model.Actor, insert string, args ...any) olderWrite {
	t.Helper()
	var written olderWrite
	event := writes.write(t, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		if err := tx.QueryRow(ctx, insert, args...).Scan(&written.id, &written.at); err != nil {
			return model.Event{}, err
		}
		var payload any = map[string]any{"id": written.id}
		if strings.HasPrefix(eventType, "ask.") {
			payload = model.AskEventPayload{Ask: model.Ask{ID: written.id, IssueKey: new("CENS-1"), Author: by, CreatedAt: written.at}}
		}
		return model.Event{IssueKey: new("CENS-1"), Type: eventType, Actor: by, Payload: payload}, nil
	})
	written.event = event.ID
	return written
}

// threadRow is one user_threads row as the thread pass marks it.
type threadRow struct {
	version, markedVersion, markedSeq *int64
}

func readThreadRow(t *testing.T, database *store.Store, login, thread string) threadRow {
	t.Helper()
	var row threadRow
	if err := database.Pool.QueryRow(context.Background(), `
		select version, marked_version, marked_seq from user_threads where login = $1 and thread_id = $2::uuid
	`, login, thread).Scan(&row.version, &row.markedVersion, &row.markedSeq); err != nil {
		t.Fatalf("read %s's row of thread %s: %v", login, thread, err)
	}
	return row
}

// requireMarkedAtItsVersion fails unless login's row of thread is marked at the version it holds,
// and marked above every delivery to login the store holds, of this thread or any other, so R6's
// equation counts none of this thread's: they are in the version already.
func requireMarkedAtItsVersion(t *testing.T, database *store.Store, login, thread string, version int64) {
	t.Helper()
	row := readThreadRow(t, database, login, thread)
	if row.version == nil || *row.version != version || row.markedVersion == nil || *row.markedVersion != version || row.markedSeq == nil {
		t.Errorf("%s's row of thread %s holds version %v, marked_version %v and marked_seq %v, want version %d marked at it",
			login, thread, row.version, row.markedVersion, row.markedSeq, version)
		return
	}
	var above int64
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples
		where kind = $1 and to_kind = $2 and to_id = $3 and seq > $4
	`, graphstore.TupleDeliveredTo, graphstore.KindPerson, login, *row.markedSeq).Scan(&above); err != nil {
		t.Fatalf("count the deliveries to %s above the mark: %v", login, err)
	}
	if above != 0 {
		t.Errorf("%d deliveries to %s stand above the mark of their row of thread %s, which counts them twice", above, login, thread)
	}
}

var (
	alice = model.Actor{Kind: "user", ID: "alice"}
	bob   = model.Actor{Kind: "user", ID: "bob"}
	carol = model.Actor{Kind: "user", ID: "carol"}
	dave  = model.Actor{Kind: "user", ID: "dave"}
)

// actorJSON is actor as a row's author column holds it.
func actorJSON(t *testing.T, actor model.Actor) string {
	t.Helper()
	encoded, err := json.Marshal(actor)
	if err != nil {
		t.Fatalf("encode %v: %v", actor, err)
	}
	return string(encoded)
}

// An issue message and its reply, and a comment and a reply that answers it, each written as the
// release before this one writes it: rows naming no node and holding no tuples_stored, which that
// release's triggers project into user_threads. The backfill states each message's pointers, who
// wrote it and what it owed its thread, and each comment's pointers and what it owed, under each
// write's own event and at its row's moment - the reply is delivered to the person who wrote the
// root before it, as the old trigger raised that person's row, and its writer takes part - then
// marks every row, the user_threads rows at the version the old triggers left them.
func TestTheBackfillStatesAnOlderBuildsMessagesAndCommentPointers(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	root := writeAsAnOlderBuild(t, writes, "message.created", alice, `
		insert into messages (issue_key, author, body) values ('CENS-1', $1, 'Ship it on Friday?')
		returning id::text, created_at`, actorJSON(t, alice))
	reply := writeAsAnOlderBuild(t, writes, "message.answered", bob, `
		insert into messages (issue_key, author, body, in_reply_to) values ('CENS-1', $1, 'Friday works.', $2)
		returning id::text, created_at`, actorJSON(t, bob), root.id)
	thread := writeAsAnOlderBuild(t, writes, "comment.created", alice, `
		insert into comments (issue_key, author, body) values ('CENS-1', $1, 'Is the copy final?')
		returning id::text, created_at`, actorJSON(t, alice))
	answer := writeAsAnOlderBuild(t, writes, "comment.created", bob, `
		insert into comments (issue_key, author, body, reply_to, answers) values ('CENS-1', $1, 'It is.', $2, $2)
		returning id::text, created_at`, actorJSON(t, bob), thread.id)

	report := backfillInboxOnce(t, writes.database)
	for kind, rows := range map[string]int64{"messages": 2, "comment_pointers": 2} {
		if done := reportedKind(t, report, kind); done.Rows != rows {
			t.Errorf("the %s pass reports %#v, want %d rows", kind, done, rows)
		}
	}

	message := func(id string) string { return graphstore.KindMessage + ":" + id }
	comment := func(id string) string { return graphstore.KindComment + ":" + id }
	person := func(login string) string { return graphstore.KindPerson + ":" + login }
	kinds := []string{graphstore.TupleRepliesTo, graphstore.TuplePartOf, graphstore.TupleAuthoredBy, graphstore.TupleAnswers,
		graphstore.TupleDeliveredTo, graphstore.TupleParticipatesIn, graphstore.TupleAddressedTo}
	requireTuples(t, writes.database, "the issue message and its reply", kinds, []string{message(root.id), message(reply.id)},
		innerTuple{from: message(root.id), kind: graphstore.TuplePartOf, to: "issue:CENS-1", event: root.event, at: root.at},
		innerTuple{from: message(root.id), kind: graphstore.TupleAuthoredBy, to: person("alice"), event: root.event, at: root.at},
		innerTuple{from: person("alice"), kind: graphstore.TupleParticipatesIn, to: message(root.id), event: root.event, at: root.at},
		innerTuple{from: message(reply.id), kind: graphstore.TupleRepliesTo, to: message(root.id), event: reply.event, at: reply.at},
		innerTuple{from: message(reply.id), kind: graphstore.TuplePartOf, to: message(root.id), event: reply.event, at: reply.at},
		innerTuple{from: message(reply.id), kind: graphstore.TupleAuthoredBy, to: person("bob"), event: reply.event, at: reply.at},
		innerTuple{from: message(reply.id), kind: graphstore.TupleDeliveredTo, to: person("alice"), event: reply.event, at: reply.at},
		innerTuple{from: person("bob"), kind: graphstore.TupleParticipatesIn, to: message(root.id), event: reply.event, at: reply.at},
	)
	// A comment's author is the node store's backfill's, so its authored_by is not this
	// child's to state, and is left out of the kinds read here.
	commentKinds := []string{graphstore.TupleRepliesTo, graphstore.TuplePartOf, graphstore.TupleAnswers,
		graphstore.TupleDeliveredTo, graphstore.TupleParticipatesIn}
	requireTuples(t, writes.database, "the comment and the reply that answers it", commentKinds, []string{comment(thread.id), comment(answer.id)},
		innerTuple{from: person("alice"), kind: graphstore.TupleParticipatesIn, to: comment(thread.id), event: thread.event, at: thread.at},
		innerTuple{from: comment(answer.id), kind: graphstore.TupleRepliesTo, to: comment(thread.id), event: answer.event, at: answer.at},
		innerTuple{from: comment(answer.id), kind: graphstore.TupleAnswers, to: comment(thread.id), event: answer.event, at: answer.at},
		innerTuple{from: comment(answer.id), kind: graphstore.TuplePartOf, to: comment(thread.id), event: answer.event, at: answer.at},
		innerTuple{from: comment(answer.id), kind: graphstore.TupleDeliveredTo, to: person("alice"), event: answer.event, at: answer.at},
		innerTuple{from: person("bob"), kind: graphstore.TupleParticipatesIn, to: comment(thread.id), event: answer.event, at: answer.at},
	)

	var unmarkedMessages, unmarkedComments int64
	if err := writes.database.Pool.QueryRow(context.Background(), `
		select (select count(*) from messages where node_kind is null or node_address <> id::text),
		       (select count(*) from comments where not tuples_stored)
	`).Scan(&unmarkedMessages, &unmarkedComments); err != nil {
		t.Fatalf("count the unmarked rows: %v", err)
	}
	if unmarkedMessages != 0 || unmarkedComments != 0 {
		t.Errorf("%d messages and %d comments are left unmarked, want none", unmarkedMessages, unmarkedComments)
	}
	// bob's reply raised alice's row of each thread, and his own row is his write's.
	requireMarkedAtItsVersion(t, writes.database, "alice", root.id, 1)
	requireMarkedAtItsVersion(t, writes.database, "bob", root.id, 0)
	requireMarkedAtItsVersion(t, writes.database, "alice", thread.id, 1)
	requireMarkedAtItsVersion(t, writes.database, "bob", thread.id, 0)
	requireASecondRunAppendsNothing(t, writes.database)
}

// A comment, an issue message and an ask written as the release before this one writes them, with
// the body blocks its block index wrote for each, a link out of each block and the item's citation:
// every row naming no node and holding no tuples_stored. The backfill states each block as refs'
// item index states one - a live block node in its item's project, made by its maker at its row's
// time, and its part_of to the item - each link as a tuple out of the block's node and each
// citation as the item's mentions, each dated by its row and carrying its source_seq, under the
// event that recorded the item; and names the nodes on the rows and sets tuples_stored. A block
// the item no longer holds is stated gone. An ask's answer part is made by whoever answered.
func TestTheBackfillStatesAnOlderBuildsItemBlocksLinksAndCitations(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	writes.createIssue(t, "CENS-2", 2, "V", alice)
	ctx := context.Background()
	comment := writeAsAnOlderBuild(t, writes, "comment.created", alice, `
		insert into comments (issue_key, author, body) values ('CENS-1', $1, 'After dispatch://CENS-2.')
		returning id::text, created_at`, actorJSON(t, alice))
	message := writeAsAnOlderBuild(t, writes, "message.created", alice, `
		insert into messages (issue_key, author, body) values ('CENS-1', $1, 'After dispatch://CENS-2.')
		returning id::text, created_at`, actorJSON(t, alice))
	ask := writeAsAnOlderBuild(t, writes, "ask.opened", alice, `
		insert into asks (issue_key, author, question) values ('CENS-1', $1, 'After dispatch://CENS-2?')
		returning id::text, created_at`, actorJSON(t, alice))

	type item struct {
		kind, column string
		written      olderWrite
	}
	items := []item{{"comment", "comment_id", comment}, {"message", "message_id", message}, {"ask", "ask_id", ask}}
	blockAt := map[string]time.Time{}
	linkAt := map[string]time.Time{}
	citedAt := map[string]time.Time{}
	for index, held := range items {
		seq := int64(100 + index)
		columns := `, made_at, changed_revision, changed_at`
		values := `, now(), 1, now()`
		if held.kind == "message" {
			columns, values = `, made_at, changed_at`, `, now(), now()`
		}
		var at time.Time
		if err := writes.database.Pool.QueryRow(ctx, `
			insert into `+held.kind+`_blocks (`+held.column+`, block_id, node_id, position, type, text_sha256, excerpt, created_at`+columns+`)
			values ($1, 'b1', $2, 0, 'paragraph', '', 'After dispatch://CENS-2.', now()`+values+`)
			returning created_at
		`, held.written.id, held.kind+"/"+held.written.id+"#b1").Scan(&at); err != nil {
			t.Fatalf("write the block of the %s: %v", held.kind, err)
		}
		blockAt[held.kind] = at
		if err := writes.database.Pool.QueryRow(ctx, `
			insert into `+held.kind+`_block_refs (`+held.column+`, block_id, from_node, to_kind, to_id, kind, created_at, source_seq)
			values ($1, 'b1', $2, 'issue', 'CENS-2', 'mentions', now(), $3)
			returning created_at
		`, held.written.id, held.kind+"/"+held.written.id+"#b1", seq).Scan(&at); err != nil {
			t.Fatalf("write the link of the %s's block: %v", held.kind, err)
		}
		linkAt[held.kind] = at
		if err := writes.database.Pool.QueryRow(ctx, `
			insert into refs (from_kind, from_id, to_kind, to_id, kind, created_at, source_seq)
			values ($1, $2, 'issue', 'CENS-2', 'mentions', now(), $3)
			returning created_at
		`, held.kind, held.written.id, seq).Scan(&at); err != nil {
			t.Fatalf("write the citation of the %s: %v", held.kind, err)
		}
		citedAt[held.kind] = at
	}
	// A comment block the comment no longer holds, and an ask's answer part, made by who answered.
	var removedAt, partAt time.Time
	if err := writes.database.Pool.QueryRow(ctx, `
		insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt, created_at,
		                            made_at, changed_revision, changed_at, removed_at)
		values ($1, 'b2', $2, null, 'paragraph', '', 'An earlier word.', now(), now(), 1, now(), now())
		returning created_at
	`, comment.id, "comment/"+comment.id+"#b2").Scan(&removedAt); err != nil {
		t.Fatalf("write the removed block of the comment: %v", err)
	}
	if err := writes.database.Pool.QueryRow(ctx, `
		insert into ask_blocks (ask_id, block_id, node_id, position, type, text_sha256, excerpt, created_at,
		                        made_at, changed_revision, changed_at, made_by)
		values ($1, 'b2', $2, 1, 'answer', '', 'Yes.', now(), now(), 1, now(), $3)
		returning created_at
	`, ask.id, "ask/"+ask.id+"#b2", actorJSON(t, bob)).Scan(&partAt); err != nil {
		t.Fatalf("write the answer part of the ask: %v", err)
	}

	report := backfillInboxOnce(t, writes.database)
	for kind, rows := range map[string]int64{
		"comment_blocks": 2, "message_blocks": 1, "ask_blocks": 2,
		"comment_block_refs": 1, "message_block_refs": 1, "ask_block_refs": 1,
		"comment_refs": 1, "message_refs": 1, "ask_refs": 1,
	} {
		if done := reportedKind(t, report, kind); done.Rows != rows {
			t.Errorf("the %s pass reports %#v, want %d rows", kind, done, rows)
		}
	}
	// Beside each item's first block, the other part_of that touches the item: the comment's removed
	// block and the ask's answer part, each placed in its item, and the message's own on its issue.
	others := map[string]innerTuple{
		"comment": {from: refs.BlockNodeKind + ":comment/" + comment.id + "#b2", kind: graphstore.TuplePartOf,
			to: "comment:" + comment.id, event: comment.event, at: removedAt},
		"message": {from: "message:" + message.id, kind: graphstore.TuplePartOf, to: "issue:CENS-1", event: message.event, at: message.at},
		"ask": {from: refs.BlockNodeKind + ":ask/" + ask.id + "#b2", kind: graphstore.TuplePartOf,
			to: "ask:" + ask.id, event: ask.event, at: partAt},
	}
	for index, held := range items {
		block := refs.BlockNodeKind + ":" + held.kind + "/" + held.written.id + "#b1"
		node := held.kind + ":" + held.written.id
		requireTuples(t, writes.database, "the "+held.kind+"'s block", []string{graphstore.TuplePartOf, graphstore.TupleMentions},
			[]string{block, node},
			innerTuple{from: block, kind: graphstore.TuplePartOf, to: node, event: held.written.event, at: blockAt[held.kind]},
			innerTuple{from: block, kind: graphstore.TupleMentions, to: "issue:CENS-2", event: held.written.event, at: linkAt[held.kind]},
			innerTuple{from: node, kind: graphstore.TupleMentions, to: "issue:CENS-2", event: held.written.event, at: citedAt[held.kind]},
			others[held.kind],
		)
		requireBlockNode(t, writes.database, held.kind+"/"+held.written.id+"#b1", graphstore.StateLive, "alice", blockAt[held.kind], held.written.event)
		// Each tuple carries its row's source_seq: both rows of this item were written at 100+index.
		want := int64(100 + index)
		var seqs []*int64
		rows, err := writes.database.Pool.Query(ctx, `
			select legacy_source_seq from graph_tuples
			where kind = 'mentions' and (from_id = $1 or from_id = $2) order by from_kind
		`, held.kind+"/"+held.written.id+"#b1", held.written.id)
		if err == nil {
			seqs, err = pgx.CollectRows(rows, pgx.RowTo[*int64])
		}
		carried := make([]any, len(seqs))
		for at, seq := range seqs {
			if carried[at] = "null"; seq != nil {
				carried[at] = *seq
			}
		}
		if err != nil || len(seqs) != 2 || seqs[0] == nil || *seqs[0] != want || seqs[1] == nil || *seqs[1] != want {
			t.Errorf("the %s's link and citation carry legacy_source_seq %v (%v), want their rows' source_seq %d each", held.kind, carried, err, want)
		}
	}
	requireBlockNode(t, writes.database, "comment/"+comment.id+"#b2", graphstore.StateGone, "alice", removedAt, comment.event)
	requireBlockNode(t, writes.database, "ask/"+ask.id+"#b2", graphstore.StateLive, "bob", partAt, ask.event)

	var unmarked int64
	if err := writes.database.Pool.QueryRow(ctx, `
		select (select count(*) from comment_blocks where node_kind is null) + (select count(*) from message_blocks where node_kind is null)
		     + (select count(*) from ask_blocks where node_kind is null) + (select count(*) from comment_block_refs where node_kind is null)
		     + (select count(*) from message_block_refs where node_kind is null) + (select count(*) from ask_block_refs where node_kind is null)
		     + (select count(*) from refs where not tuples_stored and from_kind in ('comment', 'message', 'ask'))
	`).Scan(&unmarked); err != nil {
		t.Fatalf("count the unmarked item rows: %v", err)
	}
	if unmarked != 0 {
		t.Errorf("%d block, link and citation rows of the items are left unmarked, want none", unmarked)
	}
	requireASecondRunAppendsNothing(t, writes.database)
}

// requireBlockNode fails unless the store holds the block node address in state, made by login at
// at and caused by event.
func requireBlockNode(t *testing.T, database *store.Store, address, state, login string, at time.Time, event int64) {
	t.Helper()
	node, found, err := graphstore.ReadNode(context.Background(), database.Pool, refs.BlockNodeKind, address)
	if err != nil || !found {
		t.Fatalf("read the block node %s: found %t, %v", address, found, err)
	}
	if node.State != state || node.MadeBy.ID != login || !node.MadeAt.Equal(at) || node.SourceEvent != event || node.Project != "CENS" {
		t.Errorf("the block node %s is %s, made by %s at %s under %d in %q; want %s, made by %s at %s under %d in CENS",
			address, node.State, node.MadeBy.ID, node.MadeAt, node.SourceEvent, node.Project, state, login, at, event)
	}
}

// A mention's two attempts and a targeted message's first, written as the release before this one
// writes them: rows naming no node. The backfill writes each attempt's node, made at its row's
// created_at - by the comment's or the message's author for a first attempt, which that write
// opened under its event, and by the delivery machinery at the sentinel for a later one - names it
// on the row, and states what the row holds as graphstore.QueueAttempted does: its values,
// owed_to its target while it is pending, and delivered_to the session that took it.
func TestTheBackfillStatesAnOlderBuildsDeliveryAttempts(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	comment := writeAsAnOlderBuild(t, writes, "comment.created", alice, `
		insert into comments (issue_key, author, body) values ('CENS-1', $1, 'For the agent.')
		returning id::text, created_at`, actorJSON(t, alice))
	message := writeAsAnOlderBuild(t, writes, "message.created", alice, `
		insert into messages (issue_key, author, body, target) values ('CENS-1', $1, 'For the agent.', 'session:s2')
		returning id::text, created_at`, actorJSON(t, alice))
	created := map[string]time.Time{}
	for key, statement := range map[string]string{
		"comment 1": `insert into comment_deliveries (comment_id, target, attempt, delivery, state) values ($1, 'session:s1', 1, 'aside', 'pending') returning created_at`,
		"comment 2": `insert into comment_deliveries (comment_id, target, attempt, delivery, state, session_id) values ($1, 'session:s1', 2, 'aside', 'sent', 's1') returning created_at`,
		"message 1": `insert into message_deliveries (message_id, attempt, delivery, session_id, state) values ($1, 1, 'aside', '', 'pending') returning created_at`,
	} {
		id := comment.id
		if key == "message 1" {
			id = message.id
		}
		var at time.Time
		if err := writes.database.Pool.QueryRow(ctx, statement, id).Scan(&at); err != nil {
			t.Fatalf("write the %s attempt: %v", key, err)
		}
		created[key] = at
	}

	report := backfillInboxOnce(t, writes.database)
	for kind, rows := range map[string]int64{"comment_deliveries": 2, "message_deliveries": 1} {
		if done := reportedKind(t, report, kind); done.Rows != rows {
			t.Errorf("the %s pass reports %#v, want %d rows", kind, done, rows)
		}
	}
	first := graphstore.CommentAttempt(comment.id, "session:s1", 1).ID()
	second := graphstore.CommentAttempt(comment.id, "session:s1", 2).ID()
	messaged := graphstore.MessageAttempt(message.id, 1).ID()
	attempt := func(id string) string { return graphstore.KindDeliveryAttempt + ":" + id }
	kinds := []string{graphstore.AttemptNumber, graphstore.AttemptState, graphstore.AttemptDuplicate,
		graphstore.TupleOwedTo, graphstore.TupleDeliveredTo}
	requireTuples(t, writes.database, "the three attempts", kinds, []string{attempt(first), attempt(second), attempt(messaged)},
		innerTuple{from: attempt(first), kind: graphstore.AttemptNumber, to: "value:1", event: comment.event, at: created["comment 1"]},
		innerTuple{from: attempt(first), kind: graphstore.AttemptState, to: "value:pending", event: comment.event, at: created["comment 1"]},
		innerTuple{from: attempt(first), kind: graphstore.AttemptDuplicate, to: "value:false", event: comment.event, at: created["comment 1"]},
		innerTuple{from: attempt(first), kind: graphstore.TupleOwedTo, to: "session:s1", event: comment.event, at: created["comment 1"]},
		innerTuple{from: attempt(second), kind: graphstore.AttemptNumber, to: "value:2", event: graphstore.NoEvent, at: created["comment 2"]},
		innerTuple{from: attempt(second), kind: graphstore.AttemptState, to: "value:sent", event: graphstore.NoEvent, at: created["comment 2"]},
		innerTuple{from: attempt(second), kind: graphstore.AttemptDuplicate, to: "value:false", event: graphstore.NoEvent, at: created["comment 2"]},
		innerTuple{from: attempt(second), kind: graphstore.TupleDeliveredTo, to: "session:s1", event: graphstore.NoEvent, at: created["comment 2"]},
		innerTuple{from: attempt(messaged), kind: graphstore.AttemptNumber, to: "value:1", event: message.event, at: created["message 1"]},
		innerTuple{from: attempt(messaged), kind: graphstore.AttemptState, to: "value:pending", event: message.event, at: created["message 1"]},
		innerTuple{from: attempt(messaged), kind: graphstore.AttemptDuplicate, to: "value:false", event: message.event, at: created["message 1"]},
		innerTuple{from: attempt(messaged), kind: graphstore.TupleOwedTo, to: "session:s2", event: message.event, at: created["message 1"]},
	)
	for id, want := range map[string]struct {
		by    string
		event int64
		at    time.Time
	}{
		first:    {"alice", comment.event, created["comment 1"]},
		second:   {graphstore.AttemptWriter.ID, graphstore.NoEvent, created["comment 2"]},
		messaged: {"alice", message.event, created["message 1"]},
	} {
		node, found, err := graphstore.ReadNode(ctx, writes.database.Pool, graphstore.KindDeliveryAttempt, id)
		if err != nil || !found {
			t.Fatalf("read the node of attempt %s: found %t, %v", id, found, err)
		}
		if node.MadeBy.ID != want.by || node.SourceEvent != want.event || !node.MadeAt.Equal(want.at) {
			t.Errorf("attempt %s's node is made by %s under %d at %s, want %s under %d at %s",
				id, node.MadeBy.ID, node.SourceEvent, node.MadeAt, want.by, want.event, want.at)
		}
	}
	var unmarked int64
	if err := writes.database.Pool.QueryRow(ctx, `
		select (select count(*) from comment_deliveries where node_kind is null)
		     + (select count(*) from message_deliveries where node_kind is null)
	`).Scan(&unmarked); err != nil {
		t.Fatalf("count the unmarked attempts: %v", err)
	}
	if unmarked != 0 {
		t.Errorf("%d attempt rows are left naming no node, want none", unmarked)
	}
	requireASecondRunAppendsNothing(t, writes.database)
}

// The attempt pass reads what its rows hold once per writer of a batch, not once per row
// (graphstore.AttemptFactsAsWritten takes every attempt of one writer at once). A batch of four
// comment attempts - the first attempts of alice's two comments to three sessions, and a later
// attempt the delivery machinery opened - reads the comment_deliveries rows twice, once for alice
// and once for the machinery, and each attempt's facts still carry its own comment's event.
func TestTheAttemptPassReadsItsRowsOncePerWriter(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	first := writeAsAnOlderBuild(t, writes, "comment.created", alice, `
		insert into comments (issue_key, author, body) values ('CENS-1', $1, 'For two agents.')
		returning id::text, created_at`, actorJSON(t, alice))
	second := writeAsAnOlderBuild(t, writes, "comment.created", alice, `
		insert into comments (issue_key, author, body) values ('CENS-1', $1, 'For a third.')
		returning id::text, created_at`, actorJSON(t, alice))
	backfillInboxOnce(t, writes.database)
	attempts := []struct {
		comment olderWrite
		target  string
		attempt int
	}{{first, "session:s1", 1}, {first, "session:s2", 1}, {second, "session:s3", 1}, {first, "session:s1", 2}}
	for _, written := range attempts {
		if _, err := writes.database.Pool.Exec(ctx, `
			insert into comment_deliveries (comment_id, target, attempt, delivery, state) values ($1::uuid, $2, $3, 'aside', 'pending')
		`, written.comment.id, written.target, written.attempt); err != nil {
			t.Fatalf("write attempt %d at %s as the release before does: %v", written.attempt, written.target, err)
		}
	}

	recorder := &storetest.StatementRecorder{}
	traced := storetest.Reopened(t, writes.database, func(config *pgxpool.Config) { config.ConnConfig.Tracer = recorder })
	done, err := runInboxPass(ctx, traced, attemptsPass(commentAttempts), &storeGrowth{})
	if err != nil {
		t.Fatalf("run the comment attempt pass: %v", err)
	}
	if done.Rows != 4 || !done.Complete {
		t.Errorf("the comment attempt pass reports %#v, want the 4 attempts and nothing left", done)
	}
	reads := 0
	for _, statement := range recorder.Statements() {
		if strings.Contains(statement, "join comment_deliveries d") && strings.Contains(statement, "named (comment_id, target, attempt)") {
			reads++
		}
	}
	if reads != 2 {
		t.Errorf("the pass read the comment_deliveries rows %d times for a batch of 4 attempts of 2 writers, want once per writer", reads)
	}
	attempt := func(written int) string {
		key := graphstore.CommentAttempt(attempts[written].comment.id, attempts[written].target, attempts[written].attempt)
		return graphstore.KindDeliveryAttempt + ":" + key.ID()
	}
	want := []innerTuple{}
	for written, event := range []int64{first.event, first.event, second.event, graphstore.NoEvent} {
		var at time.Time
		if err := writes.database.Pool.QueryRow(ctx, `
			select created_at from comment_deliveries where comment_id = $1::uuid and target = $2 and attempt = $3
		`, attempts[written].comment.id, attempts[written].target, attempts[written].attempt).Scan(&at); err != nil {
			t.Fatalf("read the moment of attempt %d: %v", written, err)
		}
		want = append(want, innerTuple{from: attempt(written), kind: graphstore.AttemptState, to: "value:pending", event: event, at: at})
	}
	requireTuples(t, writes.database, "the four attempts' states", []string{graphstore.AttemptState},
		[]string{attempt(0), attempt(1), attempt(2), attempt(3)}, want...)
}

// Three asks, written as the release before this one writes them. An open one, created with no
// marker on an issue carol holds: its asker takes part under its ask.opened, and carol, whom it
// waits on, takes part and is delivered it, at its created_at, and its asker is delivered nothing.
// One answered by bob after dave commented on it: under its ask.answered, read from that event,
// bob takes part and alice and dave, who wrote in its thread before him, are delivered his answer
// at its moment; dave's comment, under its own event, reached alice, who asked before it. And one
// this release created, which a person of the release before declined: the decliner takes part,
// under the ask.resolved and at the resolution's moment. Each ask is marked at the moment of the
// answer it holds, or at its created_at.
func TestTheBackfillStatesWhatAnOlderBuildsAsksAnswersAndDeclinesOwe(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	if _, err := writes.database.Pool.Exec(ctx, `update issues set assignee = 'carol' where key = 'CENS-1'`); err != nil {
		t.Fatalf("hand the issue to carol: %v", err)
	}
	open := writeAsAnOlderBuild(t, writes, "ask.opened", alice, `
		insert into asks (issue_key, author, question) values ('CENS-1', $1, 'Which day?')
		returning id::text, created_at`, actorJSON(t, alice))
	answered := writeAsAnOlderBuild(t, writes, "ask.opened", alice, `
		insert into asks (issue_key, author, question) values ('CENS-1', $1, 'Which colour?')
		returning id::text, created_at`, actorJSON(t, alice))
	commented := writeAsAnOlderBuild(t, writes, "comment.created", dave, `
		insert into comments (issue_key, author, body, ask_id) values ('CENS-1', $1, 'Blue, I think.', $2)
		returning id::text, created_at`, actorJSON(t, dave), answered.id)
	var answerAt time.Time
	answer := writes.write(t, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		if err := tx.QueryRow(ctx, `
			update asks set state = 'answered',
			       answer = jsonb_build_object('user', 'bob', 'selected', '[]'::jsonb, 'text', 'Blue.', 'at', clock_timestamp())
			where id = $1::uuid
			returning (answer->>'at')::timestamptz
		`, answered.id).Scan(&answerAt); err != nil {
			return model.Event{}, err
		}
		text := "Blue."
		return model.Event{IssueKey: new("CENS-1"), Type: "ask.answered", Actor: bob, Payload: model.AskAnsweredEventPayload{
			AskEventPayload: model.AskEventPayload{Ask: model.Ask{ID: answered.id, IssueKey: new("CENS-1"), Author: alice,
				Answer: &model.AskAnswer{User: "bob", Selected: []string{}, Text: &text, At: answerAt}}},
		}}, nil
	})
	declined := writeAsAnOlderBuild(t, writes, "ask.opened", alice, `
		insert into asks (issue_key, author, question, tuples_stored_at) values ('CENS-1', $1, 'Which size?', now())
		returning id::text, created_at`, actorJSON(t, alice))
	var declinedAt time.Time
	decline := writes.write(t, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		if err := tx.QueryRow(ctx, `select clock_timestamp()`).Scan(&declinedAt); err != nil {
			return model.Event{}, err
		}
		return model.Event{IssueKey: new("CENS-1"), Type: "ask.resolved", Actor: dave, Payload: model.AskEventPayload{
			Ask: model.Ask{ID: declined.id, IssueKey: new("CENS-1"), Author: alice,
				Resolution: &model.AskResolution{Kind: model.AskResolutionDeclined, Actor: dave, At: declinedAt}},
		}}, nil
	})

	report := backfillInboxOnce(t, writes.database)
	for kind, rows := range map[string]int64{"ask_tuples": 2, "ask_declines": 1} {
		if done := reportedKind(t, report, kind); done.Rows != rows {
			t.Errorf("the %s pass reports %#v, want %d rows", kind, done, rows)
		}
	}
	ask := func(id string) string { return graphstore.KindAsk + ":" + id }
	person := func(login string) string { return graphstore.KindPerson + ":" + login }
	kinds := []string{graphstore.TupleDeliveredTo, graphstore.TupleParticipatesIn}
	requireTuples(t, writes.database, "the open ask", kinds, []string{ask(open.id)},
		innerTuple{from: person("alice"), kind: graphstore.TupleParticipatesIn, to: ask(open.id), event: open.event, at: open.at},
		innerTuple{from: person("carol"), kind: graphstore.TupleParticipatesIn, to: ask(open.id), event: open.event, at: open.at},
		innerTuple{from: ask(open.id), kind: graphstore.TupleDeliveredTo, to: person("carol"), event: open.event, at: open.at},
	)
	requireTuples(t, writes.database, "the answered ask and the comment on it", kinds,
		[]string{ask(answered.id), graphstore.KindComment + ":" + commented.id},
		innerTuple{from: person("alice"), kind: graphstore.TupleParticipatesIn, to: ask(answered.id), event: answered.event, at: answered.at},
		innerTuple{from: person("dave"), kind: graphstore.TupleParticipatesIn, to: ask(answered.id), event: commented.event, at: commented.at},
		innerTuple{from: graphstore.KindComment + ":" + commented.id, kind: graphstore.TupleDeliveredTo, to: person("alice"), event: commented.event, at: commented.at},
		innerTuple{from: person("bob"), kind: graphstore.TupleParticipatesIn, to: ask(answered.id), event: answer.ID, at: answerAt},
		innerTuple{from: ask(answered.id), kind: graphstore.TupleDeliveredTo, to: person("alice"), event: answer.ID, at: answerAt},
		innerTuple{from: ask(answered.id), kind: graphstore.TupleDeliveredTo, to: person("dave"), event: answer.ID, at: answerAt},
	)
	requireTuples(t, writes.database, "the declined ask", kinds, []string{ask(declined.id)},
		innerTuple{from: person("dave"), kind: graphstore.TupleParticipatesIn, to: ask(declined.id), event: decline.ID, at: declinedAt},
	)
	for id, want := range map[string]time.Time{open.id: open.at, answered.id: answerAt} {
		var marker *time.Time
		if err := writes.database.Pool.QueryRow(ctx, `select tuples_stored_at from asks where id = $1::uuid`, id).Scan(&marker); err != nil {
			t.Fatalf("read the marker of ask %s: %v", id, err)
		}
		if marker == nil || !marker.Equal(want) {
			t.Errorf("ask %s is marked at %v, want %s", id, marker, want)
		}
	}
	// The old trigger raised alice's row by dave's comment and by bob's answer, and dave's by the
	// answer; the backfill marks each at that version.
	requireMarkedAtItsVersion(t, writes.database, "alice", answered.id, 2)
	requireMarkedAtItsVersion(t, writes.database, "dave", answered.id, 1)
	requireMarkedAtItsVersion(t, writes.database, "bob", answered.id, 0)
	requireASecondRunAppendsNothing(t, writes.database)
}

// An ask an older build created whose log holds no ask.opened owes its create's tuples whichever
// of this release's two writers of them reaches it first, and both state them alike: under the
// store's sentinel and at the ask's created_at - its asker's participates_in, and the participates_in
// and the delivered_to of the person it waits on. The backfill states them for one such ask
// (askOpenedOf answers the sentinel); this release's answer to another states them as its catch-up
// (inboxwrite's owedBy), and the answer's update marks that ask, so the backfill would state its
// create by nothing afterwards.
func TestTheAnswerAndTheBackfillStateACreateWithNoEventAlike(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	olderAsk := func(question string) (string, time.Time) {
		t.Helper()
		var id string
		var at time.Time
		if err := writes.database.Pool.QueryRow(ctx, `
			insert into asks (issue_key, author, question, asked_of) values ('CENS-1', $1, $2, 'dana')
			returning id::text, created_at
		`, actorJSON(t, alice), question).Scan(&id, &at); err != nil {
			t.Fatalf("write an ask as the release before does, with no event: %v", err)
		}
		return id, at
	}
	backfilled, backfilledAt := olderAsk("Which day?")
	answered, answeredAt := olderAsk("Which colour?")

	tx, err := writes.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the answer: %v", err)
	}
	defer tx.Rollback(ctx)
	ask, err := inboxwrite.ReadAsk(ctx, tx, answered)
	if err != nil {
		t.Fatalf("read the ask before its answer: %v", err)
	}
	var at time.Time
	if err := tx.QueryRow(ctx, `
		update asks set state = 'answered',
		       answer = jsonb_build_object('user', 'bob', 'selected', '[]'::jsonb, 'text', 'Blue.', 'at', clock_timestamp())
		where id = $1::uuid
		returning (answer->>'at')::timestamptz
	`, answered).Scan(&at); err != nil {
		t.Fatalf("answer the ask: %v", err)
	}
	if _, err := tx.Exec(ctx, `update asks set tuples_stored_at = $2 where id = $1::uuid`, answered, at); err != nil {
		t.Fatalf("mark the ask at its answer: %v", err)
	}
	text := "Blue."
	if err := inboxwrite.Record(ctx, tx, inboxwrite.Answered(ask, model.AskAnswer{User: "bob", Selected: []string{}, Text: &text, At: at}, bob), graphstore.NoEvent); err != nil {
		t.Fatalf("record the answer: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the answer: %v", err)
	}
	backfillInboxOnce(t, writes.database)

	created := func(id string, created time.Time) []innerTuple {
		node := graphstore.KindAsk + ":" + id
		return []innerTuple{
			{from: graphstore.KindPerson + ":alice", kind: graphstore.TupleParticipatesIn, to: node, event: graphstore.NoEvent, at: created},
			{from: graphstore.KindPerson + ":dana", kind: graphstore.TupleParticipatesIn, to: node, event: graphstore.NoEvent, at: created},
			{from: node, kind: graphstore.TupleDeliveredTo, to: graphstore.KindPerson + ":dana", event: graphstore.NoEvent, at: created},
		}
	}
	createdOf := func(id string, created time.Time) []string {
		var stated []string
		for _, tuple := range storedTuplesTouching(t, writes.database, []string{graphstore.TupleParticipatesIn, graphstore.TupleDeliveredTo}, graphstore.KindAsk+":"+id) {
			if strings.Contains(tuple, " @0 at "+created.UTC().Format(time.RFC3339Nano)) {
				stated = append(stated, tuple)
			}
		}
		return stated
	}
	for _, ask := range []struct {
		what string
		id   string
		at   time.Time
	}{{"the backfilled ask", backfilled, backfilledAt}, {"the ask this release answered", answered, answeredAt}} {
		var want []string
		for _, tuple := range created(ask.id, ask.at) {
			want = append(want, tuple.String())
		}
		slices.Sort(want)
		if got := createdOf(ask.id, ask.at); !slices.Equal(got, want) {
			t.Errorf("%s's create holds\n  %s\nwant\n  %s", ask.what, joinLines(got), joinLines(want))
		}
	}
}

// The six read-state tables' rows, written as the release before this one writes them: no
// marker. alice's Reply thread row, read and dismissed, raised by bob's reply; her read mark and
// her Clear of a conversation, the Clear under the raw actor id user_agent_state keys on; a reply
// she read by its id; her dismissal of an Agent thread; and her snooze of an ask. The backfill
// states each as a tuple from her one person node to the thread's root, the session, the reply,
// the root message or the ask, at the mark's own moment, with her row's participation and its
// delivery of bob's reply, and marks each row at that moment, the thread row at its version.
func TestTheBackfillStatesAnOlderBuildsReadStateRows(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	thread := writeAsAnOlderBuild(t, writes, "comment.created", alice, `
		insert into comments (issue_key, author, body) values ('CENS-1', $1, 'Is the copy final?')
		returning id::text, created_at`, actorJSON(t, alice))
	reply := writeAsAnOlderBuild(t, writes, "comment.created", bob, `
		insert into comments (issue_key, author, body, reply_to) values ('CENS-1', $1, 'Nearly.', $2)
		returning id::text, created_at`, actorJSON(t, bob), thread.id)
	root := writeAsAnOlderBuild(t, writes, "message.created", alice, `
		insert into messages (author, body, target) values ($1, 'Can you look?', 'session:s1')
		returning id::text, created_at`, actorJSON(t, alice))
	answer := writeAsAnOlderBuild(t, writes, "message.answered", model.Actor{Kind: "session", ID: "s1"}, `
		insert into messages (author, body, in_reply_to, target) values ('{"kind":"session","id":"s1"}', 'Looking.', $1, 'session:s1')
		returning id::text, created_at`, root.id)
	asked := writeAsAnOlderBuild(t, writes, "ask.opened", bob, `
		insert into asks (issue_key, author, question, tuples_stored_at) values ('CENS-1', $1, 'Ready?', now())
		returning id::text, created_at`, actorJSON(t, bob))

	var readThrough, dismissedAt, threadRead, cleared, agentDismissed, snoozedUntil time.Time
	if err := writes.database.Pool.QueryRow(ctx, `
		update user_threads set read_through = clock_timestamp(), dismissed_at = clock_timestamp()
		where login = 'alice' and thread_id = $1::uuid
		returning read_through, dismissed_at
	`, thread.id).Scan(&readThrough, &dismissedAt); err != nil {
		t.Fatalf("read and dismiss alice's thread as the release before does: %v", err)
	}
	for _, statement := range []struct {
		sql  string
		at   *time.Time
		args []any
	}{
		{`insert into user_agent_read (login, session_id, read_through) values ('alice', 's1', clock_timestamp()) returning read_through`, &threadRead, nil},
		{`insert into user_agent_state (login, session_id, cleared_before) values ('  Alice ', 's1', clock_timestamp()) returning cleared_before`, &cleared, nil},
		{`insert into user_agent_thread_dismissed (login, message_id, dismissed_at) values ('alice', $1::uuid, clock_timestamp()) returning dismissed_at`, &agentDismissed, []any{root.id}},
		{`insert into user_ask_snooze (login, ask_id, snoozed_until) values ('alice', $1::uuid, clock_timestamp() + interval '1 day') returning snoozed_until`, &snoozedUntil, []any{asked.id}},
	} {
		if err := writes.database.Pool.QueryRow(ctx, statement.sql, statement.args...).Scan(statement.at); err != nil {
			t.Fatalf("write a read-state row as the release before does (%s): %v", statement.sql, err)
		}
	}
	if _, err := writes.database.Pool.Exec(ctx, `
		insert into user_agent_reply_read (login, session_id, reply_id) values ('alice', 's1', $1::uuid)
	`, answer.id); err != nil {
		t.Fatalf("read the reply by its id as the release before does: %v", err)
	}

	report := backfillInboxOnce(t, writes.database)
	for kind, rows := range map[string]int64{
		"user_agent_read": 1, "user_agent_state": 1, "user_agent_reply_read": 1,
		"user_agent_thread_dismissed": 1, "user_ask_snooze": 1,
	} {
		if done := reportedKind(t, report, kind); done.Rows != rows {
			t.Errorf("the %s pass reports %#v, want %d rows", kind, done, rows)
		}
	}
	var replyRead time.Time
	if err := writes.database.Pool.QueryRow(ctx, `
		select tuples_stored_at from user_agent_reply_read where login = 'alice' and reply_id = $1::uuid
	`, answer.id).Scan(&replyRead); err != nil {
		t.Fatalf("read the marker of the reply read by its id: %v", err)
	}
	person := graphstore.KindPerson + ":alice"
	marks := []string{graphstore.TupleReadThrough, graphstore.TupleDismissed, graphstore.TupleCleared, graphstore.TupleReplyRead, graphstore.TupleSnoozed}
	requireTuples(t, writes.database, "alice's marks", marks, []string{person, graphstore.KindPerson + ":  alice "},
		innerTuple{from: person, kind: graphstore.TupleReadThrough, to: graphstore.KindComment + ":" + thread.id, event: graphstore.NoEvent, at: readThrough},
		innerTuple{from: person, kind: graphstore.TupleDismissed, to: graphstore.KindComment + ":" + thread.id, event: graphstore.NoEvent, at: dismissedAt},
		innerTuple{from: person, kind: graphstore.TupleReadThrough, to: graphstore.KindSession + ":s1", event: graphstore.NoEvent, at: threadRead},
		innerTuple{from: person, kind: graphstore.TupleCleared, to: graphstore.KindSession + ":s1", event: graphstore.NoEvent, at: cleared},
		innerTuple{from: person, kind: graphstore.TupleReplyRead, to: graphstore.KindMessage + ":" + answer.id, event: graphstore.NoEvent, at: replyRead},
		innerTuple{from: person, kind: graphstore.TupleDismissed, to: graphstore.KindMessage + ":" + root.id, event: graphstore.NoEvent, at: agentDismissed},
		innerTuple{from: person, kind: graphstore.TupleSnoozed, to: graphstore.KindAsk + ":" + asked.id, event: graphstore.NoEvent, at: snoozedUntil},
	)
	requireTuples(t, writes.database, "alice's row of the thread", []string{graphstore.TupleParticipatesIn, graphstore.TupleDeliveredTo},
		[]string{graphstore.KindComment + ":" + thread.id, graphstore.KindComment + ":" + reply.id},
		innerTuple{from: person, kind: graphstore.TupleParticipatesIn, to: graphstore.KindComment + ":" + thread.id, event: thread.event, at: thread.at},
		innerTuple{from: graphstore.KindPerson + ":bob", kind: graphstore.TupleParticipatesIn, to: graphstore.KindComment + ":" + thread.id, event: reply.event, at: reply.at},
		innerTuple{from: graphstore.KindComment + ":" + reply.id, kind: graphstore.TupleDeliveredTo, to: person, event: reply.event, at: reply.at},
	)
	for _, marked := range []struct {
		table, key string
		want       time.Time
		args       []any
	}{
		{"user_agent_read", `login = 'alice' and session_id = 's1'`, threadRead, nil},
		{"user_agent_state", `login = '  Alice ' and session_id = 's1'`, cleared, nil},
		{"user_agent_thread_dismissed", `login = 'alice' and message_id = $1::uuid`, agentDismissed, []any{root.id}},
		{"user_ask_snooze", `login = 'alice' and ask_id = $1::uuid`, snoozedUntil, []any{asked.id}},
	} {
		var marker *time.Time
		if err := writes.database.Pool.QueryRow(ctx, `select tuples_stored_at from `+marked.table+` where `+marked.key, marked.args...).Scan(&marker); err != nil {
			t.Fatalf("read the marker of %s: %v", marked.table, err)
		}
		if marker == nil || !marker.Equal(marked.want) {
			t.Errorf("%s's row is marked at %v, want its mark's moment %s", marked.table, marker, marked.want)
		}
	}
	var spellings int64
	if err := writes.database.Pool.QueryRow(ctx, `
		select count(*) from graph_nodes where kind = 'person' and lower(btrim(id)) = 'alice'
	`).Scan(&spellings); err != nil {
		t.Fatalf("count alice's person nodes: %v", err)
	}
	if spellings != 1 {
		t.Errorf("the store holds %d person nodes of alice, want one, which every spelling of her login meets at", spellings)
	}
	requireMarkedAtItsVersion(t, writes.database, "alice", thread.id, 1)
	requireASecondRunAppendsNothing(t, writes.database)
}

// A thread row this release holds dismissed, which an older build then put back in the Inbox: that
// build's "Move to Inbox" sets the row's dismissed_at to null and states nothing (inbox_dismiss.go's
// inboxRestoreThreads at 99e9da45abf1), and the marker trigger takes the row's mark off (migration
// 20261116160000), so the row comes back to the thread pass with the dismissed tuple still standing.
// The pass marks the row again and retracts that dismissal, at the moment it reads the row and at
// the sentinel, so the store no longer says dismissed where the row does not; a second run appends
// nothing.
func TestTheBackfillRetractsADismissalAnOlderBuildRestored(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	thread := writeAsAnOlderBuild(t, writes, "comment.created", alice, `
		insert into comments (issue_key, author, body) values ('CENS-1', $1, 'Is the copy final?')
		returning id::text, created_at`, actorJSON(t, alice))
	var dismissedAt time.Time
	if err := writes.database.Pool.QueryRow(ctx, `
		update user_threads set dismissed_at = clock_timestamp()
		where login = 'alice' and thread_id = $1::uuid
		returning dismissed_at
	`, thread.id).Scan(&dismissedAt); err != nil {
		t.Fatalf("dismiss alice's thread: %v", err)
	}
	backfillInboxOnce(t, writes.database)
	newestDismissal := func() (asserted bool, at time.Time, event int64) {
		t.Helper()
		if err := writes.database.Pool.QueryRow(ctx, `
			select asserted, at, source_event from graph_tuples
			where from_kind = $1 and from_id = 'alice' and kind = $2 and to_kind = $3 and to_id = $4
			order by seq desc limit 1
		`, graphstore.KindPerson, graphstore.TupleDismissed, graphstore.KindComment, thread.id).Scan(&asserted, &at, &event); err != nil {
			t.Fatalf("read alice's newest dismissed tuple into the thread: %v", err)
		}
		return asserted, at, event
	}
	if asserted, at, _ := newestDismissal(); !asserted || !at.Equal(dismissedAt) {
		t.Fatalf("after the first run alice's dismissal reads asserted %t at %s, want it standing at %s, so this test would prove nothing",
			asserted, at, dismissedAt)
	}

	updateAsAnOlderBuild(t, writes.database, "move alice's thread back to the Inbox as the release before does", `
		update user_threads set dismissed_at = null where login = 'alice' and thread_id = $1::uuid and dismissed_at is not null
	`, thread.id)
	if row := readThreadRow(t, writes.database, "alice", thread.id); row.markedSeq != nil {
		t.Fatalf("the older build's restore left alice's row marked at %d, so the thread pass would not read it", *row.markedSeq)
	}

	report := backfillInboxOnce(t, writes.database)
	if done := reportedKind(t, report, "user_threads"); done.Rows != 1 {
		t.Errorf("the user_threads pass reports %#v, want alice's row marked again", done)
	}
	if asserted, at, event := newestDismissal(); asserted || !at.After(dismissedAt) || event != graphstore.NoEvent {
		t.Errorf("after the restore alice's newest dismissed tuple reads asserted %t at %s under %d, want a retraction after %s at the sentinel",
			asserted, at, event, dismissedAt)
	}
	requireMarkedAtItsVersion(t, writes.database, "alice", thread.id, 0)
	requireASecondRunAppendsNothing(t, writes.database)
}

// updateAsAnOlderBuild runs sql as a build of the release before runs its update: in a transaction
// whose connection does not carry this build's marker writer setting (store.StoreMarkerWriterSetting),
// so each marker trigger takes the mark off the rows it writes.
func updateAsAnOlderBuild(t *testing.T, database *store.Store, what, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin %s: %v", what, err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select set_config($1, 'off', true)`, store.StoreMarkerWriterSetting); err != nil {
		t.Fatalf("take this build's marker writer setting off %s: %v", what, err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %s: %v", what, err)
	}
}

// A comment on a document of issue CENS-1 is a thread whose user_threads rows name the document
// alone, as user_threads_record keeps the comment's own null issue_key, while every writer of that
// thread takes the issue's owner lock (commentOwners). The thread pass's batch over alice's row of
// it takes the same lock: while the batch's transaction is open, a writer of the issue cannot take
// it, so no reply into the thread lands between the batch's catch-up and its row lock. The lock is
// the owner's advisory lock, and the owner's row only while an earlier build is in the fleet
// (store.LockOwners), so a writer is asked for the lock itself (store.TryLockOwners).
func TestTheThreadPassTakesTheIssueOfADocumentsCommentThread(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	var document, thread string
	if err := writes.database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CENS-1', 'CENS', 'spec', 'spec.md', 'doc', false, $1)
		returning id::text
	`, actorJSON(t, alice)).Scan(&document); err != nil {
		t.Fatalf("create CENS-1's document: %v", err)
	}
	if err := writes.database.Pool.QueryRow(ctx, `
		insert into comments (artifact_id, author, body) values ($1::uuid, $2, 'Is the copy final?') returning id::text
	`, document, actorJSON(t, alice)).Scan(&thread); err != nil {
		t.Fatalf("comment on the document as the release before does: %v", err)
	}
	backfillInboxOnce(t, writes.database)
	updateAsAnOlderBuild(t, writes.database, "read alice's thread as the release before does", `
		update user_threads set read_through = clock_timestamp() where login = 'alice' and thread_id = $1::uuid
	`, thread)

	tx, err := writes.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the batch: %v", err)
	}
	defer tx.Rollback(ctx)
	wrote, err := writeUserThreads(ctx, tx, [][]string{{thread, "alice"}})
	if err != nil {
		t.Fatalf("write the batch of alice's row: %v", err)
	}
	if wrote.rows != 1 {
		t.Errorf("the batch marked %d rows, want alice's", wrote.rows)
	}
	writer, err := writes.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin a writer of CENS-1: %v", err)
	}
	defer writer.Rollback(ctx)
	taken, err := store.TryLockOwners(ctx, writer, store.IssueOwner("CENS-1"))
	if err != nil {
		t.Fatalf("try CENS-1's owner lock beside the open batch: %v", err)
	}
	if len(taken) != 0 {
		t.Errorf("a writer of CENS-1 took the issue's owner lock %v beside the open batch, want it held by the batch", taken)
	}
}

// The thread pass's catch-up reads the comments of a batch's threads the pointer pass still owes
// through the comments' indexes - the key, comments_reply_to and comments_ask_id, one for each
// column a comment's thread is read from, or comments_tuples_unstored (migration 20261122100121),
// which holds the comments whose tuples_stored is false alone, the only rows the catch-up can owe -
// and never the whole table, which it would read once a batch of 200 rows. The statement explained
// is the one the batch sends (writeUserThreads, recorded), over a comments table of 2,000 rows this
// release stated and analysed, as a deployment's is: every row names its node, so naming a node
// narrows nothing, and only the thread's own columns, or the marker, pick the few rows a batch owes.
func TestTheThreadCatchUpReadsCommentsThroughTheirIndexes(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	var ids []string
	if err := writes.database.Pool.QueryRow(ctx, `select array_agg(gen_random_uuid()::text) from generate_series(1, 2000)`).Scan(&ids); err != nil {
		t.Fatalf("draw the comments' ids: %v", err)
	}
	nodes := make([]graphstore.Node, 0, len(ids))
	for _, id := range ids {
		nodes = append(nodes, graphstore.Node{
			Kind: graphstore.KindComment, ID: id, Supertag: graphstore.KindComment, Visibility: graphstore.VisibilityProject,
			State: graphstore.StateLive, Project: "CENS", MadeAt: time.Now().UTC(), MadeBy: alice, SourceEvent: graphstore.NoEvent,
		})
	}
	tx, err := writes.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the comments' nodes: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, nodes, nil); err != nil {
		t.Fatalf("write the comments' nodes: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into comments (id, issue_key, author, body, node_kind, node_address, tuples_stored)
		select id::uuid, 'CENS-1', $2, 'Stated.', 'comment', id, true from unnest($1::text[]) as id
	`, ids, actorJSON(t, alice)); err != nil {
		t.Fatalf("write the comments' rows: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the comments: %v", err)
	}
	if _, err := writes.database.Pool.Exec(ctx, `analyze comments`); err != nil {
		t.Fatalf("analyse comments: %v", err)
	}

	recorder := &storetest.StatementRecorder{}
	traced := storetest.Reopened(t, writes.database, func(config *pgxpool.Config) { config.ConnConfig.Tracer = recorder })
	batch, err := traced.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the batch: %v", err)
	}
	defer batch.Rollback(ctx)
	if _, err := writeUserThreads(ctx, batch, [][]string{{ids[0], "alice"}}); err != nil {
		t.Fatalf("write a batch of the thread pass: %v", err)
	}
	var catchUp string
	for _, statement := range recorder.Statements() {
		if strings.Contains(statement, "from comments c") && strings.Contains(statement, "not c.tuples_stored") {
			catchUp = statement
		}
	}
	if catchUp == "" {
		t.Fatalf("the batch read no comments its threads still owe: %q", recorder.Statements())
	}
	plan := planWithoutSeqScans(t, writes.database, catchUp, []string{ids[0]})
	if explaintest.SeqScansRelation(t, plan, "comments") {
		t.Errorf("the catch-up reads comments whole: %s", plan)
	}
	read := map[string]bool{}
	for _, scan := range explaintest.IndexScans(t, plan) {
		if scan.Index == "comments_tuples_unstored" || strings.HasPrefix(scan.Index, "comments") && strings.Contains(scan.Cond, "ANY") {
			read[scan.Index] = true
		}
	}
	if read["comments_tuples_unstored"] {
		return
	}
	for _, index := range []string{"comments_pkey", "comments_reply_to", "comments_ask_id"} {
		if !read[index] {
			t.Errorf("the catch-up reads comments through no keyed scan of %s and not through comments_tuples_unstored (it reads %v): %s", index, read, plan)
		}
	}
}

// roundTrips records every round trip a pool's connections make: a statement sent on its own, or
// one pgx batch with every statement it carried (pgx.BatchTracer).
type roundTrips struct {
	mu    sync.Mutex
	trips [][]string
}

func (r *roundTrips) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trips = append(r.trips, []string{data.SQL})
	return ctx
}

func (r *roundTrips) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *roundTrips) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trips = append(r.trips, []string{})
	return ctx
}

func (r *roundTrips) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	r.mu.Lock()
	defer r.mu.Unlock()
	last := len(r.trips) - 1
	r.trips[last] = append(r.trips[last], data.SQL)
}

func (r *roundTrips) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func (r *roundTrips) since(start int) [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.trips[start:])
}

func (r *roundTrips) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.trips)
}

// A reply an older build wrote is recorded (inboxwrite.RecordOlder) with the lock of the rows it
// reached and the read of the dismissals it ended in one round trip: the two hang on who it
// reached and not on each other, so they go as one pgx batch, the lock first. bob's reply to
// alice's comment reached alice: recording it again takes four round trips - its thread, who it
// reached, that batch, and the deliveries' store write, one statement whose query is its read of
// the nodes reserved for their author, which a tuple to a person may not run from
// (graphstore.QueueToPerson) - where the lock and the read sent one by one took five.
func TestRecordOlderLocksAndReadsTheDismissalsInOneRoundTrip(t *testing.T) {
	writes := newAddressingWrites(t)
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	ctx := context.Background()
	thread := writeAsAnOlderBuild(t, writes, "comment.created", alice, `
		insert into comments (issue_key, author, body) values ('CENS-1', $1, 'Is the copy final?')
		returning id::text, created_at`, actorJSON(t, alice))
	reply := writeAsAnOlderBuild(t, writes, "comment.created", bob, `
		insert into comments (issue_key, author, body, reply_to) values ('CENS-1', $1, 'Nearly.', $2)
		returning id::text, created_at`, actorJSON(t, bob), thread.id)
	backfillInboxOnce(t, writes.database)

	trips := &roundTrips{}
	traced := storetest.Reopened(t, writes.database, func(config *pgxpool.Config) { config.ConnConfig.Tracer = trips })
	tx, err := traced.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	start := trips.count()
	issue := "CENS-1"
	if _, err := inboxwrite.RecordOlder(ctx, tx, inboxwrite.Comment(reply.id, bob, reply.at, &issue, nil, ""), reply.event); err != nil {
		t.Fatalf("record bob's reply: %v", err)
	}
	recorded := trips.since(start)
	locks, storeWrites := 0, 0
	for _, trip := range recorded {
		switch {
		case len(trip) == 2 && strings.Contains(trip[0], "for no key update") && strings.Contains(trip[1], "current.from_id"):
			locks++
		case len(trip) == 1 && strings.Contains(trip[0], "graph_nodes_written") && strings.Contains(trip[0], "n.visibility"):
			storeWrites++
		case len(trip) != 1 || strings.Contains(trip[0], "for no key update"):
			t.Errorf("a round trip of the record carried %q, want one batch of the row lock and then the dismissals' read, and one statement of the deliveries' store write", trip)
		}
	}
	if locks != 1 || storeWrites != 1 || len(recorded) != 4 {
		t.Errorf("recording bob's reply took %d round trips, %d of them the lock's batch and %d the deliveries' write: %q; want 4, one of each",
			len(recorded), locks, storeWrites, recorded)
	}
}

// unanalyzedConversations is how many one-off conversations the linear-pass test seeds: a person's
// message to a session and the session's reply, each pair the shape of the corpus's piles.
const unanalyzedConversations = 1500

// messageReadsPerMessage bounds the rows the whole backfill reads out of messages for each message
// it states: the batch reads, each message's walk up and down its thread, the update that marks
// it, and the other passes' joins through it. Measured at 30 a message over 3,000 messages; a plan
// that reads every unmarked message for each reply read 9,069 a message here.
const messageReadsPerMessage = 40

// storeReadsPerMessage bounds the rows the whole backfill reads out of graph_tuples for each
// message it states: the probes of each message's thread walk, of the persons it reaches and of
// what it states. Measured at 24 a message over 3,000 messages; probes planned on the store's
// statistics as they stood before the passes grew it read 805 a message here.
const storeReadsPerMessage = 60

// The backfill reads each message, and the store for each message, a bounded number of times on a
// table no analyze has ever read, so it is linear in the messages owed. A deploy's migrations add
// the marker columns this child's passes test (messages.node_kind among them), and a table holds
// no statistics for a column until something analyzes it; a plan made without them estimated
// `node_kind is null` at Postgres's default of half a percent, and reachedBefore's walk of a
// message thread then read the whole node_kind-is-null range of messages_node_address - every
// message still unmarked - for each step of the walk, and hashed every message for each reply. And
// the passes grow the store they probe: on statistics taken before they ran, each probe of a
// thread walk read every tuple of its kind. The corpus is written as the release before this one
// writes it, in a database whose tables autovacuum is told to leave alone, so no analyze runs but
// the ones the backfill runs itself, and the test asserts the table holds no statistics when the
// backfill starts. The rows read are Postgres's own counters of each table's scans
// (pg_stat_user_tables, pg_stat_user_indexes), read once the backfill's connections have closed,
// which is when a backend's counters are flushed: they count what the plans read whatever else the
// pod runs, where the backfill's wall time moves with the pod's load (18s over 3,000 messages when
// measured, and 16s on the statistics taken once).
func TestTheMessagesPassIsLinearOnATableNoAnalyzeHasRead(t *testing.T) {
	writes := newAddressingWrites(t)
	ctx := context.Background()
	rows, err := writes.database.Pool.Query(ctx, `
		select c.relname::text from pg_class c
		where c.relnamespace = 'public'::regnamespace and c.relkind = 'r'
		order by 1
	`)
	if err != nil {
		t.Fatalf("read the tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the tables: %v", err)
	}
	for _, table := range tables {
		if _, err := writes.database.Pool.Exec(ctx, `alter table `+pgx.Identifier{table}.Sanitize()+` set (autovacuum_enabled = false)`); err != nil {
			t.Fatalf("keep autovacuum off %s: %v", table, err)
		}
	}
	// A pool of its own for each phase, closed when the phase ends, so the counters of its
	// connections are flushed and read whole.
	phase := func(name string, run func(database *store.Store)) {
		t.Helper()
		pooled := storetest.Reopened(t, writes.database, func(config *pgxpool.Config) {
			config.ConnConfig.RuntimeParams["application_name"] = name
		})
		run(pooled)
		pooled.Pool.Close()
		deadline := time.Now().Add(30 * time.Second)
		for {
			var open int
			if err := writes.database.Pool.QueryRow(ctx, `
				select count(*) from pg_stat_activity where datname = current_database() and application_name = $1
			`, name).Scan(&open); err != nil {
				t.Fatalf("read the connections of %s: %v", name, err)
			}
			if open == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d connections of %s are still open 30s after its pool closed", open, name)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	phase("seed-older-conversations", func(database *store.Store) {
		tag, err := database.Pool.Exec(ctx, `
			with roots as (
				insert into messages (author, body, target, created_at)
				select jsonb_build_object('kind', 'user', 'id', 'person-' || n || '@example.com'), 'A question.',
				       'session:session-' || n, now() - interval '1 day' + n * interval '1 second'
				from generate_series(1, $1::int) n
				returning id, target, created_at
			)
			insert into messages (author, body, target, in_reply_to, created_at)
			select jsonb_build_object('kind', 'session', 'id', substr(roots.target, length('session:') + 1)), 'An answer.',
			       roots.target, roots.id, roots.created_at + interval '30 seconds'
			from roots
		`, unanalyzedConversations)
		if err != nil {
			t.Fatalf("write the conversations as an older build writes them: %v", err)
		}
		if tag.RowsAffected() != unanalyzedConversations {
			t.Fatalf("wrote %d replies, want %d", tag.RowsAffected(), unanalyzedConversations)
		}
	})
	var analyzed, columnStatistics int
	if err := writes.database.Pool.QueryRow(ctx, `
		select (select count(*) from pg_stat_user_tables
		        where relname = 'messages' and (last_analyze is not null or last_autoanalyze is not null)),
		       (select count(*) from pg_stats where schemaname = 'public' and tablename = 'messages')
	`).Scan(&analyzed, &columnStatistics); err != nil {
		t.Fatalf("read the statistics of messages: %v", err)
	}
	if analyzed != 0 || columnStatistics != 0 {
		t.Fatalf("messages was analyzed %d times and holds statistics of %d columns before the backfill, want a table never analyzed",
			analyzed, columnStatistics)
	}
	// reads is the rows the scans of table have read so far.
	reads := func(table string) int64 {
		t.Helper()
		var read int64
		if err := writes.database.Pool.QueryRow(ctx, `
			select (select coalesce(seq_tup_read, 0) from pg_stat_user_tables where relname = $1)
			     + (select coalesce(sum(idx_tup_read), 0) from pg_stat_user_indexes where relname = $1)
		`, table).Scan(&read); err != nil {
			t.Fatalf("read the rows read out of %s: %v", table, err)
		}
		return read
	}
	messagesBefore, storeBefore := reads("messages"), reads("graph_tuples")
	started := time.Now()
	var report BackfillReport
	phase("backfill-older-conversations", func(database *store.Store) {
		report = backfillInboxOnce(t, database)
	})
	took := time.Since(started)
	messages := int64(2 * unanalyzedConversations)
	if done := reportedKind(t, report, "messages"); done.Rows != messages {
		t.Fatalf("the messages pass reports %#v, want %d rows", done, messages)
	}
	read, storeRead := reads("messages")-messagesBefore, reads("graph_tuples")-storeBefore
	t.Logf("the backfill stated %d messages in %s, reading %d rows out of messages (%.1f a message) and %d out of graph_tuples (%.1f a message)",
		messages, took.Round(time.Millisecond), read, float64(read)/float64(messages), storeRead, float64(storeRead)/float64(messages))
	if read > messageReadsPerMessage*messages {
		t.Errorf("the backfill read %d rows out of messages to state %d messages, %.1f a message, above %d: a pass that reads the "+
			"unmarked messages for each reply is quadratic in the messages owed", read, messages, float64(read)/float64(messages),
			messageReadsPerMessage)
	}
	if storeRead > storeReadsPerMessage*messages {
		t.Errorf("the backfill read %d rows out of graph_tuples to state %d messages, %.1f a message, above %d: probes planned on "+
			"statistics the passes outgrew read every tuple of a kind each", storeRead, messages, float64(storeRead)/float64(messages),
			storeReadsPerMessage)
	}
}
