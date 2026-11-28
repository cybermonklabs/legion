package docs

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A settlement's clear of the document's pending-settlement row (clearSettlementPending) acts on
// the editor a closed issue's settlement kept (doc_closed_edit_sources) only with the row it
// deletes. With the row there, a clear keeping an editor keeps that one and a clear keeping none
// forgets the one kept. With the row already gone - deleted by a settlement of the same updates
// that committed first, as when the resumption arms a second settlement behind one whose version
// is queued - it settles nothing owed and leaves the kept editor as the first left it, whatever
// it would keep. This row is the statement's; the settlement race itself is the api piece's to
// stage (INFRA-3149).
func TestAPendingClearThatFindsTheRowGoneLeavesTheKeptEditor(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	bob := model.Actor{Kind: "user", ID: "bob@example.com"}
	keep := func(actor model.Actor) {
		t.Helper()
		encoded, err := encodeActor(&actor)
		if err != nil {
			t.Fatalf("encode %+v: %v", actor, err)
		}
		if _, err := service.store.Pool.Exec(ctx, `
			insert into doc_closed_edit_sources (artifact_id, last_actor) values ($1::uuid, $2::jsonb)
			on conflict (artifact_id) do update set last_actor = excluded.last_actor`, artifactID, encoded); err != nil {
			t.Fatalf("keep %+v as the closed edit source: %v", actor, err)
		}
	}
	owe := func() {
		t.Helper()
		if _, err := service.store.Pool.Exec(ctx,
			`insert into doc_settlements_pending (artifact_id) values ($1::uuid) on conflict do nothing`, artifactID); err != nil {
			t.Fatalf("mark the document owing a settlement: %v", err)
		}
	}
	clearRow := func(kept *model.Actor) {
		t.Helper()
		tx, err := service.store.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the clear: %v", err)
		}
		defer tx.Rollback(ctx)
		if err := lockDocumentRoom(ctx, tx, artifactID); err != nil {
			t.Fatalf("lock the document: %v", err)
		}
		if err := clearSettlementPending(ctx, tx, artifactID, kept); err != nil {
			t.Fatalf("clear the pending settlement keeping %+v: %v", kept, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the clear: %v", err)
		}
	}
	requireKept := func(want *model.Actor, after string) {
		t.Helper()
		kept := keptClosedEditSource(t, service, artifactID)
		if (kept == nil) != (want == nil) || (kept != nil && !kept.SameAs(*want)) {
			t.Fatalf("after %s the closed edit source is %+v, want %+v", after, kept, want)
		}
	}

	keep(alice)
	owe()
	clearRow(&bob)
	requireKept(&bob, "a clear of the pending row keeping bob")
	owe()
	clearRow(nil)
	requireKept(nil, "a clear of the pending row keeping no editor")
	if owed, err := settlementPending(ctx, service.store.Pool, artifactID); err != nil || owed {
		t.Fatalf("after the clears the document owes %t (%v), want its pending row gone", owed, err)
	}

	keep(alice)
	clearRow(nil)
	requireKept(&alice, "a clear keeping no editor that found the pending row gone")
	clearRow(&bob)
	requireKept(&alice, "a clear keeping bob that found the pending row gone")
}
