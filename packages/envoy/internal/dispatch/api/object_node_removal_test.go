package api

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A document's node is addressed by its ref_key, which a later document of the same issue and the
// same slug takes again. The removal of a document states its node gone, in the removal's own
// transaction and caused by its artifact.removed event, so no read takes a removed document for a
// live one, and the document that later takes the address finds the node gone: its create moves
// it back to live and states that under its own artifact.created event, rather than finding a
// live node that states nothing and keeps the removed document's cause.
//
// Its control is the removal's node write taken out: the node then stays live after the removal,
// and the new document's node keeps the removed one's event.
func TestARemovedDocumentsNodeIsGoneAndItsAddressIsTakenAgain(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "RMV", "A document removed and made again", "The spec.\n")

	first := emptyDocument(t, handler, issue.Key, "Scratch.md")
	created := artifactEvent(t, database, "artifact.created", first.ID)
	requireDocumentNode(t, database, first.RefKey, graphstore.StateLive, created)

	removedDocument(t, handler, first.ID)
	removed := artifactEvent(t, database, "artifact.removed", first.ID)
	requireDocumentNode(t, database, first.RefKey, graphstore.StateGone, removed)

	second := emptyDocument(t, handler, issue.Key, "Scratch.md")
	if second.RefKey != first.RefKey {
		t.Fatalf("the second document took the address %q, want the removed one's %q, so this test proves nothing", second.RefKey, first.RefKey)
	}
	remade := artifactEvent(t, database, "artifact.created", second.ID)
	requireDocumentNode(t, database, second.RefKey, graphstore.StateLive, remade)

	history, err := graphstore.History(context.Background(), database.Pool, graphstore.KindArtifact, first.RefKey, graphstore.AttributeState)
	if err != nil {
		t.Fatalf("read the state history of %s: %v", first.RefKey, err)
	}
	var stated []string
	for _, tuple := range history {
		stated = append(stated, tuple.ToID)
		if tuple.SourceEvent != created && tuple.SourceEvent != removed && tuple.SourceEvent != remade {
			t.Errorf("a state tuple of %s names event %d, which is none of the three writes", first.RefKey, tuple.SourceEvent)
		}
	}
	if want := []string{graphstore.StateLive, graphstore.StateGone, graphstore.StateLive}; !equalStrings(stated, want) {
		t.Errorf("the node of %s states %v over its life, want %v", first.RefKey, stated, want)
	}
}

// A document a build before this one created has no node until the backfill writes it, and its
// removal can come first. The removal then writes the node the document's create would have
// written - live, made by the document's creator at its created_at, at the sentinel, as the
// backfill writes it - and states it gone under its own event, by the person who removed it and
// when: the node keeps its made_at and made_by over its life, so a node the removal made would
// otherwise carry the remover as its maker, and so would the document that takes the address
// next.
//
// Its control is the removal writing the gone node alone, made by the remover at the removal's
// time: the node is then made by alice now, not by bob at the row's created_at.
//
// The removal reads the time of its gone statement before its first write, so before the first
// node it writes, which is dated at the row's created_at. A row of an earlier build dated past the
// database's clock - a clock that stepped back, or a created_at the row was given - runs too: the
// gone statement is dated past the live one all the same. Its control is the removal's own date
// of the gone statement taken alone, which then states the node gone an hour before it was live.
func TestARemovalOfADocumentWithNoNodeDatesTheNodeAtItsCreation(t *testing.T) {
	for _, row := range []struct {
		name    string
		created func() time.Time
	}{
		{"a row of the past", func() time.Time { return time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC) }},
		{"a row dated past the database's clock", func() time.Time { return time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond) }},
	} {
		t.Run(row.name, func(t *testing.T) {
			handler, database := newTestHandlerWithStore(t)
			ctx := context.Background()
			issue := createInteractionIssue(t, handler, "RMN", "A document an earlier build made", "The spec.\n")
			document := emptyDocument(t, handler, issue.Key, "Scratch.md")
			created := row.created()
			// The row a build before this one wrote: made by bob at created, with no node.
			if _, err := database.Pool.Exec(ctx, `
				with tuples as (delete from graph_tuples where (from_kind = 'artifact' and from_id = $1) or (to_kind = 'artifact' and to_id = $1))
				delete from graph_nodes where kind = 'artifact' and id = $1
			`, document.RefKey); err != nil {
				t.Fatalf("take the document's node away: %v", err)
			}
			if _, err := database.Pool.Exec(ctx, `
				update artifacts set created_by = '{"kind":"user","id":"bob"}', created_at = $2 where id = $1
			`, document.ID, created); err != nil {
				t.Fatalf("make the document an earlier build's: %v", err)
			}

			removedDocument(t, handler, document.ID)
			removed := artifactEvent(t, database, "artifact.removed", document.ID)
			requireDocumentNode(t, database, document.RefKey, graphstore.StateGone, removed)
			node, _, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindArtifact, document.RefKey)
			if err != nil {
				t.Fatalf("read the node of %s: %v", document.RefKey, err)
			}
			if !node.MadeAt.Equal(created) || node.MadeBy.ID != "bob" {
				t.Errorf("the node of %s is made by %s at %s, want bob at the row's created_at %s", document.RefKey, node.MadeBy.ID, node.MadeAt, created)
			}
			history, err := graphstore.History(ctx, database.Pool, graphstore.KindArtifact, document.RefKey, graphstore.AttributeState)
			if err != nil || len(history) != 2 {
				t.Fatalf("read the state history of %s: %d tuple(s), %v; want live then gone", document.RefKey, len(history), err)
			}
			if live := history[0]; live.ToID != graphstore.StateLive || live.SourceEvent != graphstore.NoEvent || !live.At.Equal(created) || live.Who.ID != "bob" {
				t.Errorf("the first state tuple of %s is %+v, want live at the sentinel, at the row's created_at and by bob", document.RefKey, live)
			}
			if gone := history[1]; gone.Who.ID != "alice" || !gone.At.After(created) {
				t.Errorf("the gone tuple of %s is %+v, want it by alice, the person who removed it, past the row's created_at %s", document.RefKey, gone, created)
			}
		})
	}
}

// requireDocumentNode fails unless the document node at refKey is in state, caused by event, and
// states that state as its newest state tuple under the same event.
func requireDocumentNode(t *testing.T, database *store.Store, refKey, state string, event int64) {
	t.Helper()
	ctx := context.Background()
	node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindArtifact, refKey)
	if err != nil || !held {
		t.Fatalf("the store holds no node of %s (held %t, %v)", refKey, held, err)
	}
	if node.State != state || node.SourceEvent != event {
		t.Errorf("the node of %s is %s and caused by event %d, want %s and event %d", refKey, node.State, node.SourceEvent, state, event)
	}
	history, err := graphstore.History(ctx, database.Pool, graphstore.KindArtifact, refKey, graphstore.AttributeState)
	if err != nil || len(history) == 0 {
		t.Fatalf("read the state history of %s: %d tuple(s), %v", refKey, len(history), err)
	}
	if newest := history[len(history)-1]; newest.ToID != state || newest.SourceEvent != event {
		t.Errorf("the newest state tuple of %s states %s under event %d, want %s under event %d", refKey, newest.ToID, newest.SourceEvent, state, event)
	}
}

// artifactEvent is the id of the newest event of eventType whose payload carries the artifact id.
func artifactEvent(t *testing.T, database *store.Store, eventType, artifactID string) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `
		select id from events where type = $1 and payload->'artifact'->>'id' = $2 order by id desc limit 1
	`, eventType, artifactID).Scan(&id); err != nil {
		t.Fatalf("read the %s event of %s: %v", eventType, artifactID, err)
	}
	return id
}
