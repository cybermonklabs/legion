package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Retirement is what RetireDocument did: the document.retired event it appended, which the
// caller publishes once its transaction commits, and the approval the retirement superseded, nil
// where the document held none standing.
type Retirement struct {
	Event              model.Event
	SupersededApproval *model.SupersededApproval
}

// ErrDocumentRetired refuses the retirement of a document already retired: it is retired once,
// with one document.retired event.
var ErrDocumentRetired = errors.New("docs: the document is retired already")

// errRetirementReason refuses a retirement that says not why: the reason is the event's record of
// it, and the only one.
var errRetirementReason = errors.New("docs: a retirement needs a reason")

// retirementEvents appends document.retired. Append fans out nothing (the stream is
// Publish's), so the one broker serves every retirement, and the caller publishes the event on
// its own broker after its commit.
var retirementEvents = events.NewBroker()

// RetireDocument retires the document artifactID, any document, inside tx (dispatch://INFRA-1150
// R11): it sets artifacts.retired_at and clears the document's role, states the document's
// artifact node gone, appends one document.retired event naming the reason, and supersedes the
// approval the document holds, which it reports. The document keeps its row, its versions, its
// reviews, its comments, its block texts and its slug, so its versions stay readable and a
// citation of it stays valid: its address resolves to its issue from then on (refs'
// loadArtifactNodes), every list of its issue's or its project's documents leaves it out, and no
// role lookup finds it, its slug included.
//
// It takes no new text after it: every process closes its room of the document once tx commits,
// through the announcement a removal sends (AnnounceDocumentRemoved), a socket that comes back is
// refused (ServeHTTP), and api refuses an upload, a create and an edit of it 409
// DOCUMENT_RETIRED.
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
// event (graphstore.WriteUncaused, graphstore.Caused). A document already retired is refused with
// ErrDocumentRetired, and a reason that is blank with errRetirementReason.
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
	var creator model.Actor
	if err := json.Unmarshal(createdBy, &creator); err != nil {
		return Retirement{}, fmt.Errorf("decode the creator of %s: %w", refKey, err)
	}
	node, err := writeRetiredNode(ctx, tx, refKey, owner.Project, session, creator, createdAt, nil)
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

// writeRetiredNode writes the retired document's artifact node gone, held for the event that
// causes it, with stated, the caller's own facts of the same write (graphstore.WriteUncaused). The
// node keeps every other fact the store holds of it - its supertag, its visibility, its project
// and who made it - and a document whose node no writer wrote yet takes the node its upload would
// have written, made by creator when it was created. A ref_key past the store's address bound is
// no node, as every writer of the store leaves such an address out.
func writeRetiredNode(ctx context.Context, tx pgx.Tx, refKey, project string, session *string, creator model.Actor, createdAt time.Time, stated []graphstore.Tuple) (graphstore.Uncaused, error) {
	if !graphstore.AddressFits(refKey) {
		return graphstore.Uncaused{}, nil
	}
	node, held, err := graphstore.ReadNode(ctx, tx, graphstore.KindArtifact, refKey)
	if err != nil {
		return graphstore.Uncaused{}, err
	}
	if !held {
		conversation := ""
		if session != nil {
			conversation = *session
		}
		node = graphstore.ArtifactNode(refKey, project, conversation, creator, createdAt)
	}
	node.State = graphstore.StateGone
	node.SourceEvent = graphstore.NoEvent
	return graphstore.WriteUncaused(ctx, tx, []graphstore.Node{node}, stated)
}

// WriteRemovedNode states the node of document gone inside tx, the transaction that removes the
// document's row (api's removeArtifact), as a retirement states a retired document's node
// (writeRetiredNode), and retracts, as actor at at, each changed_by_run the node states now
// (graphstore.TupleChangedByRun). The write is held for the artifact.removed event the caller
// appends, and the caller hands it to graphstore.Caused once that event is appended.
//
// A retired document keeps its row, its slug and so its ref_key, so no later document takes its
// address. A removed one frees its slug, and a document made again under the same title takes the
// same ref_key, which is the id of the same node: a create restates the node and brings it back
// live (graphstore.ArtifactNode), and a changed_by_run left asserted on it would name the new
// document changed by the run that made the old one, which refs' edgeFrom reads through the new
// document's uuid. Gone, the node is counted under no run (refs' liveVisibleNodeOf), as the run's
// list leaves out a document whose row has gone.
//
// The caller holds the document's owner row, which every writer that marks the document's node
// changed takes first, so no changed_by_run of the node lands between the read here and the
// retraction.
func WriteRemovedNode(ctx context.Context, tx pgx.Tx, document model.Artifact, actor model.Actor, at time.Time) (graphstore.Uncaused, error) {
	if !graphstore.AddressFits(document.RefKey) {
		return graphstore.Uncaused{}, nil
	}
	marks, err := graphstore.CurrentOut(ctx, tx, graphstore.KindArtifact, document.RefKey, []string{graphstore.TupleChangedByRun})
	if err != nil {
		return graphstore.Uncaused{}, err
	}
	retractions := make([]graphstore.Tuple, len(marks))
	for index, mark := range marks {
		retractions[index] = graphstore.Tuple{
			FromKind: mark.FromKind, FromID: mark.FromID, Kind: mark.Kind, ToKind: mark.ToKind, ToID: mark.ToID,
			At: at, Who: actor, Asserted: false,
		}
	}
	return writeRetiredNode(ctx, tx, document.RefKey, document.Project, document.SessionID, document.CreatedBy, document.CreatedAt, retractions)
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
