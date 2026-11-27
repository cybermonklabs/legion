package graphcensus

import (
	"context"
	"testing"

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
