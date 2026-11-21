package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MachineLoginRevocations is the revocations of restricted members' machine logins Dispatch owes
// the secrets broker (migration 20261120150130): a removal from an access list records one in its
// own transaction (RecordMachineLoginRevocation), and the server's revocation loop claims it, pays it
// by revoking every machine login the member approved for their own machines, and settles it once
// the broker confirms every revoke, to a claim that came long enough after the removal (Settle).
//
// Dispatch's database orders the approvals of a restricted member's machine logins against these
// records, so the pass compares no clock of the broker's: an approval Dispatch relays holds the
// person's approval lock from its check that nothing is owed until the broker answers it
// (HoldMachineLoginApprovals), and a record takes the same lock first (LockMachineLoginApprovals),
// so it waits for an approval in flight and is ordered after it. While one is owed, Dispatch relays
// no approval of the person's, so every machine login of theirs the broker lists was approved
// before the record, and the pass revokes them all. An approval Dispatch gave up on before the
// broker answered may still commit after the record, while the broker's handler of it runs; so a
// row settles only to a claim that came the broker's bound on that handler after the record
// (Settle's after), whose list holds every such login. Both times are this database's.
type MachineLoginRevocations struct {
	pool *Pool
}

// NewMachineLoginRevocations returns the owed machine-login revocations held in pool's database.
func NewMachineLoginRevocations(pool *Pool) *MachineLoginRevocations {
	return &MachineLoginRevocations{pool: pool}
}

// OwedMachineLoginRevocation is one revocation owed: the address, as people.email holds it, the
// generation of it a claim read, which a settlement names, when the removal that owes it was
// recorded, and when that claim took it (tried_at), both on the database's clock. A pass's calls
// for the row all start after ClaimedAt, so it is what Settle holds the claim's list to.
type OwedMachineLoginRevocation struct {
	Email      string
	Generation int64
	RecordedAt time.Time
	ClaimedAt  time.Time
}

// machineLoginApprovalsLockSQL is the person's approval lock: $1 the address, as people.email holds
// it; %s is pg_advisory_xact_lock, which waits for it, or pg_try_advisory_xact_lock, which answers
// at once whether it took it.
const machineLoginApprovalsLockSQL = `select %s(hashtextextended('machine_login_approvals:' || lower(btrim($1)), 0))`

// LockMachineLoginApprovals takes, for the rest of tx, the person email's approval lock, waiting
// for an approval of theirs that Dispatch is relaying (HoldMachineLoginApprovals) to end. A removal
// of the person from an access list takes it before the project's owner lock
// (api.deleteProjectAccessPerson), so its wait for an approve the broker has not answered holds up
// no other write of the project; the runbook's block for a broker restored alone takes it too.
// Taking it again in tx returns at once.
func LockMachineLoginApprovals(ctx context.Context, tx pgx.Tx, email string) error {
	if _, err := tx.Exec(ctx, fmt.Sprintf(machineLoginApprovalsLockSQL, "pg_advisory_xact_lock"), email); err != nil {
		return fmt.Errorf("wait for the machine-login approvals of %s in flight: %w", email, err)
	}
	return nil
}

// RecordMachineLoginRevocation records in tx that Dispatch owes the broker the revocation of every
// machine login email approved: owed anew, under the next generation, when one is owed or paid
// already, so a pass that read an earlier one never settles this one. It takes the person's approval
// lock first (LockMachineLoginApprovals), so it waits for an approval of theirs in flight and the
// login that approval mints is one the pass lists. Its commit wakes the revocation loop
// (machine_login_revocations_notify).
func RecordMachineLoginRevocation(ctx context.Context, tx pgx.Tx, email string) error {
	if err := LockMachineLoginApprovals(ctx, tx, email); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		insert into machine_login_revocations (email) values (lower(btrim($1)))
		on conflict (email) do update
		set generation = machine_login_revocations.generation + 1, recorded_at = clock_timestamp(), done_at = null
	`, email); err != nil {
		return fmt.Errorf("record the revocation of %s's machine logins: %w", email, err)
	}
	return nil
}

// HoldMachineLoginApprovals tries to take, for the rest of tx, the person email's approval lock,
// and reports whether it took it and, when it did, whether a revocation of their machine logins is
// owed. An approval of theirs holds tx open until the broker has answered it, and is relayed only
// when it took the lock and nothing is owed: a removal that records a revocation meanwhile waits for
// it (LockMachineLoginApprovals), so the login the approval mints is one the pass lists, and once
// one is owed no approval is relayed until a pass settles it. It never waits: while another
// approval of the person's holds the lock, or a removal or the runbook's restore block does, it
// answers held false at once, so a person holds at most one database connection across a broker
// call, whatever burst of approves they send.
func HoldMachineLoginApprovals(ctx context.Context, tx pgx.Tx, email string) (held, owed bool, err error) {
	if err := tx.QueryRow(ctx, fmt.Sprintf(machineLoginApprovalsLockSQL, "pg_try_advisory_xact_lock"), email).Scan(&held); err != nil {
		return false, false, fmt.Errorf("hold the machine-login approvals of %s: %w", email, err)
	}
	if !held {
		return false, false, nil
	}
	if err := tx.QueryRow(ctx, `
		select exists (select 1 from machine_login_revocations where email = lower(btrim($1)) and done_at is null)
	`, email).Scan(&owed); err != nil {
		return true, false, fmt.Errorf("read whether the revocation of %s's machine logins is owed: %w", email, err)
	}
	return true, owed, nil
}

// Claim takes up to limit owed revocations no pass claimed within retry, those never tried first,
// then the longest untried, and stamps each tried now in the statement that picks them, so a pass of
// another process woken by the same commit leaves them to this one. A row another transaction holds
// (a removal recording it again) is skipped rather than waited for: its commit wakes a pass of its
// own.
func (m *MachineLoginRevocations) Claim(ctx context.Context, retry time.Duration, limit int) ([]OwedMachineLoginRevocation, error) {
	rows, err := m.pool.Query(ctx, `
		with picked as (
			select r.email from machine_login_revocations r
			where r.done_at is null and (r.tried_at is null or r.tried_at <= now() - make_interval(secs => $1))
			order by r.tried_at is not null, r.tried_at, r.recorded_at
			limit $2
			for update of r skip locked
		)
		update machine_login_revocations r set tried_at = now()
		from picked
		where r.email = picked.email
		returning r.email, r.generation, r.recorded_at, r.tried_at
	`, retry.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("claim the owed machine-login revocations: %w", err)
	}
	owed, err := pgx.CollectRows(rows, pgx.RowToStructByPos[OwedMachineLoginRevocation])
	if err != nil {
		return nil, fmt.Errorf("claim the owed machine-login revocations: %w", err)
	}
	return owed, nil
}

// Settle records that the broker confirmed owed, and reports whether it did. It does only for the
// generation the claim read, so a removal recorded since the claim is left owed for a later pass,
// and only for a claim that came after or more after the removal was recorded (ClaimedAt against
// recorded_at, both the database's clock), so a pass whose list may have run before the broker
// committed what the removal owes is left owed too: a later claim lists again.
func (m *MachineLoginRevocations) Settle(ctx context.Context, owed OwedMachineLoginRevocation, after time.Duration) (bool, error) {
	tag, err := m.pool.Exec(ctx, `
		update machine_login_revocations set done_at = now()
		where email = $1 and generation = $2 and done_at is null
			and recorded_at <= $3::timestamptz - make_interval(secs => $4)
	`, owed.Email, owed.Generation, owed.ClaimedAt, after.Seconds())
	if err != nil {
		return false, fmt.Errorf("settle the revocation of %s's machine logins: %w", owed.Email, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Sendable reports whether a pass may still call the broker for owed, which it claimed: the row is
// still owed at the generation the claim read (Settle's condition), and its claim (tried_at, on the
// database's clock) is younger than within. A pass gives within as the retry window less the broker
// client's own bound on a call and asks before every call it makes for the row, its list and each
// revoke, so it starts no call its own client would let run past the moment another process may
// claim the row again; a row past it is left for the next claim, which lists only the logins not
// yet revoked, and one a removal recorded again since is not sent under the earlier generation.
func (m *MachineLoginRevocations) Sendable(ctx context.Context, owed OwedMachineLoginRevocation, within time.Duration) (bool, error) {
	var sendable bool
	err := m.pool.QueryRow(ctx, `
		select exists (select 1 from machine_login_revocations
			where email = $1 and generation = $2 and done_at is null
				and tried_at > clock_timestamp() - make_interval(secs => $3))
	`, owed.Email, owed.Generation, within.Seconds()).Scan(&sendable)
	if err != nil {
		return false, fmt.Errorf("read whether the revocation of %s's machine logins may still be sent: %w", owed.Email, err)
	}
	return sendable, nil
}

// NextDue is how long until a claim with retry can take an owed revocation, on the database's
// clock (the one Claim judges by), at or below zero for one it can take now; due is false when none
// is owed.
func (m *MachineLoginRevocations) NextDue(ctx context.Context, retry time.Duration) (wait time.Duration, due bool, err error) {
	var micros *int64
	if err := m.pool.QueryRow(ctx, `
		select ceil(extract(epoch from min(coalesce(tried_at + make_interval(secs => $1), now())) - now()) * 1000000)::bigint
		from machine_login_revocations where done_at is null
	`, retry.Seconds()).Scan(&micros); err != nil {
		return 0, false, fmt.Errorf("read when the next machine-login revocation is due: %w", err)
	}
	if micros == nil {
		return 0, false, nil
	}
	return time.Duration(*micros) * time.Microsecond, true, nil
}

// Owed is how many machine-login revocations the broker has not confirmed, and how long the oldest
// of them has been owed since the removal that owes it (recorded_at), both on the database's clock;
// oldest is zero when none is owed. It is what an operator watches for a revocation the broker
// never confirms, whose member's machine logins still refresh until it settles; their trades are
// refused once the broker's cache of Dispatch's answer runs out either way.
func (m *MachineLoginRevocations) Owed(ctx context.Context) (count int64, oldest time.Duration, err error) {
	var micros int64
	if err := m.pool.QueryRow(ctx, `
		select count(*), coalesce(ceil(extract(epoch from now() - min(recorded_at)) * 1000000), 0)::bigint
		from machine_login_revocations where done_at is null
	`).Scan(&count, &micros); err != nil {
		return 0, 0, fmt.Errorf("read the owed machine-login revocations: %w", err)
	}
	return count, time.Duration(micros) * time.Microsecond, nil
}

// Owing reports whether a revocation of email's machine logins is owed, and its generation: what a
// test or an operator reads to tell a debt from a paid one.
func (m *MachineLoginRevocations) Owing(ctx context.Context, email string) (owed bool, generation int64, err error) {
	err = m.pool.QueryRow(ctx, `
		select done_at is null, generation from machine_login_revocations where email = lower(btrim($1))
	`, email).Scan(&owed, &generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("read the revocation of %s's machine logins: %w", email, err)
	}
	return owed, generation, nil
}
