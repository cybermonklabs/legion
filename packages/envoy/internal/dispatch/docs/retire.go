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
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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
// through the announcement a removal sends (AnnounceDocumentRemoved), and for a document of an
// issue room the issue's room as well (AnnounceDocumentRooms), whose layout leaves a retired
// document out (roomLayoutSQL): the room reloads without its fragment, so nothing typed into that
// fragment is reconciled, versioned or indexed, and the room's block index keeps the blocks it
// recorded of the document as they stood (refs.PlanBlocks). A socket that comes back is refused
// (ServeHTTP), and api refuses an upload, a create and an edit of it 409 DOCUMENT_RETIRED.
//
// No version is written, so the approval is not left behind a later version as a write leaves
// one: the approval read answers a retired document's approval stale (api's applyApprovals), and
// no review is written, since a review would record an act no person made and a request for
// changes would wake the document's agent. The approval superseded is the one that stands, the
// document's newest review where it approves the latest version; one a later version already left
// behind was superseded by that version, and an open request for changes holds no approval.
//
// Nobody runs a retired document's checks, so it retracts each prevented_by the store asserts at a
// block of the document, as actor at the retirement's own time (retiredPreventions), and the store
// refuses any later one (graphstore's pointerRules): a risk such a check covered reads prevented
// by nothing from then on, and the check reads preventing no risk
// (spec requirement r-constraint-risk; its race with a prevented_by write: api's
// lockReasoningOwners).
//
// It takes the document's owner lock (store.LockOwner, through lockArtifactOwner) first, then
// writes the store's lock order as a create writes it: the node, its projection row, the event,
// and the node's facts under that event, the retractions among them (graphstore.WriteUncaused,
// graphstore.Caused). A document already retired is refused with ErrDocumentRetired, and a reason
// that is blank with errRetirementReason.
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
	var createdAt, at time.Time
	var retired bool
	// The role the document plays: its row's, or, for a document of an issue born converted, which
	// carries none in its row, its query node's (graphstore.ArtifactQueryRoleSQL), as the artifact
	// read answers it, so the event names the part the document played either way.
	if err := tx.QueryRow(ctx, `
		select ref_key, coalesce(role, `+graphstore.ArtifactQueryRoleSQL+`, ''), session_id, created_by, created_at, retired_at is not null, now()
		from artifacts where id = $1
	`, artifactID).Scan(&refKey, &role, &session, &createdBy, &createdAt, &retired, &at); err != nil {
		return Retirement{}, fmt.Errorf("read the document to retire: %w", err)
	}
	if retired {
		return Retirement{}, ErrDocumentRetired
	}
	var creator model.Actor
	if err := json.Unmarshal(createdBy, &creator); err != nil {
		return Retirement{}, fmt.Errorf("decode the creator of %s: %w", refKey, err)
	}
	retractions, err := retiredPreventions(ctx, tx, artifactID, refKey, at.UTC(), actor)
	if err != nil {
		return Retirement{}, err
	}
	node, err := writeRetiredNode(ctx, tx, refKey, owner.Project, session, creator, createdAt, retractions)
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
	// A converted document is edited in its issue's room, which no removal of the document's own
	// name closes: every process closes that room too, whose sockets reconnect, the document's
	// own refused, and which loads again without the retired document's fragment (roomLayoutSQL).
	room, err := roomOf(ctx, tx, artifactID)
	if err != nil {
		return Retirement{}, err
	}
	if room != artifactID {
		if err := AnnounceDocumentRooms(ctx, tx, room, artifactID); err != nil {
			return Retirement{}, err
		}
	}
	return Retirement{Event: event, SupersededApproval: superseded}, nil
}

// retiredPreventions is the retraction of every prevented_by tuple the store asserts at a block of
// the document artifactID, stored at refKey (spec requirement r-constraint-risk): nobody runs a
// retired document's checks, so no risk stays prevented by one, as the index pass retracts each
// prevented_by at a block a version drops (refs' retractGonePointers), through the same read
// (refs.PreventionRetractions). The blocks are those the document's document_blocks rows name, read
// where the block index keys them (refs.DocumentRoomBlocksSQL: under the issue room's tree for a
// document of an issue room), which a retirement keeps; a block a version dropped took its pointers
// with it then. A constrained_by out of a requirement of the document stays: what the requirement
// was held to is the record of the document, which stays readable. Each retraction is dated at and
// signed by actor, the retirement's own time and writer, and rides with the facts the retirement
// states under its document.retired event (writeRetiredNode, graphstore.Caused), in the
// retirement's transaction.
//
// It logs how many it retracts, with the document's address, and answers the number to no reader:
// the risk a check covered may be one only its author may see, and a count served beside the
// document would tell another reader it is there.
func retiredPreventions(ctx context.Context, tx pgx.Tx, artifactID, refKey string, at time.Time, actor model.Actor) ([]graphstore.Tuple, error) {
	retracted, err := refs.PreventionRetractions(ctx, tx, `array(select held.node_id from document_blocks held where `+refs.DocumentRoomBlocksSQL("held", "$1::uuid")+`)`,
		[]any{artifactID}, at, actor, graphstore.NoEvent)
	if err != nil {
		return nil, fmt.Errorf("read the prevented_by tuples at the checks of %s: %w", refKey, err)
	}
	if len(retracted) > 0 {
		slog.Info("dispatch: a retired document's checks take their prevented_by tuples with them",
			"document", refKey, "retracted", len(retracted))
	}
	return retracted, nil
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

// refusedDocument reads from q, in one statement, whether the document asked is retired
// (documentRetired) and whether it is its issue's decisions query (refs.DecisionsQuerySQL): no
// socket opens on either.
func refusedDocument(ctx context.Context, q Queryer, asked string) (retired, query bool, err error) {
	if err := q.QueryRow(ctx, `select a.retired_at is not null, `+refs.DecisionsQuerySQL("a")+` from artifacts a where a.id = $1`, asked).
		Scan(&retired, &query); err != nil {
		return false, false, fmt.Errorf("read whether the document takes a socket: %w", err)
	}
	return retired, query, nil
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

// newestApproval is the document's newest review where it is an approval, of whatever version, or
// nil (standingApproval reads whether it approves the latest).
func newestApproval(ctx context.Context, tx pgx.Tx, artifactID string) (*model.SupersededApproval, error) {
	review, err := NewestReview(ctx, tx, artifactID)
	if err != nil || review == nil || review.State != "approved" {
		return nil, err
	}
	return &model.SupersededApproval{ReviewID: review.ID, ApprovedVersion: review.Version, ApprovedBy: review.Actor, Kind: review.Kind}, nil
}
