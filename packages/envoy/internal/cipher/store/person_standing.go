package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// The broker keeps the last answer Dispatch gave it for each person, whether Dispatch holds them,
// in 0045's person_standing, so that while Dispatch cannot be asked the hold gate
// (internal/cipher/hold) answers from it: a person last answered held stays held, and a restart or
// a second broker process answers as this one does. Every process writes the answers its own asks
// fetched, so the row keeps the latest by Dispatch's own clock, the answer's as_of, and never by
// the order the writes arrive in. 0046 keeps beside it the deadline an IAP deployment's Dispatch
// puts on a not-held answer (not_held_until), written with the answer it came with.

// PersonStanding is the last answer kept for person: whether Dispatch held them, the time its
// answer rests on (its as_of), and the deadline it named (its not_held_until), nil for none. found
// is false when no answer is kept, which is no error.
func (s *Store) PersonStanding(ctx context.Context, person string) (held bool, answeredAt time.Time, notHeldUntil *time.Time, found bool, err error) {
	err = s.Pool.QueryRow(ctx, `select held, answered_at, not_held_until from person_standing where person = $1`, person).Scan(&held, &answeredAt, &notHeldUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, time.Time{}, nil, false, nil
	}
	if err != nil {
		return false, time.Time{}, nil, false, err
	}
	return held, answeredAt, notHeldUntil, true, nil
}

// KeepPersonStanding writes Dispatch's answer for person, held as of asOf with the deadline
// notHeldUntil (nil for none), unless the row already holds an answer Dispatch gave later; the
// deadline is written with its answer or not at all, so the row's is always its answer's. person
// is a login as record.CanonicalLogin spells it, which the table's check refuses any other
// spelling of.
func (s *Store) KeepPersonStanding(ctx context.Context, person string, held bool, asOf time.Time, notHeldUntil *time.Time) error {
	_, err := s.Pool.Exec(ctx, `insert into person_standing (person, held, answered_at, not_held_until) values ($1, $2, $3, $4)
		on conflict (person) do update set held = excluded.held, answered_at = excluded.answered_at, not_held_until = excluded.not_held_until
		where person_standing.answered_at < excluded.answered_at`, person, held, asOf, notHeldUntil)
	return err
}
