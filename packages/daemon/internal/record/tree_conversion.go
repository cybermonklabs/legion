package record

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The store's half of the first boot's conversion of what the release before stored (R22 of
// dispatch://INFRA-3320, decision 21 of dispatch://INFRA-4730): the record of each step the
// conversion has committed, and the statements of the steps that read only the store. The daemon
// runs each step in a transaction of its own, in the order the decision gives, and records it in
// that same transaction (the daemon's convertStore and convertListed). Each statement reads the
// store as it stands, so a conversion run again after a rollback whose way back deleted the record
// converts what the release before wrote in between, and one that finds nothing left to convert
// writes nothing.

// TreeConversion is a project's record of its first boot's conversion (tree_conversions): the last
// numbered step that committed, 0 before the first, and when the last step ran, nil until it has.
type TreeConversion struct {
	Step        int
	ConvertedAt *time.Time
}

// Converted says whether the conversion has run its last step.
func (c TreeConversion) Converted() bool { return c.ConvertedAt != nil }

// ReadTreeConversion is the record of the conversion of the Dispatch project key project, the zero
// record where none is kept: a project whose first boot on this release has not committed a step,
// or one whose rollback's way back deleted the record.
func ReadTreeConversion(ctx context.Context, tx pgx.Tx, project string) (TreeConversion, error) {
	var conversion TreeConversion
	err := tx.QueryRow(ctx, `select step, converted_at from tree_conversions where project = $1`, project).
		Scan(&conversion.Step, &conversion.ConvertedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TreeConversion{}, nil
	}
	if err != nil {
		return TreeConversion{}, fmt.Errorf("read the conversion of %s: %w", project, err)
	}
	if conversion.ConvertedAt != nil {
		at := conversion.ConvertedAt.UTC()
		conversion.ConvertedAt = &at
	}
	return conversion, nil
}

// ReadTreeConversionOfAnyRelease is ReadTreeConversion on the store of any release: the zero
// record where the store keeps no tree_conversions, as the release before's store keeps none.
// `legion members-back`, which an operator runs before the upgrade, reads it (the daemon's
// ListMembersBack).
func ReadTreeConversionOfAnyRelease(ctx context.Context, tx pgx.Tx, project string) (TreeConversion, error) {
	var kept bool
	if err := tx.QueryRow(ctx, `select to_regclass('tree_conversions') is not null`).Scan(&kept); err != nil {
		return TreeConversion{}, fmt.Errorf("read whether the store keeps the conversion of %s: %w", project, err)
	}
	if !kept {
		return TreeConversion{}, nil
	}
	return ReadTreeConversion(ctx, tx, project)
}

// RecordTreeConversionStep records that step of project's conversion committed, in the
// transaction of the step itself; convertedAt is nil for every step but the last, which writes
// when the conversion ended.
func RecordTreeConversionStep(ctx context.Context, tx pgx.Tx, project string, step int, convertedAt *time.Time) error {
	if _, err := tx.Exec(ctx, `insert into tree_conversions (project, step, converted_at) values ($1, $2, $3)
		on conflict (project) do update set step = excluded.step, converted_at = excluded.converted_at`,
		project, step, convertedAt); err != nil {
		return fmt.Errorf("record step %d of the conversion of %s: %w", step, project, err)
	}
	return nil
}

// StampReleaseBeforeFences gives every unfinished row of project that names a fenced kind and
// lacks a part of its fence that part (FencedKinds), in SQL and with no decode of the payload,
// which the release before queued without it: the run and the phase entry its issue stands at now
// and, for a row that acts on a pod, the pod — the one its issue's claims run in for the issue's
// own cleanup and save rows (ReadFence), the one the row's own claim records for a stop. A pod no
// claim records (a tmux claim, one with no locator) stays unnamed, which fences no pod, as the row
// fenced none before. A row keyed by the project itself stands at the first fence (FirstFence).
// It answers how many rows it stamped; a row that carries its whole fence is left as it is, and so
// is every row of a kind that carries none, a claim release among them (FenceOf).
func StampReleaseBeforeFences(ctx context.Context, tx pgx.Tx, store Store, project string) (int64, error) {
	var generationKinds, entryKinds, podKinds []string
	var stopOps []string
	for _, keys := range FencedKinds() {
		if keys.Kind == OutboxKindSupervise {
			for _, op := range keys.Ops {
				stopOps = append(stopOps, string(op))
			}
			continue
		}
		if keys.Generation {
			generationKinds = append(generationKinds, string(keys.Kind))
		}
		if keys.Entry {
			entryKinds = append(entryKinds, string(keys.Kind))
		}
		if keys.PodUID {
			podKinds = append(podKinds, string(keys.Kind))
		}
	}
	rows, err := tx.Query(ctx, `select distinct issue from outbox where split_part(issue, '-', 1) = $1 and (
			((kind = any($2) or (kind = 'supervise' and payload->>'op' = any($5))) and coalesce((payload->>'generation')::bigint, 0) = 0)
			or ((kind = any($3) or (kind = 'supervise' and payload->>'op' = any($5))) and coalesce((payload->>'entry')::bigint, 0) = 0)
			or ((kind = any($4) or (kind = 'supervise' and payload->>'op' = any($5))) and coalesce(payload->>'podUid', '') = ''))
		order by issue`, project, generationKinds, entryKinds, podKinds, stopOps)
	if err != nil {
		return 0, fmt.Errorf("read the rows of %s a release before queued without their fence: %w", project, err)
	}
	issues, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("read the rows of %s a release before queued without their fence: %w", project, err)
	}
	var stamped int64
	for _, issue := range issues {
		fence, err := ReadFence(ctx, tx, store, issue, true)
		if err != nil {
			return stamped, fmt.Errorf("read the fence of %s: %w", issue, err)
		}
		tag, err := tx.Exec(ctx, `with fenced as (
				select id,
					(kind = any($2) or (kind = 'supervise' and payload->>'op' = any($5))) and coalesce((payload->>'generation')::bigint, 0) = 0 as generation,
					(kind = any($3) or (kind = 'supervise' and payload->>'op' = any($5))) and coalesce((payload->>'entry')::bigint, 0) = 0 as entry,
					case
						when coalesce(payload->>'podUid', '') <> '' then ''
						when kind = any($4) then $8::text
						when kind = 'supervise' and payload->>'op' = any($5) then coalesce((select c.locator->'sandbox'->>'podUid' from claims c
							where c.issue = outbox.issue and c.role = outbox.payload->>'role' and c.locator->>'runtime' = 'sandbox'), '')
						else ''
					end as pod
				from outbox where issue = $1
			)
			update outbox o set payload = o.payload
				|| case when f.generation then jsonb_build_object('generation', $6::bigint) else '{}'::jsonb end
				|| case when f.entry then jsonb_build_object('entry', $7::bigint) else '{}'::jsonb end
				|| case when f.pod <> '' then jsonb_build_object('podUid', f.pod) else '{}'::jsonb end
			from fenced f where f.id = o.id and (f.generation or f.entry or f.pod <> '')`,
			issue, generationKinds, entryKinds, podKinds, stopOps, int64(fence.Generation), fence.Entry, fence.PodUID)
		if err != nil {
			return stamped, fmt.Errorf("stamp the fence of the rows of %s a release before queued: %w", issue, err)
		}
		stamped += tag.RowsAffected()
	}
	return stamped, nil
}

// CarryHandedOverRoots copies, onto the record of each issue of project that a hand-over of the
// release before made a root of its own, the generation that hand-over carried it into and
// whether its old root's gate came with it (handed_over_roots, the release before's record of
// them): Issue.CarriedGeneration and Issue.CarriedGate, which the architect's start and catch-up
// read (R17 of dispatch://INFRA-3320: how a run started is a fact of the issue). The release
// before's rows stay, for that release after a rollback. It answers how many records it changed.
func CarryHandedOverRoots(ctx context.Context, tx pgx.Tx, project string) (int64, error) {
	tag, err := tx.Exec(ctx, `update issues i set carried_generation = h.generation, carried_gate = h.gate_carried
		from handed_over_roots h
		where h.issue = i.key and i.project = $1
			and (i.carried_generation, i.carried_gate) is distinct from (h.generation, h.gate_carried)`, project)
	if err != nil {
		return 0, fmt.Errorf("carry the handed-over roots of %s onto their records: %w", project, err)
	}
	return tag.RowsAffected(), nil
}

// Retained is one issue a linger of the release before covered, and the deadline it keeps it
// to (Issue.RetainedUntil).
type Retained struct {
	Issue string
	Until time.Time
}

// RetainLingers turns each linger of project that the release before had not finished into the
// retention of each issue it covered (decision 17 of dispatch://INFRA-4730): a root of its own
// stored tree whose linger deadline is ahead of now, or whose expiry is still under way (its
// linger_close row, or a tree_close or workspace_remove row of an issue of its stored tree, is
// unfinished; the conversion deletes those rows, and these retentions do their work). The root and
// each issue of its stored tree out of Legion's work keep the root's deadline as their own
// retained_until, and each gets one retention_end row due then, fenced at the run and phase entry
// it stands at (NewFencedRow), which the store's Enqueue queues. The root's linger_until is left
// as it is, for the release before. An issue already retained to that deadline and already owed
// its row is not written again. It answers each issue it retained, in key order.
func RetainLingers(ctx context.Context, tx pgx.Tx, store Store, project string, now time.Time) ([]Retained, error) {
	rows, err := tx.Query(ctx, `select covered.key, root.linger_until from issues root
		join issues covered on covered.project = root.project and covered.tree = root.key
		where root.project = $1 and root.key = root.tree and root.linger_until is not null
			and (covered.key = root.key or covered.phase = $3 or not (covered.status = any($4::text[])))
			and (root.linger_until > $2
				or exists (select 1 from outbox o where o.kind = 'linger_close' and o.issue = root.key)
				or exists (select 1 from outbox o join issues member on member.key = o.issue
					where member.project = root.project and member.tree = root.key
						and ((o.kind = 'supervise' and o.payload->>'op' = 'tree_close') or o.kind = 'workspace_remove')))
		order by covered.key`, project, now, string(phase.Done), inWorkStatuses)
	if err != nil {
		return nil, fmt.Errorf("read the lingers of %s the release before left: %w", project, err)
	}
	retained, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Retained, error) {
		var r Retained
		err := row.Scan(&r.Issue, &r.Until)
		r.Until = r.Until.UTC()
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the lingers of %s the release before left: %w", project, err)
	}
	for _, r := range retained {
		if _, err := tx.Exec(ctx, `update issues set retained_until = $2 where key = $1 and retained_until is distinct from $2`,
			r.Issue, r.Until); err != nil {
			return nil, fmt.Errorf("retain %s until %s: %w", r.Issue, r.Until, err)
		}
		var owed bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from outbox where kind = $1 and issue = $2)`,
			string(OutboxKindRetentionEnd), r.Issue).Scan(&owed); err != nil {
			return nil, fmt.Errorf("read whether the retention of %s is owed its end: %w", r.Issue, err)
		}
		if owed {
			continue
		}
		row, err := NewFencedRow(ctx, tx, store, r.Issue, RetentionEnd{}, r.Until)
		if err != nil {
			return nil, err
		}
		if err := store.Enqueue(ctx, tx, row); err != nil {
			return nil, fmt.Errorf("queue the end of the retention of %s: %w", r.Issue, err)
		}
	}
	return retained, nil
}

// HoldIssuesOfOperatorTrees makes the operator's hold of a tree of project (a tree lifecycle
// open under the operator's authority) a hold of each issue whose stored tree it is and that is
// in Legion's work (R22 of dispatch://INFRA-3320): the issue's own lifecycle stands open under the
// operator (OperatorHolds), at epoch one where it had none, and each claim of the issue that was
// bound to no epoch of it binds the one it stands at. A lifecycle of the issue whose cleanup has
// started is left as it stands. It answers how many lifecycles it opened or handed the operator.
func HoldIssuesOfOperatorTrees(ctx context.Context, tx pgx.Tx, project string) (int64, error) {
	token, err := claim.ProjectToken(project)
	if err != nil {
		return 0, fmt.Errorf("hold the issues of the operator's trees of %s: %w", project, err)
	}
	if err := treelifecycle.Serialize(ctx, tx); err != nil {
		return 0, err
	}
	var held int64
	if err := tx.QueryRow(ctx, `with held as (
			insert into issue_lifecycles (project, issue, epoch, authority)
			select $2, i.key, 1, 'operator' from issues i
			join tree_lifecycles l on l.project = $2 and l.tree = i.tree
			where i.project = $1 and i.phase <> $3 and i.status = any($4::text[])
				and l.authority = 'operator' and not l.cleanup_started
			on conflict (project, issue) do update set authority = 'operator', updated_at = now()
				where issue_lifecycles.authority <> 'operator' and not issue_lifecycles.cleanup_started
			returning issue, epoch
		), bound as (
			update claims c set issue_epoch = held.epoch from held
			where c.project = $2 and c.issue = held.issue and c.issue_epoch = 0
		)
		select count(*) from held`, project, token, string(phase.Done), inWorkStatuses).Scan(&held); err != nil {
		return 0, fmt.Errorf("hold the issues of the operator's trees of %s: %w", project, err)
	}
	return held, nil
}

// ReservedIssue is one issue a tree cleanup reservation of the release before covered, which the
// conversion made the reservation of the issue's own lifecycle (ReserveIssuesOfTreeCleanups).
type ReservedIssue struct {
	Issue     string
	Authority treelifecycle.Authority
}

// ReserveIssuesOfTreeCleanups makes each tree cleanup of project the release before reserved and
// has not confirmed the reservation of each issue of that tree's lifecycle: each issue a claim
// bound to the reserved epoch names. The issue's own lifecycle is reserved under the tree's
// authority, at the epoch it stands at, or at epoch one where it had none, and each claim of the
// issue bound to no epoch of it binds that one; a lifecycle of the issue already reserved, or whose
// cleanup confirmed, is left as it stands. So no claim of such an issue starts again until its own
// cleanup, which deletes its pod, confirms, as none of the tree's would have until the tree's.
//
// Each issue of undecided is left out: a hand-over the release before had in flight for it would
// still apply once its write landed, and whether it carries on as a root of its own or waits for a
// person is the conversion's step that reads the listing (the daemon's convertHandOvers), which
// reserves it where it does not carry on (ReserveIssueOfTreeCleanup). Its lifecycle is opened
// instead, under the tree's authority at epoch one, as the migration of 20261122154645 opens the
// lifecycle of each issue of a tree whose cleanup is not reserved, and each claim of it bound to no
// epoch binds that one; a lifecycle it already has is left as it stands.
//
// The tree's row is left as it is, for the release before. It answers each issue it reserved, in
// key order; its caller queues what finishes each (the daemon's conversion).
func ReserveIssuesOfTreeCleanups(ctx context.Context, tx pgx.Tx, project string, undecided []string) ([]ReservedIssue, error) {
	return reserveIssuesOfTreeCleanups(ctx, tx, project, "", undecided)
}

// ReserveIssueOfTreeCleanup is ReserveIssuesOfTreeCleanups for issue alone, which it reserves
// whatever hand-over it had: the conversion calls it for an issue whose hand-over it cancelled.
func ReserveIssueOfTreeCleanup(ctx context.Context, tx pgx.Tx, project, issue string) ([]ReservedIssue, error) {
	return reserveIssuesOfTreeCleanups(ctx, tx, project, issue, nil)
}

// reserveIssuesOfTreeCleanups is the work of both: over only, or every issue where only is empty,
// it reserves each issue covered and opens each of undecided.
func reserveIssuesOfTreeCleanups(ctx context.Context, tx pgx.Tx, project, only string, undecided []string) ([]ReservedIssue, error) {
	token, err := claim.ProjectToken(project)
	if err != nil {
		return nil, fmt.Errorf("reserve the issues of the tree cleanups of %s: %w", project, err)
	}
	if err := treelifecycle.Serialize(ctx, tx); err != nil {
		return nil, err
	}
	if undecided == nil {
		undecided = []string{}
	}
	rows, err := tx.Query(ctx, `with covered as (
			select distinct on (c.issue) c.issue, l.authority, c.issue = any($3::text[]) as undecided from claims c
			join tree_lifecycles l on l.project = c.project and l.tree = c.tree and l.epoch = c.tree_epoch
			where c.project = $1 and c.issue <> '' and ($2 = '' or c.issue = $2)
				and l.cleanup_started and l.cleanup_confirmed_at is null
			order by c.issue, l.authority = 'workflow' desc
		), reserved as (
			insert into issue_lifecycles (project, issue, epoch, authority, cleanup_started)
			select $1, issue, 1, authority, true from covered where not undecided
			on conflict (project, issue) do update set cleanup_started = true, authority = excluded.authority, updated_at = now()
				where not issue_lifecycles.cleanup_started
			returning issue, epoch, authority
		), opened as (
			insert into issue_lifecycles (project, issue, epoch, authority)
			select $1, issue, 1, authority from covered where undecided
			on conflict (project, issue) do nothing
			returning issue, epoch
		), bound as (
			update claims c set issue_epoch = held.epoch
			from (select issue, epoch from reserved union all select issue, epoch from opened) held
			where c.project = $1 and c.issue = held.issue and c.issue_epoch = 0
		)
		select issue, authority from reserved order by issue`, token, only, undecided)
	if err != nil {
		return nil, fmt.Errorf("reserve the issues of the tree cleanups of %s: %w", project, err)
	}
	reserved, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ReservedIssue, error) {
		var issue ReservedIssue
		var authority string
		err := row.Scan(&issue.Issue, &authority)
		issue.Authority = treelifecycle.Authority(authority)
		return issue, err
	})
	if err != nil {
		return nil, fmt.Errorf("reserve the issues of the tree cleanups of %s: %w", project, err)
	}
	return reserved, nil
}

// CarryTreeDraws copies each tree-unit experiment assignment a root of project holds onto each
// issue of its stored tree that has started and holds no row of that experiment of its own,
// verbatim and marked carried, under the write-once insert (PutAssignments), so a launch that reads
// the issue's own rows keeps the arm the issue has been running under (R17 of
// dispatch://INFRA-3320). It answers how many rows it wrote.
func CarryTreeDraws(ctx context.Context, tx pgx.Tx, project string) (int64, error) {
	tag, err := tx.Exec(ctx, `insert into experiment_assignments (issue_key, experiment, unit, variant, mode, drawn_at, carried)
		select member.key, source.experiment, source.unit, source.variant, source.mode, source.drawn_at, true
		from issues member
		join experiment_assignments source on source.issue_key = member.tree and source.unit = 'tree'
		where member.project = $1 and member.key <> member.tree and member.phase <> $2
		on conflict (issue_key, experiment) do nothing`, project, string(phase.Admitted))
	if err != nil {
		return 0, fmt.Errorf("carry the tree draws of %s onto their issues: %w", project, err)
	}
	return tag.RowsAffected(), nil
}

// MarkClaimsOfGateHolders marks as holding its gate (supervise.Claim.HoldsGate) each claim of
// project that no start of this release has marked (its launch_tree is empty, as
// supervise.Claim.LaunchedTree reads it) and whose issue holds its own gate: the issue's record
// holds the mark (Issue.HoldsGate), or the claim names its issue as the tree it stores, the release
// before's rule for a root (claim.IsTreeRoot). The claim's stored tree counts beside the record
// because a record the release before wrote while a rollback held takes the mark's default, false,
// even where that release ran it as a root. The project controller's claim names no issue and is
// left as it is. It marks and never clears, so a claim already marked, and one a start of this
// release wrote (its launch_tree set), is written nothing. It answers how many claims it marked.
func MarkClaimsOfGateHolders(ctx context.Context, tx pgx.Tx, project string) (int64, error) {
	token, err := claim.ProjectToken(project)
	if err != nil {
		return 0, fmt.Errorf("mark the claims of %s whose issue holds its own gate: %w", project, err)
	}
	tag, err := tx.Exec(ctx, `update claims c set holds_gate = true
		where c.project = $1 and c.issue <> '' and c.launch_tree = '' and not c.holds_gate
			and (c.issue = c.tree or exists (select 1 from issues i where i.key = c.issue and i.holds_gate))`, token)
	if err != nil {
		return 0, fmt.Errorf("mark the claims of %s whose issue holds its own gate: %w", project, err)
	}
	return tag.RowsAffected(), nil
}

// ReleaseBeforeHandover is one hand-over the release before had in flight (its handovers table):
// the child it handed to Legion as a root of its own, the tree it left, the generation of the
// linger whose close handed it over, and whether a return of that tree's root cancelled it.
type ReleaseBeforeHandover struct {
	Issue      string
	Tree       string
	Generation uint64
	Cancelled  bool
}

// ReleaseBeforeHandovers is every hand-over of project the release before had in flight, in key
// order. No decision of this release makes one; the first boot's conversion finishes or cancels
// each (DeleteReleaseBeforeHandover).
func ReleaseBeforeHandovers(ctx context.Context, tx pgx.Tx, project string) ([]ReleaseBeforeHandover, error) {
	rows, err := tx.Query(ctx, `select h.issue, h.tree, h.linger_generation, h.cancelled from handovers h
		join issues i on i.key = h.issue where i.project = $1 order by h.issue`, project)
	if err != nil {
		return nil, fmt.Errorf("read the hand-overs of %s: %w", project, err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ReleaseBeforeHandover, error) {
		var handover ReleaseBeforeHandover
		var generation int64
		err := row.Scan(&handover.Issue, &handover.Tree, &generation, &handover.Cancelled)
		handover.Generation = uint64(generation)
		return handover, err
	})
}

// DeleteReleaseBeforeHandover deletes the release before's hand-over of issue, once the
// conversion has finished or cancelled it.
func DeleteReleaseBeforeHandover(ctx context.Context, tx pgx.Tx, issue string) error {
	if _, err := tx.Exec(ctx, `delete from handovers where issue = $1`, issue); err != nil {
		return fmt.Errorf("delete the hand-over of %s: %w", issue, err)
	}
	return nil
}

// DeleteReleaseBeforeRows deletes the rows of project that only the release before runs: its
// linger_close rows, its supervise rows of op tree_close, its workspace_remove rows of a linger,
// and its hand-over status rows (the status rows whose kind is "hand-over", which this release
// neither queues nor decodes), each of which the conversion has replaced (RetainLingers,
// ReserveIssuesOfTreeCleanups) or finished (the hand-overs). A re-parent seed of INFRA-4728's
// shape and a re-assert status row stay: this release runs both. It answers how many it deleted.
func DeleteReleaseBeforeRows(ctx context.Context, tx pgx.Tx, project string) (int64, error) {
	tag, err := tx.Exec(ctx, `delete from outbox where split_part(issue, '-', 1) = $1 and (
			kind = 'linger_close'
			or (kind = 'supervise' and payload->>'op' = 'tree_close')
			or (kind = 'workspace_remove' and coalesce((payload->>'linger')::bigint, 0) <> 0)
			or (kind = 'dispatch_status' and payload->>'kind' = 'hand-over'))`, project)
	if err != nil {
		return 0, fmt.Errorf("delete the rows of %s only the release before runs: %w", project, err)
	}
	return tag.RowsAffected(), nil
}
