package workflow

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// defectReport applies a role's report_defect (intake.DefectReport, D8 of
// dispatch://INFRA-5580/artifact/plan-md). It is refused, recording nothing, for an issue the
// record does not hold (ISSUE_NOT_FOUND), in a tree that lingers (TREE_LINGERING), after the
// issue's close (DEFECT_ISSUE_CLOSED), before its task's last step passed, which is everywhere
// but retro, merging and verifying, a hold from retro or merging and the parent held before its
// retro for its children included (DEFECT_BEFORE_LAST_STEP), and for a caller that may not report
// there (DEFECT_CALLER_NOT_ALLOWED, mayReportDefect). A report whose text, trimmed with its white
// space folded, is an open reported defect's of the issue is that defect, and one naming a check
// that is already a reason of the issue joins that reason: either answers that defect's id and
// records nothing. Otherwise it records the defect at the issue's generation, writes it into the
// stored wait that close-wait.md renders (waitNamesDefects), and tells the owning architect where
// another role reported it (defect-reported).
//
// It moves no phase and completes nothing: the role goes on with its step. Only the next change
// turns a defect into work (StartNextChange, Tasks 6 and 7), which this report does not start.
// The answer names the generation of the change that will carry the defect: the one after the
// issue's, or the change that already carries the reason a report joined.
func (e *Engine) defectReport(ctx context.Context, tx pgx.Tx, fact intake.DefectReport) (intake.Result, error) {
	issue, refusal, err := e.defectIssue(ctx, tx, fact.Issue, "this report of a defect recorded nothing")
	if err != nil || refusal != nil {
		return intake.Result{Refusal: refusal}, err
	}
	lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
	if err != nil {
		return intake.Result{}, err
	}
	if lingers {
		return refusedLingering(issue, "this report of a defect"), nil
	}
	if issue.Phase == phase.Done || issue.Status == record.DoneStatus {
		return refused("DEFECT_ISSUE_CLOSED", fmt.Sprintf(
			"%s is closed, so no change of it carries a defect; report it as a finding outside the issue. This report recorded nothing.", issue.Key)), nil
	}
	if !record.StandsIn(issue, phase.Retro, phase.Merging, phase.Verifying) {
		return refused("DEFECT_BEFORE_LAST_STEP", fmt.Sprintf(
			"%s stands in %s, and a step of its task is still to pass: a gap found now goes back by request_backward_move, or by the owner's return in accepting. This report recorded nothing.",
			issue.Key, record.StandsAt(issue))), nil
	}
	allowed, err := e.mayReportDefect(ctx, tx, issue, fact)
	if err != nil {
		return intake.Result{}, err
	}
	if !allowed {
		return intake.Result{Refusal: &intake.Refusal{Status: http.StatusForbidden, Code: "DEFECT_CALLER_NOT_ALLOWED", Message: fmt.Sprintf(
			"the %s may not report a defect of %s while it stands in %s: the implementer reports in retro, the merger in merging and in verifying until its completion lands, and the owning architect from retro on. This report recorded nothing.",
			fact.Role, issue.Key, record.StandsAt(issue))}}, nil
	}
	open, err := e.store.OpenDefects(ctx, tx, issue.Key)
	if err != nil {
		return intake.Result{}, err
	}
	carrier := func(defect record.Defect) uint64 {
		if defect.CarriedBy != 0 {
			return defect.CarriedBy
		}
		return issue.Generation + 1
	}
	text := foldedDefectText(fact.Text)
	for _, defect := range open {
		if defect.Kind == record.DefectKindDefect && foldedDefectText(defect.Text) == text {
			return intake.Result{Defect: &intake.DefectAnswer{Defect: defect.ID, State: intake.DefectSame, NextChange: carrier(defect)}}, nil
		}
	}
	if fact.Check != "" {
		for _, defect := range open {
			if defect.Kind == record.DefectKindCheck && defect.Check == fact.Check {
				return intake.Result{Defect: &intake.DefectAnswer{Defect: defect.ID, State: intake.DefectJoined, NextChange: carrier(defect)}}, nil
			}
		}
	}
	id, err := e.store.PutDefect(ctx, tx, record.Defect{Issue: issue.Key, Generation: issue.Generation, Kind: record.DefectKindDefect,
		Text: strings.TrimSpace(fact.Text), Role: fact.Role, Head: fact.Head, Check: fact.Check, ReportedAt: e.now()})
	if err != nil {
		return intake.Result{}, err
	}
	if err := e.waitNamesDefects(ctx, tx, issue, nil); err != nil {
		return intake.Result{}, err
	}
	if fact.Role != claim.RoleArchitect {
		if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "defect-reported", Role: claim.RoleArchitect, Reason: fmt.Sprintf(
			"The %s reported defect %d of %s at head %s. The next change of %s carries it after the merge; withdraw it with withdraw_defect where it is no defect.",
			fact.Role, id, issue.Key, fact.Head, issue.Key)}); err != nil {
			return intake.Result{}, err
		}
	}
	e.logOnCommit(ctx, "workflow: a defect of the change was reported", "issue", issue.Key, "defect", id, "role", fact.Role,
		"phase", issue.Phase, "generation", issue.Generation)
	return intake.Result{Defect: &intake.DefectAnswer{Defect: id, State: intake.DefectRecorded, NextChange: issue.Generation + 1}}, nil
}

// defectWithdrawal applies the owning architect's withdraw_defect (intake.DefectWithdrawal, D8).
// It is refused, changing nothing, for an issue the record does not hold (ISSUE_NOT_FOUND), for a
// caller that does not own the issue by the rule of sign_off (DEFECT_WITHDRAW_NOT_OWNER), for an id
// the issue holds no defect of (DEFECT_NOT_FOUND), for a failed check's or a production check's
// reason, which only a new approved version of the Verification document changes
// (DEFECT_IS_A_CHECK), and for a defect a merged change carried, which is done
// (DEFECT_IN_MERGED_CHANGE). A defect a withdrawal already ended is answered withdrawn again, and
// nothing is written. Otherwise the defect ends with the reason, and the stored wait names the
// withdrawal once, in the version close-wait.md writes for it; in verifying that is the close's
// own judgement (judgeCloseNoting), since a withdrawal is an event the close is judged on again.
// 409 DEFECT_REQUEST_OF_A_PERSON, a request a person made, is Task 13's: no kind of reason this
// release writes is a request.
func (e *Engine) defectWithdrawal(ctx context.Context, tx pgx.Tx, fact intake.DefectWithdrawal) (intake.Result, error) {
	issue, refusal, err := e.defectIssue(ctx, tx, fact.Issue, "this withdrawal changed nothing")
	if err != nil || refusal != nil {
		return intake.Result{Refusal: refusal}, err
	}
	owns, err := e.ownsBySignOff(ctx, tx, fact.By, issue)
	if err != nil {
		return intake.Result{}, err
	}
	if !owns {
		return intake.Result{Refusal: &intake.Refusal{Status: http.StatusForbidden, Code: "DEFECT_WITHDRAW_NOT_OWNER", Message: fmt.Sprintf(
			"only the architect that owns %s by the rule of sign_off withdraws a defect of it. This withdrawal changed nothing.", issue.Key)}}, nil
	}
	defect, err := e.store.Defect(ctx, tx, issue.Key, fact.Defect)
	if err != nil {
		return intake.Result{}, err
	}
	if defect == nil {
		return intake.Result{Refusal: intake.DefectNotFound(issue.Key, fmt.Sprint(fact.Defect))}, nil
	}
	switch {
	case defect.Kind == record.DefectKindCheck || defect.Kind == record.DefectKindProduction:
		return refused("DEFECT_IS_A_CHECK", fmt.Sprintf(
			"defect %d of %s is a %s check's fail, which only a new approved version of the Verification document changes. This withdrawal changed nothing.",
			defect.ID, issue.Key, defect.Kind)), nil
	case defect.MergedAt != nil:
		return refused("DEFECT_IN_MERGED_CHANGE", fmt.Sprintf(
			"defect %d of %s was carried by change %d, which merged: it is done. This withdrawal changed nothing.", defect.ID, issue.Key, defect.CarriedBy)), nil
	case defect.WithdrawnAt != nil:
		return intake.Result{Defect: &intake.DefectAnswer{Defect: defect.ID, State: intake.DefectWithdrawn}}, nil
	}
	now := e.now()
	reason := strings.TrimSpace(fact.Reason)
	ended, err := e.store.WithdrawDefect(ctx, tx, issue.Key, defect.ID, reason, now)
	if err != nil {
		return intake.Result{}, err
	}
	if !ended {
		return intake.Result{}, fmt.Errorf("withdraw defect %d of %s: the record read it open and ended none", defect.ID, issue.Key)
	}
	if err := e.waitNamesDefects(ctx, tx, issue, []record.WaitWithdrawal{{ID: defect.ID, Text: defect.Text, Reason: reason, At: now}}); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, "workflow: a reported defect was withdrawn", "issue", issue.Key, "defect", defect.ID, "phase", issue.Phase)
	return intake.Result{Defect: &intake.DefectAnswer{Defect: defect.ID, State: intake.DefectWithdrawn}}, nil
}

// defectIssue is the record of key, or the 404 ISSUE_NOT_FOUND refusal of a call on an issue the
// record does not hold, whose sentence ends with what the call did.
func (e *Engine) defectIssue(ctx context.Context, tx pgx.Tx, key, did string) (record.Issue, *intake.Refusal, error) {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil {
		return record.Issue{}, nil, err
	}
	if issue == nil {
		return record.Issue{}, &intake.Refusal{Status: http.StatusNotFound, Code: "ISSUE_NOT_FOUND", Message: fmt.Sprintf("%s is not recorded; %s.", key, did)}, nil
	}
	return *issue, nil, nil
}

// mayReportDefect says whether fact's caller may report a defect of issue, which stands past its
// task's last step: the implementer in retro, the merger in merging and in verifying until its
// completion of the merged generation lands (Issue.MergerCompletionOwed), and the architect that
// owns the issue by the rule of sign_off from retro on, a hold included. No other role reports one.
func (e *Engine) mayReportDefect(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.DefectReport) (bool, error) {
	switch fact.Role {
	case claim.RoleImplementer:
		return issue.Phase == phase.Retro, nil
	case claim.RoleMerger:
		return issue.Phase == phase.Merging || issue.Phase == phase.Verifying && issue.MergerCompletionOwed, nil
	case claim.RoleArchitect:
		return e.ownsBySignOff(ctx, tx, fact.By, issue)
	}
	return false, nil
}

// ownsBySignOff says whether architect owns issue by the rule of sign_off: an architect of the
// issue's tree now (record.ArchitectOfTree) that is the issue's own architect or one above it
// (record.ArchitectAbove), as the workflow judges sign_off under the fact lock (requestRefusal).
func (e *Engine) ownsBySignOff(ctx context.Context, tx pgx.Tx, architect claim.Token, issue record.Issue) (bool, error) {
	if architect == "" {
		return false, nil
	}
	ofTree, err := record.ArchitectOfTree(ctx, e.store, tx, architect, issue)
	if err != nil || !ofTree {
		return false, err
	}
	return record.ArchitectAbove(ctx, e.store, tx, architect, issue)
}

// waitNamesDefects writes issue's open reported defects, and withdrawn, the defects a withdrawal
// just ended, into the stored wait that close-wait.md renders, and queues its write. In verifying
// that is the close's judgement (judgeCloseNoting), which reads the defects with every other
// term. Before the merge, in retro or merging or held from either, it replaces the stored wait's
// defects alone, keeping whatever an earlier entry into verifying left: nothing else of the close
// is read before the merge. Elsewhere it writes nothing, unless the issue holds a stored wait
// already, which then stops naming a withdrawn defect.
func (e *Engine) waitNamesDefects(ctx context.Context, tx pgx.Tx, issue record.Issue, withdrawn []record.WaitWithdrawal) error {
	if issue.Phase == phase.Verifying {
		return e.judgeCloseNoting(ctx, tx, issue, withdrawn)
	}
	if !record.StandsIn(issue, phase.Retro, phase.Merging) && len(issue.CloseWait) == 0 {
		return nil
	}
	open, err := e.store.OpenDefects(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	stored, err := storedWaitOf(issue)
	if err != nil {
		return err
	}
	stored.Defects = nil
	for _, defect := range open {
		if defect.Kind != record.DefectKindCheck && defect.Kind != record.DefectKindProduction {
			stored.Defects = append(stored.Defects, waitReason(defect))
		}
	}
	stored.Withdrawn = withdrawn
	if err := putStoredWait(&issue, stored); err != nil {
		return err
	}
	if err := e.WriteIssue(ctx, tx, issue); err != nil {
		return err
	}
	return e.publishWait(ctx, tx, issue)
}

// foldedDefectText is text trimmed, with each run of white space inside it one space: two reports
// whose texts fold to one are one defect (D8).
func foldedDefectText(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
