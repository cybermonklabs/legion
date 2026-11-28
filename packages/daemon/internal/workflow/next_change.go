package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
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
	// request applied (D1). A request with no WaitsSince waits from the moment it starts the change.
	WaitsSince *time.Time
	// Request is the request for new work that starts the change (D17), nil for a change a reason
	// after a merge starts. StartNextChange writes its row (record.DefectKindRequest), the one
	// writer of that kind, and the change carries it as its reason.
	Request *Request
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
	if record.OnNextChange(issue) {
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
	// A request is a reason of its own kind, written here alone and carried by the change it
	// starts, so the checks that passed before cannot end the change before its owner takes a turn.
	if place.Request != nil {
		request := record.Defect{Issue: issue.Key, Generation: issue.Generation, Kind: record.DefectKindRequest, Text: place.Request.Text,
			Role: place.Request.Requester, ReportedAt: now}
		if request.ID, err = e.store.PutDefect(ctx, tx, request); err != nil {
			return err
		}
		reasons = append(slices.Clone(reasons), request)
		if place.WaitsSince == nil {
			place.WaitsSince = &now
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
	stored := e.waitOf(started, view, old, now)
	if err := putStoredWait(&started, stored); err != nil {
		return err
	}
	if err := e.WriteIssue(ctx, tx, started); err != nil {
		return err
	}
	if err := e.store.MarkIssueMoved(ctx, tx, started.Key, now); err != nil {
		return err
	}
	if err := e.store.ClearChangeGeneration(ctx, tx, started.Key); err != nil {
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
// where the work stands now, and, where a request is among the reasons, the instruction of D17 to
// the owner (requestInstruction). Each sentence holds at most 25 words and no semicolon.
func nextChangeReason(issue record.Issue, reasons []record.Defect) string {
	text := fmt.Sprintf("The next change of %s started as generation %d, on its own branch with its approved design kept.", issue.Key, issue.Generation)
	if named := reasonLines(reasons); len(named) > 0 {
		text += " It carries these reasons:" + bulleted(named) + "\nIt waits in the admission line for a slot."
	} else {
		text += " It waits in the admission line for a slot."
	}
	if carriesRequest(reasons) {
		text += "\n" + requestInstruction
	}
	return text
}

// carriesRequest says whether reasons hold a request for new work (record.DefectKindRequest).
func carriesRequest(reasons []record.Defect) bool {
	return slices.ContainsFunc(reasons, func(reason record.Defect) bool { return reason.Kind == record.DefectKindRequest })
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
// the count, each reason and each change's pull request (D7, changePullRequests). No count stops a
// change: the issue goes on, and the controller can move it to backlog or icebox as it can any
// root.
func (e *Engine) tellChangeCount(ctx context.Context, tx pgx.Tx, issue record.Issue, reasons []record.Defect) error {
	every := e.cfg.ReviewRoundCap
	if every <= 0 || issue.NextChanges%every != 0 {
		return nil
	}
	text := fmt.Sprintf("%s has started %d next changes of itself, and Legion stops none of them.", issue.Key, issue.NextChanges)
	if named := reasonLines(reasons); len(named) > 0 {
		text += " This change carries these reasons:" + bulleted(named) + "\n"
	} else {
		text += " "
	}
	pulls, err := e.changePullRequests(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	if len(pulls) > 0 {
		text += "Its pull requests:" + bulleted(pulls) + "\n"
	}
	text += "Move the issue to backlog or icebox where the work should stop."
	notice := record.Notice{Kind: "next-change-count", Role: claim.RoleArchitect, Reason: text}
	if err := e.notice(ctx, tx, issue.Key, notice); err != nil {
		return err
	}
	return e.enqueue(ctx, tx, issue.Key, record.ControllerNotice(record.Notice{Kind: "next-change-count", Reason: text}))
}

// changePullRequests is each pull request of key's generations, as #<number> in number order,
// each once: every one a new generation's clear kept a clock of (cleared_pull_requests), the one
// its pull_requests row holds, and the merge snapshot's, which the evidence projection reads
// together (record.Store.MergeHistoryOf). A change that ended with no pull request names none.
func (e *Engine) changePullRequests(ctx context.Context, tx pgx.Tx, key string) ([]string, error) {
	history, err := e.store.MergeHistoryOf(ctx, tx, key)
	if err != nil {
		return nil, err
	}
	evidence := history.Evidence
	numbers := make([]int, 0, len(evidence.Clocks)+2)
	for _, clock := range evidence.Clocks {
		numbers = append(numbers, clock.Number)
	}
	if evidence.PullRequest != nil {
		numbers = append(numbers, evidence.PullRequest.Number)
	}
	numbers = append(numbers, evidence.Merge.Number)
	slices.Sort(numbers)
	numbers = slices.Compact(numbers)
	pulls := make([]string, 0, len(numbers))
	for _, number := range numbers {
		if number > 0 {
			pulls = append(pulls, fmt.Sprintf("#%d", number))
		}
	}
	return pulls, nil
}

// changeReasons is the reasons a next change of issue would carry now: its open defects whose carry
// no longer holds (record.CarriedNow: an uncarried one, or a reported defect or a request an earlier
// generation carried and never merged, as a re-admission leaves it; a carried check's or production
// check's reason was answered by its change), with each check's reason left out where that check
// passes in the read the judge holds. A check that passes holds nothing, and a reason a change
// already carries joins that change rather than starting another (D7).
func changeReasons(issue record.Issue, view closeView) []record.Defect {
	var reasons []record.Defect
	for _, defect := range view.facts.Defects {
		if record.CarriedNow(defect, issue) || !defect.Open() {
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

// waitsInRead says whether the read the judge holds lists check among the checks the close still
// waits for, in any state but a pass: a fail, no result, or unknown (record.CloseWaitOf lists every
// check that does not pass). A check's reason of a next change resolves only once it is not listed.
func waitsInRead(view closeView, check string) bool {
	for _, waiting := range view.wait.Checks {
		if waiting.Check == check {
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
			// A check that is already a reason adds none (checkReasonStands). A fail after a reason
			// that a change answered is the check failing again, which is a reason of its own.
			if defect.Check == failed.Check && defect.Open() && checkReasonStands(defect, issue) {
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

// checkReasonStands says whether defect, a check's reason of issue, is still a reason: one waiting
// for a change to start (carried by none, record.CarriedNow) or one the issue's open next change
// carries. A reason that a change carried which has ended, or that a re-admission ended, holds
// nothing: that change answered it. It is the one rule of "the check is already a reason", which a
// new fail of the check (recordCheckReasons) and a report naming the check (defectReport) both ask.
func checkReasonStands(defect record.Defect, issue record.Issue) bool {
	return !record.CarriedNow(defect, issue) || record.OnNextChange(issue) && defect.CarriedBy == issue.NextChange
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
// merged, whose fallback is MergerCompletionBound after the merge. The merge's moment is GitHub's
// merged_at, or the entry into verifying where the snapshot holds none (an older listener's
// merge fact), which is the moment the daemon took the merge (mergedEntry): counted from the
// zero time, the bound would have passed already. It answers the moment the fallback comes due
// where the completion is what holds the change, so the judge reads the checks again then.
func (e *Engine) changeDue(issue record.Issue, view closeView) (time.Time, bool) {
	if view.wait.Unread {
		return time.Time{}, false
	}
	if !issue.MergerCompletionOwed {
		return time.Time{}, true
	}
	merged := issue.Merge.MergedAt
	if merged.IsZero() {
		merged = view.facts.Entered
	}
	fallback := merged.UTC().Add(MergerCompletionBound)
	if !e.now().Before(fallback) {
		return time.Time{}, true
	}
	return fallback, false
}

// changeOpenedNoPullRequest says whether the issue's open change has opened no pull request of
// its own: the record holds none, or the one it holds is the merged pull request the change
// follows (the merge snapshot's, Issue.Merge). The change's start deletes every row that is not
// open (record.Store.ClearChangeGeneration), so a row the record holds after it is one the
// change's branch opened, in whatever state. record.PullRequest.Generation is not read: it is the
// CI settlement's generation, which names no generation of the issue.
func (e *Engine) changeOpenedNoPullRequest(ctx context.Context, tx pgx.Tx, issue record.Issue) (bool, error) {
	pr, err := e.store.PullRequestRow(ctx, tx, issue.Key)
	if err != nil || pr == nil {
		return true, err
	}
	return pr.State == record.PullRequestMerged && pr.Number == issue.Merge.Number, nil
}

// changeResolved says whether each reason the issue's open change carries is resolved (D7, B08):
// a check's reason by that check passing in a fresh read of the generation (waitsInRead: a check
// with no result or an unknown answer has not passed, so it resolves nothing), a reported defect
// or a request by a withdrawal that ended it, and a production reason by the check this end opens.
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
			if waitsInRead(view, defect.Check) {
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
func (e *Engine) endChange(ctx context.Context, tx pgx.Tx, issue record.Issue, withdrawn []record.WaitWithdrawal) error {
	now := e.now()
	ended := issue
	ended.NextChange = 0
	// The change's held start goes with it, whichever phase held it: the hold, a wait on a person,
	// and a start held in awaiting_children (ChildrenWait), which issues_children_wait_shape refuses
	// outside that phase, as StartNextChange clears them at the change's start.
	ended.Phase, ended.Hold, ended.Awaiting, ended.ChildrenWait = phase.Verifying, nil, nil, nil
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
	stored.Withdrawn = append(stored.Withdrawn, withdrawn...)
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

// mergedEntry is issue as its merge leaves it entering verifying: the change it merged ended
// (changeMerged), at GitHub's merged_at where the snapshot holds it and at now otherwise, and
// whether the merger's completion of the generation that merged is still owed
// (Issue.MergerCompletionOwed): the merger was started on its step (an entry's task stands on its
// phases row) and its completion has not landed. Both entries into verifying from a merge take
// it, the table's (enterVerifying) and a lingering tree's (closeMerged).
func (e *Engine) mergedEntry(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest, now time.Time) (record.Issue, *record.WaitChangeEnd, error) {
	at := issue.Merge.MergedAt
	if at.IsZero() {
		at = now
	}
	issue, ended, err := e.changeMerged(ctx, tx, issue, pr, at)
	if err != nil {
		return issue, nil, err
	}
	merger, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleMerger)
	if err != nil {
		return issue, nil, err
	}
	issue.MergerCompletionOwed = merger.EntryTask != "" && !merger.Completed()
	return issue, ended, nil
}

// startChangeIfDue is the judge's start of a next change of issue, which waits in verifying with
// view, its close's reading, holding something: each check Dispatch now computes as a fail is
// recorded as a reason (recordCheckReasons), and where a reason waits that no change carries and
// the change's moment has come (changeDue), the change starts in place (StartNextChange). Where
// the merger's completion is what holds the change, the checks are read again when the hour after
// the merge falls due, so the change starts then with no other event. In a tree that lingers the
// reason is recorded and no change starts: it waits until the tree runs again (D10). An issue
// whose change is already open is refused by StartNextChange's one guard, which this function
// does not repeat: a rule written twice has no control that can fail. It answers whether a
// change started.
func (e *Engine) startChangeIfDue(ctx context.Context, tx pgx.Tx, key string) (bool, error) {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil || issue.Phase != phase.Verifying {
		return false, err
	}
	view, err := e.closeWait(ctx, tx, *issue)
	if err != nil {
		return false, err
	}
	if err := e.recordCheckReasons(ctx, tx, *issue, view); err != nil {
		return false, err
	}
	if view, err = e.closeWait(ctx, tx, *issue); err != nil {
		return false, err
	}
	reasons := changeReasons(*issue, view)
	if len(reasons) == 0 {
		return false, nil
	}
	at, due := e.changeDue(*issue, view)
	if !due {
		if !at.IsZero() {
			return false, e.askCloseReadAt(ctx, tx, *issue, at)
		}
		return false, nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return false, err
	}
	return true, e.StartNextChange(ctx, tx, *issue, reasons, ChangeInPlace(*issue))
}

// judgeOpenChange is the judge of an issue whose next change is open (Issue.NextChange), which
// stands in a phase of that change and not in verifying: a read of its checks, a withdrawal and a
// result's event reach it. A check now failed is recorded as a reason (recordCheckReasons) and
// joins the change where the change has not passed its last step, so the change's work takes it;
// one that comes later waits for the change after it. Where the change opened no pull request of
// its own and each reason it carries is resolved, the change ends (endChange). Otherwise the
// stored wait is written again, with the withdrawals a version has still to say (waitOf carries
// them, and this one's are appended), and close-wait.md's row is queued where it changed.
func (e *Engine) judgeOpenChange(ctx context.Context, tx pgx.Tx, issue record.Issue, withdrawn []record.WaitWithdrawal) error {
	view, err := e.closeWait(ctx, tx, issue)
	if err != nil {
		return err
	}
	if err := e.recordCheckReasons(ctx, tx, issue, view); err != nil {
		return err
	}
	if !record.AcceptedPhase(record.StandsAt(issue)) {
		if joined := changeReasons(issue, view); len(joined) > 0 {
			if err := e.joinReasons(ctx, tx, issue, joined); err != nil {
				return err
			}
		}
	}
	if view, err = e.closeWait(ctx, tx, issue); err != nil {
		return err
	}
	none, err := e.changeOpenedNoPullRequest(ctx, tx, issue)
	if err != nil {
		return err
	}
	if none && changeResolved(issue, view) {
		return e.endChange(ctx, tx, issue, withdrawn)
	}
	old, err := storedWaitOf(issue)
	if err != nil {
		return err
	}
	stored := e.waitOf(issue, view, old, e.now())
	stored.Withdrawn = append(stored.Withdrawn, withdrawn...)
	if err := e.tellAtOnce(ctx, tx, issue, old, stored, view.wait); err != nil {
		return err
	}
	before, err := json.Marshal(old)
	if err != nil {
		return err
	}
	after, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	if string(before) == string(after) {
		return nil
	}
	issue.CloseWait = after
	if err := e.WriteIssue(ctx, tx, issue); err != nil {
		return err
	}
	return e.publishWait(ctx, tx, issue)
}

// joinReasons is reasons joining the issue's open change, which has not passed its last step
// (D7): each is carried by the change's generation, so the change's work takes it and its merge
// ends it, and the owning architect is told, naming each.
func (e *Engine) joinReasons(ctx context.Context, tx pgx.Tx, issue record.Issue, reasons []record.Defect) error {
	ids := make([]int64, 0, len(reasons))
	for _, reason := range reasons {
		ids = append(ids, reason.ID)
	}
	if err := e.store.CarryDefects(ctx, tx, issue.Key, issue.NextChange, ids); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a reason joins the open next change", "issue", issue.Key, "generation", issue.NextChange, "reasons", ids)
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: "next-change", Role: claim.RoleArchitect, Reason: fmt.Sprintf(
		"These reasons joined the open next change %d of %s, which carries them:%s\nIts later steps take them.",
		issue.NextChange, issue.Key, bulleted(reasonLines(reasons)))})
}

// mergerCompleted is the merger's completion of the generation that merged, landing while its
// issue waits in verifying (leftItsPhase took it): the record no longer owes it
// (Issue.MergerCompletionOwed), so the merger reports no defect any more (mayReportDefect), and
// the close is judged again, since a next change waits for this completion (changeDue).
func (e *Engine) mergerCompleted(ctx context.Context, tx pgx.Tx, key string) error {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil || !issue.MergerCompletionOwed {
		return err
	}
	issue.MergerCompletionOwed = false
	if err := e.WriteIssue(ctx, tx, *issue); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: the merger's completion of the merged generation landed in verifying", "issue", issue.Key,
		"generation", issue.Generation)
	return e.judgeClose(ctx, tx, *issue)
}

// carryReturn is the carry of a return (D7): a role returns its task for a cause that stays, out
// of its retro or its merge, and each open defect reported in this generation that no change
// carries yet is carried by this generation, so the returned work takes it and its merge starts no
// second change for it. It answers the defects it carried, which the start task of the phase the
// return reaches names.
func (e *Engine) carryReturn(ctx context.Context, tx pgx.Tx, issue record.Issue) ([]record.Defect, error) {
	open, err := e.store.OpenDefects(ctx, tx, issue.Key)
	if err != nil {
		return nil, err
	}
	var carried []record.Defect
	var ids []int64
	for _, defect := range open {
		if record.CarriedNow(defect, issue) || defect.Generation != issue.Generation || defect.Kind != record.DefectKindDefect {
			continue
		}
		carried, ids = append(carried, defect), append(ids, defect.ID)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if err := e.store.CarryDefects(ctx, tx, issue.Key, issue.Generation, ids); err != nil {
		return nil, err
	}
	e.logOnCommit(ctx, "workflow: a return carries the defects of its generation", "issue", issue.Key, "generation", issue.Generation, "defects", ids)
	return carried, nil
}

// withCarried is reason, the move's own words for the start task, followed by what that task says
// of the defects the return carried (carryReturn): each by id with what it says, which this
// generation's work takes. The words reach the task alone: the move posts reason on the issue as it
// was (moveReason), so no role's defect text is posted there.
func withCarried(reason string, carried []record.Defect) string {
	if len(carried) == 0 {
		return reason
	}
	named := "This return carries these defects of the change, which your work takes:" + bulleted(reasonLines(carried))
	if reason == "" {
		return named
	}
	return sentence(reason) + " " + named
}

// carriedByChange is each open reason the next change of issue carries (Defect.CarriedBy is the
// change's generation), of every kind, where trigger is the gate's opening of that change: its first
// start task names them (withChangeReasons). Every other move names none, so it reads nothing.
func (e *Engine) carriedByChange(ctx context.Context, tx pgx.Tx, trigger TriggerKind, issue record.Issue) ([]record.Defect, error) {
	if trigger != TriggerGateOpened || !record.OnNextChange(issue) {
		return nil, nil
	}
	open, err := e.store.OpenDefects(ctx, tx, issue.Key)
	if err != nil {
		return nil, err
	}
	var carried []record.Defect
	for _, reason := range open {
		if reason.CarriedBy == issue.Generation {
			carried = append(carried, reason)
		}
	}
	return carried, nil
}

// withChangeReasons is reason, the move's own words for the start task, followed by what the first
// start task of a next change says of the reasons the change carries (carriedByChange): each by id,
// kind and what it says, and a check's by its block id, which the change's work takes (D7).
func withChangeReasons(reason string, carried []record.Defect) string {
	if len(carried) == 0 {
		return reason
	}
	named := "This next change carries these reasons, which its work takes:" + bulleted(reasonLines(carried))
	if reason == "" {
		return named
	}
	return sentence(reason) + " " + named
}

// firstStartAt is where the working copy of issue's first start of a next change starts
// (record.StartAt): the issue's branch, legion/<KEY>, and the base its last pull request merged
// into, or its recorded default branch where the snapshot names none. Only the gate's opening of a
// generation that is a next change (record.OnNextChange) names one; every other move names none.
func firstStartAt(trigger TriggerKind, issue record.Issue) *record.StartAt {
	if trigger != TriggerGateOpened || !record.OnNextChange(issue) {
		return nil
	}
	base := issue.Merge.Base
	if base == "" {
		base = issue.DefaultBranch
	}
	if base == "" {
		return nil
	}
	return &record.StartAt{Bookmark: claim.IssueBranch(issue.Key), Base: base}
}
