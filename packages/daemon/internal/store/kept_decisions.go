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
// The telling follows the one validity rule both sides of this machinery follow: a row is told
// only while its own issue still stands in the claim's tree. An issue that has left it — a child
// a linger handed to Legion, a parked child a person orphaned, a queued root admission adopted —
// is owed its answer by the tree it stands in now, so its row's claim and hold go instead and
// that tree's next start or catch-up names it. The old architect having been prompted with it as
// well is the lesser fault: a person's decision is never lost.

// deliveryConfirmed is whether the delivery this claim's row holds now is a confirmed one, read
// under that row's own lock, so that once a row is there no write of another transaction lands
// between this read and the write that follows it. An unconfirmed write over such a row is the
// machine taking its own confirmation back, which is the one write that puts back what that
// confirmation told (holdKeptDecisions). A claim whose row holds no delivery at all — retired, or
// never written — has no confirmation to take back.
func deliveryConfirmed(ctx context.Context, db queryRower, token claim.Token) (bool, error) {
	var confirmed bool
	switch err := db.QueryRow(ctx, `select confirmed_at is not null from pending_task_deliveries
		where claim_token = $1 for update`, string(token)).Scan(&confirmed); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read whether the delivery %s holds was confirmed: %w", token, err)
	}
	return confirmed, nil
}

// holdKeptDecisions marks every row the delivery names held by this claim and delivery id, and
// moves to this delivery id whatever the claim's previous delivery held — a task a death took back
// comes back under a new id, and what it names is the same (supervise's interrupted,
// takeBackPending). It takes only rows this delivery owns: one claimed by a start row whose task
// this claim queued (claims.delivered_rows, which the claim's write puts down first), one this
// claim already holds, or one nobody claims or holds whose issue stands in this claim's tree (what
// this delivery names of its own tree that nothing else has taken). A row another start, catch-up
// or delivery owns now — the tree a moved issue stands in has claimed or held it — stays theirs.
//
// The insert below puts a row back, and runs on one unconfirmed write alone: the one that takes
// this claim's own confirmation back (retaken, read before the write from the row it replaces).
// That confirmation's write told the rows the delivery held and deleted them — a prompt refused
// busy after a turn of the agent's own confirmed it, a late refusal, a turn its process died in —
// so the architect never read them and the resend names them again; a second telling is the worst
// case, never a loss. It inserts, held by this delivery, each decision the delivery names that
// has no (issue, ref) row and whose issue stands in the claim's tree.
//
// Every other unconfirmed write — the queue, the mark as sent, a write-back, a start row that
// runs again after its finish was lost — puts nothing back, because a row of theirs that is
// missing was told by somebody else: a catch-up of the tree named it while no delivery held it
// and its telling forgot it (record.Store.ForgetKeptDecisions). Putting such a row back makes it
// KEPT again, the tree's next start or catch-up names it, and a person's decision reaches one
// architect twice (dispatch://INFRA-2339). The send-time narrowing drops such a mark from the
// task (the daemon's rootTask), but only at a send, and a task the root cannot hear yet is
// written many times before one. An issue that left the claim's tree is never put back. A
// delivery that names nothing holds nothing.
func holdKeptDecisions(ctx context.Context, db execer, token claim.Token, delivery string, kept []byte, retaken bool) error {
	if len(kept) == 0 {
		return nil
	}
	if _, err := db.Exec(ctx, `update parked_decisions set held_by_delivery = $2
		where held_by_claim = $1 and held_by_delivery is not null and held_by_delivery <> $2`,
		string(token), delivery); err != nil {
		return fmt.Errorf("move what the task of %s held to delivery %s: %w", token, delivery, err)
	}
	if _, err := db.Exec(ctx, `update readmitted_approvals set held_by_delivery = $2
		where held_by_claim = $1 and held_by_delivery is not null and held_by_delivery <> $2`,
		string(token), delivery); err != nil {
		return fmt.Errorf("move the approval the task of %s held to delivery %s: %w", token, delivery, err)
	}
	if retaken {
		if _, err := db.Exec(ctx, `insert into parked_decisions (issue, ref, decision, actor, at, held_by_claim, held_by_delivery)
			select named.issue, named.ref, named.decision, named.actor, named.at, $1, $2
			from jsonb_to_recordset(coalesce($3::jsonb -> 'decisions', '[]'::jsonb))
				as named (ref text, at timestamptz, issue text, decision text, actor text)
			join issues owner on owner.key = named.issue
			join claims holder on holder.token = $1 and holder.tree = owner.tree
			where named.decision is not null and named.actor is not null
			on conflict (issue, ref) do nothing`,
			string(token), delivery, kept); err != nil {
			return fmt.Errorf("put back what the task queued on %s as delivery %s named and its confirmation told: %w", token, delivery, err)
		}
	}
	if _, err := db.Exec(ctx, `update parked_decisions set claimed_by = null, held_by_claim = $1, held_by_delivery = $2
		from jsonb_to_recordset(coalesce($3::jsonb -> 'decisions', '[]'::jsonb)) as named (ref text, at timestamptz),
			claims holder, issues owner
		where holder.token = $1 and owner.key = parked_decisions.issue
		and parked_decisions.ref = named.ref and parked_decisions.at = named.at
		and (parked_decisions.held_by_claim = $1 or parked_decisions.claimed_by = any(holder.delivered_rows)
			or (parked_decisions.claimed_by is null and parked_decisions.held_by_claim is null and owner.tree = holder.tree))`,
		string(token), delivery, kept); err != nil {
		return fmt.Errorf("hold what the task queued on %s as delivery %s names: %w", token, delivery, err)
	}
	if _, err := db.Exec(ctx, `update readmitted_approvals set claimed_by = null, held_by_claim = $1, held_by_delivery = $2
		from claims holder, (select ($3::jsonb ->> 'approved')::int as approved, ($3::jsonb ->> 'latest')::int as latest) as named
		where holder.token = $1 and readmitted_approvals.issue = holder.tree
		and readmitted_approvals.approved_version = named.approved and readmitted_approvals.latest_version = named.latest
		and (readmitted_approvals.held_by_claim = $1 or readmitted_approvals.claimed_by = any(holder.delivered_rows)
			or (readmitted_approvals.claimed_by is null and readmitted_approvals.held_by_claim is null))`,
		string(token), delivery, kept); err != nil {
		return fmt.Errorf("hold the approval the task queued on %s as delivery %s names: %w", token, delivery, err)
	}
	return nil
}

// tellKeptDecisions is the confirmation's own write: the turn carrying this delivery has started,
// so every row it holds whose issue still stands in the claim's tree has been told and is deleted,
// and every row whose issue has left is KEPT again for the tree it stands in now to name.
func tellKeptDecisions(ctx context.Context, db execer, token claim.Token, delivery string) error {
	for _, table := range []string{"parked_decisions", "readmitted_approvals"} {
		if _, err := db.Exec(ctx, `delete from `+table+` using claims holder, issues owner
			where holder.token = $1 and owner.key = `+table+`.issue and `+table+`.held_by_claim = $1
			and `+table+`.held_by_delivery = $2 and owner.tree = holder.tree`, string(token), delivery); err != nil {
			return fmt.Errorf("tell what the delivery %s on %s held: %w", delivery, token, err)
		}
	}
	return releaseKeptDecisions(ctx, db, token, delivery)
}

// releaseKeptDecisions clears the hold the delivery holds on every row, which is what a retired
// delivery and a told one's leftovers both leave behind: the row is KEPT, for the next start or
// catch-up of its own issue's tree to name. It clears the hold alone: a row is claimed or held,
// never both, and a claim is settled by the start or catch-up that owns it, never by a delivery's
// release.
func releaseKeptDecisions(ctx context.Context, db execer, token claim.Token, delivery string) error {
	for _, table := range []string{"parked_decisions", "readmitted_approvals"} {
		if _, err := db.Exec(ctx, `update `+table+` set held_by_claim = null, held_by_delivery = null
			where held_by_claim = $1 and held_by_delivery = $2`, string(token), delivery); err != nil {
			return fmt.Errorf("leave what the delivery %s on %s held kept: %w", delivery, token, err)
		}
	}
	return nil
}
