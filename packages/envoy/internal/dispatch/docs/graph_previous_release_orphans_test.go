package docs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The release before this one removes a document whose node this build's tick wrote during the
// roll: its removal states nothing, so the node stands live behind no row, which the project
// census names orphaned. The roll's next tick states that node gone, as this build's own removal
// states it, and the census then reads the store complete.
//
// Its control is the tick's sweep taken out of writeObjectNodesAnotherBuildWrote: the node then
// stays live, and the census names it orphaned.
func TestARollTickStatesGoneTheNodeOfADocumentThePreviousReleaseRemoved(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	registerProcess(t, database, previousReleaseCommit)
	ref := uploadAsThePreviousRelease(t, database, "scratch-md", "Scratch.md")
	if tick := awaitedBlockIndexTick(t, service); !tick.AnotherBuild || tick.ObjectNodes < 1 {
		t.Fatalf("the roll's first tick wrote %d object node(s) and found another build %t, want the document's node", tick.ObjectNodes, tick.AnotherBuild)
	}
	removeAsThePreviousRelease(t, database, ref)
	requireProjectComplete(t, database, []string{"artifact " + ref + ": orphaned"}, "after the release before removed the document")
	if tick := awaitedBlockIndexTick(t, service); tick.OrphansGone != 1 {
		t.Errorf("the roll's next tick stated %d orphaned node(s) gone, want the removed document's", tick.OrphansGone)
	}
	node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindArtifact, ref)
	if err != nil || !held || node.State != graphstore.StateGone {
		t.Errorf("after the tick the node of %s is %+v (held %t, %v), want it gone", ref, node, held, err)
	}
	requireProjectComplete(t, database, nil, "after the roll's tick stated the orphan gone")
}

// The build's debt of the object pass is in its cursor row, so whichever process of the build
// holds the lease next runs the pass: a process's first tick finds 25 orphaned documents, states
// the bound of 20 gone, keeps the debt and is full; a tick of another process of the build, one
// that finished a pass of its own a moment ago, so that only the debt owes it the pass, reads that
// debt, states the 5 left gone and clears it; and a document removed after that, with nothing
// owed, is owed by its removal's event, so the next tick states it gone.
//
// Its control is the debt cleared whatever the pass left: the second process's tick then runs no
// pass, and the 5 stay orphaned.
func TestTheObjectPassDebtCrossesProcessesAndClearsOnceThePassFinishes(t *testing.T) {
	database, first := blockIndexService(t)
	for n := range orphanedNodesSwept + 5 {
		writeDocumentWithItsNode(t, database, fmt.Sprintf("orphan-%02d-md", n))
	}
	for n := range orphanedNodesSwept + 5 {
		removeAsThePreviousRelease(t, database, fmt.Sprintf("CORE-1/orphan-%02d-md", n))
	}
	joined := awaitedBlockIndexTick(t, first)
	if joined.OrphansGone != orphanedNodesSwept || !joined.Full || objectNodesOwed(t, database) == nil {
		t.Fatalf("the first tick stated %d orphan(s) gone, full %t, owed %v; want the bound of %d, a full tick and the debt kept",
			joined.OrphansGone, joined.Full, objectNodesOwed(t, database), orphanedNodesSwept)
	}
	second := blockIndexProcess(t, database, "")
	second.objectPassFinished.Store(time.Now().UnixNano())
	if tick := awaitedBlockIndexTick(t, second); tick.OrphansGone != 5 || objectNodesOwed(t, database) != nil {
		t.Errorf("the other process's tick stated %d orphan(s) gone and left the debt at %v, want the 5 left and the debt cleared",
			tick.OrphansGone, objectNodesOwed(t, database))
	}
	writeDocumentWithItsNode(t, database, "later-md")
	removeAsThePreviousRelease(t, database, "CORE-1/later-md")
	if tick := awaitedBlockIndexTick(t, second); tick.OrphansGone != 1 {
		t.Errorf("the tick after a removal's event stated %d orphan(s) gone, want the removed document's", tick.OrphansGone)
	}
	requireProjectComplete(t, database, nil, "after the tick the removal's event owed the pass")
}

// A pass a tick runs while another build serves and before any horizon is proven, a debt no cursor
// row records (objectPassDebt), is not the process's pass: the process's first tick with a proven
// horizon runs the object pass however recently that one ran. The release before uploads a
// document, and this build's first tick, on a log that holds no event, so with no horizon proven
// and no cursor row, writes its node. The release before then removes the document, which states
// nothing, so the node stands live behind no row, and leaves before this build ticks again. The
// next tick proves a horizon at the removal's event, starts the build's cursor there, reads no
// event and finds no other build serving: only its being the process's first tick with a proven
// horizon owes it the pass, which states the node gone. The service's clock stands still, so by it
// no time passes between the two ticks.
//
// Its control is the pass before the horizon counted as the process's own (objectPassFinished
// stored above writeObjectNodesAnotherBuildWrote's return for a debt no row records): the tick with
// a proven horizon then runs no pass, and the removed document's node stays live for up to
// objectPassInterval.
func TestTheFirstTickWithAProvenHorizonOwesTheObjectPassThoughATickBeforeAnyHorizonRanIt(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	stopped := time.Now()
	service.now = func() time.Time { return stopped }
	previousLeaves := registerProcess(t, database, previousReleaseCommit)
	ref := uploadAsThePreviousRelease(t, database, "rolled-md", "Rolled.md")

	early := reconcileBlockIndex(t, service, "")
	if !early.AnotherBuild || early.ObjectNodes != 3 {
		t.Fatalf("the tick before any horizon is proven wrote %d object node(s) and found another build %t, want the nodes of CORE-1, CORE-2 and the document and another build",
			early.ObjectNodes, early.AnotherBuild)
	}
	var cursorRow bool
	if err := database.Pool.QueryRow(ctx, `
		select exists (select 1 from block_index_event_cursors where build_commit = '')
	`).Scan(&cursorRow); err != nil || cursorRow {
		t.Fatalf("after the tick before any horizon is proven this build holds a cursor row %t (%v), want none: the tick did not take the path before a horizon, and this test proves nothing",
			cursorRow, err)
	}
	if node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindArtifact, ref); err != nil || !held || node.State != graphstore.StateLive {
		t.Fatalf("after the tick before any horizon is proven the node of %s is %+v (held %t, %v), want it live", ref, node, held, err)
	}

	removeAsThePreviousRelease(t, database, ref)
	requireProjectComplete(t, database, []string{"artifact " + ref + ": orphaned"}, "after the release before removed the document")
	previousLeaves()
	var removal int64
	if err := database.Pool.QueryRow(ctx, `select id from events where type = 'artifact.removed'`).Scan(&removal); err != nil {
		t.Fatalf("read the removal's event: %v", err)
	}

	proven := awaitedBlockIndexTick(t, service)
	if cursor := blockIndexEventCursor(t, database, ""); cursor < removal || proven.Events != 0 || proven.AnotherBuild {
		t.Fatalf("the first tick with a proven horizon started the cursor at %d, read %d event(s) and found another build %t; want the cursor at or past the removal's event %d, no event read and no other build: something else owes the pass, and this test proves nothing",
			cursor, proven.Events, proven.AnotherBuild, removal)
	}
	if proven.OrphansGone != 1 {
		t.Errorf("the first tick with a proven horizon stated %d orphaned node(s) gone, want the removed document's", proven.OrphansGone)
	}
	if node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindArtifact, ref); err != nil || !held || node.State != graphstore.StateGone {
		t.Errorf("after the first tick with a proven horizon the node of %s is %+v (held %t, %v), want it gone", ref, node, held, err)
	}
	requireProjectComplete(t, database, nil, "after the first tick with a proven horizon stated the orphan gone")
}

// writeDocumentWithItsNode writes a document of CORE-1 at slug with its node, as this build's
// upload writes them.
func writeDocumentWithItsNode(t *testing.T, database *store.Store, slug string) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := graphstore.Write(ctx, tx, []graphstore.Node{graphstore.ArtifactNode(
			"CORE-1/"+slug, "CORE", "", model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC())}, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ('CORE-1', 'CORE', $1, $1, 'doc', '{"kind":"user","id":"alice"}')
		`, slug)
		return err
	}); err != nil {
		t.Fatalf("write the document %s with its node: %v", slug, err)
	}
}

// uploadAsThePreviousRelease uploads a document of CORE-1 at slug, named name, as the release
// before this one does: the row alone, with no node. It answers the document's ref_key.
func uploadAsThePreviousRelease(t *testing.T, database *store.Store, slug, name string) string {
	t.Helper()
	var ref string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('CORE-1', 'CORE', $1, $2, 'doc', '{"kind":"user","id":"alice"}')
		returning ref_key
	`, slug, name).Scan(&ref); err != nil {
		t.Fatalf("upload the document as the release before: %v", err)
	}
	return ref
}

// removeAsThePreviousRelease removes the document at ref as the release before this one does: the
// issue's row first, then the row goes and an artifact.removed event naming it is appended, and
// nothing states its node gone.
func removeAsThePreviousRelease(t *testing.T, database *store.Store, ref string) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select 1 from issues where key = 'CORE-1' for no key update`); err != nil {
			return err
		}
		var artifact []byte
		if err := tx.QueryRow(ctx, `
			delete from artifacts where ref_key = $1
			returning jsonb_build_object('id', id::text, 'issue_key', issue_key, 'slug', slug, 'ref_key', ref_key)
		`, ref).Scan(&artifact); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify)
			select 'CORE-1', coalesce(max(seq), 0) + 1, 'artifact.removed', '{"kind":"user","id":"alice"}',
			       jsonb_build_object('artifact', $1::jsonb), false
			from events where issue_key = 'CORE-1'
		`, artifact)
		return err
	}); err != nil {
		t.Fatalf("remove %s as the release before does: %v", ref, err)
	}
}

// objectNodesOwed is the time since which the unstamped build owes the object pass, nil where it
// owes none or holds no cursor row.
func objectNodesOwed(t *testing.T, database *store.Store) *time.Time {
	t.Helper()
	var owed *time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		select (select object_nodes_owed_at from block_index_event_cursors where build_commit = '')
	`).Scan(&owed); err != nil {
		t.Fatalf("read the build's debt of the object pass: %v", err)
	}
	return owed
}
