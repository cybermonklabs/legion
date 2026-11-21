package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The writes of the IAP Directory check (DISPATCH_IDENTITY=iap), migration 20261120055702: what
// each outcome of a check of a person's Workspace account stores, and the membership pass's claim
// of the next row due a check. Each is one statement dated by the database's clock_timestamp(),
// read once where it dates two columns, so they hold one time. A login with no people row is
// changed by none of them, and none makes a row. The four outcomes' statements are named below, so
// the binding columns' census (binding_columns_test.go) runs each writer's own statement, with its
// values written in, rather than a copy that can drift from it.

// recordDirectoryActiveSQL is RecordDirectoryActive's statement: $1 the email, $2 the Directory
// user id, $3 the check's read of the row (readAt).
const recordDirectoryActiveSQL = `
	update people set directory_user_id = coalesce(directory_user_id, $2),
		directory_confirmed_at = checked.at, directory_checked_at = checked.at,
		not_held_until = checked.at + interval '24 hours', held_since = null
	from (select clock_timestamp() as at) as checked
	where email = $1 and (held_since is null or held_since < $3)
	returning not_held_until
`

// recordDirectoryHeldSQL is RecordDirectoryHeld's statement: $1 the email.
const recordDirectoryHeldSQL = `
	update people set directory_checked_at = checked.at, held_since = checked.at
	from (select clock_timestamp() as at) as checked
	where email = $1
`

// recordDirectoryFailedSQL is RecordDirectoryFailed's statement: $1 the email.
const recordDirectoryFailedSQL = `update people set directory_checked_at = clock_timestamp() where email = $1`

// recordDirectoryReboundSQL is RecordDirectoryRebound's statement: $1 the email, $2 the new
// account's Directory user id, $3 the check's read of the row (readAt).
const recordDirectoryReboundSQL = `
	update people set directory_user_id = $2,
		directory_confirmed_at = checked.at, directory_checked_at = checked.at,
		not_held_until = checked.at + interval '24 hours', held_since = null
	from (select clock_timestamp() as at) as checked
	where email = $1 and (held_since is null or held_since < $3)
	returning not_held_until
`

// RecordDirectoryActive records an Active outcome of email's Directory check: the person's own
// account answered as not suspended. readAt is the database time of the check's own read of the
// row (auth.PersonMembership.ReadAt), which came before its Directory read. It sets
// directory_confirmed_at and directory_checked_at to now, not_held_until to now plus 24 hours,
// after which a not-held answer is no longer served, and held_since to null, which releases a
// recorded hold; it stores userID, the answer's Directory user id, in directory_user_id when that
// is null and keeps a stored one otherwise, so later answers are compared with the id of the
// first. It writes nothing while the row holds a hold recorded at or after readAt: a Held outcome
// of a check that read the Directory after this one did, which this older answer must not
// release. It answers the deadline it fixed, and the zero time when it wrote nothing: for a login
// with no row, or a row held since readAt.
func (s *PgPeopleStore) RecordDirectoryActive(ctx context.Context, email, userID string, readAt time.Time) (notHeldUntil time.Time, err error) {
	err = s.pool.QueryRow(ctx, recordDirectoryActiveSQL, email, userID, readAt).Scan(&notHeldUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("record an active Directory answer for %q: %w", email, err)
	}
	return notHeldUntil, nil
}

// RecordDirectoryHeld records a Held outcome of email's Directory check: the account is suspended,
// missing, or another person's. It sets directory_checked_at to now and records a hold at that
// time, held_since set to now on every Held outcome, so the row holds the time of its latest Held
// outcome. An Active write guards on that time (RecordDirectoryActive): one resting on a read made
// before this outcome writes nothing, whether or not the row was already held when that read was
// made. Every reader of held_since compares it with signed_in_at or directory_confirmed_at, which a
// later time keeps true. It leaves directory_confirmed_at and not_held_until as they are.
func (s *PgPeopleStore) RecordDirectoryHeld(ctx context.Context, email string) error {
	if _, err := s.pool.Exec(ctx, recordDirectoryHeldSQL, email); err != nil {
		return fmt.Errorf("record a Held Directory answer for %q: %w", email, err)
	}
	return nil
}

// RecordDirectoryFailed records a Failed outcome of email's Directory check, one that answered
// neither Active nor Held: it sets directory_checked_at to now and nothing else, so it changes no
// confirmation, no deadline and no hold.
func (s *PgPeopleStore) RecordDirectoryFailed(ctx context.Context, email string) error {
	if _, err := s.pool.Exec(ctx, recordDirectoryFailedSQL, email); err != nil {
		return fmt.Errorf("record a failed Directory check for %q: %w", email, err)
	}
	return nil
}

// RecordDirectoryRebound records an Active outcome of email's Directory check for an address the
// Workspace deleted and created again, once the address holds no access that outlives the browser
// (identity.Membership.checkDirectory decides that): the earlier person's credentials are gone, so
// the account now at the address is the person the row is for. It stores userID, the new
// account's Directory user id, in directory_user_id in place of the earlier one, so later answers
// are compared with it, and confirms as RecordDirectoryActive does: directory_confirmed_at and
// directory_checked_at set to now, not_held_until to now plus 24 hours, and held_since to null,
// which releases a recorded hold, writing nothing while the row holds a hold recorded at or after
// readAt, the database time of the check's own read of the row. It answers the deadline it fixed,
// and the zero time when it wrote nothing: for a login with no row, or a row held since readAt.
func (s *PgPeopleStore) RecordDirectoryRebound(ctx context.Context, email, userID string, readAt time.Time) (notHeldUntil time.Time, err error) {
	err = s.pool.QueryRow(ctx, recordDirectoryReboundSQL, email, userID, readAt).Scan(&notHeldUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("record an active Directory answer of a new account for %q: %w", email, err)
	}
	return notHeldUntil, nil
}

// ClaimDirectoryDue stamps directory_checked_at = clock_timestamp() on one full member's row that
// is due a Directory check, held rows and rows with no refresh token included: the later of its
// directory_confirmed_at and directory_checked_at, which ignores a null, is older than age, or both
// are null (coalesced to '-infinity'). The row due longest is taken first. It answers its email; ok
// is false when none is due, which ends the membership pass's Directory step. It claims as
// ClaimOverdueConfirmation does: the inner select locks the row it picks and skips a row another
// transaction holds, so two passes never claim one row.
func (s *PgPeopleStore) ClaimDirectoryDue(ctx context.Context, age time.Duration) (email string, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `
		update people set directory_checked_at = clock_timestamp()
		where email = (
			select email from people
			where member_kind = 'full'
				and coalesce(greatest(directory_confirmed_at, directory_checked_at), '-infinity') < now() - make_interval(secs => $1)
			order by coalesce(greatest(directory_confirmed_at, directory_checked_at), '-infinity'), email
			limit 1
			for update skip locked
		)
		returning email
	`, age.Seconds()).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("claim a person whose Directory check is due: %w", err)
	}
	return email, true, nil
}
