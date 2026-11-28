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

// endHold is what holds a task before its retro (retroHold): reason is the hold, "" where nothing
// holds it. held is, for a parent's children (record.HoldForChildren), the first of the three holds
// on its acceptance that holds it (retroHeld), and waits is what a hold for the task's fix or its CI
// waits for: the check and the head of the fix (record.HoldForFix), or what of the CI at the task's
// head has not passed (record.HoldForCI).
type endHold struct {
	reason record.HoldReason
	held   acceptanceHold
	waits  []string
}

// retroHold is what holds issue before its retro where it moves into to, the retro, on w, the
// walk of its run, and no hold for every other move and where nothing holds it. A task is done only
// once nothing holds it here, and the holds are read in this order:
//
//  1. record.HoldForChildren, with the first of the three holds on its acceptance that holds it
//     (retroHeld), where w is stored and its task's last step is not the owner's sign-off.
//  2. record.HoldForFix, with the check and the head of a fix of its task that reads its job or
//     runs (record.Store.RunningFix): the steps beside a fix wait for it.
//  3. record.HoldForCI, with what of the CI at its task's head has not passed (ciWaits), for
//     every task, with plan rows or none: each check that a CI job runs must pass at the head.
//
// Each hold stands in place of the retro, from retro (holdBeforeRetro), which keeps no slot
// (record.KeepsSlot reads record.StandsAt), so the issue holds none of the slots the work it
// waits for may need, and nothing of its own runs on one. The hold ends once nothing holds the
// issue here any longer (StartHeldRetro).
func (e *Engine) retroHold(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, to phase.Phase) (endHold, error) {
	if to != phase.Retro {
		return endHold{}, nil
	}
	held, err := e.retroHeld(ctx, tx, issue, w, to)
	if err != nil {
		return endHold{}, err
	}
	if held.code != "" {
		return endHold{reason: record.HoldForChildren, held: held}, nil
	}
	if w.Stored {
		fix, err := e.store.RunningFix(ctx, tx, issue.Key, issue.Generation, w.Task)
		if err != nil {
			return endHold{}, err
		}
		if fix != nil {
			return endHold{reason: record.HoldForFix, waits: []string{fix.Check, fix.Head}}, nil
		}
	}
	_, waits, err := e.ciWaits(ctx, tx, issue.Key)
	if err != nil || len(waits) == 0 {
		return endHold{}, err
	}
	return endHold{reason: record.HoldForCI, waits: waits}, nil
}

// ciWaits is the head of the task of the issue key (taskHead) and what of its CI there has not
// passed (record.CIWaits): each check of the head's set that a CI job may run and that has not
// passed there, in the set's order, or the one word that no set of checks is held at the head
// (record.HeadStates.Checks nil: Dispatch has published nothing there yet), as the testing move
// waits on a set not held (judgeStates). A check that no CI job runs waits for nothing here,
// whatever its state, and a set held empty waits for nothing. An issue with no open pull request
// has no head, and nothing waits.
func (e *Engine) ciWaits(ctx context.Context, tx pgx.Tx, key string) (head string, waits []string, err error) {
	pr, err := e.store.PullRequestRow(ctx, tx, key)
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
// retro for its fix or its CI (reason, record.HoldForFix or record.HoldForCI) with detail
// (retroHold), at the head of pr's: what holds it and what ends the hold, and, for a hold for CI,
// the owner's acts before then (acts, ciActsOf). A parent held for its children is told by its own
// notice (retroHeldNotice, tellRetroHeld).
func endHoldNotice(issue record.Issue, w Walk, pr *record.PullRequest, reason record.HoldReason, detail []string, acts ciActs) record.Notice {
	if reason == record.HoldForFix {
		check, head := "", ""
		if len(detail) == 2 {
			check, head = detail[0], detail[1]
		}
		return retroHeldFor(fmt.Sprintf("%s passed its last step, and Legion holds it before its retro while a fix of task %s runs (check %s at %s).",
			issue.Key, w.Task, check, head))
	}
	_, head := taskHead(pr)
	return retroHeldFor(fmt.Sprintf("%s passed its last step, and Legion holds it before its retro until each check a CI job runs passes at %s: %s. "+
		"Nothing is asked of you now: a failure starts a fix where the task has one, else Legion tells you once (ci-failed). ",
		issue.Key, head, strings.Join(detail, ", ")) + acts.before())
}

// retroHeldFor is the retro-held notice whose reason is reason, naming the retro and the
// implementer whose work the hold keeps, as the held notice of any hold names the phase it is from
// (HeldNotice).
func retroHeldFor(reason string) record.Notice {
	return record.Notice{Kind: "retro-held", Role: phase.RoleFor(phase.Retro), Phase: phase.Retro, Reason: reason}
}

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
// a parent is held before its retro instead (transition, retroHold), from retro for its children
// (record.HoldForChildren), until none of the three holds it (StartHeldRetro). The hold stands in
// place of the retro, which keeps no slot (record.KeepsSlot reads record.StandsAt), so the parent
// holds none of the slots its children may need. An issue with no plan rows walks today's line,
// whose retro follows the owner's sign-off, so it is never held so.
func (e *Engine) retroHeld(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, to phase.Phase) (acceptanceHold, error) {
	steps := w.steps()
	if !w.Stored || to != phase.Retro || len(steps) == 0 || steps[len(steps)-1] == phase.Accepting {
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
// (retroHeldFor).
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
	return retroHeldFor(reason)
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
// is told again. The marks are written in the notice's own transaction (enterRetroHeld).
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
// A hold of the task's end for its fix or its CI marks the run as entered too (holdBeforeRetro),
// so a hold on the parent's acceptance that comes to hold it later in that run is told.
func (e *Engine) tellRetroHeld(ctx context.Context, tx pgx.Tx, parent record.Issue, w Walk, held acceptanceHold, entry TriggerKind) error {
	run, err := e.enterRetroHeld(ctx, tx, parent, w, entry)
	if err != nil {
		return err
	}
	movedBack := entry == TriggerBackward
	if entry == "" && held.code == signOffChildInWorkflow {
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

// enterRetroHeld is the run of the retro that issue's hold before it on w, the walk of its run,
// stands in place of (tellRetroHeld): its key, its generation, its task and the count of starts of
// the retro's place. Where entry is the trigger of the move that entered the hold
// (holdBeforeRetro), it marks the run entered by this release, and moved back out of merging where
// entry is TriggerBackward; a later read of the holds (entry "") marks nothing.
func (e *Engine) enterRetroHeld(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, entry TriggerKind) (string, error) {
	retro, err := e.store.StepStarts(ctx, tx, issue.Key, record.TaskPlace{Kind: record.PlaceRetro}.StepPlace(w.Task))
	if err != nil {
		return "", err
	}
	run := fmt.Sprintf("%s:%d:%s:%d", issue.Key, issue.Generation, w.Task, retro.Starts)
	if entry == "" {
		return run, nil
	}
	if _, err := e.store.MarkProcessed(ctx, tx, "retro-held-entered", run); err != nil {
		return "", err
	}
	if entry == TriggerBackward {
		if _, err := e.store.MarkProcessed(ctx, tx, "retro-held-moved-back", run); err != nil {
			return "", err
		}
	}
	return run, nil
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

// releaseRetroHold reads again the end of the issue key where it is held before its retro
// (StartHeldRetro), at the tail of every fact that names it (fixRule): a fact that holds a state
// of its checks, ends its fix or moves its head can end what holds it, and no such fact need write
// the issue, which the settle reads.
func (e *Engine) releaseRetroHold(ctx context.Context, tx pgx.Tx, key string) error {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return err
	}
	_, err = e.StartHeldRetro(ctx, tx, *issue)
	return err
}

// StartHeldRetro is the one release of an issue held before its retro (retroHold), and answers
// whether it started the retro: for its children (record.HoldForChildren), where none of the three
// holds on its acceptance holds it (acceptanceHeld), at the settle of a fact that wrote an issue of
// its tree (releaseHeldParents) and at an admission pass; and for its fix or its CI at its task's
// end (atTaskEnd), at the tail of a fact that names it (releaseRetroHold) as well. Every other
// issue it leaves as it stands. A tree that runs nothing (record.TreeRunning), one whose root
// lingers or waits in the line for its tree to open, starts no retro, as it runs no transition: the
// promotion or the reopen that runs it again writes its root, and that settle starts the retro.
// Where retroHold still answers a hold, the issue stays held, for the reason it answers now, and
// its owner is told a hold no notice has named yet (rehold): the last issue beneath a parent can
// leave while its CI runs, a fix can end at a head whose CI has not passed, and a parent's
// acceptance can come to be held by another of its three holds. The admission pass reads, with
// nobody acting, a held parent of its project whose holds its own facts show changed or gone
// (admit's childrenWaits), because the end of a hold over a Dispatch child, a child closed, moved
// to icebox or moved out from under the parent, never released or not, and the start of one,
// change the daemon's mirror of Dispatch alone, which no settle of the engine reads.
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
func (e *Engine) StartHeldRetro(ctx context.Context, tx pgx.Tx, held record.Issue) (bool, error) {
	if !(record.HeldForChildren(held) || atTaskEnd(held)) || !record.InWorkflow(held) {
		return false, nil
	}
	root, err := e.store.Issue(ctx, tx, held.Tree)
	if err != nil || root == nil {
		return false, err
	}
	if running, err := record.TreeRunning(ctx, e.store, tx, *root, e.cfg.Steps); err != nil || !running {
		return false, err
	}
	w, err := e.walk(ctx, tx, held)
	if err != nil {
		return false, err
	}
	// A parent whose plan rows a way back dropped is held by no plan (planHold): it is a plain
	// hold, which a retry releases, and nothing starts a retro its task cannot stand at. A task's
	// end held for its CI is every run's, and a run with no plan rows has no place to check.
	if !w.Stored && record.HeldForChildren(held) {
		return false, nil
	}
	steps := w.steps()
	if w.Stored && (len(steps) == 0 || w.Place != (record.TaskPlace{Kind: record.PlaceRetro})) {
		return false, fmt.Errorf("start the held retro of %s: task %s stands at %+v of the line %v, not at its retro", held.Key, w.Task, w.Place, w.Line)
	}
	hold, err := e.retroHold(ctx, tx, held, w, phase.Retro)
	if err != nil {
		return false, err
	}
	if hold.reason != "" {
		return false, e.rehold(ctx, tx, held, w, hold)
	}
	// Nothing holds the task any longer, and no step of it runs again: the branches beside its
	// fixes go (queueTaskEnd).
	if err := e.queueTaskEnd(ctx, tx, held, w); err != nil {
		return false, err
	}
	report, err := e.retroReport(ctx, tx, held, w)
	if err != nil {
		return false, err
	}
	pr, err := e.store.PullRequestRow(ctx, tx, held.Key)
	if err != nil {
		return false, err
	}
	act := "ci_passed"
	if record.HeldForChildren(held) {
		act = "children_done"
	}
	status := PhaseStatus(phase.Retro)
	if err := e.status(ctx, tx, held, status, ""); err != nil {
		return false, err
	}
	released := held
	released.Phase, released.Hold, released.Status = phase.Retro, nil, status
	if err := e.WriteIssue(ctx, tx, released); err != nil {
		return false, err
	}
	// The release takes the issue out of held into a phase, which is a movement (the rule in
	// dispatchIssue).
	if err := e.store.MarkIssueMoved(ctx, tx, released.Key, e.now()); err != nil {
		return false, err
	}
	e.logOnCommit(ctx, "workflow: nothing holds a task before its retro any longer, and its retro starts", "issue", released.Key, "tree", released.Tree,
		"task", w.Task, "act", act, "hold", held.Hold.Reason, "from", phase.Held, "to", phase.Retro, "status", released.Status)
	// The issue now stands in its retro, so it is accepted (record.Accepted) as a pass into the
	// retro makes it, and every step of another root that waits on it as an ancestor ends its wait
	// (dispatch://INFRA-3320, transition).
	if err := e.endPendingOn(ctx, tx, released.Key, movedAccepted(released.Key, phase.Retro)); err != nil {
		return false, err
	}
	role := phase.RoleFor(phase.Retro)
	if err := e.clearHandoff(ctx, tx, released.Key, role); err != nil {
		return false, err
	}
	if err := e.start(ctx, tx, released, w, role, func(at Standing) string {
		return task(released, at, report, pr, "")
	}, phase.Held, false); err != nil {
		return false, err
	}
	// The release moves the issue into its retro as the pass into the retro would have
	// (transition), so it queues the reconcile of its tree's news as that pass does
	// (queueMovedNews). It asks nothing of the merge forward, as the hold before it asked nothing
	// (holdBeforeRetro): the retro is where the merge comes next (classify.MergeComesNext), where a
	// move asks nothing (mergeForwardOnEntry).
	return true, e.queueMovedNews(ctx, tx, released)
}

// rehold keeps held, standing before its retro, held for what holds it now (hold, retroHold). A
// parent still held for its children is told a hold on its acceptance no notice of its run has
// named yet (tellRetroHeld). Any other hold of the reason held stands for already changes nothing.
// A hold of another reason is written in its place, from retro still, and its owner is told what
// holds the task now: by the notice of the hold on its acceptance (tellRetroHeld) for its children,
// and by its retro-held notice (endHoldNotice) for its fix or its CI. So the last issue beneath a
// parent can leave while its CI has not passed, and a fix can end at a head where it has not.
func (e *Engine) rehold(ctx context.Context, tx pgx.Tx, held record.Issue, w Walk, hold endHold) error {
	if held.Hold.Reason == hold.reason {
		if hold.reason == record.HoldForChildren {
			return e.tellRetroHeld(ctx, tx, held, w, hold.held, "")
		}
		return nil
	}
	was := held.Hold.Reason
	next := *held.Hold
	next.Reason = hold.reason
	held.Hold = &next
	if err := e.WriteIssue(ctx, tx, held); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: what holds a task before its retro changed", "issue", held.Key, "task", w.Task, "was", was,
		"reason", hold.reason, "waitsFor", hold.waits, "holding", hold.held.names)
	if hold.reason == record.HoldForChildren {
		return e.tellRetroHeld(ctx, tx, held, w, hold.held, "")
	}
	pr, err := e.store.PullRequestRow(ctx, tx, held.Key)
	if err != nil {
		return err
	}
	acts, err := e.ciActsOf(ctx, tx, held, w)
	if err != nil {
		return err
	}
	return e.notice(ctx, tx, held.Key, endHoldNotice(held, w, pr, hold.reason, hold.waits, acts))
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
	return e.acceptedReport(ctx, tx, issue, w)
}
