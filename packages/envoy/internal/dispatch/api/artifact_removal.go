package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// removeArtifact removes a document of an issue that is still untouched: one made by typing a
// title and never written in. It answers 204 and no body.
//
// It needs the person scope (routeScopes) and takes two locks: the owner's lock (store.LockOwner,
// which beginArtifactWrite takes), as the title correction does (renameArtifact), so a correction
// and a removal of one owner cannot each miss the other, and then the document's room
// (docs.LockDocumentRoom), which is the lock a durable write of the document takes. In that
// order, which is the one docs/persistence.go states: the owner's lock first, then the room, as a
// settlement takes them, and a room's own writers take no owner lock, so this joins no lock cycle.
// After both it writes the document's node, whose every writer takes the owner's lock first (the
// store's order, written on markInboxReadThreads).
//
// Every lock it waits for after the owner's lock waits at most artifactRemovalLockTimeout: the
// room's, and each lock its reads and its deletes take, among them the checks of the foreign keys
// to artifacts (id) that the delete of the artifact row runs, each such check a probe of an index
// led by its key's column (this issue's spec R7). A wait past it ends the statement, the
// transaction rolls back with nothing removed, and the route answers as it answers any statement
// that failed.
//
// Five refusals read the artifact row alone: the kind, the role, the issue's spec, a document of
// an issue room and a project's document (artifactRowIsUntouched). The removal answers them from
// the row beginArtifactWrite re-read, before the room's lock. A DELETE of the spec, of a role
// document, of a document of an issue room or of a picture then waits for no room lock that a
// room's batch, a compaction or a snapshot holds, and it frees the issue's owner lock at once for
// every comment, ask and upload of that issue.
//
// What the document holds needs the room's lock and is read under it (artifactIsUntouched),
// inside this transaction and from the row this transaction re-read: a version written, an edit
// typed, or a comment, ask, review, approval request, check result, read mark or link left
// between the request and the lock refuses the removal rather than being deleted with the
// document. An append of another process either committed before the room's lock was taken,
// which refuses the removal, or waits for this transaction and then meets the row that has gone.
//
// The rows go in one transaction: first the document's node and its versions' nodes, stated gone
// (removedDocumentNodes), the document's with each changed_by_run it states now retracted; then
// the rows the artifact's own delete would leave behind or be refused by (artifactRemovalClears)
// and the reference rows package refs keeps for the document (refs.ForgetDocument); then the
// artifact row, which cascades the keys that do cascade; then every lens on the document that the
// delete left stale, with all that stands under each. The artifact.removed event is appended after
// them and outlives the document: an issue document's events are the issue's (events_one_owner,
// ownerForArtifact), so that row carries no artifact id. What the node write states is appended
// under that event, in the batch that appends it (appendCausingEvent). A document made again
// under the same title takes the removed one's ref_key, the id of the same node, and so names no
// run as having changed it but its own, and a run counts no document that has gone, as it lists
// none. The removal holds the document's owner lock, which every writer that marks the
// document's node changed takes first, so no changed_by_run of the node lands between the read of
// those the node states and their retraction.
//
// The document's node is stated gone before its row goes, in the store's order (node, row, event,
// facts), and the state tuple is appended in the batch that appends the artifact.removed event
// (appendCausingEvent).
//
// Each lens invoked on the document records the row it stood on (graphstore.AttributeArtifactID),
// so the delete of that row leaves it stale (refs.StaleSQL), and every read answers it, and all
// that stands under it at every depth, as gone at once. In the transaction that deletes the row,
// after the delete, the removal states each of them gone as the block-index tick's sweep states
// every stale lens gone (graphcensus's StateStaleLensesGone): it reads the stale lenses on the
// document's node (refs.StaleLensesOn) and states them gone as graphstore.PlanState answers them,
// which takes what stands beneath each with it, its findings, its risks and the lenses on those, those
// the person may not see among them, each written again as the store holds it but for its state,
// keeping its cause, with one state tuple at the sentinel signed by the person at the removal's
// time, in one batch with their head rows (spec requirement r-proposed). The removal takes no person check
// of its own for them: it is the person's already (routeScopes), and the cascade is part of the
// delete. It reads, with the rest of what it reads before its first write, whether any lens stands
// on the document's node, and reads the stale lenses only where one does, so a document nothing
// stands on holds its transaction id no longer than before. No lens stands on a block of the
// document, since a document the removal takes holds no block, and the removal reads what it takes
// under the issue's owner lock, which every writer of a lens, a finding or a risk takes first
// (lockIssueOwner), and which the removal holds, so it takes no lock for them.
//
// The document's node is addressed by its ref_key, which a later document of the same issue and
// slug takes again: that document's create finds the node gone and states it live under its own
// event, rather than finding a live node that states nothing and keeps this document's cause. A node
// keeps its made_at and made_by over its life, as every node of the store does, so a document a
// build before this one created, where the store holds no node at its address yet, first gets the
// node its create would have written - live, made by its creator at its created_at, at the
// sentinel, as the backfill writes it (graphcensus's backfillObjectNodes) - and only then is it
// stated gone, by the person who removed it, at the time this transaction reads from the
// database's clock under the owner lock it holds, before its first write (graphstore.StatementTime),
// and past that first node's: later than every statement about the address, so a document made
// there next is dated later still. Where the
// store holds a node already, live or gone, no first node is written: a live one is stated gone,
// and a gone one (an earlier document's, whose address a build before this one took again) is
// left as it is, rather than moved live at a created_at that may predate its gone statement. The
// first node stays for good: a rollback to any earlier release, and a write an older build makes
// in any later roll, still reach it.
//
// The room is closed on every process that holds one, through the room-sync notification this
// transaction sends before it commits (docs.AnnounceDocumentRemoved): a room left open on
// another process is a room over a row that has gone, whose next append meets a foreign key that
// no longer resolves. The removing process closes its own room after the commit, and only then
// (docs.Service.Evict): a settlement that commits against a row that has gone would fail, and a
// client that reconnects between the two reads the document as gone rather than loading a room
// for a row that is no longer there. A settlement of the document already under way at the
// commit - queued behind this transaction for the owner's lock, or started before the eviction
// ended what the room had armed - reads the row gone and ends without a word
// (docs.Service.settleRoomWithin).
//
// So every document it removes is its own room: the room locked, read and closed here holds no
// other document, and no issue room changes.
func (s *server) removeArtifact(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	tx, artifact, _, ok := s.beginArtifactWrite(w, r)
	if !ok {
		return
	}
	defer tx.Rollback(r.Context())
	// The refusals the artifact row alone answers, judged before the room's lock: the removal of
	// the spec, of a role document, of a document of an issue room or of anything that is no
	// document waits for no lock a room's own writers hold.
	if refusal := artifactRowIsUntouched(artifact); refusal != nil {
		s.writeHandlerError(w, refusal)
		return
	}
	// From here on each lock the removal waits for, the room's first, waits at most
	// artifactRemovalLockTimeout; the owner's lock above was waited for as every writer of the
	// issue waits for it.
	if _, err := tx.Exec(r.Context(), artifactRemovalLockTimeout); err != nil {
		s.writeHandlerError(w, fmt.Errorf("bound the lock waits of the removal of %s: %w", artifact.ID, err))
		return
	}
	// The document's room, taken after the owner's lock and before anything of the document is
	// read: an append that commits between the read of its stored updates and their delete
	// would be deleted with the document, and one still in flight at the artifact's own delete
	// would meet the foreign key of doc_updates rather than this handler's refusal.
	if err := docs.LockDocumentRoom(r.Context(), tx, artifact.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The refusals that need the lock: the versions, the edits that have not settled, what
	// points at the document and what links to it.
	if refusal := artifactIsUntouched(r.Context(), tx, artifact); refusal != nil {
		s.writeHandlerError(w, refusal)
		return
	}
	// The document's node and its versions' nodes, stated gone, the document's with each
	// changed_by_run it states now retracted, in the store's order: the node rows before any row of
	// the document goes, and their facts in the batch that appends artifact.removed below
	// (removedDocumentNodes,
	// appendCausingEvent). What that node write reads, and whether a lens stands on the document's
	// node, is read first, under the locks and before the transaction's first write, so no read of
	// it runs while the transaction holds an id (#2468's write phase): the owner's lock and the
	// room's are held already, and every writer of the node, and of a lens, a finding or a risk
	// under the issue (lockIssueOwner), takes the owner's lock first, so neither the node, nor its
	// marks, nor what stands on it move before the node is stated gone.
	removed, err := removedDocumentNodes(r.Context(), tx, artifact, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	for _, clear := range artifactRemovalClears(artifact) {
		if err := clear.run(r.Context(), tx); err != nil {
			s.writeHandlerError(w, fmt.Errorf("clear the %s of the removed document: %w", clear.what, err))
			return
		}
	}
	// What package refs holds for the document: its own mentions, the index it is owed and the
	// backoff its own mentions were deferred under, through that package's own statements.
	if err := refs.ForgetDocument(r.Context(), tx, artifact.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `delete from artifacts where id = $1`, artifact.ID); err != nil {
		s.writeHandlerError(w, fmt.Errorf("remove the document %s: %w", artifact.ID, err))
		return
	}
	// Every lens on the document is stale from the delete on, since the row it recorded no longer
	// stands at the address; each goes gone with all that stands under it, by the person who removed
	// the document at the same time, in this transaction (spec requirement r-proposed), and the head
	// rows of each node it stated move with it, in the same batch.
	if removed.lensed {
		lenses, err := refs.StaleLensesOn(r.Context(), tx, artifact.RefKey)
		var gone []graphstore.Node
		if err == nil {
			gone, err = graphstore.PlanState(r.Context(), tx, lenses, graphstore.StateGone, removed.at.UTC(), actor)
		}
		if err == nil && len(gone) > 0 {
			err = store.RunWrite(r.Context(), tx, func(b *store.WriteBatch) error {
				graphstore.QueueWrite(b, gone, nil)
				graphstore.QueueHeads(b, graphstore.HeadsOf(gone, nil))
				return nil
			})
		}
		if err != nil {
			s.writeHandlerError(w, fmt.Errorf("state the lenses on the removed document %s gone: %w", artifact.ID, err))
			return
		}
	}
	// Every process closes its room of the document, from this transaction's commit. Sent here,
	// after the row is gone and before the commit, for the reason the seam gives: a removal that
	// fails to commit announces nothing.
	if err := docs.AnnounceDocumentRemoved(r.Context(), tx, artifact.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The event and the gone statement it causes, in one batch (appendCausingEvent).
	event, err := s.appendCausingEvent(r.Context(), tx, ownerForArtifact(artifact).event(
		"artifact.removed", actor, map[string]any{"artifact": artifact},
	), removed.nodes)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), event)
	// The eviction outlives the request: a caller that hung up still leaves no room open on a
	// row that has gone. A room this server could not close is logged, not answered: the
	// document is removed, which is what the caller asked for and what every later read sees.
	if err := s.deps.Docs.Evict(context.WithoutCancel(r.Context()), artifact.ID); err != nil {
		slog.Error("dispatch: close the editor room of a removed document", "artifact", artifact.ID, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// artifactRemovalLockTimeout bounds each lock wait of a removal once it holds the issue's owner
// lock (this issue's spec R7): the document's room, the tables and rows its reads and deletes take,
// and the check of each foreign key to artifacts (id) that the delete of the artifact row runs.
// Every comment, ask and upload of the issue queues behind that owner lock, so a removal that
// meets a lock it cannot take gives up rather than hold them for as long as the lock's holder
// lives: Postgres ends the waiting statement with lock_not_available (55P03), the transaction
// rolls back with nothing removed, and the route answers 500 INTERNAL, as it answers any statement
// that failed (writeHandlerError). The document stands as it was, and the removal sent again once
// the holder lets go removes it.
//
// It is set after the owner lock, which it leaves unbounded: every writer of the issue waits its
// turn for that lock, and its holder writes and commits. It is five seconds, the bound the store's
// rename pass gives a statement that waits on a row a request holds (repoRenameLockTimeout) and
// the migration runner gives every migration (pgmigrate.LockTimeout), and not the 200 ms of a
// search's progress read (embeddingProjectKeysFilledLockWait), whose answer that search can do
// without. The removal waits on purpose for a room's own writer - a batch of browser updates, a
// compaction, a snapshot - to let the room's lock go, so that it reads what that writer stored
// and refuses (artifactIsUntouched), and 200 ms would refuse a removal behind any such writer
// that ran longer.
const artifactRemovalLockTimeout = `set local lock_timeout = '5s'`

// removedDocumentNodes states the removed document's node gone, and the node of each version of it
// the store holds (R16 of the issue-tree spec: a document and its versions are nodes of its
// issue's tree, and one removed is no longer present in it), as the block index states a block a
// version no longer holds gone (refs.IndexBlocks): through the store's own node write, a restating
// write of each node whose state is gone, which moves the state and keeps the visibility, and
// states the moved state as a base attribute held for the removal's event (graphstore.PlanUncaused,
// graphstore.QueueNodes). Its part_of and every other fact stay in the log as they were, which
// is the history a removal keeps; no read answers a gone node.
//
// The document's node is stated gone as removeArtifact's own words state it: where the store holds
// no node at its address yet, first the node its create would have written, live, made by its
// creator at its created_at, at the sentinel, with the part_of to its issue the backfill writes
// beside it; then the gone node, by actor, at the time the database's clock reads under the
// owner's lock (graphstore.StatementTime) before that first node is written and past that first
// node's creation, with each changed_by_run the node states now retracted by actor at that time,
// so a document made again at its ref_key names no run as having changed it but its own. Each
// version's node is written with the supertag and the attributes the store holds
// of it, since a node write moves both, so a version keeps its number and its time, and it is
// stated gone at the same time. A version whose node the store does not hold, one a build before
// the version nodes wrote and the backfill has not reached, is no node to end, and no node is made
// for it: its row goes with the removal, so no backfill reaches it after. The two are one write,
// held for the removal's event.
//
// It reads, too, whether a lens stands on the document's node, so removeArtifact states each lens
// on it gone, with all that stands under it at every depth, once the delete of the row leaves it
// stale (refs.StaleLensesOn, graphstore.PlanState), and only where one does; a version's node,
// which stands beneath the document's too, is no lens, and is stated gone here, once.
//
// Everything it reads - whether the store holds the node, the time, the changed_by_run tuples,
// the versions' nodes and the part_of tuples into the document's node - it reads before its first
// write, so no read of it runs while the transaction holds an id (#2468's write phase). It is
// written after the owner's lock and the room lock this removal already holds and before any row
// of the document is deleted, the order the backfill writes the node of an issue's document in
// (graphcensus' writeObjectBatch). A removed document holds no block, so there is no block node to
// state gone beside it: the removal takes only a document whose one version is empty and which has
// no unsettled edit (artifactIsUntouched), and an empty version indexes no block (refs.IndexBlocks).
// A ref_key past the store's address bound is no node of the store, and nothing is written for it.
func removedDocumentNodes(ctx context.Context, tx pgx.Tx, artifact model.Artifact, actor model.Actor) (removedNodes, error) {
	if !graphstore.AddressFits(artifact.RefKey) {
		return removedNodes{}, nil
	}
	_, held, err := graphstore.ReadNode(ctx, tx, graphstore.KindArtifact, artifact.RefKey)
	if err != nil {
		return removedNodes{}, err
	}
	removedAt, err := graphstore.StatementTime(ctx, tx, graphstore.KindArtifact, artifact.RefKey)
	if err != nil {
		return removedNodes{}, err
	}
	// The first node a missing one is given below is dated at the document's creation, which a row
	// dated past the database's clock puts past the time just read.
	if !held && !removedAt.After(artifact.CreatedAt) {
		removedAt = artifact.CreatedAt.Add(time.Microsecond)
	}
	// The node stated gone names no row, so it keeps the row's id and created_at it holds
	// (graphstore.ArtifactRow): the store is append-only, and a gone node is answered nowhere.
	gone := graphstore.ArtifactNode(artifact.RefKey, artifact.Project, deref(artifact.SessionID), actor, removedAt)
	gone.State = graphstore.StateGone
	marks, err := graphstore.CurrentOut(ctx, tx, graphstore.KindArtifact, artifact.RefKey, []string{graphstore.TupleChangedByRun})
	if err != nil {
		return removedNodes{}, err
	}
	retractions := make([]graphstore.Tuple, len(marks))
	for index, mark := range marks {
		retractions[index] = graphstore.Tuple{
			FromKind: mark.FromKind, FromID: mark.FromID, Kind: mark.Kind, ToKind: mark.ToKind, ToID: mark.ToID,
			At: removedAt, Who: actor, Asserted: false,
		}
	}
	rows, err := tx.Query(ctx, `select number from artifact_versions where artifact_id = $1 order by number`, artifact.ID)
	if err != nil {
		return removedNodes{}, fmt.Errorf("read the versions of the removed document %s: %w", artifact.ID, err)
	}
	numbers, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return removedNodes{}, fmt.Errorf("read the versions of the removed document %s: %w", artifact.ID, err)
	}
	nodes := []graphstore.Node{gone}
	for _, number := range numbers {
		version, held, err := graphstore.ReadNode(ctx, tx, graphstore.KindVersion, graphstore.VersionID(artifact.RefKey, number))
		if err != nil {
			return removedNodes{}, err
		}
		if !held {
			continue
		}
		version.State, version.MadeAt, version.MadeBy = graphstore.StateGone, removedAt, actor
		version.SourceEvent, version.Restates = graphstore.NoEvent, true
		nodes = append(nodes, version)
	}
	placed, err := graphstore.CurrentIn(ctx, tx, graphstore.KindArtifact, artifact.RefKey, []string{graphstore.TuplePartOf})
	if err != nil {
		return removedNodes{}, err
	}
	lensed := slices.ContainsFunc(placed, func(tuple graphstore.Tuple) bool { return tuple.FromKind == graphstore.KindLens })
	if !held {
		made := graphstore.WithArtifactRow(graphstore.ArtifactNode(artifact.RefKey, artifact.Project, deref(artifact.SessionID), artifact.CreatedBy, artifact.CreatedAt),
			graphstore.ArtifactRow{ID: artifact.ID, CreatedAt: artifact.CreatedAt})
		var partOf []graphstore.Tuple
		if artifact.IssueKey != nil {
			partOf = []graphstore.Tuple{graphstore.ArtifactPartOf(artifact.RefKey, *artifact.IssueKey, artifact.CreatedBy, artifact.CreatedAt)}
		}
		facts := graphstore.AttributeFacts(partOf, made)
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueWrite(b, []graphstore.Node{made}, facts)
			graphstore.QueueHeads(b, graphstore.HeadsOf([]graphstore.Node{made}, facts))
			return nil
		}); err != nil {
			return removedNodes{}, fmt.Errorf("write the node of the removed document %s: %w", artifact.ID, err)
		}
	}
	written := graphstore.PlanUncaused(nodes, retractions)
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueNodes(b, written)
		return nil
	}); err != nil {
		return removedNodes{}, fmt.Errorf("state the nodes of the removed document %s gone: %w", artifact.ID, err)
	}
	return removedNodes{nodes: written, at: removedAt, lensed: lensed}, nil
}

// removedNodes is what removedDocumentNodes wrote and read for removeArtifact: the gone
// statements held for the removal's event, the time they are stated at, and whether a lens stands
// on the document's node. The zero value is a document whose address is no node of the store.
type removedNodes struct {
	nodes  graphstore.Uncaused
	at     time.Time
	lensed bool
}

// artifactRowIsUntouched answers the refusal the artifact's own row reads, or nil when that row
// is a document of an issue that is its own room. Four of these are the tests R10 names first -
// the kind, the role, the issue's spec and the owner - and each reads a column of the row
// beginArtifactWrite re-read under the owner's lock, so the removal answers them before it takes
// the room's lock (removeArtifact). A project's document reads a refusal of its own, because a
// project document's events are its own (events.artifact_id under events_one_owner) and its
// removal event would need the row the removal deletes. An artifact of an agent's conversation
// names neither an issue nor a project and is never a document, so the kind test here is the one
// it reads.
//
// A document of an issue room, whose document_rooms row names the issue's tree
// (model.Artifact.Room), is refused too. Only the spec, the Verification document and the plan
// join the room (documentJoinsIssueRoom), and the role test already refuses each of them; this
// test keeps every document of the room out of the removal whatever its role reads. Every writer
// of a document_rooms
// row takes the issue's owner lock first (createConvertedIssueRoom, queueIssueRoomJoin), so the row read here
// stands until the removal commits.
func artifactRowIsUntouched(artifact model.Artifact) error {
	switch {
	case artifact.Kind != "doc":
		return artifactNotUntouched(artifact, fmt.Sprintf("it is a %s and not a document", artifact.Kind))
	case artifact.Role != "":
		return artifactNotUntouched(artifact, fmt.Sprintf("it plays the %s role", artifact.Role))
	case isSpecDocument(artifact):
		return artifactNotUntouched(artifact, "it is the issue's spec")
	case artifact.Room != artifact.ID:
		return artifactNotUntouched(artifact, "it shares its issue's room with the spec")
	case artifact.IssueKey == nil:
		return projectDocumentRemovalRefusal(artifact)
	}
	return nil
}

// artifactIsUntouched answers the refusal what the document holds reads, or nil when it holds
// nothing and nothing names it. These are the rest of the tests R10 names, in its order: the
// versions, the unsettled edits, what points at the document and what links to it. The tests of
// the row alone ran before this one (artifactRowIsUntouched).
//
// Each test is read inside tx, the removal's own transaction, and under the document's room
// lock, which removeArtifact takes before it calls this. The versions and the unsettled edits
// are what need that lock: a room's own writer takes no owner lock, so the issue's owner lock this
// transaction holds does not order it. The writers of a comment, an ask, a review, an approval
// request and a check result do take the issue's owner lock, so one that arrives meanwhile waits
// behind this transaction, reads the document again and is refused.
func artifactIsUntouched(ctx context.Context, tx pgx.Tx, artifact model.Artifact) error {
	// The versions, read in one row: how many there are, whether the version the document was
	// made at holds text, and whether any later version does. A document made by typing a title
	// holds version 1 and nothing in it; one made with content holds that content at version 1;
	// one someone wrote in holds what settlement versioned above version 1; and a named version
	// of an empty document is a second empty row.
	var versions int
	var madeWithContent, writtenSince bool
	var settledThrough int64
	if err := tx.QueryRow(ctx, `
		select count(*)::int,
		       coalesce(bool_or(v.number = 1 and coalesce(octet_length(v.markdown), 0) > 0), false),
		       coalesce(bool_or(v.number <> 1 and coalesce(octet_length(v.markdown), 0) > 0), false),
		       coalesce((select latest.doc_update_version from artifact_versions latest
		                 where latest.artifact_id = $1 order by latest.number desc limit 1), 0)
		from artifact_versions v where v.artifact_id = $1
	`, artifact.ID).Scan(&versions, &madeWithContent, &writtenSince, &settledThrough); err != nil {
		return fmt.Errorf("read the versions of %s: %w", artifact.ID, err)
	}
	// The count is read for every value that is not one, because R10 passes only a document
	// that holds exactly one version whose markdown is empty. More than one version is a second
	// row somebody made: a settled write or a named version. No version at all is a row no
	// route of Dispatch makes - every document is made with its first version in the same
	// transaction - which is why the test of that refusal writes the row itself; the removal
	// still refuses it, because a row that holds no version is not the row these words delete.
	switch {
	case writtenSince:
		return artifactNotUntouched(artifact, "it holds writing")
	case madeWithContent:
		return artifactNotUntouched(artifact, "its first version is not empty")
	case versions > 1:
		return artifactNotUntouched(artifact, "it holds more than one version")
	case versions != 1:
		return artifactNotUntouched(artifact, "it holds no version at all")
	}
	// An edit that has not settled, in the durable terms a room is read by: a doc_updates row
	// above the latest version's doc_update_version, the cursor writeDocumentVersion stamps
	// (artifacts.go). A live room is not read, in either of the ways the rebuild route reads one
	// (docs/rebuild.go): a removal from the document's own page always has its room open, and a
	// room that is merely open stores no update. doc_settlements_pending and doc_pending_authors
	// are no part of this either, for the same reason: a settlement pass that writes nothing may
	// leave a row in each, and both cascade with the artifact row.
	var unsettled bool
	if err := tx.QueryRow(ctx, `
		select exists(select 1 from doc_updates where artifact_id = $1 and version > $2)
	`, artifact.ID, settledThrough).Scan(&unsettled); err != nil {
		return fmt.Errorf("read the unsettled edits of %s: %w", artifact.ID, err)
	}
	if unsettled {
		return artifactNotUntouched(artifact, "an edit of it has not settled")
	}
	// What points at the document. An ask or a comment of an issue names one of its documents
	// through its anchor or, for an ask written back as a question block, through the document
	// holding that block in the room it is edited in (refs.AsksOfDocumentSQL): asks_one_owner and
	// comments_one_owner keep artifact_id for a project's documents, which this route does not
	// remove. An approval request names every document it asks about in approval.documents, the
	// spec and the issue's Verification document together among them.
	var points struct {
		comment, ask, approval, review, result, read bool
	}
	if err := tx.QueryRow(ctx, `
		select exists(select 1 from comments c
		              where c.artifact_id = $1 or c.anchor->>'artifact_id' = $2),
		       exists(select 1 from asks k where k.kind <> 'approval'
		              and (k.artifact_id = $1 or `+refs.AsksOfDocumentSQL("k", "$1")+` or k.anchor->>'artifact_id' = $2)),
		       exists(select 1 from asks k where k.kind = 'approval'
		              and (k.artifact_id = $1 or `+refs.AsksOfDocumentSQL("k", "$1")+`
		                   or jsonb_path_query_array(k.approval, '$."documents"[*]."artifact_id"') @> to_jsonb($2::text))),
		       exists(select 1 from artifact_reviews w where w.artifact_id = $1),
		       exists(select 1 from check_results c where `+refs.CheckResultsOfDocumentSQL("c", "$1::uuid")+`),
		       exists(select 1 from user_document_reads u where u.artifact_id = $1)
	`, artifact.ID, artifact.ID).Scan(
		&points.comment, &points.ask, &points.approval, &points.review, &points.result, &points.read,
	); err != nil {
		return fmt.Errorf("read what points at %s: %w", artifact.ID, err)
	}
	switch {
	case points.comment:
		return artifactNotUntouched(artifact, "a comment points at it")
	case points.ask:
		return artifactNotUntouched(artifact, "an ask points at it")
	case points.approval:
		return artifactNotUntouched(artifact, "an approval request points at it")
	case points.review:
		return artifactNotUntouched(artifact, "a review points at it")
	case points.result:
		return artifactNotUntouched(artifact, "a check result points at it")
	case points.read:
		return artifactNotUntouched(artifact, "a person has read it")
	}
	// What cites the document, read by package refs, which owns the three tables a link is
	// stored in and the shape of each (refs.DocumentIsCited): a mention of the document in
	// anybody's text, or of a block of it, is work the removal would leave pointing at nothing.
	cited, err := refs.DocumentIsCited(ctx, tx, artifact.ID, artifact.RefKey)
	if err != nil {
		return err
	}
	if cited {
		return artifactNotUntouched(artifact, "something in Dispatch links to it")
	}
	return nil
}

// artifactNotUntouched is the 409 refusing the removal of a document that holds something or that
// something names, saying which test it failed. A document is removed while it is still what
// typing a title made, and anything in it or about it is somebody's work, which a removal would
// take with it.
func artifactNotUntouched(artifact model.Artifact, reason string) error {
	return errorf(http.StatusConflict, "ARTIFACT_NOT_UNTOUCHED",
		"artifact %s (%s) is not untouched: %s; a document is removed only while it holds nothing",
		artifact.Slug, artifact.Name, reason)
}

// projectDocumentRemovalRefusal is the 400 refusing the removal of a project's document. A
// project document's events are its own (events.artifact_id under events_one_owner), so its
// removal event would need the row the removal deletes.
func projectDocumentRemovalRefusal(artifact model.Artifact) error {
	return errorf(http.StatusBadRequest, "ARTIFACT_PROJECT_DOCUMENT",
		"artifact %s (%s) belongs to project %s; a project's document is not removed",
		artifact.Slug, artifact.Name, artifact.Project)
}

// artifactClear is one statement the removal runs before it deletes the artifact row, with the
// rows it clears named for the error a failure reports.
type artifactClear struct {
	what      string
	statement string
	args      []any
	// table, where set, is the table the statement clears, which the boot step that retires it
	// may have dropped (startup.SearchIndexRetirement): the clear runs only while it stands.
	table string
}

// run runs the clear in tx, and does nothing where its table no longer stands. The removal reads
// that after it has deleted the document's versions (artifactRemovalClears' order), which is the
// hold the retiring step's first drop waits out, so a table it reads standing stands until the
// removal ends.
func (c artifactClear) run(ctx context.Context, tx pgx.Tx) error {
	if c.table != "" {
		var stands bool
		if err := tx.QueryRow(ctx, `select to_regclass($1::text) is not null`, c.table).Scan(&stands); err != nil {
			return fmt.Errorf("read whether %s stands: %w", c.table, err)
		}
		if !stands {
			return nil
		}
	}
	_, err := tx.Exec(ctx, c.statement, c.args...)
	return err
}

// artifactRemovalClears is every row the artifact's own delete leaves behind that this route can
// meet, in key order.
//
// The first four are the foreign keys to artifacts(id) that carry no `on delete cascade` and
// that an untouched document still holds, so the delete of the artifact row would be refused
// while any of them stood. The rest are the artifact-keyed rows no foreign key reaches at all: a
// `references` on them would make the pre-deploy census size the table it points at, which those
// tables say in their own migrations
// (20261101182447_document_search.up.sql, 20261028195714_doc_compaction_owed.up.sql), and
// artifact_versions was 27.5 GB in production against a census limit of one GiB. The keys that
// do cascade are left to the artifact row's own delete, and the reference rows, which carry no
// key either, to package refs
// (refs.ForgetDocument). document_search_backfill's one row is not cleared, though it carries an
// artifact_id: that id is the keyset cursor of the search fill a process of the release before
// runs at each boot, a position in the fill and not a row of the document, and deleting it when
// the cursor named the removed document left no such process able to start
// (#4224).
//
// document_search's rows are cleared only while that table stands (artifactClear.table): the boot
// step that retires document search drops it, and a removal that named it once it was gone would
// fail. While it stands, a build that still reads it, a rollback's among them, would go on finding
// the removed document's text there.
//
// Five more keys to artifacts(id) carry no cascade - asks.artifact_id, asks.block_artifact_id,
// comments.artifact_id, events.artifact_id and artifact_reviews.artifact_id - and no statement
// here clears them, because no row of them can be there. The untouched test refuses the removal
// while any of them names the document (artifactIsUntouched), and every writer of those tables
// takes the issue's owner lock first (lockCommentOwner, the requireOpenOwnerStatus of asks.go and
// of reviews.go), so one that arrives meanwhile waits behind this transaction, reads the document
// again and is refused. An issue document's events are the issue's and carry no artifact id at
// all (events_one_owner, ownerForArtifact), and a project's document, whose events are its own,
// is refused outright. A delete written for them would take a person's comment, ask or review
// the day a gap in that test let one through, where the foreign key refuses the removal instead,
// and it would make "nothing deletes an ask" false where two `for no key update` levels rest on
// it (lockAskForTransition, docs' loadAskBlocks). document_rooms.artifact_id carries no cascade
// either, and no row of it can be there: a document it names is refused before the room's lock
// (artifactRowIsUntouched), and its writers take the issue's owner lock first (createConvertedIssueRoom,
// queueIssueRoomJoin).
func artifactRemovalClears(artifact model.Artifact) []artifactClear {
	id := []any{artifact.ID}
	return []artifactClear{
		{what: "versions", statement: `delete from artifact_versions where artifact_id = $1`, args: id},
		{what: "stored updates", statement: `delete from doc_updates where artifact_id = $1`, args: id},
		{what: "snapshots", statement: `delete from doc_snapshots where artifact_id = $1`, args: id},
		{what: "checkpoints", statement: `delete from doc_checkpoints where artifact_id = $1`, args: id},
		{what: "search rows", statement: `delete from document_search where artifact_id = $1`, args: id, table: "document_search"},
		{what: "compaction owed", statement: `delete from doc_compaction_owed where artifact_id = $1`, args: id},
		{what: "search terms owed", statement: `delete from search_term_queue where artifact_id = $1`, args: id},
		{what: "inbox threads", statement: `delete from user_threads where artifact_id = $1`, args: id},
	}
}
