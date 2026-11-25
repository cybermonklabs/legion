package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// An approval ask's fixed options: the labels a human's answer is matched against, and the ones
// the approval request route offers.
const (
	ApprovalOptionApprove        = "Approve"
	ApprovalOptionRequestChanges = "Request changes"
)

// A review's kind: who gave it. ReviewKindPerson is a person's review, given by answering an
// approval request or from the document header; ReviewKindPlanReview is a plan review's approval
// of a plan version, which no person is asked for (the approval request route refuses a plan
// document). A review Dispatch carries to a later version keeps the kind of the review it
// carries (writeCarriedReview), so a restored plan version is still the plan review's approval.
const (
	ReviewKindPerson     = "person"
	ReviewKindPlanReview = "plan_review"
)

// ReviewColumns selects one artifact_reviews row in the order ScanReview reads it.
const ReviewColumns = `id::text, artifact_id::text, version, state, kind, actor, reason, ask_id::text, created_at, event_seq, content_sha256, content_sha256_source`

// ReviewReadColumns selects a stored review and its carry source without adding a round trip.
// The write's RETURNING list still uses ReviewColumns: a carry's row of artifact_review_carries is
// written by a statement of its own (reviewWrite.queueEvent).
const ReviewReadColumns = ReviewColumns + `, (select carried_from_version from artifact_review_carries where review_id = artifact_reviews.id)`

// ScanReview decodes one ReviewColumns row.
func ScanReview(row pgx.Row) (model.ArtifactReview, error) {
	return scanReview(row, false)
}

// ScanReviewWithCarry reads ReviewReadColumns. Only a missing carry row falls back to the
// legacy reason, so reviews stored by the previous release are readable before backfill.
func ScanReviewWithCarry(row pgx.Row) (model.ArtifactReview, error) {
	return scanReview(row, true)
}

func scanReview(row pgx.Row, withCarry bool) (model.ArtifactReview, error) {
	var review model.ArtifactReview
	var actor []byte
	dest := [...]any{&review.ID, &review.ArtifactID, &review.Version, &review.State, &review.Kind, &actor, &review.Reason, &review.AskID, &review.CreatedAt, &review.EventSeq, &review.ContentSHA256, &review.ContentSHA256Source, &review.CarriedFromVersion}
	columns := dest[:len(dest)-1]
	if withCarry {
		columns = dest[:]
	}
	if err := row.Scan(columns...); err != nil {
		return model.ArtifactReview{}, err
	}
	if err := json.Unmarshal(actor, &review.Actor); err != nil {
		return model.ArtifactReview{}, fmt.Errorf("decode review actor: %w", err)
	}
	if withCarry && review.CarriedFromVersion == nil && review.Reason != nil {
		if version, carried := CarriedFromVersion(*review.Reason); carried {
			review.CarriedFromVersion = &version
		}
	}
	return review, nil
}

// QueueReview records a review of artifact pinned to version and appends its event,
// artifact.approved or artifact.changes_requested, on the document's owner: its issue when it
// belongs to one, else the document itself. kind is who gave it (ReviewKindPerson,
// ReviewKindPlanReview). askID is the approval ask the review answers, nil
// when it was given from the document header with no request open. The review row keeps the
// event's sequence on that owner (event_seq), which places a read of the review among the owner's
// events.
//
// Every caller already holds the owner's lock before it queues this, and its statements go on one
// write phase's batch: the row is inserted before the event is appended and its sequence written
// after. The review's id is chosen here, so the statement that writes that sequence names the row,
// and takes the sequence from the event the statements before it appended (reviewEventID, which
// for a review that is no carry is store.LatestEventID): nothing is read back in Go between them.
// The update that writes the sequence runs the foreign-key checks again, since Postgres re-checks
// a row its own transaction inserted, and so asks for the same FOR KEY SHARE locks the insert
// took: the transaction already holds them, so the request never waits. Migration 0120's trigger,
// which gives the release before this one's reviews their sequence, would give this row its
// sequence too, but only at the commit, the first moment the event exists for every release; so
// the batch writes it itself, and the row is right for any read the caller makes before it
// commits. The trigger then finds the row sequenced and leaves it. written receives the review and
// its event as the batch leaves them, and the caller, which owns the transaction, publishes the
// event after its commit.
func QueueReview(b *store.WriteBatch, broker *events.Broker, artifact model.Artifact, version int, state, kind string, actor model.Actor, reason *string, askID *string, written func(model.ArtifactReview, model.Event)) error {
	review, err := newReviewWrite(artifact, version, state, kind, actor, reason, askID, nil)
	if err != nil {
		return err
	}
	review.queueRow(b)
	if err := review.queueEvent(b, broker, nil); err != nil {
		return err
	}
	review.queueCompletion(b, written)
	return nil
}

// reviewWrite is one review as its writer queues it on a write phase's batch, in three parts:
// its row (queueRow), its event with, for a carry, the row that records the carry's source and
// that event (queueEvent), and the two statements that complete each from the other - the
// event's payload with the row's digest and the row's event_seq with the event's seq
// (queueCompletion). QueueReview queues the three back to back. A carry queues its event in its
// place and the other two at the end of the write phase's batch (writeCarriedReview), after its
// version's artifact.version and version_renders row, as R14 orders them. The review's id is
// chosen here, so each statement names the row; review and event are what the statements left,
// read by their callbacks.
type reviewWrite struct {
	id          string
	artifact    model.Artifact
	version     int
	state, kind string
	actor       model.Actor
	actorJSON   []byte
	reason      *string
	askID       *string
	carriedFrom *int
	eventType   string
	review      model.ArtifactReview
	event       model.Event
}

// newReviewWrite is the write of one review of artifact at version; carriedFrom is the version a
// carry carries it from, nil for a review that is no carry.
func newReviewWrite(artifact model.Artifact, version int, state, kind string, actor model.Actor, reason *string, askID *string, carriedFrom *int) (*reviewWrite, error) {
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		return nil, fmt.Errorf("encode review actor: %w", err)
	}
	eventType := "artifact.approved"
	if state == "changes_requested" {
		eventType = "artifact.changes_requested"
	}
	return &reviewWrite{
		id: uuid.NewString(), artifact: artifact, version: version, state: state, kind: kind,
		actor: actor, actorJSON: actorJSON, reason: reason, askID: askID, carriedFrom: carriedFrom,
		eventType: eventType,
	}, nil
}

// queueRow queues the review's row. Its insert's trigger pins the digest artifact_version_text_sha256
// answers for the version at the insert (migration 20261105084017).
func (w *reviewWrite) queueRow(b *store.WriteBatch) {
	b.Queue("", `
		insert into artifact_reviews (id, artifact_id, version, state, kind, actor, reason, ask_id)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
		returning `+ReviewColumns, w.id, w.artifact.ID, w.version, w.state, w.kind, w.actorJSON, w.reason, w.askID).QueryRow(func(row pgx.Row) error {
		scanned, err := ScanReview(row)
		if err != nil {
			return fmt.Errorf("insert artifact review: %w", err)
		}
		w.review = scanned
		w.review.CarriedFromVersion = w.carriedFrom
		if w.review.ContentSHA256 == nil {
			slog.Warn("dispatch: review recorded with no content hash: the version names no stored text",
				"artifact", w.artifact.ID, "version", w.version, "review", w.review.ID)
		}
		return nil
	})
}

// queueEvent queues the review's event, and appended, where it is given, receives it as its append
// leaves it, before its payload is completed (queueCompletion). For a carry it then records the
// carry's source and the event's own id on the review's row of artifact_review_carries, in the
// statement right after the append, which names that event as store.LatestEventID: the
// statements that complete the review and the event come after the version's artifact.version
// (writeCarriedReview), where store.LatestEventID names that event instead, so they name this one
// by the id the row recorded (reviewEventID).
func (w *reviewWrite) queueEvent(b *store.WriteBatch, broker *events.Broker, appended func(model.Event)) error {
	if err := broker.QueueAppend(b, documentAskEvent(
		artifactOwner{IssueKey: w.artifact.IssueKey}, w.artifact.ID, w.eventType, w.actor,
		model.ArtifactReviewEventPayload{
			ArtifactID:         w.artifact.ID,
			Name:               w.artifact.Name,
			Version:            w.version,
			Kind:               w.kind,
			Actor:              w.actor,
			Reason:             w.reason,
			AskID:              w.askID,
			CarriedFromVersion: w.carriedFrom,
			// content_sha256 and content_sha256_source are not set here: the insert's trigger
			// derives them, and the completion writes them into this payload.
		},
	), func(event model.Event) {
		w.event = event
		if appended != nil {
			appended(event)
		}
	}); err != nil {
		return fmt.Errorf("append %s: %w", w.eventType, err)
	}
	if w.carriedFrom != nil {
		b.Queue("record the carried review's source and its event", `
			insert into artifact_review_carries (review_id, carried_from_version, event_id) values ($1, $2, `+store.LatestEventID+`)
		`, w.id, *w.carriedFrom)
	}
	return nil
}

// reviewEventID is the SQL expression of the id of the event of the review the statement's $1
// names: the id its row of artifact_review_carries recorded where the review is a carry
// (queueEvent), whose completion runs after its version's artifact.version, and else the event
// the statements before it appended (store.LatestEventID), which a review that is no carry
// completes right after its append (QueueReview). Each read is of one row by its primary key.
const reviewEventID = `coalesce((select c.event_id from artifact_review_carries c where c.review_id = $1), ` + store.LatestEventID + `)`

// queueCompletion queues the two statements that complete the review and its event, each naming
// the event by its own id (reviewEventID), and written receives both as the batch leaves them.
// The digest of the bytes the review approves is the row's, which the insert's trigger wrote
// (migration 20261105084017), and the event's payload must carry the same two values. Nothing of
// the batch is read in Go before the event is appended - the write phase sends every statement
// back to back - so the payload takes them in SQL, from the row the batch inserted, as the row's
// event_seq takes the event's sequence. A carry needs it that way: it pins a version its own
// batch writes, so no read before the batch could know the digest at all.
func (w *reviewWrite) queueCompletion(b *store.WriteBatch, written func(model.ArtifactReview, model.Event)) {
	b.Queue("record the approved bytes' digest on "+w.eventType, `
		update events set payload = payload || jsonb_build_object(
			'content_sha256', review.content_sha256,
			'content_sha256_source', review.content_sha256_source)
		from artifact_reviews review
		where review.id = $1 and events.id = `+reviewEventID+`
		returning review.content_sha256, review.content_sha256_source
	`, w.id).QueryRow(func(row pgx.Row) error {
		var digest, source *string
		if err := row.Scan(&digest, &source); err != nil {
			return fmt.Errorf("record the approved bytes' digest on %s: %w", w.eventType, err)
		}
		payload, ok := w.event.Payload.(model.ArtifactReviewEventPayload)
		if !ok {
			return fmt.Errorf("%s payload must be model.ArtifactReviewEventPayload", w.eventType)
		}
		payload.ContentSHA256, payload.ContentSHA256Source = digest, source
		w.event.Payload = payload
		return nil
	})
	b.Queue("record the sequence of "+w.eventType, `
		update artifact_reviews set event_seq = (select seq from events where id = `+reviewEventID+`)
		where id = $1
	`, w.id).Exec(func(pgconn.CommandTag) error {
		seq := int64(w.event.Seq)
		w.review.EventSeq = &seq
		written(w.review, w.event)
		return nil
	})
}

// ReviewHashFill counts rows pinned at boot and rows whose version has no stored text.
type ReviewHashFill struct{ Filled, Unhashable int }

// FillReviewContentHashes pins old reviews in bounded batches. The update trigger
// derives the true hash and marks its provenance backfill; an existing pin never moves.
func FillReviewContentHashes(ctx context.Context, q Queryer) (ReviewHashFill, error) {
	var result ReviewHashFill
	for {
		rows, err := q.Query(ctx, `
			update artifact_reviews set content_sha256 = artifact_version_text_sha256(artifact_id, version)
			where id in (
				select id from artifact_reviews
				where content_sha256 is null and artifact_version_text_sha256(artifact_id, version) is not null
				limit 500
			)`)
		if err != nil {
			return result, fmt.Errorf("fill review content hashes: %w", err)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return result, fmt.Errorf("fill review content hashes: %w", err)
		}
		filled := int(rows.CommandTag().RowsAffected())
		result.Filled += filled
		if filled == 0 {
			break
		}
	}
	if err := q.QueryRow(ctx, `select count(*) from artifact_reviews where content_sha256 is null`).Scan(&result.Unhashable); err != nil {
		return result, fmt.Errorf("count unhashable reviews: %w", err)
	}
	return result, nil
}

// latestApprovedReviewQuery selects the newest approval of the document $1 names. Like
// latestReviewByQuery it reads one row from artifact_reviews_by_artifact_event_seq (migration
// 0120), the index in model.ReviewOrder's own order, rather than sorting every review of the document.
const latestApprovedReviewQuery = `
	select ` + ReviewReadColumns + `
	from artifact_reviews
	where artifact_id = $1 and state = 'approved'
	order by ` + model.ReviewOrder + `
	limit 1
`

// latestReviewByQuery selects the newest review of the document $1 names given by the person
// whose canonical login $2 names.
const latestReviewByQuery = `
	select ` + ReviewReadColumns + `
	from artifact_reviews
	where artifact_id = $1 and actor->>'kind' = 'user' and lower(trim(actor->>'id')) = $2
	order by ` + model.ReviewOrder + `
	limit 1
`

// newestReviewQuery selects the newest review of the document $1 names, in any state. Like the
// two above it reads one row from artifact_reviews_by_artifact_event_seq (migration 0120), the
// index in model.ReviewOrder's own order, rather than sorting every review of the document.
const newestReviewQuery = `
	select ` + ReviewReadColumns + `
	from artifact_reviews
	where artifact_id = $1
	order by ` + model.ReviewOrder + `
	limit 1
`

// LatestApprovedReview is the newest approval of artifactID, whoever gave it, or nil when nobody
// has approved the document at any version.
func LatestApprovedReview(ctx context.Context, q Queryer, artifactID string) (*model.ArtifactReview, error) {
	return newestReview(ctx, q, "latest approved review", latestApprovedReviewQuery, artifactID)
}

// NewestReview is the newest review of artifactID in any state, whoever gave it, or nil when
// nobody has reviewed the document. It orders as every newest-review read does, by the order the
// reviews' events committed (model.ReviewOrder): artifact_reviews.created_at is its transaction's start
// and its id is random, so a review that committed later can carry the earlier pair - an approval
// a version write carries forward (carryApproval) against an approval committed while that write
// was open, among others.
func NewestReview(ctx context.Context, q Queryer, artifactID string) (*model.ArtifactReview, error) {
	return newestReview(ctx, q, "newest review", newestReviewQuery, artifactID)
}

// NamedDocumentsApproved reports whether every document an approval request names stands
// approved at the version the request names for it. It is the one place that rule is written:
// the approval request route asks it to decide whether a request needs a row at all, and
// carryApproval asks it to decide whether carrying one document's approval answers the open
// request. known is the one document the caller has already judged - the document the request
// was made on, or the one whose approval is being carried - and knownApproved its verdict;
// every other document is read here.
//
// A document counts as approved only when its newest review, in any state, is an approval of
// the version the request names. The newest approval alone cannot see a later request for
// changes, so a request a person answered "Request changes" on would otherwise read as settled
// and the person never be asked again about the document they rejected.
func NamedDocumentsApproved(ctx context.Context, q Queryer, documents []model.AskApprovalDocument, known string, knownApproved bool) (bool, error) {
	return namedDocumentsApproved(ctx, q, documents, known, knownApproved, nil)
}

// namedDocumentsApproved is NamedDocumentsApproved with the reviews ledger has queued and not yet
// written (Ledger.PendingReview) read in place of the stored newest review of the documents they
// review: a queued review is newer than every stored one. A nil ledger queues none.
func namedDocumentsApproved(ctx context.Context, q Queryer, documents []model.AskApprovalDocument, known string, knownApproved bool, ledger *Ledger) (bool, error) {
	for _, document := range documents {
		if document.ArtifactID == known {
			if !knownApproved {
				return false, nil
			}
			continue
		}
		var review *model.ArtifactReview
		if pending, queued := ledger.PendingReview(document.ArtifactID); queued {
			review = &pending
		} else {
			stored, err := NewestReview(ctx, q, document.ArtifactID)
			if err != nil {
				return false, err
			}
			review = stored
		}
		if review == nil || review.State != "approved" || review.Version != document.Version {
			return false, nil
		}
	}
	return true, nil
}

// latestReviewBy is the newest review of artifactID a person gave, in any state, by his canonical
// login (model.CanonicalLogin), or nil when he has reviewed the document at no version.
func latestReviewBy(ctx context.Context, q Queryer, artifactID, login string) (*model.ArtifactReview, error) {
	return newestReview(ctx, q, fmt.Sprintf("latest review by %q", login), latestReviewByQuery, artifactID, login)
}

// changesRequestedSince reports whether anyone requested changes on review's document after
// review was given, in the order the two events committed: by their sequences when both carry one,
// else a sequenced request after one that carries none, and by time and id when neither does
// (model.ReviewOrder's rule, read one row at a time).
func changesRequestedSince(ctx context.Context, q Queryer, review model.ArtifactReview) (bool, error) {
	var requested bool
	if err := q.QueryRow(ctx, `
		select exists(
			select 1 from artifact_reviews
			where artifact_id = $1 and state = 'changes_requested'
			and case
				when $4::bigint is not null and event_seq is not null then event_seq > $4::bigint
				when $4::bigint is not null then false
				when event_seq is not null then true
				else (created_at, id) > ($2, $3::uuid)
			end
		)
	`, review.ArtifactID, review.CreatedAt, review.ID, review.EventSeq).Scan(&requested); err != nil {
		return false, fmt.Errorf("read reviews since the approval: %w", err)
	}
	return requested, nil
}

// reviewedArtifact reads the document a review names as QueueReview takes it: its id, name and
// issue.
func reviewedArtifact(ctx context.Context, q Queryer, artifactID string) (model.Artifact, error) {
	artifact := model.Artifact{ID: artifactID}
	if err := q.QueryRow(ctx, `select name, issue_key from artifacts where id = $1`, artifactID).Scan(&artifact.Name, &artifact.IssueKey); err != nil {
		return model.Artifact{}, fmt.Errorf("read the document for its review: %w", err)
	}
	return artifact, nil
}

// newestReview runs a query selecting ReviewReadColumns of at most one row and returns that review,
// nil when the query selects none; what names the read in the error any other failure wraps.
func newestReview(ctx context.Context, q Queryer, what, query string, args ...any) (*model.ArtifactReview, error) {
	review, err := ScanReviewWithCarry(q.QueryRow(ctx, query, args...))
	if err == nil {
		return &review, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return nil, fmt.Errorf("read %s: %w", what, err)
}
