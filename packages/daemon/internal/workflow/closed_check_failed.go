package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// tellClosedCheckFailed tells the controller of a fail recorded on an issue that this release
// closed on its checks (issues.closed_on_checks_at), in one closed-check-failed notice naming the
// issue, the check and the result. The close is the completion: Legion opens and reopens nothing
// for the issue, and the controller routes the fail as it routes any defect. An issue that an
// earlier release closed carries no mark and tells nobody, and an issue still in the workflow,
// a next change's among them, is the read's to judge (resultChanged). A retraction, a pass, a
// pull request's move and a deploy carry no fail and read nothing.
func (e *Engine) tellClosedCheckFailed(ctx context.Context, tx pgx.Tx, fact intake.DispatchResultChanged) error {
	if fact.Verdict != "fail" {
		return nil
	}
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil || issue.Phase != phase.Done || issue.ClosedOnChecksAt == nil {
		return err
	}
	reason := fmt.Sprintf("%s closed on its checks, and check %s then recorded a fail: result %s. Legion opens and reopens nothing for it. "+
		"Route the fail as you route any defect.", issue.Key, fact.Check, fact.ResultID)
	if err := e.enqueue(ctx, tx, issue.Key, record.ControllerNotice{Kind: record.ClosedCheckFailedNotice, Reason: reason}); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a fail after a close on checks is told to the controller", "issue", issue.Key, "tree", issue.Tree,
		"check", fact.Check, "result", fact.ResultID)
	return nil
}
