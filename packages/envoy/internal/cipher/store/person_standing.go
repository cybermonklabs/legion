package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// The broker keeps each answer of Dispatch's person route that it fetches in two rows, which one
// write keeps together (KeepPersonAnswer): the member kind the answer named in person_member_kind,
// so that while Dispatch cannot be asked its member-kind read (projectaccess.Client.MemberKind)
// serves a person last answered a full member as one; and whether Dispatch holds the person in
// person_standing, with the deadline an IAP deployment's Dispatch puts on a not-held answer
// (not_held_until), so that the hold gate (internal/cipher/hold) answers from it: a person last
// answered held stays held, and a restart or a second broker process answers as this one does.
// Every process writes the answers its own asks fetched, so each row keeps the latest by
// Dispatch's own clock, the answer's as_of, and never by the order the writes arrive in.

// KeepPersonAnswer writes one answer of Dispatch's person route for person, as of asOf, in one
// transaction: the member kind memberKind it named ("" for none) to person_member_kind, and, where
// held is not nil, whether Dispatch holds the person with the deadline notHeldUntil (nil for none)
// to person_standing; each unless its row already holds an answer Dispatch gave later. An answer
// whose held Dispatch left out (nil) keeps its kind alone. The deadline is written with its answer
// or not at all, so a row's deadline is always its answer's. Either both rows take the answer or
// neither does. person is a login as record.CanonicalLogin spells it, which each table's check
// refuses any other spelling of.
func (s *Store) KeepPersonAnswer(ctx context.Context, person, memberKind string, held *bool, asOf time.Time, notHeldUntil *time.Time) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into person_member_kind (person, member_kind, as_of) values ($1, nullif($2, ''), $3)
			on conflict (person) do update set member_kind = excluded.member_kind, as_of = excluded.as_of
			where person_member_kind.as_of < excluded.as_of`, person, memberKind, asOf); err != nil {
			return err
		}
		if held == nil {
			return nil
		}
		_, err := tx.Exec(ctx, `insert into person_standing (person, held, answered_at, not_held_until) values ($1, $2, $3, $4)
			on conflict (person) do update set held = excluded.held, answered_at = excluded.answered_at, not_held_until = excluded.not_held_until
			where person_standing.answered_at < excluded.answered_at`, person, *held, asOf, notHeldUntil)
		return err
	})
}

// KeptMemberKind is the member kind kept for person, as Dispatch spelled it and "" for an answer
// that named none, and the time that answer rests on (its as_of). found is false when no kind is
// kept, which is no error.
func (s *Store) KeptMemberKind(ctx context.Context, person string) (kind string, asOf time.Time, found bool, err error) {
	err = s.Pool.QueryRow(ctx, `select coalesce(member_kind, ''), as_of from person_member_kind where person = $1`, person).Scan(&kind, &asOf)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, err
	}
	return kind, asOf, true, nil
}

// Standing is an answer of whether Dispatch holds a person, as person_standing keeps it.
type Standing struct {
	// Held is whether Dispatch held the person.
	Held bool
	// AsOf is the time the answer rests on (its as_of).
	AsOf time.Time
	// NotHeldUntil is the deadline the answer named (its not_held_until), nil for none.
	NotHeldUntil *time.Time
}

// PersonStanding is the last answer kept for person of whether Dispatch held them. found is false
// when no answer is kept, which is no error.
func (s *Store) PersonStanding(ctx context.Context, person string) (standing Standing, found bool, err error) {
	err = s.Pool.QueryRow(ctx, `select held, answered_at, not_held_until from person_standing where person = $1`, person).
		Scan(&standing.Held, &standing.AsOf, &standing.NotHeldUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return Standing{}, false, nil
	}
	if err != nil {
		return Standing{}, false, err
	}
	return standing, true, nil
}
