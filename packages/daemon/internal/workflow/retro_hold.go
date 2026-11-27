package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// retroHold is what holds issue before its retro where it moves into to, the retro, on w, the
// walk of its run, and "" for every other move and where nothing holds it; detail names what the
// hold waits for. A task is done only once nothing holds it here, and the holds are read in this
// order:
//
//  1. record.HoldForChildren, with the issues beneath issue still in the workflow (openBeneath),
//     where w is stored and its task's last step is not the owner's sign-off. A parent's head
//     proves its children's merged work. The owner's sign-off is refused while one of them runs
//     (SIGNOFF_CHILD_IN_WORKFLOW), so a task whose last step is the sign-off passes into its retro
//     with none of them in the workflow. A task whose last step is another has no such refusal
//     before its retro: a task with no step acceptance:owner, and a task whose sign-off stands
//     before another step (`implementer acceptance:owner reviewer`), where an issue beneath the
//     parent may enter the workflow after the sign-off. An issue with no plan rows walks today's
//     line, whose retro follows the owner's sign-off, so it is never held so.
//  2. record.HoldForFix, with the check and the head of a fix of its task that reads its job or
//     runs (record.Store.RunningFix): the steps beside a fix wait for it.
//  3. record.HoldForCI, with what of the CI at its task's head has not passed (ciWaits), for
//     every task, with plan rows or none: each check that a CI job runs must pass at the head.
//
// Each hold stands in place of the retro, from retro (holdBeforeRetro), which keeps no slot
// (record.KeepsSlot reads record.StandsAt), so the issue holds none of the slots the work it
// waits for may need, and nothing of its own runs on one. The hold ends once nothing holds the
// issue here any longer (startHeldRetro).
func (e *Engine) retroHold(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, to phase.Phase) (record.HoldReason, []string, error) {
	if to != phase.Retro {
		return "", nil, nil
	}
	if steps := w.steps(); w.Stored && len(steps) > 0 && steps[len(steps)-1] != phase.Accepting {
		beneath, err := e.openBeneath(ctx, tx, issue)
		if err != nil {
			return "", nil, err
		}
		if len(beneath) > 0 {
			return record.HoldForChildren, beneath, nil
		}
	}
	if w.Stored {
		fix, err := e.store.RunningFix(ctx, tx, issue.Key, issue.Generation, w.Task)
		if err != nil {
			return "", nil, err
		}
		if fix != nil {
			return record.HoldForFix, []string{fix.Check, fix.Head}, nil
		}
	}
	_, waits, err := e.ciWaits(ctx, tx, issue.Key)
	if err != nil || len(waits) == 0 {
		return "", nil, err
	}
	return record.HoldForCI, waits, nil
}

// ciWaits is the head of the task of the issue key (taskHead) and what of its CI there has not
// passed (record.CIWaits): each check of the head's set that a CI job may run and that has not
// passed there, in the set's order, or the one word that no set of checks is held at the head
// (record.HeadStates.Checks nil: Dispatch has published nothing there yet), as the testing move
// waits on a set not held (judgeStates). A check that no CI job runs waits for nothing here,
// whatever its state, and a set held empty waits for nothing. An issue with no open pull request
// has no head, and nothing waits.
func (e *Engine) ciWaits(ctx context.Context, tx pgx.Tx, key string) (head string, waits []string, err error) {
	pr, err := e.store.PullRequest(ctx, tx, key)
	if err != nil {
		return "", nil, err
	}
	repo, head := taskHead(pr)
	if head == "" {
		return "", nil, nil
	}
	held, err := e.store.HeadStates(ctx, tx, key, repo, head)
	if err != nil {
		return "", nil, err
	}
	if held.Checks == nil {
		return head, []string{"no check set held at " + head}, nil
	}
	return head, record.CIWaits(held), nil
}

// endHoldNotice is the retro-held notice of issue, on w, the walk of its run, held before its
// retro for reason with detail (retroHold), at the head of pr's: what holds it and what ends the
// hold, and, for a hold for CI, the owner's acts before then (acts, ciActsOf).
func endHoldNotice(issue record.Issue, w Walk, pr *record.PullRequest, reason record.HoldReason, detail []string, acts ciActs) record.Notice {
	switch reason {
	case record.HoldForFix:
		check, head := "", ""
		if len(detail) == 2 {
			check, head = detail[0], detail[1]
		}
		return retroHeldFor(fmt.Sprintf("%s passed its last step, and Legion holds it before its retro while a fix of task %s runs (check %s at %s).",
			issue.Key, w.Task, check, head))
	case record.HoldForCI:
		_, head := taskHead(pr)
		return retroHeldFor(fmt.Sprintf("%s passed its last step, and Legion holds it before its retro until each check a CI job runs passes at %s: %s. "+
			"Nothing is asked of you now: a failure starts a fix where the task has one, else Legion tells you once (ci-failed). ",
			issue.Key, head, strings.Join(detail, ", ")) + acts.before())
	}
	return retroHeldNotice(issue.Key, detail)
}

// retroHeldFor is the retro-held notice whose reason is reason, naming the retro and the
// implementer whose work the hold keeps, as the held notice of any hold names the phase it is from
// (HeldNotice).
func retroHeldFor(reason string) record.Notice {
	return record.Notice{Kind: "retro-held", Role: phase.RoleFor(phase.Retro), Phase: phase.Retro, Reason: reason}
}

// retroHeldNotice tells the owner of parent that Legion holds it before its retro for beneath, the
// issues beneath it still in the workflow, and what ends the hold: each of them run to done, or
// ended by the owner's own status write in Dispatch, after which the retro starts unless the
// task's end holds it still (intake.RetroUnlessEndHold). It carries that write's rules word for
// word (record.EndChildRule) and the sentence on a write the daemon has not taken yet
// (record.StatusNotTakenYet), as the pr-merged notice does (waitingForTree).
func retroHeldNotice(parent string, beneath []string) record.Notice {
	return retroHeldFor(fmt.Sprintf(
		"%s passed its last step, and Legion holds it before its retro while these issues beneath it are still in the workflow: %s. "+
			"A parent is accepted only once no issue beneath it is still in the workflow. Run each one to done, or end it; when the last one leaves, Legion starts the retro, %s.",
		parent, strings.Join(beneath, ", "), intake.RetroUnlessEndHold) + " " + record.StatusNotTakenYet + " " + record.EndChildRule)
}

// heldParentWait is why parent, held before its retro for the issues beneath it
// (record.HoldForChildren), still waits, decided once (heldParentWaitOf) for both answers that say
// it: the refusal of a release of the hold (planHoldRefusal) and the reason posted beside the status
// a todo on a held root is set back to (releaseHeldRoot). beneath names the issues beneath it still
// open (openBeneath). With none open its tree alone holds it, since the settle of every fact
// releases such a parent in a tree that runs (releaseHeldParents): the tree lingers after its root
// left the workflow, where lingers says so, and otherwise its root waits in the line for the
// promotion that opens it, a hand-over among them (record.TreeRuns, record.QueuedRoot).
type heldParentWait struct {
	parent  record.Issue
	beneath []string
	lingers bool
}

// heldParentWaitOf decides why parent, held for the issues beneath it, still waits.
func (e *Engine) heldParentWaitOf(ctx context.Context, tx pgx.Tx, parent record.Issue) (heldParentWait, error) {
	beneath, err := e.openBeneath(ctx, tx, parent)
	if err != nil || len(beneath) > 0 {
		return heldParentWait{parent: parent, beneath: beneath}, err
	}
	lingers, err := record.TreeLingers(ctx, e.store, tx, parent.Tree)
	return heldParentWait{parent: parent, lingers: lingers}, err
}

// refusal is the answer to a release of the hold, which would start the retro's role early: the
// issues beneath still open, the last of which starts the retro (intake.RetryOfHeldParent), or why
// the tree runs nothing yet (intake.RetryOfHeldParentUntilTreeRuns).
func (w heldParentWait) refusal() *intake.Refusal {
	if len(w.beneath) > 0 {
		return intake.RetryOfHeldParent(w.parent.Key, w.beneath)
	}
	return intake.RetryOfHeldParentUntilTreeRuns(w.parent.Key, w.parent.Tree, w.lingers)
}

// reason is the reason posted beside the status a todo on the held root is set back to, which the
// person who set the todo reads: the issues beneath still open, the last of which starts the retro
// (intake.HeldParentWaitsForChildren), or why its tree runs nothing yet
// (intake.HeldParentWaitsForTree). It carries none of the architect's rules for ending a child,
// which only the architect's retro-held notice gives (retroHeldNotice).
func (w heldParentWait) reason() string {
	if len(w.beneath) > 0 {
		return intake.HeldParentWaitsForChildren(w.parent.Key, w.beneath)
	}
	return intake.HeldParentWaitsForTree(w.parent.Key, w.parent.Tree, w.lingers)
}

// releaseHeldParents is the one place a parent held before its retro for the issues beneath it
// (record.HoldForChildren) is released, at the settle of every fact and of every other
// transaction the daemon settles (SettleFact, intake.OpenSettlers): each such parent of each tree
// in which the fact wrote an issue (record.IssuesWritten) has its retro started where nothing
// beneath it is in the workflow and its tree runs (startHeldRetro). Every change of what stands
// beneath a parent is the write of an issue of the parent's tree: an issue beneath it that leaves
// the workflow (leave), a re-parent in Dispatch that moves one out from under it (admit's
// recordObservation), the parent itself handed over as a root of its own with its hold and its
// plan once its old root's linger closes (admit's readmit), and the promotion or the reopen of a
// root, which makes a tree that ran nothing run again. So nothing can leave a parent held with
// nothing open beneath it in a tree that runs: such a parent stands in a tree that runs nothing,
// whose root lingers or waits in the line, and the hold's refusal of a retry names that wait
// (planHoldRefusal) where it names no issue still open beneath it. Each tree is read once, and a
// fact that wrote no issue reads nothing.
func (e *Engine) releaseHeldParents(ctx context.Context, tx pgx.Tx, written []string) error {
	var trees []string
	for _, key := range written {
		issue, err := e.store.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		if issue != nil && !slices.Contains(trees, issue.Tree) {
			trees = append(trees, issue.Tree)
		}
	}
	for _, tree := range trees {
		members, err := e.store.TreeIssues(ctx, tx, tree)
		if err != nil {
			return err
		}
		for _, member := range members {
			if err := e.startHeldRetro(ctx, tx, member); err != nil {
				return err
			}
		}
	}
	return nil
}

// releaseRetroHold reads again the end of the issue key where it is held before its retro
// (startHeldRetro), at the tail of every fact that names it (fixRule): a fact that holds a state
// of its checks, ends its fix or moves its head can end what holds it, and no such fact need write
// the issue, which the settle reads.
func (e *Engine) releaseRetroHold(ctx context.Context, tx pgx.Tx, key string) error {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return err
	}
	return e.startHeldRetro(ctx, tx, *issue)
}

// startHeldRetro is the one release of an issue held before its retro (retroHold): for its
// children (record.HoldForChildren), at the settle of a fact that wrote an issue of its tree
// (releaseHeldParents), and for its fix or its CI at its task's end (atTaskEnd), at the tail of a
// fact that names it (releaseRetroHold) as well. Every other issue it leaves as it stands. A tree
// that runs nothing (record.TreeRunning), one whose root lingers or waits in the line for its tree
// to open, starts no retro, as it runs no transition: the promotion or the reopen that runs it
// again writes its root, and that settle starts the retro. Where retroHold still answers a hold,
// the issue stays held, for the reason it answers now (rehold): the last issue beneath a parent
// can leave while its CI runs, and a fix can end at a head whose CI has not passed.
//
// Where nothing holds the issue any longer, the hold is cleared and the issue stands in its
// retro, where its task already stands ({retro 0}, moveTask at the hold), or, for a run with no
// plan rows, the retro its sign-off moved it to. The retro's Dispatch status is queued through
// Engine.status, since the hold wrote none and the record still holds the status of the last step
// (PhaseStatus(held) is empty). The implementer is started with the task the pass into the retro
// would have started it with (transition): the report of the step the task passed last
// (retroReport), which the hold kept on that step's phases row, and the pull request. Its start
// raises its entry and the count of starts of the retro's place (start, which writes the retro's
// step), not restart, which raises neither, since this retro never ran. The act, which the log
// line names, is children_done for a parent's hold and ci_passed for a task's end: no row of the
// table moves a held issue.
func (e *Engine) startHeldRetro(ctx context.Context, tx pgx.Tx, held record.Issue) error {
	if !(record.HeldForChildren(held) || atTaskEnd(held)) || !record.InWorkflow(held) {
		return nil
	}
	root, err := e.store.Issue(ctx, tx, held.Tree)
	if err != nil || root == nil {
		return err
	}
	if running, err := record.TreeRunning(ctx, e.store, tx, *root, e.cfg.Steps); err != nil || !running {
		return err
	}
	w, err := e.walk(ctx, tx, held)
	if err != nil {
		return err
	}
	// A parent whose plan rows a way back dropped is held by no plan (planHold): it is a plain
	// hold, which a retry releases, and nothing starts a retro its task cannot stand at. A task's
	// end held for its CI is every run's, and a run with no plan rows has no place to check.
	if !w.Stored && record.HeldForChildren(held) {
		return nil
	}
	steps := w.steps()
	if w.Stored && (len(steps) == 0 || w.Place != (record.TaskPlace{Kind: record.PlaceRetro})) {
		return fmt.Errorf("start the held retro of %s: task %s stands at %+v of the line %v, not at its retro", held.Key, w.Task, w.Place, w.Line)
	}
	reason, detail, err := e.retroHold(ctx, tx, held, w, phase.Retro)
	if err != nil {
		return err
	}
	if reason != "" {
		return e.rehold(ctx, tx, held, w, reason, detail)
	}
	// Nothing holds the task any longer, and no step of it runs again: the branches beside its
	// fixes go (queueTaskEnd).
	if err := e.queueTaskEnd(ctx, tx, held, w); err != nil {
		return err
	}
	report, err := e.retroReport(ctx, tx, held, w)
	if err != nil {
		return err
	}
	pr, err := e.store.PullRequest(ctx, tx, held.Key)
	if err != nil {
		return err
	}
	act := "ci_passed"
	if record.HeldForChildren(held) {
		act = "children_done"
	}
	status := PhaseStatus(phase.Retro)
	if err := e.status(ctx, tx, held, status, ""); err != nil {
		return err
	}
	released := held
	released.Phase, released.Hold, released.Status = phase.Retro, nil, status
	if err := e.WriteIssue(ctx, tx, released); err != nil {
		return err
	}
	// The release takes the issue out of held into a phase, which is a movement (the rule in
	// dispatchIssue).
	if err := e.store.MarkIssueMoved(ctx, tx, released.Key, e.now()); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: nothing holds a task before its retro any longer, and its retro starts", "issue", released.Key, "tree", released.Tree,
		"task", w.Task, "act", act, "hold", held.Hold.Reason, "from", phase.Held, "to", phase.Retro, "status", released.Status)
	role := phase.RoleFor(phase.Retro)
	if err := e.clearHandoff(ctx, tx, released.Key, role); err != nil {
		return err
	}
	if err := e.start(ctx, tx, released, w, role, func(at Standing) string {
		return task(released, at, report, pr, "")
	}, false); err != nil {
		return err
	}
	// The release moves the issue into its retro as the pass into the retro would have
	// (transition), so it queues the reconcile of its tree's news as that pass does
	// (queueMovedNews). It asks nothing of the merge forward, as the hold before it asked nothing
	// (holdBeforeRetro): the retro is where the merge comes next (classify.MergeComesNext), where a
	// move asks nothing (mergeForwardOnEntry).
	return e.queueMovedNews(ctx, tx, released)
}

// rehold keeps held, standing before its retro, held for reason with detail (retroHold), what
// holds it now. A hold of that reason already changes nothing. Any other is written in its place,
// from retro still, and its owner is told what holds the task now by its retro-held notice: the
// last issue beneath a parent left while its CI has not passed, or a fix ended at a head where it
// has not.
func (e *Engine) rehold(ctx context.Context, tx pgx.Tx, held record.Issue, w Walk, reason record.HoldReason, detail []string) error {
	if held.Hold.Reason == reason {
		return nil
	}
	was := held.Hold.Reason
	hold := *held.Hold
	hold.Reason = reason
	held.Hold = &hold
	if err := e.WriteIssue(ctx, tx, held); err != nil {
		return err
	}
	pr, err := e.store.PullRequest(ctx, tx, held.Key)
	if err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: what holds a task before its retro changed", "issue", held.Key, "task", w.Task, "was", was,
		"reason", reason, "waitsFor", detail)
	acts, err := e.ciActsOf(ctx, tx, held, w)
	if err != nil {
		return err
	}
	return e.notice(ctx, tx, held.Key, endHoldNotice(held, w, pr, reason, detail, acts))
}

// retroReport is the one answer of the report issue's retro is told, on w, the walk of its run:
// where its task's last step is the owner's acceptance (a stored walk ending in
// acceptance:owner, or today's line), the report of the step the sign-off accepted
// (acceptedReport), which the sign-off's own move hands its transition; else the phases row of the
// last step's role. The move into the retro and the release of a hold before it read it alike, so
// the two cannot tell the retro two reports.
func (e *Engine) retroReport(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) (record.PhaseRow, error) {
	steps := w.steps()
	if len(steps) == 0 {
		return record.PhaseRow{}, fmt.Errorf("the retro's report of %s: task %s of its line %v has no step", issue.Key, w.Task, w.Line)
	}
	if last := steps[len(steps)-1]; last != phase.Accepting {
		return e.phaseRow(ctx, tx, issue.Key, phase.RoleFor(last))
	}
	return e.acceptedReport(ctx, tx, issue)
}
