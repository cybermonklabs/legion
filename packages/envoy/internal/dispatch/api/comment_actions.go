package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// lockedComment is the prologue of every change to an existing comment that a closed owner
// refuses - resolve, reopen, accept, reject: read the comment, take its owner's lock and refuse a
// closed owner, then read it again in a statement after that lock, so the mutation decides on
// the comment as the lock's previous holder left it. It takes no row lock: every writer of a
// comment holds its owner's lock, so the comment holds still until this transaction ends.
func (s *server) lockedComment(ctx context.Context, tx pgx.Tx, id string) (model.Comment, error) {
	open, err := s.lockOwnerOfComment(ctx, tx, id)
	if err != nil {
		return model.Comment{}, err
	}
	if err := refuseClosed(open, refusedWhenClosed); err != nil {
		return model.Comment{}, err
	}
	return s.loadComment(ctx, tx, id)
}

// lockedCommentAllowingClosed is the same prologue for the writes a closed issue still takes: a
// comment's mention delivery and the mentioned agent's reply to it, and the author's own edit
// and delete (lockedCommentForCorrection). It takes the owner's lock in the same order without
// the closed refusal.
func (s *server) lockedCommentAllowingClosed(ctx context.Context, tx pgx.Tx, id string) (model.Comment, error) {
	if _, err := s.lockOwnerOfComment(ctx, tx, id); err != nil {
		return model.Comment{}, err
	}
	return s.loadComment(ctx, tx, id)
}

// correctionActor is the caller of a correction: an edit or a delete, of a comment
// (editComment, deleteComment) or of an issue message (editMessage, deleteMessage,
// message_corrections.go). The author of a post is a session or a person, so all four read
// both.
//
// The session comes first, through requireActor: the one the credential proves, or the one the
// body names, which is how a bearer names its writer on every other write it makes, the post's
// own create among them. A caller that names a session acts as that session, and the author
// check compares that session with the author.
//
// What is left is a credential that proves no session on a call that names none either. Where
// it holds the person scope it is a person's own - their browser's cookie, or a personal token
// of theirs - and the caller is that person (actingPerson), the path the person half of the
// author rule has always taken and the one the spec keeps for a personal token. Without it
// such a token could correct nothing: bearerSessionActor would ask its body to name a session
// (400 ACTOR_KIND), and a session is not the author of the comment its owner wrote in a
// browser (403 NOT_AUTHOR). Every other credential that proves no session - the deployment's
// shared token, a pod's service-account token - holds no person scope and keeps that refusal.
// The ask reword reaches the person along its own path: editAsk resolves the actor with
// requireActor and resolves the person beside it, and falls back to that person only where the
// session it resolved is not the ask's author (asks.go).
//
// The person's act names the agent beside them, from `?session=` (actingPerson). The body
// names no writer at all in that arm, so actSessionFromBody would have nothing to read.
//
// ok is false once it has written the refusal.
func (s *server) correctionActor(w http.ResponseWriter, r *http.Request, supplied *model.Actor) (model.Actor, bool) {
	caller, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return model.Actor{}, false
	}
	// The session the credential proves, and a body that names a writer at all: the two
	// bearerSessionActor reads, through the predicates it reads them with (server.go), so a
	// body naming anything but the session it acts as is answered there and not silently taken
	// as its owner's own write.
	if !credentialProvesSession(caller) && !actorNamesWriter(supplied) && caller.Credential.Holds(model.ScopePerson) {
		return s.actingPerson(w, r)
	}
	return s.requireActor(w, r, supplied)
}

// lockedCommentForCorrection is the prologue both corrections of a comment take, the comment
// twin of lockedMessageForCorrection (message_corrections.go): the prologue that does not refuse
// a closed owner - a wrong post nobody can correct after the issue closes is the defect these
// two routes exist for, and a correction adds no new work to a closed issue - and then the
// refusals in the order every write of this change takes them: the author first, so a caller
// that is not the author learns nothing else about the post; then a post already deleted, since
// a delete is final. A comment has no kind to refuse on, which the message twin refuses between
// the two. verb names the write in each refusal.
//
// The author is compared as a writer and not field by field: a person's actor carries the
// credential that acted beside them (model.ActorAct), so an agent correcting its person's
// comment carries an Act the stored author has not, and the person is who the authorship is
// about.
//
// ok is false once it has written the refusal.
func (s *server) lockedCommentForCorrection(
	w http.ResponseWriter, r *http.Request, tx pgx.Tx, actor model.Actor, verb string,
) (model.Comment, bool) {
	comment, err := s.lockedCommentAllowingClosed(r.Context(), tx, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return model.Comment{}, false
	}
	if !comment.Author.SameAs(actor) {
		writeError(w, "NOT_AUTHOR", http.StatusForbidden, "only the comment author may "+verb+" it")
		return model.Comment{}, false
	}
	if comment.DeletedAt != nil {
		s.writeHandlerError(w, deletedPostError(refs.ItemComment))
		return model.Comment{}, false
	}
	return comment, true
}

// lockOwnerOfComment reads the comment and locks its owner, answering whether that owner is
// open.
func (s *server) lockOwnerOfComment(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	unlockedComment, err := s.loadComment(ctx, tx, id)
	if err != nil {
		return false, err
	}
	_, open, err := ownerStatus(ctx, ownerOf(unlockedComment.IssueKey, unlockedComment.ArtifactID), lockedOwnerRead(tx))
	return open, err
}

func (s *server) resolveComment(w http.ResponseWriter, r *http.Request) {
	s.commentAction(w, r, "resolve")
}

func (s *server) acceptComment(w http.ResponseWriter, r *http.Request) {
	s.commentAction(w, r, "accept")
}

func (s *server) rejectComment(w http.ResponseWriter, r *http.Request) {
	s.commentAction(w, r, "reject")
}

func (s *server) reopenComment(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	comment, err := s.lockedComment(r.Context(), tx, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if comment.ReplyTo != nil || comment.AskID != nil {
		writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "reopen the thread root")
		return
	}
	artifactName, err := s.commentArtifactName(r.Context(), tx, comment)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// Everything up to Commit is the read phase (Docs.Join): this route and ProjectMark queue their
	// writes on the ledger, in the order made, and Commit sends them as one batch.
	if err := queueExec(ledger, `
		update comments
		set resolved = false, resolved_by = null, resolved_at = null
		where id = $1
	`, comment.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	comment.Resolved = false
	comment.ResolvedBy = nil
	comment.ResolvedAt = nil
	// A reopen is refused above on a reply, so the mark this writes is the thread's own; a
	// resolved orphaned suggestion reopens like any thread (projectThreadMark).
	if err := s.projectThreadMark(documentCtx, tx, ledger, comment, actor); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	payload, err := s.commentEventPayload(r.Context(), tx, &comment, artifactName, commentEventThread{}, model.ReferenceChanges{})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.queueEvent(ledger, ownerOf(comment.IssueKey, comment.ArtifactID).event(
		"comment.reopened",
		actor,
		payload,
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	comment, err = s.hydrateWrittenComment(r.Context(), tx, actor, comment)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), *event)
	WriteJSON(w, http.StatusOK, newShapedComment(comment, wantsDisplayTrees(r)))
}

// editCommentInput is the body of PATCH /api/v1/comments/{id}.
//
// actor is who makes the edit, for a bearer caller that names its own session: the route is the
// author's, and an agent's comment has an author of kind session, so a session that reads its
// own writer from no credential names it here, exactly as the resolve below does
// (commentAction). A person's credential names them and carries no actor at all.
//
// blocks and base_revision are the ids the dashboard editor carried through the edit: each
// block's id and where its span starts in body, in UTF-16 code units, against the comment's
// revision the editor read them at (refs.IndexItem). Without them the ids follow the text.
type editCommentInput struct {
	Body         string               `json:"body" required:"true"`
	Actor        *model.Actor         `json:"actor"`
	BaseRevision *int                 `json:"base_revision"`
	Blocks       []itemblocks.Carried `json:"blocks"`
}

func (s *server) editComment(w http.ResponseWriter, r *http.Request) {
	if !requireUUIDPath(w, r, "comment") {
		return
	}
	var input editCommentInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.correctionActor(w, r, input.Actor)
	if !ok {
		return
	}
	// An edit stores line feeds alone, as a comment's creation does (createCommentFor): the
	// display read ends a line at a carriage return too, but a body that holds one draws
	// differently in every other reader of the stored text. The carried offsets below are the
	// dashboard editor's, and a ProseMirror document holds no carriage return, so no offset this
	// normalisation shortens is one a caller sent.
	input.Body = pmdoc.LineFeeds(input.Body)
	if strings.TrimSpace(input.Body) == "" {
		writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "comment body is required")
		return
	}
	if err := blockBoundError(itemblocks.Check("body", input.Body)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var carried *refs.CarriedEdit
	if input.Blocks != nil {
		if input.BaseRevision == nil {
			writeError(w, "INVALID_COMMENT", http.StatusBadRequest, "blocks: requires base_revision, the comment's revision the blocks were read at")
			return
		}
		carried = &refs.CarriedEdit{BaseRevision: *input.BaseRevision, Blocks: input.Blocks}
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	// The prologue that keeps lockedComment's 409 ISSUE_CLOSED off this route and refuses every
	// caller but the author of a comment that stands.
	comment, ok := s.lockedCommentForCorrection(w, r, tx, actor, "edit")
	if !ok {
		return
	}
	// The owner lock above decided this comment is edited, a closed issue's included, so its
	// anchor's document writes are comment marks (docs.WithCommentMarks): the margin projection
	// below, which a closed issue takes as it takes the edit. A comment carrying a pending
	// suggestion takes the context here, where the create withholds it from one: the projection
	// rewrites the record of a mark that already stands - the comment's body is that record's
	// text - and places no suggestion of its own, and a new suggestion is what a closed issue
	// refuses.
	documentCtx = docs.WithCommentMarks(documentCtx)
	artifactName, err := s.commentArtifactName(r.Context(), tx, comment)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// Everything up to Commit is the read phase (Docs.Join): this route and ProjectMark queue their
	// writes on the ledger, in the order made, and Commit sends them as one batch. edited_at is the
	// transaction's now(), which the queued update writes, read here so the projection and the
	// event carry it before that update runs.
	editedAt, err := ledger.Now(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := queueExec(ledger, `
		update comments
		set body = $2, edited_at = $3
		where id = $1
	`, comment.ID, input.Body, editedAt); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	previousBody := comment.Body
	comment.Body = input.Body
	comment.EditedAt = timestampPtr(&editedAt)
	if err := queuePostBodyHistory(ledger, refs.ItemComment, comment.ID, previousBody, actor); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	referenceChanges, err := s.queueEditedItemReferences(r.Context(), tx, ledger, comment.Author, refs.ItemComment, comment.ID, previousBody, comment.Body, editedAt, carried)
	if carriedErr := (*itemblocks.CarriedError)(nil); errors.As(err, &carriedErr) {
		writeError(w, "INVALID_COMMENT", http.StatusBadRequest, carriedErr.Error())
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := s.projectThreadMark(documentCtx, tx, ledger, comment, actor); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	payload, logins, err := s.commentCorrectionPayload(r.Context(), tx, &comment, artifactName, referenceChanges)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.queueLoggedEvent(ledger, ownerOf(comment.IssueKey, comment.ArtifactID).event(
		"comment.edited",
		actor,
		payload,
	), logins, "comment", comment.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	comment, err = s.hydrateWrittenComment(r.Context(), tx, actor, comment)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), *event)
	s.storeDisplayTrees(r.Context(), comment.ID, input.Body)
	WriteJSON(w, http.StatusOK, newShapedComment(comment, wantsDisplayTrees(r)))
}

// recordOrphanedCommentAnchor records on the comment's row what a document write just found:
// the mark its anchor names is no longer in the document (docs.ErrAnchorOrphaned). The anchor
// keeps everything else it carries, so the comment still reads as the one made on that quote,
// and the write is queued on the caller's own ledger, which both callers of this then end their
// own way: the accept and the reject arms of commentAction commit the orphaning, with the event
// that dates it (queueOrphanedAnchorEvent), and answer 409, where deleteComment carries on and
// deletes the comment, whose comment.deleted carries the orphaned anchor.
//
// Only the orphaned key is written, the one key this refusal decides: writing the whole anchor
// would write back every other key of the column as this transaction read it, which is not what
// orphaning an anchor means. The narrow write says what it changes, and the artifact the
// comment's anchored_to tuple names stays among the rest, so this write restates nothing the
// store holds.
func recordOrphanedCommentAnchor(ledger *docs.Ledger, comment *model.Comment) error {
	comment.Anchor.Orphaned = true
	return queueExec(ledger, `
		update comments set anchor = jsonb_set(anchor, '{orphaned}', to_jsonb(true)) where id = $1
	`, comment.ID)
}

// queueOrphanedAnchorEvent queues on ledger the comment.anchor_refreshed that dates the orphaning
// of comment's anchor, which an accept or a reject records when it finds the mark gone
// (recordOrphanedCommentAnchor). It is the event an anchor refresh appends for a comment whose
// mark it finds gone (commentAnchorRefreshEvent): the comment's row as the orphaning leaves it,
// with its mention and delivery rows, and its anchor document's name, project key and slug. A
// read at a cut takes the anchor of the comment's newest event at the cut, so without this event
// a read at a cut after the orphaning and before the comment's next event gave the anchor as not
// orphaned (asof.CommentAnchorsOrphanedNotGivenStatement). The route publishes the event after
// its commit.
func (s *server) queueOrphanedAnchorEvent(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, comment model.Comment, actor model.Actor) (*model.Event, error) {
	build, err := s.commentAnchorRefreshEvent(ctx, tx, comment, actor, true)
	if err != nil {
		return nil, err
	}
	event, err := build(*comment.Anchor)
	if err != nil {
		return nil, err
	}
	return s.queueEvent(ledger, event)
}

func (s *server) commentAction(w http.ResponseWriter, r *http.Request, action string) {
	var input actorInput
	if !s.decodeOptionalJSON(w, r, &input) {
		return
	}
	var actor model.Actor
	var ok bool
	if action == "resolve" {
		actor, ok = s.requireActor(w, r, input.Actor)
	} else {
		actor, ok = s.actingPerson(w, r)
	}
	if !ok {
		return
	}
	// Read before the transaction opens (specWriterOf), for the check of the accept and the
	// reject below; the resolve writes no document text and reads nothing for it.
	var writer specWriter
	if action != "resolve" {
		writer = s.specWriterOf(r, actor)
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	comment, err := s.lockedComment(r.Context(), tx, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	artifactName, err := s.commentArtifactName(r.Context(), tx, comment)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	projectionKind := ""
	eventType := "comment.resolved"
	resolvedBy, err := encodeJSON(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// Everything up to Commit is the read phase (Docs.Join): this route and the document operations
	// queue their writes on the ledger, in the order made, and Commit sends them as one batch.
	// resolved_at is the transaction's now(), which the queued update writes, read here so the
	// projection and the event carry it before that update runs.
	switch action {
	case "resolve":
		// An orphaned suggestion's mark is gone, so it has no projection kind to carry; the
		// resolve still lands and ProjectMark writes only the marks map. Resolve is the one way
		// to close a suggestion that can no longer be accepted or rejected.
		kind, kindErr := s.commentProjectionKind(documentCtx, comment)
		if kindErr != nil && !errors.Is(kindErr, docs.ErrAnchorMissing) {
			s.writeHandlerError(w, kindErr)
			return
		}
		if kindErr == nil {
			projectionKind = kind
		}
		resolvedAt, err := ledger.Now(r.Context())
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := queueExec(ledger, `
			update comments
			set resolved = true, resolved_by = $2, resolved_at = $3
			where id = $1
		`, comment.ID, resolvedBy, resolvedAt); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		comment.Resolved = true
		comment.ResolvedBy = &actor
		comment.ResolvedAt = timestampPtr(&resolvedAt)
	case "accept", "reject":
		// Both move the suggestion's replacement text into the document, and the delete cleared
		// that text with the body, so neither has anything left to move. It is refused before
		// the suggestion itself is read, since the delete took that too. A resolve of a deleted
		// thread stands: it moves no text.
		if comment.DeletedAt != nil {
			s.writeHandlerError(w, deletedPostError(refs.ItemComment))
			return
		}
		if comment.Suggestion == nil {
			writeError(w, "INVALID_SUGGESTION", http.StatusBadRequest, "accept and reject require a suggestion")
			return
		}
		if comment.Suggestion.Accepted != nil {
			writeError(w, "ALREADY_ACTIONED", http.StatusConflict, "suggestion has already been actioned")
			return
		}
		if action == "accept" && comment.Anchor == nil {
			writeError(w, "INVALID_SUGGESTION", http.StatusBadRequest, "accept requires an anchored suggestion")
			return
		}
		kind, kindErr := s.commentProjectionKind(documentCtx, comment)
		if kindErr != nil && !errors.Is(kindErr, docs.ErrAnchorMissing) {
			s.writeHandlerError(w, kindErr)
			return
		}
		if kindErr == nil {
			projectionKind = kind
		}
		if comment.Anchor != nil {
			// The check of who writes an issue's spec or Verification document, on the document
			// the suggestion is anchored in, before the document call. The accept and the reject
			// need the person scope at the route's gate, so the check passes them at once.
			if err := refuseSpecWriterOfDocuments(r.Context(), tx, writer, comment.Anchor.ArtifactID); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			var markErr error
			if action == "accept" {
				markErr = s.deps.Docs.AcceptSuggestion(documentCtx, comment.Anchor.ArtifactID, comment.Anchor.MarkID, comment.Suggestion.ReplaceWith, actor)
			} else {
				markErr = s.deps.Docs.RejectSuggestion(documentCtx, comment.Anchor.ArtifactID, comment.Anchor.MarkID, actor)
			}
			if markErr != nil {
				if !errors.Is(markErr, docs.ErrAnchorOrphaned) {
					s.writeHandlerError(w, markErr)
					return
				}
				// An anchor already orphaned changes nothing, and appends no event, as a refresh
				// that changes nothing appends none.
				changed := !comment.Anchor.Orphaned
				if err := recordOrphanedCommentAnchor(ledger, &comment); err != nil {
					s.writeHandlerError(w, err)
					return
				}
				var orphaned *model.Event
				if changed {
					if orphaned, err = s.queueOrphanedAnchorEvent(r.Context(), tx, ledger, comment, actor); err != nil {
						s.writeHandlerError(w, err)
						return
					}
				}
				if err := ledger.Commit(r.Context()); err != nil {
					s.writeHandlerError(w, err)
					return
				}
				if orphaned != nil {
					s.publish(r.Context(), *orphaned)
				}
				s.writeHandlerError(w, markErr)
				return
			}
		}
		accepted := action == "accept"
		if accepted {
			eventType = "suggestion.accepted"
		} else {
			eventType = "suggestion.rejected"
		}
		comment.Resolved = true
		comment.Suggestion.Accepted = &accepted
		// The decision is merged into the column (jsonb ||), so every key the create stored and
		// this write does not name stays: the kind, which the delete of a rejected suggestion reads
		// to tell an insert's quote, the comment's own words, from document text (deleteComment).
		suggestion, err := encodeJSON(comment.Suggestion)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		resolvedAt, err := ledger.Now(r.Context())
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		// The owner's lock this transaction holds keeps the suggestion as it was read above, so the
		// guard finds it undecided; were it decided, the update's callback refuses as the read
		// refused it, and Commit returns that refusal. The read above found the column holding a
		// suggestion, an object, so the merge has an object to merge into.
		if err := ledger.Queue(func(b *store.WriteBatch) error {
			b.Queue("", `
				update comments
				set resolved = true, resolved_by = $2, resolved_at = $3, suggestion = suggestion || $4::jsonb
				where id = $1 and suggestion->>'accepted' is null
			`, comment.ID, resolvedBy, resolvedAt, suggestion).Exec(func(tag pgconn.CommandTag) error {
				if tag.RowsAffected() == 0 {
					return errorf(http.StatusConflict, "ALREADY_ACTIONED", "suggestion has already been actioned")
				}
				return nil
			})
			return nil
		}); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		comment.ResolvedBy = &actor
		comment.ResolvedAt = timestampPtr(&resolvedAt)
	}
	s.registerCommentAnchor(ledger, comment, actor, true)
	if comment.Anchor != nil {
		replies, err := s.loadReplyChain(r.Context(), tx, everyReader, "reply_to", comment.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := s.deps.Docs.ProjectMark(documentCtx, comment.Anchor.ArtifactID, comment.Anchor.MarkID, commentMarkRecord(comment, replies, projectionKind), actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	events := make([]*model.Event, 0, 2)
	if action == "accept" {
		// Accepting a suggestion is a decision, so it names the version it produced; the
		// transactional apply never schedules a settle (R30), so this is the only version write.
		summary := acceptedVersionName(comment.Body, comment.Anchor.Quote, comment.Suggestion.ReplaceWith)
		named, err := s.deps.Docs.NamedVersion(documentCtx, comment.Anchor.ArtifactID, summary, actor)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		// The version records the comment whose accept named it (artifact_versions.accepted_from),
		// in this transaction, so the comment's delete finds the version by that link and renames
		// it (renameAcceptedVersion) however many other versions of the document carry its time.
		// The update is queued after the version's insert, which NamedVersion queued above.
		if err := queueExec(ledger, `
			update artifact_versions set accepted_from = $3 where artifact_id = $1 and number = $2
		`, comment.Anchor.ArtifactID, named.Version.Number, comment.ID); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		diff, err := s.namedVersionDiff(r.Context(), tx, ledger, comment.Anchor.ArtifactID, named.Version)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		versionEvent, err := s.commitArtifactVersionEvent(ledger, ownerOf(comment.IssueKey, comment.ArtifactID), actor, comment.Anchor.ArtifactID, artifactName, named, diff)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = append(events, versionEvent)
	}
	payload, err := s.commentEventPayload(r.Context(), tx, &comment, artifactName, commentEventThread{}, model.ReferenceChanges{})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.queueEvent(ledger, ownerOf(comment.IssueKey, comment.ArtifactID).event(
		eventType,
		actor,
		payload,
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events = append(events, event)
	comment, err = s.hydrateWrittenComment(r.Context(), tx, actor, comment)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), appendedEvents(events...)...)
	if action == "accept" {
		// The accepted text may have gone the way an edit's can: removed by a concurrent browser
		// change after this write's version was rendered and before it reached the room, which is
		// past undoing (LEGION-269). The accept stands and says so.
		lost, known := ledger.LostOps(comment.Anchor.ArtifactID)
		accepted := acceptedComment{shapedComment: newShapedComment(comment, wantsDisplayTrees(r))}
		if known {
			gone := len(lost) > 0
			accepted.Lost = &gone
		}
		WriteJSON(w, http.StatusOK, accepted)
		return
	}
	WriteJSON(w, http.StatusOK, newShapedComment(comment, wantsDisplayTrees(r)))
}

// acceptedComment is the accept route's response: the comment, plus whether the live document
// already lacks the text the accept wrote. It is null when the check reached no verdict - the
// publish failed and the room is reloading, or the room holds a tree too deep to read - which is
// not the same statement as false, exactly as an edit's lost_ops distinguishes them.
type acceptedComment struct {
	shapedComment
	Lost *bool `json:"lost"`
}

// acceptedVersionName is the name an accept gives the document version it makes: the comment's
// body, or the change the suggestion made where the comment has no body. The name rides the
// artifact.version event, so it takes the part an event carries (model.EventBody): a comment
// body is bounded block by block and can be far longer than that. A delete of the comment
// renames that version to the name a suggestion with no body takes (renameAcceptedVersion),
// so the one form is written here for both.
func acceptedVersionName(body, quote, replaceWith string) string {
	name := strings.TrimSpace(body)
	if name == "" {
		name = fmt.Sprintf("Accepted suggestion: %q → %q", quote, replaceWith)
	}
	name, _ = model.EventBody(name)
	return name
}
