package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// closeView is one reading of what a merged issue's close waits for: the facts the predicate
// judged (record.CloseFacts), its answer (record.CloseWaitOf), the newest read's answer where it
// decodes, and the issue's tree as the reading found it, which the stored wait names the items
// beneath by.
type closeView struct {
	facts   record.CloseFacts
	wait    record.CloseWait
	answer  *record.CheckReadAnswer
	members []record.Issue
}

// closeWait is the one reader of the predicate's inputs: it reads each of them for issue in tx and
// judges them (record.CloseWaitOf), counting a read only after the issue's newest entry into
// verifying, which the stored wait keeps (record.StoredWait.Entered).
func (e *Engine) closeWait(ctx context.Context, tx pgx.Tx, issue record.Issue) (closeView, error) {
	stored, err := storedWaitOf(issue)
	if err != nil {
		return closeView{}, err
	}
	return e.closeWaitFrom(ctx, tx, issue, stored.Entered)
}

// closeWaitFrom is closeWait with entered as the issue's newest entry into verifying: the moment
// of a merge that enters it now, at which no read made before counts. The tree is read as the
// store holds it, with issue in place of the issue's own row: a merge that enters verifying reads
// the issues beneath it with the issue already there, so a child the daemon held at admitted under
// it in merging counts as beneath it once it is no longer accepted (record.Accepted).
func (e *Engine) closeWaitFrom(ctx context.Context, tx pgx.Tx, issue record.Issue, entered time.Time) (closeView, error) {
	members, err := e.treeMembers(ctx, tx, issue)
	if err != nil {
		return closeView{}, err
	}
	for i := range members {
		if members[i].Key == issue.Key {
			members[i] = issue
		}
	}
	read, err := e.store.NewestCheckRead(ctx, tx, issue.Key)
	if err != nil {
		return closeView{}, err
	}
	check, err := e.store.ProductionCheck(ctx, tx, issue.Key)
	if err != nil {
		return closeView{}, err
	}
	defects, err := e.store.OpenDefects(ctx, tx, issue.Key)
	if err != nil {
		return closeView{}, err
	}
	facts := record.CloseFacts{
		Repo: issue.Repo, Merge: issue.Merge, Entered: entered, Beneath: record.OpenBeneath(issue, members), Read: read,
		Generation: issue.Generation, Production: check, ProductionOn: e.checkRole() != "", Defects: defects,
		OpenChange: issue.NextChange != 0,
	}
	view := closeView{facts: facts, members: members}
	if read != nil && read.Failure == "" && len(read.Answer) > 0 {
		var answer record.CheckReadAnswer
		if err := json.Unmarshal(read.Answer, &answer); err == nil {
			view.answer = &answer
			if view.facts.OpenChildren, err = e.openChildren(ctx, tx, answer.Children, members); err != nil {
				return closeView{}, err
			}
		}
	}
	view.wait = record.CloseWaitOf(view.facts)
	return view, nil
}

// openChildren is each child of the read's Dispatch issue that the children-first rule reads as
// open (record.ChildOpen) and that the record does not hold in the workflow: a child the record
// holds in the workflow is an issue beneath, whose own record judges it. So no close comes past a
// child Dispatch holds open, whatever the daemon's copy of Dispatch holds.
func (e *Engine) openChildren(ctx context.Context, tx pgx.Tx, children []dispatch.IssueChild, members []record.Issue) ([]string, error) {
	byKey := make(map[string]record.Issue, len(members))
	for _, member := range members {
		byKey[member.Key] = member
	}
	var open []string
	for _, child := range children {
		var recorded *record.Issue
		if member, ok := byKey[child.Key]; ok {
			recorded = &member
		} else {
			read, err := e.store.Issue(ctx, tx, child.Key)
			if err != nil {
				return nil, err
			}
			recorded = read
		}
		if !record.ChildOpen(record.DispatchIssueRow{Key: child.Key, Status: child.Status}, recorded) {
			continue
		}
		if recorded != nil && record.InWorkflow(*recorded) {
			continue
		}
		open = append(open, child.Key)
	}
	slices.Sort(open)
	return open, nil
}

// storedWaitOf is the stored wait the issue's row holds (issues.close_wait), the zero wait where
// none was stored.
func storedWaitOf(issue record.Issue) (record.StoredWait, error) {
	var stored record.StoredWait
	if len(issue.CloseWait) == 0 {
		return stored, nil
	}
	if err := json.Unmarshal(issue.CloseWait, &stored); err != nil {
		return record.StoredWait{}, fmt.Errorf("read the stored wait of %s: %w", issue.Key, err)
	}
	return stored, nil
}

// putStoredWait sets the issue's stored wait to stored, as the row's JSON.
func putStoredWait(issue *record.Issue, stored record.StoredWait) error {
	encoded, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("store the wait of %s: %w", issue.Key, err)
	}
	issue.CloseWait = encoded
	return nil
}

// waitOf is the stored wait of view: each item the close waits for, with since when it stands.
// An item that stood in old in the same state keeps its since. Every other item stands from now.
// Entered is old's: only an entry into verifying sets it (enterVerifying, endChange). Ended and
// Closed are set by the change's end and the close alone, so each is said in one version.
func (e *Engine) waitOf(issue record.Issue, view closeView, old record.StoredWait, now time.Time) record.StoredWait {
	wait := view.wait
	stored := record.StoredWait{Entered: old.Entered}
	blocks := map[string]dispatch.CheckBlock{}
	if view.answer != nil {
		for _, block := range view.answer.Results.Checks {
			blocks[block.BlockID] = block
		}
	}
	for _, check := range wait.Checks {
		item := record.WaitCheck{ID: check.Check, When: blocks[check.Check].When, Answer: check.Answer, Reason: check.Reason, Since: now}
		for _, was := range old.Checks {
			if was.ID == item.ID && was.Answer == item.Answer && was.Reason == item.Reason {
				item.Since = was.Since
			}
		}
		stored.Checks = append(stored.Checks, item)
	}
	if wait.Production != "" {
		item := record.WaitProduction{State: wait.Production, Generation: issue.Generation, Merge: issue.Merge.SHA, Since: now}
		if check := view.facts.Production; check != nil && check.Generation == issue.Generation && check.Generation != 0 {
			item.Merge = check.MergeSHA
			if check.Open() {
				item.NextLookAt = check.WakeAt
			} else {
				item.Outcome = string(check.Outcome)
			}
		}
		if was := old.Production; was != nil && was.State == item.State && was.Generation == item.Generation && was.Merge == item.Merge {
			item.Since = was.Since
		}
		stored.Production = &item
	}
	statuses := map[string]string{}
	for _, member := range view.members {
		statuses[member.Key] = member.Status
	}
	stored.Beneath = waitIssues(wait.Beneath, statuses, old.Beneath, now)
	children := map[string]string{}
	if view.answer != nil {
		for _, child := range view.answer.Children {
			children[child.Key] = child.Status
		}
	}
	stored.OpenChildren = waitIssues(wait.OpenChildren, children, old.OpenChildren, now)
	for _, id := range wait.Defects {
		for _, defect := range view.facts.Defects {
			if strconv.FormatInt(defect.ID, 10) == id {
				stored.Defects = append(stored.Defects, waitReason(defect))
			}
		}
	}
	if wait.OpenChange {
		change := record.WaitChange{Generation: issue.NextChange, Reasons: []record.WaitReason{}, Since: now}
		for _, defect := range view.facts.Defects {
			if defect.CarriedBy == issue.NextChange {
				change.Reasons = append(change.Reasons, waitReason(defect))
			}
		}
		if was := old.NextChange; was != nil && was.Generation == change.Generation {
			change.Since = was.Since
		}
		stored.NextChange = &change
	}
	if wait.Unread {
		cause := unreadCause(issue, view)
		item := record.WaitUnread{Cause: cause, Since: now}
		if was := old.Unread; was != nil && was.Cause == cause {
			item.Since = was.Since
		}
		stored.Unread = &item
	}
	if wait.Unapproved && view.answer != nil {
		item := record.WaitUnapproved{Checks: record.UnapprovedChecks(view.answer.Approval), Since: now}
		if was := old.Unapproved; was != nil && slices.Equal(was.Checks, item.Checks) {
			item.Since = was.Since
		}
		stored.Unapproved = &item
	}
	return stored
}

// waitIssues is the issues keys name, each with its status in statuses and since when it stands:
// since its entry in old where it stood there in the same status, now otherwise.
func waitIssues(keys []string, statuses map[string]string, old []record.WaitIssue, now time.Time) []record.WaitIssue {
	var issues []record.WaitIssue
	for _, key := range keys {
		item := record.WaitIssue{Key: key, Status: statuses[key], Since: now}
		for _, was := range old {
			if was.Key == key && was.Status == item.Status {
				item.Since = was.Since
			}
		}
		issues = append(issues, item)
	}
	return issues
}

// waitReason is a reason of a next change, or a reported defect, as the stored wait names it: a
// reported defect with the role that reported it and the head it saw it at.
func waitReason(defect record.Defect) record.WaitReason {
	return record.WaitReason{ID: defect.ID, Kind: defect.Kind, Text: defect.Text, Check: defect.Check, Role: defect.Role, Head: defect.Head,
		Since: defect.ReportedAt}
}

// unreadCause is why the newest read does not count: none since the entry, a failed one, one in
// flight across the entry, an answer that does not decode, or checks whose bases name no merge
// commit of the newest merge, with what Dispatch said of each where it said anything.
func unreadCause(issue record.Issue, view closeView) string {
	read := view.facts.Read
	switch {
	case read == nil:
		return "no read of the checks since the merge"
	case read.Failure != "":
		return "the read failed: " + read.Failure
	case !read.ReadAt.After(view.facts.Entered):
		return "the read began before the entry into verifying"
	case view.answer == nil:
		return "the read's answer does not decode"
	}
	var said []string
	for _, check := range view.answer.Results.Checks {
		if check.ComparedNothing != "" {
			said = append(said, fmt.Sprintf("%s: %s", check.BlockID, check.ComparedNothing))
		}
	}
	cause := fmt.Sprintf("no basis of the read names the merge commit %s", issue.Merge.SHA)
	if len(said) > 0 {
		cause += ". Dispatch said:" + bulleted(said)
	}
	return cause
}

// askCloseRead queues the read of issue's checks (record.CloseRead) where none waits to run: one
// row waits for an issue at a time, and the read it makes when it runs answers every event before
// it. A row that runs already may have read Dispatch before this event, and the read it carries
// back is applied while its row is claimed, so only a row no runner has claimed counts
// (record.Store.OutboxWaiting).
func (e *Engine) askCloseRead(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	return e.askCloseReadAt(ctx, tx, issue, e.now())
}

// askCloseReadAt is askCloseRead due at at: now for an event that asks for a read, later for the
// backoff of a read that did not count. A row of the issue that waits is made due at the earlier
// of its own time and at (record.Store.ExpediteOutbox) rather than left where it stands, since a
// backoff can have left it an hour out and an event's read would wait that hour for it. So a
// later ask moves no nearer row out, and no ask queues a second row: the issue keeps one read.
func (e *Engine) askCloseReadAt(ctx context.Context, tx pgx.Tx, issue record.Issue, at time.Time) error {
	waiting, found, err := e.store.OutboxWaiting(ctx, tx, issue.Key, record.OutboxKindCloseRead)
	if err != nil {
		return err
	}
	if found {
		_, err := e.store.ExpediteOutbox(ctx, tx, waiting, at)
		return err
	}
	return e.enqueueAt(ctx, tx, issue.Key, record.CloseRead{Generation: issue.Generation}, at)
}

// publishWait queues the row that writes issue's stored wait onto close-wait.md where none waits to
// run: the row renders the wait the record holds when it runs, so one waiting row stands for every
// change before it runs. A row that runs already may have rendered the wait before this change, so
// it does not count (record.Store.OutboxWaiting).
func (e *Engine) publishWait(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	_, waiting, err := e.store.OutboxWaiting(ctx, tx, issue.Key, record.OutboxKindCloseWaitPublish)
	if err != nil || waiting {
		return err
	}
	return e.enqueue(ctx, tx, issue.Key, record.CloseWaitPublish{Generation: issue.Generation})
}

// closeReadAgain is when a read that did not decide is made again (readAgainSince): one whose
// basis named no merge commit of the newest merge, or one with a check Dispatch could not place
// or read. It is as long after now as the wait has stood so, between a minute and an hour, so a
// merge Dispatch orders soon is read soon and one it never orders costs a read an hour.
func closeReadAgain(now time.Time, unreadSince time.Time) time.Time {
	wait := min(max(now.Sub(unreadSince), time.Minute), time.Hour)
	return now.Add(wait)
}

// enterVerifying is the move of issue into verifying on trigger, the merge, where its close waits
// (D1): the move itself (arrive, which writes the status testing and no message), the stored wait
// with the moment of the entry, since when the issue waits (GitHub's merged_at of the merge), the
// row that writes close-wait.md, the first read of the checks, and one close-waits notice to the
// owning architect. It starts nobody and suspends nothing: no role works the wait, the claims and
// the pod stand, and the idle release frees the pod.
func (e *Engine) enterVerifying(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, trigger TriggerKind, handoff record.PhaseRow, pr *record.PullRequest) error {
	now := e.now()
	entering := issue
	entering.Phase, entering.Hold = phase.Verifying, nil
	view, err := e.closeWaitFrom(ctx, tx, entering, now)
	if err != nil {
		return err
	}
	old, err := storedWaitOf(issue)
	if err != nil {
		return err
	}
	stored := e.waitOf(issue, view, old, now)
	stored.Entered = now
	if err := putStoredWait(&issue, stored); err != nil {
		return err
	}
	if issue.CloseWaitSince == nil && !issue.Merge.MergedAt.IsZero() {
		since := issue.Merge.MergedAt.UTC()
		issue.CloseWaitSince = &since
	}
	if _, err := e.arrive(ctx, tx, &issue, &w, trigger, phase.Verifying, PhaseStatus(phase.Verifying), nil, handoff, pr, ""); err != nil {
		return err
	}
	if _, err := e.clearMergeForward(ctx, tx, issue.Key); err != nil {
		return err
	}
	return e.waitEntered(ctx, tx, issue, view, pr)
}

// waitEntered is what every entry into verifying queues after its write: the row that writes
// close-wait.md, the first read of the checks, and the close-waits notice.
func (e *Engine) waitEntered(ctx context.Context, tx pgx.Tx, issue record.Issue, view closeView, pr *record.PullRequest) error {
	if err := e.publishWait(ctx, tx, issue); err != nil {
		return err
	}
	if err := e.askCloseRead(ctx, tx, issue); err != nil {
		return err
	}
	number := issue.Merge.Number
	if pr != nil {
		number = pr.Number
	}
	e.logOnCommit(ctx, "workflow: a merged issue waits in verifying for the checks of its goal", "issue", issue.Key, "tree", issue.Tree,
		"pullRequest", number, "beneath", view.wait.Beneath)
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: "close-waits", Role: claim.RoleArchitect, Reason: closeWaitsReason(issue, number, view.wait)})
}

// closeWaitsReason is the close-waits notice of an entry into verifying: the merge, that the issue
// waits there with the status testing, each issue beneath it the close waits for, and where the
// wait is read.
func closeWaitsReason(issue record.Issue, number int, wait record.CloseWait) string {
	reason := fmt.Sprintf("Pull request #%d of %s merged. %s now waits in verifying for the checks of its goal, with the status testing.", number, issue.Key, issue.Key)
	if len(wait.Beneath) > 0 {
		reason += " Its close also waits for these issues beneath it:" + bulleted(wait.Beneath) + "\n"
	} else {
		reason += " "
	}
	return reason + "Read what the close waits for in close-wait.md and in legion state."
}

// bulleted is items one to a line, each after a dash, so a notice that names many items keeps each
// sentence short: the sentence before it ends in a colon.
func bulleted(items []string) string {
	var text strings.Builder
	for _, item := range items {
		text.WriteString("\n- " + item)
	}
	return text.String()
}

// closeChecksRead applies the read of an issue's checks (intake.CloseChecksRead): it stores the
// read as the issue's newest (record.Store.PutCheckRead), which keeps a read only where no later
// one stands, and judges the close of an issue in verifying on it.
func (e *Engine) closeChecksRead(ctx context.Context, tx pgx.Tx, fact intake.CloseChecksRead) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	read := record.CheckRead{Issue: fact.Issue, Generation: fact.Generation, ReadAt: fact.ReadAt.UTC(), Failure: fact.Failure, RecordedAt: e.now()}
	if fact.Answer != nil && fact.Failure == "" {
		encoded, err := json.Marshal(fact.Answer)
		if err != nil {
			return intake.Result{}, fmt.Errorf("store the read of %s's checks: %w", fact.Issue, err)
		}
		read.Answer = encoded
	} else if read.Failure == "" {
		read.Failure = "the read carried no answer"
	}
	kept, err := e.store.PutCheckRead(ctx, tx, read)
	if err != nil || !kept {
		return intake.Result{}, err
	}
	if issue.Phase != phase.Verifying {
		return intake.Result{}, nil
	}
	if err := e.openCheckAtRead(ctx, tx, *issue); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.judgeClose(ctx, tx, *issue)
}

// openCheckAtRead opens the production check of issue, which waits in verifying, at a fresh read
// (D6): a read that counts, made after the issue's newest entry into verifying with each check's
// basis at the newest merge (record.CloseWait.Unread), with nothing beneath it in the workflow, no
// reason of a next change waiting, a reported one or a check Dispatch computes as failed, and no
// change open, where no check of the issue's generation and merge stands (openCheck). So the
// bound never counts from a read that names an earlier merge, and a fail's next change opens the
// check of its own merge. A merged root whose children still run opens its check at the read its
// last child's leave queues, so its bound counts from that open. A project that runs no
// production check opens nothing.
func (e *Engine) openCheckAtRead(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	if e.checkRole() == "" {
		return nil
	}
	view, err := e.closeWait(ctx, tx, issue)
	if err != nil {
		return err
	}
	if view.wait.Unread {
		return nil
	}
	failed := slices.ContainsFunc(view.wait.Checks, func(check record.CheckWait) bool { return check.State == record.CheckStateFail })
	if len(view.wait.Beneath) > 0 || len(view.facts.Defects) > 0 || failed || view.facts.OpenChange {
		return nil
	}
	if check := view.facts.Production; check != nil && check.Generation == issue.Generation && check.MergeSHA == issue.Merge.SHA {
		return nil
	}
	lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
	if err != nil {
		return err
	}
	return e.openCheck(ctx, tx, issue, issue.Merge, lingers)
}

// resultChanged is a change of what Dispatch computes for an issue's checks
// (intake.DispatchResultChanged): a result recorded or retracted, a pull request's move, or a
// deploy. It reads the issue once and takes one path. An issue in verifying has its checks read
// again, since none of these decides a check, and a result recorded after the production check
// closed as not run opens one more look (lookAfterNotRun). A done issue that this release closed
// on its checks tells the controller of a fail (tellClosedCheckFailed).
func (e *Engine) resultChanged(ctx context.Context, tx pgx.Tx, fact intake.DispatchResultChanged) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	switch issue.Phase {
	case phase.Done:
		return intake.Result{}, e.tellClosedCheckFailed(ctx, tx, *issue, fact)
	case phase.Verifying:
		if err := e.lookAfterNotRun(ctx, tx, *issue, fact); err != nil {
			return intake.Result{}, err
		}
		return intake.Result{}, e.askCloseRead(ctx, tx, *issue)
	}
	return intake.Result{}, nil
}

// lookAfterNotRun opens one more look of issue's production check where a result is recorded
// (fact.Verdict, result.recorded) for a check that has no pass in the newest read, the check of the
// issue's generation and newest merge closed not_run, and no look is open (D6). No timer and no
// deploy opens such a look. The record opens again in place: its close cleared, Last set, and the
// look number counted on, so each row and report of an earlier look stays fenced by its look
// number and OpenedAt. relook_seq holds the Dispatch sequence of the event that opened it, so a
// redelivered event opens nothing.
func (e *Engine) lookAfterNotRun(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DispatchResultChanged) error {
	if fact.Verdict == "" || fact.Check == "" {
		return nil
	}
	check, err := e.store.ProductionCheck(ctx, tx, issue.Key)
	if err != nil || check == nil {
		return err
	}
	if check.Open() || check.Outcome != record.CheckNotRun || check.Generation != issue.Generation || check.MergeSHA != issue.Merge.SHA ||
		fact.Seq <= check.RelookSeq {
		return nil
	}
	read, err := e.store.NewestCheckRead(ctx, tx, issue.Key)
	if err != nil || read == nil || len(read.Answer) == 0 {
		return err
	}
	var answer record.CheckReadAnswer
	if err := json.Unmarshal(read.Answer, &answer); err != nil {
		return nil
	}
	withoutPass := false
	for _, block := range answer.Results.Checks {
		if block.BlockID == fact.Check && block.PassStands.Answer != dispatch.PassStandsYes {
			withoutPass = true
		}
	}
	if !withoutPass {
		return nil
	}
	check.ClosedAt, check.Outcome, check.RelookSeq = nil, "", fact.Seq
	e.logOnCommit(ctx, "workflow: a result after the production check closed as not run opens one more look", "issue", issue.Key,
		"check", fact.Check, "result", fact.ResultID, "seq", fact.Seq)
	return e.startLook(ctx, tx, issue, check, wakeResult, fmt.Sprintf("%s, recorded after the check closed as not run", fact.Check), true)
}

// closeWaitNoResult tells the owning architect once, at the close of the production check of issue,
// which waits in verifying, where the check closed as not run or a check still has no pass in the
// newest read: each such check with its answer and reason, and the check's outcome (D5, D6).
func (e *Engine) closeWaitNoResult(ctx context.Context, tx pgx.Tx, issue record.Issue, check record.ProductionCheck) error {
	var without []string
	read, err := e.store.NewestCheckRead(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	if read != nil && read.Failure == "" && len(read.Answer) > 0 {
		var answer record.CheckReadAnswer
		if err := json.Unmarshal(read.Answer, &answer); err == nil {
			for _, block := range answer.Results.Checks {
				if block.PassStands.Answer != dispatch.PassStandsYes {
					without = append(without, fmt.Sprintf("%s (%s)", block.BlockID, strings.Trim(block.PassStands.Answer+", "+block.PassStands.Reason, ", ")))
				}
			}
		}
	}
	if check.Outcome != record.CheckNotRun && len(without) == 0 {
		return nil
	}
	reason := fmt.Sprintf("The production check of %s closed as %s, at generation %d and merge commit %s.", issue.Key, check.Outcome, check.Generation, check.MergeSHA)
	if len(without) > 0 {
		reason += " These checks have no pass:" + bulleted(without) + "\n"
	} else {
		reason += " "
	}
	reason += fmt.Sprintf("%s stays open in verifying. Read what its close waits for in close-wait.md.", issue.Key)
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: "close-wait-no-result", Role: claim.RoleArchitect, Reason: reason})
}

// askAncestorsRead queues a read of the checks of each issue above issue that stands in
// verifying: a change of what stands beneath an issue changes what its close waits for, and the
// read is what reads its children from Dispatch after the change.
func (e *Engine) askAncestorsRead(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	members, err := e.treeMembers(ctx, tx, issue)
	if err != nil {
		return err
	}
	for _, ancestor := range record.Above(issue, members) {
		if ancestor.Phase != phase.Verifying {
			continue
		}
		if err := e.askCloseRead(ctx, tx, ancestor); err != nil {
			return err
		}
	}
	return nil
}

// judgeClose is the one judge of a merged issue's close: every door that can write done for a
// merged issue reaches it, and so does every event that changes an input of the predicate. It
// acts on issue as the store holds it in verifying, and on no other issue. Where the close waits
// for nothing it closes the issue (closeOnChecks). Otherwise it writes the stored wait where an
// item came, went or changed its state, queues one row for close-wait.md where none stands, tells
// the owning architect at once of a check newly failed, newly unknown for a pass and a fail at one
// commit, or newly changed without approval, and reads again later a read that did not decide: a
// basis named no merge commit of the newest merge, or Dispatch could not place or read a check.
func (e *Engine) judgeClose(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	return e.judgeCloseNoting(ctx, tx, issue, nil)
}

// judgeCloseNoting is judgeClose for an event that close-wait.md names once, in a version it
// writes: withdrawn, the defects a withdrawal ended (defectWithdrawal), which the stored wait it
// writes holds as its Withdrawn beside every withdrawal the wait already held (heldWithdrawals).
// The wait keeps a withdrawal until a version of the document holding it is uploaded, whatever
// writes the wait first; the row that uploads it takes it out (daemon's closeWaitPublish).
func (e *Engine) judgeCloseNoting(ctx context.Context, tx pgx.Tx, issue record.Issue, withdrawn []record.WaitWithdrawal) error {
	if issue.Phase != phase.Verifying {
		return nil
	}
	view, err := e.closeWait(ctx, tx, issue)
	if err != nil {
		return err
	}
	if view.wait.Done() {
		return e.closeOnChecks(ctx, tx, issue, view, withdrawn)
	}
	return e.storeWait(ctx, tx, issue, view, withdrawn)
}

// storeWait writes the stored wait of view where it differs from the one the row holds, with the
// row that publishes it, the notices of D11 that come at once, and the read again on the backoff
// of a read that did not decide (readAgainSince). withdrawn is what judgeCloseNoting names once.
func (e *Engine) storeWait(ctx context.Context, tx pgx.Tx, issue record.Issue, view closeView, withdrawn []record.WaitWithdrawal) error {
	now := e.now()
	old, err := storedWaitOf(issue)
	if err != nil {
		return err
	}
	stored := e.waitOf(issue, view, old, now)
	stored.Withdrawn = heldWithdrawals(old, withdrawn)
	if err := e.tellAtOnce(ctx, tx, issue, old, stored, view.wait); err != nil {
		return err
	}
	if since, again := readAgainSince(view, stored); again {
		if err := e.askCloseReadAt(ctx, tx, issue, closeReadAgain(now, since)); err != nil {
			return err
		}
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

// readAgainSince is since when stored, the stored wait of view, has waited for what only a read
// again decides, and false where it waits for nothing of the kind (D4). That is a read that
// counts after the entry with a basis naming no merge commit of the newest merge (Unread), and
// each check Dispatch answers unknown because it could not place a result or could not make the
// read (not_placed, read_failed). The earliest of their since sets the backoff (closeReadAgain).
func readAgainSince(view closeView, stored record.StoredWait) (time.Time, bool) {
	var since time.Time
	found := false
	take := func(at time.Time) {
		if !found || at.Before(since) {
			since, found = at, true
		}
	}
	if read := view.facts.Read; stored.Unread != nil && read != nil && read.Failure == "" && read.ReadAt.After(view.facts.Entered) {
		take(stored.Unread.Since)
	}
	for _, check := range stored.Checks {
		if check.Answer == dispatch.PassStandsUnknown && (check.Reason == dispatch.PassStandsReasonNotPlaced || check.Reason == dispatch.PassStandsReasonReadFailed) {
			take(check.Since)
		}
	}
	return since, found
}

// tellAtOnce tells the owning architect at once of each item of stored that old did not hold in
// that state: a check Dispatch computes as failed, naming its deciding result and its commit; a
// check unknown for a pass and a fail at one commit; and checks changed since the approved version,
// which a person must approve. Under `production_check: off` it also tells, at the first read that
// counts after the entry, each check with no pass, which an eligible agent must run (D5). A read
// before it that counted only in part, some checks at no merge commit of the newest merge, told
// nothing, since it left the wait unread.
func (e *Engine) tellAtOnce(ctx context.Context, tx pgx.Tx, issue record.Issue, old, stored record.StoredWait, wait record.CloseWait) error {
	stood := func(check record.WaitCheck) bool {
		for _, was := range old.Checks {
			if was.ID == check.ID && was.Answer == check.Answer && was.Reason == check.Reason {
				return true
			}
		}
		return false
	}
	for _, check := range stored.Checks {
		if stood(check) {
			continue
		}
		switch {
		case check.Answer == dispatch.PassStandsNo && check.Reason == dispatch.PassStandsReasonFail:
			if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "check-failed", Role: claim.RoleArchitect,
				Reason: checkFailedReason(issue, check.ID, wait)}); err != nil {
				return err
			}
		case check.Answer == dispatch.PassStandsUnknown && check.Reason == dispatch.PassStandsReasonPassAndFail:
			if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "check-unknown", Role: claim.RoleArchitect,
				Reason: fmt.Sprintf("Check %s of %s has a pass and a fail at one commit. Dispatch counts no pass of it, and no later pass at that commit ends it.", check.ID, issue.Key)}); err != nil {
				return err
			}
		}
	}
	if e.checkRole() == "" && old.Unread != nil && stored.Unread == nil && len(stored.Checks) > 0 {
		var ids []string
		for _, check := range stored.Checks {
			ids = append(ids, fmt.Sprintf("%s (%s)", check.ID, strings.Trim(check.Answer+", "+check.Reason, ", ")))
		}
		if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "close-wait-no-result", Role: claim.RoleArchitect, Reason: fmt.Sprintf(
			"No role of this project runs a check after the merge of %s. These checks have no pass:%s\nHave an eligible agent run each check and record its result.",
			issue.Key, bulleted(ids))}); err != nil {
			return err
		}
	}
	if stored.Unapproved != nil && (old.Unapproved == nil || !slices.Equal(old.Unapproved.Checks, stored.Unapproved.Checks)) {
		return e.notice(ctx, tx, issue.Key, record.Notice{Kind: "checks-unapproved", Role: claim.RoleArchitect,
			Reason: fmt.Sprintf("These checks of %s changed since the approved Verification document:%s\nA person must approve its current version before %s closes.",
				issue.Key, bulleted(stored.Unapproved.Checks), issue.Key)})
	}
	return nil
}

// checkFailedReason is the check-failed notice of a check Dispatch computes as failed: the check,
// its deciding result and the commit it was recorded at, as the newest read answers them (wait).
func checkFailedReason(issue record.Issue, check string, wait record.CloseWait) string {
	for _, failed := range wait.Checks {
		if failed.Check == check && failed.Result != "" {
			return fmt.Sprintf("Dispatch computes check %s of %s as failed after its merge. Its deciding result is %s, at commit %s.",
				check, issue.Key, failed.Result, failed.Commit)
		}
	}
	return fmt.Sprintf("Dispatch computes check %s of %s as failed after its merge. Read its deciding result with dispatch results %s.", check, issue.Key, issue.Key)
}

// closeOnChecks closes issue, which waits in verifying for nothing any more (judgeClose): the
// stored wait says that it closed and when, and its row is queued before the status write of done,
// so close-wait.md says it while the issue is still open in Dispatch, which refuses an upload to a
// closed issue. The issue is marked closed on its checks (issues.closed_on_checks_at). In a tree
// that does not linger the table's row out of verifying writes done and the issue leaves the
// workflow (leave), which suspends a merged child's pod where its production check closed while
// it stood in verifying. In a lingering tree no transition runs, so the close writes the member's
// done directly, as a merge in a lingering tree did before the wait. withdrawn is what
// judgeCloseNoting names once: the withdrawal that left the close waiting for nothing.
func (e *Engine) closeOnChecks(ctx context.Context, tx pgx.Tx, issue record.Issue, view closeView, withdrawn []record.WaitWithdrawal) error {
	now := e.now()
	old, err := storedWaitOf(issue)
	if err != nil {
		return err
	}
	stored := e.waitOf(issue, view, old, now)
	stored.Withdrawn = heldWithdrawals(old, withdrawn)
	stored.Closed = &now
	if err := putStoredWait(&issue, stored); err != nil {
		return err
	}
	closedAt := now
	issue.ClosedOnChecksAt = &closedAt
	if err := e.enqueue(ctx, tx, issue.Key, record.CloseWaitPublish{Generation: issue.Generation}); err != nil {
		return err
	}
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return err
	}
	row, ok := w.row(issue.Phase, TriggerChecksPassed, "", Snapshot{Phase: issue.Phase})
	if !ok {
		return fmt.Errorf("close %s on its checks: its table holds no row out of %s", issue.Key, issue.Phase)
	}
	e.logOnCommit(ctx, "workflow: a merged issue closes on the checks of its goal", "issue", issue.Key, "tree", issue.Tree, "generation", issue.Generation)
	lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
	if err != nil {
		return err
	}
	if lingers {
		return e.closeLingeringMember(ctx, tx, issue, w, row)
	}
	if _, err := e.arrive(ctx, tx, &issue, &w, TriggerChecksPassed, row.To, PhaseStatus(row.To), nil, record.PhaseRow{}, nil, ""); err != nil {
		return err
	}
	return e.leave(ctx, tx, issue, "done")
}

// heldWithdrawals is what a stored wait written over old holds of withdrawals: each one old holds,
// which no uploaded version of close-wait.md has said yet, then withdrawn, the ones the write
// adds. waitOf builds every other item of the wait afresh, so this is what keeps a withdrawal from
// being replaced before the row that writes close-wait.md renders it.
func heldWithdrawals(old record.StoredWait, withdrawn []record.WaitWithdrawal) []record.WaitWithdrawal {
	if len(old.Withdrawn) == 0 && len(withdrawn) == 0 {
		return nil
	}
	return append(slices.Clone(old.Withdrawn), withdrawn...)
}

// closeLingeringMember writes the close of issue, a member of a lingering tree, on row, the table's
// row out of verifying: the status done, the end of the step it stood in, its task done, the issue
// written, and child-closed to the architect above a child. It retires nobody and suspends nothing:
// the linger's own rows already did, and the tree's close retires every member's claims.
func (e *Engine) closeLingeringMember(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, row Row) error {
	status := PhaseStatus(row.To)
	if err := e.status(ctx, tx, issue, status, ""); err != nil {
		return err
	}
	if err := e.nameMoveEnd(ctx, w, issue, TriggerChecksPassed, row.To, record.PhaseRow{}); err != nil {
		return err
	}
	if err := e.moveTask(ctx, tx, &issue, &w, row.To, record.TaskDone); err != nil {
		return err
	}
	issue.Hold = nil
	if status != "" {
		issue.Status = status
	}
	if err := e.WriteIssue(ctx, tx, issue); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a lingering tree's member closed on its checks", "issue", issue.Key, "tree", issue.Tree, "status", issue.Status)
	if claim.IsTreeRoot(issue.Key, issue.Tree) {
		return nil
	}
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: fmt.Sprintf("%s is done", issue.Key)})
}

// judgeEachVerifying reads again and judges the close of each live issue of the project in
// verifying: the backstop of every event that judges a close (D15), on the controller's tick and at
// the boot. A read is queued only where none stands.
func (e *Engine) judgeEachVerifying(ctx context.Context, tx pgx.Tx, issues []record.Issue) error {
	for _, listed := range issues {
		if listed.Phase != phase.Verifying {
			continue
		}
		issue, err := e.store.Issue(ctx, tx, listed.Key)
		if err != nil {
			return err
		}
		if issue == nil || issue.Phase != phase.Verifying {
			continue
		}
		if err := e.askCloseRead(ctx, tx, *issue); err != nil {
			return err
		}
		if err := e.judgeClose(ctx, tx, *issue); err != nil {
			return err
		}
	}
	return nil
}

// tellEachStanding tells the owning architect of each live issue of the project in verifying, once
// each UTC day at the first controller tick of the day, each term that still holds its close
// (D11, close-wait-standing): the turn's own read is queued by judgeEachVerifying, and its answer
// opens a production check of the issue's generation where none stands (openCheckAtRead). An
// issue that waits for nothing is not told, and a term that stands no more is not named.
func (e *Engine) tellEachStanding(ctx context.Context, tx pgx.Tx, issues []record.Issue) error {
	now := e.now()
	for _, live := range issues {
		if live.Phase != phase.Verifying {
			continue
		}
		issue, err := e.store.Issue(ctx, tx, live.Key)
		if err != nil {
			return err
		}
		if issue == nil || issue.Phase != phase.Verifying {
			continue
		}
		if told := issue.StandingToldAt; told != nil && told.UTC().Format(time.DateOnly) == now.UTC().Format(time.DateOnly) {
			continue
		}
		view, err := e.closeWait(ctx, tx, *issue)
		if err != nil {
			return err
		}
		if view.wait.Done() {
			continue
		}
		stored, err := storedWaitOf(*issue)
		if err != nil {
			return err
		}
		lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
		if err != nil {
			return err
		}
		terms := standingTerms(*issue, stored, lingers)
		if len(terms) == 0 {
			continue
		}
		if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "close-wait-standing", Role: claim.RoleArchitect,
			Reason: fmt.Sprintf("%s still waits in verifying for its close. Each term that holds it:", issue.Key) + bulleted(terms) + "\nRead the whole wait in close-wait.md."}); err != nil {
			return err
		}
		toldAt := now
		issue.StandingToldAt = &toldAt
		if err := e.WriteIssue(ctx, tx, *issue); err != nil {
			return err
		}
	}
	return nil
}

// standingTerms is one sentence for each term of stored, the issue's stored wait, that holds its
// close, with its cause and since when it stands (D11): the read that does not count, the checks
// changed without approval, each issue beneath and each open child, each check without a pass,
// the production check where it holds the close with its way out, each reported reason, and a
// reason that waits in a tree that does not run.
func standingTerms(issue record.Issue, stored record.StoredWait, lingers bool) []string {
	since := func(at time.Time) string { return at.UTC().Format(time.RFC3339) }
	var terms []string
	if unread := stored.Unread; unread != nil {
		terms = append(terms, fmt.Sprintf("Its checks are unread since %s: %s.", since(unread.Since), unread.Cause))
	}
	if unapproved := stored.Unapproved; unapproved != nil {
		terms = append(terms, fmt.Sprintf("Since %s these checks changed without a person's approval: %s.", since(unapproved.Since), strings.Join(unapproved.Checks, ", ")))
	}
	for _, beneath := range stored.Beneath {
		terms = append(terms, fmt.Sprintf("%s beneath it is in the workflow at %s since %s.", beneath.Key, beneath.Status, since(beneath.Since)))
	}
	for _, child := range stored.OpenChildren {
		terms = append(terms, fmt.Sprintf("Its child %s is open in Dispatch at %s since %s.", child.Key, child.Status, since(child.Since)))
	}
	for _, check := range stored.Checks {
		terms = append(terms, fmt.Sprintf("Check %s has no pass since %s: %s.", check.ID, since(check.Since), strings.Trim(check.Answer+", "+check.Reason, ", ")))
	}
	if production := stored.Production; production != nil {
		state := production.State
		switch {
		case production.Outcome != "":
			state = "closed as " + production.Outcome
		case production.NextLookAt != nil:
			state = "open, its next look at " + since(*production.NextLookAt)
		}
		terms = append(terms, fmt.Sprintf("Its production check of generation %d at merge commit %s is %s since %s.", production.Generation, production.Merge, state, since(production.Since)),
			"Its way out is a pass at its look, one more look after a late result, or the next change of a fail.")
	}
	for _, defect := range stored.Defects {
		terms = append(terms, fmt.Sprintf("Reason %d waits since %s: %s.", defect.ID, since(defect.Since), defect.Text))
	}
	if change := stored.NextChange; change != nil {
		terms = append(terms, fmt.Sprintf("Its next change %d is open since %s.", change.Generation, since(change.Since)))
	}
	if lingers && (len(stored.Defects) > 0 || stored.NextChange != nil) {
		terms = append(terms, fmt.Sprintf("The tree of %s does not run while its root lingers, so no next change starts.", issue.Key))
	}
	return terms
}

// waitedFor is what a person's done on an issue in verifying closed past: each item of its stored
// wait, in the words of close-wait.md's headings, for the close's message.
func waitedFor(issue record.Issue) []string {
	stored, err := storedWaitOf(issue)
	if err != nil {
		return nil
	}
	var items []string
	for _, check := range stored.Checks {
		items = append(items, fmt.Sprintf("check %s (%s)", check.ID, strings.Trim(check.Answer+", "+check.Reason, ", ")))
	}
	if production := stored.Production; production != nil {
		items = append(items, "the production check, "+production.State)
	}
	for _, beneath := range stored.Beneath {
		items = append(items, fmt.Sprintf("%s beneath it (%s)", beneath.Key, beneath.Status))
	}
	for _, child := range stored.OpenChildren {
		items = append(items, fmt.Sprintf("its open child %s (%s)", child.Key, child.Status))
	}
	for _, defect := range stored.Defects {
		items = append(items, fmt.Sprintf("defect %d", defect.ID))
	}
	if stored.NextChange != nil {
		items = append(items, fmt.Sprintf("its open change %d", stored.NextChange.Generation))
	}
	if stored.Unread != nil {
		items = append(items, "a read of its checks")
	}
	if stored.Unapproved != nil {
		items = append(items, "a person's approval of its Verification document")
	}
	return items
}

// personClosedPast is a person's done on issue, which waits in verifying (D16, door 6): the close
// stands, as a person's close does. Its message names what the close waited for, since a closed
// issue still takes a message and Dispatch refuses close-wait.md's next version. The stored wait
// says that the issue closed, and each open reason of a next change is marked closed by the
// person. An open production check goes on as for any closed issue (leave). A merged child whose
// check closed while it waited has its pod suspended by the leave, since no check runs on.
func (e *Engine) personClosedPast(ctx context.Context, tx pgx.Tx, issue *record.Issue) error {
	now := e.now()
	members, err := e.treeMembers(ctx, tx, *issue)
	if err != nil {
		return err
	}
	items := waitedFor(*issue)
	for _, member := range record.Beneath(*issue, members) {
		named := fmt.Sprintf("%s beneath it (%s)", member.Key, member.Status)
		if member.Status != record.DoneStatus && !slices.ContainsFunc(items, func(item string) bool { return strings.HasPrefix(item, member.Key+" ") }) {
			items = append(items, named)
		}
	}
	body := fmt.Sprintf("A person closed %s before the checks of its goal closed it.", issue.Key)
	if len(items) > 0 {
		body += " Its close waited for:" + bulleted(items)
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.MessagePost{Body: body}); err != nil {
		return err
	}
	if _, err := e.store.CloseDefectsByPerson(ctx, tx, issue.Key, now); err != nil {
		return err
	}
	stored, err := storedWaitOf(*issue)
	if err != nil {
		return err
	}
	stored.Closed = &now
	if err := putStoredWait(issue, stored); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a person closed an issue whose close waited for its checks", "issue", issue.Key, "waited", items)
	return e.WriteIssue(ctx, tx, *issue)
}

// closesByChecks says whether issue, a child, closes by its checks and not by its owning
// architect's done: its work merged (Issue.Merge), so its checks or a person close it (D16, door 7).
func closesByChecks(issue record.Issue) bool {
	return issue.Merge.Merged() && !claim.IsTreeRoot(issue.Key, issue.Tree)
}

// architectsDoneSetBack tells the owning architect that its done on issue, a merged child, was set
// back, and judges the child's close, which its checks decide.
func (e *Engine) architectsDoneSetBack(ctx context.Context, tx pgx.Tx, key string) error {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return err
	}
	if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "close-waits", Role: claim.RoleArchitect, Reason: fmt.Sprintf(
		"The daemon set your done on %s back. Its pull request merged, so its checks or a person close it. Read what its close waits for in close-wait.md.", issue.Key)}); err != nil {
		return err
	}
	return e.judgeClose(ctx, tx, *issue)
}

// verificationChanged is a version or an approval of an issue's document (intake.DispatchArtifact)
// where that document is the Verification document the issue's newest read names
// (dispatch.IssueResults.VerificationArtifactID) and the issue waits in verifying: a new version
// can reword or add a check, and an approval can approve a rewording, so the checks are read again
// (D9, D15). A version of close-wait.md, or of any other document, judges nothing.
func (e *Engine) verificationChanged(ctx context.Context, tx pgx.Tx, fact intake.DispatchArtifact) error {
	if fact.Kind != intake.DispatchArtifactVersion && fact.Kind != intake.DispatchArtifactApproved {
		return nil
	}
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil || issue.Phase != phase.Verifying {
		return err
	}
	read, err := e.store.NewestCheckRead(ctx, tx, issue.Key)
	if err != nil || read == nil || len(read.Answer) == 0 {
		return err
	}
	var answer record.CheckReadAnswer
	if err := json.Unmarshal(read.Answer, &answer); err != nil || answer.Results.VerificationArtifactID != fact.ArtifactID {
		return nil
	}
	return e.askCloseRead(ctx, tx, *issue)
}
