package graphcensus

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Each event that leaves the store what the object pass cures owes the pass, read through that
// event alone: issue.created for an issue and for its spec with no node, artifact.created for a
// document with none, ask.opened for an ask, message.created for a message, and artifact.removed
// for a document whose node stands behind no row. Once the pass has cured it the same event owes
// nothing, an event past the range read owes nothing, and an event of another type owes nothing
// whatever its row holds.
//
// Each case leaves one row or node to cure, beside rows and nodes that all agree, so a case fails
// alone where its event's arm of eventsOweObjectPass is taken out.
func TestAnEventOwesTheObjectPassOnlyWhileItLeavesARowAndItsNodeApart(t *testing.T) {
	database := storetest.Open(t)
	executeStatements(t, database, `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`)
	writeEveryOwedNode(t, database)
	// removedRef is the address of the document the "a document" case makes and the removal case
	// removes.
	var removedRef string
	for _, event := range []struct {
		name string
		// write leaves the store one row with no node or one node with no row, and appends its
		// event, in one transaction, as a build before this one does.
		write func(t *testing.T)
		// cure is the pass's part that cures it, nil for writeEveryOwedNode.
		cure func(t *testing.T)
		owed bool
	}{
		{name: "an issue", owed: true, write: func(t *testing.T) {
			executeStatements(t, database, `
				insert into issues (key, project_key, number, title, created_by, rank)
				values ('CORE-2', 'CORE', 2, 'Two', '{"kind":"user","id":"alice"}', 'V');
				insert into events (issue_key, seq, type, actor, payload, notify)
				values ('CORE-2', 1, 'issue.created', '{"kind":"user","id":"alice"}', '{"key":"CORE-2"}', false);
			`)
		}},
		{name: "the spec of an issue that holds its node", owed: true, write: func(t *testing.T) {
			executeStatements(t, database, `
				insert into issues (key, project_key, number, title, created_by, rank)
				values ('CORE-3', 'CORE', 3, 'Three', '{"kind":"user","id":"alice"}', 'W');
			`)
			writeEveryOwedNode(t, database)
			executeStatements(t, database, `
				insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
				values ('CORE-3', 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}');
				insert into events (issue_key, seq, type, actor, payload, notify)
				values ('CORE-3', 1, 'issue.created', '{"kind":"user","id":"alice"}', '{"key":"CORE-3"}', false);
			`)
		}},
		{name: "a document", owed: true, write: func(t *testing.T) {
			executeStatements(t, database, `
				with document as (
					insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
					values ('CORE-1', 'CORE', 'notes', 'notes.md', 'doc', false, '{"kind":"user","id":"alice"}')
					returning id
				)
				insert into events (issue_key, seq, type, actor, payload, notify)
				select 'CORE-1', 1, 'artifact.created', '{"kind":"user","id":"alice"}',
				       jsonb_build_object('artifact', jsonb_build_object('id', document.id::text, 'issue_key', 'CORE-1')), false
				from document;
			`)
		}},
		{name: "an ask", owed: true, write: func(t *testing.T) {
			executeStatements(t, database, `
				with ask as (
					insert into asks (issue_key, author, question) values ('CORE-1', '{"kind":"user","id":"alice"}', 'Owed?')
					returning id
				)
				insert into events (issue_key, seq, type, actor, payload, notify)
				select 'CORE-1', 2, 'ask.opened', '{"kind":"user","id":"alice"}', jsonb_build_object('id', ask.id::text), false
				from ask;
			`)
		}},
		{name: "a message", owed: true, write: func(t *testing.T) {
			executeStatements(t, database, `
				with message as (
					insert into messages (issue_key, author, body, target) values (null, '{"kind":"user","id":"alice"}', 'Owed.', 'session:ses_owed')
					returning id
				)
				insert into events (seq, type, actor, payload, notify)
				select 0, 'message.created', '{"kind":"user","id":"alice"}', jsonb_build_object('id', message.id::text), false
				from message;
			`)
		}},
		{name: "a reply to a message", owed: true, write: func(t *testing.T) {
			// The message the "a message" case made holds its node by now. A session's reply keeps
			// its parent's target (api's insertSessionReply) and appends message.answered, not
			// message.created.
			executeStatements(t, database, `
				with reply as (
					insert into messages (issue_key, author, body, target, in_reply_to)
					select null, '{"kind":"session","id":"ses_owed"}', 'Answered.', target, id
					from messages where body = 'Owed.'
					returning id
				)
				insert into events (seq, type, actor, payload, notify)
				select 0, 'message.answered', '{"kind":"session","id":"ses_owed"}', jsonb_build_object('id', reply.id::text), false
				from reply;
			`)
		}},
		{name: "a removed document whose node stands", owed: true, write: func(t *testing.T) {
			// The document the "a document" case made holds its node by now.
			if err := database.Pool.QueryRow(context.Background(), `
				with removed as (
					delete from artifacts where issue_key = 'CORE-1' and slug = 'notes' returning id, ref_key
				)
				insert into events (issue_key, seq, type, actor, payload, notify)
				select 'CORE-1', 3, 'artifact.removed', '{"kind":"user","id":"alice"}',
				       jsonb_build_object('artifact', jsonb_build_object('id', removed.id::text, 'issue_key', 'CORE-1', 'ref_key', removed.ref_key)), false
				from removed
				returning payload->'artifact'->>'ref_key'
			`).Scan(&removedRef); err != nil {
				t.Fatalf("remove document notes of CORE-1 as a build before this one does: %v", err)
			}
		}, cure: func(t *testing.T) {
			if err := StateOrphanedGone(context.Background(), database, removedRef); err != nil {
				t.Fatalf("state the orphaned node of %s gone: %v", removedRef, err)
			}
		}},
		{name: "an issue another event than its create names", owed: false, write: func(t *testing.T) {
			executeStatements(t, database, `
				insert into issues (key, project_key, number, title, created_by, rank)
				values ('CORE-4', 'CORE', 4, 'Four', '{"kind":"user","id":"alice"}', 'X');
				insert into events (issue_key, seq, type, actor, payload, notify)
				values ('CORE-4', 1, 'issue.updated', '{"kind":"user","id":"alice"}', '{"key":"CORE-4"}', false);
			`)
		}},
	} {
		t.Run(event.name, func(t *testing.T) {
			before := newestEventID(t, database)
			event.write(t)
			after := newestEventID(t, database)
			requireObjectPassOwed(t, database, before, after, event.owed, "with the row and its node apart")
			requireObjectPassOwed(t, database, after, after+1000, false, "past the event")
			if event.cure != nil {
				event.cure(t)
			}
			writeEveryOwedNode(t, database)
			requireObjectPassOwed(t, database, before, after, false, "once the object pass cured it")
		})
	}
}

// A row whose node the store holds but whose row names it nowhere is owed its name alone, and its
// batch costs a transaction, the owner locks and a write of each node all the same, so the object
// pass bounds such batches by a budget of their own, as many batches as its budget of nodes: the
// first pass after this build deploys over a build that names no node on its row reads every issue
// and document of the deployment as such a row. Over two batches of issues owed their name alone
// and, after them, one issue owed its node, a pass of one batch names the first batch, leaves the
// second for the next run, still writes the owed issue's node, and answers More, in two
// transactions; the next pass names the batch left, in one, and answers no More.
//
// A pass whose batches owed names alone spent no budget would read otherwise: its first run would
// name both batches, in three transactions, and answer no More. The first run's More and its two
// transactions are what show the naming budget spent.
func TestRowsOwedTheirNameAloneSpendANamingBudgetOfTheirOwn(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	named := 2 * backfillBatch
	nodes := make([]graphstore.Node, named)
	for index := range nodes {
		nodes[index] = graphstore.IssueNode(fmt.Sprintf("NAME-%d", index+1), "NAME", alice, time.Now().UTC())
	}
	executeStatements(t, database, `
		insert into projects (key, name, default_assignee) values ('NAME', 'Named', 'alice'), ('OWED', 'Owed', 'alice')
	`)
	// Each issue of NAME holds its node and names it nowhere, as a build that names no node on its
	// row writes them, and OWED-1, whose key sorts after theirs, holds none, as the release before
	// writes it.
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := graphstore.Write(ctx, tx, nodes, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			select 'NAME-' || n, 'NAME', n, 'Named', '{"kind":"user","id":"alice"}'::jsonb, 'U' from generate_series(1, $1::integer) n
			union all
			select 'OWED-1', 'OWED', 1, 'Owed', '{"kind":"user","id":"alice"}'::jsonb, 'U'
		`, named)
		return err
	}); err != nil {
		t.Fatalf("seed the issues owed their name alone and the issue owed its node: %v", err)
	}
	begins := &beginCounter{}
	traced := storetest.TracedStore(t, database, begins)
	for run, want := range []struct {
		rows         int64
		more         bool
		transactions int64
		named        int
	}{
		{rows: 1, more: true, transactions: 2, named: backfillBatch},
		{rows: 0, more: false, transactions: 1, named: named},
	} {
		begins.count.Store(0)
		pass, err := WriteOwedObjectNodes(ctx, traced, 1)
		if err != nil || pass.Rows != want.rows || pass.More != want.more {
			t.Fatalf("pass %d of one batch answered %+v (%v), want %d row(s) and More %t", run+1, pass, err, want.rows, want.more)
		}
		if opened := begins.count.Load(); opened != want.transactions {
			t.Errorf("pass %d opened %d transaction(s), want %d: one a batch of each budget at most", run+1, opened, want.transactions)
		}
		var rows int
		if err := database.Pool.QueryRow(ctx, `select count(*) from issues where project_key = 'NAME' and node_kind is not null`).Scan(&rows); err != nil || rows != want.named {
			t.Errorf("after pass %d %d issue(s) of NAME name their node (%v), want %d", run+1, rows, err, want.named)
		}
		if _, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindIssue, "OWED-1"); err != nil || !held {
			t.Errorf("after pass %d the store holds no node of OWED-1 (held %t, %v): the names held back the node behind them", run+1, held, err)
		}
	}
	requireComplete(t, ctx, database, "after the two passes")
}

// executeStatements runs statements on the pool, as one implicit transaction, failing the test
// where they fail.
func executeStatements(t *testing.T, database *store.Store, statements string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), statements); err != nil {
		t.Fatalf("%v:\n%s", err, statements)
	}
}

// writeEveryOwedNode runs the object pass with no bound, so every row of the four kinds holds its
// node.
func writeEveryOwedNode(t *testing.T, database *store.Store) {
	t.Helper()
	if pass, err := WriteOwedObjectNodes(context.Background(), database, 0); err != nil || pass.More || pass.Moved != nil {
		t.Fatalf("write every owed node: %+v, %v", pass, err)
	}
}

// newestEventID is the id of the newest event the log holds, 0 where it holds none.
func newestEventID(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var newest int64
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(max(id), 0) from events`).Scan(&newest); err != nil {
		t.Fatalf("read the newest event: %v", err)
	}
	return newest
}

// requireObjectPassOwed fails unless EventsOweObjectPass over (after, through] answers want.
func requireObjectPassOwed(t *testing.T, database *store.Store, after, through int64, want bool, when string) {
	t.Helper()
	owed, err := EventsOweObjectPass(context.Background(), database.Pool, after, through)
	if err != nil || owed != want {
		t.Errorf("the events in (%d, %d] owe the object pass %t (%v) %s, want %t", after, through, owed, err, when, want)
	}
}
