package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// statusRefused is Dispatch's refusal, with PARENT_STATUS_CONFLICT, of a status write the daemon
// queued that is no close (intake.StatusRefused; decision 12 of dispatch://INFRA-4730). The record
// counted the write before its echo (R16 of dispatch://INFRA-3320), so the write is undone: the
// record's status goes back to the one Dispatch last showed it (record.Issue.DispatchStatus),
// while it still holds the refused one, and whoever asked for the write is told the code and the
// issues Dispatch named: the issue's architect, or the project's controller for its set_status.
// A refusal of a write of an earlier run or phase entry changes and tells nothing, since the
// issue no longer stands where the write was asked for.
func (e *Engine) statusRefused(ctx context.Context, tx pgx.Tx, fact intake.StatusRefused) (intake.Result, error) {
	fence, err := record.ReadFence(ctx, tx, e.store, fact.Issue, false)
	if err != nil {
		return intake.Result{}, err
	}
	if fence.Outlives(record.Fence{Generation: fact.Generation, Entry: fact.Entry}) {
		e.logOnCommit(ctx, "workflow: a refused status write of an earlier run or phase entry changes nothing", "issue", fact.Issue,
			"status", fact.Status, "generation", fact.Generation, "entry", fact.Entry)
		return intake.Result{}, nil
	}
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil {
		return intake.Result{}, err
	}
	holds := "the daemon records nothing of it"
	if issue != nil {
		holds = fmt.Sprintf("the daemon's record of it reads %s", issue.Status)
		if issue.Status == fact.Status && issue.DispatchStatus != "" && issue.DispatchStatus != fact.Status {
			issue.Status = issue.DispatchStatus
			if err := e.store.PutIssue(ctx, tx, *issue); err != nil {
				return intake.Result{}, err
			}
			holds = fmt.Sprintf("the daemon's record of it reads %s again, as Dispatch shows it", issue.Status)
		}
	}
	named := "an issue it did not name"
	if len(fact.Issues) > 0 {
		named = strings.Join(fact.Issues, ", ")
	}
	reason := fmt.Sprintf("Dispatch refused the daemon's write of %s on %s with PARENT_STATUS_CONFLICT, since it would leave a parent's status out of line with its children's: %s. The write is not made again, and %s. Move those issues, or ask for the write again with the option that moves or unlinks them.",
		fact.Status, fact.Issue, named, holds)
	e.logOnCommit(ctx, "workflow: a refused status write is undone and its caller told", "issue", fact.Issue, "status", fact.Status,
		"issues", fact.Issues, "controller", fact.Controller)
	if fact.Controller {
		return intake.Result{}, e.enqueue(ctx, tx, fact.Issue, record.ControllerNotice{Kind: "message-refused", Reason: reason})
	}
	return intake.Result{}, e.notice(ctx, tx, fact.Issue, record.Notice{Kind: "message-refused", Role: claim.RoleArchitect, Reason: reason})
}
