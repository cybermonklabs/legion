package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// removeArtifact removes a document of an issue that is still untouched: one made by typing a
// title and never written in. It answers 204 and no body.
//
// It needs the person scope (routeScopes) and takes two locks: the owner's row, as the title
// correction does (renameArtifact), so a correction and a removal of one owner cannot each miss
// the other, and then the document's room (docs.LockDocumentRoom), which is the lock a durable
// write of the document takes. In that order, which is the one docs/persistence.go states: the
// owner row first, then the room, as a settlement takes them, and a room's own writers take no
// owner row, so this joins no lock cycle.
//
// Four refusals read the artifact row alone: the kind, the role, the issue's spec and a
// project's document (artifactRowIsUntouched). The removal answers them from the row
// beginArtifactWrite re-read, before the room's lock. A DELETE of the spec, of a role document
// or of a picture then waits for no room lock that a room's batch, a compaction or a snapshot
// holds, and it frees the issue's row at once for every comment, ask and upload of that issue.
//
// What the document holds needs the room's lock and is read under it (artifactIsUntouched),
// inside this transaction and from the row this transaction re-read: a version written, an edit
// typed, or a comment, ask, review, approval request, check result, read mark or link left
// between the request and the lock refuses the removal rather than being deleted with the
// document. An append of another process either committed before the room's lock was taken,
// which refuses the removal, or waits for this transaction and then meets the row that has gone.
//
// The rows go in one transaction: first the rows the artifact's own delete would leave behind or
// be refused by (artifactRemovalClears) and the reference rows package refs keeps for the
// document (refs.ForgetDocument), then the artifact row, which cascades the keys that do
// cascade. The artifact.removed event is appended after them and outlives the document: an issue
// document's events are the issue's (events_one_owner, ownerForArtifact), so that row carries no
// artifact id.
//
// The room is closed on every process that holds one, through the room-sync notification this
// transaction sends before it commits (docs.AnnounceDocumentRemoved): a room left open on
// another process is a room over a row that has gone, whose next append meets a foreign key that
// no longer resolves. The removing process closes its own room after the commit, and only then
// (docs.Service.Evict): a settlement that commits against a row that has gone would fail, and a
// client that reconnects between the two reads the document as gone rather than loading a room
// for a row that is no longer there.
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
	// the spec, of a role document or of anything that is no document waits for no lock a room's
	// own writers hold.
	if refusal := artifactRowIsUntouched(artifact); refusal != nil {
		s.writeHandlerError(w, refusal)
		return
	}
	// The document's room, taken after the owner's row and before anything of the document is
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
	for _, clear := range artifactRemovalClears(artifact) {
		if _, err := tx.Exec(r.Context(), clear.statement, clear.args...); err != nil {
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
	// Every process closes its room of the document, from this transaction's commit. Sent here,
	// after the row is gone and before the commit, for the reason the seam gives: a removal that
	// fails to commit announces nothing.
	if err := docs.AnnounceDocumentRemoved(r.Context(), tx, artifact.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, ownerForArtifact(artifact).event(
		"artifact.removed", actor, map[string]any{"artifact": artifact},
	))
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

// artifactRowIsUntouched answers the refusal the artifact's own row reads, or nil when that row
// is a document of an issue. These are the four tests R10 names first - the kind, the role, the
// issue's spec and the owner - and each reads a column of the row beginArtifactWrite re-read
// under the owner's lock, so the removal answers them before it takes the room's lock
// (removeArtifact). A project's document reads a refusal of its own, because a project
// document's events are its own (events.artifact_id under events_one_owner) and its removal
// event would need the row the removal deletes. An artifact of an agent's conversation names
// neither an issue nor a project and is never a document, so the kind test here is the one it
// reads.
func artifactRowIsUntouched(artifact model.Artifact) error {
	switch {
	case artifact.Kind != "doc":
		return artifactNotUntouched(artifact, fmt.Sprintf("it is a %s and not a document", artifact.Kind))
	case artifact.Role != "":
		return artifactNotUntouched(artifact, fmt.Sprintf("it plays the %s role", artifact.Role))
	case isSpecDocument(artifact):
		return artifactNotUntouched(artifact, "it is the issue's spec")
	case artifact.IssueKey == nil:
		return projectDocumentRemovalRefusal(artifact)
	}
	return nil
}

// artifactIsUntouched answers the refusal what the document holds reads, or nil when it holds
// nothing and nothing names it. These are the rest of the tests R10 names, in its order: the
// versions, the unsettled edits, what points at the document and what links to it. The four
// tests of the row alone ran before this one (artifactRowIsUntouched).
//
// Each test is read inside tx, the removal's own transaction, and under the document's room
// lock, which removeArtifact takes before it calls this. The versions and the unsettled edits
// are what need that lock: a room's own writer takes no owner row, so the issue's row this
// transaction holds does not order it. The writers of a comment, an ask, a review, an approval
// request and a check result do take the issue's row, so one that arrives meanwhile waits
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
	// through its anchor or, for an ask written back as a question block, through
	// block_artifact_id: asks_one_owner and comments_one_owner keep artifact_id for a project's
	// documents, which this route does not remove. An approval request names every document it
	// asks about in approval.documents, the spec and the issue's Verification document together
	// among them.
	var points struct {
		comment, ask, approval, review, result, read bool
	}
	if err := tx.QueryRow(ctx, `
		select exists(select 1 from comments c
		              where c.artifact_id = $1 or c.anchor->>'artifact_id' = $2),
		       exists(select 1 from asks k where k.kind <> 'approval'
		              and (k.artifact_id = $1 or k.block_artifact_id = $1 or k.anchor->>'artifact_id' = $2)),
		       exists(select 1 from asks k where k.kind = 'approval'
		              and (k.artifact_id = $1 or k.block_artifact_id = $1
		                   or jsonb_path_query_array(k.approval, '$."documents"[*]."artifact_id"') @> to_jsonb($2::text))),
		       exists(select 1 from artifact_reviews w where w.artifact_id = $1),
		       exists(select 1 from check_results c where c.check_artifact_id = $1),
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
}

// artifactRemovalClears is every row the artifact's own delete leaves behind that this route can
// meet, in key order.
//
// The first four are the foreign keys to artifacts(id) that carry no `on delete cascade` and
// that an untouched document still holds, so the delete of the artifact row would be refused
// while any of them stood. The rest are the artifact-keyed rows no foreign key reaches at all: a
// `references` on them would make the pre-deploy census size the table it points at, which those
// tables say in their own migrations (20261101182447_document_search.up.sql,
// 20261028195714_doc_compaction_owed.up.sql), and artifact_versions was 27.5 GB in production
// against a census limit of one GiB. The keys that do cascade are left to the artifact row's own
// delete, and the reference rows, which carry no key either, to package refs
// (refs.ForgetDocument).
//
// Five more keys to artifacts(id) carry no cascade - asks.artifact_id, asks.block_artifact_id,
// comments.artifact_id, events.artifact_id and artifact_reviews.artifact_id - and no statement
// here clears them, because no row of them can be there. The untouched test refuses the removal
// while any of them names the document (artifactIsUntouched), and every writer of those tables
// takes the issue's row first (lockCommentOwner, the requireOpenOwnerStatus of asks.go and of
// reviews.go), so one that arrives meanwhile waits behind this transaction, reads the document
// again and is refused. An issue document's events are the issue's and carry no artifact id at
// all (events_one_owner, ownerForArtifact), and a project's document, whose events are its own,
// is refused outright. A delete written for them would take a person's comment, ask or review
// the day a gap in that test let one through, where the foreign key refuses the removal instead,
// and it would make "nothing deletes an ask" false where two `for no key update` levels rest on
// it (lockAskForTransition, docs' loadAskBlocks).
func artifactRemovalClears(artifact model.Artifact) []artifactClear {
	id := []any{artifact.ID}
	return []artifactClear{
		{what: "versions", statement: `delete from artifact_versions where artifact_id = $1`, args: id},
		{what: "stored updates", statement: `delete from doc_updates where artifact_id = $1`, args: id},
		{what: "snapshots", statement: `delete from doc_snapshots where artifact_id = $1`, args: id},
		{what: "checkpoints", statement: `delete from doc_checkpoints where artifact_id = $1`, args: id},
		{what: "search rows", statement: `delete from document_search where artifact_id = $1`, args: id},
		{what: "search backfill rows", statement: `delete from document_search_backfill where artifact_id = $1`, args: id},
		{what: "compaction owed", statement: `delete from doc_compaction_owed where artifact_id = $1`, args: id},
		{what: "search terms owed", statement: `delete from search_term_queue where artifact_id = $1`, args: id},
		{what: "inbox threads", statement: `delete from user_threads where artifact_id = $1`, args: id},
	}
}
