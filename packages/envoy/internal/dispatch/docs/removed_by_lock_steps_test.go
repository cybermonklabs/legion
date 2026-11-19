package docs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestAPersonsRemovalOfAnAnsweredAskTakesTheStoreLockOrder drives the write of a person's removal
// of an answered ask's block (graphstore.TupleRemovedBy, planned by removalsOf into the version's
// block index), along the edit path that reaches it, with the machinery of
// TestEveryDocsStoreWriterTakesTheStoreLockOrder: the path's steps are checked against the order
// written on api/inbox_read.go's markInboxReadThreads (requireTheDocsOrderAdmits), and each is held
// in turn while the path's backend is read at the statement it waits at and at the table locks it
// holds (requireEachDocsStepIsWaitedAtInTurn).
//
// The path is a person's edit that deletes the block of an answered ask, versioned in the same
// transaction as POST /api/v1/artifacts/{id}/edits versions it: under the issue's owner lock, the
// version's insert takes the document's fence row, its block index writes the gone block's node and
// the person's node in its one node statement (the removals ride in the index's plan, refs'
// beside), and takes the document's block rows and tie rows out; the artifact.version event
// follows, and what the index stated and the removed_by tuple are appended under it in one
// statement (graphstore.QueueCaused).
func TestAPersonsRemovalOfAnAnsweredAskTakesTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	rig := newDocsWriterRig(t)
	writer := rig.personsRemoval()
	requireTheDocsOrderAdmits(t, writer)
	requireEachDocsStepIsWaitedAtInTurn(t, ctx, rig, writer)
}

// personsRemoval is a person's edit that deletes the block of an answered ask of the rig's asked
// document. Each run first writes the block as a session's edit with its version, settles it,
// which opens its ask, and answers the ask; the run's person is new to it, so the person's node is
// held as a row another transaction inserted and has not committed, in the block index's node
// statement. The fence row is there from the document's first version, so it is held as a row; the
// block statement's node, rows and the event and the tuples are held as their tables.
func (r *docsWriterRig) personsRemoval() docsWriter {
	return docsWriter{
		name: "removalsOf after a person's edit",
		steps: []docsStep{
			r.ownerStep(),
			{
				name: "the revision fence", order: docsFenceStep, hold: holdFenceRow, at: "with head as (select ",
				takes: []docsTableLock{{"document_block_versions", docsRowExclusiveMode}},
			},
			docsBlockNodesStep(),
			{
				name: "the person's node", order: docsNodeStep, fenced: true,
				hold: holdDocsNewNode, at: docsNodeStatement,
				takes: []docsTableLock{{"graph_nodes", docsRowExclusiveMode}},
			},
			{
				name: "the block rows", order: docsProjectionStep, names: "the block nodes", fenced: true,
				hold: holdDocsTableWrites("document_blocks"), at: "delete from document_blocks",
				takes: []docsTableLock{{"document_blocks", docsRowExclusiveMode}},
			},
			{
				name: "the tie rows", order: docsProjectionStep, names: "the block nodes", fenced: true,
				hold: holdDocsTableWrites("block_refs"), at: "delete from block_refs",
				takes: []docsTableLock{{"block_refs", docsRowExclusiveMode}},
			},
			docsEventAppendStep(),
			{
				name: "the block index's tuples and the removal", order: docsTupleStep, caused: true, fenced: true,
				hold: holdDocsTableWrites("graph_tuples"), at: docsTupleStatement,
				takes: []docsTableLock{{"graph_tuples", docsRowExclusiveMode}},
			},
		},
		ready: func(t *testing.T, ctx context.Context) docsRun {
			block := r.fresh("lock-removed")
			r.editAsVersion(t, ctx, r.asked, model.Actor{Kind: "session", ID: r.fresh("lock-steps-asker")}, askBlockMarkdown(block))
			r.awaitDurable(t, r.asked)
			if err := r.settle(ctx, r.asked); err != nil {
				t.Fatalf("settle the ask block %s: %v", block, err)
			}
			answerIndexedAsk(t, r.service, r.asked, block)
			r.awaitDurable(t, r.asked)
			login := r.fresh("lock-remover")
			person := model.Actor{Kind: "user", ID: login}
			return docsRun{
				document: r.asked,
				made:     graphstore.PersonNode(login, person, time.Now().UTC(), graphstore.NoEvent),
				write: func() error {
					return r.removeAsPerson(ctx, person, block)
				},
			}
		},
	}
}

// removeAsPerson deletes block from the asked document as person's edit and versions it in the
// same transaction, as POST /api/v1/artifacts/{id}/edits does, queueing the version's event and
// what the version stated under it as that route does, and answers nil where the ask of the block
// then holds one asserted removed_by to that person.
func (r *docsWriterRig) removeAsPerson(ctx context.Context, person model.Actor, block string) error {
	tx, err := r.service.store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	joined, ledger := r.service.Join(ctx, tx)
	defer ledger.Discard()
	if edit, err := r.service.ApplyOps(joined, r.asked, []model.EditOp{{Op: "delete", Block: block}}, person, nil); err != nil || !edit.Changed {
		return fmt.Errorf("the person's delete of %s (changed %v): %w", block, edit.Changed, err)
	}
	written, err := r.service.SnapshotVersion(joined, r.asked, person)
	if err != nil || !written.Wrote {
		return fmt.Errorf("version the delete of %s (wrote %v): %w", block, written.Wrote, err)
	}
	issue := "CORE-1"
	var event int64
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		if err := r.service.events.QueueAppend(b, model.Event{
			IssueKey: &issue, Type: "artifact.version", Actor: person,
			Payload: map[string]any{"artifact_id": r.asked, "version": map[string]any{"number": written.Version.Number}},
		}, func(appended model.Event) { event = appended.ID }); err != nil {
			return err
		}
		return graphstore.QueueCaused(ctx, b, written.Blocks, graphstore.LatestEvent)
	}); err != nil {
		return err
	}
	if err := ledger.Commit(ctx); err != nil {
		return err
	}
	var removals int
	if err := r.database.Pool.QueryRow(ctx, `
		select count(*) from graph_tuples t join asks k on t.from_id = k.id::text
		where k.block_artifact_id = $1 and k.block_id = $2 and t.from_kind = $3 and t.kind = $4
		  and t.to_kind = $5 and t.to_id = $6 and t.asserted and t.source_event = $7
	`, r.asked, block, graphstore.KindAsk, graphstore.TupleRemovedBy, graphstore.KindPerson, model.CanonicalLogin(person.ID), event).Scan(&removals); err != nil {
		return err
	}
	if removals != 1 {
		return fmt.Errorf("the ask of %s holds %d removals by %s under the version's event, want one", block, removals, person.ID)
	}
	return nil
}
