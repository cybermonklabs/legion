package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// What a root architect's start task names of the decisions a person made while nobody in its
// tree could hear them (record.KeptDecisions, carried as supervise.Delivery.Kept and persisted as
// pending_task_deliveries.kept_decisions) is held by the delivery that task was queued as, and
// told at that delivery's confirmation. Those rows stand in parked_decisions throughout
// (20261101163106_parked_decisions): the claim's write of the delivery takes each from the start row that
// named it and marks it held by this claim and delivery id, the write recording the confirmation
// tells it, and the write that retires the delivery leaves it KEPT again. Each is the delivery's
// own write, so no ordering of queues, confirmations, retirements and restarts can leave a row
// held by a delivery that is not there or told by one whose turn never started.
//
// Which rows a delivery may name is decided where its task is written, never here: the task's
// composer names only the rows of the issues owed to the claim's issue on the Dispatch chain
// (record.KeptMembers, R18 of dispatch://INFRA-3320), and the send-time narrowing drops from the
// task, and releases the hold of, each named row whose issue the chain no longer shows owed to
// it (the daemon's rootTask). So these writes hold and tell exactly what the delivery names, read
// from the delivery itself, and read no tree an issue or a claim stores. An issue that moved under
// another gate root after the last send is told to the architect that held it, the lesser fault: a
// person's decision is never lost.

// putBackARetakenConfirmation puts back what this claim's own confirmation told, and does so on
// one unconfirmed write alone: the one that takes that confirmation back. The confirmation's
// write told the rows the delivery held and deleted them — a prompt refused busy after a turn of
// the agent's own confirmed it, a late refusal, a turn its process died in — so the architect
// never read them and the resend names them again; a second telling is the worst case, never a
// loss. It inserts, held by this delivery, each decision the delivery names that has no
// (issue, ref) row.
//
// Which write that is, is read from the row the write is about to replace: a delivery the claim
// holds now with a confirmation on it. So this runs before the upsert, since afterwards nothing
// says what the write undid, and the read takes that row's own lock, so no write of another
// transaction lands between the two. A claim whose row holds no delivery at all — retired, or
// never written — has no confirmation to take back, and a task that names nothing has nothing to
// put back.
//
// Every other unconfirmed write — the queue, the mark as sent, a write-back, a send a refusal
// took back — puts nothing back, because a row of theirs that is missing was told by somebody
// else: a catch-up of the root named it while no delivery held it and its telling forgot it
// (record.Store.ForgetKeptDecisionsOf). Putting such a row back makes it KEPT again, the root's
// next start or catch-up names it, and a person's decision reaches one architect twice
// (dispatch://INFRA-2339). The send-time narrowing drops such a mark from the task (the daemon's
// rootTask), but only at a send, and a task the root cannot hear yet is written many times
// before one.
func putBackARetakenConfirmation(ctx context.Context, tx pgx.Tx, token claim.Token, delivery string, kept []byte) error {
	if len(kept) == 0 {
		return nil
	}
	var confirmed bool
	switch err := tx.QueryRow(ctx, `select confirmed_at is not null from pending_task_deliveries
		where claim_token = $1 for update`, string(token)).Scan(&confirmed); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("read whether the delivery %s holds was confirmed: %w", token, err)
	}
	if !confirmed {
		return nil
	}
	if _, err := tx.Exec(ctx, `insert into parked_decisions (issue, ref, decision, actor, at, seq, artifact, version,
			held_by_claim, held_by_delivery)
		select named.issue, named.ref, named.decision, named.actor, named.at, nullif(named.seq, 0),
			coalesce(named.artifact, ''), nullif(named.version, 0), $1, $2
		from jsonb_to_recordset(coalesce($3::jsonb -> 'decisions', '[]'::jsonb))
			as named (ref text, at timestamptz, issue text, decision text, actor text, seq bigint, artifact text,
				version integer)
		where named.issue is not null and named.decision is not null and named.actor is not null
		on conflict (issue, ref) do nothing`,
		string(token), delivery, kept); err != nil {
		return fmt.Errorf("put back what the task queued on %s as delivery %s named and its confirmation told: %w", token, delivery, err)
	}
	return nil
}

// holdKeptDecisions marks every row the delivery names held by this claim and delivery id, and
// moves to this delivery id whatever the claim's previous delivery held — a task a death took back
// comes back under a new id, and what it names is the same (supervise's interrupted,
// takeBackPending). It takes only rows this delivery owns: one claimed by a start row whose task
// this claim queued (claims.delivered_rows, which the claim's write puts down first), one this
// claim already holds, or one the delivery names that nobody claims or holds (what this delivery
// names that nothing else has taken). A row another start, catch-up or delivery owns now — the
// gate root a moved issue stands under has claimed or held it — stays theirs. The approval it
// holds is the claim's own issue's.
//
// It puts no row back: a row the task names and the table does not hold is
// putBackARetakenConfirmation's, which runs before the write of the delivery this holds for and
// only where that write takes this claim's own confirmation back. A delivery that names nothing
// holds nothing.
func holdKeptDecisions(ctx context.Context, tx pgx.Tx, token claim.Token, delivery string, kept []byte) error {
	if len(kept) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `update parked_decisions set held_by_delivery = $2
		where held_by_claim = $1 and held_by_delivery is not null and held_by_delivery <> $2`,
		string(token), delivery); err != nil {
		return fmt.Errorf("move what the task of %s held to delivery %s: %w", token, delivery, err)
	}
	if _, err := tx.Exec(ctx, `update readmitted_approvals set held_by_delivery = $2
		where held_by_claim = $1 and held_by_delivery is not null and held_by_delivery <> $2`,
		string(token), delivery); err != nil {
		return fmt.Errorf("move the approval the task of %s held to delivery %s: %w", token, delivery, err)
	}
	if _, err := tx.Exec(ctx, `update parked_decisions set claimed_by = null, held_by_claim = $1, held_by_delivery = $2
		from jsonb_to_recordset(coalesce($3::jsonb -> 'decisions', '[]'::jsonb)) as named (ref text, at timestamptz),
			claims holder
		where holder.token = $1
		and parked_decisions.ref = named.ref and parked_decisions.at = named.at
		and (parked_decisions.held_by_claim = $1 or parked_decisions.claimed_by = any(holder.delivered_rows)
			or (parked_decisions.claimed_by is null and parked_decisions.held_by_claim is null))`,
		string(token), delivery, kept); err != nil {
		return fmt.Errorf("hold what the task queued on %s as delivery %s names: %w", token, delivery, err)
	}
	if _, err := tx.Exec(ctx, `update readmitted_approvals set claimed_by = null, held_by_claim = $1, held_by_delivery = $2
		from claims holder, (select ($3::jsonb ->> 'approved')::int as approved, ($3::jsonb ->> 'latest')::int as latest) as named
		where holder.token = $1 and readmitted_approvals.issue = holder.issue
		and readmitted_approvals.approved_version = named.approved and readmitted_approvals.latest_version = named.latest
		and (readmitted_approvals.held_by_claim = $1 or readmitted_approvals.claimed_by = any(holder.delivered_rows)
			or (readmitted_approvals.claimed_by is null and readmitted_approvals.held_by_claim is null))`,
		string(token), delivery, kept); err != nil {
		return fmt.Errorf("hold the approval the task queued on %s as delivery %s names: %w", token, delivery, err)
	}
	return nil
}

// tellKeptDecisions is the confirmation's own write: the turn carrying this delivery has started,
// so every row it holds has been told and is deleted. What it holds is what its task names after
// the last send's narrowing, which released every row the Dispatch chain no longer shows owed to
// the claim's issue (the daemon's rootTask), so nothing it holds is another architect's.
func tellKeptDecisions(ctx context.Context, tx pgx.Tx, token claim.Token, delivery string) error {
	for _, table := range []string{"parked_decisions", "readmitted_approvals"} {
		if _, err := tx.Exec(ctx, `delete from `+table+` where held_by_claim = $1 and held_by_delivery = $2`,
			string(token), delivery); err != nil {
			return fmt.Errorf("tell what the delivery %s on %s held: %w", delivery, token, err)
		}
	}
	return nil
}

// releaseKeptDecisions clears the hold the delivery holds on every row, which is what a retired
// delivery leaves behind: the row is KEPT, for the next start or catch-up of the gate root its
// issue is owed to to name. It clears the hold alone: a row is claimed or held, never both, and a
// claim is settled by the start or catch-up that owns it, never by a delivery's release.
func releaseKeptDecisions(ctx context.Context, tx pgx.Tx, token claim.Token, delivery string) error {
	for _, table := range []string{"parked_decisions", "readmitted_approvals"} {
		if _, err := tx.Exec(ctx, `update `+table+` set held_by_claim = null, held_by_delivery = null
			where held_by_claim = $1 and held_by_delivery = $2`, string(token), delivery); err != nil {
			return fmt.Errorf("leave what the delivery %s on %s held kept: %w", delivery, token, err)
		}
	}
	return nil
}
