package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// The broker keeps the member kind the last answer of Dispatch's person route named for each
// person in 0050's person_member_kind, so that while Dispatch cannot be asked its member-kind read
// (projectaccess.Client.MemberKind) serves a person last answered a full member as one. Every
// process writes the kind of each answer its own asks fetched, held or not, so the row keeps the
// latest by Dispatch's own clock, the answer's as_of, and never by the order the writes arrive in.
// The kind is ordered apart from the hold person_standing keeps, and served only while it is no
// older than that hold's answer (KeptMemberKind).

// KeepMemberKind writes the member kind memberKind ("" for none) that Dispatch's answer for person
// named as of asOf, unless the row already holds one Dispatch gave later. person is a login as
// record.CanonicalLogin spells it, which the table's check refuses any other spelling of.
func (s *Store) KeepMemberKind(ctx context.Context, person, memberKind string, asOf time.Time) error {
	_, err := s.Pool.Exec(ctx, `insert into person_member_kind (person, member_kind, as_of) values ($1, nullif($2, ''), $3)
		on conflict (person) do update set member_kind = excluded.member_kind, as_of = excluded.as_of
		where person_member_kind.as_of < excluded.as_of`, person, memberKind, asOf)
	return err
}

// KeptMemberKind is the member kind kept for person, as Dispatch spelled it and "" for an answer
// that named none, and the time that answer rests on (its as_of). It is also "" while the hold
// person_standing keeps for person rests on a later answer than the kind does: a broker release
// that keeps no kind wrote that answer, whose kind, a demotion among them, was not kept, so the
// earlier kind no longer stands. found is false when no kind is kept, which is no error.
func (s *Store) KeptMemberKind(ctx context.Context, person string) (kind string, asOf time.Time, found bool, err error) {
	err = s.Pool.QueryRow(ctx, `select case when standing.answered_at > kept.as_of then '' else coalesce(kept.member_kind, '') end, kept.as_of
		from person_member_kind kept left join person_standing standing on standing.person = kept.person
		where kept.person = $1`, person).Scan(&kind, &asOf)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, err
	}
	return kind, asOf, true, nil
}
