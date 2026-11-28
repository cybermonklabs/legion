package record

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// GateWrite writes after, the row before as the boot's reset after an earlier release's boot
// leaves it (ResetAfterEarlierRelease), in the reset's transaction. The daemon's write is the
// workflow engine's, which tells each change of standing the reset makes through the paths every
// other move takes (workflow's Engine.ResetGate); a test's is a plain Store.PutGate.
type GateWrite func(ctx context.Context, tx pgx.Tx, before, after DesignGate) error

// GateReset is what ResetGatesAfterEarlierRelease did, for the boot's log line.
type GateReset struct {
	// EarlierBoot is the newest boot of the project before this one, which no boot of this
	// release recorded (design_gate_verdict_boots): 0 where the project has no boot or its newest
	// boot is one of this release, and the reset did nothing.
	EarlierBoot int64
	// Rows is how many design_gates rows the reset changed and wrote, BasesEnded how many of them
	// it ended a basis on, and Seeds how many gate seeds it queued.
	Rows, BasesEnded, Seeds int
}

// projectIssues is the condition of a design_gates row of one of the issues of the project $1.
const projectIssues = `issue in (select key from issues where project = $1)`

// ResetGatesAfterEarlierRelease is the boot's reset after an earlier release's boot
// (dispatch://INFRA-6065, R2), in tx. A release before the hash gate can boot on this schema: it
// names none of the columns the hash gate added, so it reads every gate by the version rule,
// settles nothing a version owing its digest held back, and leaves those columns as this release
// last wrote them; and it records no row of design_gate_verdict_boots, which every boot of this
// release writes with its daemon_boot row (the store's RecordBoot). So where the newest daemon_boot
// row of project has no such row, an earlier release booted since this release last did, and this
// boot does to the rows of the project's issues what the migration's way back does, before it
// applies any fact (the daemon's openWorkflow runs it before its first write of a record, and
// before this boot's own RecordBoot, so the newest boot is the one before this):
//
//   - It clears carried on each row and marks the copies again as the migration does
//     (carriedCopies), so a copy an earlier release carried is marked and a row it wrote over a
//     copy of its own document is no longer one.
//   - It puts each row in version mode (ResetAfterEarlierRelease), first ending each basis that a
//     version owing its digest held past that version, and hands each row that changes to write
//     with the row as it stood, so the write reads the told standing before it is cleared. Then it
//     clears every column the hash gate added on every row, so no value that a row's read leaves
//     out stays behind, such as a root version beside no basis.
//   - It queues the gate seed of each registered step the deployment takes (TakesStep) of each
//     tree in the workflow (InWorkflow), open or closed, so each row's read of Dispatch gives it
//     its verdict again (EnqueueGateSeed, which queues none beside a seed still pending).
//
// A project with no boot, and one whose newest boot is one of this release, gets none of it. The
// first boot after the migration finds the boot of the release before it and resets: the rows are
// in version mode already, so it changes none of them, and it queues the seeds. store is the
// record store the reset queues the seeds with, steps the deployment's gated steps, and now the
// time each seed is due.
func ResetGatesAfterEarlierRelease(ctx context.Context, tx pgx.Tx, store Store, project string, steps []DesignStep, write GateWrite,
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
	gates, err := gatesWhere(ctx, tx, projectIssues, project, "of project "+project)
	if err != nil {
		return GateReset{}, err
	}
	for _, before := range gates {
		after, ended, changed := ResetAfterEarlierRelease(before)
		if !changed {
			continue
		}
		if err := write(ctx, tx, before, after); err != nil {
			return GateReset{}, err
		}
		reset.Rows++
		if ended {
			reset.BasesEnded++
		}
	}
	if _, err := tx.Exec(ctx, `update design_gates set latest_digest_version = null, approval_current = null, latest_digest = null,
		latest_digest_seq = null, latest_digest_at = null, approved_digest = null, told_open = null, basis_root_version = null
		where `+projectIssues, project); err != nil {
		return GateReset{}, fmt.Errorf("put the design gates of %s in version mode: %w", project, err)
	}
	if reset.Seeds, err = queueResetSeeds(ctx, tx, store, project, steps, gates, now); err != nil {
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

// queueResetSeeds queues the gate seed of each of gates, the rows of project's issues, whose step
// the deployment takes (TakesStep) and whose issue is recorded in a tree whose root is in the
// workflow (InWorkflow), as the boot's seed of each closed step reads them (the daemon's
// queueRecordedGatesPublish), and answers how many it queued.
func queueResetSeeds(ctx context.Context, tx pgx.Tx, store Store, project string, steps []DesignStep, gates []DesignGate,
	now time.Time) (int, error) {
	var keys []string
	for _, gate := range gates {
		if TakesStep(steps, gate.Step.OrSpec()) {
			keys = append(keys, gate.Issue)
		}
	}
	if len(keys) == 0 {
		return 0, nil
	}
	read, err := store.IssuesOf(ctx, tx, project, slices.Compact(keys))
	if err != nil {
		return 0, err
	}
	issues := make(map[string]Issue, len(read))
	for _, issue := range read {
		issues[issue.Key] = issue
	}
	roots, err := ReadTreeRoots(ctx, tx, store, project, read)
	if err != nil {
		return 0, err
	}
	maps.Copy(issues, roots)
	seeded := 0
	for _, gate := range gates {
		issue, recorded := issues[gate.Issue]
		if root, ok := issues[issue.Tree]; !TakesStep(steps, gate.Step.OrSpec()) || !recorded || !ok || !InWorkflow(root) {
			continue
		}
		seed := GateSeed{ArtifactID: gate.ArtifactID, Step: gate.Step.OrSpec(), Version: gate.LatestVersion, Generation: issue.Generation}
		added, err := EnqueueGateSeed(ctx, tx, store, gate.Issue, seed, now)
		if err != nil {
			return 0, err
		}
		if added {
			seeded++
		}
	}
	return seeded, nil
}
