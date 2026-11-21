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

// lockedComment is the prologue of every change to an existing comment - resolve, reopen, edit,
// accept, reject: read the comment, take its owner's lock and refuse a closed owner, then read it
// again in a statement after that lock, so the mutation decides on the comment as the lock's
// previous holder left it. It takes no row lock: every writer of a comment holds its owner's
// lock, so the comment holds still until this transaction ends.
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

// lockedCommentForDelivery is the same prologue for a comment's mention delivery and the
// mentioned agent's reply to it, which a closed issue still takes: it takes the owner's lock in
// the same order without the closed refusal.
func (s *server) lockedCommentForDelivery(ctx context.Context, tx pgx.Tx, id string) (model.Comment, error) {
	if _, err := s.lockOwnerOfComment(ctx, tx, id); err != nil {
		return model.Comment{}, err
	}
	return s.loadComment(ctx, tx, id)
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

// codeCommentChanged is the refusal of a reopen or a resolve whose condition the thread no longer
// meets.
const codeCommentChanged = "COMMENT_CHANGED"

// reopenComment is POST /api/v1/comments/{id}/reopen. Its optional body names the acting session
// where the caller's credential proved none (actSessionFromBody), as the person routes take it.
// The route took no body before it took this one, so a body off the network that does not decode
// is ignored, as an empty one is, and the thread reopened as it was; a request the command bar
// sends in-process is held to its body (decodeJSON), since sender and route are one build. The
// command bar's undo makes the reopen conditional (goingCondition.ExpectResolvedAt, on the
// request's context): the thread must still hold the resolution of that moment, as the comment's
// read answers resolved_at, when the reopen takes the comment's lock, or it is refused, 409
// COMMENT_CHANGED, with nothing written. Every other caller reopens whatever resolution the thread
// holds. It answers as the resolve does (resolvedComment): the comment as the reopen left it, and
// the resolution it found under the comment's lock, which the command bar's undo of the reopen
// reads.
func (s *server) reopenComment(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input actorInput
	if goingServedInProcess(r.Context()) {
		if !s.decodeOptionalJSON(w, r, &input) {
			return
		}
	} else if r.ContentLength != 0 && decodeJSON(r, &input) != nil {
		input = actorInput{}
	}
	if actor, ok = actSessionFromBody(w, actor, input.Actor); !ok {
		return
	}
	condition := goingConditionOf(r)
	expected := condition.ExpectResolvedAt
	if refusal := condition.writeBackRefusal(expected != nil && strings.TrimSpace(*expected) != "", "resolution it expects the thread to hold"); refusal != nil {
		s.writeHandlerError(w, refusal)
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
	if expected != nil && (!comment.Resolved || comment.ResolvedAt == nil || *comment.ResolvedAt != *expected) {
		held := "open"
		if comment.Resolved && comment.ResolvedAt != nil {
			held = "resolved at " + *comment.ResolvedAt
		}
		writeError(w, codeCommentChanged, http.StatusConflict, fmt.Sprintf(
			"the thread is %s now, not the resolution of %s the reopen expected, so nothing was written", held, *expected))
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
	found := threadResolution{Resolved: comment.Resolved, ResolvedBy: comment.ResolvedBy, ResolvedAt: comment.ResolvedAt}
	comment.Resolved = false
	comment.ResolvedBy = nil
	comment.ResolvedAt = nil
	if comment.Anchor != nil {
		s.registerCommentAnchor(ledger, comment, actor, true)
		replies, err := s.loadReplyChain(r.Context(), tx, "reply_to", comment.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		// A resolved orphaned suggestion reopens like any thread: its mark is gone, so it has no
		// projection kind to carry and ProjectMark writes only the marks map.
		projectionKind := ""
		kind, kindErr := s.commentProjectionKind(documentCtx, comment)
		if kindErr != nil && !errors.Is(kindErr, docs.ErrAnchorMissing) {
			s.writeHandlerError(w, kindErr)
			return
		}
		if kindErr == nil {
			projectionKind = kind
		}
		if err := s.deps.Docs.ProjectMark(documentCtx, comment.Anchor.ArtifactID, comment.Anchor.MarkID, commentMarkRecord(comment, replies, projectionKind), actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
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
	// What the reopen answers it found and left, handed to the command bar's landing of this
	// request, which records it in this transaction (goingRecordAnswer).
	goingRecordAnswer(r.Context(), http.StatusOK, func() any {
		return resolvedComment{shapedComment: newShapedComment(comment, false), Previous: found}
	})
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), *event)
	WriteJSON(w, http.StatusOK, resolvedComment{shapedComment: newShapedComment(comment, wantsDisplayTrees(r)), Previous: found})
}

// editCommentInput is the body of PATCH /api/v1/comments/{id}.
//
// blocks and base_revision are the ids the dashboard editor carried through the edit: each
// block's id and where its span starts in body, in UTF-16 code units, against the comment's
// revision the editor read them at (refs.IndexItem). Without them the ids follow the text.
type editCommentInput struct {
	Body         string               `json:"body" required:"true"`
	BaseRevision *int                 `json:"base_revision"`
	Blocks       []itemblocks.Carried `json:"blocks"`
}

func (s *server) editComment(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input editCommentInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
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
	comment, err := s.lockedComment(r.Context(), tx, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The author is compared as a writer and not field by field: actingPerson hands back the
	// person with the credential that acted beside them (model.ActorAct), so an agent editing
	// its person's comment carries an Act the stored author has not, and the person is who the
	// authorship is about.
	if !comment.Author.SameAs(actor) {
		writeError(w, "NOT_AUTHOR", http.StatusForbidden, "only the comment author may edit it")
		return
	}
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
	referenceChanges, err := s.queueEditedItemReferences(r.Context(), tx, ledger, refs.ItemComment, comment.ID, previousBody, comment.Body, editedAt, carried)
	if carriedErr := (*itemblocks.CarriedError)(nil); errors.As(err, &carriedErr) {
		writeError(w, "INVALID_COMMENT", http.StatusBadRequest, carriedErr.Error())
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if comment.Anchor != nil {
		s.registerCommentAnchor(ledger, comment, actor, true)
		replies, err := s.loadReplyChain(r.Context(), tx, "reply_to", comment.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		projectionKind, err := s.commentProjectionKind(documentCtx, comment)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := s.deps.Docs.ProjectMark(documentCtx, comment.Anchor.ArtifactID, comment.Anchor.MarkID, commentMarkRecord(comment, replies, projectionKind), actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	payload, err := s.commentEventPayload(r.Context(), tx, &comment, artifactName, commentEventThread{}, referenceChanges)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.queueStampedEvent(ledger, ownerOf(comment.IssueKey, comment.ArtifactID).event(
		"comment.edited",
		actor,
		payload,
	), "comment", comment.ID)
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
	// The command bar's undo of a reopen makes the resolve conditional (goingCondition.ExpectOpen,
	// on the request's context): the thread must be open when the resolve takes the comment's
	// lock, or it is refused, 409 COMMENT_CHANGED, with nothing written.
	condition := goingConditionOf(r)
	if action == "resolve" {
		if refusal := condition.writeBackRefusal(condition.ExpectOpen, "expectation that the thread is open"); refusal != nil {
			s.writeHandlerError(w, refusal)
			return
		}
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
	// found is the thread's resolution as a resolve found it under the comment's lock, which the
	// resolve answers beside the comment it left (resolvedComment).
	var found *threadResolution
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
		if condition.ExpectOpen && comment.Resolved {
			writeError(w, codeCommentChanged, http.StatusConflict, "the thread is resolved now, not open as the resolve expected, so nothing was written")
			return
		}
		found = &threadResolution{Resolved: comment.Resolved, ResolvedBy: comment.ResolvedBy, ResolvedAt: comment.ResolvedAt}
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
				comment.Anchor.Orphaned = true
				// Only the orphaned key is written, the one key this refusal decides: writing the
				// whole anchor would write back every other key of the column as this transaction
				// read it, which is not what orphaning an anchor means. The narrow write says what
				// it changes.
				if err := queueExec(ledger, `
					update comments set anchor = jsonb_set(anchor, '{orphaned}', to_jsonb(true)) where id = $1
				`, comment.ID); err != nil {
					s.writeHandlerError(w, err)
					return
				}
				if err := ledger.Commit(r.Context()); err != nil {
					s.writeHandlerError(w, err)
					return
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
		// refused it, and Commit returns that refusal.
		if err := ledger.Queue(func(b *store.WriteBatch) error {
			b.Queue("", `
				update comments
				set resolved = true, resolved_by = $2, resolved_at = $3, suggestion = $4
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
		replies, err := s.loadReplyChain(r.Context(), tx, "reply_to", comment.ID)
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
		summary := strings.TrimSpace(comment.Body)
		if summary == "" {
			summary = fmt.Sprintf("Accepted suggestion: %q → %q", comment.Anchor.Quote, comment.Suggestion.ReplaceWith)
		}
		// The version's name rides the artifact.version event, so it takes the part an event
		// carries: a comment body is bounded block by block and can be far longer than that.
		summary, _ = model.EventBody(summary)
		named, err := s.deps.Docs.NamedVersion(documentCtx, comment.Anchor.ArtifactID, summary, actor)
		if err != nil {
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
	if found != nil {
		// What the resolve answers it found and left, handed to the command bar's landing of this
		// request, which records it in this transaction (goingRecordAnswer).
		goingRecordAnswer(r.Context(), http.StatusOK, func() any {
			return resolvedComment{shapedComment: newShapedComment(comment, false), Previous: *found}
		})
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
	if found != nil {
		WriteJSON(w, http.StatusOK, resolvedComment{shapedComment: newShapedComment(comment, wantsDisplayTrees(r)), Previous: *found})
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

// threadResolution is a thread's resolution as a comment's read answers it: whether it is
// resolved, by whom and when, null for an open thread.
type threadResolution struct {
	Resolved   bool         `json:"resolved"`
	ResolvedBy *model.Actor `json:"resolved_by"`
	ResolvedAt *string      `json:"resolved_at"`
}

// resolvedComment is what POST /api/v1/comments/{id}/resolve and POST /api/v1/comments/{id}/reopen
// answer: the comment as the write left it, and beside it Previous, the thread's resolution when
// the write took the comment's lock. A caller that undoes the write reads both here: what it reads
// of the thread before or after the write may have moved on either side of its own transaction.
type resolvedComment struct {
	shapedComment
	Previous threadResolution `json:"previous"`
}
