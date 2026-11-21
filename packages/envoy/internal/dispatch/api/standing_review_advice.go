package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// standingReview is the review a question's issue already carries on its specification: the
// newest review of the issue's primary document, given by a person and pinned to the version
// that document is at now. An agent opening a question on such an issue is asking about text
// someone has already read and judged, so the question's answer names the review and asks the
// sender to say what changed since. It is about the document and not about the words of the
// question, so it holds for a question worded in any way; the related-item check beside it
// (Suggestions) is the one that depends on the words.
type standingReview struct {
	// By is who reviewed, as the review records the actor's id.
	By string `json:"by"`
	// At is when the review was recorded. For a carried review this is when Dispatch recorded
	// the carry, not when the person reviewed.
	At time.Time `json:"at"`
	// State is "approved" or "changes_requested".
	State string `json:"state"`
	// Version is the version the review is pinned to, which is the document's newest.
	Version int `json:"version"`
	// Reason is the reviewer's reason cut at standingReviewReasonLimit characters, empty when
	// the review carries none, which an approval may. A carried review's reason is Dispatch's
	// own carry sentence rather than anything the person wrote.
	Reason string `json:"reason,omitempty"`
	// CarriedFromVersion is set exactly when the review is one Dispatch carried forward to this
	// version rather than one the person gave on it, and is the version the carry's reason
	// names: the version the person reviewed, or, where a carry followed a carry, the version
	// the carry before it named. It is omitted from every other review, which is what says the
	// person reviewed this version himself.
	CarriedFromVersion int `json:"carried_from_version,omitempty"`
}

// standingReviewReasonLimit is how much of the reviewer's reason the advice carries, counted in
// characters so a cut never splits one.
const standingReviewReasonLimit = 500

// standingReviewAdvice is the review the issue's specification already carries when a question
// is opened on it, or nil when it carries none and whenever the read failed: the question is
// written either way, so this reports nothing rather than refusing the write. ledger is the
// question's own, whose queued writes - a version the anchor's snapshot queued, a review that
// version's carry queued - the table holds only once Commit has run them, so the specification
// is read as the question's write leaves it.
//
// Like every other advice query this runs behind a savepoint of its own (adviceReport),
// because a failed statement aborts the transaction it ran in and a failed read must never fail
// the write it describes. It writes nothing, so the savepoint is always rolled back, which also
// restores the statement_timeout it set (endAdviceSavepoint). A savepoint that could not be
// opened or rolled back has cost the write its transaction, and is the error this answers, for
// the write to fail with.
func (s *server) standingReviewAdvice(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, route, issueKey string) (*standingReview, error) {
	return adviceReport(ctx, s, tx, route, issueKey, "standing_review", func(adviceTx pgx.Tx) (*standingReview, error) {
		return standingSpecReview(ctx, adviceTx, ledger, issueKey)
	})
}

// standingSpecReview is standingReviewAdvice's rule, over whatever queryer the caller holds: the
// newest review of the issue's primary document (docs.NewestReview, which adds no read of
// artifact_reviews to this module), reported when all three of these hold - the review is
// pinned to the document's newest version, its kind is a person's, and its actor is a person
// with no agent named beside it. An issue with no primary document, a document with no version
// and a document nobody has reviewed each report nothing, as does a review of an older version,
// a plan review, and a review an agent wrote under a person's scope.
//
// ledger, nil for a reader outside any write, holds what the caller's transaction has queued and
// not yet written: a review it queued is newer than every stored one
// (docs.Ledger.PendingReview), and a version it queued is the document's newest
// (docs.Ledger.PendingVersionNumber), so each answers for the table where it is there.
func standingSpecReview(ctx context.Context, q queryer, ledger *docs.Ledger, issueKey string) (*standingReview, error) {
	var artifactID string
	if err := q.QueryRow(ctx, `
		select id::text from artifacts where issue_key = $1 and is_primary and kind = 'doc'
	`, issueKey).Scan(&artifactID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read the issue's primary document: %w", err)
	}
	// The review is read before the version: an issue nobody has reviewed, and one whose newest
	// review is a plan review or an agent's, is answered without asking for the version number
	// at all, and those are most issues.
	review, err := newestSpecReview(ctx, q, ledger, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the document's newest review: %w", err)
	}
	if review == nil || review.Kind != docs.ReviewKindPerson {
		return nil, nil
	}
	if review.Actor.Kind != "user" || review.Actor.Act != nil {
		return nil, nil
	}
	latest, err := newestSpecVersion(ctx, q, ledger, artifactID)
	if err != nil || latest == nil || review.Version != *latest {
		return nil, err
	}
	standing := &standingReview{
		By:      review.Actor.ID,
		At:      review.CreatedAt,
		State:   review.State,
		Version: review.Version,
	}
	// A carried review's stored reason is Dispatch's own carry sentence rather than anything
	// the person wrote, so the carry is reported and the sentence is not: a reader that printed
	// it would quote Dispatch back to the sender as the reviewer's words.
	if review.Reason != nil {
		if from, carried := docs.CarriedFromVersion(*review.Reason); carried {
			standing.CarriedFromVersion = from
		} else {
			standing.Reason = firstCharacters(*review.Reason, standingReviewReasonLimit)
		}
	}
	return standing, nil
}

// newestSpecReview is the newest review of artifactID as the caller's queued writes leave it: the
// review its transaction queued where it queued one, else the newest stored one.
func newestSpecReview(ctx context.Context, q queryer, ledger *docs.Ledger, artifactID string) (*model.ArtifactReview, error) {
	if review, queued := ledger.PendingReview(artifactID); queued {
		return &review, nil
	}
	return docs.NewestReview(ctx, q, artifactID)
}

// newestSpecVersion is the newest version of artifactID as the caller's queued writes leave it:
// the version its transaction queued where it queued one, else the newest stored one, nil when
// the document has none.
func newestSpecVersion(ctx context.Context, q queryer, ledger *docs.Ledger, artifactID string) (*int, error) {
	if number, queued := ledger.PendingVersionNumber(artifactID); queued {
		return &number, nil
	}
	return latestVersionNumber(ctx, q, artifactID)
}

// firstCharacters is text cut to the first count characters, whole: a cut between two bytes of
// one rune would leave the reader a replacement character where a letter was. One pass: a text
// of count runes or fewer never reaches the cut and is returned as it is.
func firstCharacters(text string, count int) string {
	runes := 0
	for index := range text {
		if runes == count {
			return text[:index]
		}
		runes++
	}
	return text
}
