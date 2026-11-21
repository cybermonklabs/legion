package store

// A claim's pending delivery: its writes, which the claim's own write goes with where both must
// land together, and how its kept decisions and its take's mark are held in their columns.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// PutDelivery writes the claim's pending delivery, replacing the one it had: a claim has at most
// one. A token no claim carries is refused. It is one transaction, because the write carries what
// the task names of the decisions a person made while nobody in its root's tree could hear them:
// queued or taken back, the delivery holds those rows; confirmed, it tells them (kept_decisions.go).
func (s *Store) PutDelivery(ctx context.Context, token claim.Token, d supervise.Delivery) error {
	return s.Tx(ctx, func(tx pgx.Tx) error { return putDelivery(ctx, tx, token, d) })
}

// putDelivery writes a delivery through the caller's transaction: the one PutDelivery opens, or
// the one that carries the claim the delivery is confirmed against. It takes a transaction
// because it reads the row it is about to replace and holds that reading until it has written
// (kept_decisions.go's putBackARetakenConfirmation), which no autocommit statement can do. The
// rows the task names go with it: a delivery still unconfirmed holds them, under this claim and
// this delivery id, taking each from the start row that claimed it and from whatever id the
// claim's previous delivery held it under; the write that records the confirmation tells them.
// All of it is in the caller's transaction, so no row is ever held by a delivery the store does
// not hold, and no confirmation commits without telling what it confirmed.
func putDelivery(ctx context.Context, tx pgx.Tx, token claim.Token, d supervise.Delivery) error {
	if d.Generation > math.MaxInt64 {
		return fmt.Errorf("put delivery %s on %s: generation %d does not fit a bigint", d.ID, token, d.Generation)
	}
	if d.Number > math.MaxInt64 {
		return fmt.Errorf("put delivery %s on %s: task number %d does not fit a bigint", d.ID, token, d.Number)
	}
	kept := keptDecisions(d.Kept)
	unconfirmed := d.ConfirmedAt.IsZero()
	if unconfirmed {
		if err := putBackARetakenConfirmation(ctx, tx, token, d.ID, kept); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task,
		phase, generation, entry, number, origin, taken_text, turn_started, take_told, check_look, check_opened_at,
		queued_at, confirmed_at, interrupted, out_of_memory,
		send_failures, last_send_failure_at, last_send_error, kept_decisions,
		provider_error_retries, provider_error_note, round_named)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20,
		coalesce($21::jsonb, '{}'::jsonb), $22, $23, $24)
		on conflict (claim_token) do update set delivery_id = excluded.delivery_id,
		task = excluded.task, phase = excluded.phase, generation = excluded.generation,
		entry = excluded.entry, number = excluded.number, origin = excluded.origin,
		taken_text = excluded.taken_text, turn_started = excluded.turn_started, take_told = excluded.take_told,
		check_look = excluded.check_look,
		check_opened_at = excluded.check_opened_at, queued_at = excluded.queued_at,
		confirmed_at = excluded.confirmed_at, interrupted = excluded.interrupted,
		out_of_memory = excluded.out_of_memory, send_failures = excluded.send_failures,
		last_send_failure_at = excluded.last_send_failure_at, last_send_error = excluded.last_send_error,
		kept_decisions = excluded.kept_decisions,
		provider_error_retries = excluded.provider_error_retries,
		provider_error_note = excluded.provider_error_note, round_named = excluded.round_named`,
		string(token), d.ID, d.Task, string(d.Phase), int64(d.Generation), d.Entry,
		int64(d.Number), d.Origin, d.TakenText, d.TurnStarted, takeToldColumn(d.TakeTold),
		d.CheckLook.Look, instant(d.CheckLook.OpenedAt), d.QueuedAt,
		instant(d.ConfirmedAt), d.Interrupted, d.OutOfMemory,
		d.SendFailures, instant(d.LastSendFailureAt), d.LastSendError, kept,
		d.ProviderErrorRetries, d.ProviderErrorNote, d.RoundNamed,
	)
	if err != nil {
		return fmt.Errorf("put delivery %s on %s: %w", d.ID, token, err)
	}
	if unconfirmed {
		return holdKeptDecisions(ctx, tx, token, d.ID, kept)
	}
	return tellKeptDecisions(ctx, tx, token, d.ID)
}

// PutClaimAndDelivery writes a claim and its pending delivery in one transaction: an accepted take
// records both, and a daemon that died between them would leave a claim recorded as working whose
// delivery no take could reach again — a task the worker has already taken, so its value is never
// written again and the phase waits. It is also where an accepted take tells the decisions its
// delivery holds, in that same transaction and for the same reason.
func (s *Store) PutClaimAndDelivery(ctx context.Context, c supervise.Claim, d supervise.Delivery) error {
	var queued bool
	if err := s.Tx(ctx, func(tx pgx.Tx) (err error) {
		queued, err = s.PutClaimAndDeliveryTx(ctx, tx, c, d)
		return err
	}); err != nil {
		return err
	}
	s.CommittedClaim(c, queued)
	return nil
}

// PutClaimAndDeliveryTx is PutClaimAndDelivery inside a caller's transaction, for the stop a
// project's pause makes of a claim that holds a pending delivery (PutClaimTx). It answers whether
// the claim's write queued the publication of its issue's role sessions, for the caller's
// CommittedClaim once its transaction has committed.
func (s *Store) PutClaimAndDeliveryTx(ctx context.Context, tx pgx.Tx, c supervise.Claim, d supervise.Delivery) (bool, error) {
	queued, err := s.putClaim(ctx, tx, c)
	if err != nil {
		return false, err
	}
	return queued, putDelivery(ctx, tx, c.Token, d)
}

// RetireDelivery removes the claim's pending delivery, fenced on its id: retiring a delivery the
// claim does not hold is refused, and the one it does hold stays. The claim it is given is written
// in the same transaction, because the run a claim serves is set by the retiring of the task that
// ran: a delete that committed without it would leave a daemon crashed in between with a claim
// that has forgotten the run its worker is still working. What the delivery held is left KEPT in
// that same transaction: a retired task tells nothing, so each row waits for the next start or
// catch-up of its own issue's tree to name it.
//
// A delete that removes no row is refused two ways, because pending_task_deliveries is keyed on
// the claim: the row holds no delivery at all, or it holds one under another id. The first is
// the retirement this call already committed and whose answer the caller lost, and it carries
// supervise.ErrNoSuchDelivery so that the caller can read it as its own write made good rather
// than as a fresh failure. The second is a different claim altogether — a write of another
// delivery under the claim committed after its answer was cut off, so the row holds that delivery
// while the caller is retiring the one memory holds — and it carries supervise.ErrOtherDelivery,
// naming both ids, which is not ErrNoSuchDelivery and must never be read as one: that would drop
// the delivery memory holds and leave the stored one behind, pending and unretired. Which it is can
// only be read from the row, so it is read here, in the same transaction as the delete that
// found nothing.
func (s *Store) RetireDelivery(ctx context.Context, c supervise.Claim, deliveryID string) error {
	var queued bool
	if err := s.Tx(ctx, func(tx pgx.Tx) (err error) {
		if queued, err = s.putClaim(ctx, tx, c); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			"delete from pending_task_deliveries where claim_token = $1 and delivery_id = $2",
			string(c.Token), deliveryID,
		)
		if err != nil {
			return fmt.Errorf("retire delivery %s on %s: %w", deliveryID, c.Token, err)
		}
		if tag.RowsAffected() == 0 {
			var stored string
			switch err := tx.QueryRow(ctx,
				"select delivery_id from pending_task_deliveries where claim_token = $1",
				string(c.Token),
			).Scan(&stored); {
			case errors.Is(err, pgx.ErrNoRows):
				return fmt.Errorf("retire delivery %s on %s: %w", deliveryID, c.Token, supervise.ErrNoSuchDelivery)
			case err != nil:
				return fmt.Errorf("read the pending delivery of %s: %w", c.Token, err)
			default:
				return fmt.Errorf("retire delivery %s on %s: the row holds delivery %s: %w",
					deliveryID, c.Token, stored, supervise.ErrOtherDelivery)
			}
		}
		return releaseKeptDecisions(ctx, tx, c.Token, deliveryID)
	}); err != nil {
		return err
	}
	s.CommittedClaim(c, queued)
	return nil
}

// keptDecisions is a delivery's kept-decision record as the column holds it and as the delivery
// carries it: an empty record ("{}", or nothing at all for a claim with no delivery) is a task that
// names no decision, which the delivery holds as no bytes at all, so a restored delivery equals the
// one that was written.
func keptDecisions(kept []byte) []byte {
	if len(kept) == 0 || string(kept) == "{}" {
		return nil
	}
	return kept
}

// The two names pending_task_deliveries.take_told holds, one for each of the claim's records the
// accepted take of its pending task marked told (supervise.Delivery.TakeTold): the session the
// claim abandoned and what the last let-go of its process ended. The column's check takes these
// two and no other (20261113175649_task_numbers).
const (
	toldAbandonedSession = "abandoned"
	toldEndedWork        = "ended"
)

// takeToldColumn is a delivery's TakeTold as take_told holds it: the names of the records its take
// told, and the empty array, never null, where it told none.
func takeToldColumn(told supervise.Sentences) []string {
	names := []string{}
	if told.Abandoned {
		names = append(names, toldAbandonedSession)
	}
	if told.Ended {
		names = append(names, toldEndedWork)
	}
	return names
}

// takeToldOf is take_told read back as the delivery's TakeTold; null, for a claim with no pending
// delivery, reads as a take that told nothing.
func takeToldOf(names []string) supervise.Sentences {
	return supervise.Sentences{
		Abandoned: slices.Contains(names, toldAbandonedSession),
		Ended:     slices.Contains(names, toldEndedWork),
	}
}
