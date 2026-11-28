package record

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// GateReset is what ResetGatesAfterEarlierRelease did, for the boot's log line.
type GateReset struct {
	// EarlierBoot is the newest boot of the project before this one, which ran no reset and so has
	// no row of design_gate_verdict_boots: an earlier release's, or this release's without the
	// workflow. It is 0 where the project has no boot or its newest boot ran the reset, and the
	// reset did nothing.
	EarlierBoot int64
	// Rows is how many design_gates rows held a value of a column the hash gate added, which the
	// reset cleared, and Seeds how many gate seeds it queued.
	Rows, Seeds int
}

// projectIssues is the condition of a design_gates row of one of the issues of the project $1.
const projectIssues = `issue in (select key from issues where project = $1)`

// ResetGatesAfterEarlierRelease is the boot's reset after an earlier release's boot
// (dispatch://INFRA-6065, R2), in tx. A release before the hash gate can boot on this schema: it
// names none of the columns the hash gate added, so it reads every gate by the version rule and
// leaves those columns as this release last wrote them; and it records no row of
// design_gate_verdict_boots, which every boot of this release with the workflow writes with its
// daemon_boot row, in the transaction that runs this (the store's RecordWorkflowBoot). A boot of
// this release without the workflow runs no reset and writes none either (the store's
// RecordBoot). So where the newest daemon_boot row of project has no such row, an earlier release,
// or this release without the workflow, booted since this release last booted with it, and this
// resets, which changes nothing on a row already in version mode but queues its seed. The daemon
// runs this in the transaction that records its boot, before that transaction writes the boot's
// rows, so the newest boot it reads is the one before this, and before the boot applies any fact
// (the daemon's openWorkflow); a boot that fails later has its marker already, so the next boot
// does not reset again over rows this release wrote since. Where it resets, it does this to the
// rows of the project's issues:
//
//   - It clears carried on each row and marks the copies again as the migration does
//     (carriedCopies), so a copy an earlier release carried is marked and a row it wrote over a
//     copy of its own document is no longer one.
//   - It puts each row in version mode and clears every other column the hash gate added: the
//     verdict, its digests and its mark, the approved digest, the told standing and the basis's
//     root version. It ends no basis and tells nothing: the columns cannot show what the earlier
//     release did, and the standing version mode reads is the one the earlier release left. A
//     step open on a basis that a version owing its digest held past that version reads closed
//     in version mode, since its root version is below the latest version, and a pending one
//     waits as the earlier release left it.
//   - It queues the gate seed of each registered step the deployment takes (TakesStep) of each
//     tree in the workflow (InWorkflow), open or closed, so each row's read of Dispatch gives it
//     its verdict again and tells each change from the standing version mode reads
//     (EnqueueGateSeeds, the rule the boot's seed of each closed step follows, which queues none
//     beside a seed still pending).
//
// A project with no boot, and one whose newest boot is this release's with the workflow, gets none
// of it. The first boot after the migration finds the boot of the release before it and resets:
// the rows are in version mode already, so it clears none of them, and it queues the seeds. store
// is the record store the reset queues the seeds with, steps the deployment's gated steps, and now
// the time each seed is due.
func ResetGatesAfterEarlierRelease(ctx context.Context, tx pgx.Tx, store Store, project string, steps []DesignStep,
	now time.Time) (GateReset, error) {
	var reset GateReset
	var marked bool
	err := tx.QueryRow(ctx, `select id, exists (select 1 from design_gate_verdict_boots where boot = daemon_boot.id)
		from daemon_boot where project = $1 order by id desc limit 1`, project).Scan(&reset.EarlierBoot, &marked)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return GateReset{}, nil
	case err != nil:
		return GateReset{}, fmt.Errorf("read the newest boot of %s: %w", project, err)
	case marked:
		return GateReset{}, nil
	}
	if _, err := tx.Exec(ctx, `update design_gates set carried = false where carried and `+projectIssues, project); err != nil {
		return GateReset{}, fmt.Errorf("clear the carried copies of %s: %w", project, err)
	}
	if _, err := tx.Exec(ctx, carriedCopies, project); err != nil {
		return GateReset{}, fmt.Errorf("mark the carried copies of %s: %w", project, err)
	}
	cleared, err := tx.Exec(ctx, `update design_gates set latest_digest_version = null, approval_current = null, latest_digest = null,
		latest_digest_seq = null, latest_digest_at = null, approved_digest = null, told_open = null, basis_root_version = null
		where `+projectIssues+` and (latest_digest_version is not null or approval_current is not null or latest_digest is not null
		or latest_digest_seq is not null or latest_digest_at is not null or approved_digest is not null or told_open is not null
		or basis_root_version is not null)`, project)
	if err != nil {
		return GateReset{}, fmt.Errorf("put the design gates of %s in version mode: %w", project, err)
	}
	reset.Rows = int(cleared.RowsAffected())
	gates, err := gatesWhere(ctx, tx, projectIssues, project, "of project "+project)
	if err != nil {
		return GateReset{}, err
	}
	keys := make([]string, 0, len(gates))
	for _, gate := range gates {
		keys = append(keys, gate.Issue)
	}
	issues, err := ReadIssuesAndTreeRoots(ctx, tx, store, project, keys)
	if err != nil {
		return GateReset{}, err
	}
	if reset.Seeds, err = EnqueueGateSeeds(ctx, tx, store, steps, gates, issues, now); err != nil {
		return GateReset{}, err
	}
	return reset, nil
}

// carriedCopies is the statement of migration 20261127204213_design_gate_verdict that marks each
// copy of a gate a hand-over carried, restricted to the rows of the issues of the project $1: a
// row whose issue holds a hand-over (handed_over_roots) that carried a gate at the issue's current
// generation, on a document a row of another issue names.
const carriedCopies = `update design_gates set carried = true
	from handed_over_roots, issues
	where handed_over_roots.issue = design_gates.issue
	  and issues.key = design_gates.issue
	  and issues.project = $1
	  and handed_over_roots.generation = issues.generation
	  and handed_over_roots.gate_carried
	  and exists (select 1 from design_gates other
	              where other.artifact_id = design_gates.artifact_id and other.issue <> design_gates.issue)`
