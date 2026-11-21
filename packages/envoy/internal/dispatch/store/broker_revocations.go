package store

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// BrokerRevocationsChannel is the Postgres notification channel a recorded broker revocation
// announces itself on at commit (broker_revocations_notify, migration 20261116232217): the channel
// whose pglisten.Hub wake the server's revocation loop runs on.
const BrokerRevocationsChannel = "dispatch_broker_revocations"

// recordBrokerRevocationSQL records that Dispatch owes the secrets broker a revocation of the
// account $3, of the sign-in issuer $2, at $1, an address as people.email holds it, which the
// account $4 took from it: the broker ends every credential $3 holds under the address, and every
// one that records no account, whenever it issued it, refuses $3 any new one, leaves $4's, and
// deletes its record of $4 as an account the address was taken from. A people write that ends an
// account's hold on the address runs it as the last statement of its own transaction, under the
// people row's lock (PgPeopleStore.SignInSubject replacing the account the row records, of either
// member kind), so the debt commits with the change that owes it, and recorded_at is the
// database's clock as close to that commit as a statement reads it (clock_timestamp(), not the
// transaction's start, now()): the order the replacements of one address committed in, which
// their revocations are paid in (payableBrokerRevocation). An account owed one already, or paid,
// is owed again under the next generation, ordered at this commit, so a pass never settles it
// under a generation it read before; its last claim stands (tried_at), so a call of the earlier
// generation still in flight is over before anything of the address is paid again.
//
// session_generation is the address's user_sessions.generation as this statement reads it: the
// sign-in's people write moved it (people_account_ends_sessions, people_first_account_ends_sessions)
// before this, its last statement, so it is the generation the replacement left, at the insert and
// at the upsert alike (migration 20261118152422 refuses any other). The broker refuses an approve
// whose check at Dispatch read a lower one (cipher.Decision.ApproverSessionGeneration).
const recordBrokerRevocationSQL = `
	insert into broker_revocations (email, issuer, subject, holder_subject, recorded_at, session_generation)
	values ($1, $2, $3, $4, clock_timestamp(), (select generation from user_sessions where login = $1))
	on conflict (email, issuer, subject) do update
	set generation = broker_revocations.generation + 1, holder_subject = excluded.holder_subject,
		recorded_at = excluded.recorded_at, session_generation = excluded.session_generation, done_at = null
`

// settleReclaimedBrokerRevocationsSQL settles every revocation of $1, an address as people.email
// holds it, still owed for the account $3, of the sign-in issuer $2, which a sign-in replacing the
// address's account takes the address back for (PgPeopleStore.SignInSubject, in the transaction that
// records the revocation of the account it replaces, just before it). Paid later, such a revocation
// would end what the account made once it held the address again, and have the broker refuse it
// until the revocation the same sign-in records was paid, the retry window later at the least; the
// account holds the address, so nothing of it is owed. It settles a revocation a pass has claimed
// too: a call the broker did not confirm is then never tried again, and the late answer to one still
// in flight settles nothing (Settle writes only an owed row). That call cannot be recalled, so the
// broker may still end what the account made since and refuse it. A revocation of the account a
// pass paid before the account took the address back leaves the broker refusing it too. Either
// way the revocation the sign-in records, of the account it took the address from, whatever
// member kind that account's row held by then, undoes the refusal when it is paid, since a revoke
// deletes the holder's record at the broker, and it is paid after a call still in flight is over,
// since no claim takes a row of the address within the retry window after the last claim of any of
// them, a settled one included (payableBrokerRevocation). It writes done_at alone, as a settlement
// does, so it wakes nothing (broker_revocations_notify fires on generation alone).
const settleReclaimedBrokerRevocationsSQL = `
	update broker_revocations set done_at = now()
	where email = $1 and issuer = $2 and subject = $3 and done_at is null
`

// BrokerRevocations is the revocations Dispatch owes the secrets broker (migration
// 20261116232217), which the server's revocation loop claims, pays with one call to the broker
// naming the replaced account, and settles once the broker confirms it. It writes nothing a people
// write reads: the debt is recorded by the people writes themselves (recordBrokerRevocationSQL).
type BrokerRevocations struct {
	pool *Pool
}

// NewBrokerRevocations returns the owed revocations held in pool's database.
func NewBrokerRevocations(pool *Pool) *BrokerRevocations {
	return &BrokerRevocations{pool: pool}
}

// OwedBrokerRevocation is one revocation owed: the address, the replaced account (its sign-in
// issuer and its `sub`), the account that took the address from it, the generation of it a read
// saw, which is the one a settlement names, and the address's session generation that recording
// left, which the call to the broker carries: nil for a row recorded before migration
// 20261118152422 added the column, which the call then leaves out, and the address's generation for
// every recording since, which the replacing sign-in's people write made the user_sessions row for
// before the recording read it.
type OwedBrokerRevocation struct {
	Email             string
	Issuer            string
	Subject           string
	HolderSubject     string
	Generation        int64
	SessionGeneration *int64
}

// Listed is how `envoy-dispatch clear-broker-revocation <address>` prints o when it lists what an
// address owes, after "<address> owes ": the replaced account and the generation a settlement
// names, then the account's sign-in issuer, the account that took the address, and the address's
// session generation at the recording, "none" for a row recorded before migration 20261118152422
// added the column.
//
// The session generation is printed because an operator has to tell the rows apart by it. A row
// carrying none is from before migration 20261118152422: the broker refuses it on every call
// (400 SESSION_GENERATION_REQUIRED), so an operator records it again with the address's current
// generation before paying it. Settling it directly would tell the broker nothing and lose the only
// record that it is owed.
func (o OwedBrokerRevocation) Listed() string {
	generation := "none"
	if o.SessionGeneration != nil {
		generation = strconv.FormatInt(*o.SessionGeneration, 10)
	}
	return fmt.Sprintf("%s:%d (issuer %s, taken by %s, session generation %s)",
		o.Subject, o.Generation, o.Issuer, o.HolderSubject, generation)
}

// payableBrokerRevocation is the predicate, over an owed broker_revocations row r, that a claim
// may take it now, $1 seconds being the retry window: no revocation of the same address before it
// in pay order (brokerRevocationPayOrder) is owed still, and no row of the address, r's own
// included, was claimed within the window. A revocation also tells the broker to stop refusing the
// account that took the address, which may be one an earlier revocation named and which holds the
// address again; so the revocations of one address are paid one at a time, and a pass starts no
// call of an address that its own client would let run into the next claim of that address (the
// client gives a call five seconds, well inside the window). Paid out of that order, the
// revocation of A, which B took, would end the broker's refusal of B that the later revocation of
// B, which C took, had made, and the broker, which judges a revoke by its session generation,
// would answer the earlier one superseded.
//
// A row with no session generation, which only an earlier head of this branch recorded, holds back
// no other row of its address, neither by being owed nor by its claim: the broker answers it 400
// SESSION_GENERATION_REQUIRED on every call, so it would hold the address's later revocations
// forever. It is paid last (brokerRevocationPayOrder), and stays owed until an operator records it
// again, which gives it the address's generation (the runbook's record-again).
const payableBrokerRevocation = `not exists (
	select 1 from broker_revocations other
	where other.email = r.email
		and (other.session_generation is not null or (other.issuer, other.subject) = (r.issuer, r.subject))
		and (other.tried_at > now() - make_interval(secs => $1)
			or other.done_at is null
				and (coalesce(other.session_generation, 9223372036854775807), other.recorded_at, other.issuer, other.subject)
					< (coalesce(r.session_generation, 9223372036854775807), r.recorded_at, r.issuer, r.subject))
)`

// brokerRevocationPayOrder is the order an address's revocations are paid in: by the session
// generation each replacement left, the order the broker judges them by, a row recorded with none
// (by an earlier head of this branch) last; then by recorded_at, the replacing commit's moment on
// the database's clock, which alone would pay them out of order after the clock stepped back between
// two replacements; then by issuer and subject, so the order is total.
const brokerRevocationPayOrder = `coalesce(session_generation, 9223372036854775807), recorded_at, issuer, subject`

// Claim takes up to limit owed revocations a claim may take now (payableBrokerRevocation: at most
// one of an address, the first it owes in pay order, none of whose rows a pass claimed within retry), those
// never tried first, then the longest untried, and stamps each tried now, in the one statement that
// picks them, so a pass of another process woken by the same commit, or this process's next pass
// inside retry, leaves the address to this one. A row another transaction holds (a people write
// recording it again) is skipped rather than waited for: its commit wakes a pass of its own. The
// stamp is the claim's only write, so it wakes nothing (broker_revocations_notify fires on
// generation alone).
func (b *BrokerRevocations) Claim(ctx context.Context, retry time.Duration, limit int) ([]OwedBrokerRevocation, error) {
	rows, err := b.pool.Query(ctx, `
		with picked as (
			select r.email, r.issuer, r.subject from broker_revocations r
			where r.done_at is null and `+payableBrokerRevocation+`
			order by r.tried_at is not null, r.tried_at, r.recorded_at
			limit $2
			for update of r skip locked
		)
		update broker_revocations r set tried_at = now()
		from picked
		where (r.email, r.issuer, r.subject) = (picked.email, picked.issuer, picked.subject)
		returning r.email, r.issuer, r.subject, r.holder_subject, r.generation, r.session_generation
	`, retry.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("claim the owed broker revocations: %w", err)
	}
	owed, err := pgx.CollectRows(rows, pgx.RowToStructByPos[OwedBrokerRevocation])
	if err != nil {
		return nil, fmt.Errorf("claim the owed broker revocations: %w", err)
	}
	return owed, nil
}

// Settle records that the broker confirmed owed, and reports whether it did. It does only for the
// generation the claim read: the account replaced again since the claim is left owed, for a later
// pass.
func (b *BrokerRevocations) Settle(ctx context.Context, owed OwedBrokerRevocation) (bool, error) {
	tag, err := b.pool.Exec(ctx, `
		update broker_revocations set done_at = now()
		where email = $1 and issuer = $2 and subject = $3 and generation = $4 and done_at is null
	`, owed.Email, owed.Issuer, owed.Subject, owed.Generation)
	if err != nil {
		return false, fmt.Errorf("settle the broker revocation of %s: %w", owed.Email, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Sendable reports whether a pass may still call the broker for owed, which it claimed: the row is
// still owed at the generation the claim read (Settle's condition), and its claim (tried_at, on the
// database's clock) is younger than within. A pass gives within as the retry window less the broker
// client's own bound on a call, so it starts no call its own client would let run past the moment
// another process may claim the address's next revocation; a row past it is left for the next
// claim, and one another process settled since is not sent again. The broker may go on working on
// a call the client gave up on, so nothing here rests on the call itself being over: a late call
// that reaches the broker writes nothing a later revocation of the address does not overwrite
// (the broker judges a revoke by its session generation, and supersedes one below the greatest it
// applied).
func (b *BrokerRevocations) Sendable(ctx context.Context, owed OwedBrokerRevocation, within time.Duration) (bool, error) {
	var sendable bool
	err := b.pool.QueryRow(ctx, `
		select exists (select 1 from broker_revocations
			where email = $1 and issuer = $2 and subject = $3 and generation = $4 and done_at is null
				and tried_at > clock_timestamp() - make_interval(secs => $5))
	`, owed.Email, owed.Issuer, owed.Subject, owed.Generation, within.Seconds()).Scan(&sendable)
	if err != nil {
		return false, fmt.Errorf("read whether the broker revocation of %s may still be sent: %w", owed.Email, err)
	}
	return sendable, nil
}

// StillOwed reports whether owed is still owed at the generation the claim read, Settle's own
// condition: false once another pass settled it, or a sign-in recorded it again.
func (b *BrokerRevocations) StillOwed(ctx context.Context, owed OwedBrokerRevocation) (bool, error) {
	var still bool
	err := b.pool.QueryRow(ctx, `
		select exists (select 1 from broker_revocations
			where email = $1 and issuer = $2 and subject = $3 and generation = $4 and done_at is null)
	`, owed.Email, owed.Issuer, owed.Subject, owed.Generation).Scan(&still)
	if err != nil {
		return false, fmt.Errorf("read whether the broker revocation of %s is still owed: %w", owed.Email, err)
	}
	return still, nil
}

// NextDue is how long until a claim with retry can take an owed revocation, on the database's clock
// (the one Claim judges by), at or below zero for one it can take now; due is false when none is
// owed. Of an address's revocations only the first owed in pay order (brokerRevocationPayOrder) is
// counted, due retry after the last claim of any row of the address: the later ones wait for it to
// settle, and the next of them is due retry after its claim.
func (b *BrokerRevocations) NextDue(ctx context.Context, retry time.Duration) (wait time.Duration, due bool, err error) {
	var micros *int64
	if err := b.pool.QueryRow(ctx, `
		select ceil(extract(epoch from min(coalesce(
			(select max(other.tried_at) from broker_revocations other where other.email = r.email
				and (other.session_generation is not null or (other.issuer, other.subject) = (r.issuer, r.subject))) + make_interval(secs => $1),
			now()
		)) - now()) * 1000000)::bigint
		from broker_revocations r
		where r.done_at is null and not exists (
			select 1 from broker_revocations earlier
			where earlier.email = r.email and earlier.done_at is null and earlier.session_generation is not null
				and (coalesce(earlier.session_generation, 9223372036854775807), earlier.recorded_at, earlier.issuer, earlier.subject)
					< (coalesce(r.session_generation, 9223372036854775807), r.recorded_at, r.issuer, r.subject)
		)
	`, retry.Seconds()).Scan(&micros); err != nil {
		return 0, false, fmt.Errorf("read when the next broker revocation is due: %w", err)
	}
	if micros == nil {
		return 0, false, nil
	}
	return time.Duration(*micros) * time.Microsecond, true, nil
}

// Owed is how many revocations the broker has not confirmed, and how long the oldest of them has
// been owed since its replacing commit (recorded_at), both on the database's clock; oldest is zero
// when none is owed. It is what an operator watches for a revocation the broker never confirms,
// whose replaced account's credentials the broker still holds until it settles; Dispatch refuses
// them from the replacing commit either way (api.brokerPersonAdmitted).
func (b *BrokerRevocations) Owed(ctx context.Context) (count int64, oldest time.Duration, err error) {
	var micros int64
	if err := b.pool.QueryRow(ctx, `
		select count(*), coalesce(ceil(extract(epoch from now() - min(recorded_at)) * 1000000), 0)::bigint
		from broker_revocations where done_at is null
	`).Scan(&count, &micros); err != nil {
		return 0, 0, fmt.Errorf("read the owed broker revocations: %w", err)
	}
	return count, time.Duration(micros) * time.Microsecond, nil
}

// Owing is every revocation of email, an address in any case and spacing, that the broker has not
// confirmed, in the order they are paid, each with the generation this read saw: what an operator
// reads before clearing one (Clear).
func (b *BrokerRevocations) Owing(ctx context.Context, email string) ([]OwedBrokerRevocation, error) {
	rows, err := b.pool.Query(ctx, `
		select email, issuer, subject, holder_subject, generation, session_generation from broker_revocations
		where email = canonical_login($1) and done_at is null
		order by `+brokerRevocationPayOrder+`
	`, email)
	if err != nil {
		return nil, fmt.Errorf("read the owed broker revocations of %s: %w", email, err)
	}
	owed, err := pgx.CollectRows(rows, pgx.RowToStructByPos[OwedBrokerRevocation])
	if err != nil {
		return nil, fmt.Errorf("read the owed broker revocations of %s: %w", email, err)
	}
	return owed, nil
}

// BrokerRevocationSeen names one revocation of an address as an operator's read of it (Owing)
// saw it: the replaced account's `sub` and the generation the read answered.
type BrokerRevocationSeen struct {
	Subject    string
	Generation int64
}

// Clear settles the revocations of email, an address in any case and spacing, that seen names,
// without the broker: the operator's way out of one the broker never confirms (`envoy-dispatch
// clear-broker-revocation`). It settles each only at the generation seen names and only while it
// is owed, so a revocation recorded since the operator's read, or an account replaced again since,
// stays owed, and one a pass settled meanwhile is left as it is; it answers those it settled. It
// writes done_at alone, as a pass's settlement does, so it wakes nothing (broker_revocations_notify
// fires on generation alone), and a pass paying one at that moment finds it settled and writes
// nothing more (Settle writes only an owed row). It changes nothing Dispatch admits, which holds
// every broker token to its own account or creation time against people.sub_recorded_at, not to
// this table (api.brokerAccountReplaced). Clearing revokes nothing at the broker: a credential of
// the replaced account that the broker still holds goes on reading the secrets the broker serves
// without asking Dispatch, so the operator revokes it at the broker first.
func (b *BrokerRevocations) Clear(ctx context.Context, email string, seen []BrokerRevocationSeen) ([]OwedBrokerRevocation, error) {
	subjects, generations := make([]string, len(seen)), make([]int64, len(seen))
	for i, revocation := range seen {
		subjects[i], generations[i] = revocation.Subject, revocation.Generation
	}
	rows, err := b.pool.Query(ctx, `
		update broker_revocations r set done_at = now()
		from unnest($2::text[], $3::bigint[]) as seen(subject, generation)
		where r.email = canonical_login($1) and r.subject = seen.subject and r.generation = seen.generation
			and r.done_at is null
		returning r.email, r.issuer, r.subject, r.holder_subject, r.generation, r.session_generation
	`, email, subjects, generations)
	if err != nil {
		return nil, fmt.Errorf("clear the broker revocations of %s: %w", email, err)
	}
	cleared, err := pgx.CollectRows(rows, pgx.RowToStructByPos[OwedBrokerRevocation])
	if err != nil {
		return nil, fmt.Errorf("clear the broker revocations of %s: %w", email, err)
	}
	return cleared, nil
}
