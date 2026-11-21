package record

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The reads below are the successors of the reads that join on the tree an issue or a claim
// stores (dispatch://INFRA-4730, R15 and R18 of dispatch://INFRA-3320): each takes the issues it is
// about from its caller, which reads them from its Chains (the issues whose gate root an issue
// is, the gate root itself, the issue's own architect), so no statement here restates a rule of
// the chain. The tree-keyed reads they replace stay while a caller still uses them, and go with
// the last one (dispatch://INFRA-4730's t5 to t7).

func (s *Postgres) SessionClaimsOf(ctx context.Context, tx pgx.Tx, issues []string, session string) (bool, error) {
	if len(issues) == 0 {
		return false, nil
	}
	var claims bool
	if err := tx.QueryRow(ctx, "select exists (select 1 from claims where issue = any($1) and session = $2 and session <> '')",
		issues, session).Scan(&claims); err != nil {
		return false, fmt.Errorf("read the claims of session %s of %v: %w", session, issues, err)
	}
	return claims, nil
}

// keptCountsFor is keptCounts over the decisions owed to one architect: the claim and the hold a
// kept row carries count only while the claiming outbox row is root's own and the holding claim is
// a claim of root, the issue whose architect the caller's members are owed to. row is the kept
// row's alias and root the placeholder that binds root's key. A void claim or hold is ignored, so
// a change of parent leaves nothing to move: the architect the row's issue is owed to now names it.
func keptCountsFor(row, root string) string {
	return `(` + row + `.claimed_by is null or not exists (
		select 1 from outbox claimer where claimer.id = ` + row + `.claimed_by and claimer.issue = ` + root + `))
		and (` + row + `.held_by_claim is null or not exists (
		select 1 from claims holder join pending_task_deliveries held on held.claim_token = holder.token
		where holder.token = ` + row + `.held_by_claim and held.delivery_id = ` + row + `.held_by_delivery
		and held.confirmed_at is null and holder.issue = ` + root + `))`
}

func (s *Postgres) ParkedDecisionsOf(ctx context.Context, tx pgx.Tx, members []string) ([]ParkedDecision, error) {
	return parkedDecisionsOf(ctx, tx, "", members, false)
}

// UnclaimedKeptOf locks every kept row of members, and root's readmitted approval, before it reads
// which of them nothing counting claims, for the reason UnclaimedKept does.
func (s *Postgres) UnclaimedKeptOf(ctx context.Context, tx pgx.Tx, root string, members []string) ([]ParkedDecision, KeptApproval, error) {
	if _, err := tx.Exec(ctx, `select 1 from parked_decisions kept where kept.issue = any($1)
		order by kept.issue, kept.ref for update of kept`, members); err != nil {
		return nil, KeptApproval{}, fmt.Errorf("lock the decisions owed to %s: %w", root, err)
	}
	if _, err := tx.Exec(ctx, `select 1 from readmitted_approvals where issue = $1 for update`, root); err != nil {
		return nil, KeptApproval{}, fmt.Errorf("lock the approval kept for %s: %w", root, err)
	}
	decisions, err := parkedDecisionsOf(ctx, tx, root, members, true)
	if err != nil {
		return nil, KeptApproval{}, err
	}
	var approval KeptApproval
	err = tx.QueryRow(ctx, `select step, approved_version, latest_version from readmitted_approvals
		where issue = $1 and `+keptCountsFor("readmitted_approvals", "$1"), root).Scan(&approval.Step, &approval.Approved, &approval.Latest)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, KeptApproval{}, fmt.Errorf("read the approval kept for %s: %w", root, err)
	}
	return decisions, approval, nil
}

// parkedDecisionsOf is every decision of members, or only those root's start or catch-up may name
// (keptCountsFor), newest first, ties by ref.
func parkedDecisionsOf(ctx context.Context, tx pgx.Tx, root string, members []string, nameable bool) ([]ParkedDecision, error) {
	rows, err := tx.Query(ctx, `select kept.issue, kept.ref, kept.decision, kept.actor, kept.at, coalesce(kept.claimed_by, 0),
		coalesce(kept.held_by_claim, ''), coalesce(kept.held_by_delivery, '')
		from parked_decisions kept
		where kept.issue = any($1) and (not $2 or (`+keptCountsFor("kept", "$3")+`)) order by kept.at desc, kept.ref`,
		members, nameable, root)
	if err != nil {
		return nil, fmt.Errorf("read the decisions of %v: %w", members, err)
	}
	decisions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ParkedDecision, error) {
		var decision ParkedDecision
		err := row.Scan(&decision.Issue, &decision.Ref, &decision.Decision, &decision.Actor, &decision.At,
			&decision.ClaimedBy, &decision.HeldBy.Claim, &decision.HeldBy.Delivery)
		return decision, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the decisions of %v: %w", members, err)
	}
	return decisions, nil
}

// ClaimKeptDecisionsOf claims for outbox row what kept names of members and nothing counting
// claims or holds, under UnclaimedKeptOf's lock, and refuses a row that names what it could not
// claim, as ClaimKeptDecisions does.
func (s *Postgres) ClaimKeptDecisionsOf(ctx context.Context, tx pgx.Tx, root string, members []string, kept KeptDecisions, row int64) error {
	if len(kept.Decisions) > 0 {
		refs, at := keptMarks(kept)
		claimed, err := tx.Exec(ctx, `update parked_decisions kept set claimed_by = $4, held_by_claim = null, held_by_delivery = null
			from unnest($2::text[], $3::timestamptz[]) as named (ref, at)
			where kept.issue = any($1) and kept.ref = named.ref and kept.at = named.at and `+keptCountsFor("kept", "$5"),
			members, refs, at, row, root)
		if err != nil {
			return fmt.Errorf("claim the decisions named to %s for outbox row %d: %w", root, row, err)
		}
		if claimed.RowsAffected() != int64(len(kept.Decisions)) {
			return fmt.Errorf("outbox row %d names %d decisions owed to %s and could claim %d: a row names only what it claims",
				row, len(kept.Decisions), root, claimed.RowsAffected())
		}
	}
	if kept.Approved > 0 {
		claimed, err := tx.Exec(ctx, `update readmitted_approvals set claimed_by = $4, held_by_claim = null, held_by_delivery = null
			where issue = $1 and approved_version = $2 and latest_version = $3 and `+keptCountsFor("readmitted_approvals", "$1"),
			root, kept.Approved, kept.Latest, row)
		if err != nil {
			return fmt.Errorf("claim the approval named to %s for outbox row %d: %w", root, row, err)
		}
		if claimed.RowsAffected() != 1 {
			return fmt.Errorf("outbox row %d names the approval kept for %s and could not claim it: a row names only what it claims", row, root)
		}
	}
	return nil
}

// ForgetKeptDecisionsOf tells what kept names of members, and leaves a named row whose issue is
// owed to another architect now to that architect, as ForgetKeptDecisions does for a tree.
func (s *Postgres) ForgetKeptDecisionsOf(ctx context.Context, tx pgx.Tx, root string, members []string, kept KeptDecisions, row int64) error {
	if len(kept.Decisions) > 0 {
		refs, at := keptMarks(kept)
		if _, err := tx.Exec(ctx, `delete from parked_decisions kept
			using unnest($2::text[], $3::timestamptz[]) as named (ref, at)
			where kept.issue = any($1) and kept.ref = named.ref and kept.at = named.at
			and (kept.claimed_by = $4 or kept.claimed_by is null)`, members, refs, at, row); err != nil {
			return fmt.Errorf("tell the decisions named to %s: %w", root, err)
		}
		if _, err := tx.Exec(ctx, `update parked_decisions kept set claimed_by = null
			from unnest($2::text[], $3::timestamptz[]) as named (ref, at)
			where not (kept.issue = any($1)) and kept.ref = named.ref and kept.at = named.at
			and kept.claimed_by = $4`, members, refs, at, row); err != nil {
			return fmt.Errorf("leave the decisions named to %s that it is no longer owed: %w", root, err)
		}
	}
	if kept.Approved > 0 {
		if _, err := tx.Exec(ctx, `delete from readmitted_approvals
			where issue = $1 and approved_version = $2 and latest_version = $3 and (claimed_by = $4 or claimed_by is null)`,
			root, kept.Approved, kept.Latest, row); err != nil {
			return fmt.Errorf("tell the approval named to %s: %w", root, err)
		}
	}
	return nil
}

func (s *Postgres) ReleasedKeptIssues(ctx context.Context, tx pgx.Tx, row int64) ([]string, error) {
	issues, err := releasedIssues(ctx, tx, `with decisions as (
		update parked_decisions set claimed_by = null where claimed_by = $1 returning issue
	), approvals as (
		update readmitted_approvals set claimed_by = null where claimed_by = $1 returning issue
	), released as (
		select issue from decisions union all select issue from approvals
	)`, row)
	if err != nil {
		return nil, fmt.Errorf("release the claims of outbox row %d: %w", row, err)
	}
	return issues, nil
}

func (s *Postgres) ReleasedHeldIssues(ctx context.Context, tx pgx.Tx, token claim.Token, delivery string, marks []DecisionMark) ([]string, error) {
	if len(marks) == 0 {
		return nil, nil
	}
	refs := make([]string, 0, len(marks))
	for _, mark := range marks {
		refs = append(refs, mark.Ref)
	}
	issues, err := releasedIssues(ctx, tx, `with released as (
		update parked_decisions set held_by_claim = null, held_by_delivery = null
		where held_by_claim = $1 and held_by_delivery = $3 and ref = any($2::text[]) returning issue
	)`, string(token), refs, delivery)
	if err != nil {
		return nil, fmt.Errorf("release what the task %s held for %s names: %w", delivery, token, err)
	}
	return issues, nil
}

// releasedIssues runs a release whose common table expressions return, under the name released,
// the issue of every row it released, and reports those issues, each once, by key.
func releasedIssues(ctx context.Context, tx pgx.Tx, release string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, release+` select distinct issue from released order by issue`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s *Postgres) ArchitectLaunchesOf(ctx context.Context, tx pgx.Tx, issues []string) (map[string]uint64, error) {
	if len(issues) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `select issue, generation from claims where role = $1 and issue = any($2::text[])`,
		string(claim.RoleArchitect), issues)
	if err != nil {
		return nil, fmt.Errorf("read the launches of the architects of %v: %w", issues, err)
	}
	defer rows.Close()
	launches := map[string]uint64{}
	for rows.Next() {
		var issue string
		var generation int64
		if err := rows.Scan(&issue, &generation); err != nil {
			return nil, fmt.Errorf("scan the launch of an architect of %v: %w", issues, err)
		}
		launches[issue] = uint64(generation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the launches of the architects of %v: %w", issues, err)
	}
	return launches, nil
}

func (s *Postgres) ArchitectServedOf(ctx context.Context, tx pgx.Tx, issue string) (uint64, int64, error) {
	var served, entry int64
	err := tx.QueryRow(ctx, `select serving_generation, served_entry from claims where role = $1 and issue = $2`,
		string(claim.RoleArchitect), issue).Scan(&served, &entry)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read what the architect of %s last served: %w", issue, err)
	}
	return uint64(served), entry, nil
}

// MembersRunOf takes the two phases and the five statuses in work from their one statement
// (phase.Admitted, InLegionsWork's phase.Done and inWorkStatuses) as parameters rather than
// spelling them into the select.
func (s *Postgres) MembersRunOf(ctx context.Context, tx pgx.Tx, members []string) (bool, error) {
	if len(members) == 0 {
		return false, nil
	}
	var run bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from issues
		where key = any($1) and phase <> $2 and phase <> $3 and status = any($4::text[]))`,
		members, string(phase.Admitted), string(phase.Done), inWorkStatuses).Scan(&run); err != nil {
		return false, fmt.Errorf("read whether one of %v stands past admitted in Legion's work: %w", members, err)
	}
	return run, nil
}
