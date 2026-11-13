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

// booted is the daemon's boot (intake.DaemonBooted). It does three things, in this order.
//
// First, the upgrade's sweep (sweepMerged): an issue of this project that awaits its merge on a
// pull request already merged is one the migration moved out of production_check, and the merge
// that would have closed it will never come again, so the boot closes it as the merge would have
// (closeMerged) — unless an issue beneath it is still in the workflow, which leaves it where it
// is to close when the last of them leaves (leave's closeAwaitingMerge), so the deploy suspends no
// running child under a mass of lingers.
//
// Second, the status repair (correctStatuses): every issue the daemon works whose recorded
// Dispatch status is not the status of its phase gets the Dispatch status write that corrects it,
// so a write the daemon lost before the restart is put back by it. The controller tick runs the
// same repair, so no later loss waits for a restart.
//
// Third, the resume by rule: every claim of this project that owns its issue's current phase and
// that the daemon or its own exit suspended (supervise's ByRule) is started again. Nothing else
// would start it: the phase's start ran before the restart, a re-admission's starts are queued
// only when a tree is re-admitted, and the next start is the next phase's. This is the one trigger
// main did not have, so it acts on no other source: an operator's suspension holds until the
// operator resumes the claim, and a suspension recorded before the source existed (unknown) may be
// a drain made before the upgrade, so both are left as they are. A released claim is the one ByRule
// source the boot leaves too: the daemon released its idle pod, and what brings the pod and its
// bundled work back is the next wake for its role, not a daemon restart, so a boot that started it
// would raise a pod nothing asked for. One boot step does start a released claim, once per project
// and never again: the cutover's repair (cutoverMergerRepair), a fact of its own that the boot
// applies before its consumers start, owes a start to each merger the `awaiting_merge` cutover
// left released with no wake to come.
//
// The phase's owner is the role that works the issue's phase (RoleFor) on an issue in the
// workflow whose tree is running — this generation of it, by the one rule every reader of that
// question asks (record.RunningRoots, record.TreeRuns): its lifecycle stands open under the
// workflow, its root does not linger, and its root is not itself waiting in the line for the
// tree to be opened. A lingering tree's claims stay suspended until re-admission runs the tree
// again, and a queued root's members are started by the promotion that opens the tree, which
// gives each mid-phase member its own start (admit's startMidPhaseMembers): a start queued here
// instead would re-point the claim at a tree with no lifecycle, and would put the member in the
// line's first part ahead of its own root. It is the lifecycle rather than the root's row
// because a row says the daemon has started that one issue and nothing about the tree: a root
// that arrived from another tree carries one while the tree of its own has still to be opened
// (record.KeepsSlot).
// A phase its own terminating fact has already met is given no owner either (PhaseFinished): a
// merged root stands in merging for as long as the rest of its tree runs, and resuming its merger
// there would hand it the same phase again at every boot, for days.
// Accepting's owner is the tree's architect, which admission starts with the tree, so no issue
// there is given one. Each owner is given the start admission gives a re-admitted tree's
// mid-phase child, through the outbox's start like every other start (StartOrOwe): one row that
// resumes the claim by rule and carries the task that takes its phase on (ResumePhaseTask, which
// names no head, since the merger reads the head it merges on GitHub). The outbox does not
// deliver that task to a claim still holding its phase's task, and the row waits for the issue's
// admission slot as every start of a phase worker does. A role a start already covers (StartFor)
// gets no second. The sweep runs first so the resume reads the trees it closed as lingering.
func (e *Engine) booted(ctx context.Context, tx pgx.Tx, _ intake.DaemonBooted) (intake.Result, error) {
	if err := e.sweepMerged(ctx, tx); err != nil {
		return intake.Result{}, err
	}
	issues, err := e.store.Issues(ctx, tx)
	if err != nil {
		return intake.Result{}, err
	}
	own := make([]record.Issue, 0, len(issues))
	byKey := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
		if issue.Project == e.cfg.Project {
			own = append(own, issue)
		}
	}
	facts, err := record.ReadLineFacts(ctx, tx, e.store, e.cfg.Project, own, e.cfg.Steps)
	if err != nil {
		return intake.Result{}, err
	}
	running := record.RunningRoots(own, facts)
	if err := e.correctStatuses(ctx, tx, issues, running, "boot"); err != nil {
		return intake.Result{}, err
	}
	for _, issue := range issues {
		role := RoleFor(issue.Phase)
		if issue.Project != e.cfg.Project || role == "" || role == claim.RoleArchitect || record.OutOfWorkflow(issue.Status) {
			continue
		}
		root, ok := byKey[issue.Tree]
		if !ok {
			continue
		}
		// The tree must be running — this generation of it, by the one rule every reader of that
		// question asks (record.TreeRuns, read here through record.RunningRoots): the workflow has
		// opened its lifecycle and not closed it, the root does not linger, and the root is not
		// itself waiting in the line for the tree to be opened, whose architect's first task comes
		// with its promotion.
		if _, runs := running[root.Key]; !runs {
			continue
		}
		finished, err := e.phaseFinished(ctx, tx, issue)
		if err != nil {
			return intake.Result{}, err
		}
		if finished {
			continue
		}
		project, err := claim.ProjectToken(issue.Project)
		if err != nil {
			return intake.Result{}, err
		}
		token, err := claim.NewToken(project, issue.Key, role)
		if err != nil {
			return intake.Result{}, err
		}
		run, err := e.store.RoleRun(ctx, tx, token, issue.Key, role, issue.Generation)
		if err != nil {
			return intake.Result{}, err
		}
		held := run.Claim
		if held == nil || held.State != supervise.StateSuspended || !held.SuspendedBy.ByRule() ||
			!StartFor(run, root, issue.Generation, issue.Phase) {
			continue
		}
		if err := StartOrOwe(ctx, tx, e.store, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: role,
			Generation: issue.Generation, Phase: issue.Phase, Task: ResumePhaseTask(issue), ResumeTask: true, RuleResume: true}, e.now()); err != nil {
			return intake.Result{}, err
		}
		e.logOnCommit(ctx, "workflow: the boot starts a suspended claim that owns its issue's phase", "issue", issue.Key, "role", role,
			"phase", issue.Phase, "suspendedBy", held.SuspendedBy)
	}
	return intake.Result{}, nil
}

// sweepMerged closes every issue of the project that awaits a merge GitHub already reported
// (booted). Each is re-read before its close: a child's close may have closed the merged issues
// above it (closeAwaitingMerge), and a root's close lingers the tree its children then close
// under. A root whose tree still runs is left where it stands by closeMerged, which owns that
// rule, and the sweep tells nobody of the wait: a restart must not repeat the architect's
// notice, and a root can now wait for days across many boots.
func (e *Engine) sweepMerged(ctx context.Context, tx pgx.Tx) error {
	issues, err := e.store.Issues(ctx, tx)
	if err != nil {
		return err
	}
	for _, listed := range issues {
		if listed.Project != e.cfg.Project || !awaitsMerge(listed) {
			continue
		}
		pr, err := e.store.PullRequest(ctx, tx, listed.Key)
		if err != nil || pr == nil || pr.State != record.PullRequestMerged {
			if err != nil {
				return err
			}
			continue
		}
		issue, err := e.store.Issue(ctx, tx, listed.Key)
		if err != nil || issue == nil || !awaitsMerge(*issue) {
			if err != nil {
				return err
			}
			continue
		}
		waiting, err := e.closeMerged(ctx, tx, *issue, pr)
		if err != nil {
			return err
		}
		if len(waiting) > 0 {
			continue
		}
		e.logOnCommit(ctx, "workflow: the boot closes an issue whose pull request merged before the upgrade", "issue", issue.Key,
			"tree", issue.Tree, "pullRequest", pr.Number, "phase", issue.Phase)
	}
	return nil
}

// cutoverMergerTold is the Dispatch message the cutover's repair posts on each issue whose merger
// it owed a start (cutoverMergerRepair), in the plain words of dispatch://INFRA-3274's plan,
// Appendix C.
const cutoverMergerTold = "The Legion upgrade from a build with the phase `awaiting_merge` left the merger of this issue stopped. " +
	"The idle release stopped the pod of the merger while the issue waited for its merge. " +
	"Nothing would start the merger again. " +
	"At its boot, the daemon gave the merger its merging task."

// cutoverMergerRepair is the boot's one-time repair of the mergers the `awaiting_merge` cutover
// left stopped (intake.CutoverMergerRepair, dispatch://INFRA-3274/spec R2). A build with that
// phase moved an issue there when its merger completed merging with a READY head, since the
// daemon merged then, and no role worked the phase, so the idle release stopped the merger's pod
// while the issue waited and armed no backstop for it. Migration 20261029120700 moved each such
// issue back to merging, where the merger now merges, and wrote no task. Nothing else would start
// that merger again: the boot's resume by rule (booted) and admission's slot repair (admit's
// resumeSlottedWorkers) pass over a released claim, the relaunch's ready (readyWorkerTask) and
// the silence repair read only a live claim, no backstop is armed, and no CI result or message
// may ever come. Its pull request would stay open unless GitHub's queue or auto-merge still held
// it. The boot applies the fact once per project under a fixed event id, so processed_events is
// its once-only record and a later boot repairs nothing.
//
// It selects each issue of the project whose merger the old build left so, by all of these:
//   - the issue stands in merging itself and is in the workflow. The migration put every
//     awaiting_merge issue there, and a hold from merging is the architect's to retry, which
//     starts the merger (restart).
//   - its tree runs, by the rule booted reads (record.RunningRoots): a lingering tree's claims
//     wait for its re-admission, and a queued root's members for its promotion, each of which
//     starts every mid-phase member (admit's startMidPhaseMembers).
//   - merging is not finished (PhaseFinished), the one rule every owner of a phase is read by: a
//     merged pull request's merge closes the issue, and the merger has nothing left to do.
//   - its pull request record is open: the merger merges an open pull request, and the architect
//     is told of one closed unmerged.
//   - the merger's phase row holds a READY head (HandoffCommit): the old build moved the issue
//     to awaiting_merge on that completion.
//   - the merger's claim at the issue's generation is suspended by `released`: every other state
//     has a starter of its own. The boot relaunches a live claim of the old build's plugin and its
//     ready gives it the resume task (readyWorkerTask), a claim the daemon or its exit suspended
//     gets booted's resume, an operator's suspension holds for the operator, and a failed claim
//     waits for the architect.
//   - no backstop of the merger is open (Pods.OpenPodBackstop): a release of a role that works
//     its phase arms one, which wakes it, and the old build armed none in awaiting_merge, since
//     no role worked it there.
//   - StartFor answers true: a role a start already covers gets no second, as in booted.
//
// For each issue it selects, in the fact's transaction, it clears the merger's handoff
// (clearHandoff): the READY was a completion under the old rule, and kept it would read as a
// merger that has completed merging, which the silence repair never prompts. It owes the start
// booted owes for a resume by rule, the merging resume task included, through StartOrOwe, so the
// slot mark is StartOrOwe's and this handler sets none. It posts one Dispatch message on the
// issue (cutoverMergerTold). No later start of that boot gives the merger a second task: booted
// and the slot repair pass over a released claim and StartFor sees the queued start,
// readyWorkerTask covers a queued start of the phase, the silence repair reads no suspended
// claim, and a later StartOrOwe replaces the owed start in place. It starts no other role. Once
// the fact commits, it logs one line per merger with the slot mark of the queued start, and one
// line with the count and each issue, zero included. It owes nothing without the daemon's pod
// records (Config.Pods), since it cannot read a backstop without them.
func (e *Engine) cutoverMergerRepair(ctx context.Context, tx pgx.Tx, _ intake.CutoverMergerRepair) (intake.Result, error) {
	owed := []string{}
	if e.cfg.Pods != nil {
		issues, err := e.store.Issues(ctx, tx)
		if err != nil {
			return intake.Result{}, err
		}
		own := make([]record.Issue, 0, len(issues))
		byKey := make(map[string]record.Issue, len(issues))
		for _, issue := range issues {
			byKey[issue.Key] = issue
			if issue.Project == e.cfg.Project {
				own = append(own, issue)
			}
		}
		running, err := record.ReadRunningRoots(ctx, tx, e.store, e.cfg.Project, own, e.cfg.Steps)
		if err != nil {
			return intake.Result{}, err
		}
		for _, issue := range own {
			token, stopped, err := e.cutoverStoppedMerger(ctx, tx, issue, byKey, running)
			if err != nil {
				return intake.Result{}, err
			}
			if !stopped {
				continue
			}
			if err := e.oweCutoverMerger(ctx, tx, issue, token); err != nil {
				return intake.Result{}, err
			}
			owed = append(owed, issue.Key)
		}
	}
	msg := fmt.Sprintf("cutover merger repair: owed a start to %d mergers", len(owed))
	if len(owed) > 0 {
		msg += ": " + strings.Join(owed, ", ")
	}
	e.logOnCommit(ctx, msg, "count", len(owed), "issues", owed)
	return intake.Result{}, nil
}

// cutoverStoppedMerger is whether issue's merger is one the cutover left stopped, by the terms
// cutoverMergerRepair states, and the merger's token. byKey holds the project's issues by key and
// running the roots whose trees run (record.RunningRoots).
func (e *Engine) cutoverStoppedMerger(ctx context.Context, tx pgx.Tx, issue record.Issue, byKey map[string]record.Issue,
	running map[string]struct{},
) (claim.Token, bool, error) {
	if issue.Phase != phase.Merging || record.OutOfWorkflow(issue.Status) {
		return "", false, nil
	}
	root, ok := byKey[issue.Tree]
	if !ok {
		return "", false, nil
	}
	if _, runs := running[root.Key]; !runs {
		return "", false, nil
	}
	finished, err := e.phaseFinished(ctx, tx, issue)
	if err != nil || finished {
		return "", false, err
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil || pr == nil || pr.State != record.PullRequestOpen {
		return "", false, err
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleMerger)
	if err != nil || row.HandoffCommit == "" {
		return "", false, err
	}
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return "", false, err
	}
	token, err := claim.NewToken(project, issue.Key, claim.RoleMerger)
	if err != nil {
		return "", false, err
	}
	run, err := e.store.RoleRun(ctx, tx, token, issue.Key, claim.RoleMerger, issue.Generation)
	if err != nil {
		return "", false, err
	}
	if held := run.Claim; held == nil || held.State != supervise.StateSuspended || held.SuspendedBy != supervise.SuspendedByReleased {
		return "", false, nil
	}
	armed, err := e.cfg.Pods.OpenPodBackstop(ctx, tx, issue.Key, claim.RoleMerger)
	if err != nil || armed {
		return "", false, err
	}
	return token, StartFor(run, root, issue.Generation, phase.Merging), nil
}

// oweCutoverMerger owes issue's stopped merger, token, its merging task (cutoverMergerRepair):
// it clears the merger's handoff, owes the start booted owes for a resume by rule, posts the
// cutover's message on the issue, and logs on commit the slot mark the queued start carries, read
// back from the merger's queued rows, since the mark is StartOrOwe's to set.
func (e *Engine) oweCutoverMerger(ctx context.Context, tx pgx.Tx, issue record.Issue, token claim.Token) error {
	if err := e.clearHandoff(ctx, tx, issue.Key, claim.RoleMerger); err != nil {
		return err
	}
	if err := StartOrOwe(ctx, tx, e.store, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleMerger,
		Generation: issue.Generation, Phase: phase.Merging, Task: ResumePhaseTask(issue), ResumeTask: true, RuleResume: true}, e.now()); err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.MessagePost{Body: cutoverMergerTold}); err != nil {
		return err
	}
	run, err := e.store.RoleRun(ctx, tx, token, issue.Key, claim.RoleMerger, issue.Generation)
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
