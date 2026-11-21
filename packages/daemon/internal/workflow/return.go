package workflow

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// backInWork is the engine's reaction to a newer status of a recorded issue that is not out of
// the workflow (dispatchIssue): Dispatch shows the issue todo, in_progress, testing, needs_review
// or retro, whoever wrote it (R20 of dispatch://INFRA-3320: the daemon acts on each issue's own
// status). Only decision 8's three cases open a new run (NewRun):
//
//   - a todo on a record at phase done: an issue the daemon closed, or one the release before
//     left there when it left the workflow;
//   - a move out of Dispatch's done into any status in work of a record at phase done whose run's
//     pull request is recorded merged: the reopen of an issue its merge closed (opensRunOnReopen);
//   - the todo of rerun_child on an issue park_child parked (record.Issue.ParkedByArchitect).
//
// Any other status in work on a record at phase done opens nothing, and the issue stays out of
// Legion's work (R15): R25's fix moving an issue the daemon closed into in_progress is one. An
// issue a stop left in its phase that Dispatch shows in work again returns to work (ReturnToWork).
//
// An issue already in Legion's work moves only on a todo: a todo on a held issue releases its hold
// (releaseHeldRoot), and a todo on a root whose architect claim failed starts that claim again
// (RetryFailedArchitect). Any other status in work changes nothing here; admission records it.
func (e *Engine) backInWork(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) error {
	switch {
	case issue.Phase == phase.Done:
		opens, err := e.opensNewRun(ctx, tx, issue, fact)
		if err != nil || !opens {
			return err
		}
		return e.NewRun(ctx, tx, issue, fact)
	case !record.InLegionsWork(issue):
		if fact.Status == "todo" && issue.ParkedByArchitect {
			return e.NewRun(ctx, tx, issue, fact)
		}
		return e.ReturnToWork(ctx, tx, issue, fact)
	case fact.Status != "todo":
		return nil
	case issue.Phase == phase.Held && issue.Hold != nil:
		return e.releaseHeldRoot(ctx, tx, issue, fact)
	}
	_, err := e.RetryFailedArchitect(ctx, tx, issue)
	return err
}

// opensNewRun says whether fact, a status in work, opens a new run of issue, a record at phase
// done (decision 8 of dispatch://INFRA-4730): a todo does, and so does a move out of Dispatch's
// done into any status in work while the pull request of the run that ended is recorded merged,
// the reopen of an issue its merge closed. A move from another status into work, such as R25's
// fix moving an issue from the backlog to in_progress, opens nothing.
func (e *Engine) opensNewRun(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) (bool, error) {
	if fact.Status == "todo" {
		return true, nil
	}
	if issue.DispatchStatus != "done" {
		return false, nil
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil || pr == nil {
		return false, err
	}
	return pr.State == record.PullRequestMerged && uint64(pr.Generation) == issue.Generation, nil
}

// ReturnToWork takes back into Legion's work an issue a stop left in its phase (leave) that
// Dispatch shows in work again, whoever wrote the status (R20 of dispatch://INFRA-3320). It
// resumes the phase it stood in with the same generation, its sessions and its run: its pull
// request, branch, phase records, design gate and, in awaiting_children, the start it holds
// (record.Issue.ChildrenWait, R13). Its record takes the observation and the status of the phase
// it stands in, which is queued to Dispatch where it differs from what Dispatch shows (Engine.status),
// as a move into that phase queues it; a phase with no status of its own keeps the status Dispatch
// shows.
//
// The worker of the phase it stands in is started at a new phase entry (start), raised as a move
// back raises one (enter: the return is from the phase itself), with the phase's resume task and the
// reason the entry is new (returnTask): so every completion of the entry before the stop is
// refused where it commits, and the worker is handed the work of this entry rather than the one
// the stop ended (R20). The start waits for a slot among the issues whose work came back, oldest
// first, from the moment the return wrote it (record.Waiting, LineReturning), unless the phase
// keeps none. A phase whose terminating fact is already met starts no worker (PhaseFinished): a
// merged issue stands in merging only for its close.
//
// Every other claim of the issue that has run is started again in its own session (decision 9:
// a stop suspended it, a person's done retired it), and the outbox's start brings each back
// through its suspended or retired branch, as `legion claims retry` does: the claims of the roles
// that finished their phases, which stay live until the issue closes, and the issue's own
// architect. A root that has still to make its first start (record.QueuedRoot) is the waiting
// line's: its promotion starts its architect, so nothing of it is started here. An issue that
// waits — at admitted for its first phase, in awaiting_children for its open children (R11:
// a return above a waiting parent starts no role of it), or on a person — starts no phase worker
// here: the line, the children's close or the person's answer is what starts it.
//
// A held issue returns held, its architect told nothing new: a todo releases the hold as a todo
// on any held issue in work does (releaseHeldRoot), and any other status leaves the hold for the
// architect's retry. The architect above the issue, where an issue above it is in Legion's work, is
// told the issue is back in work.
func (e *Engine) ReturnToWork(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) error {
	back := observedIssue(issue, fact)
	back.Status = fact.Status
	status := PhaseStatus(back.Phase)
	if err := e.status(ctx, tx, back, status, ""); err != nil {
		return err
	}
	if status != "" {
		back.Status = status
	}
	if err := e.store.PutIssue(ctx, tx, back); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: an issue a stop left came back into Legion's work", "issue", back.Key, "phase", back.Phase,
		"generation", back.Generation, "status", back.Status)
	if err := e.tellAbove(ctx, tx, back, fmt.Sprintf("%s is %s again; it resumes %s at a new phase entry", back.Key, fact.Status, back.Phase)); err != nil {
		return err
	}
	if back.Phase == phase.Held && back.Hold != nil {
		// The held role's claim failed, and the architect's retry is what starts it again: every
		// other claim comes back, and a todo releases the hold.
		if err := e.reviveClaims(ctx, tx, back, RoleFor(back.Hold.From)); err != nil {
			return err
		}
		if fact.Status != "todo" {
			return nil
		}
		return e.releaseHeldRoot(ctx, tx, back, fact)
	}
	queued, err := e.queuedRoot(ctx, tx, back)
	if err != nil || queued {
		return err
	}
	return e.resumeRun(ctx, tx, back)
}

// resumeRun starts the run issue stands in where its phase left it: every other claim of it that
// has run comes back in its own session (reviveClaims), and the worker of its phase is started at
// a new phase entry with the phase's resume task (returnTask). A phase no worker of it works
// starts no worker (runOwner), its step starting again where it stands (StartStepRecord), and a
// merge that landed while the issue was out of work closes it once nothing beneath it is in the
// workflow (closeAwaitingMerge).
func (e *Engine) resumeRun(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	owner, err := e.runOwner(ctx, tx, issue)
	if err != nil {
		return err
	}
	if err := e.reviveClaims(ctx, tx, issue, owner); err != nil {
		return err
	}
	if owner == "" {
		if err := e.StartStepRecord(ctx, tx, issue); err != nil {
			return err
		}
		return e.closeAwaitingMerge(ctx, tx, issue)
	}
	parent, err := e.testingParent(ctx, tx, issue)
	if err != nil {
		return err
	}
	return e.start(ctx, tx, issue, owner, returnToWorkTask(issue, parent), issue.Phase)
}

// runOwner is the role whose worker carries on the phase issue stands in, "" where none does: a
// phase no worker owns — admitted, awaiting_children, awaiting_person and accepting — and a phase
// whose terminating fact is already met (PhaseFinished): a merged issue stands in merging only for
// its close.
func (e *Engine) runOwner(ctx context.Context, tx pgx.Tx, issue record.Issue) (claim.Role, error) {
	switch issue.Phase {
	case phase.Admitted, phase.AwaitingChildren, phase.AwaitingPerson, phase.Accepting:
		return "", nil
	}
	owner := RoleFor(issue.Phase)
	if owner == "" {
		return "", nil
	}
	finished, err := e.phaseFinished(ctx, tx, issue)
	if err != nil || finished {
		return "", err
	}
	return owner, nil
}

// StartCarriedRun starts, at its promotion, the run a root a hand-over of the release before
// carried in brings with it (record.CarriedIn): each claim it ran with but its architect comes back
// in its own session (reviveClaims), the architect being the promotion's own start, and the worker
// of the phase it carries is handed that phase's work again at a raised entry
// (StartPromotedWorker). Its step starts again where it stands (StartStepRecord), whatever the
// promotion starts of it. A root carried in held starts no worker, and its architect is told of
// the hold as a root's (HeldNotice): the hold's first notice was routed in the tree it left.
func (e *Engine) StartCarriedRun(ctx context.Context, tx pgx.Tx, root record.Issue) error {
	if root.Phase == phase.Held && root.Hold != nil {
		if err := e.enqueue(ctx, tx, root.Key, HeldNotice(root)); err != nil {
			return err
		}
		if err := e.reviveClaims(ctx, tx, root, claim.RoleArchitect, RoleFor(root.Hold.From)); err != nil {
			return err
		}
		return e.StartStepRecord(ctx, tx, root)
	}
	owner, err := e.runOwner(ctx, tx, root)
	if err != nil {
		return err
	}
	if err := e.reviveClaims(ctx, tx, root, claim.RoleArchitect, owner); err != nil {
		return err
	}
	if owner != "" {
		if err := e.StartPromotedWorker(ctx, tx, root, owner); err != nil {
			return err
		}
	}
	return e.StartStepRecord(ctx, tx, root)
}

// returnToWorkTask is what the worker of the phase an issue stands in is told when the issue comes
// back into Legion's work after a stop (ReturnToWork): the phase's resume task (ResumePhaseTask),
// since the phase's work is under way, with why the entry it is handed is a new one. A testing
// task of a parent ends with the base-merge sentence (withBaseMergeRun), as every testing task of
// a parent does; parent says whether issue is a parent, as ResumePhaseTask's does.
func returnToWorkTask(issue record.Issue, parent bool) string {
	text := continueLine(issue) + fmt.Sprintf(" Resume the existing phase work: %s came back into Legion's work after a stop, and this is a new entry of the phase it stood in.", issue.Key)
	return withBaseMergeRun(text, issue.Phase, parent)
}

// queuedRoot says whether issue holds its own gate and has still to make its first start
// (record.QueuedRoot over its own design gate): the waiting line's promotion is what starts such a
// root and its architect.
func (e *Engine) queuedRoot(ctx context.Context, tx pgx.Tx, issue record.Issue) (bool, error) {
	if issue.Phase != phase.Admitted {
		return false, nil
	}
	holds, err := e.holdsOwnGate(ctx, tx, issue)
	if err != nil || !holds {
		return false, err
	}
	gates, err := e.store.GatesOf(ctx, tx, issue.Key)
	if err != nil {
		return false, err
	}
	return record.QueuedRoot(issue, record.TreeGate{Steps: e.cfg.Steps, Rows: gates}), nil
}

// reviveClaims starts again, each in its own session, every claim of issue that has run but the
// claims of skip, whose starts are another path's (the phase's own start, the promotion's
// architect start): one plain start each, with no task, of the issue's generation and the phase
// it stands in, so the outbox's start brings a suspended claim back by the rule and relaunches a
// retired one, and the start acts only while the issue still stands in that phase. A role holds a
// claim row only once it has run (the read of record.Store.RoleRuns, one batched call): a role that
// never ran has no session to bring back, and a start would make one with nothing asked of it.
func (e *Engine) reviveClaims(ctx context.Context, tx pgx.Tx, issue record.Issue, skip ...claim.Role) error {
	project, err := claim.ProjectToken(e.cfg.Project)
	if err != nil {
		return err
	}
	var keys []record.RoleRunKey
	for _, role := range claim.Roles {
		if slices.Contains(skip, role) {
			continue
		}
		token, err := claim.NewToken(project, issue.Key, role)
		if err != nil {
			// An issue whose key yields no claim token holds no claim row at all.
			return nil
		}
		keys = append(keys, record.RoleRunKey{Token: token, Issue: issue.Key, Role: role, Generation: issue.Generation})
	}
	runs, err := e.store.RoleRuns(ctx, tx, keys)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if runs[key.Token].Claim == nil {
			continue
		}
		if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: key.Role,
			Generation: issue.Generation, Phase: issue.Phase}); err != nil {
			return err
		}
	}
	return nil
}

// tellAbove tells the architect above issue, where an issue above it is in Legion's work, reason as
// a child-status notice: the owner walk starts at the parent (record.NoticeStartsAtParent). An
// issue with nothing above it in Legion's work has no architect above to tell.
func (e *Engine) tellAbove(ctx context.Context, tx pgx.Tx, issue record.Issue, reason string) error {
	chains, err := record.ReadChains(ctx, tx, e.store, issue.Project, []string{issue.Key})
	if err != nil {
		return err
	}
	if len(chains.Above(issue.Key)) == 0 {
		return nil
	}
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: "child-status", Role: claim.RoleArchitect, Reason: reason})
}

// NewRun opens a new run of issue: a new generation from its first phase, for decision 8's three
// cases (backInWork says which). The run waits at admitted for its first start, at the status
// Dispatch shows, whichever status in work brought it: a root that a person handed to Legion waits
// in the line for its promotion (record.QueuedRoot), and an issue under a parent in Legion's work
// for its first phase (record.Waiting), as any first start does.
//
// It clears the issue's own run and nothing of another issue's (R17 of dispatch://INFRA-3320): the
// held start, the hold, the wait on a person and the close Dispatch refused go with the run that
// ended, so does the mark that the run stood in a phase (record.Issue.TakenBack, decision 19) and
// the mark that the architect parked it (record.Issue.ParkedByArchitect), and the generation's
// facts are cleared (record.Store.ClearGeneration): a merged or closed pull request, the rounds of
// an open one, the design gate and the recorded handoffs. A person's approval of a version the
// issue's document has since moved past is kept first (keepStaleApproval, R3), so the new run's
// architect is told to request approval of the latest version. An open production check closes as
// not run (markCheckClosed): the new run's generation fences a completion left by an interrupted
// phase, and the closed check refuses an old look's report, so every claim stays live. The claims
// come back on their sessions when the new run's starts reach them.
//
// A generation that ended takes what its architect handed back with it: the new run is a handout
// (record.Store.StartHandout), so an ask or a hand-back of the old run, delivered late, declares
// nothing in this one, and its catch-up cursor starts again (record.Store.RestartCaughtUp). The
// new generation is news of the issue whatever read brought it, so it is marked moved
// (record.Store.MarkIssueMoved). The architect above the issue, where an issue above it is in
// Legion's work, is told.
func (e *Engine) NewRun(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchIssue) error {
	if issue.Generation == ^uint64(0) {
		return fmt.Errorf("open a new run of %s: generation overflows", issue.Key)
	}
	check, err := e.store.ProductionCheck(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	if check != nil && check.Open() {
		if err := e.markCheckClosed(ctx, tx, check, record.CheckNotRun); err != nil {
			return err
		}
	}
	if err := e.keepStaleApproval(ctx, tx, issue.Key); err != nil {
		return err
	}
	if err := e.store.ClearGeneration(ctx, tx, issue.Key, e.now()); err != nil {
		return err
	}
	run := observedIssue(issue, fact)
	run.Generation++
	run.Phase, run.Hold, run.Awaiting, run.ChildrenWait, run.PendingClose = phase.Admitted, nil, nil, nil, nil
	run.TakenBack, run.ParkedByArchitect = false, false
	run.Status = fact.Status
	if err := e.store.PutIssue(ctx, tx, run); err != nil {
		return err
	}
	if err := e.store.MarkIssueMoved(ctx, tx, run.Key, e.now()); err != nil {
		return err
	}
	if err := e.store.StartHandout(ctx, tx, run.Key); err != nil {
		return err
	}
	if err := e.store.RestartCaughtUp(ctx, tx, run.Key); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a new run of an issue opens", "issue", run.Key, "generation", run.Generation, "status", fact.Status)
	return e.tellAbove(ctx, tx, run, fmt.Sprintf("%s is %s; it opens a new run, generation %d, from its first phase", run.Key, fact.Status, run.Generation))
}

// keepStaleApproval keeps, for the architect start of the issue key a new run is clearing, the
// person's approval a gate of its holds of a version its document has since moved past: the new
// generation's architect registers each step again on a cleared gate, and its start task
// (ParkedDecisionTask) tells it to request approval of the latest version. One approval is kept,
// read from the first required step whose approved version sits below its latest, because
// readmitted_approvals keeps one row for each issue; it carries that step, so the task names the
// document the person reviewed rather than the spec of every deployment.
//
// The approval read is the person's own (record.DesignGate.ApprovedByPerson), never the version
// an ancestor's approval opened the step at (dispatch://INFRA-3320, R2 and R3): a step that stands
// open on a basis at its latest version keeps the person's approval of an earlier version apart,
// and that is the approval kept, so the new run neither loses it nor names the basis as a person's
// approval.
//
// Where the deployment gates no step, which asks no person, nothing is kept and any pair an
// earlier new run kept is dropped: no start names one there, so no delivery would forget it.
// Otherwise an issue whose required steps hold no stale approval drops an earlier pair, which its
// gates have since outgrown; one with a required step it holds no gate for (an earlier new run
// cleared it and nothing has registered since) keeps the earlier pair, which no start has
// delivered yet.
func (e *Engine) keepStaleApproval(ctx context.Context, tx pgx.Tx, key string) error {
	steps := e.GatedSteps()
	if len(steps) == 0 {
		return e.store.DropReadmittedApproval(ctx, tx, key)
	}
	gates, err := e.store.GatesOf(ctx, tx, key)
	if err != nil {
		return fmt.Errorf("read the gate of %s for its new run: %w", key, err)
	}
	unregistered := false
	for _, step := range steps {
		gate, ok := gates[step]
		if !ok {
			unregistered = true
			continue
		}
		if approved := gate.ApprovedByPerson(); approved != nil && approved.Version < gate.LatestVersion {
			return e.store.KeepReadmittedApproval(ctx, tx, key, record.KeptApproval{
				Step: step, Approved: approved.Version, Latest: gate.LatestVersion})
		}
	}
	if unregistered {
		return nil
	}
	return e.store.DropReadmittedApproval(ctx, tx, key)
}

// RetryFailedArchitect starts again, in its session and with fresh budgets, the architect claim
// of issue, a root in Legion's work that holds its own gate, whose claim failed, as `legion claims
// retry` does (R20 of dispatch://INFRA-3320: a failed role is not restarted through a status). It
// is what a todo on such a root asks for: a person's todo the engine reads (backInWork), and the
// controller's set_status of todo, which writes no status and moves no issue under the root. The
// start is the plain one (childArchitectStart), with no phase, task or entry, which the outbox's
// start runs through supervise's retry of a failed claim (supervise.RequestRetry); a start already
// queued for that claim is that start, so no second one is queued. It answers whether the claim
// had failed, and writes nothing for any other issue or claim.
func (e *Engine) RetryFailedArchitect(ctx context.Context, tx pgx.Tx, issue record.Issue) (bool, error) {
	if !record.InLegionsWork(issue) {
		return false, nil
	}
	holds, err := e.holdsOwnGate(ctx, tx, issue)
	if err != nil || !holds {
		return false, err
	}
	project, err := claim.ProjectToken(e.cfg.Project)
	if err != nil {
		return false, err
	}
	token, err := claim.NewToken(project, issue.Key, claim.RoleArchitect)
	if err != nil {
		return false, nil
	}
	run, err := e.store.RoleRun(ctx, tx, token, issue.Key, claim.RoleArchitect, issue.Generation)
	if err != nil || run.Claim == nil || run.Claim.State != supervise.StateFailed {
		return false, err
	}
	if architectStartQueued(run, issue.Phase) {
		return true, nil
	}
	e.logOnCommit(ctx, "workflow: a todo on a root whose architect failed starts that architect again", "issue", issue.Key, "phase", issue.Phase)
	return true, e.enqueue(ctx, tx, issue.Key, childArchitectStart(issue))
}
