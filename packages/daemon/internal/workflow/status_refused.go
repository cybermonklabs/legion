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
// counted the write before its echo (R16 of dispatch://INFRA-3320), so the write is undone
// (undoRefusedStatus), and whoever asked for the write is told the code and the issues Dispatch
// named: the issue's architect, or the project's controller for its set_status. A refusal of a
// write of an earlier run or phase entry changes and tells nothing, since the issue no longer
// stands where the write was asked for.
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
		undone, err := e.undoRefusedStatus(ctx, tx, *issue, fact.Status)
		if err != nil {
			return intake.Result{}, err
		}
		if undone {
			holds = fmt.Sprintf("the daemon takes %s again, the status Dispatch shows of it", issue.DispatchStatus)
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

// undoRefusedStatus undoes on issue's record the daemon's write of refused, which Dispatch refused
// after the record counted it (statusRefused), and says whether it did: the record takes again the
// status Dispatch shows of it (record.Issue.DispatchStatus), where it still stands where the write
// put it.
//
// A write across the edge of Legion's work counted the move it makes (countCallWrite): a stop left
// Legion's work (leave), and a todo came back into it (ReturnToWork), opened a new run (NewRun) or
// recorded a child under its parent at work. So the status Dispatch shows is taken as a person's
// move is (applyStatus): a refused stop comes back into work, its claims started again and its
// task running, and a refused todo leaves, its claims and its task stopped. The record stands where
// a stop put it while it holds the stop, and where a todo put it while it is in Legion's work,
// whose return writes the status of the phase it comes back to over the todo (takeBack). A write
// within one side of that edge moved nothing but the status, so the status alone goes back, while
// the record still holds the refused one.
func (e *Engine) undoRefusedStatus(ctx context.Context, tx pgx.Tx, issue record.Issue, refused string) (bool, error) {
	shown := issue.DispatchStatus
	if shown == "" || shown == refused {
		return false, nil
	}
	stop := record.OutOfWorkflow(refused)
	if stop == record.OutOfWorkflow(shown) {
		if issue.Status != refused {
			return false, nil
		}
		issue.Status = shown
		return true, e.store.PutIssue(ctx, tx, issue)
	}
	if stop && issue.Status != refused || !stop && !record.InLegionsWork(issue) {
		return false, nil
	}
	return true, e.applyStatus(ctx, tx, issue, RecordedFact(issue, shown))
}
