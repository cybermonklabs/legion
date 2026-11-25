package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Retirement is what RetireDocument did: the document.retired event it appended, which the
// caller publishes once its transaction commits, and the approval the retirement superseded, nil
// where the document held none standing.
type Retirement struct {
	Event              model.Event
	SupersededApproval *model.SupersededApproval
}

// ErrDocumentRetired refuses what a retired document no longer takes: a second retirement of it
// (it is retired once, with one document.retired event), and a live write into it, a version of it
// or a rebuild of it, which its owner's lock reads it retired under (retiredWriteRefusal,
// RetiredDocumentError).
var ErrDocumentRetired = errors.New("docs: the document is retired already")

// RetiredDocumentError is a write refused because its document is retired: a live write
// (joinLiveWrite), a version (NamedVersion, SnapshotVersion) or a rebuild (RebuildDocument), each
// refused by retiredWriteRefusal. It holds the document's id, name and slug, and the issue it
// belongs to (nil for a project document), read off the owner the write locked, and the number of
// its latest saved version (savedVersionNumber). api answers it as it answers a write it refuses
// itself: 409 DOCUMENT_RETIRED naming the document and the address of that version, or 409
// DECISIONS_ARE_NODES for an issue's decisions document (retiredDocumentRefusal). It is
// ErrDocumentRetired.
type RetiredDocumentError struct {
	ID, Name, Slug string
	IssueKey       *string
	LatestVersion  int
}

func (e *RetiredDocumentError) Error() string {
	return fmt.Sprintf("docs: document %s (%s) is retired and takes no new text", e.ID, e.Slug)
}

func (e *RetiredDocumentError) Unwrap() error { return ErrDocumentRetired }

// retiredWriteRefusal refuses a write into artifactID, whose owner lockArtifactOwner read in tx,
// the write's transaction: a RetiredDocumentError, logged at Info naming the document, where the
// read found it retired (artifactOwner.Retired), and nil where it stands. The refusal is an input a
// route answers with a documented 409 in ordinary use - an agent that still holds a retired
// decisions document's address writes to it - and no fault, so it logs no warning. The retirement
// takes the same lock, so it committed before that read or waits for the write's transaction.
func retiredWriteRefusal(ctx context.Context, tx pgx.Tx, artifactID string, owner artifactOwner) error {
	if !owner.Retired {
		return nil
	}
	latest, err := savedVersionNumber(ctx, tx, artifactID)
	if err != nil {
		return err
	}
	slog.Info("dispatch: refuse a write into a retired document", "document", artifactID)
	return &RetiredDocumentError{ID: artifactID, Name: owner.Name, Slug: owner.Slug, IssueKey: owner.IssueKey, LatestVersion: latest}
}

// errRetirementReason refuses a retirement that says not why: the reason is the event's record of
// it, and the only one.
var errRetirementReason = errors.New("docs: a retirement needs a reason")

// retirementEvents appends document.retired. Append fans out nothing (the stream is
// Publish's), so the one broker serves every retirement, and the caller publishes the event on
// its own broker after its commit.
var retirementEvents = events.NewBroker()

// RetireDocument retires the document artifactID, any document, inside tx: it sets
// artifacts.retired_at and clears the document's role, states the document's
// artifact node gone, appends one document.retired event naming the reason, and supersedes the
// approval the document holds, which it reports. The document keeps its row, its versions, its
// reviews, its comments, its block texts and its slug, so its versions stay readable and a
// citation of it stays valid: its address resolves to its issue from then on (refs'
// loadArtifactNodes), every list of its issue's or its project's documents leaves it out, and no
// role lookup finds it, its slug included.
//
// It takes no new text after it: every process closes its room of the document once tx commits,
// through the announcement a removal sends (AnnounceDocumentRemoved), a socket that comes back is
// refused (ServeHTTP), api refuses an upload, a create and an edit of it 409 DOCUMENT_RETIRED, or
// DECISIONS_ARE_NODES for an issue's decisions document (api's retiredDocumentRefusal), and this
// package refuses a live write, a version and a rebuild of it (retiredWriteRefusal), a rebuild
// only where DOCUMENT_LIVE and DOCUMENT_LOADS do not refuse it first.
//
// No version is written, so the approval is not left behind a later version as a write leaves
// one: the approval read answers a retired document's approval stale (api's applyApprovals), and
// no review is written, since a review would record an act no person made and a request for
// changes would wake the document's agent. The approval superseded is the one that stands, the
// document's newest review where it approves the latest version; one a later version already left
// behind was superseded by that version, and an open request for changes holds no approval.
//
// It takes the document's owner row first (lockArtifactOwner), then writes the store's lock order
// as a create writes it: the node, its projection row, the event, and the node's facts under that
// event (graphstore.PlanUncaused and QueueNodes, then graphstore.Caused). A document already
// retired is refused with ErrDocumentRetired, and a reason that is blank with errRetirementReason.
func RetireDocument(ctx context.Context, tx pgx.Tx, artifactID string, actor model.Actor, reason string) (Retirement, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Retirement{}, errRetirementReason
	}
	owner, _, err := lockArtifactOwner(ctx, tx, artifactID)
	if err != nil {
		return Retirement{}, err
	}
	var refKey, role string
	var session *string
	var createdBy []byte
	var createdAt time.Time
	var retired bool
	if err := tx.QueryRow(ctx, `
		select ref_key, coalesce(role, ''), session_id, created_by, created_at, retired_at is not null
		from artifacts where id = $1
	`, artifactID).Scan(&refKey, &role, &session, &createdBy, &createdAt, &retired); err != nil {
		return Retirement{}, fmt.Errorf("read the document to retire: %w", err)
	}
	if retired {
		return Retirement{}, ErrDocumentRetired
	}
	node, err := writeRetiredNode(ctx, tx, refKey, owner.Project, session, createdBy, createdAt)
	if err != nil {
		return Retirement{}, err
	}
	if _, err := tx.Exec(ctx, `update artifacts set retired_at = now(), role = null where id = $1`, artifactID); err != nil {
		return Retirement{}, fmt.Errorf("retire the document: %w", err)
	}
	superseded, err := standingApproval(ctx, tx, artifactID)
	if err != nil {
		return Retirement{}, err
	}
	event, err := retirementEvents.Append(ctx, tx, documentAskEvent(owner, artifactID, model.DocumentRetiredEventType, actor,
		model.DocumentRetiredEventPayload{
			ArtifactID: artifactID, RefKey: refKey, Name: owner.Name, Role: role, Reason: reason,
			SupersededApproval: superseded,
		}))
	if err != nil {
		return Retirement{}, fmt.Errorf("append %s: %w", model.DocumentRetiredEventType, err)
	}
	if err := graphstore.Caused(ctx, tx, node, event.ID); err != nil {
		return Retirement{}, fmt.Errorf("state the retired document's node gone: %w", err)
	}
	if err := AnnounceDocumentRemoved(ctx, tx, artifactID); err != nil {
		return Retirement{}, err
	}
	return Retirement{Event: event, SupersededApproval: superseded}, nil
}

// documentRetired is whether the document whose id is room is retired (artifacts.retired_at),
// read from q.
func documentRetired(ctx context.Context, q Queryer, room string) (bool, error) {
	var retired bool
	if err := q.QueryRow(ctx, `select retired_at is not null from artifacts where id = $1`, room).Scan(&retired); err != nil {
		return false, fmt.Errorf("read whether the document is retired: %w", err)
	}
	return retired, nil
}

// retiredUnderOwnerLock is whether the document artifactID is retired, read in the statement after
// its owner's lock (lockArtifactOwner, artifactOwner.Retired), the lock RetireDocument takes: a
// retirement that committed before the read is read, and one that has not waits for tx to end.
// Every writer that walks an issue's documents reads it before it writes into one, and skips a
// document it reads retired: it writes nothing into it, refuses nothing, and goes on with the
// issue's other documents (WriteIssueAskStates, FollowClosedIssueVersions, KeepAskBlockInStep). A
// comment thread's margin record skips a retired document as they do (ProjectMark), and so does
// the reject of a deleted comment's pending suggestion (RejectDeletedCommentSuggestion). A list
// that leaves retired documents out is read before this lock is, or under a lock its caller
// took, so it is never the read that decides.
func retiredUnderOwnerLock(ctx context.Context, tx pgx.Tx, artifactID string) (bool, error) {
	owner, _, err := lockArtifactOwner(ctx, tx, artifactID)
	if err != nil {
		return false, err
	}
	return owner.Retired, nil
}

// retiredOutsideLiveWrite is whether the document artifactID is retired, for a write that holds no
// live write to it yet: read under its owner's lock (retiredUnderOwnerLock), and false without a
// read where the transaction holds its write to the document already, which read the document
// standing under that lock (joinLiveWrite) and so no retirement passes before the transaction
// ends. ProjectMark and RejectDeletedCommentSuggestion write nothing into a retired document by it.
func retiredOutsideLiveWrite(ctx context.Context, artifactID string) (bool, error) {
	tx, joined := txFromContext(ctx)
	if !joined {
		return false, errUnjoined
	}
	if joinedLiveWrite(ctx, artifactID) != nil {
		return false, nil
	}
	return retiredUnderOwnerLock(ctx, tx, artifactID)
}

// writeRetiredNode writes the retired document's artifact node gone, held for the event that
// causes it. The node keeps every other fact the store holds of it - its supertag, its visibility,
// its project and who made it - and a document whose node no writer wrote yet takes the node its
// upload would have written, made by its creator when it was created. A ref_key past the store's
// address bound is no node, as every writer of the store leaves such an address out.
func writeRetiredNode(ctx context.Context, tx pgx.Tx, refKey, project string, session *string, createdBy []byte, createdAt time.Time) (graphstore.Uncaused, error) {
	if !graphstore.AddressFits(refKey) {
		return graphstore.Uncaused{}, nil
	}
	node, held, err := graphstore.ReadNode(ctx, tx, graphstore.KindArtifact, refKey)
	if err != nil {
		return graphstore.Uncaused{}, err
	}
	if !held {
		var creator model.Actor
		if err := json.Unmarshal(createdBy, &creator); err != nil {
			return graphstore.Uncaused{}, fmt.Errorf("decode the creator of %s: %w", refKey, err)
		}
		conversation := ""
		if session != nil {
			conversation = *session
		}
		node = graphstore.ArtifactNode(refKey, project, conversation, creator, createdAt)
	}
	node.State = graphstore.StateGone
	node.SourceEvent = graphstore.NoEvent
	write := graphstore.PlanUncaused([]graphstore.Node{node}, nil)
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueNodes(b, write)
		return nil
	}); err != nil {
		return graphstore.Uncaused{}, err
	}
	return write, nil
}

// standingApproval is the approval of artifactID that stands, the newest review where it approves
// the document's latest version (newestApproval), as the approval read answers one approved (api's
// applyApprovals), or nil.
func standingApproval(ctx context.Context, tx pgx.Tx, artifactID string) (*model.SupersededApproval, error) {
	approval, err := newestApproval(ctx, tx, artifactID)
	if err != nil || approval == nil {
		return nil, err
	}
	var latest int
	if err := tx.QueryRow(ctx, `select coalesce(max(number), 0) from artifact_versions where artifact_id = $1`, artifactID).Scan(&latest); err != nil {
		return nil, fmt.Errorf("read the latest version of the document to retire: %w", err)
	}
	if approval.ApprovedVersion != latest {
		return nil, nil
	}
	return approval, nil
}
