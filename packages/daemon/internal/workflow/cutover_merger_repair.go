package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// cutoverMergerTold is the Dispatch message the cutover's repair posts on each issue whose merger
// it owed a start (cutoverMergerRepair): the message dispatch://INFRA-3274/spec R2 asks for, which
// says in plain words that the cutover left the merger stopped and owed the merger its merging
// task. It says the daemon queued that task, not that it gave it: the start takes the slot mark
// the slot rule gives `merging` (StartOrOwe), which is none, since merging is past the issue's
// review (record.KeepsSlot), so admission holds nothing of it; but the daemon's outbox holds it
// while the tree's design gate is closed (the outbox's mergerWaitsForTheDesignGate), so the boot
// may give the merger nothing yet.
const cutoverMergerTold = "The cutover from a Legion build with the phase `awaiting_merge` left the merger of this issue stopped. " +
	"The idle release stopped the pod of the merger while the issue waited for its merge. " +
	"Nothing would start the merger again. " +
	"The cutover owed the merger its merging task. " +
	"At its boot, the daemon queued that task. " +
	"The merger starts when the design gate of its tree allows it."

// cutoverMergerRepair is the boot's one-time repair of the mergers the `awaiting_merge` cutover
// left stopped (intake.CutoverMergerRepair, dispatch://INFRA-3274/spec R2). A build with that
// phase moved an issue there when its merger completed merging with a READY head, since the
// daemon merged then, and no role worked the phase, so the idle release stopped the merger's pod
// while the issue waited and armed no backstop for it. Migration 20261029120700 moved each such
// issue back to merging, where the merger now merges, and wrote no task. Nothing else would start
// that merger again: the boot's resume by rule (booted) passes over a released claim; admission's
// slot repair and the entry sweep (admit's owedEntryRoles) pass over a released claim level with
// the entry its phases row names, which is where the upgrade to the phase entry puts a merger
// that took its merging task (migration 20261111225552); the relaunch's ready (readyWorkerTask)
// and the silence repair read only a live claim; no backstop is armed; and no CI result or
// message may ever come. Its pull request would stay open unless GitHub's queue or auto-merge
// still held it. The boot applies the fact once per project under a fixed event id, so
// processed_events is its once-only record and a later boot repairs nothing.
//
// It reads its mergers from the suspended owners of the project's unfinished phases in running
// trees (suspendedPhaseOwners), the read booted starts its owners from, so the repair and the
// boot's resume by rule pick an owner by one rule. By that rule the issue is in the workflow; its
// tree runs (record.RunningRoots), since a lingering tree's claims wait for its re-admission and
// a queued root's members for its promotion, each of which starts every mid-phase member (admit's
// startMidPhaseMembers); its phase is not finished (PhaseFinished), since a merged pull request's
// merge closes the issue and leaves the merger nothing to do; and the owner's claim at the issue's
// generation stands suspended. Of those owners it selects each merger the old build left so, by
// all of these:
//   - the issue stands in merging, whose owner is the merger. The migration put every
//     awaiting_merge issue there, and a hold from merging is the architect's to retry, which
//     starts the merger (restart).
//   - the merger's claim is suspended by `released`: every other claim has a starter of its own.
//     A claim the daemon or its exit suspended gets booted's resume, and an operator's suspension
//     holds for the operator. The owners' read answers no live claim, which the boot relaunches
//     onto this daemon's plugin and whose ready gives it its phase's task (readyWorkerTask), and
//     no failed claim, which waits for the architect.
//   - its pull request record is open: the merger merges an open pull request, and the architect
//     is told of one closed unmerged.
//   - the merger's phase row holds a READY head (HandoffCommit): the old build moved the issue
//     to awaiting_merge on that completion.
//   - no backstop of the merger is open (Pods.OpenPodBackstop): a release of a role that works
//     its phase arms one, which wakes it, and the old build armed none in awaiting_merge, since
//     no role worked it there.
//   - StartFor answers true: a role a start already covers gets no second, as in booted.
//
// For each issue it selects, in the fact's transaction, it clears the merger's handoff
// (clearHandoff): the READY was a completion under the old rule, and kept it would read as a
// merger that has completed merging, which the silence repair never prompts. Then it reads the
// merger's phases row and builds the start with the builder of booted's resume by rule
// (startSuspendedOwner), from the entry that row names: a merger behind that entry (OwedEntry) is
// sent the work of that entry at that entry (sendOwedEntry), and a merger level with it is sent
// the merging resume task (ResumePhaseTask) at entry 0. It builds the start that way, and not as
// one fixed request, for the reason booted gives: the resume task tells a claim to carry on work
// it already holds, so a merger behind its entry would spend its first turn on the wrong prompt,
// and the completion it answered with would be refused as behind that entry. Neither start raises
// an entry, and both carry booted's ruleResume mark, since each starts a claim the daemon itself
// suspended and the mark tells the outbox to finish the row rather than retry a claim that spends
// its launch budget. Both are built by Engine.started and written through StartOrOwe, so the slot
// mark is StartOrOwe's and this handler sets none. It posts one Dispatch message on the issue
// (cutoverMergerTold).
//
// No later start of that boot gives the merger a second task. booted passes over a released
// claim, and StartFor sees the queued start. Admission's slot repair and the entry sweep send
// nothing to a claim level with its entry, and for a claim behind it OwedEntry reads the queued
// start that carries that entry. readyWorkerTask gives nothing to a claim holding a task or
// covered by a queued start of its phase (queuedRowCovers). The silence repair reads no
// suspended claim and no claim a queued start covers (unpromptedPhaseWorker). A later start
// through StartOrOwe, while the owed start is still queued and unleased (held by a closed design
// gate, or not yet run), takes it out and writes its own in its place (Store.EnqueueStart). It
// starts no other role. Once the fact commits, it logs one line per merger with the slot mark of
// the queued start, and one line with the count and each issue, zero included. It owes nothing
// without the daemon's pod records (Config.Pods), since it cannot read a backstop without them.
func (e *Engine) cutoverMergerRepair(ctx context.Context, tx pgx.Tx, _ intake.CutoverMergerRepair) (intake.Result, error) {
	owed := []string{}
	if e.cfg.Pods != nil {
		owners, err := e.suspendedPhaseOwners(ctx, tx)
		if err != nil {
			return intake.Result{}, err
		}
		for _, owner := range owners.suspended {
			stopped, err := e.cutoverStoppedMerger(ctx, tx, owner)
			if err != nil {
				return intake.Result{}, err
			}
			if !stopped {
				continue
			}
			if err := e.oweCutoverMerger(ctx, tx, owner); err != nil {
				return intake.Result{}, err
			}
			owed = append(owed, owner.issue.Key)
		}
	}
	msg := fmt.Sprintf("cutover merger repair: owed a start to %d mergers", len(owed))
	if len(owed) > 0 {
		msg += ": " + strings.Join(owed, ", ")
	}
	e.logOnCommit(ctx, msg, "count", len(owed), "issues", owed)
	return intake.Result{}, nil
}

// cutoverStoppedMerger is whether owner, a suspended owner of an unfinished phase in a running
// tree (suspendedPhaseOwners), is a merger the cutover left stopped, by the terms of its own that
// cutoverMergerRepair states.
func (e *Engine) cutoverStoppedMerger(ctx context.Context, tx pgx.Tx, owner suspendedOwner) (bool, error) {
	issue := owner.issue
	if issue.Phase != phase.Merging || owner.run.Claim.SuspendedBy != supervise.SuspendedByReleased {
		return false, nil
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil || pr == nil || pr.State != record.PullRequestOpen {
		return false, err
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleMerger)
	if err != nil || row.HandoffCommit == "" {
		return false, err
	}
	armed, err := e.cfg.Pods.OpenPodBackstop(ctx, tx, issue.Key, claim.RoleMerger)
	if err != nil || armed {
		return false, err
	}
	return StartFor(owner.run, owner.root, issue.Generation, phase.Merging), nil
}

// oweCutoverMerger owes the stopped merger owner its merging task (cutoverMergerRepair): it
// clears the merger's handoff, reads the merger's phases row, owes the start the boot gives a
// claim the daemon itself suspended (startSuspendedOwner) from the entry that row names, posts the
// cutover's message on the issue, and logs on commit the slot mark the queued start carries, read
// back from the merger's queued rows, since the mark is StartOrOwe's to set.
func (e *Engine) oweCutoverMerger(ctx context.Context, tx pgx.Tx, owner suspendedOwner) error {
	issue := owner.issue
	if err := e.clearHandoff(ctx, tx, issue.Key, claim.RoleMerger); err != nil {
		return err
	}
	// The row is read after the clear, so the row the start is built from is the row the
	// transaction leaves. The clear moves no entry, and the merger's run is the one the selection
	// read: nothing between the two writes a queued row or the claim.
	row, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleMerger)
	if err != nil {
		return err
	}
	if _, err := e.startSuspendedOwner(ctx, tx, owner, row); err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.MessagePost{Body: cutoverMergerTold}); err != nil {
		return err
	}
	run, err := e.store.RoleRun(ctx, tx, owner.token, issue.Key, claim.RoleMerger, issue.Generation)
	if err != nil {
		return err
	}
	var queued *record.SuperviseRequest
	for i := range run.Queued {
		if request := &run.Queued[i].Request; request.Op == "start" && request.Phase == phase.Merging {
			queued = request
		}
	}
	if queued == nil {
		return fmt.Errorf("the cutover owed the merger of %s its merging task, and no start of it is queued", issue.Key)
	}
	e.logOnCommit(ctx, "workflow: the cutover owes a stopped merger its merging task", "issue", issue.Key, "needsSlot", queued.NeedsSlot)
	return nil
}
