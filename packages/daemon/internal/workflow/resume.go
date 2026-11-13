package workflow

import (
	"context"

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
// The claims it weighs are the suspended owners of the project's unfinished phases in running
// trees (suspendedPhaseOwners), the one read by which a boot step picks the owner of a phase to
// start, and it starts each of them whose claim a ByRule source suspended. Each is given the start
// admission gives a re-admitted tree's mid-phase child, built where every start of a phase worker
// is built (Engine.started) and so written through the outbox's start like every other
// (StartOrOwe), and what that start carries is decided by the entry the role's phases row names,
// read here as the ready of such a claim reads it (catch_up.go's readyWorkerTask) and admission
// reads it for the same decision (admit's owedEntryRoles). A claim behind that entry is sent the
// work of that entry at that entry (startSuspendedOwner: OwedEntry, sendOwedEntry), which is the
// task the transition wrote when it raised the number: the resume says to carry the existing phase
// work on, and the work of an entry this claim was never given is work it does not have, so the
// resume would spend the claim's first turn after the boot on the wrong prompt and the completion
// it answered with would be refused as behind that entry. A claim level with that entry is sent
// the task that takes its phase on (ResumePhaseTask), which the outbox does not deliver to a claim
// still holding its phase's task. Either start resumes the claim by rule and waits for the issue's
// admission slot as every start of a phase worker does. A role a start already covers (StartFor)
// gets no second.
//
// The sweep runs first so the resume reads the trees it closed as lingering. The owners' read
// (suspendedPhaseOwners) comes next, and the status repair runs on the issues and running roots
// that read holds, before any start. The read writes nothing, and the repair writes none of the
// pull requests, claims and queued starts the read takes beside the issues, so the read answers
// what it would answer after the repair.
func (e *Engine) booted(ctx context.Context, tx pgx.Tx, _ intake.DaemonBooted) (intake.Result, error) {
	if err := e.sweepMerged(ctx, tx); err != nil {
		return intake.Result{}, err
	}
	owners, err := e.suspendedPhaseOwners(ctx, tx)
	if err != nil {
		return intake.Result{}, err
	}
	if err := e.correctStatuses(ctx, tx, owners.issues, owners.running, "boot"); err != nil {
		return intake.Result{}, err
	}
	for _, owner := range owners.suspended {
		issue, held := owner.issue, owner.run.Claim
		if !held.SuspendedBy.ByRule() || !StartFor(owner.run, owner.root, issue.Generation, issue.Phase) {
			continue
		}
		row, err := e.phaseRow(ctx, tx, issue.Key, owner.role)
		if err != nil {
			return intake.Result{}, err
		}
		behind, err := e.startSuspendedOwner(ctx, tx, owner, row)
		if err != nil {
			return intake.Result{}, err
		}
		if behind {
			e.logOnCommit(ctx, "workflow: the boot starts a suspended claim behind the entry its phases row names on that entry's work",
				"issue", issue.Key, "role", owner.role, "phase", issue.Phase, "entry", row.Entry, "suspendedBy", held.SuspendedBy)
			continue
		}
		e.logOnCommit(ctx, "workflow: the boot starts a suspended claim that owns its issue's phase", "issue", issue.Key, "role", owner.role,
			"phase", issue.Phase, "suspendedBy", held.SuspendedBy)
	}
	return intake.Result{}, nil
}

// suspendedOwner is a claim that owns its issue's unfinished phase in a running tree and stands
// suspended (suspendedPhaseOwners), with what a start of it is weighed against and built from:
// the issue; its tree root, which StartFor and OwedEntry read; the role that works the phase;
// that role's claim token; and the role's queued rows and claim at the issue's generation, whose
// Claim is never nil.
type suspendedOwner struct {
	issue record.Issue
	root  record.Issue
	role  claim.Role
	token claim.Token
	run   record.RoleRun
}

// phaseOwners is what suspendedPhaseOwners reads: every issue the store holds, of every project;
// the roots of the project's trees that run (record.RunningRoots); and the suspended owners of
// the project's unfinished phases in those trees, in the order the store lists the issues.
type phaseOwners struct {
	issues    []record.Issue
	running   map[string]struct{}
	suspended []suspendedOwner
}

// suspendedPhaseOwners reads, for each issue of the project, the suspended owner of its phase
// while that phase is unfinished and the issue's tree runs. It is the one rule by which a boot
// step picks the owner of a phase to start: the boot's resume by rule (booted) and the cutover's
// repair of the mergers the `awaiting_merge` cutover left stopped (cutoverMergerRepair) both read
// their owners here, and each narrows them by terms of its own, booted to a claim a ByRule source
// suspended and the repair to a merger the idle release suspended. So a change to how the boot
// picks a suspended owner reaches both. It writes nothing.
//
// The phase's owner is the role that works the issue's phase (phase.RoleFor) on an issue in the
// workflow whose tree is running — this generation of it, by the one rule every reader of that
// question asks (record.RunningRoots, record.TreeRuns): its lifecycle stands open under the
// workflow, its root does not linger, and its root is not itself waiting in the line for the
// tree to be opened. A lingering tree's claims stay suspended until re-admission runs the tree
// again, and a queued root's members are started by the promotion that opens the tree, which
// gives each mid-phase member its own start (admit's startMidPhaseMembers): a start queued for
// such an owner instead would re-point the claim at a tree with no lifecycle, and would put the
// member in the line's first part ahead of its own root. It is the lifecycle rather than the
// root's row because a row says the daemon has started that one issue and nothing about the
// tree: a root that arrived from another tree carries one while the tree of its own has still to
// be opened (record.KeepsSlot).
// A phase its own terminating fact has already met is given no owner either (PhaseFinished): a
// merged root stands in merging for as long as the rest of its tree runs, and resuming its merger
// there would hand it the same phase again at every boot, for days.
// Accepting's owner is the tree's architect, which admission starts with the tree, so no issue
// there is given one. An owner is answered only while its claim at the issue's generation stands
// suspended, whatever the source: which sources a start may act on is each caller's to judge.
func (e *Engine) suspendedPhaseOwners(ctx context.Context, tx pgx.Tx) (phaseOwners, error) {
	issues, err := e.store.Issues(ctx, tx)
	if err != nil {
		return phaseOwners{}, err
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
		return phaseOwners{}, err
	}
	owners := phaseOwners{issues: issues, running: record.RunningRoots(own, facts)}
	for _, issue := range own {
		role := phase.RoleFor(issue.Phase)
		if role == "" || role == claim.RoleArchitect || record.OutOfWorkflow(issue.Status) {
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
		if _, runs := owners.running[root.Key]; !runs {
			continue
		}
		finished, err := e.phaseFinished(ctx, tx, issue)
		if err != nil {
			return phaseOwners{}, err
		}
		if finished {
			continue
		}
		project, err := claim.ProjectToken(issue.Project)
		if err != nil {
			return phaseOwners{}, err
		}
		token, err := claim.NewToken(project, issue.Key, role)
		if err != nil {
			return phaseOwners{}, err
		}
		run, err := e.store.RoleRun(ctx, tx, token, issue.Key, role, issue.Generation)
		if err != nil {
			return phaseOwners{}, err
		}
		if held := run.Claim; held == nil || held.State != supervise.StateSuspended {
			continue
		}
		owners.suspended = append(owners.suspended, suspendedOwner{issue: issue, root: root, role: role, token: token, run: run})
	}
	return owners, nil
}

// startSuspendedOwner owes owner the start the boot gives a claim the daemon itself suspended,
// built from the entry row names, and says whether the claim stands behind that entry. A claim
// behind it (OwedEntry, which reads the owner's root for a tree close) is sent the work of that
// entry at that entry (sendOwedEntry). A claim level with it is sent the phase's resume task
// (ResumePhaseTask) at entry 0: that start hands the role the work it is already on rather than a
// new entry, so it moves no phases row either. The boot's resume by rule (booted) and the
// cutover's repair of a merger the `awaiting_merge` cutover left released (oweCutoverMerger) both
// build their start here, so the two are built alike. Neither hands it a claim the operator
// suspended: that suspension holds until the operator resumes the claim.
//
// Neither start raises an entry (Engine.enter is called on neither): the boot hands the role no
// number the record does not already name. The ruleResume mark is what tells the outbox to
// finish the row rather than retry a claim that spends its launch budget resuming
// (record.SuperviseRequest.RuleResume). It names this door, the boot's start of a claim the
// daemon itself suspended, by rule (booted) or by the idle release (cutoverMergerRepair), and not
// the task's text, so both starts carry it.
func (e *Engine) startSuspendedOwner(ctx context.Context, tx pgx.Tx, owner suspendedOwner, row record.PhaseRow) (behind bool, err error) {
	if OwedEntry(owner.run, owner.root, row.Entry) {
		return true, e.sendOwedEntry(ctx, tx, owner.issue, owner.role, row, startMarks{ruleResume: true})
	}
	return false, e.started(ctx, tx, owner.issue, owner.role, ResumePhaseTask(owner.issue), 0,
		startMarks{resumeTask: true, ruleResume: true})
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
