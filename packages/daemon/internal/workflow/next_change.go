package workflow

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// MergerCompletionBound is how long a next change waits for the merger's completion of the
// generation that merged before it starts without it (D7): the merger completes in the turn that
// merged, so the bound is the fallback for a merger that never completes, and no change waits on
// a worker that has gone for longer than an hour.
const MergerCompletionBound = time.Hour

// ChangePlace is where a next change of an issue stands when it starts (StartNextChange): the
// tree it runs in, the parent it hangs under there, and since when its close waits. A reason
// found after a merge starts the change where the issue already stands (ChangeInPlace); a
// request for new work on a closed issue names the placement its own door decided (D17).
type ChangePlace struct {
	// Tree is the tree the change runs in and Parent the issue's parent in it, nil for none.
	Tree   string
	Parent *string
	// WaitsSince is when the issue's close waits from (Issue.CloseWaitSince): nil carries the
	// moment the merge set, which every reason after a merge does, and a time is the moment a
	// request applied (D1).
	WaitsSince *time.Time
}

// ChangeInPlace is the placement of a change that starts where the issue already stands: its own
// tree and parent, carrying the wait its merge began.
func ChangeInPlace(issue record.Issue) ChangePlace {
	return ChangePlace{Tree: issue.Tree, Parent: issue.Parent}
}

// StartNextChange is the one transition of new work on an issue whose work merged (D7): the next
// generation of that issue, at admitted with its gate kept, carrying reasons. Its callers are the
// judge of the close, for a reason found after the merge, and, from Task 13, each entry door of a
// request for new work, each of which decides place first.
//
// In order: an open production check of the issue is closed as not run, with no claim retirement
// and no IssueSuspend, since the change's own roles work in that pod; the reasons are marked as
// carried by the new generation, so the merge of this change ends them and no later reason starts
// a second change for them; the record takes its next generation in place, at admitted with the
// status todo, the merge snapshot, the mark of the close on checks and the count of changes
// carried; the generation's facts are cleared with the gate kept (record.Store.ClearChangeGeneration),
// so the change asks no person to approve the design again; and the owning architect is told.
//
// It raises the issue's own generation and never its tree's, so a root's next change keeps each
// member's queued idle release, its suspension and its design-news credits, and leaves the tree's
// architect's run standing. It touches no other issue's record, phase row, pull request, gate or
// claim.
//
// The one hard guard, which is no count: a next change never starts while another one of the same
// issue is open (Issue.NextChange).
func (e *Engine) StartNextChange(ctx context.Context, tx pgx.Tx, issue record.Issue, reasons []record.Defect, place ChangePlace) error {
	if issue.NextChange != 0 {
		e.logOnCommit(ctx, "workflow: no next change starts while one of the issue is open", "issue", issue.Key,
			"generation", issue.Generation, "open", issue.NextChange)
		return nil
	}
	now := e.now()
	next := issue.Generation + 1
	check, err := e.store.ProductionCheck(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	if check != nil && check.Open() {
		if err := e.markCheckClosed(ctx, tx, check, record.CheckNotRun); err != nil {
			return err
		}
	}
	ids := make([]int64, 0, len(reasons))
	for _, reason := range reasons {
		ids = append(ids, reason.ID)
	}
	if err := e.store.CarryDefects(ctx, tx, issue.Key, next, ids); err != nil {
		return err
	}
	// The status write is read against the status the record holds now, before the field below
	// moves it: Dispatch's echo of that write then carries the status the record holds, so it
	// re-enters nothing (dispatchIssue).
	if err := e.status(ctx, tx, issue, "todo", ""); err != nil {
		return err
	}
	started := issue
	started.Generation, started.NextChange = next, next
	started.NextChanges++
	started.Phase, started.Hold, started.Awaiting, started.ChildrenWait = phase.Admitted, nil, nil, nil
	started.Status = "todo"
	started.MergerCompletionOwed = false
	started.StandingToldAt = nil
	if place.Tree != "" {
		started.Tree, started.Parent = place.Tree, place.Parent
	}
	if place.WaitsSince != nil {
		since := place.WaitsSince.UTC()
		started.CloseWaitSince = &since
	}
	// The stored wait names the change and each reason it carries, which close-wait.md's next
	// version says, so the reasons are carried above before it is read.
	view, err := e.closeWait(ctx, tx, started)
	if err != nil {
		return err
	}
	old, err := storedWaitOf(issue)
	if err != nil {
		return err
	}
	if err := putStoredWait(&started, e.waitOf(started, view, old, now)); err != nil {
		return err
	}
	if err := e.WriteIssue(ctx, tx, started); err != nil {
		return err
	}
	if err := e.store.MarkIssueMoved(ctx, tx, started.Key, now); err != nil {
		return err
	}
	if err := e.store.ClearChangeGeneration(ctx, tx, started.Key, now); err != nil {
		return err
	}
	if err := e.publishWait(ctx, tx, started); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: the next change of an issue starts", "issue", started.Key, "tree", started.Tree,
		"generation", next, "changes", started.NextChanges, "reasons", ids)
	if err := e.notice(ctx, tx, started.Key, record.Notice{Kind: "next-change", Role: claim.RoleArchitect,
		Reason: nextChangeReason(started, reasons)}); err != nil {
		return err
	}
	return e.tellChangeCount(ctx, tx, started, reasons)
}

// nextChangeReason is the next-change notice: the change's generation, each reason it carries and
// where the work stands now. Each sentence holds at most 25 words and no semicolon.
func nextChangeReason(issue record.Issue, reasons []record.Defect) string {
	text := fmt.Sprintf("The next change of %s started as generation %d, on its own branch with its approved design kept.", issue.Key, issue.Generation)
	if named := reasonLines(reasons); len(named) > 0 {
		return text + " It carries these reasons:" + bulleted(named) + "\nIt waits in the admission line for a slot."
	}
	return text + " It waits in the admission line for a slot."
}

// reasonLines is one line for each reason of a change: its id, its kind and what it says.
func reasonLines(reasons []record.Defect) []string {
	var named []string
	for _, reason := range reasons {
		text := reason.Text
		if reason.Check != "" {
			text = "check " + reason.Check + ": " + text
		}
		named = append(named, fmt.Sprintf("%d (%s) %s", reason.ID, reason.Kind, text))
	}
	return named
}

// tellChangeCount tells the owning architect and the project's controller once at each multiple
// of the deployment's review round cap (Config.ReviewRoundCap) next changes of one issue, naming
// the count, each reason and the issue's pull requests (D7). No count stops a change: the issue
// goes on, and the controller can move it to backlog or icebox as it can any root.
func (e *Engine) tellChangeCount(ctx context.Context, tx pgx.Tx, issue record.Issue, reasons []record.Defect) error {
	cap := e.cfg.ReviewRoundCap
	if cap <= 0 || issue.NextChanges%cap != 0 {
		return nil
	}
	text := fmt.Sprintf("%s has started %d next changes of itself, and Legion stops none of them.", issue.Key, issue.NextChanges)
	if named := reasonLines(reasons); len(named) > 0 {
		text += " This change carries these reasons:" + bulleted(named) + "\n"
	} else {
		text += " "
	}
	if issue.Merge.Number != 0 {
		text += fmt.Sprintf("Its newest merged pull request is #%d. ", issue.Merge.Number)
	}
	text += "Move the issue to backlog or icebox where the work should stop."
	notice := record.Notice{Kind: "next-change-count", Role: claim.RoleArchitect, Reason: text}
	if err := e.notice(ctx, tx, issue.Key, notice); err != nil {
		return err
	}
	return e.enqueue(ctx, tx, issue.Key, record.ControllerNotice(record.Notice{Kind: "next-change-count", Reason: text}))
}

// changeReasons is the reasons a next change of issue would carry now: its open defects that no
// change carries yet (record.Store.OpenDefects, CarriedBy 0), with each check's reason left out
// where that check passes in the read the judge holds. A check that passes holds nothing, and a
// reason a change already carries joins that change rather than starting another (D7).
func changeReasons(view closeView) []record.Defect {
	var reasons []record.Defect
	for _, defect := range view.facts.Defects {
		if defect.CarriedBy != 0 || !defect.Open() {
			continue
		}
		if defect.Kind == record.DefectKindCheck && !failedInRead(view, defect.Check) {
			continue
		}
		reasons = append(reasons, defect)
	}
	return reasons
}

// failedInRead says whether the read the judge holds answers check as a fail.
func failedInRead(view closeView, check string) bool {
	for _, waiting := range view.wait.Checks {
		if waiting.Check == check && waiting.State == record.CheckStateFail {
			return true
		}
	}
	return false
}

// recordCheckReasons records a reason of the next change for each check the read answers as a
// fail that the issue holds no reason of yet (D7): its kind is check, it names the check and the
// deciding result Dispatch gave, and its head is the commit that result was recorded at. A check
// that is already a reason adds none, whether the reason waits or an open change carries it, and
// a fail whose deciding result a reason already names adds none either, so a merged change's old
// fail starts no change after it.
func (e *Engine) recordCheckReasons(ctx context.Context, tx pgx.Tx, issue record.Issue, view closeView) error {
	if view.wait.Unread {
		return nil
	}
	held, err := e.store.Defects(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	for _, failed := range view.wait.Checks {
		if failed.State != record.CheckStateFail {
			continue
		}
		known := false
		for _, defect := range held {
			if defect.Kind != record.DefectKindCheck {
				continue
			}
			if defect.Result != "" && defect.Result == failed.Result {
				known = true
				break
			}
			if defect.Check == failed.Check && defect.Open() {
				known = true
				break
			}
		}
		if known {
			continue
		}
		id, err := e.store.PutDefect(ctx, tx, record.Defect{Issue: issue.Key, Generation: issue.Generation, Kind: record.DefectKindCheck,
			Text: checkFailedText(failed), Check: failed.Check, Result: failed.Result, Head: failed.Commit, ReportedAt: e.now()})
		if err != nil {
			return err
		}
		e.logOnCommit(ctx, "workflow: a check Dispatch computes as failed is a reason of the next change", "issue", issue.Key,
			"generation", issue.Generation, "check", failed.Check, "result", failed.Result, "defect", id)
	}
	return nil
}

// checkFailedText is what a check's reason says: the check Dispatch computed as failed and the
// result that decided it.
func checkFailedText(failed record.CheckWait) string {
	text := "Dispatch computes check " + failed.Check + " as failed after the merge."
	if failed.Result != "" {
		text += " Its deciding result is " + failed.Result + "."
	}
	return text
}

// changeDue says whether the next change of issue may start now (D7): never before the merge of
// the reviewed work, so the change starts at the later of the first read of the checks that
// counts after the entry into verifying and the merger's completion of the generation that
// merged, whose fallback is MergerCompletionBound after the merge. It answers the moment the
// fallback comes due where the completion is what holds the change, so the judge reads the checks
// again then.
func (e *Engine) changeDue(issue record.Issue, view closeView) (time.Time, bool) {
	if view.wait.Unread {
		return time.Time{}, false
	}
	if !issue.MergerCompletionOwed {
		return time.Time{}, true
	}
	fallback := issue.Merge.MergedAt.UTC().Add(MergerCompletionBound)
	if !e.now().Before(fallback) {
		return time.Time{}, true
	}
	return fallback, false
}

// changeOpenedNoPullRequest says whether the issue's open change has opened no pull request of
// its own generation: the record holds none, or the one it holds is of a generation before the
// change (record.PullRequest.Generation).
func (e *Engine) changeOpenedNoPullRequest(ctx context.Context, tx pgx.Tx, issue record.Issue) (bool, error) {
	pr, err := e.store.PullRequestRow(ctx, tx, issue.Key)
	if err != nil || pr == nil {
		return true, err
	}
	return pr.Generation < issue.NextChange, nil
}

// changeResolved says whether each reason the issue's open change carries is resolved (D7, B08):
// a check's reason by that check passing in a fresh read of the generation, a reported defect or
// a request by a withdrawal that ended it, and a production reason by the check this end opens.
// The other terms of the close are not asked: the open change, the superseded check and the check
// not yet opened hold nothing here.
func changeResolved(issue record.Issue, view closeView) bool {
	if view.wait.Unread {
		return false
	}
	for _, defect := range view.facts.Defects {
		if defect.CarriedBy != issue.NextChange || !defect.Open() {
			continue
		}
		switch defect.Kind {
		case record.DefectKindProduction:
		case record.DefectKindCheck:
			if failedInRead(view, defect.Check) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// endChange ends the issue's open next change with no pull request, once each reason it carries
// is resolved (D7, B08): issues.next_change is cleared, the issue stands in verifying again with
// a new entry in its stored wait, which says that the change ended and how, a read of the checks
// is queued, and a production check of this generation opens at the retained merge where the
// project runs one and nothing beneath it is in the workflow. A start row of the change's phases
// is stale from then on, since the phase changed.
func (e *Engine) endChange(ctx context.Context, tx pgx.Tx, issue record.Issue, view closeView) error {
	now := e.now()
	ended := issue
	ended.NextChange = 0
	ended.Phase, ended.Hold = phase.Verifying, nil
	w, err := e.walk(ctx, tx, ended)
	if err != nil {
		return err
	}
	if err := e.moveTask(ctx, tx, &ended, &w, phase.Verifying, record.TaskDone); err != nil {
		return err
	}
	ended.Phase = phase.Verifying
	if err := e.status(ctx, tx, issue, PhaseStatus(phase.Verifying), ""); err != nil {
		return err
	}
	if status := PhaseStatus(phase.Verifying); status != "" {
		ended.Status = status
	}
	after, err := e.closeWaitFrom(ctx, tx, ended, now)
	if err != nil {
		return err
	}
	old, err := storedWaitOf(issue)
	if err != nil {
		return err
	}
	stored := e.waitOf(ended, after, old, now)
	stored.Entered = now
	stored.Ended = &record.WaitChangeEnd{Generation: issue.NextChange, At: now}
	if err := putStoredWait(&ended, stored); err != nil {
		return err
	}
	if err := e.WriteIssue(ctx, tx, ended); err != nil {
		return err
	}
	if err := e.store.MarkIssueMoved(ctx, tx, ended.Key, now); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a next change that opened no pull request ended", "issue", ended.Key, "generation", issue.NextChange,
		"mergeCommit", ended.Merge.SHA)
	if err := e.waitEntered(ctx, tx, ended, after, nil); err != nil {
		return err
	}
	if len(after.wait.Beneath) > 0 || e.checkRole() == "" {
		return nil
	}
	lingers, err := record.TreeLingers(ctx, e.store, tx, ended.Tree)
	if err != nil {
		return err
	}
	return e.openCheck(ctx, tx, ended, ended.Merge, lingers)
}

// changeMerged ends the issue's open next change at its merge: issues.next_change is cleared and
// each reason that change carries is marked merged (record.Store.MergeDefects), so a reason a
// merged change carried holds the close no more and starts no change after it. It answers the
// issue as the end left it and what the stored wait says of that end. A merge of a generation
// that is no open change of the issue ends nothing.
func (e *Engine) changeMerged(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest, at time.Time) (record.Issue, *record.WaitChangeEnd, error) {
	carried := issue.Generation
	if issue.NextChange != carried {
		// The generation that merged is no open change of the issue, so the reasons it carries
		// are the ones a return carried into it (D7), which this merge ends all the same.
		if err := e.store.MergeDefects(ctx, tx, issue.Key, carried, at); err != nil {
			return issue, nil, err
		}
		return issue, nil, nil
	}
	if err := e.store.MergeDefects(ctx, tx, issue.Key, carried, at); err != nil {
		return issue, nil, err
	}
	issue.NextChange = 0
	number := 0
	if pr != nil {
		number = pr.Number
	}
	e.logOnCommit(ctx, "workflow: a next change merged its pull request", "issue", issue.Key, "generation", carried, "pullRequest", number)
	return issue, &record.WaitChangeEnd{Generation: carried, PullRequest: number, At: at}, nil
}
