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
)

// retroHeld is the first of the three holds on issue's acceptance that holds it (acceptanceHeld,
// in R8's order) where issue moves into the retro (to) and its task's last step is not the owner's
// sign-off, and no hold for every other move. A parent is accepted only once checks across the
// issues under it prove each of its requirements (dispatch://INFRA-3320 R8): after the issues
// beneath it in its tree, the roots of any tree that rest on its approval, and its open Dispatch
// children. The owner's sign-off is refused while any of them holds it (acceptanceHolds), so a
// task whose last step is the sign-off passes into its retro with none of them holding it. A task
// whose last step is another has no such refusal before its retro: a task with no step
// acceptance:owner, and a task whose sign-off stands before another step (`implementer
// acceptance:owner reviewer`), where an issue may come to hold the parent after the sign-off. Such
// a parent is held before its retro instead (transition), from retro for its children
// (record.HoldForChildren), until none of the three holds it (StartHeldRetro). The hold stands in
// place of the retro, which keeps no slot (record.KeepsSlot reads record.StandsAt), so the parent
// holds none of the slots its children may need. An issue with no plan rows walks today's line
// (todaysWalk), whose last step is the owner's sign-off, so it is never held so.
func (e *Engine) retroHeld(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, to phase.Phase) (acceptanceHold, error) {
	steps := w.steps()
	if to != phase.Retro || len(steps) == 0 || steps[len(steps)-1] == phase.Accepting {
		return acceptanceHold{}, nil
	}
	return e.acceptanceHeld(ctx, tx, issue)
}

// retroHeldNotice tells the owner of parent that Legion holds it before its retro while held, the
// first of the three holds on its acceptance, holds it: the move that entered the hold, the pass of
// its task's last step or, where movedBack, a move back out of merging; each issue of that hold, in
// the words of the sign-off's refusal of it (acceptanceHold.holding); what that refusal tells the
// owner to do about them (holdTexts.retro); and what starts the retro
// (intake.HeldParentRetroStarts). A notice of the issues beneath it or of its open Dispatch
// children ends with the sentence on a write the daemon has not taken yet and that write's rules
// word for word (endChildWords), as the pr-merged notice does (waitingForTree); a resting root is
// a tree of its own, which no status write of this architect ends. It names the retro and the
// implementer whose work the hold keeps, as the held notice of any hold names the phase it is from
// (HeldNotice).
func retroHeldNotice(parent string, held acceptanceHold, movedBack bool) record.Notice {
	texts := acceptanceHolds[held.code]
	entered := "%[1]s passed its last step"
	if movedBack {
		entered = "%[1]s moved back from merging"
	}
	reason := fmt.Sprintf(entered+", and Legion holds it before its retro while %[2]s. "+texts.retro+" %[3]s", parent, held.holding(), intake.HeldParentRetroStarts)
	if texts.childRules {
		reason += " " + endChildWords
	}
	return record.Notice{Kind: "retro-held", Role: phase.RoleFor(phase.Retro), Phase: phase.Retro, Reason: reason}
}

// tellRetroHeld tells the owner of parent, held before its retro on w, the walk of its run, what
// holds it (retroHeldNotice): once for each of the three holds on its acceptance and each run of
// the retro the hold stands in place of. The hold tells the first that holds the parent as it is
// entered (holdBeforeRetro, where entry is the trigger of the move that entered it), and each read
// of the holds after it (StartHeldRetro, at the settle of each fact and at an admission pass,
// where entry is "") tells the first that holds it then where no notice of the run has named that
// hold: so the owner hears of each hold as it comes first in R8's order, naming each issue that
// holds the parent then, and never again for a settle or a pass that finds the same hold. The run
// is the count of starts of the retro's place on the parent's task (record.StepStarts) in its
// generation, which the release raises, so a parent held again after a later move into its retro
// is told again. The marks are written in the notice's own transaction.
//
// Each notice of a run names the move that entered its hold: the hold marks a run a move back out
// of merging entered (TriggerBackward), and a later read words its notice by that mark. A run with
// no such mark was entered by the pass of the task's last step, or by an earlier release, whose
// hold marked no move; a later read words the notice of such a run as that pass.
//
// The issues beneath the parent are told by a later read only in a run whose hold this release
// entered, which marks the run so: a hold an earlier release entered held the parent for the
// issues beneath it alone and told its owner of them then, so the first settle after the upgrade
// tells them nothing again, and tells only a hold of the other two that comes to hold the parent.
func (e *Engine) tellRetroHeld(ctx context.Context, tx pgx.Tx, parent record.Issue, w Walk, held acceptanceHold, entry TriggerKind) error {
	retro, err := e.store.StepStarts(ctx, tx, parent.Key, record.TaskPlace{Kind: record.PlaceRetro}.StepPlace(w.Task))
	if err != nil {
		return err
	}
	run := fmt.Sprintf("%s:%d:%s:%d", parent.Key, parent.Generation, w.Task, retro.Starts)
	movedBack := entry == TriggerBackward
	if entry != "" {
		if _, err := e.store.MarkProcessed(ctx, tx, "retro-held-entered", run); err != nil {
			return err
		}
		if movedBack {
			if _, err := e.store.MarkProcessed(ctx, tx, "retro-held-moved-back", run); err != nil {
				return err
			}
		}
	} else if held.code == signOffChildInWorkflow {
		if ours, err := e.store.Processed(ctx, tx, "retro-held-entered", run); err != nil || !ours {
			return err
		}
	}
	fresh, err := e.store.MarkProcessed(ctx, tx, "retro-held-told", run+":"+held.code)
	if err != nil || !fresh {
		return err
	}
	if entry == "" {
		if movedBack, err = e.store.Processed(ctx, tx, "retro-held-moved-back", run); err != nil {
			return err
		}
	}
	return e.notice(ctx, tx, parent.Key, retroHeldNotice(parent.Key, held, movedBack))
}

// heldParentWait is why parent, held before its retro (record.HoldForChildren), still waits,
// decided once (heldParentWaitOf) for both answers that say it: the refusal of a release of the
// hold (planHoldRefusal) and the reason posted beside the status a todo on a held root is set back
// to (releaseHeldRoot). held is the first of the three holds on its acceptance that holds it
// (acceptanceHeld). With none holding it its tree alone holds it, since the settle of every fact
// and every admission pass release such a parent in a tree that runs (releaseHeldParents, admit's
// childrenWaits): the tree lingers after its root left the workflow, where lingers says so, and
// otherwise its root waits in the line for the promotion that opens it, a hand-over among them
// (record.TreeRuns, record.QueuedRoot).
type heldParentWait struct {
	parent  record.Issue
	held    acceptanceHold
	lingers bool
}

// heldParentWaitOf decides why parent, held before its retro, still waits.
func (e *Engine) heldParentWaitOf(ctx context.Context, tx pgx.Tx, parent record.Issue) (heldParentWait, error) {
	held, err := e.acceptanceHeld(ctx, tx, parent)
	if err != nil || held.code != "" {
		return heldParentWait{parent: parent, held: held}, err
	}
	lingers, err := record.TreeLingers(ctx, e.store, tx, parent.Tree)
	return heldParentWait{parent: parent, lingers: lingers}, err
}

// refusal is the answer to a release of the hold, which would start the retro's role early: the
// hold that holds the parent, naming each of its issues (intake.RetryOfHeldParent), or why the
// tree runs nothing yet (intake.RetryOfHeldParentUntilTreeRuns).
func (w heldParentWait) refusal() *intake.Refusal {
	if w.held.code != "" {
		return intake.RetryOfHeldParent(w.parent.Key, w.held.holding())
	}
	return intake.RetryOfHeldParentUntilTreeRuns(w.parent.Key, w.parent.Tree, w.lingers)
}

// reason is the reason posted beside the status a todo on the held root is set back to, which the
// person who set the todo reads: the hold that holds the root, naming each of its issues
// (intake.HeldParentWaitsForChildren), or why its tree runs nothing yet
// (intake.HeldParentWaitsForTree). It carries none of the architect's rules for ending a child,
// which only the architect's retro-held notice gives (retroHeldNotice).
func (w heldParentWait) reason() string {
	if w.held.code != "" {
		return intake.HeldParentWaitsForChildren(w.parent.Key, w.held.holding())
	}
	return intake.HeldParentWaitsForTree(w.parent.Key, w.parent.Tree, w.lingers)
}

// releaseHeldParents is where a parent held before its retro (record.HoldForChildren) is released
// at the settle of every fact and of every other transaction the daemon settles (SettleFact,
// intake.OpenSettlers): each such parent of each tree in which the fact wrote an issue
// (record.IssuesWritten), or which holds an ancestor a root stopped resting on in the fact
// (restEnded), has its retro started where none of the three holds on its acceptance holds it and
// its tree runs, and is told a hold no notice of its run has named where one does
// (StartHeldRetro). Every change of what stands beneath a parent is the write of an issue of the
// parent's tree: an issue beneath it that leaves the workflow (leave), a re-parent in Dispatch
// that moves one out from under it (admit's recordObservation), the parent itself handed over as a
// root of its own with its hold and its plan once its old root's linger closes (admit's readmit),
// and the promotion or the reopen of a root, which makes a tree that ran nothing run again. A root
// that rests on the parent's approval stops resting on it when it leaves the workflow or when its
// step's wait on the parent's approval request ends, each of which names the parent (restEnded).
// The end of a hold over a Dispatch child, a child never released among them, changes the daemon's
// mirror of Dispatch alone, which the admission pass reads for every held parent of its project
// (admit's childrenWaits, StartHeldRetro). So nothing leaves a parent held with nothing holding it
// in a tree that runs past the next settle or pass: such a parent stands in a tree that runs
// nothing, whose root lingers or waits in the line, and the hold's refusal of a retry names that
// wait (planHoldRefusal) where it names no issue that holds it. Each tree is read once, and a fact
// that wrote no issue and ended no rest reads nothing.
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
			if _, err := e.StartHeldRetro(ctx, tx, member); err != nil {
				return err
			}
		}
	}
	return nil
}

// StartHeldRetro starts the retro of parent where it is held before it (record.HoldForChildren),
// none of the three holds on its acceptance holds it (acceptanceHeld) and its tree runs
// (record.TreeRunning), in the transaction whose settle or admission pass finds it so, and answers
// whether it started it; every other issue it leaves as it stands. Where a hold still holds the
// parent, its owner is told it if no notice of the run has named it yet (tellRetroHeld). A tree
// that runs nothing, one whose root lingers or waits in the line for its tree to open, starts no
// retro, as it runs no transition: the promotion or the reopen that runs it again writes its root,
// and that settle starts the retro. The settle reads each held parent of each tree the fact wrote
// (releaseHeldParents). The admission pass reads, with nobody acting, a held parent of its project
// whose holds its own facts show changed or gone (admit's childrenWaits), because the end of a hold
// over a Dispatch child, a child closed, moved to icebox or moved out from under the parent, never
// released or not, and the start of one, change the daemon's mirror of Dispatch alone, which no
// settle of the engine reads.
//
// The hold is cleared and parent stands in its retro, where its task already stands ({retro 0},
// moveTask at the hold). The retro's Dispatch status is queued through Engine.status, since the
// hold wrote none and the record still holds the status of the last step (PhaseStatus(held) is
// empty). The implementer is started with the task the pass into the retro would have started it
// with (transition): the report of the step the task passed last, its handoff, or the verdict of
// a role of the deployment, which writes none, as the hold kept them on that step's phases row,
// and the pull request. Its start raises its entry and the count of starts of
// the retro's place (start, which writes the retro's step), not restart, which raises neither,
// since this retro never ran. The act is children_done, which the log line names: no row of the
// table moves a held issue.
func (e *Engine) StartHeldRetro(ctx context.Context, tx pgx.Tx, parent record.Issue) (bool, error) {
	if !record.HeldForChildren(parent) || !record.InWorkflow(parent) {
		return false, nil
	}
	root, err := e.store.Issue(ctx, tx, parent.Tree)
	if err != nil || root == nil {
		return false, err
	}
	if running, err := record.TreeRunning(ctx, e.store, tx, *root, e.cfg.Steps); err != nil || !running {
		return false, err
	}
	w, err := e.walk(ctx, tx, parent)
	if err != nil {
		return false, err
	}
	// A parent whose plan rows a way back dropped is held by no plan (planHold): it is a plain
	// hold, which a retry releases, and nothing starts a retro its task cannot stand at.
	if !w.Stored {
		return false, nil
	}
	held, err := e.acceptanceHeld(ctx, tx, parent)
	if err != nil {
		return false, err
	}
	if held.code != "" {
		return false, e.tellRetroHeld(ctx, tx, parent, w, held, "")
	}
	steps := w.steps()
	if len(steps) == 0 || w.Place != (record.TaskPlace{Kind: record.PlaceRetro}) {
		return false, fmt.Errorf("start the held retro of %s: task %s stands at %+v of the line %v, not at its retro", parent.Key, w.Task, w.Place, w.Line)
	}
	// The report of the task's last step is on the row of that step's role, whatever runs it: one of
	// Legion's roles or a role the deployment defines, whose phase (step) names no role of its own.
	last, err := e.phaseRow(ctx, tx, parent.Key, claim.Role(w.Steps[len(w.Steps)-1].Role))
	if err != nil {
		return false, err
	}
	pr, err := e.store.PullRequestRow(ctx, tx, parent.Key)
	if err != nil {
		return false, err
	}
	status := PhaseStatus(phase.Retro)
	if err := e.status(ctx, tx, parent, status, ""); err != nil {
		return false, err
	}
	parent.Phase, parent.Hold, parent.Status = phase.Retro, nil, status
	if err := e.WriteIssue(ctx, tx, parent); err != nil {
		return false, err
	}
	// The release takes the issue out of held into a phase, which is a movement (the rule in
	// dispatchIssue).
	if err := e.store.MarkIssueMoved(ctx, tx, parent.Key, e.now()); err != nil {
		return false, err
	}
	e.logOnCommit(ctx, "workflow: nothing holds a parent held before its retro any longer, and its retro starts", "issue", parent.Key, "tree", parent.Tree,
		"task", w.Task, "act", "children_done", "from", phase.Held, "to", phase.Retro, "status", parent.Status)
	// The parent now stands in its retro, so it is accepted (record.Accepted) as a pass into the
	// retro makes it, and every step of another root that waits on it as an ancestor ends its wait
	// (dispatch://INFRA-3320, transition).
	if err := e.endPendingOn(ctx, tx, parent.Key, movedAccepted(parent.Key, phase.Retro)); err != nil {
		return false, err
	}
	role := phase.RoleFor(phase.Retro)
	if err := e.clearHandoff(ctx, tx, parent.Key, role); err != nil {
		return false, err
	}
	if err := e.start(ctx, tx, parent, w, role, func(at Standing) string {
		return task(parent, at, last, pr, "")
	}, phase.Held, false); err != nil {
		return false, err
	}
	// The release moves the parent into its retro as the pass into the retro would have
	// (transition), so it queues the reconcile of its tree's news as that pass does
	// (queueMovedNews). It asks nothing of the merge forward, as the hold before it asked nothing
	// (holdBeforeRetro): the retro is where the merge comes next (classify.MergeComesNext), where a
	// move asks nothing (mergeForwardOnEntry).
	return true, e.queueMovedNews(ctx, tx, parent)
}
