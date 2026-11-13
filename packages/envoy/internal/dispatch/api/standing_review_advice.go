package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
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
// written either way, so this reports nothing rather than refusing the write.
//
// Like every other advice query this runs behind a savepoint of its own (adviceSavepoint),
// because a failed statement aborts the transaction it ran in and this must never fail the write
// it describes. It writes nothing, so the savepoint is always rolled back, which also restores
// the statement_timeout it set.
func (s *server) standingReviewAdvice(ctx context.Context, tx pgx.Tx, route, issueKey string) *standingReview {
	adviceTx, ok := s.adviceSavepoint(ctx, tx, route, issueKey, "standing_review")
	if !ok {
		return nil
	}
	defer func() { _ = adviceTx.Rollback(ctx) }()
	review, err := standingSpecReview(ctx, adviceTx, issueKey)
	if err != nil {
		s.logAdviceError(route, issueKey, "standing_review", err)
		return nil
	}
	return review
}

// standingSpecReview is standingReviewAdvice's rule, over whatever queryer the caller holds: the
// newest review of the issue's primary document (docs.NewestReview, which adds no read of
// artifact_reviews to this module), reported when all three of these hold - the review is
// pinned to the document's newest version, its kind is a person's, and its actor is a person
// with no agent named beside it. An issue with no primary document, a document with no version
// and a document nobody has reviewed each report nothing, as does a review of an older version,
// a plan review, and a review an agent wrote under a person's scope.
func standingSpecReview(ctx context.Context, q queryer, issueKey string) (*standingReview, error) {
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
	review, err := docs.NewestReview(ctx, q, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the document's newest review: %w", err)
	}
	if review == nil || review.Kind != docs.ReviewKindPerson {
		return nil, nil
	}
	if review.Actor.Kind != "user" || review.Actor.Act != nil {
		return nil, nil
	}
	latest, err := latestVersionNumber(ctx, q, artifactID)
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
