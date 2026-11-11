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
// It needs the person scope (routeScopes) and takes the owner's lock before it reads the
// document, as the title correction does (renameArtifact), so a correction and a removal of one
// owner cannot each miss the other. Every test of "untouched" is read inside this transaction,
// under that lock, from the row this transaction re-read: a version written, an edit typed, or a
// comment, ask, review, approval request, check result, read mark or citation left between the
// request and the lock refuses the removal rather than being deleted with the document.
//
// The rows go in one transaction: first the nine foreign keys to artifacts(id) that do not
// cascade and the artifact-keyed rows no foreign key reaches (artifactRemovalClears), then the
// artifact row, which cascades the fifteen keys that do. The artifact.removed event is appended
// after that clear of events.artifact_id, so the clear cannot take it; an issue document's events
// are the issue's, so that row carries no artifact id and outlives the document.
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
	// The backoff a mention of this document was owed an excerpt under, cleared through the
	// statement the excerpt reconciliation already holds for it.
	if err := refs.ClearExcerptBackoff(r.Context(), tx, artifact.ID); err != nil {
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
	s.publish(event)
	// The eviction outlives the request: a caller that hung up still leaves no room open on a
	// row that has gone. A room this server could not close is logged, not answered: the
	// document is removed, which is what the caller asked for and what every later read sees.
	if err := s.deps.Docs.Evict(context.WithoutCancel(r.Context()), artifact.ID); err != nil {
		slog.Error("dispatch: close the editor room of a removed document", "artifact", artifact.ID, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// artifactIsUntouched answers the refusal artifact reads, or nil when it is a document of an
// issue that holds nothing and nothing names. Each test is read inside tx, the removal's own
// transaction, so what it reads is what the delete below it deletes.
//
// The tests are the ones R10 names, in its order: the kind, the role, the issue's spec, the
// owner, the versions, the unsettled edits, and what points at the document. A project's
// document reads a refusal of its own, because a project document's events are its own
// (events.artifact_id under events_one_owner) and its removal event would need the row the
// removal deletes. An artifact of an agent's conversation names neither an issue nor a project
// and is never a document, so the kind test above is the one it reads.
func artifactIsUntouched(ctx context.Context, tx pgx.Tx, artifact model.Artifact) error {
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
	switch {
	case writtenSince:
		return artifactNotUntouched(artifact, "it holds writing")
	case madeWithContent:
		return artifactNotUntouched(artifact, "its first version is not empty")
	case versions > 1:
		return artifactNotUntouched(artifact, "it holds more than one version")
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
		comment, ask, approval, review, result, read, citation bool
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
		       exists(select 1 from user_document_reads u where u.artifact_id = $1),
		       exists(select 1 from refs r
		              where ((r.to_kind = 'artifact' and r.to_id = $3) or (r.to_kind = 'block' and r.to_id like $4))
		                and not (r.from_kind = 'artifact' and r.from_id = $2))
		       or exists(select 1 from block_refs b
		                 where ((b.to_kind = 'artifact' and b.to_id = $3) or (b.to_kind = 'block' and b.to_id like $4))
		                   and b.artifact_id <> $1)
	`, artifact.ID, artifact.ID, artifact.RefKey, artifact.RefKey+"#%").Scan(
		&points.comment, &points.ask, &points.approval, &points.review, &points.result, &points.read, &points.citation,
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
	case points.citation:
		return artifactNotUntouched(artifact, "another document cites it")
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

// artifactRemovalClears is every row the artifact's own delete leaves behind, in key order.
//
// The first nine are the foreign keys to artifacts(id) that carry no `on delete cascade`, so the
// delete of the artifact row would be refused while any of them stood. The rest are the
// artifact-keyed rows no foreign key reaches at all: a `references` on them would make the
// pre-deploy census size the table it points at, which those tables say in their own migrations
// (20261101182447_document_search.up.sql, 20261028195714_doc_compaction_owed.up.sql), and
// artifact_versions was 27.5 GB in production against a census limit of one GiB. The fifteen keys
// that do cascade are left to the artifact row's own delete.
//
// refs is keyed by text on both sides: the document's own mentions are from_kind 'artifact' with
// its id, and a mention of the document is to_kind 'artifact' with its reference key. The queue
// rows of refs_grammar_owed are keyed by kind and id, and a document is owed under two kinds -
// `artifact`, its own block index, and `scope`, the sources around it - so the id alone is read.
func artifactRemovalClears(artifact model.Artifact) []artifactClear {
	id := []any{artifact.ID}
	return []artifactClear{
		{what: "versions", statement: `delete from artifact_versions where artifact_id = $1`, args: id},
		{what: "stored updates", statement: `delete from doc_updates where artifact_id = $1`, args: id},
		{what: "snapshots", statement: `delete from doc_snapshots where artifact_id = $1`, args: id},
		{what: "checkpoints", statement: `delete from doc_checkpoints where artifact_id = $1`, args: id},
		{what: "asks", statement: `delete from asks where artifact_id = $1`, args: id},
		{what: "question block asks", statement: `delete from asks where block_artifact_id = $1`, args: id},
		{what: "comments", statement: `delete from comments where artifact_id = $1`, args: id},
		{what: "events", statement: `delete from events where artifact_id = $1`, args: id},
		{what: "reviews", statement: `delete from artifact_reviews where artifact_id = $1`, args: id},
		{what: "search rows", statement: `delete from document_search where artifact_id = $1`, args: id},
		{what: "search backfill rows", statement: `delete from document_search_backfill where artifact_id = $1`, args: id},
		{what: "compaction owed", statement: `delete from doc_compaction_owed where artifact_id = $1`, args: id},
		{what: "search terms owed", statement: `delete from search_term_queue where artifact_id = $1`, args: id},
		{what: "inbox threads", statement: `delete from user_threads where artifact_id = $1`, args: id},
		{what: "index owed", statement: `delete from refs_grammar_owed where id = $1`, args: id},
		{
			what: "mentions",
			statement: `delete from refs
				where (from_kind = 'artifact' and from_id = $1) or (to_kind = 'artifact' and to_id = $2)`,
			args: []any{artifact.ID, artifact.RefKey},
		},
	}
}
