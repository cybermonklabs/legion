package api

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// mentionClaimArrays is claims as the four arrays the sender's statements match a comment_deliveries
// row against: its comment, target and attempt, and the claim this sender wrote on it.
func mentionClaimArrays(claims []mentionClaim) (comments, targets []string, attempts []int32, claimed []time.Time) {
	comments = make([]string, len(claims))
	targets = make([]string, len(claims))
	attempts = make([]int32, len(claims))
	claimed = make([]time.Time, len(claims))
	for i, claim := range claims {
		comments[i], targets[i] = claim.key.comment, claim.key.target
		attempts[i], claimed[i] = int32(claim.key.attempt), claim.claimedAt
	}
	return comments, targets, attempts, claimed
}

// refreshMentionClaims is the mention sender's refresh: one statement writes the claim of every
// attempt the sender holds, and answers the claims it wrote. A row that no longer carries the
// claim this sender recorded is left alone and not answered - a try has taken it for itself, or
// another sender has, or it has settled - so refreshHeldClaims leaves that send's recorded claim
// as it was.
func (s *server) refreshMentionClaims(ctx context.Context, queued []mentionClaim) ([]mentionClaim, error) {
	comments, targets, attempts, claimed := mentionClaimArrays(queued)
	rows, err := s.deps.Store.Pool.Query(ctx, `
		update comment_deliveries d set claimed_at = now()
		from unnest($1::uuid[], $2::text[], $3::int[], $4::timestamptz[])
			as queued(comment_id, target, attempt, claimed_at)
		where d.comment_id = queued.comment_id and d.target = queued.target and d.attempt = queued.attempt
		  and d.state = 'pending' and d.claimed_at = queued.claimed_at
		returning d.comment_id::text, d.target, d.attempt, d.claimed_at
	`, comments, targets, attempts, claimed)
	if err != nil {
		return nil, err
	}
	return collectMentionClaims(rows)
}

// rereadMentionClaims is the mention sender's reread: one statement reads, and writes nothing,
// the claim of every attempt named whose row is pending and carries a claim of this sender's own:
// at or past the claim the sender recorded and less than a lease after it
// (model.DeliveryClaimSinceSQL), since another sender takes a claim only once the one on the row
// has lapsed, and so writes one more than a lease past it. The sender asks it for attempts whose
// recorded claim a refresh named and got no answer for (mentionClaimState.unsure), and records
// what it answers.
func (s *server) rereadMentionClaims(ctx context.Context, unsure []mentionClaim) ([]mentionClaim, error) {
	comments, targets, attempts, claimed := mentionClaimArrays(unsure)
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select d.comment_id::text, d.target, d.attempt, d.claimed_at
		from comment_deliveries d
		join unnest($1::uuid[], $2::text[], $3::int[], $4::timestamptz[])
			as recorded(comment_id, target, attempt, claim)
			on d.comment_id = recorded.comment_id and d.target = recorded.target and d.attempt = recorded.attempt
		where d.state = 'pending' and `+model.DeliveryClaimSinceSQL("recorded.claim"), comments, targets, attempts, claimed)
	if err != nil {
		return nil, err
	}
	return collectMentionClaims(rows)
}

// collectMentionClaims reads the claims a statement of the sender's answers.
func collectMentionClaims(rows pgx.Rows) ([]mentionClaim, error) {
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (mentionClaim, error) {
		var claim mentionClaim
		err := row.Scan(&claim.key.comment, &claim.key.target, &claim.key.attempt, &claim.claimedAt)
		return claim, err
	})
}

// releaseMentionClaims is the mention sender's release: one statement gives back the claims of the
// attempts it names, so an attempt no lane took, or none started, is pending and unclaimed. The
// sender gives a claim back only where no try of it is in flight - a send it refused, and the
// drain's after every send in flight ended - so each is scoped to a claim of this sender's own, at
// or past the claim it recorded and less than a lease after it (model.DeliveryClaimSinceSQL): the
// row may carry a refresh's claim the sender never learned (mentionClaimState.unsure), and an
// attempt another sender has taken since keeps that sender's claim.
func (s *server) releaseMentionClaims(ctx context.Context, held []mentionClaim) error {
	comments, targets, attempts, claimed := mentionClaimArrays(held)
	_, err := s.deps.Store.Pool.Exec(ctx, `
		update comment_deliveries d set claimed_at = null
		from unnest($1::uuid[], $2::text[], $3::int[], $4::timestamptz[])
			as held(comment_id, target, attempt, claim)
		where d.comment_id = held.comment_id and d.target = held.target and d.attempt = held.attempt
		  and d.state = 'pending' and `+model.DeliveryClaimSinceSQL("held.claim"), comments, targets, attempts, claimed)
	return err
}

// claimQueuedMention takes the claim on an owed attempt a keyed repeat is about to put back on a
// lane (replayComment), so the delivery sweep leaves it to this process exactly as it leaves the
// attempt a comment of this process's own queued. ok is false when another sender holds it: that
// sender owns the send, and nothing is queued for it here.
func (s *server) claimQueuedMention(ctx context.Context, key mentionKey) (claimed time.Time, ok bool, err error) {
	err = s.deps.Store.Pool.QueryRow(ctx, `
		update comment_deliveries set claimed_at = now()
		where comment_id = $1 and target = $2 and attempt = $3 and state = 'pending' and `+model.DeliveryClaimFreeSQL+`
		returning claimed_at
	`, key.comment, key.target, key.attempt).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return claimed, true, nil
}
