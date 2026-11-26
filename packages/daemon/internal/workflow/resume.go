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

// booted is the daemon's boot (intake.DaemonBooted). It does four things, in this order.
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
// source the boot leaves too: the daemon released its idle pod, which held no unpushed work, and
// what brings the pod back is the next wake for its role, not a daemon restart, so a boot that
// started it would raise a pod nothing asked for. Two other boot steps can start a released claim.
// Admission's pass at the boot sends the work of an entry to a released claim that stands behind
// the entry its phases row names (admit's owedEntryRoles), as each of its passes does. And the
// cutover's repair (cutoverMergerRepair), a fact of its own that the boot applies once per project,
// before its consumers start, owes a start to each merger the `awaiting_merge` cutover left
// released with no wake to come.
//
// The claims it weighs are the suspended owners of the project's unfinished phases in running trees
// (suspendedPhaseOwners), the read the cutover's repair picks its mergers from too, and it starts
// each of them whose claim a ByRule source suspended. Each is given the start admission gives a
// re-admitted tree's mid-phase child, built where every start of a phase worker is built
// (Engine.started) and so written through the outbox's start like every other (StartOrOwe), and
// what that start carries is decided by the entry the role's phases row names, read here as the
// ready of such a claim reads it (catch_up.go's readyWorkerTask) and admission reads it for the
// same decision (admit's owedEntryRoles). A claim behind that entry is sent the work of that entry
// at that entry (startSuspendedOwner: OwedEntry, sendOwedEntry), which is the task the transition
// wrote when it raised the number: the resume says to carry the existing phase work on, and the
// work of an entry this claim was never given is work it does not have, so the resume would spend
// the claim's first turn after the boot on the wrong prompt and the completion it answered with
// would be refused as behind that entry. A claim level with that entry is sent the task that takes
// its phase on (ResumeTask), which the outbox does not deliver to a claim still holding its phase's
// task. Either start resumes the claim by rule and takes the slot mark the slot rule gives its
// phase (StartOrOwe), as every start of a phase worker built through started does, so it waits for
// the issue's admission slot in a phase that keeps one and for none in accepting, retro or merging;
// the fourth step's start of a phase owner level with its entry is enqueued directly and takes none
// (startStoppedClaim). A role a start already covers (StartFor) gets no second.
//
// Fourth, the start of every claim the project release stopped (startReleaseStopped), of every
// role: the release holds every start until this boot, and the claims it stopped are read by
// their source alone, `project_release` (supervise.SuspendedByProjectRelease), since nothing
// else records them for the boot. It runs after the resume by rule, which acts on none of them,
// and after the two other boot steps that can start such a claim, which write their starts
// before this fact: admission's first pass, the listing's reconcile, which sends a claim behind
// its entry that entry's work (admit's owedEntryRoles), and the cutover's repair. A claim one of
// them started is covered by that queued start (StartFor) and gets none here. A phase owner
// level with its entry starts with its phase's resume task, so its ready writes no second start
// (readyWorkerTask); a claim behind its entry gets that entry's work, and any other claim no task.
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
	if err := e.startReleaseStopped(ctx, tx, owners); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, nil
}

// releaseStopped is one claim the project release stopped that the boot weighs a start of
// (startReleaseStopped): the claim as the store names it, its issue, and either the issue's tree
// root, for an issue in the workflow whose tree runs, or the open production check whose look
// the claim works, for an issue the workflow has taken out.
type releaseStopped struct {
	held  record.SuspendedClaim
	issue record.Issue
	root  record.Issue
	look  *record.ProductionCheck
}

// startReleaseStopped is booted's fourth step: it starts again each claim of the project the
// project release stopped (dispatch://INFRA-3583/spec R8), every role but the controller, whose
// claim stands on no issue and which the controller keeper resumes, since it resumes every
// suspension but an operator's (the daemon's controllerKeeper). It reads them by their source, with
// a read of its own (record.Store.SuspendedClaims): the claims of an issue in the workflow whose
// tree runs (record.RunningRoots, the rule the resume by rule reads), whose run it reads at the
// generation the issue holds, which is the run the start it writes serves; and the role that works
// an open look of a closed issue's production check (record.ProductionCheck.Role), which the
// release stopped mid-look. Every other such claim is left as it stands — one of an issue taken out
// of the workflow is a close's, and one of a tree that lingers or waits in the line is its first
// run's after its re-admission or its promotion, which starts every role of a mid-phase member
// (admit's returnOwedMembers, through startMidPhaseMembers) — as the resume by rule leaves those.
//
// Each claim gets one start, and none when a queued start already covers it (StartFor): the
// start of a phase owner behind the entry its phases row names that admission's first pass
// wrote, the cutover's repair of a merger, a start the release held in the outbox. Whichever
// start a claim has, the claim is still started once the issue has left the phase or the entry
// that start names, by the start put in that start's place (record.ReleaseStoppedStart).
// What a start carries is the work the record owes the role, and nothing else:
//
//   - A phase owner behind that entry is sent the work of that entry at that entry
//     (sendOwedEntry, with the ruleResume mark), as the resume by rule sends it.
//   - A phase owner level with its entry — its phase unfinished, nothing owed — gets one start of
//     its phase carrying the phase's resume task (Engine.ResumeTask, marked ResumeTask) and the
//     ruleResume mark: its turn ended with nothing pending, and readyWorkerTask, which gives such
//     a worker that same task once it reports ready, would otherwise write it a second start.
//     The outbox delivers no resume task to a claim that still holds a task of the phase, so a
//     claim whose turn the release cut is sent its own kept task on the resume and not this one.
//   - Every other claim — an architect, a sub-architect, a role of an earlier phase, the owner of
//     a phase its terminating fact has met, a role of a parent waiting in awaiting_children, a
//     phase no role works (phase.RoleFor), whose held start alone gives its worker work again
//     (ReturnFromChildren) — gets one start with no task and the ruleResume mark, enqueued
//     directly as admission's start of a role that does not own its member's phase is
//     (startMidPhaseMembers), so it waits for no slot. The outbox's start resumes the claim by
//     rule, paced as every launch is (supervise's reserveWake), and its pending task, if it holds
//     one, is sent on that resume, since a project release does not retire it
//     (supervise.SuspendSource.RetiresPhaseTask). A phase owner's start names its phase, so the
//     phase rules that read a queued start of the owner (StartFor, OwedEntry, readyWorkerTask's
//     cover) see it; any other role's names none, as an architect's start names none, so the
//     outbox's phase fence cannot end it when the phase moves while it waits its turn. Its cover
//     is read under both, the issue's phase and none, since a promotion's start of such a role
//     names the member's phase.
//   - The production check's role gets the open look's start again: a start carrying that look
//     (CheckLook) and the check it belongs to, the shape startLook writes, without startLook's
//     Looks++, which opens the next look rather than this one. It is the one start an issue out
//     of the workflow runs (the outbox's checkLookHolds). It carries no task: the look's own task
//     is the one the claim kept and is sent on the resume, and a task on this row would be
//     delivered a second time, since the outbox holds back no task of a start that names no
//     phase.
func (e *Engine) startReleaseStopped(ctx context.Context, tx pgx.Tx, owners phaseOwners) error {
	project, err := claim.ProjectToken(e.cfg.Project)
	if err != nil {
		return err
	}
	stopped, err := e.store.SuspendedClaims(ctx, tx, project, supervise.SuspendedByProjectRelease)
	if err != nil || len(stopped) == 0 {
		return err
	}
	byKey := make(map[string]record.Issue, len(owners.issues))
	for _, issue := range owners.issues {
		byKey[issue.Key] = issue
	}
	var checks map[string]record.ProductionCheck
	candidates := make([]releaseStopped, 0, len(stopped))
	keys := make([]record.RoleRunKey, 0, len(stopped))
	for _, held := range stopped {
		issue, ok := byKey[held.Issue]
		if !ok || issue.Project != e.cfg.Project {
			continue
		}
		candidate := releaseStopped{held: held, issue: issue}
		if record.InWorkflow(issue) {
			root, ok := byKey[issue.Tree]
			if !ok {
				continue
			}
			if _, runs := owners.running[root.Key]; !runs {
				continue
			}
			candidate.root = root
		} else {
			if checks == nil {
				if checks, err = e.openChecks(ctx, tx); err != nil {
					return err
				}
			}
			check, ok := checks[issue.Key]
			if !ok || !check.LookOpen || check.Role != held.Role {
				continue
			}
			candidate.look = &check
		}
		candidates = append(candidates, candidate)
		keys = append(keys, record.RoleRunKey{Token: held.Token, Issue: issue.Key, Role: held.Role, Generation: issue.Generation})
	}
	if len(candidates) == 0 {
		return nil
	}
	runs, err := e.store.RoleRuns(ctx, tx, keys)
	if err != nil {
		return err
	}
	for _, stopped := range candidates {
		run := runs[stopped.held.Token]
		// The claim is read again with its run, and one no longer stopped by the release — a
		// resume between the two reads — is no longer this step's.
		if !run.Claim.ProjectReleased() {
			continue
		}
		if stopped.look != nil {
			if err := e.startStoppedLook(ctx, tx, stopped, run); err != nil {
				return err
			}
			continue
		}
		if err := e.startStoppedClaim(ctx, tx, stopped, run); err != nil {
			return err
		}
	}
	return nil
}

// openChecks is every open production check of the project, by issue.
func (e *Engine) openChecks(ctx context.Context, tx pgx.Tx) (map[string]record.ProductionCheck, error) {
	open, err := e.store.OpenProductionChecks(ctx, tx)
	if err != nil {
		return nil, err
	}
	checks := make(map[string]record.ProductionCheck, len(open))
	for _, check := range open {
		if check.Project == e.cfg.Project {
			checks[check.Issue] = check
		}
	}
	return checks, nil
}

// startStoppedLook gives the production check's role the release stopped mid-look the open
// look's start again, unless a start of that look is already queued (startReleaseStopped).
func (e *Engine) startStoppedLook(ctx context.Context, tx pgx.Tx, stopped releaseStopped, run record.RoleRun) error {
	issue, check := stopped.issue, *stopped.look
	for _, queued := range run.Queued {
		if queued.Request.Op == "start" && queued.Request.CheckLook == check.Looks && check.NamedBy(queued.Request.CheckOpenedAt) {
			return nil
		}
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: check.Role,
		Generation: issue.Generation, CheckLook: check.Looks, CheckOpenedAt: check.OpenedAt, RuleResume: true}); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: the boot starts a claim the project release stopped on the production check look it was in",
		"issue", issue.Key, "role", check.Role, "look", check.Looks)
	return nil
}

// startStoppedClaim gives a claim the project release stopped, of an issue in the workflow whose
// tree runs, its one start of the boot, unless a queued start covers it (startReleaseStopped): a
// phase owner's carries the entry it is behind, or else its phase's resume task.
func (e *Engine) startStoppedClaim(ctx context.Context, tx pgx.Tx, stopped releaseStopped, run record.RoleRun) error {
	issue, root, role := stopped.issue, stopped.root, stopped.held.Role
	owns := role != claim.RoleArchitect && phase.RoleFor(issue.Phase) == role
	if owns {
		// A phase its own terminating fact has met has no owner, as the resume by rule and the
		// promotion read it (suspendedPhaseOwners, startMidPhaseMembers): its role comes back
		// with no work, like a role of an earlier phase.
		finished, err := e.phaseFinished(ctx, tx, issue)
		if err != nil {
			return err
		}
		owns = !finished
	}
	request := record.ReleaseStoppedStart(issue.Tree, role, issue.Generation)
	if owns {
		if !StartFor(run, root, issue.Generation, issue.Phase) {
			return nil
		}
		row, err := e.phaseRow(ctx, tx, issue.Key, role)
		if err != nil {
			return err
		}
		if OwedEntry(run, root, row.Entry) {
			if err := e.sendOwedEntry(ctx, tx, issue, role, row, startMarks{ruleResume: true}); err != nil {
				return err
			}
			e.logOnCommit(ctx, "workflow: the boot starts a claim the project release stopped behind the entry its phases row names on that entry's work",
				"issue", issue.Key, "role", role, "phase", issue.Phase, "entry", row.Entry)
			return nil
		}
		// Level with its entry: the phase's resume task rides this start, so readyWorkerTask finds
		// the task held once the worker reports ready and writes no second start.
		resume, err := e.ResumeTask(ctx, tx, issue)
		if err != nil {
			return err
		}
		request.Phase, request.Task, request.ResumeTask = issue.Phase, resume, true
	} else if !StartFor(run, root, issue.Generation, "") || !StartFor(run, root, issue.Generation, issue.Phase) {
		return nil
	}
	if err := e.enqueue(ctx, tx, issue.Key, request); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: the boot starts a claim the project release stopped", "issue", issue.Key, "role", role,
		"phase", issue.Phase, "ownsPhase", owns)
	return nil
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

// phaseOwners is what suspendedPhaseOwners reads: every issue the store holds, which the daemon's
// store holds of the project alone (internal/daemon's projectRecords); the roots of the project's
// trees that run (record.RunningRoots); and the suspended owners of the project's unfinished
// phases in those trees, in the order the store lists the issues.
type phaseOwners struct {
	issues    []record.Issue
	running   map[string]struct{}
	suspended []suspendedOwner
}

// suspendedPhaseOwners reads, for each issue of the project, the suspended owner of its phase
// while that phase is unfinished and the issue's tree runs. The boot's resume by rule (booted) and
// the cutover's repair of the mergers the `awaiting_merge` cutover left stopped
// (cutoverMergerRepair) both read their owners here, and each narrows them by terms of its own,
// booted to a claim a ByRule source suspended and the repair to a merger the idle release
// suspended, so a change to how either picks a suspended owner reaches both. Admission's pass at
// the boot reads its phase workers by a read of its own (admit's phaseWorkers) and sends only the
// work of an entry a claim stands behind (owedEntryRoles). It writes nothing.
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
// The owner of admitted and of accepting is an architect and no phase worker, so no issue in
// either is given one. On a root it is the root's own architect, which admission starts with the
// tree. On a child it is the architect that owns it: its own while that claim holds its role,
// else the one above. At admitted that is the one above, since the child's own has not started
// yet or a leave suspended or retired it, unless a person set the child back to todo while it ran
// or was held: that re-entry starts and stops nothing, so a child's own architect that held its
// role still holds it (ReenterChild). An owner is answered only while its claim at the issue's
// generation stands suspended, whatever the source: which sources a start may act on is each
// caller's to judge.
func (e *Engine) suspendedPhaseOwners(ctx context.Context, tx pgx.Tx) (phaseOwners, error) {
	issues, err := e.store.Issues(ctx, tx)
	if err != nil {
		return phaseOwners{}, err
	}
	byKey := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	facts, err := record.ReadLineFacts(ctx, tx, e.store, e.cfg.Project, issues, e.cfg.Steps)
	if err != nil {
		return phaseOwners{}, err
	}
	owners := phaseOwners{issues: issues, running: record.RunningRoots(issues, facts)}
	for _, issue := range issues {
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
// (ResumeTask) at entry 0: that start hands the role the work it is already on rather than a
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
//
// Both starts are re-assertions (startMarks.reasserts): the boot is the daemon coming back, no
// fact of the issue, and the work each sends is work the record already owes the role, so neither
// marks a movement of the issue. sendOwedEntry sets the mark itself on the start it writes, and
// the resume task's start sets it here.
func (e *Engine) startSuspendedOwner(ctx context.Context, tx pgx.Tx, owner suspendedOwner, row record.PhaseRow) (behind bool, err error) {
	if OwedEntry(owner.run, owner.root, row.Entry) {
		return true, e.sendOwedEntry(ctx, tx, owner.issue, owner.role, row, startMarks{ruleResume: true})
	}
	resume, err := e.ResumeTask(ctx, tx, owner.issue)
	if err != nil {
		return false, err
	}
	return false, e.started(ctx, tx, owner.issue, owner.role, resume, 0, startMarks{resumeTask: true, ruleResume: true, reasserts: true})
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
		if !awaitsMerge(listed) {
			continue
		}
		pr, err := e.store.PullRequestRow(ctx, tx, listed.Key)
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
